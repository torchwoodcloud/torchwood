package projectschema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	domainprojects "github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

// SchemaManager 实现 projects.SchemaManager 端口：schema 生命周期的
// infra 侧唯一入口（此前 CREATE/DROP SCHEMA 内联在 app/server/projects.go）。
type SchemaManager struct {
	db *clients.Database
	// invalidator 是组合根注入的进程内缓存失效回调（Round4 J5-3）：Invalidate
	// 时级联调用，桥接 documentdb 的 internalIDCache（避免 projectschema ↔
	// documentdb 反向依赖）。可为 nil（单测/未装配场景）。
	invalidator func(projectID string)
}

// NewSchemaManager 构造绑定单个 Database 实例的 schema 生命周期管理器
// （就绪缓存按实例隔离，见 Apply 的缓存键说明）。
func NewSchemaManager(db *clients.Database) *SchemaManager {
	return &SchemaManager{db: db}
}

// SetInvalidator 注册 Invalidate 的级联回调（组合根桥接用，Round4 J5-3）。
func (m *SchemaManager) SetInvalidator(fn func(projectID string)) {
	m.invalidator = fn
}

var _ domainprojects.SchemaManager = (*SchemaManager)(nil)

// Ensure 幂等确保项目 schema 存在且迁移到最新（含就绪缓存直通；ctx 携带
// 事务时并入调用方事务且不写缓存）。
func (m *SchemaManager) Ensure(ctx context.Context, projectID string) error {
	return Apply(ctx, m.db, projectID)
}

// Exists 报告项目数据面 schema 当前是否物理存在（T-R1 护栏）。直查
// pg_namespace 绕过就绪缓存——缓存只记"就绪"，不反映带外 DROP；
// projectID 经 ident 白名单校验，杜绝 schema 名拼接注入。
func (m *SchemaManager) Exists(ctx context.Context, projectID string) (bool, error) {
	schema, err := ident.ProjectSchemaName(projectID)
	if err != nil {
		return false, err
	}
	var one int
	if err := m.db.Conn(ctx).QueryRowContext(ctx,
		`SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = ?`, schema).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("check project schema: %w", err)
	}
	return true, nil
}

// DropCascade 删除项目数据面 schema（CASCADE）。须在调用方事务内执行
// （ctx 携带事务时自动并入），与控制面行删除原子提交。
func (m *SchemaManager) DropCascade(ctx context.Context, projectID string) error {
	schema, err := ident.ProjectSchemaName(projectID)
	if err != nil {
		return err
	}
	if _, err := m.db.Conn(ctx).ExecContext(ctx,
		fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, quoteIdent(schema))); err != nil {
		return fmt.Errorf("drop project schema: %w", err)
	}
	return nil
}

// Invalidate 清除就绪缓存并级联清除 DropCascade 语义下的进程内状态；
// 组合根注入的 invalidator（documentdb internalIDCache，Round4 J5-3）
// 一并触发。
func (m *SchemaManager) Invalidate(projectID string) {
	Invalidate(m.db, projectID)
	if m.invalidator != nil {
		m.invalidator(projectID)
	}
}

// ReconcileOrphanSchemas 对账物理 schema 与 catalog 清单（Round4 J5-5）：
// 列出 pg_namespace 中 tw_ 前缀 schema，与「tw_<p> 本体 + document_databases
// 目录登记的两段式」求差，差异项（孤儿）DROP CASCADE。孤儿只可能来自
// 历史部分失败/带外 DDL；在项目删除事务之外调用，失败仅告警不阻断删除。
//
// 安全边界：仅清理能被精确归属到本项目的 schema——必须形如 tw_<p>_ 且剩余段
// 通过 ident 白名单校验；其余（含其它项目的 tw_<p2>、非系统命名）一律不动，
// 防止前缀碰撞误删在线租户（如删 foo 时不许碰 foobar 的 schema）。
func (m *SchemaManager) ReconcileOrphanSchemas(ctx context.Context, projectID string) (int, error) {
	if err := ident.ValidateSchemaResourceID(projectID); err != nil {
		return 0, fmt.Errorf("reconcile schemas: %w", err)
	}
	projectSchema, err := ident.ProjectSchemaName(projectID)
	if err != nil {
		return 0, err
	}
	twoSegmentPrefix := projectSchema + "_"

	// catalog 清单：本项目已登记的业务库 id。catalog 已全局化（阶段②包 A，
	// public.catalog_databases，000003），项目 schema 是否存在不再影响清单
	// 可读性——直接查全局表。
	expected := map[string]struct{}{projectSchema: {}}
	var scanErr error
	rows, err := m.db.Conn(ctx).QueryContext(ctx,
		`SELECT database_id FROM public.catalog_databases WHERE project_id = ?`, projectID)
	if err != nil {
		return 0, fmt.Errorf("read catalog databases: %w", err)
	}
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var dbID string
			if serr := rows.Scan(&dbID); serr != nil {
				scanErr = fmt.Errorf("scan catalog database id: %w", serr)
				return
			}
			schema, serr := ident.SchemaName(projectID, dbID)
			if serr == nil {
				expected[schema] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			scanErr = fmt.Errorf("iterate catalog databases: %w", err)
		}
	}()
	if scanErr != nil {
		return 0, scanErr
	}

	// 物理清单：全部 tw_ 前缀 namespace。LIKE 中下划线是单字符通配符，转义之。
	rows, err = m.db.Conn(ctx).QueryContext(ctx,
		`SELECT nspname FROM pg_catalog.pg_namespace WHERE nspname LIKE 'tw\_%'`)
	if err != nil {
		return 0, fmt.Errorf("list tw_ namespaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return 0, fmt.Errorf("scan namespace: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate namespaces: %w", err)
	}

	dropped := 0
	for _, name := range names {
		if _, ok := expected[name]; ok {
			continue
		}
		// 归属校验：只处理「本项目的两段式 + 合法 database id」形态。
		if !strings.HasPrefix(name, twoSegmentPrefix) {
			continue
		}
		dbID := strings.TrimPrefix(name, twoSegmentPrefix)
		if ident.ValidateSchemaResourceID(dbID) != nil {
			continue
		}
		if _, err := m.db.Conn(ctx).ExecContext(ctx,
			fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, quoteIdent(name))); err != nil {
			// 单个孤儿 DROP 失败仅告警，继续处理其余项。
			slog.Warn("drop orphan project schema failed",
				"project_id", projectID, "schema", name, "error", err)
			continue
		}
		dropped++
		slog.Warn("dropped orphan project schema",
			"project_id", projectID, "schema", name)
	}
	return dropped, nil
}

// isMissingRelation 已随每项目 catalog 退役删除（阶段②包 A：ReconcileOrphanSchemas
// 改读 public.catalog_databases，表由 public 迁移保证存在，不再需要缺失容错分支）。
