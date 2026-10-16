package main

import (
	"errors"
	"log/slog"

	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
	"github.com/torchwoodcloud/torchwood/internal/app/assets"
	appbilling "github.com/torchwoodcloud/torchwood/internal/app/billing"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	"github.com/torchwoodcloud/torchwood/internal/app/leaderboards"
	apppayments "github.com/torchwoodcloud/torchwood/internal/app/payments"
	appstorage "github.com/torchwoodcloud/torchwood/internal/app/storage"
	"github.com/torchwoodcloud/torchwood/internal/app/subscriptions"
	appwebhooks "github.com/torchwoodcloud/torchwood/internal/app/webhooks"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	domainwebhooks "github.com/torchwoodcloud/torchwood/internal/domain/webhooks"
	infrabilling "github.com/torchwoodcloud/torchwood/internal/infra/billing"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	infrapayments "github.com/torchwoodcloud/torchwood/internal/infra/payments"
	infraqueue "github.com/torchwoodcloud/torchwood/internal/infra/queue"
	"github.com/torchwoodcloud/torchwood/internal/infra/realtime"
	infrastorage "github.com/torchwoodcloud/torchwood/internal/infra/storage"
	infrawebhooks "github.com/torchwoodcloud/torchwood/internal/infra/webhooks"
	"github.com/torchwoodcloud/torchwood/internal/pkg/bootkit"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/uow"
	workerpkg "github.com/torchwoodcloud/torchwood/worker"
)

//go:generate wire

// ProviderSet 只装配作业端口与其适配器，避免 app/infra 桶包把
// Account / gRPC / documentdb 拉进进程依赖图。
var ProviderSet = wire.NewSet(
	boot.New,

	appfunctions.NewFunctions,
	// Webhooks 一等公民（v4 §6，D5）：CRUD + 投递引擎聚合（worker 消费
	// DeliveryScheduler 端口 + 事件入口；Bind 与具体类型 provider 同集）。
	appwebhooks.NewService,
	wire.Bind(new(domainwebhooks.DeliveryScheduler), new(*appwebhooks.Service)),
	apppayments.NewPayments,
	assets.NewAssets,
	leaderboards.NewLeaderboards,
	leaderboards.NewAssetsRewardGranter,
	subscriptions.NewSubscriptions,
	subscriptions.NewOrderFulfiller,
	wire.Bind(new(domainpayments.SubscriptionCallbackHandler), new(*subscriptions.Subscriptions)),
	appbilling.NewBilling,
	appstorage.NewStorage,
	NewStorageOptions,

	// 与 cmd/server 共享的装配样板收敛在 bootkit（Round4 J4-1）。
	bootkit.NewLogger,
	bootkit.NewComponentBuilders,
	bootkit.NewPreStarts,
	bootkit.NewDrains,
	bootkit.NewPreStops,
	bootkit.NewPostStops,
	NewGrantsReconcileHook,
	NewScaleMetricsHook,
	NewSchemaReconcileHook,
	newOutboxWorkerService,
	NewAppConfig,
	NewComponents,
	workerpkg.ProviderSet,
	// OrphanChunkCleaner（worker 包导出的清理端口）绑定到 app 层 Storage
	// 实现；Bind 须与具体类型 provider 同集求值，故留在此处。
	wire.Bind(new(workerpkg.OrphanChunkCleaner), new(*appstorage.Storage)),

	clients.NewDataClients,
	clients.NewDatabase,
	clients.NewRedis,
	wire.Bind(new(uow.Runner), new(*clients.Database)),
	wire.Bind(new(uow.Isolator), new(*clients.Database)),
	// P0 执行身份：worker 的 ProcessInvocation 与 server 同步路径共用
	// Redis 执行 token 服务（铸造/主动吊销）。实现在 infra/functions——
	// worker 依赖图禁入 infra/auth（import guard）。
	infrafunctions.NewRedisExecutionTokenService,
	wire.Bind(new(domainfunctions.ExecutionTokenService), new(*infrafunctions.RedisExecutionTokenService)),
	bunrepo.NewProjectRepository,
	bunrepo.NewFunctionRepository,
	// P1 触发器模块：cron 调度循环经 Functions 聚合领取到期触发器。
	bunrepo.NewFunctionTriggerRepository,
	// Webhooks 一等公民（v4 §6，D5）：订阅 + 投递行仓储（迁移 000027）。
	bunrepo.NewWebhookRepository,
	bunrepo.NewWebhookDeliveryRepository,
	bunrepo.NewBucketRepository,
	bunrepo.NewFileRepository,
	bunrepo.NewPaymentOrderRepository,
	bunrepo.NewPaymentCallbackEventRepository,
	bunrepo.NewPaymentFulfillmentRepository,
	bunrepo.NewAssetDefRepository,
	bunrepo.NewAssetHoldingRepository,
	bunrepo.NewAssetLedgerRepository,
	bunrepo.NewLeaderboardBoardRepository,
	bunrepo.NewLeaderboardEntryRepository,
	bunrepo.NewLeaderboardSettlementRepository,
	bunrepo.NewIdempotencyStore,
	wire.Bind(new(databases.IdempotencyStore), new(*bunrepo.IdempotencyStore)),
	bunrepo.NewSubscriptionPlanRepository,
	bunrepo.NewSubscriptionRepository,
	bunrepo.NewProviderIndexRepository,
	bunrepo.NewUsageRepository,
	bunrepo.NewBillingStatementRepository,
	// Analytics worker 面（PR5）：rollup / maintenance 用例 + worker 仓储
	//（四端口同实现：rollup 重算、分区治理、tombstone 清洗、注销钩子写入）。
	appanalytics.NewRollup,
	appanalytics.NewMaintenanceFromConfig,
	bunrepo.NewAnalyticsWorkerRepository,
	wire.Bind(new(domainanalytics.RollupRepository), new(*bunrepo.AnalyticsWorkerRepository)),
	wire.Bind(new(domainanalytics.MaintenanceRepository), new(*bunrepo.AnalyticsWorkerRepository)),
	wire.Bind(new(domainanalytics.TombstoneCleaner), new(*bunrepo.AnalyticsWorkerRepository)),
	wire.Bind(new(domainanalytics.DeletionQueueRepository), new(*bunrepo.AnalyticsWorkerRepository)),
	wire.Bind(new(domainstorage.BucketRepository), new(*bunrepo.BucketRepository)),
	wire.Bind(new(domainstorage.FileRepository), new(*bunrepo.FileRepository)),
	infraevents.ProviderSet,
	// v4 RPC 轴调用出口（Invoker 端口）：worker 的异步调用（队列领取）与
	// server 同步路径同一实现（守卫 client + functions.invoke 配置）。
	infrafunctions.NewInvoker,
	wire.Bind(new(domainfunctions.Invoker), new(*infrafunctions.HTTPInvoker)),
	// P2 客户端调用面：每用户限频端口（worker 装配消费 ClientInvoke 用例
	// 之外的构造依赖；消费路径本身不经 worker）。
	infrafunctions.NewClientQuotaLimiter,
	wire.Bind(new(domainfunctions.ClientQuotaLimiter), new(*infrafunctions.ClientQuotaLimiter)),
	infrapayments.ProviderSet,
	infrabilling.ProviderSet,
	infraqueue.ProviderSet,
	infrastorage.ProviderSet,
	// Webhooks 投递出口（v4 §6，D5）：守卫 client + HTTPNotifier。
	infrawebhooks.ProviderSet,
	realtime.NewStreamTransport,
)

// NewGrantsReconcileHook 返回 nil：列授权全量 reconcile（门禁 A1）是
// documentdb 域职责，仅 server 侧执行——worker 的依赖闭包不得包含
// documentdb（import guard TestWorkerDepsGraph 守此边界），经 NewPreStarts
// 的可选参数注入 nil 即跳过该钩子。
func NewGrantsReconcileHook() bootkit.GrantsReconcileHook { return nil }

// NewScaleMetricsHook 返回 nil：规模预警线表计数采集（门禁 B12）同属
// documentdb 域职责，仅 server 侧执行——边界论证同 NewGrantsReconcileHook。
func NewScaleMetricsHook() bootkit.ScaleMetricsHook { return nil }

// NewSchemaReconcileHook 返回 nil：schema 漂移对账（门禁 B3，缺列/INVALID
// 索引/幽灵表修复）同属 documentdb 域职责，仅 server 侧执行——边界论证同
// NewGrantsReconcileHook。
func NewSchemaReconcileHook() bootkit.SchemaReconcileHook { return nil }

func NewAppConfig(app lynx.App) (*config.AppConfig, error) {
	var c config.AppConfig
	if err := config.UnmarshalConfig(app.Config(), &c); err != nil {
		return nil, err
	}
	// R4-J4-1：与 server 同一安全校验口径（jwt.secret / encryption_key 强度）。
	// worker 虽不签发用户 JWT，但会用主密钥派生页 token 验签密钥并消费
	// server 签发的 page_token，弱主密钥属同一攻击面，故一并 fail-closed
	// （此前仅 server 校验、worker 静默跳过，属装配分叉）。
	if err := bootkit.ValidateAppConfig(app.Logger(), &c); err != nil {
		return nil, err
	}
	if c.GetData().GetDatabase().GetSource() == "" {
		return nil, errors.New("data.database.source must be set (env TORCHWOOD_DATA_DATABASE_SOURCE)")
	}
	// R4-J2-4：与 server 同一主密钥派生页 token 验签密钥（worker 的 outbox
	// dead-letter 列表消费 server 签发的 page_token）。缺主密钥拒绝启动。
	if err := bootkit.InitPageTokenSigning(&c); err != nil {
		return nil, err
	}
	// 阶段③-b 包 C：与 server 同一 roles 签名密钥派生（注入 GUC 用进程内钥；
	// tw_secrets 落库由部署期 owner 作业完成——门禁 B15，启动钩子已退役）。
	if err := bootkit.InitRolesSigSigning(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// newOutboxWorkerService 把 outbox 领取端口包成 lynx actor（壳样板收敛于
// bootkit.RunnerService，与 cmd/server 的 realtime-subscriber 同款）。
func newOutboxWorkerService(w *infraevents.OutboxWorker, logger *slog.Logger) *bootkit.RunnerService {
	return bootkit.NewRunnerService("outbox-worker", w, logger)
}

func NewComponents(worker *workerpkg.Worker, cleaner *workerpkg.ChunkCleaner, trimmer *workerpkg.StreamTrimmer, outbox *bootkit.RunnerService, paymentCloser *workerpkg.PaymentCloser, assetExpirer *workerpkg.AssetExpirer, subscriptionBiller *workerpkg.SubscriptionBiller, usageRollup *workerpkg.UsageRollupWorker, leaderboardsCleaner *workerpkg.LeaderboardsCleaner, leaderboardsSettler *workerpkg.LeaderboardsSettler, analyticsRollup *workerpkg.AnalyticsRollupWorker, analyticsMaintenance *workerpkg.AnalyticsMaintenanceWorker) []lynx.Service {
	return []lynx.Service{worker, cleaner, trimmer, outbox, paymentCloser, assetExpirer, subscriptionBiller, usageRollup, leaderboardsCleaner, leaderboardsSettler, analyticsRollup, analyticsMaintenance}
}

// NewStorageOptions 返回生产默认的空选项集（WithClock 等仅供测试注入）。
func NewStorageOptions() []appstorage.StorageOption { return nil }
