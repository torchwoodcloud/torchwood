package functions

import (
	"github.com/google/wire"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

var ProviderSet = wire.NewSet(
	// v4 RPC 轴调用出口（Invoker 端口生产实现）：守卫 client + 签名注入。
	// 事件轴 Notifier 不在本包装配——webhooks ProviderSet 已提供同一
	// HTTPNotifier（复用 infra/functions 实现，零第二套 HTTP 适配），
	// Functions 聚合与 Webhooks Service 共用同一端口绑定。
	NewInvoker,
	wire.Bind(new(domainfunctions.Invoker), new(*HTTPInvoker)),
	// P0 执行身份：Redis 执行 token 服务（server 同步路径经 validator 校验、
	// worker 异步路径经 Functions 聚合铸造/吊销；实现在本包以保 worker 依赖
	// 图不触 infra/auth，见 cmd/worker/import_guard_test.go）。
	NewRedisExecutionTokenService,
	// P1 触发器模块：HTTP 触发器 per-IP 固定窗口限频（组合根经
	// NewTriggerIPLimiter 适配到 serverhttp.TriggerIPLimiter 窄接口）。
	NewTriggerIPRateLimiter,
	// P2 客户端调用面：每用户限频 Redis 固定窗口（端口绑定在 infra
	// ProviderSet：wire.Bind(domainfunctions.ClientQuotaLimiter)）。
	NewClientQuotaLimiter,
)

// NewInvoker 构造 RPC 轴调用出口（domainfunctions.Invoker 生产实现）：
// 守卫 client（ssrfguard 拨号点校验）+ functions.invoke.allow_private_targets
// 平台级私网放行（自托管私网寻址场景，2026-10-15 私网 target 勘误）。
func NewInvoker(cfg *config.AppConfig) *HTTPInvoker {
	allow := cfg.GetFunctions().GetInvoke().GetAllowPrivateTargets()
	return NewHTTPInvoker(NewGuardedInvokerClient(allow))
}
