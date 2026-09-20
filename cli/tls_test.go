package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/lynx-go/commands"
	"github.com/stretchr/testify/require"
)

// TestInsecureRemoteWarning 四象限（loopback/非 loopback × TLS 开关）：
// 仅「非 loopback + 未启用 TLS」告警，其余一律静默。
func TestInsecureRemoteWarning(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		tls      bool
		wantWarn bool
	}{
		{"loopback 明文不告警（默认地址）", "127.0.0.1:9060", false, false},
		{"localhost 明文不告警", "localhost:9060", false, false},
		{"IPv6 回环明文不告警", "[::1]:9060", false, false},
		{"loopback 开 TLS 不告警", "127.0.0.1:9060", true, false},
		{"远端开 TLS 不告警", "api.example.com:443", true, false},
		{"远端明文告警", "api.example.com:443", false, true},
		{"公网 IPv4 明文告警", "203.0.113.7:9060", false, true},
		{"裸主机名明文告警", "grpc.internal.example.net", false, true},
		{"空 endpoint 不告警", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := insecureRemoteWarning(tc.endpoint, tc.tls)
			if !tc.wantWarn {
				require.Empty(t, msg, "不应告警")
				return
			}
			require.NotEmpty(t, msg)
			require.True(t, strings.HasPrefix(msg, "warning:"), "告警须以 warning: 开头")
			require.Contains(t, msg, "--tls", "告警须给出 --tls 处置建议")
			require.True(t, strings.HasSuffix(msg, "\n"), "告警以独立行落到 stderr")
		})
	}
}

// TestVerbRunWarnsInsecureRemote：告警经 verb.Run 落到 env.Stderr（不走 slog）。
func TestVerbRunWarnsInsecureRemote(t *testing.T) {
	isolateConfig(t)
	g := &GlobalFlags{
		endpoint: "api.example.com:443",
		timeout:  "30s",
		output:   "json",
		apiKey:   "dummy",
	}
	v := newRPCCmd(g)
	var stderr strings.Builder
	env := &commands.Environment{Stdout: &strings.Builder{}, Stderr: &stderr}
	// 传两个位置参数触发 UsageError：告警在 Run 前段（validate 之后）落
	// stderr，RPC 本身不发生——用例快速且无网络依赖。
	_ = v.Run(context.Background(), env, []string{"a", "b"})
	require.Contains(t, stderr.String(), "warning: endpoint")
	require.Contains(t, stderr.String(), "not a loopback address")
}
