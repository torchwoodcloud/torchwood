package bun

import (
	"github.com/google/wire"
	"github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
)

var ProviderSet = wire.NewSet(
	bunrepo.NewProjectRepository,
	bunrepo.NewProjectSettingsWriter,
	bunrepo.NewOAuthProviderRepository,
	bunrepo.NewAPIKeyRepository,
	bunrepo.NewInviteCodeRepository,
	bunrepo.NewIdempotencyStore,
	wire.Bind(new(databases.IdempotencyStore), new(*bunrepo.IdempotencyStore)),
	bunrepo.NewAdminRepository,
	bunrepo.NewAdminProjectRepository,
	bunrepo.NewAuditRepository,
	bunrepo.NewOutboxRepository,
	bunrepo.NewFunctionRepository,
	// P1 触发器模块：触发器仓储（function_triggers，迁移 000015）。
	bunrepo.NewFunctionTriggerRepository,
	bunrepo.NewPaymentOrderRepository,
	bunrepo.NewPaymentCallbackEventRepository,
	bunrepo.NewPaymentFulfillmentRepository,
	bunrepo.NewProviderIndexRepository,
	bunrepo.NewAssetDefRepository,
	bunrepo.NewAssetHoldingRepository,
	bunrepo.NewAssetLedgerRepository,
	bunrepo.NewSubscriptionPlanRepository,
	bunrepo.NewSubscriptionRepository,
	bunrepo.NewUsageRepository,
	bunrepo.NewBillingStatementRepository,
	bunrepo.NewUserRepository,
	bunrepo.NewSessionRepository,
	bunrepo.NewIdentityRepository,
	bunrepo.NewGroupRepository,
	bunrepo.NewMembershipRepository,
	bunrepo.NewBucketRepository,
	bunrepo.NewFileRepository,
	// Analytics 摄入仓储（PR2 摄入链；项目 schema analytics_* 表）+ 查询仓储
	//（PR3 查询面）+ worker 面仓储（PR5：rollup/分区治理/tombstone）。
	bunrepo.NewAnalyticsIngestRepository,
	wire.Bind(new(analytics.IngestRepository), new(*bunrepo.AnalyticsIngestRepository)),
	bunrepo.NewAnalyticsQueryRepository,
	wire.Bind(new(analytics.QueryRepository), new(*bunrepo.AnalyticsQueryRepository)),
	bunrepo.NewAnalyticsWorkerRepository,
	wire.Bind(new(analytics.RollupRepository), new(*bunrepo.AnalyticsWorkerRepository)),
	wire.Bind(new(analytics.MaintenanceRepository), new(*bunrepo.AnalyticsWorkerRepository)),
	wire.Bind(new(analytics.TombstoneCleaner), new(*bunrepo.AnalyticsWorkerRepository)),
	wire.Bind(new(analytics.DeletionQueueRepository), new(*bunrepo.AnalyticsWorkerRepository)),
)
