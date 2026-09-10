package interceptor_test

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/api/interceptor"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

func runClientInfo(t *testing.T, ic *interceptor.ClientInfoInterceptor, ctx context.Context) contexts.ClientInfo {
	t.Helper()
	var captured contexts.ClientInfo
	_, err := ic.UnaryMiddleware(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) {
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

	ic := interceptor.NewClientInfoInterceptor(nil)
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

	trusted, err := interceptor.ParseTrustedProxies([]string{"10.0.0.0/8"})
	require.NoError(t, err)
	ic := interceptor.NewClientInfoInterceptor(trusted)

	ctx := incomingWithPeer(metadata.Pairs(
		"x-forwarded-for", "203.0.113.99",
		"x-real-ip", "198.51.100.7",
	), "192.0.2.10")

	captured := runClientInfo(t, ic, ctx)
	require.Equal(t, "192.0.2.10", captured.IP)
}

func TestClientInfoInterceptor_TrustedPeerUsesXFFFirstHop(t *testing.T) {
	t.Parallel()

	trusted, err := interceptor.ParseTrustedProxies([]string{"127.0.0.1/32"})
	require.NoError(t, err)
	ic := interceptor.NewClientInfoInterceptor(trusted)

	ctx := incomingWithPeer(metadata.Pairs(
		"x-forwarded-for", "203.0.113.1, 10.0.0.1",
	), "127.0.0.1")

	captured := runClientInfo(t, ic, ctx)
	require.Equal(t, "203.0.113.1", captured.IP)
}

func TestClientInfoInterceptor_TrustedPeerWithoutXFFFallsBackToRealIP(t *testing.T) {
	t.Parallel()

	trusted, err := interceptor.ParseTrustedProxies([]string{"127.0.0.0/8"})
	require.NoError(t, err)
	ic := interceptor.NewClientInfoInterceptor(trusted)

	ctx := incomingWithPeer(metadata.Pairs(
		"x-real-ip", "198.51.100.7",
	), "127.0.0.1")

	captured := runClientInfo(t, ic, ctx)
	require.Equal(t, "198.51.100.7", captured.IP)
}

func TestClientInfoInterceptor_EmptyTrustedListNeverTrusts(t *testing.T) {
	t.Parallel()

	ic := interceptor.NewClientInfoInterceptor(nil)
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
