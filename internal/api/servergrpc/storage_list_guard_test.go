package servergrpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestStorageList_RejectsQueries（redesign 阶段②包 C / §1-6 契约断裂项）：
// storage buckets/files 面从不消费查询过滤——静默忽略会让调用方以为过滤已
// 生效，改为携带 queries 即 InvalidArgument（预决策 7）。守卫先于任何
// storage 依赖触发（nil 服务即断言守卫前置）。
func TestStorageList_RejectsQueries(t *testing.T) {
	s := NewStorageService(nil)

	_, err := s.ListBuckets(context.Background(), &sharedv1.ListRequest{
		Queries: []string{`equal("name","avatar")`},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "queries are not supported")

	_, err = s.ListFiles(context.Background(), &serverv1.ListFilesRequest{
		BucketId: "b1",
		Queries:  []string{`equal("name","a.png")`},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "queries are not supported")

	// 不携带 queries 时不在守卫处失败（继续走 project 上下文校验，
	// nil 服务的 unauthenticated 分支即证明守卫放行）。
	_, err = s.ListBuckets(context.Background(), &sharedv1.ListRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err), "无 queries 时守卫放行")
	_, err = s.ListFiles(context.Background(), &serverv1.ListFilesRequest{BucketId: "b1"})
	require.Equal(t, codes.Unauthenticated, status.Code(err), "无 queries 时守卫放行")
}
