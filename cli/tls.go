package cli

import (
	"fmt"
	"net"
	"strings"
)

// insecureRemoteWarning 检测「非 loopback 远端 + 未启用 TLS」的风险组合：
// API Key 头将以明文跨越网络。命中时返回一行 stderr 告警文案（带换行），
// loopback 地址或已启用 --tls 返回 ""（不告警）。仅提示不阻断——明文
// gRPC 在受信任内网是合法部署形态。
func insecureRemoteWarning(endpoint string, tls bool) string {
	if tls || endpoint == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		// 无端口形态（如裸主机名）按整体视作 host；含 scheme 前缀的
		// target 同样落在这里，只做启发式判断。
		host = endpoint
	}
	if isLoopbackHost(host) {
		return ""
	}
	return fmt.Sprintf("warning: endpoint %q is not a loopback address and TLS is disabled; the API key travels in plaintext (set --tls when connecting through a TLS-terminating proxy)\n", endpoint)
}

// isLoopbackHost 判定 host 是否本机回环：localhost（含 *.localhost）或
// 回环 IP（127.0.0.0/8、::1）。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
