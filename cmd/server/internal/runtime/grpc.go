package runtime

import (
	"fmt"
	"time"

	"github.com/lynx-go/grpcapi/authz"
	grpcapiinterceptor "github.com/lynx-go/grpcapi/interceptor"
	"github.com/lynx-go/grpcapi/policy"
	grpcapiserver "github.com/lynx-go/grpcapi/server"
	"github.com/lynx-go/lynx"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	consolev1 "github.com/torchwoodcloud/torchwood/genproto/console/v1"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/internal/api/clientgrpc"
	"github.com/torchwoodcloud/torchwood/internal/api/consolegrpc"
	"github.com/torchwoodcloud/torchwood/internal/api/interceptor"
	"github.com/torchwoodcloud/torchwood/internal/api/servergrpc"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/health"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func NewGRPCServer(
	app lynx.App,
	cfg *config.AppConfig,
	validator *auth.Validator,
	auditRepo audit.Repository,
	rateLimiter domainauth.RateLimiter,
	checkers *health.Checkers,
	account *clientgrpc.AccountService,
	clientDatabases *clientgrpc.DatabasesService,
	clientGroups *clientgrpc.GroupsService,
	clientPayments *clientgrpc.PaymentsService,
	clientAssets *clientgrpc.AssetsService,
	clientSubscriptions *clientgrpc.SubscriptionsService,
	clientFunctions *clientgrpc.FunctionsService,
	clientLeaderboards *clientgrpc.LeaderboardsService,
	clientAnalytics *clientgrpc.AnalyticsService,
	clientRuntimeVars *clientgrpc.RuntimeVarsService,
	health *servergrpc.HealthService,
	projects *servergrpc.ProjectsService,
	storage *servergrpc.StorageService,
	users *servergrpc.UsersService,
	// 对外 token 校验面（AuthService/VerifyToken，introspection）。
	serverAuth *servergrpc.AuthService,
	apiKeys *servergrpc.APIKeysService,
	oauthProviders *servergrpc.OAuthProvidersService,
	groups *servergrpc.GroupsService,
	databases *servergrpc.DatabasesService,
	functions *servergrpc.FunctionsService,
	// Webhooks 一等公民（functions-v4.md §6，D5）：订阅 CRUD + 投递浏览。
	webhooks *servergrpc.WebhooksService,
	serverPayments *servergrpc.PaymentsService,
	serverAssets *servergrpc.AssetsService,
	serverSubscriptions *servergrpc.SubscriptionsService,
	billingService *servergrpc.BillingService,
	usageMeter domainbilling.UsageCounter,
	consoleAuth *consolegrpc.AuthService,
	adminsService *consolegrpc.AdminsService,
	outboxService *servergrpc.OutboxService,
	auditLogsService *servergrpc.AuditLogsService,
	serverLeaderboards *servergrpc.LeaderboardsService,
	consoleLeaderboards *consolegrpc.LeaderboardsService,
	serverAnalytics *servergrpc.AnalyticsService,
	serverRunbook *servergrpc.RunbookService,
	serverRuntimeVars *servergrpc.RuntimeVarsService,
	policySet *policy.Set,
) (*GRPCAPIService, error) {
	grpcCfg := cfg.GetServer().GetGrpc()
	timeout := parseDuration(grpcCfg.GetTimeout(), 30*time.Second)

	// 执行侧迁移（grpcapi v1.0 §11.4）：授权执行走库 authz 拦截器
	//（RequirementDecider 按规范树求值），挂在 torchwood authn 拦截器
	//（认证 + 业务校验 + 双 principal 注入）之后。策略唯一来源仍是
	// ProvideMethodPolicies 注入的策略集。
	authInterceptor, err := interceptor.NewAuthInterceptor(validator, policySet)
	if err != nil {
		return nil, err
	}
	authInterceptor = authInterceptor.WithLogger(app.Logger())
	// M5 C6：auth 拒绝并联审计（拦截器层拒绝到不了 audit 中间件，无双写）。
	authInterceptor = authInterceptor.WithDenyAuditSink(auditRepo)
	authzInterceptor, err := authz.NewInterceptor(policySet, nil, authz.Options{
		OnDecision: authInterceptor.AuthzOnDecision(),
		Logger:     app.Logger(),
	})
	if err != nil {
		return nil, err
	}
	trustedProxies, err := interceptor.ParseTrustedProxies(cfg.GetSecurity().GetTrustedProxies())
	if err != nil {
		return nil, fmt.Errorf("parse security.trusted_proxies: %w", err)
	}
	auditInterceptor := interceptor.NewAuditInterceptor(auditRepo).WithLogger(app.Logger()).WithTrustedProxies(trustedProxies)
	// clientInfo 换库（grpcapi 阶段 1）：机制本体在 grpcapi/interceptor，配置
	// 仍以 security.trusted_proxies 注入。上面的 ParseTrustedProxies 保留用于
	// 启动期报错 + audit 回退路径；库构造器内部按同一规则再解析一次（结果
	// 一致；解析失败库侧 fail-closed 不采信转发头）。XFF 解析语义为库版的
	// 自右向左逐跳回溯（第一个不可信地址即客户端），不再是直取首跳。
	clientInfoInterceptor := grpcapiinterceptor.NewClientInfo(grpcapiinterceptor.ClientInfoConfig{
		TrustedProxies: cfg.GetSecurity().GetTrustedProxies(),
	})
	// 通用 API 限流（roadmap §3.4）：挂在 clientInfo 与 auth 之后（需要
	// trusted-proxy 校验后的 IP 与 principal）、audit 之前；复用
	// domainauth.RateLimiter 端口的 Redis 固定窗口实现。
	rateLimitInterceptor := interceptor.NewRateLimitInterceptor(rateLimiter, cfg)
	usageInterceptor := interceptor.NewUsageInterceptor(usageMeter).WithLogger(app.Logger())
	// 形状校验（protovalidate 注解，09-api-guide §1.6）换库：插在链尾——
	// audit/usage 之后、handler 之前。校验失败请求与手写校验时期行为
	// 完全一致（InvalidArgument 审计行照常落库、用量照常计数），只是把
	// handler 开头的形状检查外提为 proto 声明。
	validateInterceptor := grpcapiinterceptor.NewValidate()

	// 拦截器链换库装配（lynx 深度集成）：torchwood 自有拦截器按库 Stage
	// 语义入链（名字/顺序 = 库约定；clientinfo 打头、validate 收尾，
	// authn 先于 authz 由 server.New 校验）。流式面 = clientInfo/authn/authz
	//（rateLimit/audit/usage 暂无 stream 实现，与既有装配一致）。
	chain, err := grpcapiinterceptor.AssembleWith(grpcapiinterceptor.DefaultChainOptions(),
		grpcapiinterceptor.Stage{Name: grpcapiinterceptor.StageClientInfo, Order: grpcapiinterceptor.OrderClientInfo,
			Unary: clientInfoInterceptor.Unary(), Stream: clientInfoInterceptor.Stream()},
		grpcapiinterceptor.Stage{Name: grpcapiinterceptor.StageAuthn, Order: grpcapiinterceptor.OrderAuthn,
			Unary: authInterceptor.UnaryAuthMiddleware, Stream: authInterceptor.StreamAuthMiddleware},
		grpcapiinterceptor.Stage{Name: grpcapiinterceptor.StageRateLimit, Order: grpcapiinterceptor.OrderRateLimit,
			Unary: rateLimitInterceptor.UnaryRateLimitMiddleware},
		grpcapiinterceptor.Stage{Name: grpcapiinterceptor.StageAudit, Order: grpcapiinterceptor.OrderAudit,
			Unary: auditInterceptor.UnaryAuditMiddleware},
		grpcapiinterceptor.Stage{Name: grpcapiinterceptor.StageUsage, Order: grpcapiinterceptor.OrderUsage,
			Unary: usageInterceptor.UnaryUsageMiddleware},
		grpcapiinterceptor.Stage{Name: grpcapiinterceptor.StageAuthz, Order: grpcapiinterceptor.OrderAuthz,
			Unary: authzInterceptor.Unary(), Stream: authzInterceptor.Stream()},
		grpcapiinterceptor.Stage{Name: grpcapiinterceptor.StageValidate, Order: grpcapiinterceptor.OrderValidate,
			Unary: validateInterceptor.Unary()},
	)
	if err != nil {
		return nil, err
	}

	// gRPC 面装配换库（lynx 深度集成）：server.New 统一装配服务注册、
	// 拦截器链与启动断言（注册↔策略双向覆盖、extra 冲突、链序），传输层
	// 生命周期由 grpcAPIService 适配 lynx（listener/排水/健康轮询）。
	// gateway（HTTP 面）维持既有装配：网络拨号到本服务监听地址。
	apiServer, err := grpcapiserver.New(app.Context(), grpcapiserver.Config{
		Policy:      policySet,
		Services:    apiRegistrations(account, clientDatabases, clientGroups, clientPayments, clientAssets, clientSubscriptions, clientFunctions, clientLeaderboards, clientAnalytics, clientRuntimeVars, health, projects, storage, users, serverAuth, apiKeys, oauthProviders, groups, databases, functions, webhooks, serverPayments, serverAssets, serverSubscriptions, billingService, consoleAuth, adminsService, auditLogsService, serverLeaderboards, consoleLeaderboards, serverAnalytics, serverRunbook, serverRuntimeVars, outboxService),
		DisableHTTP: true,
		// 健康面由 grpcAPIService 自注册（lynx 风格轮询聚合）。
		DisableHealth: true,
		Chain:         chain,
		// torchwood authn 是自定义 Stage（凭证语义归 torchwood），不经
		// grpcapi authn.Chain；authz stage 在链上，受 server.New 校验。
		AllowNoAuthenticator: true,
		AllowNoCSRF:          true,
		MaxRecvMsgSize:       8 << 20,
		TrustedProxies:       cfg.GetSecurity().GetTrustedProxies(),
		Logger:               app.Logger(),
	})
	if err != nil {
		return nil, err
	}
	gs := apiServer.GRPCServer()

	// fail-closed：所有已注册方法都必须带有策略声明（注册 → 策略方向；
	// 反向「策略 → 注册」由 server.New validateRegisteredPolicies 校验）。
	if err := grpcapiinterceptor.AssertAllRegisteredHavePolicy(gs, policySet); err != nil {
		return nil, err
	}

	svc := NewGRPCAPIService("grpc", grpcCfg.GetAddr(), apiServer, gs, checkers.Deps, timeout, app.Logger())
	return svc, nil
}

// apiRegistrations 把业务服务实现登记为 grpcapi Registration（gRPC 注册
// 回调 + 服务 descriptor；HTTP 注册由既有 gateway 装配承担，故不设
// RegisterHTTP——server.New 以 DisableHTTP 跳过该面）。
func apiRegistrations(account *clientgrpc.AccountService, clientDatabases *clientgrpc.DatabasesService, clientGroups *clientgrpc.GroupsService, clientPayments *clientgrpc.PaymentsService, clientAssets *clientgrpc.AssetsService, clientSubscriptions *clientgrpc.SubscriptionsService, clientFunctions *clientgrpc.FunctionsService, clientLeaderboards *clientgrpc.LeaderboardsService, clientAnalytics *clientgrpc.AnalyticsService, clientRuntimeVars *clientgrpc.RuntimeVarsService, health *servergrpc.HealthService, projects *servergrpc.ProjectsService, storage *servergrpc.StorageService, users *servergrpc.UsersService, serverAuth *servergrpc.AuthService, apiKeys *servergrpc.APIKeysService, oauthProviders *servergrpc.OAuthProvidersService, groups *servergrpc.GroupsService, databases *servergrpc.DatabasesService, functions *servergrpc.FunctionsService, webhooks *servergrpc.WebhooksService, serverPayments *servergrpc.PaymentsService, serverAssets *servergrpc.AssetsService, serverSubscriptions *servergrpc.SubscriptionsService, billingService *servergrpc.BillingService, consoleAuth *consolegrpc.AuthService, adminsService *consolegrpc.AdminsService, auditLogsService *servergrpc.AuditLogsService, serverLeaderboards *servergrpc.LeaderboardsService, consoleLeaderboards *consolegrpc.LeaderboardsService, serverAnalytics *servergrpc.AnalyticsService, serverRunbook *servergrpc.RunbookService, serverRuntimeVars *servergrpc.RuntimeVarsService, outboxService *servergrpc.OutboxService) []grpcapiserver.Registration {
	reg := func(sd protoreflect.ServiceDescriptor, register func(*grpc.Server)) grpcapiserver.Registration {
		return grpcapiserver.Registration{Name: string(sd.FullName()), Descriptor: sd, RegisterGRPC: register}
	}
	clientFile := func(f protoreflect.FileDescriptor) protoreflect.ServiceDescriptor { return f.Services().Get(0) }
	serverFile := func(f protoreflect.FileDescriptor) protoreflect.ServiceDescriptor { return f.Services().Get(0) }
	regs := []grpcapiserver.Registration{
		reg(clientFile(clientv1.File_client_v1_account_proto), func(s *grpc.Server) { clientv1.RegisterAccountServiceServer(s, account) }),
		reg(clientFile(clientv1.File_client_v1_databases_proto), func(s *grpc.Server) { clientv1.RegisterDatabasesServiceServer(s, clientDatabases) }),
		reg(clientFile(clientv1.File_client_v1_groups_proto), func(s *grpc.Server) { clientv1.RegisterGroupsServiceServer(s, clientGroups) }),
		reg(clientFile(clientv1.File_client_v1_payments_proto), func(s *grpc.Server) { clientv1.RegisterPaymentsServiceServer(s, clientPayments) }),
		reg(clientFile(clientv1.File_client_v1_assets_proto), func(s *grpc.Server) { clientv1.RegisterAssetsServiceServer(s, clientAssets) }),
		reg(clientFile(clientv1.File_client_v1_subscriptions_proto), func(s *grpc.Server) { clientv1.RegisterSubscriptionsServiceServer(s, clientSubscriptions) }),
		reg(clientFile(clientv1.File_client_v1_functions_proto), func(s *grpc.Server) { clientv1.RegisterFunctionsServiceServer(s, clientFunctions) }),
		reg(clientFile(clientv1.File_client_v1_leaderboards_proto), func(s *grpc.Server) { clientv1.RegisterLeaderboardsServiceServer(s, clientLeaderboards) }),
		// Analytics 摄入双面（PR2）：client 面 Principal 归因、server 面
		// 可信代报；查询实现随 PR3。
		reg(clientFile(clientv1.File_client_v1_analytics_proto), func(s *grpc.Server) { clientv1.RegisterAnalyticsServiceServer(s, clientAnalytics) }),
		// RuntimeVars client 面（阶段 3 匿名拉取端点）。
		reg(clientFile(clientv1.File_client_v1_runtime_vars_proto), func(s *grpc.Server) { clientv1.RegisterRuntimeVarsServiceServer(s, clientRuntimeVars) }),
		reg(serverFile(serverv1.File_server_v1_health_proto), func(s *grpc.Server) { serverv1.RegisterHealthServiceServer(s, health) }),
		reg(serverFile(serverv1.File_server_v1_projects_proto), func(s *grpc.Server) { serverv1.RegisterProjectsServiceServer(s, projects) }),
		reg(serverFile(serverv1.File_server_v1_storage_proto), func(s *grpc.Server) { serverv1.RegisterStorageServiceServer(s, storage) }),
		reg(serverFile(serverv1.File_server_v1_users_proto), func(s *grpc.Server) { serverv1.RegisterUsersServiceServer(s, users) }),
		// 对外 token 校验面（introspection）。
		reg(serverFile(serverv1.File_server_v1_auth_proto), func(s *grpc.Server) { serverv1.RegisterAuthServiceServer(s, serverAuth) }),
		reg(serverFile(serverv1.File_server_v1_apikeys_proto), func(s *grpc.Server) { serverv1.RegisterAPIKeysServiceServer(s, apiKeys) }),
		reg(serverFile(serverv1.File_server_v1_oauth_providers_proto), func(s *grpc.Server) { serverv1.RegisterOAuthProvidersServiceServer(s, oauthProviders) }),
		reg(serverFile(serverv1.File_server_v1_groups_proto), func(s *grpc.Server) { serverv1.RegisterGroupsServiceServer(s, groups) }),
		reg(serverFile(serverv1.File_server_v1_databases_proto), func(s *grpc.Server) { serverv1.RegisterDatabasesServiceServer(s, databases) }),
		reg(serverFile(serverv1.File_server_v1_functions_proto), func(s *grpc.Server) { serverv1.RegisterFunctionsServiceServer(s, functions) }),
		// Webhooks 一等公民（functions-v4.md §6，D5）：9 方法全实现。
		reg(serverFile(serverv1.File_server_v1_webhooks_proto), func(s *grpc.Server) { serverv1.RegisterWebhooksServiceServer(s, webhooks) }),
		reg(serverFile(serverv1.File_server_v1_payments_proto), func(s *grpc.Server) { serverv1.RegisterPaymentsServiceServer(s, serverPayments) }),
		reg(serverFile(serverv1.File_server_v1_assets_proto), func(s *grpc.Server) { serverv1.RegisterAssetsServiceServer(s, serverAssets) }),
		reg(serverFile(serverv1.File_server_v1_subscriptions_proto), func(s *grpc.Server) { serverv1.RegisterSubscriptionsServiceServer(s, serverSubscriptions) }),
		reg(serverFile(serverv1.File_server_v1_billing_proto), func(s *grpc.Server) { serverv1.RegisterBillingServiceServer(s, billingService) }),
		reg(clientFile(consolev1.File_console_v1_auth_proto), func(s *grpc.Server) { consolev1.RegisterConsoleAuthServiceServer(s, consoleAuth) }),
		reg(clientFile(consolev1.File_console_v1_admins_proto), func(s *grpc.Server) { consolev1.RegisterAdminsServiceServer(s, adminsService) }),
		reg(clientFile(consolev1.File_console_v1_leaderboards_proto), func(s *grpc.Server) { consolev1.RegisterLeaderboardsServiceServer(s, consoleLeaderboards) }),
		reg(serverFile(serverv1.File_server_v1_audit_logs_proto), func(s *grpc.Server) { serverv1.RegisterAuditLogsServiceServer(s, auditLogsService) }),
		reg(serverFile(serverv1.File_server_v1_leaderboards_proto), func(s *grpc.Server) { serverv1.RegisterLeaderboardsServiceServer(s, serverLeaderboards) }),
		// 查询方法嵌 Unimplemented 占位（实现随 PR3），注册保证策略/反射/
		// swagger 覆盖断言即刻可见。
		reg(serverFile(serverv1.File_server_v1_analytics_proto), func(s *grpc.Server) { serverv1.RegisterAnalyticsServiceServer(s, serverAnalytics) }),
		// Runbook 迁移状态面（阶段 A：三方法全实现，策略/swagger 断言即刻可见）。
		reg(serverFile(serverv1.File_server_v1_runbook_proto), func(s *grpc.Server) { serverv1.RegisterRunbookServiceServer(s, serverRunbook) }),
		// RuntimeVars server 面（阶段 2 服务面：13 方法 + runtime_vars scope 词表）。
		reg(serverFile(serverv1.File_server_v1_runtime_vars_proto), func(s *grpc.Server) { serverv1.RegisterRuntimeVarsServiceServer(s, serverRuntimeVars) }),
	}
	// Outbox 走读面条件装配（实现存在才登记，nil 跳过）。
	if outboxService != nil {
		regs = append(regs, reg(serverv1.File_server_v1_outbox_proto.Services().Get(0),
			func(s *grpc.Server) { serverv1.RegisterOutboxServiceServer(s, outboxService) }))
	}
	return regs
}

// authzFileDescriptors 是 authz 推导（collectMethodsByAccess）与 swagger 一致性
// 测试共用的业务 proto 文件清单（单一事实源；新增服务文件只登记此处）。
func authzFileDescriptors() []protoreflect.FileDescriptor {
	return []protoreflect.FileDescriptor{
		clientv1.File_client_v1_account_proto,
		clientv1.File_client_v1_databases_proto,
		clientv1.File_client_v1_groups_proto,
		clientv1.File_client_v1_payments_proto,
		clientv1.File_client_v1_assets_proto,
		clientv1.File_client_v1_subscriptions_proto,
		clientv1.File_client_v1_functions_proto,
		clientv1.File_client_v1_leaderboards_proto,
		// Analytics（docs/design/analytics.md）：client 面仅摄入；PR1 只登记
		// proto 策略（handler/注册随 PR2 到位）。
		clientv1.File_client_v1_analytics_proto,
		// RuntimeVars client 面（docs/design/runtime-vars.md §2.3：唯一方法
		// GetRuntimeVars，ACCESS_PUBLIC 白名单登记见 domain/auth/policy.go）。
		clientv1.File_client_v1_runtime_vars_proto,
		serverv1.File_server_v1_projects_proto,
		serverv1.File_server_v1_health_proto,
		serverv1.File_server_v1_storage_proto,
		serverv1.File_server_v1_users_proto,
		// 对外 token 校验面（introspection）。
		serverv1.File_server_v1_auth_proto,
		serverv1.File_server_v1_apikeys_proto,
		serverv1.File_server_v1_oauth_providers_proto,
		serverv1.File_server_v1_groups_proto,
		serverv1.File_server_v1_databases_proto,
		serverv1.File_server_v1_functions_proto,
		// Webhooks 一等公民（functions-v4.md §6，D5）：proto 策略登记。
		serverv1.File_server_v1_webhooks_proto,
		serverv1.File_server_v1_payments_proto,
		serverv1.File_server_v1_assets_proto,
		serverv1.File_server_v1_subscriptions_proto,
		serverv1.File_server_v1_billing_proto,
		serverv1.File_server_v1_outbox_proto,
		serverv1.File_server_v1_audit_logs_proto,
		serverv1.File_server_v1_leaderboards_proto,
		// Analytics server 面：摄入 + 七查询；PR1 只登记 proto 策略
		// （摄入 handler 随 PR2、查询实现随 PR3 到位）。
		serverv1.File_server_v1_analytics_proto,
		// Runbook 迁移状态面（docs/design/runbook.md §2.2 阶段 A）。
		serverv1.File_server_v1_runbook_proto,
		// RuntimeVars server 面（docs/design/runtime-vars.md §2.3；client 面
		// proto 随阶段 3 登记）。
		serverv1.File_server_v1_runtime_vars_proto,
		consolev1.File_console_v1_auth_proto,
		consolev1.File_console_v1_admins_proto,
		consolev1.File_console_v1_leaderboards_proto,
	}
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}
