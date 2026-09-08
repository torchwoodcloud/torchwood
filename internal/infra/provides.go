package infra

import (
	"github.com/google/wire"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	domaingroups "github.com/torchwooddev/torchwood/internal/domain/groups"
	domainidgen "github.com/torchwooddev/torchwood/internal/domain/idgen"
	domainstorage "github.com/torchwooddev/torchwood/internal/domain/storage"
	domainusers "github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/internal/infra/auth"
	infrabilling "github.com/torchwooddev/torchwood/internal/infra/billing"
	"github.com/torchwooddev/torchwood/internal/infra/bun"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/infra/documentdb"
	infraevents "github.com/torchwooddev/torchwood/internal/infra/events"
	infrafunctions "github.com/torchwooddev/torchwood/internal/infra/functions"
	"github.com/torchwooddev/torchwood/internal/infra/health"
	infraidgen "github.com/torchwooddev/torchwood/internal/infra/idgen"
	inframessaging "github.com/torchwooddev/torchwood/internal/infra/messaging"
	infrapayments "github.com/torchwooddev/torchwood/internal/infra/payments"
	infraqueue "github.com/torchwooddev/torchwood/internal/infra/queue"
	infrarealtime "github.com/torchwooddev/torchwood/internal/infra/realtime"
	infrastorage "github.com/torchwooddev/torchwood/internal/infra/storage"
	"github.com/torchwooddev/torchwood/pkg/uow"
)

var ProviderSet = wire.NewSet(
	clients.NewDataClients,
	clients.NewDatabase,
	clients.NewRedis,
	wire.Bind(new(uow.Runner), new(*clients.Database)),
	wire.Bind(new(uow.Isolator), new(*clients.Database)),
	// projectschema.SchemaManager 的提供与 domain 绑定已迁至 server 组合根
	// （cmd/server/provides.go NewSchemaManager，Round4 J5-3：桥接 documentdb
	// internalIDCache 失效回调）。
	health.NewCheckers,

	auth.NewValidatorWithOneTimeTokens,
	auth.NewSessionService,
	auth.NewRedisOTPChallengeStore,
	auth.NewRedisOAuthStateStore,
	auth.NewRedisAccountTokenStore,
	auth.NewRedisAdminTokenRevokeStore,
	auth.NewRedisLoginThrottleFromConfig,
	auth.NewRedisRefreshRotationStore,
	auth.NewRedisRateLimiter,
	auth.NewTOTPService,
	auth.NewRedisMFAChallengeStore,
	auth.NewRedisOneTimeTokenStore,
	auth.NewOAuthAuthenticatorFactory,
	auth.NewWeChatMiniProgramExchanger,
	auth.NewOTPGenerator,
	auth.NewSessionCookieVerifier,
	wire.Bind(new(domainauth.SessionService), new(*auth.SessionService)),
	wire.Bind(new(domainauth.OTPChallengeStore), new(*auth.RedisOTPChallengeStore)),
	wire.Bind(new(domainauth.OAuthStateStore), new(*auth.RedisOAuthStateStore)),
	wire.Bind(new(domainauth.AccountTokenStore), new(*auth.RedisAccountTokenStore)),
	wire.Bind(new(domainauth.AdminTokenRevokeStore), new(*auth.RedisAdminTokenRevokeStore)),
	wire.Bind(new(domainauth.LoginThrottle), new(*auth.RedisLoginThrottle)),
	wire.Bind(new(domainauth.RefreshRotationStore), new(*auth.RedisRefreshRotationStore)),
	wire.Bind(new(domainauth.RateLimiter), new(*auth.RedisRateLimiter)),
	wire.Bind(new(domainauth.OneTimeTokenStore), new(*auth.RedisOneTimeTokenStore)),

	infraidgen.ProviderSet,
	wire.Bind(new(domainidgen.Generator), new(*infraidgen.Service)),

	inframessaging.ProviderSet,

	bun.ProviderSet,
	documentdb.ProviderSet,
	wire.Bind(new(domainusers.Repository), new(*bunrepo.UserRepository)),
	wire.Bind(new(domainauth.SessionRepository), new(*bunrepo.SessionRepository)),
	wire.Bind(new(domainauth.IdentityRepository), new(*bunrepo.IdentityRepository)),
	wire.Bind(new(domaingroups.GroupRepository), new(*bunrepo.GroupRepository)),
	wire.Bind(new(domaingroups.MembershipRepository), new(*bunrepo.MembershipRepository)),
	wire.Bind(new(domainstorage.BucketRepository), new(*bunrepo.BucketRepository)),
	wire.Bind(new(domainstorage.FileRepository), new(*bunrepo.FileRepository)),
	infraevents.ProviderSet,
	infrarealtime.ProviderSet,
	infrastorage.ProviderSet,
	infrafunctions.ProviderSet,
	infrapayments.ProviderSet,
	infrabilling.ProviderSet,
	infraqueue.ProviderSet,
)
