package bun

import (
	"github.com/google/wire"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
)

var ProviderSet = wire.NewSet(
	bunrepo.NewProjectRepository,
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
)
