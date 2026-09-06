// 存量列授权全量 reconcile 扫描测试（转出 POC 门禁 A1，docs/developer/15-exit-poc.md）：
// 门禁判据的集成锁定——构造列授权故意偏离终态的表（手工 REVOKE/GRANT），执行
// 扫描后 information_schema.column_privileges 与 refreshColumnGrants 幂等重建
// 一致（以"从未偏离、建表即终态"的对照集合 + 二次扫描幂等为双向对照）；空库
// 扫描 no-op；幽灵 catalog 行（物理表缺失）单表跳过不中断全量。
package documentdb

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/testutil"
)

// captureColumnPrivileges 抓取 tw_app 在表上的列级授权（information_schema.
// column_privileges，"col:TYPE" 行切片，排序稳定）。表级授权（SELECT/DELETE）
// 不入此视图，与 refreshColumnGrants 的口径一致。
func captureColumnPrivileges(t *testing.T, ctx context.Context, db *clients.Database, schema, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT column_name || ':' || privilege_type
		 FROM information_schema.column_privileges
		 WHERE table_schema = ? AND table_name = ? AND grantee = 'tw_app'
		 ORDER BY column_name, privilege_type`, schema, table)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

// expectedTerminalPrivileges 按终态口径（refreshColumnGrants）计算列级授权集：
// SELECT 全列（表级 GRANT SELECT 经 PG information_schema.column_privileges
// 视图按列展开）；INSERT 除 _tenant 外全列（含 _acl）；UPDATE 再排除 _acl。
func expectedTerminalPrivileges(cols []string) []string {
	out := make([]string, 0, 3*len(cols))
	for _, c := range cols {
		out = append(out, c+":SELECT")
		if c != "_tenant" {
			out = append(out, c+":INSERT")
		}
		if c != "_tenant" && c != "_acl" {
			out = append(out, c+":UPDATE")
		}
	}
	sort.Strings(out)
	return out
}

type grantsReconcileEnv struct {
	db          *clients.Database
	p           *postgresDocumentDB
	project     string
	internalID  int64
	docsSchema  string
	docsTable   string
	mirrorTable string
}

func setupGrantsReconcileEnv(t *testing.T) *grantsReconcileEnv {
	t.Helper()
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	p := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)
	projectID, internalID, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	t.Cleanup(cleanup)
	require.NoError(t, p.CreateDatabase(ctx, projectID, "app", "App DB"))
	attrs := []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "qty", Key: "qty", Type: "integer"},
	}
	perms := []databases.Permission{
		{Type: "read", Role: "any"},
		{Type: "create", Role: "users"},
	}
	require.NoError(t, p.CreateCollection(ctx, projectID, "app", "docs", "Docs", attrs, nil, perms, true))
	// mirror 从不偏离：建表路径 ensureCollectionRLS → refreshColumnGrants 产出
	// 的授权就是终态对照物（门禁判据的"幂等重建结果"侧）。
	require.NoError(t, p.CreateCollection(ctx, projectID, "app", "mirror", "Mirror", attrs, nil, perms, true))
	return &grantsReconcileEnv{
		db:          db,
		p:           p,
		project:     projectID,
		internalID:  internalID,
		docsSchema:  testSchema(t, projectID, "app"),
		docsTable:   testPhysicalName(t, ctx, db, projectID, "app", "docs"),
		mirrorTable: testPhysicalName(t, ctx, db, projectID, "app", "mirror"),
	}
}

// TestGrantsReconcile_DeviationRestored 是门禁 A1 完成判据本体：构造授权故意
// 偏离终态的表（REVOKE 少授 + GRANT 多授 R13a 旧形态）→ 执行扫描 →
// column_privileges 与 refreshColumnGrants 幂等重建一致；幽灵 catalog 行单表
// 跳过不中断全量；扫描幂等（二次扫描零增量）。
func TestGrantsReconcile_DeviationRestored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	env := setupGrantsReconcileEnv(t)
	tbl := env.docsSchema + "." + env.docsTable

	// 故意偏离终态（对齐存量旧授权形态的真实谱系）：
	//   - REVOKE SELECT：少授（表级读丢失，查询路径 42501）；
	//   - GRANT UPDATE (_acl)：R13a 旧形态多授（tw_app 直改 _acl 的旁路复活）；
	//   - GRANT INSERT (_tenant)：多授（预决策 6 的 _tenant 锁死被打开）；
	//   - REVOKE UPDATE (title)：少授（数据列更新 42501）。
	for _, stmt := range []string{
		fmt.Sprintf(`REVOKE SELECT ON %s FROM tw_app`, tbl),
		fmt.Sprintf(`GRANT UPDATE (_acl), INSERT (_tenant) ON %s TO tw_app`, tbl),
		fmt.Sprintf(`REVOKE UPDATE (title) ON %s FROM tw_app`, tbl),
	} {
		_, err := env.db.ExecContext(ctx, stmt)
		require.NoError(t, err, "seed deviation: %s", stmt)
	}
	// 偏离确实可见（对照基线，确认扫描矫正的必要性）。
	before := captureColumnPrivileges(t, ctx, env.db, env.docsSchema, env.docsTable)
	require.Contains(t, before, "_acl:UPDATE", "偏离基线：R13a 旧形态多授在场")
	require.Contains(t, before, "_tenant:INSERT", "偏离基线：_tenant 写通道被打开")
	require.NotContains(t, before, "title:UPDATE", "偏离基线：title 更新被误收走")
	require.NotContains(t, before, "title:SELECT", "偏离基线：表级 SELECT 被误收走（视图按列展开行随之消失）")

	// 幽灵 catalog 行（物理表缺失）：扫描必须单表跳过并继续，不中断全量。
	now := time.Now()
	_, err := env.db.NewInsert().Model(&model.DocumentCollection{
		ProjectID: env.project, DatabaseID: "app", CollectionID: "ghost",
		Name: "Ghost", PhysicalName: "ghost", DocumentSecurity: false,
		Permissions: "[]", Attrs: "[]", Indexes: "[]",
		SchemaVersion: 1, DDLSeq: 1, CreatedAt: now, UpdatedAt: now,
	}).Exec(ctx)
	require.NoError(t, err)

	res, err := ReconcileCollectionColumnGrants(ctx, env.db)
	require.NoError(t, err)
	require.Equal(t, GrantsReconcileResult{Scanned: 3, Reconciled: 2, Missing: 1, Failed: 0}, res)

	// 门禁判据：扫描后 column_privileges 与 refreshColumnGrants 幂等重建一致。
	// 对照一：偏离表恢复到终态精确集。
	docsCols, err := env.p.tableColumns(ctx, env.docsSchema, env.docsTable)
	require.NoError(t, err)
	after := captureColumnPrivileges(t, ctx, env.db, env.docsSchema, env.docsTable)
	require.Equal(t, expectedTerminalPrivileges(docsCols), after)
	// 对照二：与从未偏离的 mirror 表（建表即终态）完全一致。
	mirrorCols, err := env.p.tableColumns(ctx, env.docsSchema, env.mirrorTable)
	require.NoError(t, err)
	mirror := captureColumnPrivileges(t, ctx, env.db, env.docsSchema, env.mirrorTable)
	require.Equal(t, expectedTerminalPrivileges(mirrorCols), mirror)
	require.Equal(t, mirror, after, "偏离→扫描后的授权必须与幂等重建终态逐行一致")

	// 幂等：二次扫描零增量，授权集稳定（顺带记录稳态扫描耗时，供启动开销
	// 评估——全量枚举 + 每表一个 tw_owner 事务的 REVOKE/GRANT 重建）。
	start := time.Now()
	res2, err := ReconcileCollectionColumnGrants(ctx, env.db)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, GrantsReconcileResult{Scanned: 3, Reconciled: 2, Missing: 1, Failed: 0}, res2)
	require.Equal(t, after, captureColumnPrivileges(t, ctx, env.db, env.docsSchema, env.docsTable))
	t.Logf("steady-state scan: scanned=%d elapsed=%v (≈%v/table)", res2.Scanned, elapsed, elapsed/time.Duration(res2.Scanned))

	// 功能级抽验：终态口径真实生效（不只 catalog 视图好看）。
	asApp := func(roles []string, fn func(txCtx context.Context) error) error {
		return env.db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{
			Role: clients.RoleApp, Roles: roles, Tenant: env.internalID,
		}), fn)
	}
	// REVOKE SELECT 已矫正：tw_app 恢复可读（少授偏离若未修复此处即 42501）。
	require.NoError(t, asApp([]string{"any"}, func(txCtx context.Context) error {
		var n int
		return env.db.Conn(txCtx).QueryRowContext(txCtx,
			fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)).Scan(&n)
	}), "扫描必须矫正 REVOKE SELECT 的少授偏离")
	// GRANT UPDATE (_acl) 已收回：tw_app 直改 _acl 仍被列权限封死（R13a）。
	err = asApp([]string{"any"}, func(txCtx context.Context) error {
		_, err := env.db.Conn(txCtx).ExecContext(txCtx,
			fmt.Sprintf(`UPDATE %s SET _acl = '{read:any}' WHERE _id = 'none'`, tbl))
		return err
	})
	require.ErrorContains(t, err, "42501", "扫描必须收回 R13a 旧形态的 UPDATE(_acl) 多授")
	// GRANT INSERT (_tenant) 已收回：_tenant 写通道重新锁死（预决策 6）。
	// roles={users} 使集合级 create policy 放行，42501 只能来自列权限。
	err = asApp([]string{"users"}, func(txCtx context.Context) error {
		_, err := env.db.Conn(txCtx).ExecContext(txCtx,
			fmt.Sprintf(`INSERT INTO %s (_id, _tenant) VALUES ('t-tenant', 999)`, tbl))
		return err
	})
	require.ErrorContains(t, err, "42501", "扫描必须收回 _tenant 列写授权")
	// R16 ③ 合法通道不受扫描影响：INSERT 携带 _acl 仍放行。
	require.NoError(t, asApp([]string{"users"}, func(txCtx context.Context) error {
		_, err := env.db.Conn(txCtx).ExecContext(txCtx,
			fmt.Sprintf(`INSERT INTO %s (_id, _acl) VALUES ('seeded-ok', '{read:any}')`, tbl))
		if err != nil {
			return err
		}
		_, err = env.db.Conn(txCtx).ExecContext(txCtx,
			fmt.Sprintf(`DELETE FROM %s WHERE _id = 'seeded-ok'`, tbl))
		return err
	}), "扫描不得误伤 R16 ③ 的 INSERT 携带 _acl 合法通道")
}

// TestGrantsReconcile_EmptyCatalogNoOp：空库（catalog 无业务集合行）扫描为
// no-op——零计数、零错误、零语句副作用。
func TestGrantsReconcile_EmptyCatalogNoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	res, err := ReconcileCollectionColumnGrants(ctx, db)
	require.NoError(t, err)
	require.Equal(t, GrantsReconcileResult{}, res, "空库扫描必须 no-op（全零计数）")
}
