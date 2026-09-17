package dispatcher

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖镜像源 registry host 名称级校验（三期阶段三，设计 §3 安全基线
// 的可实现口径）：引用 host 段解析表驱动 + 准入规则表驱动（IP 字面量/
// localhost 拒绝、allow_insecure 放行、allowed_registries 白名单精确/后缀
// 匹配与未命中拒绝）。

func TestParseImageReferenceHost_Table(t *testing.T) {
	cases := []struct {
		name      string
		reference string
		wantHost  string
	}{
		{"no-host-tag-defaults-docker-io", "nginx:1.25", "docker.io"},
		{"no-host-digest-defaults-docker-io", "nginx@sha256:" + digest64("a"), "docker.io"},
		{"bare-repo-defaults-docker-io", "ubuntu", "docker.io"},
		{"library-path-defaults-docker-io", "library/nginx", "docker.io"},
		{"explicit-docker-io", "docker.io/library/nginx:1.25", "docker.io"},
		{"explicit-domain", "ghcr.io/acme/greet:v1", "ghcr.io"},
		{"domain-with-port", "registry.example.com:5000/acme/app@sha256:" + digest64("b"), "registry.example.com:5000"},
		{"single-label-with-port", "myregistry:5000/acme/app:v1", "myregistry:5000"},
		{"localhost", "localhost/acme/app:v1", "localhost"},
		{"localhost-with-port", "localhost:5000/acme/app:v1", "localhost:5000"},
		{"sub-localhost", "registry.localhost/acme/app:v1", "registry.localhost"},
		{"ipv4-literal", "192.168.1.5:5000/acme/app:v1", "192.168.1.5:5000"},
		{"ipv6-bracket-with-port", "[::1]:5000/acme/app:v1", "[::1]:5000"},
		{"ipv6-bracket-no-port", "[fd00::1]/acme/app:v1", "[fd00::1]"},
		{"host-uppercased", "GHCR.IO/acme/greet:v1", "ghcr.io"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantHost, parseImageReferenceHost(tc.reference))
		})
	}
}

func TestValidateImageRegistryHost_Table(t *testing.T) {
	// digest64 不在本文件定义域内时补一条本地实现（与 daemon_import_test
	// 共享 digest64 helper——见该文件）。
	cases := []struct {
		name       string
		host       string
		allowed    []string
		insecure   bool
		wantCode   codes.Code
		wantErrHas string // 空 = 期望放行
	}{
		// ——默认（无白名单、无 allow_insecure）：拒 IP/localhost，放正常域名——
		{name: "normal-domain-allowed", host: "ghcr.io", wantCode: codes.OK},
		{name: "docker-io-allowed", host: "docker.io", wantCode: codes.OK},
		{name: "domain-with-port-allowed", host: "registry.example.com:5000", wantCode: codes.OK},
		{name: "ipv4-literal-refused", host: "192.168.1.5:5000", wantCode: codes.InvalidArgument, wantErrHas: "IP literal"},
		{name: "ipv4-no-port-refused", host: "10.0.0.1", wantCode: codes.InvalidArgument, wantErrHas: "IP literal"},
		{name: "ipv6-bracket-refused", host: "[::1]:5000", wantCode: codes.InvalidArgument, wantErrHas: "IP literal"},
		{name: "ipv6-bare-refused", host: "fd00::1", wantCode: codes.InvalidArgument, wantErrHas: "IP literal"},
		{name: "localhost-refused", host: "localhost:5000", wantCode: codes.InvalidArgument, wantErrHas: "localhost address"},
		{name: "sub-localhost-refused", host: "registry.localhost", wantCode: codes.InvalidArgument, wantErrHas: "localhost address"},
		// ——allow_insecure = 全放行（含 IP/localhost）——
		{name: "insecure-allows-ipv4", host: "192.168.1.5:5000", insecure: true, wantCode: codes.OK},
		{name: "insecure-allows-localhost", host: "localhost:5000", insecure: true, wantCode: codes.OK},
		{name: "insecure-allows-domain", host: "registry.internal.example.com", insecure: true, wantCode: codes.OK},
		// ——白名单：精确/后缀命中优先放行（含 IP/localhost 显式登记）——
		{name: "whitelist-exact", host: "ghcr.io", allowed: []string{"ghcr.io"}, wantCode: codes.OK},
		{name: "whitelist-suffix-subdomain", host: "registry.example.com", allowed: []string{"example.com"}, wantCode: codes.OK},
		{name: "whitelist-suffix-with-port", host: "registry.example.com:5000", allowed: []string{"example.com"}, wantCode: codes.OK},
		{name: "whitelist-exact-with-port", host: "myregistry:5000", allowed: []string{"myregistry:5000"}, wantCode: codes.OK},
		{name: "whitelist-ip-explicit", host: "192.168.1.5:5000", allowed: []string{"192.168.1.5:5000"}, wantCode: codes.OK},
		{name: "whitelist-localhost-explicit", host: "localhost:5000", allowed: []string{"localhost:5000"}, wantCode: codes.OK},
		// ——白名单未命中：拒绝（正向白名单语义）——
		{name: "whitelist-miss-refused", host: "docker.io", allowed: []string{"ghcr.io"}, wantCode: codes.InvalidArgument, wantErrHas: "not in functions.image.allowed_registries"},
		{name: "whitelist-prefix-no-suffix-match", host: "badexample.com", allowed: []string{"example.com"}, wantCode: codes.InvalidArgument, wantErrHas: "not in functions.image.allowed_registries"},
		{name: "whitelist-wrong-port-refused", host: "registry.example.com:5001", allowed: []string{"registry.example.com:5000"}, wantCode: codes.InvalidArgument, wantErrHas: "not in functions.image.allowed_registries"},
		// 白名单未命中的 IP host：IP 字面量规则先命中（消息明示两条放行通道）。
		{name: "whitelist-miss-ip-still-refused", host: "10.9.9.9", allowed: []string{"ghcr.io"}, wantCode: codes.InvalidArgument, wantErrHas: "IP literal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageRegistryHost(tc.host, tc.allowed, tc.insecure)
			if tc.wantErrHas == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, tc.wantCode, status.Code(err))
			require.Contains(t, errorMessage(err), tc.wantErrHas)
		})
	}
}

// TestValidateImageRegistryHost_WhitespaceEntriesTolerated 白名单空白条目
// 容忍（配置手误不放大攻击面——空白条目不命中任何 host）。
func TestValidateImageRegistryHost_WhitespaceEntriesTolerated(t *testing.T) {
	require.NoError(t, validateImageRegistryHost("ghcr.io", []string{"", "  ", "ghcr.io"}, false))
	require.Error(t, validateImageRegistryHost("docker.io", []string{"", "  "}, false))
}

// TestValidateImageRegistryHost_ErrorDisclosesHost 校验失败错误必须明示命中
// 规则与放行通道（设计 §3「校验失败 InvalidArgument 明示命中规则」）。
func TestValidateImageRegistryHost_ErrorDisclosesHost(t *testing.T) {
	err := validateImageRegistryHost("192.168.1.5:5000", nil, false)
	require.ErrorContains(t, err, "192.168.1.5:5000")
	require.ErrorContains(t, err, "functions.image.allowed_registries")
	require.ErrorContains(t, err, "functions.image.allow_insecure")
}
