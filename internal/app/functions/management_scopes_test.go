package functions

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// P0 执行身份：declared_scopes 的业务规则（词表/形态校验、自动去重、空集
// 合法）落在 app 层 Create/SetFunctionScopes；词表纯函数的表驱动覆盖在
// domain/functions/scopes_test.go。
func TestCreateFunction_DeclaredScopes(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	ctx := platformAdminCtx()

	t.Run("校验通过并规范化（去重 + 排序）", func(t *testing.T) {
		fn, err := uc.CreateFunction(ctx, CreateFunctionCommand{
			ID: "fn_scopes", ProjectID: "p1", Name: "f", Runtime: "node-18.0",
			DeclaredScopes: []string{"users:read", "assets:write", "assets:write"},
		})
		require.NoError(t, err)
		require.Equal(t, []string{"assets:write", "users:read"}, fn.DeclaredScopes)
	})

	t.Run("空集合法（无平台访问权限）", func(t *testing.T) {
		fn, err := uc.CreateFunction(ctx, CreateFunctionCommand{
			ID: "fn_noscope", ProjectID: "p1", Name: "f", Runtime: "node-18.0",
		})
		require.NoError(t, err)
		require.Empty(t, fn.DeclaredScopes)
	})

	t.Run("危险资源拒绝（functions 自我复制面）", func(t *testing.T) {
		_, err := uc.CreateFunction(ctx, CreateFunctionCommand{
			ID: "fn_bad", ProjectID: "p1", Name: "f", Runtime: "node-18.0",
			DeclaredScopes: []string{"functions:write"},
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("非法形态拒绝", func(t *testing.T) {
		_, err := uc.CreateFunction(ctx, CreateFunctionCommand{
			ID: "fn_bad2", ProjectID: "p1", Name: "f", Runtime: "node-18.0",
			DeclaredScopes: []string{"assets.write"},
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.True(t, strings.Contains(status.Convert(err).Message(), "must match") || strings.Contains(status.Convert(err).Message(), "not allowed"))
	})
}

func TestSetFunctionScopes(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	seedReadyFunction(repo, "p1", "fn_1", true, 15)

	t.Run("全量替换 + 规范化", func(t *testing.T) {
		fn, err := uc.SetFunctionScopes(platformAdminCtx(), "p1", "fn_1",
			[]string{"databases:read", "assets:write", "assets:write"})
		require.NoError(t, err)
		require.Equal(t, []string{"assets:write", "databases:read"}, fn.DeclaredScopes)

		stored, err := repo.GetFunction(context.Background(), "p1", "fn_1")
		require.NoError(t, err)
		require.Equal(t, []string{"assets:write", "databases:read"}, stored.DeclaredScopes, "全量替换落库")
	})

	t.Run("空集撤销全部平台访问", func(t *testing.T) {
		fn, err := uc.SetFunctionScopes(platformAdminCtx(), "p1", "fn_1", nil)
		require.NoError(t, err)
		require.Empty(t, fn.DeclaredScopes)
	})

	t.Run("词表外资源拒绝且不落库", func(t *testing.T) {
		_, err := uc.SetFunctionScopes(platformAdminCtx(), "p1", "fn_1", []string{"billing:write"})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		stored, err := repo.GetFunction(context.Background(), "p1", "fn_1")
		require.NoError(t, err)
		require.Empty(t, stored.DeclaredScopes, "拒绝路径不得部分写入")
	})

	t.Run("函数不存在 404", func(t *testing.T) {
		_, err := uc.SetFunctionScopes(platformAdminCtx(), "p1", "fn_missing", []string{"assets:read"})
		require.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("端用户拒绝（RequireServerPrincipal）", func(t *testing.T) {
		ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
			ActorKind: shared.ActorKindEndUser, UserID: "u1",
		})
		_, err := uc.SetFunctionScopes(ctx, "p1", "fn_1", nil)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})
}
