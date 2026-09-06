package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/infra/documentdb"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// TestNonSuperuserAuthenticator_MigrateAndSmoke（转出 POC 门禁 A2，
// docs/developer/15-exit-poc.md）：非 superuser 应用 DSN 的双账号形态在
// 「迁移」与「运行」两态的端到端验证；授权面 SQL 与 13-operations §4.5
// 逐条对应（可复制执行的 runbook 即本测试的引导段）。
//
// 双账号契约：
//   - owner 引导账号（superuser，即 compose/CI 的 POSTGRES_USER 形态）：仅
//     迁移与扩展引导——000005 `CREATE EXTENSION vector`（非 trusted，superuser
//     专属）、000004 `GRANT CREATE ON SCHEMA public TO tw_system` 与
//     `ALTER FUNCTION ... OWNER TO tw_system` 都需要特权身份；roles_sig 密钥
//     落库（B15 部署期 owner 作业，对齐 `torchwood admin sync-roles-sig`）
//     同属此账号；
//   - tw_authenticator（非 superuser、无 BYPASSRLS）：仅 000004 三角色
//     membership + 库级 CONNECT/CREATE + 控制面静态表 DML（边界邻居面），
//     server/worker 运行态 DSN 全部流量走它；对 public.tw_secrets **零权限**
//     （B15 收口：密钥不可读 → GUC 伪造通道封死，迁移 000004）。
//
// 流程：独立临时库上 owner 跑全量迁移 → owner 建 authenticator 并授权 →
// 断言 rolsuper=false / 三角色 membership / SET ROLE 可达 / untrusted 扩展
// 安装被拒 / tw_secrets 零权限 → 以 owner 完成 roles_sig 落库作业 + 建项目
// schema + 建业务库/集合/写读文档冒烟（authenticator 只注入验签）。角色与
// 库名唯一化（集群级对象，避免并行会话竞态）；建库/迁移/删库段按 testutil
// 并行安全契约（A6）持集群级 lifecycle advisory lock（runInDBLifecycleLock）。
func TestNonSuperuserAuthenticator_MigrateAndSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	adminDSN := AdminDSN()
	baseDSN := TestDSN()
	if adminDSN == "" || baseDSN == "" {
		t.Skip("TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE / TORCHWOOD_TEST_DATABASE_SOURCE not set; skipping A2 non-superuser test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	adminDB := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(adminDSN))), pgdialect.New())
	defer func() { _ = adminDB.Close() }()

	// 1) 独立临时库（owner 引导账号建库）+ 全量迁移（双账号契约的「迁移 =
	// owner」落点）：000005 的 vector 扩展、000004 的 GRANT CREATE ON SCHEMA
	// public TO tw_system / ALTER FUNCTION OWNER TO tw_system 均需特权身份；
	// 000004 的 `GRANT ... TO CURRENT_USER` 亦落在 owner 上，authenticator 的
	// membership 由第 2) 步显式授予。段内持集群级 lifecycle lock（A6 并行安全
	// 契约：000004 up 的 membership GRANT 是集群目录写）。
	dbName := uniqueTestDBName()
	roleName := strings.ToLower(fmt.Sprintf("tw_auth_%d_%d", os.Getpid(), testDBSeq.Add(1)))
	rolePass := "tw-authenticator-test-" + roleName

	ownerDSN, err := replaceDatabaseName(baseDSN, dbName)
	require.NoError(t, err)
	ownerDB := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(ownerDSN))), pgdialect.New())
	defer func() { _ = ownerDB.Close() }()

	// 2) owner 引导：创建非 superuser authenticator + 授权面（SQL 与
	// 13-operations §4.5 对应；角色名唯一化仅为测试隔离，生产为固定名）。
	bootstrap := []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION`, roleName, rolePass),
		// 000004 授权面：变色龙三角色 membership（PostgREST authenticator 模式）。
		fmt.Sprintf(`GRANT tw_owner, tw_app, tw_system TO %s`, roleName),
		// 库级权限：CONNECT + CREATE（tw_<project.id> schema 供给——projectschema.Apply
		// 以 base identity CREATE SCHEMA；对齐 000004 授 tw_owner 的 CREATE ON DATABASE）。
		fmt.Sprintf(`GRANT CONNECT, CREATE ON DATABASE %s TO %s`, dbName, roleName),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, roleName),
		// 控制面静态表 DML（边界邻居面，base identity）：public 全表排除 catalog
		// 两表（读写仅经角色可达，与 000004 授权面一致）与 tw_secrets。roleName
		// 为测试生成的安全标识符直接内插；%I 属 SQL format 动词，不经 fmt.Sprintf。
		// tw_secrets 自 B15（迁移 000004）起对运行账号零权限，永久排除。
		`DO $do$ DECLARE t text; BEGIN
			FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'public'
				AND tablename NOT IN ('catalog_databases', 'catalog_collections', 'tw_secrets')
			LOOP
				EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO ` + roleName + `', t);
			END LOOP;
		END $do$`,
		// projectschema 静态迁移的 FK 面（000002/000004/000005/000006/000007
		// 的 `REFERENCES public.projects(id)`）：建外键要求被引用表上的
		// REFERENCES 权限。
		fmt.Sprintf(`GRANT REFERENCES ON public.projects TO %s`, roleName),
	}

	err = runInDBLifecycleLock(ctx, adminDB, func(ctx context.Context) error {
		if err := createTestDatabase(ctx, adminDB, dbName); err != nil {
			return err
		}
		if err := runMigrations(ctx, ownerDB); err != nil {
			return fmt.Errorf("owner 引导账号下全量迁移: %w", err)
		}
		for _, stmt := range bootstrap {
			if _, err := ownerDB.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("bootstrap authenticator %q: %w", stmt, err)
			}
		}
		return nil
	})
	require.NoError(t, err)

	// cleanup：先删库（authenticator 在库内有 schema 对象，DROP ROLE 前必须
	// 消解依赖）再 DROP ROLE（membership 行随 DROP ROLE 自动撤销）。
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cleanupCancel()
		cleanupDB := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(adminDSN))), pgdialect.New())
		defer func() { _ = cleanupDB.Close() }()
		err := runInDBLifecycleLock(cleanupCtx, cleanupDB, func(ctx context.Context) error {
			return retryOnClusterContention(ctx, func() error {
				return dropTestDatabase(ctx, cleanupDB, dbName)
			})
		})
		if err != nil {
			t.Errorf("drop scratch db %s: %v", dbName, err)
		}
		if _, err := cleanupDB.ExecContext(cleanupCtx, "DROP ROLE IF EXISTS "+roleName); err != nil {
			t.Errorf("drop role %s: %v", roleName, err)
		}
	})

	// 3) A2 完成判据核心断言：rolsuper 必须 false，且无任何特权位。
	var rolsuper, rolcreatedb, rolcreaterole, rolbypassrls, rolreplication bool
	require.NoError(t, adminDB.QueryRowContext(ctx,
		`SELECT rolsuper, rolcreatedb, rolcreaterole, rolbypassrls, rolreplication
		 FROM pg_roles WHERE rolname = ?`, roleName,
	).Scan(&rolsuper, &rolcreatedb, &rolcreaterole, &rolbypassrls, &rolreplication))
	require.False(t, rolsuper, "authenticator rolsuper 必须为 false（A2 完成判据）")
	require.False(t, rolcreatedb)
	require.False(t, rolcreaterole)
	require.False(t, rolbypassrls, "authenticator 不得带 BYPASSRLS")
	require.False(t, rolreplication)

	var memberships []string
	rows, err := adminDB.QueryContext(ctx, `
		SELECT r.rolname FROM pg_auth_members m
		JOIN pg_roles r ON r.oid = m.roleid
		JOIN pg_roles a ON a.oid = m.member
		WHERE a.rolname = ? ORDER BY 1`, roleName)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		memberships = append(memberships, name)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []string{"tw_app", "tw_owner", "tw_system"}, memberships,
		"authenticator 应持有 000004 三角色 membership")

	// 4) 以 authenticator 连接：SET ROLE 三角色可达性 + 引导面反例。
	authDSN, err := replaceDatabaseUser(ownerDSN, roleName, rolePass)
	require.NoError(t, err)
	authBase := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(authDSN),
		pgdriver.WithBufferSize(2<<20),
	)), pgdialect.New())
	defer func() { _ = authBase.Close() }()

	// SET ROLE 可达性检查必须走专用单连接：连接池下 SET ROLE 状态会驻留在
	// 池成员上（RESET 落在另一连接），泄漏的 tw_owner 身份会让后续语句踩空
	//——生产运行态同理，身份切换一律事务内 SET LOCAL ROLE（06-databases
	// 不变量 #14），本检查的专用连接用完即关。
	setRoleConn, err := authBase.Conn(ctx)
	require.NoError(t, err)
	for _, role := range []string{"tw_owner", "tw_app", "tw_system"} {
		_, err := setRoleConn.ExecContext(ctx, "SET ROLE "+role)
		require.NoError(t, err, "SET ROLE %s 应可达", role)
		var cur string
		require.NoError(t, setRoleConn.QueryRowContext(ctx, "SELECT current_user").Scan(&cur))
		require.Equal(t, role, cur)
		_, err = setRoleConn.ExecContext(ctx, "RESET ROLE")
		require.NoError(t, err)
	}
	require.NoError(t, setRoleConn.Close())
	// 反例：非 trusted 扩展安装是 superuser 引导面（000005 的 vector 属同类，
	// 但它在临时库已随迁移装好、IF NOT EXISTS 会 no-op 跳过权限检查，故用
	// 同为 untrusted 且未安装的 pg_stat_statements 做确定性别证），必须被拒
	// ——否则迁移与运行账号的权限边界形同虚设。
	_, err = authBase.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	require.Error(t, err, "非 superuser authenticator 不得安装 untrusted 扩展（引导面必须 owner 账号）")

	// 4') B15 完成判据核心断言：authenticator 对 public.tw_secrets 无任何权限
	//（\dp tw_secrets 等价：has_table_privilege 七特权位全 false——该函数含
	// membership 间接授权语义，即 SET ROLE 可达面的全集；与 SECURITY DEFINER
	// owner 链区分：验签函数以自身 owner（迁移执行者）读表，不经运行账号）。
	// 表 owner 另查——owner 对自有表隐式全权，不在此七位内。
	var twSecretsOwner string
	require.NoError(t, ownerDB.QueryRowContext(ctx,
		`SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'public.tw_secrets'::regclass`,
	).Scan(&twSecretsOwner))
	require.NotEqual(t, roleName, twSecretsOwner, "authenticator 不得是 tw_secrets 的 owner")
	for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
		var allowed bool
		require.NoError(t, authBase.QueryRowContext(ctx,
			`SELECT has_table_privilege(?, 'public.tw_secrets', ?)`, roleName, priv).Scan(&allowed),
			"has_table_privilege 查询失败（priv=%s）", priv)
		require.False(t, allowed, "authenticator 对 tw_secrets 不得持有 %s（B15：DSN 泄漏不得读密钥伪造 GUC 提权）", priv)
	}

	// 5) 运行态冒烟（全部流量走 authenticator DSN）：roles_sig 密钥落库改
	// owner 引导身份执行（B15 部署期作业，对齐 `torchwood admin sync-roles-sig`
	// 的内部路径 InitRolesSigKey + SyncRolesSigKey）→ 建项目（控制面 INSERT +
	// 项目 schema CREATE SCHEMA）→ 建业务库/集合（tw_owner DDL）→ 写文档
	//（tw_system）→ RLS 读回（tw_app + sig 验签；authenticator 只注入验签，
	// 不再落库）。
	require.NoError(t, clients.InitRolesSigKey(TestRolesSigMaster))
	ownerClient := &clients.Database{DB: ownerDB}
	require.NoError(t, clients.SyncRolesSigKey(ctx, ownerClient),
		"owner 引导账号应能完成 roles_sig 密钥落库（B15 部署期作业面）")
	authDB := &clients.Database{DB: authBase}

	projectID, _, cleanupProject := CreateTestProjectT(ctx, t, authDB)
	defer cleanupProject()

	docDB := documentdb.NewPostgresDocumentDB(authDB, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "A2 Smoke DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
	}, nil, nil, true))
	created, err := docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
		Data: map[string]any{"title": "hello-a2", "views": 1},
	}, []databases.Permission{
		{Type: "read", Role: "any"},
		{Type: "update", Role: "any"},
		{Type: "delete", Role: "any"},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	got, err := docDB.GetDocument(ctx, projectID, "app", "posts", created.ID, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "hello-a2", got.Data["title"])
}

// replaceDatabaseUser 替换 DSN 中的用户与口令（其余 host/port 参数保持）。
func replaceDatabaseUser(dsn, user, pass string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(user, pass)
	return u.String(), nil
}
