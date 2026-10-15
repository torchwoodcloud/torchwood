// Package webhooks 是 Webhooks 子系统的 infra 装配：投递出口复用
// infra/functions 的事件轴 Notifier（HTTPNotifier——CloudEvents 1.0
// structured mode over 签名 HTTP，per-webhook 双签），出站 client 经
// ssrfguard 守卫（NewGuardedInvokerClient 同款：webhook 是用户配置的任意
// URL，SSRF guard 默认强制、无放行开关）。本包不新增 HTTP 适配——第二套
// 投递实现即协议漂移温床（v4 §5「一个引擎两种 policy」）。
package webhooks

import (
	"net/http"

	"github.com/google/wire"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
)

// ProviderSet 是 Webhooks 投递出口的 infra 装配（server/worker 双进程同构：
// server 消费 CRUD 面、worker 消费引擎面——Notifier 在两进程都装配，成本
// 仅一个守卫 http.Client）。
var ProviderSet = wire.NewSet(
	NewGuardedDeliveryClient,
	NewNotifier,
	// 具体类型 → domainfunctions.Notifier 端口绑定（domainwebhooks.Notifier
	// 是其类型别名——webhooks 域不另立第二套投递契约）。
	wire.Bind(new(domainfunctions.Notifier), new(*infrafunctions.HTTPNotifier)),
)

// NewGuardedDeliveryClient 返回生产出站 client：ssrfguard 拨号点校验、
// 无放行开关（webhook 接收方不受 functions.invoke.allow_private_targets
// 平台级开关影响——恒 false）、无客户端级超时（预算由每次投递的 ctx
// 承载）。
func NewGuardedDeliveryClient() *http.Client {
	return infrafunctions.NewGuardedInvokerClient(false)
}

// NewNotifier 构造事件轴投递出口（domainfunctions.Notifier 生产实现）：
// HTTPNotifier + 守卫 client。签名注入（v1 当前钥 + v2 轮换窗口旧钥）在
// HTTPNotifier 内完成——per-webhook 密钥经 Notification 载荷传入。
func NewNotifier(client *http.Client) *infrafunctions.HTTPNotifier {
	return infrafunctions.NewHTTPNotifier(client)
}
