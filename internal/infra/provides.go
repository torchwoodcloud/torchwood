package infra

import (
	"github.com/google/wire"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	domaingroups "github.com/torchwooddev/torchwood/internal/domain/groups"
	domainidgen "github.com/torchwooddev/torchwood/internal/domain/idgen"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	domainstorage "github.com/torchwooddev/torchwood/internal/domain/storage"
	domainusers "github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/internal/infra/auth"
	"github.com/torchwooddev/torchwood/internal/infra/auth/principalcache"
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
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/pkg/uow"
)

// NewSessionServiceWithCache 构造会话服务并注入 principal 缓存（P0.5：
// 登出/封禁路径写失效标记的单一咽喉在 DeleteSessionsByUser）。
func NewSessionServiceWithCache(
	cfg *config.AppConfig,
	sessions domainauth.SessionRepository,
	roles domainauth.UserRoleResolver,
	rotation domainauth.RefreshRotationStore,
	pcache *principalcache.Cache,
) *auth.SessionService {
	s := auth.NewSessionService(cfg, sessions, roles, rotation)
	s.SetPrincipalCache(pcache)
	return s
}

// NewValidatorWithCache 构造凭证校验器并注入 principal 缓存（P0.5 热路径
// 清账；nil 缓存 = 直连 DB 实时校验，测试侧直接构造不受影响）。
func NewValidatorWithCache(
	cfg *config.AppConfig,
	apiKeyRepo projects.APIKeyRepository,
	projectRepo projects.Repository,
	adminRepo projects.AdminRepository,
	adminProjectRepo projects.AdminProjectRepository,
	adminRevokeStore domainauth.AdminTokenRevokeStore,
	sessions domainauth.SessionRepository,
	usersRepo domainusers.Repository,
	roleResolver domainauth.UserRoleResolver,
	oneTimeTokens domainauth.OneTimeTokenStore,
	execTokens domainfunctions.ExecutionTokenService,
	pcache *principalcache.Cache,
) *auth.Validator {
	v := auth.NewValidatorWithOneTimeTokens(cfg, apiKeyRepo, projectRepo, adminRepo, adminProjectRepo, adminRevokeStore, sessions, usersRepo, roleResolver, oneTimeTokens, execTokens)
	v.SetPrincipalCache(pcache)
	return v
}

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

	// P0.5 热路径清账：端用户 principal 短 TTL 缓存（validator 命中时 1 次
	// Redis 失效标记检查替代 4 次 DB 往返；登出/封禁经 SessionService 写失效
	// 标记）。组合根经包装 provider 注入，测试侧直接构造的调用点不受影响。
	principalcache.New,
	NewSessionServiceWithCache,
	NewValidatorWithCache,
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
	// P0 执行身份：铸造/校验/吊销端口（实现在 infra/functions——worker 依赖
	// 图禁入 infra/auth，见 cmd/worker/import_guard_test.go；server 与 worker
	// 都已依赖 infra/functions）。
	wire.Bind(new(domainfunctions.ExecutionTokenService), new(*infrafunctions.RedisExecutionTokenService)),
	// P2 客户端调用面：每用户限频（Redis 固定窗口；故障降级由 app 层裁决）。
	wire.Bind(new(domainfunctions.ClientQuotaLimiter), new(*infrafunctions.ClientQuotaLimiter)),

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
