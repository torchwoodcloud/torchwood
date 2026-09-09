// schema-per-project 布局的量化预警线 SLO 指标（转出 POC 门禁 B12，
// docs/developer/15-exit-poc.md；redesign §3.1 缓解 3 / §4.7）：三类规模指标
// 中的两类在进程内采集落指标——
//
//  1. torchwood_documentdb_tables_total{kind}：pg_class × pg_namespace 聚合
//     表计数（kind=catalog=public 控制面+全局 catalog；kind=project_schema=
//     一段式 tw_<project.id> 静态平面；kind=business=两段式 tw_<p>_<db> 业务
//     文档面）。物理 relation 规模是 pg_dump/relcache/autovacuum/迁移重放
//     劣化的共同先行量（§3.1：社区阈值几百 schema 舒适、1–2 千起劣化），
//     超限告警语义 = 触发多集群分片规划评估（§3.1 / §11-G1，见
//     docs/developer/13-operations.md §5.1）。采集入口为启动钩子
//     （cmd/server NewScaleMetricsHook，对齐 A1 CollectionGrantsReconcileHook
//     模式）：启动同步一次 + 进程内小时级周期刷新（调度在组合根，本包只
//     提供幂等采集函数）。
//  2. torchwood_documentdb_pgdump_duration_seconds：全库 pg_dump 计时。
//     进程内只暴露指标骨架（恒 0，直到未来内置调度器存在）——打点契约由
//     外部 cron/运维脚本上报（Prometheus Pushgateway 或 node_exporter 文本
//     文件 collector），见 13-operations.md §5.1 的上报契约与告警规则；
//     ObservePgDumpDuration 供未来内置调度器与本包单测使用。
//
// 第三类（迁移重放耗时 torchwood_documentdb_schema_migrate_duration_seconds）
// 落在 projectschema migrator 的 applyUpTo 埋点——重放执行体在那边。
//
// 分片出口预留（§11-G1）：catalog 行 cluster 内全局，本采集以当前连接库的
// 系统目录为准，分片后每集群进程各自扫各自库，函数无需改动（同
// grants_reconcile.go 的出口论证）。
package documentdb

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

// ScaleTablesTotal 物理表计数（relkind r/p，不含视图/物化视图/索引/TOAST/
// 序列——表是 DDL 与行存规模的主导项；索引膨胀随表走，观察项不混入）。
// 导出（对齐 realtime 指标形态）：组合根闭包与跨包接线测试读取同一实例。
var ScaleTablesTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "torchwood_documentdb_tables_total",
	Help: "Physical table count by plane: kind=catalog (public control plane + global catalog), " +
		"kind=project_schema (tw_<project.id> static plane), kind=business (tw_<p>_<db> document plane). " +
		"Quantified early-warning line for schema-per-project layout (redesign §3.1).",
}, []string{"kind"})

// pgDumpDurationSeconds 全库 pg_dump 计时（指标骨架，进程内恒 0——打点契约
// 在外部 cron/运维脚本，经 Pushgateway 或文本文件 collector 上报，见
// docs/developer/13-operations.md §5.1）。
var pgDumpDurationSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "torchwood_documentdb_pgdump_duration_seconds",
	Help: "Duration of the last full pg_dump. In-process series stays 0 (reporting contract is " +
		"external cron via Pushgateway/textfile collector, see docs/developer/13-operations.md).",
})

func init() {
	prometheus.MustRegister(ScaleTablesTotal, pgDumpDurationSeconds)
}

// ScaleCounts 是一轮表计数采集的快照（供组合根日志行使用）。
type ScaleCounts struct {
	// Catalog 是 public 控制面 + 全局 catalog 的物理表数。
	Catalog int64
	// ProjectSchema 是一段式 tw_<project.id>（静态平面）的物理表数。
	ProjectSchema int64
	// Business 是两段式 tw_<p>_<db>（业务文档面）的物理表数。
	Business int64
}

// scaleCountSQL 单语句聚合三平面计数：一段式/两段式正则与 pkg/ident 的
// projectSchemaNameRe / schemaNameRe 同构且互斥（project.id 不含 "_"），
// 不需要逐 schema 枚举；relkind r/p 只数物理表。
const scaleCountSQL = `SELECT
	count(*) FILTER (WHERE n.nspname = 'public'),
	count(*) FILTER (WHERE n.nspname ~ '^tw_[a-z][a-z0-9]{0,27}$'),
	count(*) FILTER (WHERE n.nspname ~ '^tw_[a-z][a-z0-9]{0,27}_[a-z][a-z0-9]{0,27}$')
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND (n.nspname = 'public' OR n.nspname ~ '^tw_')`

// CollectScaleMetrics 采集当前库的三平面表计数并写入
// torchwood_documentdb_tables_total。幂等只读（pg_class/pg_namespace 系统目录
// 聚合，单语句），可在启动钩子与周期刷新中反复调用；db 为 nil 时直接返回
// 零值（单测/未装配场景，对齐 ReconcileCollectionColumnGrants 的 nil 防御）。
func CollectScaleMetrics(ctx context.Context, db *clients.Database) (ScaleCounts, error) {
	var res ScaleCounts
	if db == nil {
		return res, nil
	}
	row := db.QueryRowContext(ctx, scaleCountSQL)
	if err := row.Scan(&res.Catalog, &res.ProjectSchema, &res.Business); err != nil {
		return res, fmt.Errorf("aggregate pg_class table counts: %w", err)
	}
	ScaleTablesTotal.WithLabelValues("catalog").Set(float64(res.Catalog))
	ScaleTablesTotal.WithLabelValues("project_schema").Set(float64(res.ProjectSchema))
	ScaleTablesTotal.WithLabelValues("business").Set(float64(res.Business))
	return res, nil
}

// ObservePgDumpDuration 上报一次全库 pg_dump 耗时。当前调用方仅测试与未来
// 内置调度器——生产打点契约是外部 cron 经 Pushgateway/文本文件 collector
// 上报（13-operations.md §5.1），应用内序列保持 0 不参与该告警判定。
func ObservePgDumpDuration(d time.Duration) {
	pgDumpDurationSeconds.Set(d.Seconds())
}
