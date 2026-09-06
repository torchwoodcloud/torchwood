package interceptor

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"google.golang.org/grpc/peer"
)

// TrustedProxies 声明可信代理网段。仅当直连 peer 命中这些网段时，
// 才采纳其转发的 X-Forwarded-For / X-Real-Ip；否则一律使用 peer 自身地址，
// 防止客户端伪造 XFF 绕过 IP 限流与审计留痕。空列表 = 不信任任何代理。
type TrustedProxies struct {
	prefixes []netip.Prefix
}

// ParseTrustedProxies 解析 CIDR 列表（也接受裸 IP，按 /32、/128 处理）。
func ParseTrustedProxies(cidrs []string) (*TrustedProxies, error) {
	tp := &TrustedProxies{}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			addr, aerr := netip.ParseAddr(c)
			if aerr != nil {
				return nil, fmt.Errorf("invalid trusted proxy %q: %w", c, err)
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		tp.prefixes = append(tp.prefixes, p.Masked())
	}
	return tp, nil
}

func (t *TrustedProxies) trusted(ip string) bool {
	if t == nil {
		return false
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	for _, p := range t.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ResolveClientIP 计算有效客户端 IP：peer 可信且带有转发头时取 XFF 首跳
// （无 XFF 退到 X-Real-Ip）；否则使用 peer 自身地址。
func (t *TrustedProxies) ResolveClientIP(peerIP, xff, realIP string) string {
	if t.trusted(peerIP) {
		if ip := FirstForwardedHop(xff); ip != "" {
			return ip
		}
		if realIP = strings.TrimSpace(realIP); realIP != "" {
			return realIP
		}
	}
	return strings.TrimSpace(peerIP)
}

// FirstForwardedHop 取 X-Forwarded-For 的首个（最靠近客户端的）地址。
func FirstForwardedHop(xff string) string {
	xff = strings.TrimSpace(xff)
	if xff == "" {
		return ""
	}
	if idx := strings.Index(xff, ","); idx >= 0 {
		xff = xff[:idx]
	}
	return strings.TrimSpace(xff)
}

// PeerIPFromAddr 从 "host:port" 形式的对端地址提取 IP 部分。
func PeerIPFromAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// PeerIP 从 gRPC 请求上下文提取直连 peer 的 IP；无 peer 信息时返回空串。
func PeerIP(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return PeerIPFromAddr(p.Addr.String())
	}
	return ""
}
