package functions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 池策略管理面（v3 §5/OQ2：UpdateFunction optional ×5）：presence 语义
// （未设置不修改）、单字段值域（与 DB CHECK 同源）、min≤max 跨字段校验。
func TestUpdateFunction_PoolPolicy(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	seedReadyFunction(repo, "p1", "fn_1", true, 15)

	t.Run("presence:未设置字段不修改", func(t *testing.T) {
		fn, err := uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			Concurrency: intPtr(4),
		})
		require.NoError(t, err)
		require.Equal(t, 4, fn.Concurrency)
		// 其余池列保持种子零值/缺省不动（只改了 concurrency）。
		require.Equal(t, 0, fn.MinInstances)
		require.Equal(t, 0, fn.MaxInstances)
		require.Equal(t, 0, fn.IdleTTLSeconds)
		require.Equal(t, 0, fn.MaxRequestsPerInstance)

		stored, err := repo.GetFunction(context.Background(), "p1", "fn_1")
		require.NoError(t, err)
		require.Equal(t, 4, stored.Concurrency, "落库")
	})

	t.Run("五字段同请求全量更新", func(t *testing.T) {
		fn, err := uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			MinInstances:           intPtr(2),
			MaxInstances:           intPtr(8),
			IdleTTLSeconds:         intPtr(60),
			MaxRequestsPerInstance: intPtr(100),
			Concurrency:            intPtr(16),
		})
		require.NoError(t, err)
		require.Equal(t, 2, fn.MinInstances)
		require.Equal(t, 8, fn.MaxInstances)
		require.Equal(t, 60, fn.IdleTTLSeconds)
		require.Equal(t, 100, fn.MaxRequestsPerInstance)
		require.Equal(t, 16, fn.Concurrency)
	})

	t.Run("跨字段:min_instances > max_instances 拒绝", func(t *testing.T) {
		_, err := uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			MinInstances: intPtr(9), // 存量 max=8
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))

		_, err = uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			MaxInstances: intPtr(1), // 存量 min=2
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("跨字段:同请求内 min≤max 合法", func(t *testing.T) {
		_, err := uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			MinInstances: intPtr(3),
			MaxInstances: intPtr(3),
		})
		require.NoError(t, err)
	})

	t.Run("值域兜底:concurrency 越界拒绝", func(t *testing.T) {
		_, err := uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			Concurrency: intPtr(17),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))

		_, err = uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			Concurrency: intPtr(0),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("值域兜底:idle_ttl < 30 与 max_requests < 1 拒绝", func(t *testing.T) {
		_, err := uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			IdleTTLSeconds: intPtr(29),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))

		_, err = uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			MaxRequestsPerInstance: intPtr(0),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("值域兜底:min < 0 / max < 1 拒绝", func(t *testing.T) {
		_, err := uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			MinInstances: intPtr(-1),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))

		_, err = uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			MaxInstances: intPtr(0),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("拒绝路径不落库", func(t *testing.T) {
		_, err := uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_1",
			IdleTTLSeconds: intPtr(1),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		stored, err := repo.GetFunction(context.Background(), "p1", "fn_1")
		require.NoError(t, err)
		require.Equal(t, 60, stored.IdleTTLSeconds, "拒绝路径不得部分写入")
	})

	t.Run("函数不存在 404", func(t *testing.T) {
		_, err := uc.UpdateFunction(platformAdminCtx(), UpdateFunctionCommand{
			ProjectID: "p1", FunctionID: "fn_missing",
			Concurrency: intPtr(2),
		})
		require.Equal(t, codes.NotFound, status.Code(err))
	})
}
