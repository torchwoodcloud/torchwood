package interceptor

import (
	"context"

	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// ClientInfoInterceptor extracts client IP and user agent from gRPC metadata
// (populated by grpc-gateway from HTTP headers) into the request context.
// X-Forwarded-For / X-Real-Ip 仅在直连 peer 命中可信代理网段时被采纳，
// 否则一律使用 gRPC peer 地址，防止伪造来源绕过 IP 限流与审计。
type ClientInfoInterceptor struct {
	trusted *TrustedProxies
}

func NewClientInfoInterceptor(trusted *TrustedProxies) *ClientInfoInterceptor {
	return &ClientInfoInterceptor{trusted: trusted}
}

func (c *ClientInfoInterceptor) UnaryMiddleware(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = contexts.WithClientInfo(ctx, c.extractClientInfo(ctx, md))
	}
	return handler(ctx, req)
}

func (c *ClientInfoInterceptor) extractClientInfo(ctx context.Context, md metadata.MD) contexts.ClientInfo {
	xff := firstMetadataValue(md, "x-forwarded-for")
	if xff == "" {
		// grpc-gateway 默认会给非 IANA 永久头加 grpcgateway- 前缀。
		xff = firstMetadataValue(md, "grpcgateway-x-forwarded-for")
	}
	realIP := firstMetadataValue(md, "x-real-ip")

	var ip string
	if peerIP := PeerIP(ctx); peerIP != "" {
		ip = c.trusted.ResolveClientIP(peerIP, xff, realIP)
	} else {
		// 无 peer 信息（进程内调用/测试）时退化为直接取头部。
		ip = FirstForwardedHop(xff)
		if ip == "" {
			ip = realIP
		}
	}

	ua := firstMetadataValue(md, "grpcgateway-user-agent")
	if ua == "" {
		ua = firstMetadataValue(md, "user-agent")
	}
	return contexts.ClientInfo{IP: ip, UserAgent: ua}
}
