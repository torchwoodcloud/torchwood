package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	"github.com/torchwooddev/torchwood/internal/api"
	apirealtime "github.com/torchwooddev/torchwood/internal/api/realtime"
	"github.com/torchwooddev/torchwood/internal/api/servergrpc"
	"github.com/torchwooddev/torchwood/internal/api/serverhttp"
	"github.com/torchwooddev/torchwood/internal/app"
	appserver "github.com/torchwooddev/torchwood/internal/app/server"
	appstorage "github.com/torchwooddev/torchwood/internal/app/storage"
	"github.com/torchwooddev/torchwood/internal/domain"
	databases "github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	domainstorage "github.com/torchwooddev/torchwood/internal/domain/storage"
	"github.com/torchwooddev/torchwood/internal/infra"
	"github.com/torchwooddev/torchwood/internal/infra/auth"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/infra/documentdb"
	"github.com/torchwooddev/torchwood/internal/infra/health"
	"github.com/torchwooddev/torchwood/internal/infra/projectschema"
	"github.com/torchwooddev/torchwood/internal/pkg/bootkit"
	"github.com/torchwooddev/torchwood/internal/pkg/buildinfo"
	config "github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/runtime"
)

//go:generate wire

var ProviderSet = wire.NewSet(
	boot.New,
	api.ProviderSet,
	app.ProviderSet,
	infra.ProviderSet,
	domain.ProviderSet,
	runtime.ProviderSet,

	// 与 cmd/worker 共享的装配样板收敛在 bootkit（Round4 J4-1）。
	bootkit.NewLogger,
	bootkit.NewComponentBuilders,
	bootkit.NewOnStarts,
	bootkit.NewOnStops,
	NewGrantsReconcileHook,
	NewScaleMetricsHook,
	NewSchemaReconcileHook,
	NewAppConfig,
	NewComponents,
	NewBuildInfo,
	// SchemaManager 桥接 documentdb internalIDCache 失效回调（Round4 J5-3），
	// 取代 infra.ProviderSet 内的裸 projectschema.NewSchemaManager。
	NewSchemaManager,
	wire.Bind(new(projects.SchemaManager), new(*projectschema.SchemaManager)),
	NewProjectsOptions,
	NewStorageOptions,
	NewRealtimeSubscriberService,
	// Realtime 握手校验复用 auth.Validator（api.ProviderSet 与
	// infra.ProviderSet 在此组合，Bind 放组合根）。
	wire.Bind(new(apirealtime.CredentialValidator), new(*auth.Validator)),
	// HTTP / gRPC handler 窄接口绑定（J4-5）：消费端仅依赖最小方法集，
	// 具体类型 *auth.Validator / *health.Checkers 仅在组合根出现。
	wire.Bind(new(serverhttp.AuthValidator), new(*auth.Validator)),
	wire.Bind(new(servergrpc.HealthCheckers), new(*health.Checkers)),
)

func NewAppConfig(app lynx.App) (*config.AppConfig, error) {
	var c config.AppConfig
	if err := config.UnmarshalConfig(app.Config(), &c); err != nil {
		return nil, err
	}
	if err := bootkit.ValidateAppConfig(app.Logger(), &c); err != nil {
		return nil, err
	}
	// R4-J2-4：启用页 token 签名（HMAC，purpose 派生自 jwt.secret）。
	if err := bootkit.InitPageTokenSigning(&c); err != nil {
		return nil, err
	}
	// 阶段③-b 包 C：启用 roles GUC 签名密钥派生（A2，page-token 同模式）；
	// 注入 GUC 用进程内钥。落库由部署期 owner 一次性作业完成（`torchwood
	// admin sync-roles-sig`，门禁 B15——启动钩子已退役，运行 DSN 对
	// tw_secrets 零权限，迁移 000004）。
	if err := bootkit.InitRolesSigSigning(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// NewBuildInfo 返回编译期注入的版本信息（package 级 var，由 ldflags 填充）。
func NewBuildInfo() buildinfo.BuildInfo {
	return buildinfo.BuildInfo{Version: version, Commit: commit, Date: date}
}

// NewComponents 返回服务注册顺序：grpc → gateway → realtime-subscriber
// → metrics。
//
// 关停顺序说明（R09-P2-4，更新至 Lynx v1.3.0）：Lynx v1.3.0 仍经 oklog/run 停止服务——正常关停
// 路径按注册顺序（而非逆序）逐个有界停止，即 grpc → gateway →
// realtime-subscriber → metrics；逆序停止仅用于框架内部 Init/OnStart
// 失败路径的资源清理（stopServices）。依赖方向为 gateway → grpc，理想
// 顺序应先停 gateway 再停 grpc；但关停前已有 30s 排水窗口（readiness
// 摘流 + LB 摘除），且各服务 Stop 均有界，故 grpc 先停仅影响窗口内剩余
// 的少量在途转发请求，可接受。realtime-subscriber 停在 gateway 之后：
// 此时新事件仍会被 worker XADD 进 Stream，重启后由 XGROUP 0-0 + PEL
// 认领续投（at-least-once）。metrics 最后停，Prometheus 采集在关停全程
// 可用。cleanup（DB/Redis 等底层资源）不注册进 Lynx，在 runner.RunE()
// 返回后由 main 统一执行。
func NewComponents(
	grpcServer *lynxgrpc.Server,
	gatewayServer *runtime.GRPCGatewayServer,
	realtimeSubscriber *RealtimeSubscriberService,
	metricsServer *runtime.MetricsServer,
) []lynx.Service {
	return []lynx.Service{
		grpcServer,
		gatewayServer,
		realtimeSubscriber,
		metricsServer,
	}
}

// NewSchemaManager 构造桥接了 documentdb internalIDCache 失效回调的 schema
// 管理器（Round4 J5-3）：项目删除（DROP SCHEMA）后 Invalidate 同时清除
// documentdb 的 internal_id 进程内缓存，否则同 ID 重建项目时旧缓存会以陈旧
// internal_id 打 _tenant 标签，造成静默数据分裂。经结构化接口断言完成桥接，
// 避免 projectschema ↔ documentdb 反向依赖。
func NewSchemaManager(db *clients.Database, docDB databases.DocumentDB) *projectschema.SchemaManager {
	m := projectschema.NewSchemaManager(db)
	if inv, ok := docDB.(documentdb.InternalIDCacheInvalidator); ok {
		m.SetInvalidator(inv.InvalidateInternalIDCache)
	}
	return m
}

// NewGrantsReconcileHook 产出启动期存量列授权全量 reconcile 闭包（门禁 A1，
// docs/developer/15-exit-poc.md）：遍历 catalog 全部业务集合物理表，按
// R13a/R16 终态口径逐表幂等重建列级 GRANT。挂在 server 侧——worker 不碰
// 文档层（import guard TestWorkerDepsGraph 守此边界），故经 NewOnStarts 的
// 可选参数注入而非 bootkit 共享装配。失败语义同 bootkit 既有钩子：单表失败
// 不阻断启动（documentdb 内部日志 + 失败计数指标），OnStart 先于监听。
func NewGrantsReconcileHook(db *clients.Database, logger *slog.Logger) bootkit.GrantsReconcileHook {
	return func(ctx context.Context) error {
		if db == nil {
			return nil
		}
		res, err := documentdb.ReconcileCollectionColumnGrants(ctx, db)
		if err != nil {
			if logger != nil {
				logger.Error("collection column grants reconcile: catalog enumeration failed", "error", err)
			}
			return nil
		}
		if logger != nil {
			logger.Info("collection column grants reconcile done",
				"scanned", res.Scanned, "reconciled", res.Reconciled,
				"missing", res.Missing, "failed", res.Failed)
		}
		return nil
	}
}

// scaleMetricsRefreshInterval 是规模预警线表计数的周期刷新间隔。表计数随
// DDL 缓慢变化，小时级刷新足够追上预警线（>500/>1500，见 13-operations.md
// §5.1）；单次采集是 pg_class × pg_namespace 的单语句聚合，开销可忽略。
const scaleMetricsRefreshInterval = time.Hour

// NewSchemaReconcileHook 产出启动期 schema 漂移对账闭包（门禁 B3，redesign
// §4.4）：遍历全局 catalog 全部业务集合，扫三类漂移（缺列 / INVALID·failed
// 索引含 building 超时残留的中断恢复 / 幽灵表）自动修复 + 告警，默认索引
// 缺失经 CONCURRENTLY 通道补齐。挂在 server 侧——worker 不碰文档层（import
// guard TestWorkerDepsGraph 守此边界），经 NewOnStarts 注入。失败语义同
// NewGrantsReconcileHook：单集合失败不阻断启动（documentdb 内部日志 + 失败
// 计数指标），OnStart 先于监听。
func NewSchemaReconcileHook(db *clients.Database, logger *slog.Logger) bootkit.SchemaReconcileHook {
	return func(ctx context.Context) error {
		if db == nil {
			return nil
		}
		report, err := documentdb.ReconcileSchemaDrift(ctx, db, documentdb.SchemaReconcileOptions{})
		if err != nil {
			if logger != nil {
				logger.Error("schema drift reconcile: catalog enumeration failed", "error", err)
			}
			return nil
		}
		if logger != nil {
			logger.Info("schema drift reconcile done",
				"scanned", report.Scanned, "fixed", report.Fixed,
				"failed", report.Failed, "duration", report.Duration)
		}
		return nil
	}
}

// NewScaleMetricsHook 产出启动期规模预警线表计数采集闭包（门禁 B12，
// docs/developer/15-exit-poc.md；redesign §3.1 缓解 3 / §4.7）：对当前库执行
// pg_class × pg_namespace 聚合，更新 torchwood_documentdb_tables_total{kind}
// 三平面计数。挂在 server 侧——worker 不碰文档层（import guard
// TestWorkerDepsGraph 守此边界），经 NewOnStarts 的 scaleMetrics 参数注入。
// 失败语义同 NewGrantsReconcileHook：采集失败只记日志，不阻断启动（规模
// 观测缺失是可观测性降级而非可用性故障，下次周期刷新重试）。首次同步采集
// 成功后拉起进程内小时级周期刷新 goroutine（进程生命周期，无显式停止——
// 随进程退出回收；每次刷新带独立超时）。
func NewScaleMetricsHook(db *clients.Database, logger *slog.Logger) bootkit.ScaleMetricsHook {
	return func(ctx context.Context) error {
		if db == nil {
			return nil
		}
		res, err := documentdb.CollectScaleMetrics(ctx, db)
		if err != nil {
			if logger != nil {
				logger.Error("documentdb scale metrics collect failed", "error", err)
			}
			return nil
		}
		if logger != nil {
			logger.Info("documentdb scale metrics collected",
				"catalog", res.Catalog, "project_schema", res.ProjectSchema, "business", res.Business)
		}
		go func() {
			ticker := time.NewTicker(scaleMetricsRefreshInterval)
			defer ticker.Stop()
			for range ticker.C {
				bgCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
				if _, err := documentdb.CollectScaleMetrics(bgCtx, db); err != nil && logger != nil {
					logger.Warn("documentdb scale metrics refresh failed", "error", err)
				}
				cancel()
			}
		}()
		return nil
	}
}

// NewProjectsOptions 装配项目用例的生产选项（Round4 J5-2）：注入对象存储
// Purger 与配置，项目删除事务提交后异步清空共享桶 {projectID}/ 前缀。
func NewProjectsOptions(purger domainstorage.Purger, cfg *config.AppConfig) []appserver.ProjectsOption {
	return []appserver.ProjectsOption{appserver.WithObjectPurger(purger, cfg)}
}

// NewStorageOptions 返回生产默认的空选项集（WithClock 等仅供测试注入）。
func NewStorageOptions() []appstorage.StorageOption { return nil }
