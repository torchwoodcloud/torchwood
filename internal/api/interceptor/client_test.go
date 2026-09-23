package interceptor_test

import (
	"context"
	"net"
	"testing"

	grpcapiinterceptor "github.com/lynx-go/grpcapi/interceptor"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/api/interceptor"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// clientInfo 拦截器行为矩阵（grpcapi 阶段 1 换库后）：实现本体在
// grpcapi/interceptor，本文件经 torchwood 包符号 + contexts 门面读取，
// 锁定换库后与 ctx 槽（contextx.ClientInfo）的端到端对接。
//
// 行为差异声明（相对旧本地实现，有意改进）：peer 命中可信网段时 XFF
// 解析为"自右向左逐跳回溯，第一个不可信地址即客户端"（客户端伪造注入
// 的前缀跳被截断）；旧实现直取首跳，会把客户端自行携带的伪造跳当作来源。
func runClientInfo(t *testing.T, ic *grpcapiinterceptor.ClientInfoInterceptor, ctx context.Context) contexts.ClientInfo {
	t.Helper()
	var captured contexts.ClientInfo
	_, err := ic.Unary()(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) {
		captured = contexts.ClientInfoFrom(ctx)
		return nil, nil
	})
	require.NoError(t, err)
	return captured
}

func incomingWithPeer(md metadata.MD, addr string) context.Context {
	ctx := metadata.NewIncomingContext(context.Background(), md)
	return peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(addr), Port: 12345}})
}

func TestClientInfoInterceptor_NoPeerFallsBackToHeaders(t *testing.T) {
	t.Parallel()

	ic := grpcapiinterceptor.NewClientInfo(grpcapiinterceptor.ClientInfoConfig{})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-forwarded-for", "203.0.113.1, 10.0.0.1",
		"grpcgateway-user-agent", "TestAgent/2.0",
	))

	captured := runClientInfo(t, ic, ctx)
	require.Equal(t, "203.0.113.1", captured.IP)
	require.Equal(t, "TestAgent/2.0", captured.UserAgent)
}

func TestClientInfoInterceptor_UntrustedPeerIgnoresXFF(t *testing.T) {
	t.Parallel()

	ic := grpcapiinterceptor.NewClientInfo(grpcapiinterceptor.ClientInfoConfig{
		TrustedProxies: []string{"10.0.0.0/8"},
	})

	ctx := incomingWithPeer(metadata.Pairs(
		"x-forwarded-for", "203.0.113.99",
		"x-real-ip", "198.51.100.7",
	), "192.0.2.10")

	captured := runClientInfo(t, ic, ctx)
	require.Equal(t, "192.0.2.10", captured.IP)
}

// 行为变化点（原 TrustedPeerUsesXFFFirstHop）：peer 可信时不再直取首跳，
// 而是自右向左回溯——仅边缘代理（127.0.0.1）可信时，XFF 最右跳
// "10.0.0.1" 是它所见地址且不在可信集内，即被认定为客户端；旧实现会
// 直取首跳 "203.0.113.1"（含客户端可伪造跳）。
func TestClientInfoInterceptor_TrustedPeerBacktracksToRightmostUntrusted(t *testing.T) {
	t.Parallel()

	ic := grpcapiinterceptor.NewClientInfo(grpcapiinterceptor.ClientInfoConfig{
		TrustedProxies: []string{"127.0.0.1/32"},
	})

	ctx := incomingWithPeer(metadata.Pairs(
		"x-forwarded-for", "203.0.113.1, 10.0.0.1",
	), "127.0.0.1")

	captured := runClientInfo(t, ic, ctx)
	require.Equal(t, "10.0.0.1", captured.IP)
}

// 伪造跳截断：客户端自带的 "6.6.6.6" 前缀跳不得成为来源——逐跳回溯从
// 可信链末端（10.0.0.1 → 203.0.113.1）向左，停在第一个不可信地址。
func TestClientInfoInterceptor_TrustedPeerTruncatesSpoofedPrefixHops(t *testing.T) {
	t.Parallel()

	ic := grpcapiinterceptor.NewClientInfo(grpcapiinterceptor.ClientInfoConfig{
		TrustedProxies: []string{"10.0.0.0/8", "192.0.2.0/24"},
	})

	ctx := incomingWithPeer(metadata.Pairs(
		"x-forwarded-for", "6.6.6.6, 203.0.113.1, 10.0.0.1",
	), "192.0.2.5")

	captured := runClientInfo(t, ic, ctx)
	require.Equal(t, "203.0.113.1", captured.IP)
}

func TestClientInfoInterceptor_TrustedPeerWithoutXFFFallsBackToRealIP(t *testing.T) {
	t.Parallel()

	ic := grpcapiinterceptor.NewClientInfo(grpcapiinterceptor.ClientInfoConfig{
		TrustedProxies: []string{"127.0.0.0/8"},
	})

	ctx := incomingWithPeer(metadata.Pairs(
		"x-real-ip", "198.51.100.7",
	), "127.0.0.1")

	captured := runClientInfo(t, ic, ctx)
	require.Equal(t, "198.51.100.7", captured.IP)
}

func TestClientInfoInterceptor_EmptyTrustedListNeverTrusts(t *testing.T) {
	t.Parallel()

	ic := grpcapiinterceptor.NewClientInfo(grpcapiinterceptor.ClientInfoConfig{})
	ctx := incomingWithPeer(metadata.Pairs(
		"x-forwarded-for", "203.0.113.99",
	), "127.0.0.1")

	captured := runClientInfo(t, ic, ctx)
	require.Equal(t, "127.0.0.1", captured.IP)
}

func TestParseTrustedProxies(t *testing.T) {
	t.Parallel()

	tp, err := interceptor.ParseTrustedProxies([]string{"10.0.0.0/8", " 192.0.2.1 ", ""})
	require.NoError(t, err)
	require.NotNil(t, tp)

	_, err = interceptor.ParseTrustedProxies([]string{"not-a-cidr"})
	require.Error(t, err)
}
