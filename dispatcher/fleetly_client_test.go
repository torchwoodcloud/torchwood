package dispatcher

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewGRPCFleetlyClientTransportModes 客户端构造按 endpoint scheme 选择
// 传输安全（明文/TLS/TLS-insecure 双形态都要支持）：三种合法形态构造成功
// （grpc.NewClient 是惰性连接，构造期不发流量）；未知 scheme fail-closed 且
// 错误点名原 endpoint。
func TestNewGRPCFleetlyClientTransportModes(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		"fleetlyd:8421",          // 明文（既有部署缺省）
		"tls://ctrl.dev:8421",    // TLS + 系统 CA
		"tls-insecure://10.124.0.3:8421", // TLS + 跳过校验（按 IP 无 SAN）
	} {
		endpoint := endpoint
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			cli, err := newGRPCFleetlyClient(endpoint, "tok")
			require.NoError(t, err)
			t.Cleanup(func() { _ = cli.Close() })
			require.NotNil(t, cli.tasks)
			require.NotNil(t, cli.builds)
		})
	}

	t.Run("unknown scheme rejected", func(t *testing.T) {
		t.Parallel()
		_, err := newGRPCFleetlyClient("tlsx://fleetlyd:8421", "tok")
		require.ErrorContains(t, err, "unknown scheme")
		require.ErrorContains(t, err, "tlsx://fleetlyd:8421")
	})

	t.Run("empty endpoint rejected", func(t *testing.T) {
		t.Parallel()
		_, err := newGRPCFleetlyClient("", "tok")
		require.ErrorContains(t, err, "endpoint is empty")
	})
}

// TestFleetlyBearerCredentialsRequireTransportSecurity PerRPCCredentials 的
// 传输安全要求跟随 endpoint 模式：明文 = false（栈内同网络通路），TLS 形态
// = true（gRPC 传输层强制凭据只走安全连接）。
func TestFleetlyBearerCredentialsRequireTransportSecurity(t *testing.T) {
	t.Parallel()
	require.False(t, fleetlyBearerCredentials{token: "tok"}.RequireTransportSecurity())
	require.False(t, fleetlyBearerCredentials{token: "tok", secure: false}.RequireTransportSecurity())
	require.True(t, fleetlyBearerCredentials{token: "tok", secure: true}.RequireTransportSecurity())
}
