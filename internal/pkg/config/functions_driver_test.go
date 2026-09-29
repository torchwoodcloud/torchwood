package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseFleetlyEndpoint 传输模式解析（gRPC 社区约定 scheme）：
// grpc:// 与裸 host:port = 明文；grpcs:// = TLS + 系统 CA（?server_name= 覆盖
// SNI）；grpcs://?insecure=true = TLS 跳过校验；空/未知 scheme fail-closed。
func TestParseFleetlyEndpoint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		endpoint   string
		wantAddr   string
		wantMode   FleetlyEndpointMode
		wantServer string
		wantErr    string
	}{
		{name: "裸 host:port = 明文", endpoint: "fleetlyd:8421", wantAddr: "fleetlyd:8421", wantMode: FleetlyEndpointPlain},
		{name: "grpc:// 显式明文", endpoint: "grpc://fleetlyd:8421", wantAddr: "fleetlyd:8421", wantMode: FleetlyEndpointPlain},
		{name: "裸地址两侧空白裁剪", endpoint: "  fleetlyd:8421  ", wantAddr: "fleetlyd:8421", wantMode: FleetlyEndpointPlain},
		{name: "grpcs:// TLS 校验", endpoint: "grpcs://ctrl.dev.fleetly.run:8421", wantAddr: "ctrl.dev.fleetly.run:8421", wantMode: FleetlyEndpointTLS},
		{name: "grpcs://?insecure=true 跳过校验", endpoint: "grpcs://10.124.0.3:8421?insecure=true", wantAddr: "10.124.0.3:8421", wantMode: FleetlyEndpointTLSInsecure},
		{name: "grpcs://?insecure=1 跳过校验", endpoint: "grpcs://10.124.0.3:8421?insecure=1", wantAddr: "10.124.0.3:8421", wantMode: FleetlyEndpointTLSInsecure},
		{name: "grpcs://?server_name= 覆盖 SNI", endpoint: "grpcs://10.124.0.3:8421?server_name=ctrl.dev.fleetly.run", wantAddr: "10.124.0.3:8421", wantMode: FleetlyEndpointTLS, wantServer: "ctrl.dev.fleetly.run"},
		{name: "grpcs:// 组合 query", endpoint: "grpcs://10.0.0.5:8421?insecure=true&server_name=ctrl.internal", wantAddr: "10.0.0.5:8421", wantMode: FleetlyEndpointTLSInsecure, wantServer: "ctrl.internal"},
		{name: "空拒绝", endpoint: "", wantErr: "endpoint is empty"},
		{name: "空白拒绝", endpoint: "   ", wantErr: "endpoint is empty"},
		{name: "grpcs 空地址拒绝", endpoint: "grpcs://", wantErr: "no host:port after the grpcs:// scheme"},
		{name: "grpc 空地址拒绝", endpoint: "grpc://", wantErr: "no host:port after the grpc:// scheme"},
		{name: "insecure 非法值拒绝", endpoint: "grpcs://h:1?insecure=yes", wantErr: "invalid grpcs query parameter insecure"},
		{name: "未知 scheme fail-closed", endpoint: "tlsx://fleetlyd:8421", wantErr: "unknown scheme"},
		{name: "http scheme fail-closed", endpoint: "http://fleetlyd:8421", wantErr: "unknown scheme"},
		{name: "旧 tls scheme 现已 fail-closed", endpoint: "tls://ctrl.dev:8421", wantErr: "unknown scheme"},
		{name: "旧 tls-insecure scheme 现已 fail-closed", endpoint: "tls-insecure://10.0.0.1:8421", wantErr: "unknown scheme"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseFleetlyEndpoint(tc.endpoint)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantAddr, got.Addr)
			require.Equal(t, tc.wantMode, got.Mode)
			require.Equal(t, tc.wantServer, got.ServerName)
		})
	}
}
