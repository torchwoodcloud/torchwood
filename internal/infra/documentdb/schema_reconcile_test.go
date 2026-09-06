// schema 漂移对账测试（转出 POC 门禁 B3 三件套之三）：repair 对注入的
// 缺列 / INVALID 索引 / 幽灵表三类漂移各有修复测试（判据原文）；dry-run
// 只报告不落 DDL；building 超时残留的 reconcile 级中断恢复（valid 补账 /
// 缺失重建 / 活 CIC 不动）；无主 INVALID 索引清理；幂等二次扫描零增量。
package documentdb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
)

func setupReconcileEnv(t *testing.T) *onlineIndexEnv {
	return setupOnlineIndexEnv(t, 5)
}

// columnExists 查物理列是否存在。
func (env *onlineIndexEnv) columnExists(t *testing.T, ctx context.Context, col string) bool {
	t.Helper()
	var n int
	require.NoError(t, env.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = ? AND column_name = ?`, env.schema, env.physical, col).Scan(&n))
	return n > 0
}

// ghostTableExists 查幽灵表是否在场。
func (env *onlineIndexEnv) ghostTableExists(t *testing.T, ctx context.Context, name string) bool {
	t.Helper()
	var n int
	require.NoError(t, env.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pg_tables WHERE schemaname = ? AND tablename = ?`, env.schema, name).Scan(&n))
	return n > 0
}

// injectInvalidIndex 注入 INVALID 物理索引：unique CIC 撞存量重复（PG 语义：
// CIC 失败留下 indisvalid=false 的残留）。catalog 未登记（无主残留形态）或
// 由调用方自行登记。
func (env *onlineIndexEnv) injectInvalidIndex(t *testing.T, ctx context.Context, indexID string) {
	t.Helper()
	// 同名合法索引先清（CIC 的 IF NOT EXISTS 会命中既有名直接跳过）。
	env.dropPhysicalIndexDirect(t, ctx, indexID)
	// 重复值（unique 不可满足）。
	for _, id := range []string{"iv-dup-1", "iv-dup-2"} {
		_, err := env.p.CreateDocument(ctx, env.project, env.database, env.collection, databases.Document{
			ID: id, Data: map[string]any{"code": "ivdup"},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}
	err := env.p.createIndexConcurrently(ctx, env.schema, env.physical, databases.Index{
		ID: indexID, Type: "unique", Attributes: []string{"code"},
	}, []databases.Attribute{{ID: "code", Key: "code", Type: "string", Size: 64}})
	require.Error(t, err, "注入依赖：CIC 撞重复必须失败并残留 INVALID 索引")
	require.True(t, env.physicalIndexExists(t, ctx, indexID))
	require.False(t, env.physicalIndexValid(t, ctx, indexID))
	// 清理重复值（后续重建必须可成功）。
	for _, id := range []string{"iv-dup-1", "iv-dup-2"} {
		require.NoError(t, env.p.DeleteDocument(ctx, env.project, env.database, env.collection, id,
			databases.DeleteOptions{SkipVersion: true}, databases.SystemPrincipal))
	}
}

// appendIndexEntryDirect 绕过状态机向 catalog 追加索引条目（模拟崩溃残留/
// failed 遗留的直写通道）。
func (env *onlineIndexEnv) appendIndexEntryDirect(t *testing.T, ctx context.Context, idx databases.Index) {
	t.Helper()
	m := new(model.DocumentCollection)
	require.NoError(t, env.db.NewSelect().Model(m).
		Where("project_id = ? AND database_id = ? AND collection_id = ?", env.project, env.database, env.collection).
		Scan(ctx))
	idxs, err := decodeIndexes(m.Indexes)
	require.NoError(t, err)
	idxs = append(idxs, idx)
	idxsJSON, err := encodeIndexes(idxs)
	require.NoError(t, err)
	m.Indexes = idxsJSON
	_, err = env.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	require.NoError(t, err)
}

// itemsOf 过滤报告条目。
func itemsOf(rep *SchemaDriftReport, kind string) []DriftItem {
	var out []DriftItem
	for _, it := range rep.Items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

// TestSchemaReconcile_ThreeDriftClasses 是门禁判据本体：注入缺列 / INVALID
// 索引 / 幽灵表三类漂移，dry-run 三类全检出且零修复；repair 三类全修复且
// 功能级生效；二次扫描幂等零增量。
func TestSchemaReconcile_ThreeDriftClasses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupReconcileEnv(t)
	tbl := env.schema + "." + env.physical

	// 漂移 1：缺列——catalog 有 qty attr、物理列被带外 DROP。
	_, err := env.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s DROP COLUMN qty`, tbl))
	require.NoError(t, err)
	require.False(t, env.columnExists(t, ctx, "qty"))

	// 漂移 2：INVALID 索引——带外 CIC 失败残留（无主形态）+ catalog active
	// 条目指向 INVALID 索引（有主形态）。
	env.injectInvalidIndex(t, ctx, "orphan_invalid")
	require.NoError(t, env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "tracked_idx", Type: "key", Attributes: []string{"code"},
	}))
	env.injectInvalidIndex(t, ctx, "tracked_idx")

	// 漂移 3：幽灵表——业务 schema 内 catalog 无行的物理表。
	_, err = env.db.ExecContext(ctx,
		fmt.Sprintf(`CREATE TABLE %s."%s" (id int)`, env.schema, reconcileGhostName(env.physical)))
	require.NoError(t, err)
	require.True(t, env.ghostTableExists(t, ctx, reconcileGhostName(env.physical)))

	// dry-run：三类全检出、零修复、零失败。
	dry, err := ReconcileSchemaDrift(ctx, env.db, SchemaReconcileOptions{DryRun: true})
	require.NoError(t, err)
	require.False(t, env.columnExists(t, ctx, "qty"), "dry-run 不得落 DDL")
	require.True(t, env.physicalIndexExists(t, ctx, "orphan_invalid"), "dry-run 不得清残留")
	require.True(t, env.ghostTableExists(t, ctx, reconcileGhostName(env.physical)), "dry-run 不得删表")
	require.Len(t, itemsOf(&dry, DriftMissingColumn), 1, "缺列检出: %+v", dry.Items)
	require.Len(t, itemsOf(&dry, DriftInvalidIndex), 2, "INVALID 索引（无主+有主）检出: %+v", dry.Items)
	require.Len(t, itemsOf(&dry, DriftGhostTable), 1, "幽灵表检出: %+v", dry.Items)
	for _, it := range itemsOf(&dry, DriftMissingColumn) {
		require.Equal(t, DriftDetected, it.Action)
	}

	// repair：三类全修复。
	rep, err := ReconcileSchemaDrift(ctx, env.db, SchemaReconcileOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, len(itemsOf(&rep, DriftMissingColumn)))
	require.Equal(t, DriftFixed, itemsOf(&rep, DriftMissingColumn)[0].Action)
	require.Equal(t, 2, len(itemsOf(&rep, DriftInvalidIndex)))
	for _, it := range itemsOf(&rep, DriftInvalidIndex) {
		require.Equal(t, DriftFixed, it.Action)
	}
	require.Equal(t, 1, len(itemsOf(&rep, DriftGhostTable)))
	require.Equal(t, DriftFixed, itemsOf(&rep, DriftGhostTable)[0].Action)
	require.Equal(t, 0, rep.Failed, "items: %+v", rep.Items)

	// 功能级验证：
	// 1) 缺列恢复——类型正确 + 列授权刷新 + 可写。
	require.True(t, env.columnExists(t, ctx, "qty"))
	_, err = env.p.CreateDocument(ctx, env.project, env.database, env.collection, databases.Document{
		Data: map[string]any{"code": "post-repair", "qty": int64(7)},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err, "修复后 qty 列必须可写（列授权刷新）")
	// 2) INVALID 索引——无主残留已清、有主条目重建为 valid + active。
	require.False(t, env.physicalIndexExists(t, ctx, "orphan_invalid"), "无主 INVALID 残留必须清除")
	require.True(t, env.physicalIndexValid(t, ctx, "tracked_idx"))
	require.Equal(t, databases.IndexStatusActive, env.catalogIndexStatus(t, ctx, "tracked_idx"))
	// 3) 幽灵表已删。
	require.False(t, env.ghostTableExists(t, ctx, reconcileGhostName(env.physical)))

	// 幂等：二次扫描零增量（对照：首轮 4 项修复）。
	rep2, err := ReconcileSchemaDrift(ctx, env.db, SchemaReconcileOptions{})
	require.NoError(t, err)
	require.Equal(t, 0, len(rep2.Items), "稳态扫描必须零漂移: %+v", rep2.Items)
	require.Equal(t, 0, rep2.Failed)
}

// reconcileGhostName 拼一个合法的幽灵表名（c_ghost_ 前缀 + 真实名变体，
// 不与任何集合物理表名冲突）。
func reconcileGhostName(physical string) string {
	return "c_ghost_" + physical[2:]
}

// TestSchemaReconcile_BuildingResidualRecovery：reconcile 级中断恢复——
// stale building + valid 物理索引 → 补账 active（不重建）；stale building +
// 缺失物理索引 → CIC 重建；新鲜 building（活 CIC）→ 不动。
func TestSchemaReconcile_BuildingResidualRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupReconcileEnv(t)

	require.NoError(t, env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "recovery_a", Type: "key", Attributes: []string{"code"},
	}))
	require.NoError(t, env.p.CreateIndex(ctx, env.project, env.database, env.collection, databases.Index{
		ID: "recovery_b", Type: "key", Attributes: []string{"qty"},
	}))

	// 情形 1：崩溃在事务 B 前——物理 valid、catalog building（推老越过超时）。
	env.markIndexStatusDirect(t, ctx, "recovery_a", databases.IndexStatusBuilding, 2*time.Hour)
	// 情形 2：崩溃在 CIC 前——物理缺失、catalog building。
	env.dropPhysicalIndexDirect(t, ctx, "recovery_b")
	env.markIndexStatusDirect(t, ctx, "recovery_b", databases.IndexStatusBuilding, 2*time.Hour)

	rep, err := ReconcileSchemaDrift(ctx, env.db, SchemaReconcileOptions{
		BuildingStaleAfter: time.Minute, // 收窄超时（updated_at 已推老 2h）
	})
	require.NoError(t, err)
	require.Equal(t, 1, len(itemsOf(&rep, DriftBuildingValid)))
	require.Equal(t, DriftFixed, itemsOf(&rep, DriftBuildingValid)[0].Action)
	require.Equal(t, 1, len(itemsOf(&rep, DriftBuildingRebuilt)))
	require.Equal(t, DriftFixed, itemsOf(&rep, DriftBuildingRebuilt)[0].Action)
	require.Equal(t, 0, rep.Failed, "items: %+v", rep.Items)

	require.Equal(t, databases.IndexStatusActive, env.catalogIndexStatus(t, ctx, "recovery_a"))
	require.True(t, env.physicalIndexValid(t, ctx, "recovery_a"), "valid 残留必须补账而非重建")
	require.Equal(t, databases.IndexStatusActive, env.catalogIndexStatus(t, ctx, "recovery_b"))
	require.True(t, env.physicalIndexValid(t, ctx, "recovery_b"), "缺失残留必须重建")

	// 情形 3：新鲜 building（updated_at 未超时）= 活 CIC——扫描不动。
	env.markIndexStatusDirect(t, ctx, "recovery_a", databases.IndexStatusBuilding, 0)
	rep2, err := ReconcileSchemaDrift(ctx, env.db, SchemaReconcileOptions{
		BuildingStaleAfter: time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, 0, len(itemsOf(&rep2, DriftBuildingValid)), "活 CIC 不得被误清: %+v", rep2.Items)
	require.Equal(t, databases.IndexStatusBuilding, env.catalogIndexStatus(t, ctx, "recovery_a"))
}

// TestSchemaReconcile_FailedEntryReentry：failed 条目 reconcile 重入——
// 残留 INVALID 清理后重建收敛 active；重入再失败落回 failed（单轮单次）。
func TestSchemaReconcile_FailedEntryReentry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupReconcileEnv(t)

	// failed 条目 + INVALID 残留（重复值已由 injectInvalidIndex 清理）。
	env.injectInvalidIndex(t, ctx, "heal_me")
	env.appendIndexEntryDirect(t, ctx, databases.Index{
		ID: "heal_me", Type: "unique", Attributes: []string{"code"}, Status: databases.IndexStatusFailed,
	})

	rep, err := ReconcileSchemaDrift(ctx, env.db, SchemaReconcileOptions{
		BuildingStaleAfter: time.Minute,
	})
	require.NoError(t, err)
	require.Equal(t, 1, len(itemsOf(&rep, DriftFailedRebuilt)))
	require.Equal(t, DriftFixed, itemsOf(&rep, DriftFailedRebuilt)[0].Action)
	require.Equal(t, databases.IndexStatusActive, env.catalogIndexStatus(t, ctx, "heal_me"))
	require.True(t, env.physicalIndexValid(t, ctx, "heal_me"))
}
