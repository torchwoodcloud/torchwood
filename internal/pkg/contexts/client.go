package contexts

import (
	"context"

	"github.com/lynx-go/grpcapi/contextx"
)

// ClientInfo captures request metadata useful for session and audit records.
//
// 门面（grpcapi 阶段 1，DESIGN §6 裁决 6）：存储机制委托
// grpcapi/contextx（与库 clientInfo 拦截器共享同一 ctx 槽），公开签名
// 保持不变，151 个引用点零改动。
type ClientInfo struct {
	IP        string
	UserAgent string
}

func WithClientInfo(ctx context.Context, info ClientInfo) context.Context {
	return contextx.WithClientInfo(ctx, contextx.ClientInfo(info))
}

func ClientInfoFrom(ctx context.Context) ClientInfo {
	v, _ := contextx.ClientInfoFrom(ctx)
	return ClientInfo(v)
}
