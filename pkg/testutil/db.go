// Package testutil 提供集成测试公共基建。
//
// # 并行安全契约（转出门禁 A6，`go test ./... -p 4` 稳定性）
//
//   - 隔离库生命周期互斥：SetupTestDB 与迁移循环测试的 CREATE DATABASE、
//     迁移执行（含 000004 的 CREATE ROLE/GRANT membership 等集群目录写）、
//     pg_terminate_backend + DROP DATABASE 段全部持有集群级 advisory lock
//     （testDBLifecycleLockKey）——`-p N` 下每个包是独立进程，进程内互斥锁
//     无效，因此互斥点选在 PG 服务端：跨进程、跨包内 t.Parallel 均互斥。
//     锁只包建库/迁移/删库段，库内用例执行不持锁，并行度不受影响。
//     迁移段入锁的原因：000004 up 的 GRANT membership 与迁移循环 down 的
//     REVOKE 并发更新 pg_auth_members 同一目录行会撞 XX000 tuple concurrently
//     deleted（角色保留方案下 GRANT/REVOKE 仍存在，必须串行化）。
//   - 瞬时故障重试：建库/删库语句对集群瞬时过载（连接打满 too many clients、
//     i/o timeout 等）做指数退避重试（≤3 次）兜底；CREATE DATABASE 在前次
//     响应丢失后重试撞"已存在"（42P04）视为成功。
//   - 角色对象所有权策略：000004 建的 tw_owner/tw_app/tw_system 是集群级角色、
//     被所有并行测试库共享——up 以原子幂等方式创建（duplicate 容错），down 仅
//     清理本库（REASSIGN/DROP OWNED + REVOKE membership）且不 DROP ROLE。
//     因此角色名下对象恒属于某个测试库、随该库 DROP DATABASE 一并消亡，
//     不产生跨库清理义务，迁移循环 down 与并行库无集群对象竞争（详见
//     db/migrations/000004_rbac_rls_roles_sig.down.sql 头注释）。
package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/infra/projectschema"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

var testDBSeq atomic.Uint64

// testDBLifecycleLockKey 是隔离库生命周期互斥的集群级 advisory lock key。
// 任意进程内所有测试库的 CREATE/DROP DATABASE 段经它在 PG 服务端全局串行化，
// 避免 pg_database 行级 AccessExclusiveLock 争用与连接数打满（A6 根因①）。
const testDBLifecycleLockKey = int64(0x7477_6C69_665F_6462) // "twlif_db"

// testDBRetryAttempts 是建库/删库段对集群瞬时过载的最大重试次数（不含初次）。
const testDBRetryAttempts = 5

// retryOnClusterContention 对瞬时过载类错误做指数退避重试
// （250ms/500ms/1s/2s/4s），非瞬时错误立即返回（见包注释并行安全契约）。
func retryOnClusterContention(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = fn()
		if err == nil || attempt >= testDBRetryAttempts || !isTransientClusterError(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(250<<attempt) * time.Millisecond):
		}
	}
}

// newAdminDB 建读超时 10s 的 admin 连接池：admin 侧只跑短语句
// （CREATE/DROP DATABASE、pg_terminate_backend、advisory lock），拥堵时
// 快速失败进入重试而非长时间挂读（长读超时会让一次尝试吞掉整个重试预算）。
// 池不设 MaxOpenConns：持 lifecycle 锁的连接与 fn 内语句（如 cleanup 的
// 删库段）并发取用同一池，上限 1 会自锁。
func newAdminDB(adminDSN string) *bun.DB {
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(adminDSN),
		pgdriver.WithReadTimeout(10*time.Second),
	))
	return bun.NewDB(sqldb, pgdialect.New())
}

// runInDBLifecycleLock 在集群级 advisory lock 下执行 fn（见包注释并行安全契约）。
// 取锁用 pg_try_advisory_lock 轮询而非阻塞版 pg_advisory_lock：阻塞等待在
// pgdriver 里是一次长读，排队超过读超时会断连报 i/o timeout；try 版每次
// 往返毫秒级，等待循环由 ctx 控制总预算。连接建立纳入瞬时错误重试；解锁
// 使用独立短超时 ctx，不受外层 ctx 耗尽影响，保证锁必然释放。
func runInDBLifecycleLock(ctx context.Context, admin *bun.DB, fn func(ctx context.Context) error) error {
	var conn *sql.Conn
	if err := retryOnClusterContention(ctx, func() error {
		if conn != nil {
			_ = conn.Close()
			conn = nil
		}
		c, err := admin.DB.Conn(ctx) // 内嵌 *sql.DB：原生 Conn，bun.Conn 包装不适合跨重试持柄
		if err != nil {
			return err
		}
		conn = c
		return nil
	}); err != nil {
		return fmt.Errorf("acquire admin conn for lifecycle lock: %w", err)
	}
	defer func() { _ = conn.Close() }()
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer unlockCancel()
		_, _ = conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", testDBLifecycleLockKey)
	}()

	// 原生 *sql.Conn 不经 bun 的 `?`→`$n` 重写，pgdriver 只认 `$n` 占位符。
	for {
		var acquired bool
		err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", testDBLifecycleLockKey).Scan(&acquired)
		if err != nil {
			return fmt.Errorf("acquire test db lifecycle lock: %w", err)
		}
		if acquired {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire test db lifecycle lock: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fn(ctx)
}

// isTransientClusterError 判断是否为单 PG 实例被并行测试打满时的瞬时错误
// （连接数上限 / 网络中断）。这类错误退避重试即可恢复，不是测试逻辑失败。
func isTransientClusterError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		switch pgErr.Field('C') {
		case "53300", // too_many_connections
			"57P03",          // cannot_connect_now
			"08000",          // connection_exception
			"08006",          // connection_failure
			"08001", "08004": // sqlclient_unable_to_establish / connection_rejected
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"too many clients",
		"i/o timeout",
		"connection reset",
		"broken pipe",
		"cannot connect now",
		"server closed the connection",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isDuplicateDatabaseError 报告 CREATE DATABASE 是否因库已存在而失败
// （42P04 duplicate_database）：重试窗口内前次执行可能已在服务端生效。
func isDuplicateDatabaseError(err error) bool {
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		return pgErr.Field('C') == "42P04"
	}
	return strings.Contains(strings.ToLower(err.Error()), "already exists")
}

// createTestDatabase 在 lifecycle 锁内建库（含瞬时过载重试与 duplicate 容错）。
func createTestDatabase(ctx context.Context, admin *bun.DB, dbName string) error {
	return retryOnClusterContention(ctx, func() error {
		_, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName)
		if err != nil && isDuplicateDatabaseError(err) {
			return nil
		}
		return err
	})
}

// TestDSN returns the DSN for integration tests, read from the
// TORCHWOOD_TEST_DATABASE_SOURCE environment variable. There is no
// hard-coded fallback: ports are owned by the local compose/.env setup,
// so a missing variable fails fast in SetupTestDB.
func TestDSN() string {
	return os.Getenv("TORCHWOOD_TEST_DATABASE_SOURCE")
}

// AdminDSN returns a DSN to the postgres maintenance database, read from the
// TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE environment variable.
func AdminDSN() string {
	return os.Getenv("TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE")
}

// sharedAdminOnce / sharedAdminDB 是进程级共享的 admin 连接池。每次
// SetupTestDB 的建库/迁移/删库段都需要 admin 连接，进程内复用单一池避免
// 每测试新建池的连接建立洪峰（-p 4 满负载实证 PG 认证层拥堵：SASL
// read timeout）。sql.DB 并发安全，池不 Close（进程退出即回收）。
var (
	sharedAdminOnce sync.Once
	sharedAdminDB   *bun.DB
)

func sharedAdmin() *bun.DB {
	sharedAdminOnce.Do(func() { sharedAdminDB = newAdminDB(AdminDSN()) })
	return sharedAdminDB
}

// SetupTestDB creates a fresh test database, runs migrations, and returns a bun DB client.
func SetupTestDB(t *testing.T) *clients.Database {
	t.Helper()
	adminDSN := AdminDSN()
	baseDSN := TestDSN()
	if adminDSN == "" {
		t.Fatal("TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE is not set (run via `task test`, which loads .env, or export it manually)")
	}
	if baseDSN == "" {
		t.Fatal("TORCHWOOD_TEST_DATABASE_SOURCE is not set (run via `task test`, which loads .env, or export it manually)")
	}
	dbName := uniqueTestDBName()

	testDSN, err := replaceDatabaseName(baseDSN, dbName)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}

	adminDB := sharedAdmin()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(testDSN),
		// 默认写缓冲 4KB 放不下单个 >4KB 的参数（如 outbox 截断测试的
		// 300KiB 文档 JSON）；测试库统一放宽到 2MiB。
		pgdriver.WithBufferSize(2<<20),
	))
	// 单库稳态连接上限：测试内部并发（模拟冲突、并行上传等）最多十余根，
	// 上限防池无界扩张放大集群连接压力；超出仅排队不报错。
	sqldb.SetMaxOpenConns(16)
	db := &clients.Database{DB: bun.NewDB(sqldb, pgdialect.New())}
	t.Cleanup(func() { _ = db.Close() })

	// 建库 + 迁移持集群级 advisory lock（并行安全契约见包注释）：迁移中的
	// 000004 up（CREATE ROLE + GRANT membership）是集群目录写，与其他包并行
	// 迁移以及迁移循环测试 down 的 REVOKE 并发会撞 pg_auth_members 行竞态
	// （XX000 tuple concurrently deleted），故一并串行化；SyncRolesSigKey 只写
	// 本库 tw_secrets，在锁外。
	if err := runInDBLifecycleLock(ctx, adminDB, func(ctx context.Context) error {
		if err := createTestDatabase(ctx, adminDB, dbName); err != nil {
			return err
		}
		return runMigrations(ctx, db.DB)
	}); err != nil {
		t.Fatalf("create test db / run migrations: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cleanupCancel()
		err := runInDBLifecycleLock(cleanupCtx, adminDB, func(ctx context.Context) error {
			return retryOnClusterContention(ctx, func() error {
				return dropTestDatabase(ctx, adminDB, dbName)
			})
		})
		if err != nil {
			t.Errorf("drop test db %s: %v", dbName, err)
		}
	})

	// roles_sig（000004，阶段③-b 包 C）：进程内派生签名密钥并落库 tw_secrets，
	// tw_roles() 验签依赖——漏接则所有 tw_app RLS 查询 fail-closed（测试红）。
	if err := clients.InitRolesSigKey(TestRolesSigMaster); err != nil {
		t.Fatalf("init roles sig key: %v", err)
	}
	if err := clients.SyncRolesSigKey(ctx, db); err != nil {
		t.Fatalf("sync roles sig key: %v", err)
	}
	return db
}

// TestRolesSigMaster 是集成测试的 roles 签名主密钥（与生产同强度口径）。
const TestRolesSigMaster = "integration-test-roles-sig-master-0123456789abcdef"

func uniqueTestDBName() string {
	// Postgres folds unquoted identifiers to lowercase; keep the generated
	// name lowercase so the DSN database name matches the created database.
	// UnixNano 成分：进程被杀（cleanup 未跑）会留下孤儿库，pid 复用 + seq 归零
	// 时旧格式名可能与之相撞，CREATE 的 duplicate 容错会误放行连上旧库——
	// 迁移撞 42P07 relation already exists（2026-09-05 验收实测）；时间成分
	// 使撞名概率归零。
	return strings.ToLower(fmt.Sprintf("%s_%d_%d_%x", testDBPrefix(), os.Getpid(), testDBSeq.Add(1), time.Now().UnixNano()))
}

func testDBPrefix() string {
	dsn := TestDSN()
	u, err := url.Parse(dsn)
	if err != nil || u.Path == "" || u.Path == "/" {
		return "TORCHWOOD_test"
	}
	return strings.TrimPrefix(u.Path, "/")
}

func replaceDatabaseName(dsn, dbName string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

func dropTestDatabase(ctx context.Context, admin *bun.DB, dbName string) error {
	if _, err := admin.ExecContext(ctx, `
		SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity
		WHERE datname = ? AND pid <> pg_backend_pid()
	`, dbName); err != nil {
		return err
	}
	_, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbName)
	return err
}

func runMigrations(ctx context.Context, db *bun.DB) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(root, "db", "migrations", "*.up.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		// #nosec G304 -- 路径来自仓库内 db/migrations 目录的白名单枚举，非外部输入。
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f, err)
		}
		// 单迁移对瞬时过载重试：静态 SQL 多语句经 pgdriver simple protocol
		// 单次往返执行，PG 侧为一个隐式事务——失败全回滚，重放安全。
		if err := retryOnClusterContention(ctx, func() error {
			_, err := db.ExecContext(ctx, string(sqlBytes))
			return err
		}); err != nil {
			return fmt.Errorf("execute migration %s: %w", f, err)
		}
	}
	return nil
}

// CreateTestProjectT 是 CreateTestProject 的 (*testing.T) 变体：失败走
// t.Fatal（带用例位置）而非 panic，新代码一律使用本变体；旧签名保留兼容
// 既有调用。
func CreateTestProjectT(ctx context.Context, t *testing.T, db *clients.Database) (projectID string, internalID int64, cleanup func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("create test project: %v", r)
		}
	}()
	return CreateTestProjectThrough(ctx, db, 0)
}

func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate testutil package")
	}
	return filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
}

// CreateTestProject inserts a test project, applies all project schema migrations, and returns its public id, internal id, and cleanup func.
func CreateTestProject(ctx context.Context, db *clients.Database) (string, int64, func()) {
	return CreateTestProjectThrough(ctx, db, 0)
}

// CreateTestProjectThrough 插入项目并 Apply 不超过 maxVersion 的迁移（maxVersion<=0 表示全部）。
func CreateTestProjectThrough(ctx context.Context, db *clients.Database, maxVersion int64) (string, int64, func()) {
	project := &model.Project{
		ID:        fmt.Sprintf("t%x", time.Now().UnixNano()),
		Name:      fmt.Sprintf("Test Project %d", time.Now().UnixNano()),
		Status:    "active",
		Settings:  map[string]any{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if _, err := db.NewInsert().Model(project).Exec(ctx); err != nil {
		panic(err)
	}
	var internalID int64
	if err := db.NewSelect().Model((*model.Project)(nil)).Column("internal_id").Where("id = ?", project.ID).Scan(ctx, &internalID); err != nil {
		panic(err)
	}
	var err error
	// 项目 schema apply 对集群瞬时过载（i/o timeout 等）退避重试：apply 不在
	// lifecycle 锁内（库内语句），-p 4 满负载下实测偶发单语句 i/o timeout；
	// Apply 按版本表断点续跑（已应用版本跳过），整体重试幂等。
	err = retryOnClusterContention(ctx, func() error {
		if maxVersion > 0 {
			return projectschema.ApplyUpTo(ctx, db, project.ID, maxVersion)
		}
		return projectschema.Apply(ctx, db, project.ID)
	})
	if err != nil {
		panic(err)
	}
	cleanup := func() {
		_, _ = db.NewDelete().Model((*model.Project)(nil)).Where("id = ?", project.ID).Exec(ctx)
	}
	return project.ID, internalID, cleanup
}

// CatalogIdent 返回项目数据面 schema 的 bun.Ident，供测试 ModelTableExpr。
func CatalogIdent(projectID string) bun.Ident {
	s, err := ident.ProjectSchemaName(projectID)
	if err != nil {
		panic(err)
	}
	return bun.Ident(s)
}

// CatalogQuoted 返回 quoteIdent 后的项目 schema 名，供测试拼接 Raw SQL。
func CatalogQuoted(projectID string) string {
	s, err := ident.ProjectSchemaName(projectID)
	if err != nil {
		panic(err)
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// InsertCatalogDatabase 向 public 全局 catalog_databases 插入一行。
func InsertCatalogDatabase(ctx context.Context, db *clients.Database, projectID, id, name string) {
	now := time.Now()
	if _, err := db.NewInsert().Model(&model.DocumentDatabase{
		ProjectID:  projectID,
		DatabaseID: id,
		Name:       name,
		CreatedAt:  now,
		UpdatedAt:  now,
	}).Exec(ctx); err != nil {
		panic(err)
	}
}
