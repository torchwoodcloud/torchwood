// 执行身份与角色分层测试（阶段③包 B，redesign §3.2 工程纪律 / A1 / §11-J 原型任务）：
//   - 注入正确性：SET LOCAL ROLE / app.roles 事务内可见、事务外零残留；
//   - 中段切换恢复：ctx 身份与连接角色在 withDocumentTx 边界一致；
//   - 角色分层：tw_app 不得 DDL，tw_owner 可建表；
//   - 原型① SET LOCAL ROLE 到 BYPASSRLS 角色后旁路生效（current_user 语义）；
//   - 原型② 每请求一事务 vs autocommit 的往返开销（多语句合并单往返）。
package documentdb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
)

func TestExecIdentity_InjectionScopedToTx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	docDB := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)
	id := clients.ExecIdentity{Role: clients.RoleApp, Roles: []string{"any", "user:bob"}}

	var curUser, roles string
	require.NoError(t, docDB.withDocumentTx(ctx, id, func(txCtx context.Context) error {
		return db.Conn(txCtx).QueryRowContext(txCtx,
			`SELECT current_user, current_setting('app.roles', true)`).Scan(&curUser, &roles)
	}))
	require.Equal(t, clients.RoleApp, curUser)
	require.Equal(t, "any\x1fuser:bob", roles)

	// 事务结束零残留（SET LOCAL 语义）：新语句回 authenticator；app.roles 回落
	// 空串占位（自定义 GUC 在同连接上 RESET 后为 ''，非 NULL——string_to_array('')
	// = {} 同样零角色，fail-closed 语义不变）。
	var afterUser string
	var afterRoles string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT current_user, COALESCE(current_setting('app.roles', true), '')`).Scan(&afterUser, &afterRoles))
	require.Equal(t, "torchwood", afterUser, "authenticator 本身份")
	var afterCard int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT cardinality(string_to_array(COALESCE(current_setting('app.roles', true), ''), chr(31)))`).Scan(&afterCard))
	require.Zero(t, afterCard, "app.roles 事务外必须解包为零角色（fail-closed）")
}

func TestExecIdentity_MidTxSwitchRestoresOuter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	docDB := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)

	// 复合 uow 事务（无身份）内嵌 withDocumentTx：内层见 tw_app，退出后回
	// authenticator（RESET ROLE）——后续外层语句不受内层切换影响。
	var inner, outer string
	require.NoError(t, db.RunInTx(ctx, func(txCtx context.Context) error {
		if err := docDB.withDocumentTx(txCtx, clients.ExecIdentity{Role: clients.RoleApp, Roles: []string{"any"}}, func(innerCtx context.Context) error {
			return db.Conn(innerCtx).QueryRowContext(innerCtx, `SELECT current_user`).Scan(&inner)
		}); err != nil {
			return err
		}
		return db.Conn(txCtx).QueryRowContext(txCtx, `SELECT current_user`).Scan(&outer)
	}))
	require.Equal(t, clients.RoleApp, inner)
	require.Equal(t, "torchwood", outer, "中段切换退出后必须恢复 authenticator")
}

func TestExecIdentity_RoleSeparation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	docDB := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)
	schema := "tw_exec_identity_sep"
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, quoteIdent(schema)))
	})

	// tw_owner 可建 schema + 表（CREATE ON DATABASE 由 000004 授予）。
	require.NoError(t, docDB.withOwnerTx(ctx, func(txCtx context.Context) error {
		return docDB.ensureSchema(txCtx, schema)
	}))

	// tw_app 不得建表（角色分层：DDL 归 tw_owner）。
	err := docDB.withDocumentTx(ctx, clients.ExecIdentity{Role: clients.RoleApp, Roles: []string{"any"}}, func(txCtx context.Context) error {
		_, err := db.Conn(txCtx).ExecContext(txCtx,
			fmt.Sprintf(`CREATE TABLE %s.t_app (id INT)`, quoteIdent(schema)))
		return err
	})
	require.Error(t, err, "tw_app 必须被拒绝 DDL")
}

// TestExecIdentity_BypassRLSViaSetLocalRole 是 A1 遗留原型任务①：
// SET LOCAL ROLE 到 BYPASSRLS 角色（tw_system）后，RLS 旁路是否按 current_user
// 语义生效。若不生效，本测试红——fallback 方案（独立 system DSN）需回设计。
func TestExecIdentity_BypassRLSViaSetLocalRole(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	docDB := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)
	tbl := "tw_proto_rls_bypass"
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
CREATE TABLE %s (id INT, _acl TEXT[] NOT NULL DEFAULT '{}');
ALTER TABLE %s ENABLE ROW LEVEL SECURITY;
ALTER TABLE %s FORCE ROW LEVEL SECURITY;
CREATE POLICY p_proto ON %s FOR SELECT USING (current_setting('app.roles', true) = 'any');
INSERT INTO %s VALUES (1, '{}');
GRANT SELECT ON %s TO tw_app, tw_system;
`, tbl, tbl, tbl, tbl, tbl, tbl))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tbl)) })

	count := func(txCtx context.Context) int {
		var n int
		require.NoError(t, db.Conn(txCtx).QueryRowContext(txCtx,
			fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)).Scan(&n))
		return n
	}

	// tw_app 未注入 roles → GUC NULL → policy false → 0 行（fail-closed）。
	require.NoError(t, docDB.withDocumentTx(ctx, clients.ExecIdentity{Role: clients.RoleApp}, func(txCtx context.Context) error {
		require.Zero(t, count(txCtx), "漏注入 roles 必须 0 行（fail-closed）")
		return nil
	}))

	// tw_app 注入 any → 可见 1 行。
	require.NoError(t, docDB.withDocumentTx(ctx, clients.ExecIdentity{Role: clients.RoleApp, Roles: []string{"any"}}, func(txCtx context.Context) error {
		require.Equal(t, 1, count(txCtx))
		return nil
	}))

	// tw_system（BYPASSRLS）未注入 roles → 仍全量可见：SET LOCAL ROLE 切换后
	// current_user = tw_system，BYPASSRLS 按 current_user 生效 ✓。
	require.NoError(t, docDB.withDocumentTx(ctx, clients.ExecIdentity{Role: clients.RoleSystem}, func(txCtx context.Context) error {
		require.Equal(t, 1, count(txCtx), "BYPASSRLS 角色经 SET LOCAL ROLE 切换后必须旁路 policy")
		return nil
	}))
}

// TestExecIdentity_PerRequestTxOverhead 是 A1 遗留原型任务②：每请求一事务
// （BEGIN + 身份注入 + SELECT + COMMIT）与 autocommit 单语句的往返开销对比。
// pgdriver 全程 simple protocol + 客户端插参——两条注入语句合并为单次往返，
// 本测试同时测未合并形态（两次 Exec）作对照，数字进 t.Log 供复审报告引用。
func TestExecIdentity_PerRequestTxOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if testing.Verbose() == false {
		// 数字仅复审报告用；非 verbose 跑法下跳过计时循环，缩短常规 CI。
		t.Skip("skipping timing prototype (run with -v for numbers)")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	docDB := NewPostgresDocumentDB(db, nil).(*postgresDocumentDB)
	id := clients.ExecIdentity{Role: clients.RoleApp, Roles: []string{"any", "user:bob"}}
	const n = 300

	query := `SELECT 1`
	// 预热（建连池）。
	for i := 0; i < 20; i++ {
		_, err := db.ExecContext(ctx, query)
		require.NoError(t, err)
	}

	// (a) autocommit 单语句。
	start := time.Now()
	for i := 0; i < n; i++ {
		_, err := db.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	autocommitPer := time.Since(start) / n

	// (b) 每请求一事务（注入多语句合并 = 1 次往返）。
	start = time.Now()
	for i := 0; i < n; i++ {
		require.NoError(t, docDB.withDocumentTx(ctx, id, func(txCtx context.Context) error {
			_, err := db.Conn(txCtx).ExecContext(txCtx, query)
			return err
		}))
	}
	txMergedPer := time.Since(start) / n

	// (c) 每请求一事务（注入未合并 = 2 次额外往返，对照组）。
	start = time.Now()
	for i := 0; i < n; i++ {
		require.NoError(t, db.RunInTx(ctx, func(txCtx context.Context) error {
			if _, err := db.Conn(txCtx).ExecContext(txCtx, `SET LOCAL ROLE tw_app`); err != nil {
				return err
			}
			if _, err := db.Conn(txCtx).ExecContext(txCtx,
				`SELECT set_config('app.roles', ?, true)`, "any\x1fuser:bob"); err != nil {
				return err
			}
			_, err := db.Conn(txCtx).ExecContext(txCtx, query)
			return err
		}))
	}
	txSplitPer := time.Since(start) / n

	t.Logf("per-request tx overhead prototype (n=%d, loopback): autocommit=%v, tx+merged-inject=%v (+%.1f%%), tx+split-inject=%v (+%.1f%%)",
		n, autocommitPer, txMergedPer, float64(txMergedPer-autocommitPer)/float64(autocommitPer)*100,
		txSplitPer, float64(txSplitPer-autocommitPer)/float64(autocommitPer)*100)
}

// TestExecIdentity_FailClosedRolesGUC 是 fail-closed 语义的行为级锚点：
// app.roles 未注入（空事务身份）时，policy 形谓词（current_setting 对比）不命中。
func TestExecIdentity_FailClosedRolesGUC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	// 空 roles 的 ExecIdentity（模拟任何"身份构造缺角色集"的调用面）。
	var roles any
	require.NoError(t, db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{Role: clients.RoleApp}), func(txCtx context.Context) error {
		return db.Conn(txCtx).QueryRowContext(txCtx, `SELECT current_setting('app.roles', true)`).Scan(&roles)
	}))
	// 空串（注入了空 join）→ string_to_array('' , chr(31)) = '{}' → 无角色可匹配。
	require.Equal(t, "", roles)
	var n int
	require.NoError(t, db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{Role: clients.RoleApp}), func(txCtx context.Context) error {
		return db.Conn(txCtx).QueryRowContext(txCtx,
			`SELECT cardinality(string_to_array(current_setting('app.roles', true), chr(31)))`).Scan(&n)
	}))
	require.Zero(t, n, "空 roles 注入必须解包为零角色（policy 恒 false）")
}

// 编译期锚点（阶段③-b 包 C 更新）：systemExecIdentity 已随 _acl 写点收敛到
// tw_set_document_acl 退役；尾随读回/事件快照的系统身份经 execIdentityFor
// （SystemPrincipal → tw_system）承载，锚点改挂 execIdentityFor。
var _ = execIdentityFor

// 静态检查 execIdentityFor 的主体映射。
func TestExecIdentity_PrincipalMapping(t *testing.T) {
	app := execIdentityFor(databases.Principal{Roles: []string{"user:bob"}})
	require.Equal(t, clients.RoleApp, app.Role)
	require.Equal(t, []string{"user:bob", "any"}, app.Roles)

	sys := execIdentityFor(databases.SystemPrincipal)
	require.Equal(t, clients.RoleSystem, sys.Role)

	admin := execIdentityFor(databases.Principal{PlatformAdmin: true})
	require.Equal(t, clients.RoleSystem, admin.Role, "PlatformAdmin 与 System 同走 BYPASSRLS 旁路")
}
