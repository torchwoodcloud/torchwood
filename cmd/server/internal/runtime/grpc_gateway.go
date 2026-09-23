package runtime

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lynx-go/grpcapi/gateway"
	"github.com/lynx-go/lynx"
	lynxhttp "github.com/lynx-go/lynx/server/http"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	consolev1 "github.com/torchwoodcloud/torchwood/genproto/console/v1"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	apirealtime "github.com/torchwoodcloud/torchwood/internal/api/realtime"
	"github.com/torchwoodcloud/torchwood/internal/api/serverhttp"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/health"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc"
)

type GRPCGatewayServer struct {
	*lynxhttp.Server
}

// authIncomingHeaderMatcher / authOutgoingHeaderMatcher：入/出站 header
// matcher 机制换库（grpcapi 阶段 1，gateway.IncomingMatcher/OutgoingMatcher），
// 项目专属头经参数注入，包级变量保留供矩阵测试与装配共用：
//   - 入站 authorization 恒拒绝（放行即双写 401），基础放行集
//     cookie/x-api-key/x-request-id/idempotency-key + 项目头 x-torchwood-project；
//   - 出站 set-cookie 与幂等重放标记 x-torchwood-replayed 直透为响应头，
//     其余 key 保持 Grpc-Metadata- 前缀默认行为。
var (
	authIncomingHeaderMatcher = gateway.IncomingMatcher("x-torchwood-project")
	authOutgoingHeaderMatcher = gateway.OutgoingMatcher("x-torchwood-replayed")
)

func NewGRPCGatewayServer(
	app lynx.App,
	cfg *config.AppConfig,
	checkers *health.Checkers,
	fileHandler *serverhttp.FileHandler,
	oauthHandler *serverhttp.OAuthHandler,
	functionsHandler *serverhttp.FunctionsHandler,
	paymentsHandler *serverhttp.PaymentsHandler,
	functionTriggersHandler *serverhttp.FunctionTriggersHandler,
	realtimeHandler *apirealtime.Handler,
	policySet *domainauth.PolicySet,
) (*GRPCGatewayServer, error) {
	httpCfg := cfg.GetServer().GetHttp()
	timeout := parseDuration(httpCfg.GetTimeout(), 60*time.Second)

	grpcAddr := cfg.GetServer().GetGrpc().GetAddr()

	mux := runtime.NewServeMux(
		runtime.WithErrorHandler(HTTPErrorHandler),
		runtime.WithIncomingHeaderMatcher(authIncomingHeaderMatcher),
		runtime.WithOutgoingHeaderMatcher(authOutgoingHeaderMatcher),
		runtime.WithMarshalerOption("*", gateway.NewMarshaler()),
		runtime.WithMarshalerOption("*/*", gateway.NewMarshaler()),
		runtime.WithMarshalerOption("application/json", gateway.NewMarshaler()),
	)

	// 拨号换库（grpcapi 阶段 1）：gateway.Dial = 端点归一（gateway.LocalEndpoint，
	// 同原 grpcEndpointFromAddr）+ insecure 凭证 + 惰性 grpc.NewClient；整条
	// gateway 共享一条连接（原先每个 Register*HandlerFromEndpoint 各自惰性
	// 建连）。转发收包上限补齐为 8MiB（与服务端 MaxRecvMsgSize 对齐；原实现
	// 未显式设置，>4MiB 的响应会在 gateway 侧被拒）。
	conn, err := gateway.Dial(app.Context(), grpcAddr, gateway.DialConfig{})
	if err != nil {
		return nil, err
	}

	register := []gateway.RegisterFunc{
		registerClient(clientv1.NewAccountServiceClient, clientv1.RegisterAccountServiceHandlerClient),
		registerClient(clientv1.NewDatabasesServiceClient, clientv1.RegisterDatabasesServiceHandlerClient),
		registerClient(clientv1.NewGroupsServiceClient, clientv1.RegisterGroupsServiceHandlerClient),
		registerClient(serverv1.NewHealthServiceClient, serverv1.RegisterHealthServiceHandlerClient),
		registerClient(serverv1.NewProjectsServiceClient, serverv1.RegisterProjectsServiceHandlerClient),
		registerClient(serverv1.NewStorageServiceClient, serverv1.RegisterStorageServiceHandlerClient),
		registerClient(serverv1.NewUsersServiceClient, serverv1.RegisterUsersServiceHandlerClient),
		// 对外 token 校验面（POST /v1/server/auth/tokens:verify，供桥接
		// 服务/Agent 经 HTTP 消费）。
		registerClient(serverv1.NewAuthServiceClient, serverv1.RegisterAuthServiceHandlerClient),
		registerClient(serverv1.NewAPIKeysServiceClient, serverv1.RegisterAPIKeysServiceHandlerClient),
		registerClient(serverv1.NewOAuthProvidersServiceClient, serverv1.RegisterOAuthProvidersServiceHandlerClient),
		registerClient(serverv1.NewGroupsServiceClient, serverv1.RegisterGroupsServiceHandlerClient),
		registerClient(serverv1.NewDatabasesServiceClient, serverv1.RegisterDatabasesServiceHandlerClient),
		registerClient(serverv1.NewFunctionsServiceClient, serverv1.RegisterFunctionsServiceHandlerClient),
		registerClient(serverv1.NewPaymentsServiceClient, serverv1.RegisterPaymentsServiceHandlerClient),
		registerClient(serverv1.NewBillingServiceClient, serverv1.RegisterBillingServiceHandlerClient),
		registerClient(clientv1.NewPaymentsServiceClient, clientv1.RegisterPaymentsServiceHandlerClient),
		registerClient(serverv1.NewAssetsServiceClient, serverv1.RegisterAssetsServiceHandlerClient),
		registerClient(serverv1.NewSubscriptionsServiceClient, serverv1.RegisterSubscriptionsServiceHandlerClient),
		registerClient(clientv1.NewAssetsServiceClient, clientv1.RegisterAssetsServiceHandlerClient),
		registerClient(clientv1.NewSubscriptionsServiceClient, clientv1.RegisterSubscriptionsServiceHandlerClient),
		registerClient(clientv1.NewFunctionsServiceClient, clientv1.RegisterFunctionsServiceHandlerClient),
		registerClient(consolev1.NewConsoleAuthServiceClient, consolev1.RegisterConsoleAuthServiceHandlerClient),
		registerClient(consolev1.NewAdminsServiceClient, consolev1.RegisterAdminsServiceHandlerClient),
		// 审计日志查询面（outbox 走 gRPC-only 未登记；Console 前端经
		// /v1/server/audit-logs 消费，必须挂 gateway）。
		registerClient(serverv1.NewAuditLogsServiceClient, serverv1.RegisterAuditLogsServiceHandlerClient),
		registerClient(clientv1.NewLeaderboardsServiceClient, clientv1.RegisterLeaderboardsServiceHandlerClient),
		registerClient(serverv1.NewLeaderboardsServiceClient, serverv1.RegisterLeaderboardsServiceHandlerClient),
		registerClient(consolev1.NewLeaderboardsServiceClient, consolev1.RegisterLeaderboardsServiceHandlerClient),
		// Analytics 摄入双面（PR2）：POST /v1/analytics/events（端侧会话）
		// 与 POST /v1/server/analytics/events（API Key/admin）。
		registerClient(clientv1.NewAnalyticsServiceClient, clientv1.RegisterAnalyticsServiceHandlerClient),
		registerClient(serverv1.NewAnalyticsServiceClient, serverv1.RegisterAnalyticsServiceHandlerClient),
		// Runbook 迁移状态面（阶段 A）：/v1/server/runbooks/{runbook}/steps
		//（CLI 经 gRPC InvokeJSON，gateway 供 Agent/OpenAPI 面）。
		registerClient(serverv1.NewRunbookServiceClient, serverv1.RegisterRunbookServiceHandlerClient),
		// RuntimeVars server 面（阶段 2 服务面）：/v1/server/runtime-var-sets
		// CRUD + vars + versions（Console/CLI/Agent 经 gateway 消费）。
		registerClient(serverv1.NewRuntimeVarsServiceClient, serverv1.RegisterRuntimeVarsServiceHandlerClient),
		// RuntimeVars client 面（阶段 3 匿名拉取端点）：GET /v1/runtime-vars/
		// {var_set_id}——客户端 SDK 轮询入口，必须挂 gateway。
		registerClient(clientv1.NewRuntimeVarsServiceClient, clientv1.RegisterRuntimeVarsServiceHandlerClient),
	}
	if err := gateway.Register(app.Context(), mux, conn, register...); err != nil {
		return nil, err
	}

	// Custom HTTP handlers for file upload/download and OAuth callbacks.
	fileHandler.Register(mux)
	oauthHandler.Register(mux)
	functionsHandler.Register(mux)
	paymentsHandler.Register(mux)
	// /f/{project_id}/{trigger_token}（P1 触发器模块）：公开触发路由，
	// token 即鉴权，不经 gRPC 拦截器链。
	functionTriggersHandler.Register(mux)
	// /.well-known/torchwood（B10）：Agent 可发现性目录——纯 HTTP 面静态
	// 路由（无 gRPC 对应物、公开端点），payload 构造期直读单一事实源。
	wellKnown := serverhttp.NewWellKnownHandler(policySet)
	wellKnown.Register(mux)

	handler := http.Handler(mux)

	consoleHandler, err := NewConsoleHandler()
	if err != nil {
		return nil, err
	}

	// / 站点首页 landing 页（精确匹配，不影响其余路径的 404 语义）。
	landingHandler := NewLandingHandler()

	// /v1/realtime 是长连接 WebSocket：不套 TimeoutHandler（下放
	// 握手超时与 ping 滑窗自行管理）；其余路径统一 60s TimeoutHandler
	// 兜底慢 handler。
	var routed http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			landingHandler.ServeHTTP(w, r)
		case r.URL.Path == "/v1/realtime":
			realtimeHandler.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/console/") || r.URL.Path == "/console":
			http.TimeoutHandler(consoleHandler, timeout, "timeout").ServeHTTP(w, r)
		default:
			http.TimeoutHandler(handler, timeout, "timeout").ServeHTTP(w, r)
		}
	})

	if cors := httpCfg.GetCors(); cors != nil {
		routed = CORSMiddleware(cors, app.Logger())(routed)
	}

	// http.Server 超时（v2 设计 §4.1 锁定）：
	// - WithTimeout(0) 只清 lynx 默认 60s（否则读写超时打在 net.Conn 上，
	//   Hijack 之后 WS 连接约 60s 被杀，ping 无法重置一次性 deadline）；
	// - WithServerOptions 在内部赋值之后执行，覆盖为
	//   ReadTimeout=0 / WriteTimeout=0 / ReadHeaderTimeout=10s（保留
	//   慢握手 / Slowloris 上限）。
	return &GRPCGatewayServer{lynxhttp.NewServer(routed,
		lynxhttp.WithAddr(httpCfg.GetAddr()),
		lynxhttp.WithTimeout(0),
		lynxhttp.WithServerOptions(func(s *http.Server) {
			s.ReadTimeout = 0
			s.WriteTimeout = 0
			s.ReadHeaderTimeout = 10 * time.Second
		}),
		// Recovery 声明在最外层：gateway 转发/自定义 handler/Console SPA
		// 任一环节 panic 都被恢复为 500 + 统一 JSON 错误体，不拖垮进程。
		lynxhttp.WithMiddleware(lynxhttp.Recovery()),
		// /healthz/readiness 依赖 checkers（任一失败 503）；请求日志为
		// Debug 级（lynx requestlog），需 --log-level debug 可见。
		lynxhttp.WithHealthCheckers(func() []lynx.Checker { return checkers.Deps() }),
		lynxhttp.WithLogger(app.Logger()),
		lynxhttp.WithRequestLog(true),
	)}, nil
}

// registerClient 以闭包适配 genproto 生成的 New*Client + Register*HandlerClient
// 对为 gateway.RegisterFunc（与原 Register*HandlerFromEndpoint 注册同源，
// 仅建连方式由"每注册一连接"收敛为共享连接）。
func registerClient[T any](
	newClient func(grpc.ClientConnInterface) T,
	register func(context.Context, *runtime.ServeMux, T) error,
) gateway.RegisterFunc {
	return func(ctx context.Context, mux *runtime.ServeMux, conn grpc.ClientConnInterface) error {
		return register(ctx, mux, newClient(conn))
	}
}

//nolint:unused
func portFromAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "8088"
	}
	return port
}
