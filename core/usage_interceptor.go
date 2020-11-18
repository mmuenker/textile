package core

import (
	"context"
	"fmt"
	"strings"

	grpcm "github.com/grpc-ecosystem/go-grpc-middleware"
	powc "github.com/textileio/powergate/api/client"
	billing "github.com/textileio/textile/v2/api/billingd/client"
	"github.com/textileio/textile/v2/api/billingd/common"
	"github.com/textileio/textile/v2/buckets"
	mdb "github.com/textileio/textile/v2/mongodb"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type preFunc func(ctx context.Context, method string) (context.Context, error)
type postFunc func(ctx context.Context, method string) error

func unaryServerInterceptor(pre preFunc, post postFunc) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		newCtx, err := pre(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		res, err := handler(newCtx, req)
		if err != nil {
			return nil, err
		}
		if err = post(newCtx, info.FullMethod); err != nil {
			return nil, err
		}
		return res, nil
	}
}

func streamServerInterceptor(pre preFunc, post postFunc) grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		newCtx, err := pre(stream.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		wrapped := grpcm.WrapServerStream(stream)
		wrapped.WrappedContext = newCtx
		err = handler(srv, wrapped)
		if err != nil {
			return err
		}
		return post(newCtx, info.FullMethod)
	}
}

func (t *Textile) preUsageFunc(ctx context.Context, method string) (context.Context, error) {
	if t.bc == nil {
		return ctx, nil
	}
	for _, ignored := range authIgnoredMethods {
		if method == ignored {
			return ctx, nil
		}
	}
	for _, ignored := range usageIgnoredMethods {
		if method == ignored {
			return ctx, nil
		}
	}
	account, ok := mdb.AccountFromContext(ctx)
	if !ok {
		return ctx, nil
	}

	// Collect new users.
	if account.User.CreatedAt.IsZero() && account.User.Type == mdb.User {
		var powInfo *mdb.PowInfo
		if t.pc != nil {
			ctxAdmin := context.WithValue(ctx, powc.AdminKey, t.conf.PowergateAdminToken)
			res, err := t.pc.Admin.Users.Create(ctxAdmin)
			if err != nil {
				return ctx, err
			}
			powInfo = &mdb.PowInfo{ID: res.User.Id, Token: res.User.Token}
		}
		user, err := t.collections.Accounts.CreateUser(ctx, account.User.Key, powInfo)
		if err != nil {
			return ctx, err
		}
		ctx = mdb.NewAccountContext(ctx, user, account.Org)
		account, _ = mdb.AccountFromContext(ctx)
	}

	// Collect new customers.
	cus, err := t.bc.GetCustomer(ctx, account.Owner().Key)
	if err != nil {
		if strings.Contains(err.Error(), mongo.ErrNoDocuments.Error()) {
			opts := []billing.Option{
				billing.WithEmail(account.Owner().Email),
			}
			if account.Owner().Type == mdb.User {
				key, ok := mdb.APIKeyFromContext(ctx)
				if !ok {
					return ctx, status.Error(codes.PermissionDenied, "Bad API key")
				}
				opts = append(opts, billing.WithParentKey(key.Owner))
			}
			if _, err := t.bc.CreateCustomer(ctx, account.Owner().Key, opts...); err != nil {
				return ctx, err
			}
			cus, err = t.bc.GetCustomer(ctx, account.Owner().Key)
			if err != nil {
				return ctx, err
			}
		} else {
			return ctx, err
		}
	}
	if err := common.StatusCheck(cus.SubscriptionStatus); err != nil {
		return ctx, status.Error(codes.FailedPrecondition, err.Error())
	}
	if !cus.Billable && cus.DailyUsage["network_egress"].Free == 0 {
		err = fmt.Errorf("network egress exhausted: %v", common.ErrExceedsFreeQuota)
		return ctx, status.Error(codes.ResourceExhausted, err.Error())
	}

	// @todo: Attach egress info that can be used to fail-fast in PullPath?
	switch method {
	case "/api.bucketsd.pb.APIService/Create",
		"/api.bucketsd.pb.APIService/PushPath",
		"/api.bucketsd.pb.APIService/SetPath",
		"/api.bucketsd.pb.APIService/Remove",
		"/api.bucketsd.pb.APIService/RemovePath",
		"/api.bucketsd.pb.APIService/PushPathAccessRoles":
		owner := &buckets.BucketOwner{
			StorageUsed: cus.DailyUsage["stored_data"].Total,
		}
		if cus.Billable {
			owner.StorageAvailable = -1
		} else {
			owner.StorageAvailable = cus.DailyUsage["stored_data"].Free
		}
		ctx = buckets.NewBucketOwnerContext(ctx, owner)
	case
		"/threads.pb.API/Verify",
		"/threads.pb.API/Has",
		"/threads.pb.API/Find",
		"/threads.pb.API/FindByID",
		"/threads.pb.API/ReadTransaction",
		"/threads.pb.API/Listen":
		if !cus.Billable && cus.DailyUsage["instance_reads"].Free == 0 {
			err = fmt.Errorf("threaddb reads exhausted: %v", common.ErrExceedsFreeQuota)
			return ctx, status.Error(codes.ResourceExhausted, err.Error())
		}
	case "/threads.pb.API/Create",
		"/threads.pb.API/Save",
		"/threads.pb.API/Delete",
		"/threads.pb.API/WriteTransaction":
		if !cus.Billable && cus.DailyUsage["instance_writes"].Free == 0 {
			err = fmt.Errorf("threaddb writes exhausted: %v", common.ErrExceedsFreeQuota)
			return ctx, status.Error(codes.ResourceExhausted, err.Error())
		}
	}
	return ctx, nil
}

func (t *Textile) postUsageFunc(ctx context.Context, method string) error {
	if t.bc == nil {
		return nil
	}
	for _, ignored := range authIgnoredMethods {
		if method == ignored {
			return nil
		}
	}
	account, ok := mdb.AccountFromContext(ctx)
	if !ok {
		return nil
	}
	owner, ok := buckets.BucketOwnerFromContext(ctx)
	if !ok {
		return nil
	}
	switch method {
	case "/api.bucketsd.pb.APIService/Create",
		"/api.bucketsd.pb.APIService/PushPath",
		"/api.bucketsd.pb.APIService/SetPath",
		"/api.bucketsd.pb.APIService/Remove",
		"/api.bucketsd.pb.APIService/RemovePath",
		"/api.bucketsd.pb.APIService/PushPathAccessRoles":
		if _, err := t.bc.IncCustomerUsage(
			ctx,
			account.Owner().Key,
			map[string]int64{
				"stored_data": owner.StorageDelta,
			},
		); err != nil {
			return err
		}
	}
	return nil
}