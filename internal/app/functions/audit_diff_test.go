package functions

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

// TestUpdateFunction_AuditChanges（审计 changes 试点）：UpdateFunction 的
// before/after diff 经 ctx holder 回填——只含被修改字段、from/to 保真；
// 未变更字段不出现在 diff。
func TestUpdateFunction_AuditChanges(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	fn := seedReadyFunction(repo, "p1", "fn_1", true, 15)
	fn.ClientCallable = true
	fn.ClientPerUserLimit = 10
	fn.ClientLimitWindow = "day"

	// holder 由审计拦截器在真实链路预置，测试侧显式安装后读取。
	ctx := contexts.WithAuditMetadataHolder(platformAdminCtx())
	_, err := uc.UpdateFunction(ctx, UpdateFunctionCommand{
		ProjectID:          "p1",
		FunctionID:         "fn_1",
		ClientPerUserLimit: intPtr(20),
		Concurrency:        intPtr(4),
	})
	require.NoError(t, err)

	changes, ok := contexts.AuditMetadata(ctx)["changes"].(map[string]any)
	require.True(t, ok, "changes 应经 holder 回填为 map")

	limit, ok := changes["client_per_user_limit"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 10, limit["from"])
	require.Equal(t, 20, limit["to"])

	concurrency, ok := changes["concurrency"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 0, concurrency["from"])
	require.Equal(t, 4, concurrency["to"])

	// 未修改的字段不出现在 diff（presence 语义）。
	require.NotContains(t, changes, "name")
	require.NotContains(t, changes, "timeout_seconds")
	require.NotContains(t, changes, "client_limit_window")
}

// TestUpdateFunction_AuditChanges_NoDiff：无字段变更时不写 changes
// （种子对齐 DB DEFAULT：concurrency=1，零值会被存量防御归一化成真实写入）。
func TestUpdateFunction_AuditChanges_NoDiff(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	seeded := seedReadyFunction(repo, "p1", "fn_1", true, 15)
	seeded.Concurrency = 1

	ctx := contexts.WithAuditMetadataHolder(platformAdminCtx())
	_, err := uc.UpdateFunction(ctx, UpdateFunctionCommand{
		ProjectID:  "p1",
		FunctionID: "fn_1",
	})
	require.NoError(t, err)

	md := contexts.AuditMetadata(ctx)
	if md != nil {
		require.NotContains(t, md, "changes")
	}
}
