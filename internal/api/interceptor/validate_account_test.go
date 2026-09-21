package interceptor

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestValidateInterceptorAccountProfile：name/avatar 写入门禁求值（客户端
// PATCH /v1/account 与 server UpdateUser 同口径）——name ≤64 码点（len 族按
// Unicode 码点计，CJK 一字一点）且整串无控制字符；avatar 非空必须 https 且
// ≤1024 字节，空串 = 清除；字段未设置（nil，presence 语义 = 不修改）不触发规则。
func TestValidateInterceptorAccountProfile(t *testing.T) {
	t.Parallel()
	v := newValidateInterceptorForTest(t)
	clientMethod := &grpc.UnaryServerInfo{FullMethod: "/torchwood.client.v1.AccountService/UpdateAccount"}
	serverMethod := &grpc.UnaryServerInfo{FullMethod: "/torchwood.server.v1.UsersService/UpdateUser"}

	call := func(method *grpc.UnaryServerInfo, req any) error {
		_, err := v.UnaryValidateMiddleware(context.Background(), req, method,
			func(ctx context.Context, req any) (any, error) { return nil, nil })
		return err
	}

	name64 := strings.Repeat("兽", 64) // 恰 64 码点（192 字节）→ 通过
	nameOver := strings.Repeat("兽", 65)
	withControl := "守卫\n者"

	// —— 客户端面 UpdateAccountRequest ——
	require.NoError(t, call(clientMethod, &clientv1.UpdateAccountRequest{Name: &name64}))
	require.Equal(t, codes.InvalidArgument, status.Code(call(clientMethod, &clientv1.UpdateAccountRequest{Name: &nameOver})))
	require.Equal(t, codes.InvalidArgument, status.Code(call(clientMethod, &clientv1.UpdateAccountRequest{Name: &withControl})))

	avatarOK := "https://thirdwx.qlogo.cn/mmopen/a/132"
	avatarHTTP := "http://thirdwx.qlogo.cn/mmopen/a/132"
	avatarOver := "https://" + strings.Repeat("x", 1024) // > 1024 字节
	empty := ""
	require.NoError(t, call(clientMethod, &clientv1.UpdateAccountRequest{Avatar: &avatarOK}))
	require.NoError(t, call(clientMethod, &clientv1.UpdateAccountRequest{Avatar: &empty})) // 空串 = 清除
	require.Equal(t, codes.InvalidArgument, status.Code(call(clientMethod, &clientv1.UpdateAccountRequest{Avatar: &avatarHTTP})))
	require.Equal(t, codes.InvalidArgument, status.Code(call(clientMethod, &clientv1.UpdateAccountRequest{Avatar: &avatarOver})))
	// 未设置 = 不修改，不触发规则（与 name 同提亦然）。
	require.NoError(t, call(clientMethod, &clientv1.UpdateAccountRequest{Name: &name64, Avatar: &avatarOK}))

	// —— 管理面 UpdateUserRequest 同口径 ——
	require.NoError(t, call(serverMethod, &serverv1.UpdateUserRequest{Name: &name64}))
	require.Equal(t, codes.InvalidArgument, status.Code(call(serverMethod, &serverv1.UpdateUserRequest{Name: &nameOver})))
	require.Equal(t, codes.InvalidArgument, status.Code(call(serverMethod, &serverv1.UpdateUserRequest{Name: &withControl})))
	require.NoError(t, call(serverMethod, &serverv1.UpdateUserRequest{Avatar: &avatarOK}))
	require.Equal(t, codes.InvalidArgument, status.Code(call(serverMethod, &serverv1.UpdateUserRequest{Avatar: &avatarHTTP})))
}
