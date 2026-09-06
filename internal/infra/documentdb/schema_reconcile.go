// catalog ↔ pg_catalog 漂移对账与修复（转出 POC 门禁 B3，redesign §4.4 /
// §2-C5）：扫三类漂移——缺列（catalog 有 attr 物理表无列）、INVALID/failed
// 索引（含 building 超时残留的中断恢复）、幽灵表（物理表 catalog 无行）——
// 自动修复 + 告警；`torchwood admin schema repair` CLI 手动触发同逻辑，支持
// dry-run（只报告 diff 不修复）。
//
// 扫描骨架对齐 grants_reconcile.go（A1 门禁）：遍历全局 catalog 全部业务集合
// （sentinel 排除），单表失败不中断全量（日志 + 指标），ORDER BY 全键保证
// 多进程扫描的加锁顺序一致。多集群/分片出口预留同 A1：以 catalog 为准即随
// 集群走。
//
// 与 DDL touch 的分界（B3 包 A 的语义收窄）：CreateIndex 事务 A 纯 catalog
// DML，不再搭车默认索引/RLS/列授权自愈（任何锁型 DDL 都阻塞并发读写）——
// 存量表的默认时间索引/_acl GIN 缺失在本扫描内经 CONCURRENTLY 通道补齐。
package documentdb

import (
	"context"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/pkg/ident"
)

// buildingStaleAfter 是 building 残留的超时阈值（预决策 1：>30min 视为进程
// 崩溃残留）。catalog 行级 updated_at 是唯一的近期性信号（索引条目无独立
// 时间戳）：行新鲜时跳过全部 building 条目（保守方向——活 CIC 不被误清），
// 行超时才重入。
const buildingStaleAfter = 30 * time.Minute

// 漂移条目类别（DriftItem.Kind）。
const (
	DriftMissingColumn       = "missing_column"          // catalog 有 attr、物理表无列
	DriftInvalidIndex        = "invalid_index"           // 物理索引 indisvalid=false
	DriftGhostTable          = "ghost_table"             // 物理表 catalog 无行
	DriftBuildingRebuilt     = "building_stale_rebuilt"  // building 超时残留重建（中断恢复）
	DriftBuildingValid       = "building_valid_backfill" // building 残留但物理索引 valid → 补账 active
	DriftFailedRebuilt       = "failed_index_rebuilt"    // failed 条目重入
	DriftMissingActiveIndex  = "missing_active_index"    // catalog active 但物理索引缺失（带外 DROP）
	DriftMissingDefaultIndex = "missing_default_index"   // 默认时间索引/_acl GIN 缺失（DDL touch 收窄后的自愈通道）
	DriftCatalogRowNoTable   = "catalog_row_no_table"    // catalog 有行物理表缺失（不可自动修复——重建意味着放弃存量数据）
)

// 漂移条目处置（DriftItem.Action）。
const (
	DriftDetected = "detected" // dry-run：仅报告
	DriftFixed    = "fixed"
	DriftFailed   = "failed"
)

// DriftItem 是单条漂移（或其修复结果）。
type DriftItem struct {
	Kind   string `json:"kind"`
	Target string `json:"target"` // schema.table[.column|.index]
	Action string `json:"action"` // detected（dry-run）/ fixed / failed
	Detail string `json:"detail,omitempty"`
}

// SchemaDriftReport 是一轮扫描的汇总。
type SchemaDriftReport struct {
	Scanned  int         `json:"scanned"`
	DryRun   bool        `json:"dry_run"`
	Items    []DriftItem `json:"items"`
	Fixed    int         `json:"fixed"`
	Failed   int         `json:"failed"`
	Duration string      `json:"duration,omitempty"`
}

// SchemaReconcileOptions 是扫描选项。
type SchemaReconcileOptions struct {
	// DryRun=true 只报告 diff 不修复（repair CLI 的 --dry-run 形态）。
	DryRun bool
	// BuildingStaleAfter 覆盖 building 超时阈值（0 = 缺省 30min；测试注入
	// 负值/极小值模拟"立即超时"）。
	BuildingStaleAfter time.Duration
}

// schemaReconcileFailures 计数单表修复失败（对齐孤儿 schema 对账/列授权扫描
// 的告警模式：单表失败不中断全量，靠日志 + 指标暴露）。
var schemaReconcileFailures = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "torchwood_documentdb_schema_reconcile_failures_total",
	Help: "Schema drift reconcile failures during scan (per collection).",
})

func init() {
	prometheus.MustRegister(schemaReconcileFailures)
}

// ReconcileSchemaDrift 对全局 catalog 全部业务集合执行三类漂移扫描与修复
// （启动钩子 / repair CLI 共用入口）。db 为 nil 时返回零值（单测/未装配场景，
// 对齐 ReconcileCollectionColumnGrants）。
//
// 失败语义：单集合失败仅日志 + 指标 + 条目记录，不中断全量、不返回错误
// （启动期返回错误会阻断 Lynx OnStart——漂移是存量风险而非可用性故障，
// 下一次扫描或 repair CLI 重试兜底）；仅枚举 catalog 本身失败时返回错误。
func ReconcileSchemaDrift(ctx context.Context, db *clients.Database, opts SchemaReconcileOptions) (SchemaDriftReport, error) {
	if db == nil {
		return SchemaDriftReport{}, nil
	}
	return (&postgresDocumentDB{db: db}).reconcileSchemaDrift(ctx, opts)
}

type reconcileEntry struct {
	projectID  string
	databaseID string
	schema     string
	physical   string
	collection string
	attrs      []databases.Attribute
	indexes    []databases.Index
	updatedAt  time.Time
}

func (p *postgresDocumentDB) reconcileSchemaDrift(ctx context.Context, opts SchemaReconcileOptions) (SchemaDriftReport, error) {
	start := time.Now()
	stale := opts.BuildingStaleAfter
	if stale == 0 {
		stale = buildingStaleAfter
	}
	rep := SchemaDriftReport{DryRun: opts.DryRun}
	defer func() { rep.Duration = time.Since(start).Round(time.Millisecond).String() }()

	// 遍历骨架对齐 grants_reconcile（ORDER BY 全键：多进程扫描加锁顺序一致）。
	rows, err := p.conn(ctx).QueryContext(ctx,
		`SELECT project_id, database_id, physical_name, collection_id, attrs, indexes, updated_at
		 FROM public.catalog_collections
		 WHERE database_id <> ? ORDER BY project_id, database_id, physical_name`,
		ident.ProjectDataPlaneID)
	if err != nil {
		return rep, fmt.Errorf("enumerate catalog collections: %w", err)
	}
	var entries []reconcileEntry
	scanErr := func() error {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var e reconcileEntry
			var attrsRaw, idxRaw string
			if err := rows.Scan(&e.projectID, &e.databaseID, &e.physical, &e.collection, &attrsRaw, &idxRaw, &e.updatedAt); err != nil {
				return fmt.Errorf("scan catalog collection row: %w", err)
			}
			e.attrs, err = decodeAttributes(attrsRaw)
			if err != nil {
				return fmt.Errorf("decode attrs for %s.%s: %w", e.projectID, e.collection, err)
			}
			e.indexes, err = decodeIndexes(idxRaw)
			if err != nil {
				return fmt.Errorf("decode indexes for %s.%s: %w", e.projectID, e.collection, err)
			}
			schema, serr := ident.SchemaName(e.projectID, e.databaseID)
			if serr != nil {
				// catalog 行 id 不满足白名单（写入侧已校验，理论不可达）：
				// 单行失败不拼 SQL。
				rep.record(DriftItem{Kind: DriftCatalogRowNoTable, Target: e.projectID + "/" + e.collection,
					Action: DriftFailed, Detail: "invalid schema resource id: " + serr.Error()})
				schemaReconcileFailures.Inc()
				continue
			}
			e.schema = schema
			entries = append(entries, e)
		}
		return rows.Err()
	}()
	if scanErr != nil {
		return rep, scanErr
	}

	// catalogBySchema 供幽灵表检测（schema → catalog 物理名集合）。
	catalogBySchema := map[string]map[string]bool{}
	for _, e := range entries {
		if catalogBySchema[e.schema] == nil {
			catalogBySchema[e.schema] = map[string]bool{}
		}
		catalogBySchema[e.schema][e.physical] = true
	}

	for _, e := range entries {
		rep.Scanned++
		tbl := e.schema + "." + e.physical
		cols, err := p.tableColumns(ctx, e.schema, e.physical)
		if err != nil {
			rep.record(e.fail(DriftCatalogRowNoTable, tbl, "inspect table: "+err.Error()))
			continue
		}
		if len(cols) == 0 {
			// catalog 有行物理表缺失：自动"修复"= 按契约重建空表——意味着
			// 接受存量数据丢失，不可默认执行；报告告警交运维决策。
			rep.record(e.warn(DriftCatalogRowNoTable, tbl, "catalog row without physical table; manual recreation required"))
			continue
		}
		p.reconcileMissingColumns(ctx, e, cols, &rep)
		p.reconcileCollectionIndexes(ctx, e, stale, &rep)
	}

	// 幽灵表：业务 schema（只放用户集合物理表）中 catalog 无行的表。
	for schema, names := range catalogBySchema {
		tables, err := p.schemaTables(ctx, schema)
		if err != nil {
			schemaReconcileFailures.Inc()
			slog.Error("schema reconcile: list schema tables", "schema", schema, "error", err)
			continue
		}
		for _, t := range tables {
			if names[t] {
				continue
			}
			target := schema + "." + t
			if opts.DryRun {
				rep.record(DriftItem{Kind: DriftGhostTable, Target: target, Action: DriftDetected,
					Detail: "physical table without catalog row"})
				continue
			}
			if err := p.dropGhostTable(ctx, schema, t); err != nil {
				schemaReconcileFailures.Inc()
				rep.record(DriftItem{Kind: DriftGhostTable, Target: target, Action: DriftFailed, Detail: err.Error()})
				slog.Error("schema reconcile: drop ghost table", "target", target, "error", err)
				continue
			}
			rep.record(DriftItem{Kind: DriftGhostTable, Target: target, Action: DriftFixed, Detail: "dropped"})
			slog.Warn("schema reconcile: ghost table dropped (physical table without catalog row)",
				"target", target)
		}
	}
	return rep, nil
}

// reconcileMissingColumns 修复缺列漂移（class 1）：catalog 有 attr（active）、
// 物理表无列 → ADD COLUMN IF NOT EXISTS + refreshColumnGrants（同一 tw_owner
// 事务）。required 且无 default 且表非空时不可自动回填（NOT NULL 列加在存量
// 行上必然失败）——记 failed 交运维。
func (p *postgresDocumentDB) reconcileMissingColumns(ctx context.Context, e reconcileEntry, cols []string, rep *SchemaDriftReport) {
	for _, attr := range e.attrs {
		if !attrActive(attr) {
			continue // deprecated/retired 的物理形态由 B4 生命周期管理
		}
		if slices.Contains(cols, attr.Key) {
			continue
		}
		target := e.schema + "." + e.physical + "." + attr.Key
		colSQL, err := attributeColumnSQL(attr)
		if err != nil {
			rep.record(e.fail(DriftMissingColumn, target, "build column ddl: "+err.Error()))
			schemaReconcileFailures.Inc()
			continue
		}
		if attr.Required && attr.Default == nil {
			empty, err := p.tableEmpty(ctx, e.schema, e.physical)
			if err != nil {
				rep.record(e.fail(DriftMissingColumn, target, "check empty: "+err.Error()))
				schemaReconcileFailures.Inc()
				continue
			}
			if !empty {
				rep.record(e.fail(DriftMissingColumn, target,
					"required column without default cannot be backfilled onto existing rows; add a default or empty the table"))
				schemaReconcileFailures.Inc()
				continue
			}
		}
		if rep.DryRun {
			rep.record(DriftItem{Kind: DriftMissingColumn, Target: target, Action: DriftDetected,
				Detail: "catalog attr without physical column"})
			continue
		}
		txErr := p.withOwnerTx(ctx, func(txCtx context.Context) error {
			if _, err := p.conn(txCtx).ExecContext(txCtx,
				fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s`, tableName(e.schema, e.physical), colSQL),
			); err != nil {
				return err
			}
			// 补列后重刷列级 GRANT（tw_app 写入面，对齐 CreateAttribute 语义）。
			return p.refreshColumnGrants(txCtx, e.schema, e.physical)
		})
		if txErr != nil {
			schemaReconcileFailures.Inc()
			rep.record(e.fail(DriftMissingColumn, target, txErr.Error()))
			slog.Error("schema reconcile: add missing column", "target", target, "error", txErr)
			continue
		}
		rep.record(DriftItem{Kind: DriftMissingColumn, Target: target, Action: DriftFixed, Detail: "column added"})
		slog.Warn("schema reconcile: missing column restored", "target", target)
	}
}

// reconcileCollectionIndexes 修复索引漂移（class 2）+ 默认索引自愈：
//   - building 超时残留（中断恢复，判据"building 残留可重入"）：物理 valid →
//     补账 active；INVALID/缺失 → DROP 残留后 CIC 重入；
//   - failed 条目：DROP 残留后 CIC 重入（重入再失败落回 failed，单轮单次）；
//   - active 条目物理缺失（带外 DROP）/INVALID → CIC 重建；
//   - catalog 无主的 INVALID 索引 → DROP（垃圾清理）；
//   - 默认时间索引/_acl GIN 缺失 → CIC 补齐（DDL touch 收窄后的自愈通道）。
//
// catalog 状态回写经 indexSetStatus（CAS）；dry-run 全部只报告。
func (p *postgresDocumentDB) reconcileCollectionIndexes(ctx context.Context, e reconcileEntry, stale time.Duration, rep *SchemaDriftReport) {
	indexState, err := p.tableIndexStates(ctx, e.schema, e.physical)
	if err != nil {
		rep.record(e.fail(DriftInvalidIndex, e.schema+"."+e.physical, "inspect indexes: "+err.Error()))
		schemaReconcileFailures.Inc()
		return
	}
	buildingStale := time.Since(e.updatedAt) > stale
	attempt := func(kind, indexID, detail string) {
		target := e.schema + "." + e.physical + "." + indexID
		if rep.DryRun {
			rep.record(DriftItem{Kind: kind, Target: target, Action: DriftDetected, Detail: detail})
			return
		}
		idx, ok := e.indexByID(indexID)
		if !ok {
			return
		}
		// 无主 INVALID 残留先清（CIC 的 IF NOT EXISTS 会命中残留名直接跳过）。
		if err := p.dropIndexResidue(ctx, e.schema, e.physical, indexID); err != nil {
			schemaReconcileFailures.Inc()
			rep.record(e.fail(kind, target, "drop residue: "+err.Error()))
			return
		}
		attrs, err := p.collectionAttrsForIndex(ctx, e.projectID, e.databaseID, e.collection)
		if err != nil {
			schemaReconcileFailures.Inc()
			rep.record(e.fail(kind, target, "load attrs: "+err.Error()))
			return
		}
		if err := p.createIndexConcurrently(ctx, e.schema, e.physical, idx, attrs); err != nil {
			// 重入失败：清残留 + 落回 failed（可再次重入）。
			_ = p.dropIndexResidue(ctx, e.schema, e.physical, indexID)
			_, _ = p.indexSetStatus(ctx, e.projectID, e.databaseID, e.collection, indexID, databases.IndexStatusFailed)
			schemaReconcileFailures.Inc()
			rep.record(e.fail(kind, target, err.Error()))
			slog.Error("schema reconcile: index rebuild failed", "target", target, "error", err)
			return
		}
		if _, err := p.indexSetStatus(ctx, e.projectID, e.databaseID, e.collection, indexID, databases.IndexStatusActive); err != nil {
			schemaReconcileFailures.Inc()
			rep.record(e.fail(kind, target, "mark active: "+err.Error()))
			return
		}
		rep.record(DriftItem{Kind: kind, Target: target, Action: DriftFixed, Detail: detail})
		slog.Warn("schema reconcile: index repaired", "target", target, "kind", kind)
	}

	for _, idx := range e.indexes {
		state, exists := indexState[physicalIndexName(e.physical, idx.ID)]
		switch idx.StatusOrDefault() {
		case databases.IndexStatusBuilding:
			if !buildingStale {
				continue // 活 CIC：不动（保守方向）
			}
			switch {
			case exists && state.valid:
				// 崩溃在事务 B 前：物理索引有效，补账 active。
				p.backfillActive(ctx, e, idx.ID, rep)
			case !exists:
				attempt(DriftBuildingRebuilt, idx.ID, "stale building with missing physical index; rebuilt")
			default:
				attempt(DriftBuildingRebuilt, idx.ID, "stale building with INVALID physical index; rebuilt")
			}
		case databases.IndexStatusFailed:
			attempt(DriftFailedRebuilt, idx.ID, "failed entry re-entered")
		default: // active
			switch {
			case !exists:
				attempt(DriftMissingActiveIndex, idx.ID, "active entry with missing physical index; rebuilt")
			case !state.valid:
				attempt(DriftInvalidIndex, idx.ID, "active entry with INVALID physical index; rebuilt")
			}
		}
	}

	// 无主 INVALID 索引（垃圾清理）：不匹配任何 catalog 条目。
	catalogNames := map[string]bool{}
	for _, idx := range e.indexes {
		catalogNames[physicalIndexName(e.physical, idx.ID)] = true
	}
	catalogNames[physicalIndexName(e.physical, "tenant_created")] = true
	catalogNames[physicalIndexName(e.physical, "acl")] = true
	for name, st := range indexState {
		if st.valid || catalogNames[name] {
			continue
		}
		target := e.schema + "." + e.physical + "." + name
		if rep.DryRun {
			rep.record(DriftItem{Kind: DriftInvalidIndex, Target: target, Action: DriftDetected,
				Detail: "orphaned INVALID index"})
			continue
		}
		if _, err := p.execOwnerStatement(ctx, dropIndexStatement(e.schema, name)); err != nil {
			schemaReconcileFailures.Inc()
			rep.record(e.fail(DriftInvalidIndex, target, "drop orphan: "+err.Error()))
			continue
		}
		rep.record(DriftItem{Kind: DriftInvalidIndex, Target: target, Action: DriftFixed, Detail: "orphaned INVALID index dropped"})
		slog.Warn("schema reconcile: orphaned INVALID index dropped", "target", target)
	}

	// 默认索引自愈（缺失才建，走 CIC）。
	p.ensureDefaultIndexReconciled(ctx, e, indexState, "tenant_created",
		fmt.Sprintf(`CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (_tenant, _created_at, _id)`,
			quoteIdent(physicalIndexName(e.physical, "tenant_created")), tableName(e.schema, e.physical)), rep)
	p.ensureDefaultIndexReconciled(ctx, e, indexState, "acl",
		fmt.Sprintf(`CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s USING gin (_acl)`,
			quoteIdent(physicalIndexName(e.physical, "acl")), tableName(e.schema, e.physical)), rep)
}

// backfillActive 把 building 残留但物理索引 valid 的条目补账为 active
// （崩溃发生在 CIC 完成之后、事务 B 之前）。
func (p *postgresDocumentDB) backfillActive(ctx context.Context, e reconcileEntry, indexID string, rep *SchemaDriftReport) {
	target := e.schema + "." + e.physical + "." + indexID
	if rep.DryRun {
		rep.record(DriftItem{Kind: DriftBuildingValid, Target: target, Action: DriftDetected,
			Detail: "stale building with valid physical index"})
		return
	}
	found, err := p.indexSetStatus(ctx, e.projectID, e.databaseID, e.collection, indexID, databases.IndexStatusActive)
	if err != nil || !found {
		schemaReconcileFailures.Inc()
		detail := "mark active failed"
		if err != nil {
			detail = err.Error()
		} else {
			detail = "entry vanished (concurrent delete)"
		}
		rep.record(e.fail(DriftBuildingValid, target, detail))
		return
	}
	rep.record(DriftItem{Kind: DriftBuildingValid, Target: target, Action: DriftFixed,
		Detail: "valid physical index promoted to active"})
	slog.Warn("schema reconcile: stale building entry promoted to active (valid index)", "target", target)
}

// ensureDefaultIndexReconciled 幂等补齐默认索引（存在且 valid 即跳过；缺失或
// INVALID 走 CIC——事务内 CREATE INDEX 会取 SHARE 锁阻塞并发读写，不能在
// 这里用）。states 是调用方已取的表索引状态快照（避免重复 pg_index 查询）。
func (p *postgresDocumentDB) ensureDefaultIndexReconciled(ctx context.Context, e reconcileEntry, states map[string]indexState, suffix, cicSQL string, rep *SchemaDriftReport) {
	name := physicalIndexName(e.physical, suffix)
	if st, ok := states[name]; ok && st.valid {
		return
	}
	target := e.schema + "." + e.physical + "." + name
	if rep.DryRun {
		rep.record(DriftItem{Kind: DriftMissingDefaultIndex, Target: target, Action: DriftDetected,
			Detail: "default index missing or invalid"})
		return
	}
	// 残留 INVALID 先清（CONCURRENTLY 的 IF NOT EXISTS 会命中残留名跳过）。
	if _, err := p.execOwnerStatement(ctx, dropIndexStatement(e.schema, name)); err != nil {
		schemaReconcileFailures.Inc()
		rep.record(e.fail(DriftMissingDefaultIndex, target, "drop residue: "+err.Error()))
		return
	}
	if err := p.execConcurrentDDL(ctx, cicSQL); err != nil {
		schemaReconcileFailures.Inc()
		rep.record(e.fail(DriftMissingDefaultIndex, target, err.Error()))
		slog.Error("schema reconcile: default index build failed", "target", target, "error", err)
		return
	}
	rep.record(DriftItem{Kind: DriftMissingDefaultIndex, Target: target, Action: DriftFixed, Detail: "default index built concurrently"})
	slog.Warn("schema reconcile: default index restored", "target", target)
}

// ---------------------------------------------------------------------------
// 扫描原语
// ---------------------------------------------------------------------------

// indexState 是物理索引的 pg_index 摘要。
type indexState struct {
	valid bool
}

// tableIndexStates 返回表的全部索引（名 → 状态）。
func (p *postgresDocumentDB) tableIndexStates(ctx context.Context, schema, physical string) (map[string]indexState, error) {
	rows, err := p.conn(ctx).QueryContext(ctx, `
		SELECT ci.relname, i.indisvalid
		FROM pg_index i
		JOIN pg_class ci ON ci.oid = i.indexrelid
		JOIN pg_class ct ON ct.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = ct.relnamespace
		WHERE n.nspname = ? AND ct.relname = ?`, schema, physical)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]indexState{}
	for rows.Next() {
		var name string
		var st indexState
		if err := rows.Scan(&name, &st.valid); err != nil {
			return nil, err
		}
		out[name] = st
	}
	return out, rows.Err()
}

// schemaTables 列出 schema 的全部表名（幽灵表检测）。
func (p *postgresDocumentDB) schemaTables(ctx context.Context, schema string) ([]string, error) {
	rows, err := p.conn(ctx).QueryContext(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = ? ORDER BY tablename`, schema)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// tableEmpty 报告表是否无行（required 缺列可否安全回填的判定）。
func (p *postgresDocumentDB) tableEmpty(ctx context.Context, schema, physical string) (bool, error) {
	var empty bool
	err := p.conn(ctx).QueryRowContext(ctx,
		fmt.Sprintf(`SELECT NOT EXISTS (SELECT 1 FROM %s LIMIT 1)`, tableName(schema, physical))).Scan(&empty)
	return empty, err
}

// dropIndexResidue 清理物理索引残留（幂等；独立短事务 tw_owner）。
func (p *postgresDocumentDB) dropIndexResidue(ctx context.Context, schema, physical, indexID string) error {
	_, err := p.execOwnerStatement(ctx, dropIndexStatement(schema, physicalIndexName(physical, indexID)))
	return err
}

// dropGhostTable 删除幽灵表（业务 schema 内 catalog 无行的物理表）。
func (p *postgresDocumentDB) dropGhostTable(ctx context.Context, schema, physical string) error {
	_, err := p.execOwnerStatement(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, tableName(schema, physical)))
	return err
}

// execConcurrentDDL 在事务外独立连接上以 tw_owner 会话身份执行单条 DDL
// （CONCURRENTLY 语句专用；复用 createIndexConcurrently 的连接纪律——用毕
// RESET ROLE，失败剔除连接）。
func (p *postgresDocumentDB) execConcurrentDDL(ctx context.Context, stmt string) error {
	sqlDB := p.db.DB.DB
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	resetDone := false
	defer func() {
		if resetDone {
			_ = conn.Close()
			return
		}
		// RESET 失败/未执行：连接带着 tw_owner 会话角色，强制出池销毁。
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`SET ROLE %s`, clients.RoleOwner)); err != nil {
		return err
	}
	defer func() {
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), `RESET ROLE`); err == nil {
			resetDone = true
		}
	}()
	_, err = conn.ExecContext(ctx, stmt)
	return err
}

// indexByID 取条目定义副本。
func (e reconcileEntry) indexByID(id string) (databases.Index, bool) {
	for _, i := range e.indexes {
		if i.ID == id {
			return i, true
		}
	}
	return databases.Index{}, false
}

// attrActive 报告属性处于 active（或缺省=active）生命周期状态——缺列修复
// 只服务 active 属性；deprecated/retired 的物理形态由 B4 生命周期管理。
func attrActive(a databases.Attribute) bool {
	return a.StatusOrDefault() == databases.AttrStatusActive
}

// record 收录条目并维护计数。
func (r *SchemaDriftReport) record(item DriftItem) {
	switch item.Action {
	case DriftFixed:
		r.Fixed++
	case DriftFailed:
		r.Failed++
	}
	r.Items = append(r.Items, item)
}

// fail 构造失败条目（kind/target 上下文复用）。
func (e reconcileEntry) fail(kind, target, detail string) DriftItem {
	return DriftItem{Kind: kind, Target: target, Action: DriftFailed, Detail: detail}
}

// warn 构造告警条目（报告但不计入 fixed/failed——运维决策项）。
func (e reconcileEntry) warn(kind, target, detail string) DriftItem {
	return DriftItem{Kind: kind, Target: target, Action: DriftDetected, Detail: detail}
}
