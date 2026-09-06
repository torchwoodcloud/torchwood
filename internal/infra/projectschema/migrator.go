package projectschema

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/pkg/ident"
)

// SchemaMigrateDurationSeconds 记录最近一次项目 schema 迁移 Apply 的耗时
// （秒；就绪缓存命中直通不经过 applyUpTo，不刷新本指标）。量化预警线三
// 指标之一（转出 POC 门禁 B12，redesign §3.1 缓解 3 / §4.7）：迁移重放耗时
// 是 schema-per-project 规模劣化的直接观测量（每项目 DDL 对象随业务库/集合
// 数线性增长，重放时间随之劣化），超限告警语义 = 触发多集群分片规划评估
// （阈值与来源见 docs/developer/13-operations.md §5.1）。语义为"最近一次
// Apply"（含 advisory 锁等待与迁移事务本体，成功与失败都刷新——失败的重放
// 时长同样是规模信号）；EnsureAll 多路并行时为最后写赢。
var SchemaMigrateDurationSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "torchwood_documentdb_schema_migrate_duration_seconds",
	Help: "Duration of the most recent per-project schema migration Apply (cache-miss path only; " +
		"includes advisory-lock wait and the migration transaction).",
})

func init() {
	prometheus.MustRegister(SchemaMigrateDurationSeconds)
}

//go:embed migrations/*.up.sql
var migrationFS embed.FS

// readySchemas 缓存"该 Database 实例上该 project schema 已达最新迁移版本"。
// 键含 *clients.Database 指针：测试进程内多套 testutil 库共用同一 projectID
// 时不互相污染；生产每进程一个 Database 实例。迁移集是编译期 embed，新增
// 迁移 = 新二进制 = 新进程，缓存无需显式失效。
var readySchemas sync.Map // map[readyKey]struct{}

type readyKey struct {
	db        *clients.Database
	projectID string
}

// Apply 在调用方已打开的 tx（或自行 RunInTx）上执行待应用的项目 DDL。
// SQL 文件替换 {{schema}} 后零查询参数，走 simple protocol 多语句 Exec。
// 中途失败：事务内标记随 ROLLBACK 撤销，随后经独立连接补写 dirty=true
// 持久化（EnsureAll 路径靠它跳过脏项目；CreateProject 路径整体回滚后
// schema 不存在，补写失败属预期，best-effort 忽略）。
//
// 缓存命中（本进程确认过该 schema 已达最新）直接返回，跳过迁移事务与
// advisory 锁——这是数据面 repo 每次 Scoped 调用的热路径。仅在独立事务
// 提交成功后写缓存：外层事务回滚时 schema_migrations 写入一并撤销，
// 缓存不得说谎。
func Apply(ctx context.Context, db *clients.Database, projectID string) error {
	if err := ident.ValidateSchemaResourceID(projectID); err != nil {
		return fmt.Errorf("invalid project id: %w", err)
	}
	if _, ok := readySchemas.Load(readyKey{db: db, projectID: projectID}); ok {
		return nil
	}
	err := applyUpTo(ctx, db, projectID, 0)
	if err == nil && !clients.InTx(ctx) {
		readySchemas.Store(readyKey{db: db, projectID: projectID}, struct{}{})
	}
	return err
}

// Invalidate 清除 project 的就绪缓存。项目删除（DROP SCHEMA）后必须调用，
// 否则同 ID 重建项目时缓存直通会跳过 schema 重建。
func Invalidate(db *clients.Database, projectID string) {
	readySchemas.Delete(readyKey{db: db, projectID: projectID})
}

// ApplyUpTo 应用不超过 maxVersion 的迁移（maxVersion<=0 表示全部）。
// 唯一非零调用方是 testutil：sentinel 系统集合测试面停在 000008——静态表
// 保持 sys_* staging 名，腾出最终名给测试重建的旧文档表（生产全量 Apply，
// 新库 000008+000009 同事务直达最终名）。
func ApplyUpTo(ctx context.Context, db *clients.Database, projectID string, maxVersion int64) error {
	return applyUpTo(ctx, db, projectID, maxVersion)
}

func applyUpTo(ctx context.Context, db *clients.Database, projectID string, maxVersion int64) error {
	if err := ident.ValidateSchemaResourceID(projectID); err != nil {
		return fmt.Errorf("invalid project id: %w", err)
	}
	schema, err := ident.ProjectSchemaName(projectID)
	if err != nil {
		return err
	}
	quoted := quoteIdent(schema)
	var failedVersion int64
	// B12 埋点：计时覆盖 advisory 锁等待 + 迁移事务本体（success 与 failure
	// 都刷新——失败重放的耗时同样是规模信号；缓存命中不进本函数）。
	start := time.Now()
	err = db.RunInTx(ctx, func(txCtx context.Context) error {
		if _, err := db.Conn(txCtx).ExecContext(txCtx,
			`SELECT pg_advisory_xact_lock(hashtext('tw_schema'), hashtext(?))`, projectID); err != nil {
			return fmt.Errorf("project schema lock: %w", err)
		}
		if _, err := db.Conn(txCtx).ExecContext(txCtx,
			fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, quoted)); err != nil {
			return fmt.Errorf("create project schema: %w", err)
		}
		// RBAC schema 级授权（阶段③包 B，public 000004 建角色）：schema 供给
		// 的一部分而非版本化迁移——ApplyUpTo 截断（legacy 状态模拟）也必须授予，
		// 否则 tw_owner 无法在项目数据面建 sentinel 集合表。幂等可重复。
		if _, err := db.Conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
			`GRANT USAGE ON SCHEMA %s TO tw_app, tw_system; GRANT USAGE, CREATE ON SCHEMA %s TO tw_owner`,
			quoted, quoted)); err != nil {
			return fmt.Errorf("grant rbac schema privileges: %w", err)
		}
		if _, err := db.Conn(txCtx).ExecContext(txCtx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.schema_migrations (
    version BIGINT PRIMARY KEY,
    dirty BOOLEAN NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`, quoted)); err != nil {
			return fmt.Errorf("ensure schema_migrations: %w", err)
		}

		var dirty bool
		err := db.Conn(txCtx).QueryRowContext(txCtx,
			fmt.Sprintf(`SELECT COALESCE(bool_or(dirty), false) FROM %s.schema_migrations`, quoted),
		).Scan(&dirty)
		if err != nil {
			return fmt.Errorf("read dirty flag: %w", err)
		}
		if dirty {
			return fmt.Errorf("project schema %s is dirty", schema)
		}

		var applied int64
		if err := db.Conn(txCtx).QueryRowContext(txCtx,
			fmt.Sprintf(`SELECT COALESCE(MAX(version), 0) FROM %s.schema_migrations`, quoted),
		).Scan(&applied); err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}

		files, err := listMigrations()
		if err != nil {
			return err
		}
		for _, f := range files {
			if f.version <= applied {
				continue
			}
			if maxVersion > 0 && f.version > maxVersion {
				continue
			}
			body := strings.ReplaceAll(f.sql, "{{schema}}", quoted)
			if _, err := db.Conn(txCtx).ExecContext(txCtx, body); err != nil {
				failedVersion = f.version
				return fmt.Errorf("apply %s: %w", f.name, err)
			}
			if _, err := db.Conn(txCtx).ExecContext(txCtx,
				fmt.Sprintf(`INSERT INTO %s.schema_migrations (version, dirty) VALUES (%d, false)`, quoted, f.version)); err != nil {
				return fmt.Errorf("record %s: %w", f.name, err)
			}
		}
		return nil
	})
	if err != nil && failedVersion > 0 {
		markDirtyStandalone(ctx, db, quoted, failedVersion)
	}
	SchemaMigrateDurationSeconds.Set(time.Since(start).Seconds())
	return err
}

// markDirtyStandalone 经池连接（不并入 ctx 已有事务）把失败版本标记为
// dirty，使事务 ROLLBACK 后标记仍持久可见。best-effort：写入失败（如
// CreateProject 回滚后 schema 已不存在）时静默，Apply 的原错误仍向上传播。
func markDirtyStandalone(ctx context.Context, db *clients.Database, quotedSchema string, version int64) {
	_, _ = db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s.schema_migrations (version, dirty) VALUES (%d, true)
ON CONFLICT (version) DO UPDATE SET dirty = true`, quotedSchema, version))
}

// EnsureAll 对每个项目 Apply；最多 4 路并行。脏项目记入返回 error。
func EnsureAll(ctx context.Context, db *clients.Database, projectIDs []string) error {
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	errCh := make(chan error, len(projectIDs))
	for _, id := range projectIDs {
		id := id
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := Apply(ctx, db, id); err != nil {
				errCh <- fmt.Errorf("project %s: %w", id, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	var errs []string
	for err := range errCh {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("ensure project schemas: %s", strings.Join(errs, "; "))
	}
	return nil
}

type migrationFile struct {
	name    string
	version int64
	sql     string
}

func listMigrations() ([]migrationFile, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	out := make([]migrationFile, 0, len(names))
	for _, name := range names {
		n := strings.TrimSuffix(name, ".up.sql")
		parts := strings.SplitN(n, "_", 2)
		ver, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration name %s: %w", name, err)
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migrationFile{name: name, version: ver, sql: string(body)})
	}
	return out, nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
