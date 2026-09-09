package main

import (
	"errors"

	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/torchwooddev/torchwood/internal/app/assets"
	appbilling "github.com/torchwooddev/torchwood/internal/app/billing"
	appfunctions "github.com/torchwooddev/torchwood/internal/app/functions"
	apppayments "github.com/torchwooddev/torchwood/internal/app/payments"
	appstorage "github.com/torchwooddev/torchwood/internal/app/storage"
	"github.com/torchwooddev/torchwood/internal/app/subscriptions"
	"github.com/torchwooddev/torchwood/internal/bootkit"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	domainpayments "github.com/torchwooddev/torchwood/internal/domain/payments"
	domainstorage "github.com/torchwooddev/torchwood/internal/domain/storage"
	infrabilling "github.com/torchwooddev/torchwood/internal/infra/billing"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	infraevents "github.com/torchwooddev/torchwood/internal/infra/events"
	infrafunctions "github.com/torchwooddev/torchwood/internal/infra/functions"
	infrapayments "github.com/torchwooddev/torchwood/internal/infra/payments"
	infraqueue "github.com/torchwooddev/torchwood/internal/infra/queue"
	"github.com/torchwooddev/torchwood/internal/infra/realtime"
	infrastorage "github.com/torchwooddev/torchwood/internal/infra/storage"
	config "github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/pkg/uow"
)

//go:generate wire

// ProviderSet 只装配作业端口与其适配器，避免 app/infra 桶包把
// Account / gRPC / documentdb 拉进进程依赖图。
var ProviderSet = wire.NewSet(
	boot.New,

	appfunctions.ProvideSemaphores,
	appfunctions.NewFunctionsWithUsage,
	apppayments.NewPayments,
	assets.NewAssets,
	subscriptions.NewSubscriptions,
	subscriptions.NewOrderFulfiller,
	wire.Bind(new(domainpayments.SubscriptionCallbackHandler), new(*subscriptions.Subscriptions)),
	appbilling.NewBilling,
	appstorage.NewStorage,
	NewStorageOptions,

	// 与 cmd/server 共享的装配样板收敛在 bootkit（Round4 J4-1）。
	bootkit.NewLogger,
	bootkit.NewComponentBuilders,
	bootkit.NewOnStarts,
	bootkit.NewOnStops,
	NewGrantsReconcileHook,
	NewScaleMetricsHook,
	NewSchemaReconcileHook,
	NewAppConfig,
	NewComponents,
	NewWorker,
	NewChunkCleaner,
	NewStreamTrimmer,
	NewOutboxWorkerService,
	NewPaymentCloser,
	NewAssetExpirer,
	NewSubscriptionBiller,
	NewUsageRollupWorker,
	wire.Bind(new(chunkCleaner), new(*appstorage.Storage)),

	clients.NewDataClients,
	clients.NewDatabase,
	clients.NewRedis,
	wire.Bind(new(uow.Runner), new(*clients.Database)),
	wire.Bind(new(uow.Isolator), new(*clients.Database)),
	// P0 执行身份：worker 的 ProcessExecution 与 server 同步路径共用
	// Redis 执行 token 服务（铸造/主动吊销）。实现在 infra/functions——
	// worker 依赖图禁入 infra/auth（import guard）。
	infrafunctions.NewRedisExecutionTokenService,
	wire.Bind(new(domainfunctions.ExecutionTokenService), new(*infrafunctions.RedisExecutionTokenService)),
	bunrepo.NewProjectRepository,
	bunrepo.NewFunctionRepository,
	// P1 触发器模块：cron 调度循环经 Functions 聚合领取到期触发器。
	bunrepo.NewFunctionTriggerRepository,
	bunrepo.NewBucketRepository,
	bunrepo.NewFileRepository,
	bunrepo.NewPaymentOrderRepository,
	bunrepo.NewPaymentCallbackEventRepository,
	bunrepo.NewPaymentFulfillmentRepository,
	bunrepo.NewAssetDefRepository,
	bunrepo.NewAssetHoldingRepository,
	bunrepo.NewAssetLedgerRepository,
	bunrepo.NewSubscriptionPlanRepository,
	bunrepo.NewSubscriptionRepository,
	bunrepo.NewProviderIndexRepository,
	bunrepo.NewUsageRepository,
	bunrepo.NewBillingStatementRepository,
	wire.Bind(new(domainstorage.BucketRepository), new(*bunrepo.BucketRepository)),
	wire.Bind(new(domainstorage.FileRepository), new(*bunrepo.FileRepository)),
	infraevents.ProviderSet,
	// 执行器按 functions.executor 配置择一绑定（P0.5）："docker"（默认，
	// v1 回退）| "dispatcher"（v2 常驻 runner，经 functions-dispatcher 分发，
	// worker 零 docker.sock 依赖）。
	infrafunctions.NewDockerExecutor,
	infrafunctions.NewDispatcherExecutor,
	infrafunctions.ProvideExecutor,
	infrapayments.ProviderSet,
	infrabilling.ProviderSet,
	infraqueue.ProviderSet,
	infrastorage.ProviderSet,
	realtime.NewStreamTransport,
)

// NewGrantsReconcileHook 返回 nil：列授权全量 reconcile（门禁 A1）是
// documentdb 域职责，仅 server 侧执行——worker 的依赖闭包不得包含
// documentdb（import guard TestWorkerDepsGraph 守此边界），经 NewOnStarts
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

func NewComponents(worker *Worker, cleaner *ChunkCleaner, trimmer *StreamTrimmer, outbox *OutboxWorkerService, paymentCloser *PaymentCloser, assetExpirer *AssetExpirer, subscriptionBiller *SubscriptionBiller, usageRollup *UsageRollupWorker) []lynx.Service {
	return []lynx.Service{worker, cleaner, trimmer, outbox, paymentCloser, assetExpirer, subscriptionBiller, usageRollup}
}

// NewStorageOptions 返回生产默认的空选项集（WithClock 等仅供测试注入）。
func NewStorageOptions() []appstorage.StorageOption { return nil }
