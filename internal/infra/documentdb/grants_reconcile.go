// 存量列授权全量 reconcile 扫描（转出 POC 门禁 A1，docs/developer/15-exit-poc.md）：
// R13a/R16 后业务表列级 GRANT 终态口径以 refreshColumnGrants 为唯一权威
// （SELECT 全列；INSERT 数据列 + 除 _tenant 外系统列含 _acl；UPDATE 排除
// _tenant/_acl；DELETE 表级），但存量表的旧授权形态只在 DDL touch
// （reconcileVersionColumn 汇聚点）时被矫正——无 DDL touch 的表带着旧口径
// 存活。本入口在启动期一次性扫齐：遍历全局 catalog 全部业务集合物理表，
// 逐表执行 refreshColumnGrants 幂等重建，不再依赖 DDL touch 逐表碰运气。
//
// 扫描的是纯授权刷新（refreshColumnGrants，会话 #10 自 ensureCollectionRLS
// 抽出）而非整个 ensureCollectionRLS：A1 的漂移域是列授权；policy/FORCE RLS
// 的幂等重建仍留在 DDL touch 路径，扫描期少一半语句（无 policy DROP/CREATE
// 的 relcache 失效开销），门禁判据的对照物（refreshColumnGrants 幂等重建）
// 与扫描执行体同源。
//
// 多集群/分片出口预留（redesign §3.1 / §11-G1）：catalog 行天然 cluster 内
// 全局，遍历以 catalog 为准即随集群走；分片后每集群进程各自扫各自 catalog，
// 本函数无需改动。
package documentdb

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

// grantsReconcileFailures 计数单表授权刷新失败（对齐孤儿 schema 对账的告警
// 模式：单表失败不中断全量，靠日志 + 指标暴露，而非阻断启动）。
var grantsReconcileFailures = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "torchwood_documentdb_grants_reconcile_failures_total",
	Help: "Collection column-grant reconcile failures during startup scan (per table).",
})

func init() {
	prometheus.MustRegister(grantsReconcileFailures)
}

// GrantsReconcileResult 是一轮全量扫描的汇总计数。
type GrantsReconcileResult struct {
	// Scanned 是 catalog 遍历的业务集合行数（不含 sentinel 系统集合）。
	Scanned int
	// Reconciled 是 refreshColumnGrants 成功的表数。
	Reconciled int
	// Missing 是 catalog 有行但物理表缺失的幽灵表数（跳过——物理漂移归
	// catalog↔pg_catalog 对账，redesign §4.4 / 门禁 B3，不属于列授权域）。
	Missing int
	// Failed 是单表刷新失败数（已记 Error 日志与失败指标，未中断全量）。
	Failed int
}

// ReconcileCollectionColumnGrants 对 catalog 全部业务集合物理表执行列级授权
// 幂等重建（启动钩子入口）。db 为 nil 时直接返回零值（单测/未装配场景，
// 对齐 ProjectSchemaEnsureHook 的 nil 防御）。
//
// 失败语义：单表失败仅日志 + 指标 + 计数，不中断全量、不返回错误（返回错误
// 会经 Lynx OnStart 阻断启动——授权漂移是存量风险而非可用性故障，扫不掉的
// 表由下一次 DDL touch 或下一次启动重试兜底）；仅枚举 catalog 本身失败时
// 返回错误（连清单都读不到，扫了等于没扫，必须让启动期日志可见）。
func ReconcileCollectionColumnGrants(ctx context.Context, db *clients.Database) (GrantsReconcileResult, error) {
	if db == nil {
		return GrantsReconcileResult{}, nil
	}
	return (&postgresDocumentDB{db: db}).reconcileCollectionColumnGrants(ctx)
}

// reconcileCollectionColumnGrants 枚举与逐表刷新。
func (p *postgresDocumentDB) reconcileCollectionColumnGrants(ctx context.Context) (GrantsReconcileResult, error) {
	var res GrantsReconcileResult

	// sentinel（_）排除：项目数据面系统集合是静态平面表级授权（createCollectionTable
	// 的 else 分支，预决策 9），不启 RLS、无列授权口径，不在本扫描域。
	// ORDER BY 全键：每表 REVOKE/GRANT 的 AccessExclusiveLock 获取顺序对全部
	// 进程一致——server 与 worker 同时启动各扫一遍也不会互相死锁（只会逐表
	// 串行等待，后者扫到的已是终态，幂等无害）。
	rows, err := p.conn(ctx).QueryContext(ctx,
		`SELECT project_id, database_id, physical_name FROM public.catalog_collections
		 WHERE database_id <> ? ORDER BY project_id, database_id, physical_name`,
		ident.ProjectDataPlaneID)
	if err != nil {
		return res, fmt.Errorf("enumerate catalog collections: %w", err)
	}
	type entry struct{ projectID, databaseID, physical string }
	var entries []entry
	scanErr := func() error {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.projectID, &e.databaseID, &e.physical); err != nil {
				return fmt.Errorf("scan catalog collection row: %w", err)
			}
			entries = append(entries, e)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate catalog collections: %w", err)
		}
		return nil
	}()
	if scanErr != nil {
		return res, scanErr
	}

	for _, e := range entries {
		res.Scanned++
		schema, err := ident.SchemaName(e.projectID, e.databaseID)
		if err != nil {
			// catalog 行的 id 不满足白名单（理论不可达，写入侧已校验）：
			// 归单表失败，不拼 SQL。
			res.Failed++
			grantsReconcileFailures.Inc()
			slog.Error("reconcile column grants: invalid schema resource id",
				"project_id", e.projectID, "database_id", e.databaseID, "physical", e.physical, "error", err)
			continue
		}
		// 幽灵行跳过：catalog 有行而物理表缺失（部分失败/带外 DROP 残留）。
		// 对缺失表执行 refreshColumnGrants 必然报关系不存在，属预期噪声而非
		// 授权漂移；物理一致性由 catalog↔pg_catalog 对账（redesign §4.4）管。
		cols, err := p.tableColumns(ctx, schema, e.physical)
		if err != nil {
			res.Failed++
			grantsReconcileFailures.Inc()
			slog.Error("reconcile column grants: inspect table columns",
				"schema", schema, "table", e.physical, "error", err)
			continue
		}
		if len(cols) == 0 {
			res.Missing++
			slog.Warn("reconcile column grants: catalog row without physical table (ghost), skipped",
				"schema", schema, "table", e.physical)
			continue
		}
		// 与 DDL touch（reconcileVersionColumn）同特权路径：tw_owner 事务内
		// 执行（000004 后表 owner = tw_owner，DSN authenticator 需 SET LOCAL
		// ROLE 才握有 GRANT/REVOKE 权），顺带把 REVOKE→GRANT 序列包成单事务
		// ——中断只可能是全有或全无，不留半刷新态。
		txErr := p.withOwnerTx(ctx, func(txCtx context.Context) error {
			return p.refreshColumnGrants(txCtx, schema, e.physical)
		})
		if txErr != nil {
			res.Failed++
			grantsReconcileFailures.Inc()
			slog.Error("reconcile column grants: refresh failed (table left as-is, will retry on next DDL touch or startup)",
				"schema", schema, "table", e.physical, "error", txErr)
			continue
		}
		res.Reconciled++
	}
	return res, nil
}
