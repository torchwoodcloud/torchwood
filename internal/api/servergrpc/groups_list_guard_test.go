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

// TestGroupsList_RejectsQueries（R12b，同 storage 法）：groups 面从不消费
// 查询过滤——静默忽略会让调用方以为过滤已生效，携带 queries 即
// InvalidArgument。守卫先于任何 groups 依赖触发（nil 服务即断言守卫前置）。
func TestGroupsList_RejectsQueries(t *testing.T) {
	s := NewGroupsService(nil)

	_, err := s.ListGroups(context.Background(), &sharedv1.ListRequest{
		Queries: []string{`equal("name","admins")`},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "queries are not supported")

	_, err = s.ListMemberships(context.Background(), &serverv1.ListMembershipsRequest{
		GroupId: "g1",
		Queries: []string{`equal("status","accepted")`},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "queries are not supported")

	// 不携带 queries 时守卫放行（继续走 project 上下文校验，
	// nil 服务的 unauthenticated 分支即证明）。
	_, err = s.ListGroups(context.Background(), &sharedv1.ListRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err), "无 queries 时守卫放行")
	_, err = s.ListMemberships(context.Background(), &serverv1.ListMembershipsRequest{GroupId: "g1"})
	require.Equal(t, codes.Unauthenticated, status.Code(err), "无 queries 时守卫放行")
}
