// roles_sig 双钥轮换窗口测试（转出 POC 门禁 A4，方案①，15-exit-poc）：
//   - 验收判据①：换钥后旧钥签发的 sig 在 60s TTL 窗口内仍验签通过
//     （previous 槽命中），RLS 全路径（policy → tw_roles → tw_tenant）不降级；
//   - 验收判据②：窗口外（exp 过期）旧钥 sig 拒绝——过期判定先于钥匹配，
//     previous 命中无法给过期 sig 续命；
//   - third 条直接删：连续两次换钥后两代前 sig 拒绝、行数回到 2（Go 侧
//     平移逻辑的行数不变量）+ 表级约束（每 purpose 至多一把 current）；
//   - 同钥重启幂等（current 已是目标钥时整体 no-op）与回滚场景（目标钥在
//     previous 位时提回 current）。
package documentdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
)

const (
	dualKeyPurpose  = "tw-roles-guc-v1"
	dualKeyMasterG1 = "dualkey-rotation-master-gen1-0000000000000000"
	dualKeyMasterG2 = "dualkey-rotation-master-gen2-0000000000000000"
	dualKeyMasterG3 = "dualkey-rotation-master-gen3-0000000000000000"
)

// TestRolesSig_DualKeyRotationWindow 换钥时序（钥状态迁移矩阵；fixture
// SetupTestDB 已先同步过测试主密钥，故 G1 的首次 sync 也是一次换钥）：
//
//	操作               current  previous   断言
//	──────────────────────────────────────────────────────────────
//	fixture            TM       ∅          行数 1
//	sync(G1)           G1       TM         行数 2
//	sync(G2)           G2       G1         G1 sig 窗口内通过；
//	                                       G1 过期 sig 窗口外拒绝
//	重新 sync(G2)      G2       G1         行数 2（幂等 no-op）
//	sync(G3)           G3       G2         行数 2（G1 third 条被删）；
//	                                       G1 sig 拒绝、G2 sig 窗口内通过
//	sync(G1)（回滚）   G1       G3         G1 落 current、previous 平移为 G3
func TestRolesSig_DualKeyRotationWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	f := newSigFixture(ctx, t)
	count := countVisible(t, f.db, f.tbl)
	rolesN := probe(t, f.db, `SELECT cardinality(public.tw_roles())`)
	tenantV := probe(t, f.db, `SELECT public.tw_tenant()`)

	// InitRolesSigKey 是进程全局态：用例结束后还原测试主密钥，避免影响
	// 同包后续用例（SetupTestDB 也会重初始化，此处双保险）。
	t.Cleanup(func() { _ = clients.InitRolesSigKey(testutil.TestRolesSigMaster) })

	// rotate 模拟"改 security.jwt.secret 后重启"：进程内重派生 + 启动钩子
	// 同步落库，返回新钥 hex（供以旧钥身份签发时代 sig）。
	rotate := func(master string) string {
		require.NoError(t, clients.InitRolesSigKey(master))
		keyHex, ok := clients.RolesSigKeyHex()
		require.True(t, ok)
		require.NoError(t, clients.SyncRolesSigKey(ctx, f.db))
		return keyHex
	}
	rowsFor := func() int {
		t.Helper()
		var n int
		require.NoError(t, f.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM public.tw_secrets WHERE purpose = ?`, dualKeyPurpose).Scan(&n))
		return n
	}
	// slot 断言 current/previous 槽位的钥（十六进制）。
	slotKey := func(current bool) string {
		t.Helper()
		var k string
		require.NoError(t, f.db.QueryRowContext(ctx,
			`SELECT key_hex FROM public.tw_secrets WHERE purpose = ? AND is_current = ?`,
			dualKeyPurpose, current).Scan(&k))
		return k
	}

	// 初始态：fixture 的 SetupTestDB 已同步测试主密钥（仅 current 一行）。
	require.EqualValues(t, 1, rowsFor(), "初始仅 current 一行")
	// 换钥前：G1 为 current，签发"换钥前时代"的 sig（exp = now+60s，窗口内）
	// 与一份窗口外（exp 已过期）的 G1 sig。
	k1 := rotate(dualKeyMasterG1)
	require.Equal(t, k1, slotKey(true))
	require.EqualValues(t, 2, rowsFor(), "G1 换钥后 current+previous 两行")
	g1Sig := clients.SignRolesSig(k1, f.internalID, "any", time.Now())
	g1Expired := clients.SignRolesSig(k1, f.internalID, "any", time.Now().Add(-2*time.Minute))

	// 换钥（G1→G2）：旧 current 降级 previous，新钥落 current。
	k2 := rotate(dualKeyMasterG2)
	require.Equal(t, k2, slotKey(true), "新钥必须落在 current 位")
	require.Equal(t, k1, slotKey(false), "旧钥必须降级 previous（而非删除）")
	require.EqualValues(t, 2, rowsFor(), "换钥后 current+previous 两行")

	// 验收判据①：换钥后旧钥 sig 在 60s 窗口内仍验签通过（previous 命中），
	// 角色解包 / tenant 解包 / RLS 可见全链路不降级。
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any", g1Sig, func(txCtx context.Context) error {
		require.EqualValues(t, 1, rolesN(txCtx), "换钥后旧钥 sig（60s 窗口内）必须验签通过")
		require.EqualValues(t, f.internalID, tenantV(txCtx), "previous 命中同样解包出 tenant")
		require.EqualValues(t, 1, count(txCtx), "旧钥 sig → tw_roles 解包 → policy 放行 → 可见")
		return nil
	}))

	// 验收判据②：窗口外（exp 过期）旧钥 sig 拒绝——过期判定先于钥匹配。
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any", g1Expired, func(txCtx context.Context) error {
		require.EqualValues(t, 0, rolesN(txCtx), "过期旧钥 sig（窗口外）必须拒绝")
		require.Nil(t, tenantV(txCtx), "过期旧钥 sig → tw_tenant() NULL")
		require.EqualValues(t, 0, count(txCtx))
		return nil
	}))

	// 双钥在场时新钥（current）正常注入路径回归：合法注入 → 可见。
	require.NoError(t, f.db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{
		Role: clients.RoleApp, Roles: []string{"any"}, Tenant: f.internalID,
	}), func(txCtx context.Context) error {
		require.EqualValues(t, 1, count(txCtx), "双钥在场时 current 注入路径不受影响")
		return nil
	}))

	// 同钥重启幂等：重复 sync(G2) 不改变槽位与行数。
	rotate(dualKeyMasterG2)
	require.Equal(t, k2, slotKey(true))
	require.Equal(t, k1, slotKey(false))
	require.EqualValues(t, 2, rowsFor(), "同钥重启必须幂等（无新增行）")

	// 二次换钥（G2→G3）：G1 的 sig 失效（third 条直接删）。
	k3 := rotate(dualKeyMasterG3)
	require.Equal(t, k3, slotKey(true))
	require.Equal(t, k2, slotKey(false), "previous 只保留紧邻上一把")
	require.EqualValues(t, 2, rowsFor(), "third 条直接删——行数不变量 ≤2")
	g1SigAfterG3 := clients.SignRolesSig(k1, f.internalID, "any", time.Now())
	g2Sig := clients.SignRolesSig(k2, f.internalID, "any", time.Now())
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any", g1SigAfterG3, func(txCtx context.Context) error {
		require.EqualValues(t, 0, rolesN(txCtx), "两代前的 sig（third 条已删）必须拒绝")
		return nil
	}))
	require.NoError(t, asAppWithGUC(ctx, f.db, f.internalID, "any", g2Sig, func(txCtx context.Context) error {
		require.EqualValues(t, 1, rolesN(txCtx), "紧邻上一把（previous）sig 仍验签通过")
		return nil
	}))

	// 回滚场景（G3→G1）：目标钥不在任何槽位，正常落 current；previous 平移
	// 为 G3（G2 被裁掉）。
	k1Again := rotate(dualKeyMasterG1)
	require.Equal(t, k1, k1Again, "同主密钥重派生结果一致")
	require.Equal(t, k1, slotKey(true))
	require.Equal(t, k3, slotKey(false))
	require.EqualValues(t, 2, rowsFor())

	// 表级约束：每 purpose 至多一把 current（部分唯一索引）——直插第二条
	// current 必须被拒绝。
	_, err := f.db.ExecContext(ctx,
		`INSERT INTO public.tw_secrets (purpose, key_hex, is_current) VALUES (?, ?, TRUE)`,
		dualKeyPurpose, "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface")
	require.Error(t, err, "第二条 current 必须被部分唯一索引 tw_secrets_single_current 拒绝")

	// 合法注入路径在双钥 + 约束验证后仍全绿（收尾回归：current 签名 →
	// tw_roles 解包 → policy 放行）。
	require.NoError(t, f.db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{
		Role: clients.RoleApp, Roles: []string{"any"}, Tenant: f.internalID,
	}), func(txCtx context.Context) error {
		require.EqualValues(t, 1, rolesN(txCtx))
		require.EqualValues(t, f.internalID, tenantV(txCtx))
		require.EqualValues(t, 1, count(txCtx))
		return nil
	}))
}
