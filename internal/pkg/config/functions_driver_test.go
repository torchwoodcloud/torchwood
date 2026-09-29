package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseFleetlyEndpoint 传输模式解析（endpoint scheme 显式选择）：
// 裸 host:port = 明文（既有部署缺省不变）；tls:// = TLS + 系统 CA；
// tls-insecure:// = TLS 跳过校验；空/未知 scheme fail-closed。
func TestParseFleetlyEndpoint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		endpoint string
		wantAddr string
		wantMode FleetlyEndpointMode
		wantErr  string
	}{
		{name: "裸 host:port = 明文", endpoint: "fleetlyd:8421", wantAddr: "fleetlyd:8421", wantMode: FleetlyEndpointPlain},
		{name: "裸地址两侧空白裁剪", endpoint: "  fleetlyd:8421  ", wantAddr: "fleetlyd:8421", wantMode: FleetlyEndpointPlain},
		{name: "tls scheme", endpoint: "tls://ctrl.dev.fleetly.run:8421", wantAddr: "ctrl.dev.fleetly.run:8421", wantMode: FleetlyEndpointTLS},
		{name: "tls-insecure scheme", endpoint: "tls-insecure://10.124.0.3:8421", wantAddr: "10.124.0.3:8421", wantMode: FleetlyEndpointTLSInsecure},
		{name: "空拒绝", endpoint: "", wantErr: "endpoint is empty"},
		{name: "空白拒绝", endpoint: "   ", wantErr: "endpoint is empty"},
		{name: "tls 空地址拒绝", endpoint: "tls://", wantErr: "no host:port after the tls:// scheme"},
		{name: "tls-insecure 空地址拒绝", endpoint: "tls-insecure://", wantErr: "no host:port after the tls-insecure:// scheme"},
		{name: "未知 scheme fail-closed", endpoint: "tlsx://fleetlyd:8421", wantErr: "unknown scheme"},
		{name: "http scheme fail-closed", endpoint: "http://fleetlyd:8421", wantErr: "unknown scheme"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			addr, mode, err := ParseFleetlyEndpoint(tc.endpoint)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantAddr, addr)
			require.Equal(t, tc.wantMode, mode)
		})
	}
}
