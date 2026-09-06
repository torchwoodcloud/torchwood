// roles_sig 密钥面收口时序测试（转出 POC 门禁 B15）：部署顺序 = 迁移（000004）
// → `torchwood admin sync-roles-sig`（owner 身份落库）→ 服务启动；服务在密钥
// 未落库时启动 → tw_roles() 验签 fail-closed（零角色）→ 文档查询不可见，属
// 预期（既有 000004 fail-closed 语义：与漏注入/错 sig 同型，首个业务查询暴露
// 而非静默放行）。本测试以 "DELETE tw_secrets 模拟 owner 未跑作业 → 零角色
// 不可见 → owner 身份重跑作业 → 恢复" 锁定该时序契约。
package documentdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwooddev/torchwood/internal/infra/clients"
)

func TestRolesSig_KeyNotSeeded_FailClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	f := newSigFixture(ctx, t)
	count := countVisible(t, f.db, f.tbl)
	rolesN := probe(t, f.db, `SELECT cardinality(public.tw_roles())`)
	tenantV := probe(t, f.db, `SELECT public.tw_tenant()`)
	keyHex, ok := clients.RolesSigKeyHex()
	require.True(t, ok, "testutil.SetupTestDB 已初始化签名密钥")

	// 基线：合法 sig 注入 → 可见（fixture 已落库测试主密钥）。
	sig := clients.SignRolesSig(keyHex, f.internalID, "any", time.Now())
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any", sig, func(txCtx context.Context) error {
		require.EqualValues(t, 1, rolesN(txCtx))
		require.EqualValues(t, f.internalID, tenantV(txCtx))
		require.EqualValues(t, 1, count(txCtx))
		return nil
	}))

	// 模拟"首次部署 / 换钥后 owner 未跑 sync 作业"：密钥面清空（表空，
	// tw_sig_match 无钥可命中）。运行态进程内钥仍在（InitRolesSigSigning
	// 照常派生、注入照常携带 sig），但 DB 侧验签无钥 → fail-closed。
	_, err := f.db.ExecContext(ctx, `DELETE FROM public.tw_secrets`)
	require.NoError(t, err)
	sigUnseeded := clients.SignRolesSig(keyHex, f.internalID, "any", time.Now())
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any", sigUnseeded, func(txCtx context.Context) error {
		require.EqualValues(t, 0, rolesN(txCtx), "钥未落库时 tw_roles 必须零角色（fail-closed）")
		require.Nil(t, tenantV(txCtx), "钥未落库时 tw_tenant 必须 NULL")
		require.EqualValues(t, 0, count(txCtx), "钥未落库时文档查询不可见（零角色 → RLS 恒 false）")
		return nil
	}))

	// owner 身份重跑部署作业（SetupTestDB 的 DSN 即 owner 引导账号形态，
	// 等价 `torchwood admin sync-roles-sig` 内部路径）→ 恢复可见。
	require.NoError(t, clients.SyncRolesSigKey(ctx, f.db))
	sigAfter := clients.SignRolesSig(keyHex, f.internalID, "any", time.Now())
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any", sigAfter, func(txCtx context.Context) error {
		require.EqualValues(t, 1, rolesN(txCtx), "owner 落库后 tw_roles 恢复解包")
		require.EqualValues(t, f.internalID, tenantV(txCtx))
		require.EqualValues(t, 1, count(txCtx), "owner 落库后文档查询恢复可见")
		return nil
	}))
}
