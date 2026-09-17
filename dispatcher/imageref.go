package dispatcher

// 本文件实现镜像源 registry host 的名称级校验（三期阶段三，设计
// docs/design/functions-runtimes-and-sources.md §3 安全基线的可实现口径）。
//
// 诚实口径（对抗审查 A2 的可实现版）：pull 的网络发起方是宿主 docker
// daemon，IP 拨号点 guard 不可实施于 daemon——因此采用「名称级校验 +
// 白名单 + 信任级论证」：部署者 = functions.write 特权主体（与 zip 上传
// 同级）。残余面（公网域名解析到内网、错误回显端口扫描侧信道）诚实声明、
// 随 egress 原语后置；本层消灭的是「引用直接写 IP/localhost 打内网」的
// 最廉价攻击形态，白名单给出内网 registry 的显式放行通道。

import (
	"net"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// defaultRegistryHost 是无显式 host 引用（"nginx"、"nginx:latest"、
// "nginx@sha256:..."、"library/nginx"）归属的默认 registry。
const defaultRegistryHost = "docker.io"

// parseImageReferenceHost 解析镜像引用的 registry host 段（Docker reference
// 语法）：host 只可能出现在首个 "/" 之前，且仅当该段含 "." 或 ":" 或为
// 字面量 "localhost"（否则首段是仓库路径组件，如 "library/nginx" 仍归属
// docker.io）；无 "/" 的引用无显式 host → docker.io。返回值统一小写，
// 含端口（"registry.example.com:5000"）与 IPv6 括号形态（"[::1]:5000"）。
func parseImageReferenceHost(reference string) string {
	i := strings.Index(reference, "/")
	if i < 0 {
		return defaultRegistryHost
	}
	first := reference[:i]
	if !strings.ContainsAny(first, ".:") && !strings.EqualFold(first, "localhost") {
		// 首段不含 "."/":" 且非 localhost = 仓库路径组件（"library/nginx"），
		// 归属 docker.io 默认 registry（与 docker reference 解析同口径）。
		return defaultRegistryHost
	}
	return strings.ToLower(first)
}

// stripPort 剥离 host 段的端口（"registry.example.com:5000" →
// "registry.example.com"；IPv6 括号形态 "[::1]:5000" → "::1"），返回小写。
func stripPort(hostport string) string {
	h := strings.ToLower(hostport)
	if strings.HasPrefix(h, "[") { // IPv6 括号形态
		if i := strings.Index(h, "]"); i >= 0 {
			return h[1:i]
		}
		return h
	}
	if i := strings.LastIndex(h, ":"); i >= 0 {
		return h[:i]
	}
	return h
}

// isIPLiteralHost 判定 host（可带端口）是否 IP 字面量：IPv4 点分
// （"192.168.1.5"、"192.168.1.5:5000"）与 IPv6（裸地址 "fd00::1"、括号形态
// "[::1]:5000"）。裸 IPv6 整体优先尝试（LastIndex 剥端口会截断多冒号地址）。
func isIPLiteralHost(host string) bool {
	h := strings.ToLower(host)
	if net.ParseIP(h) != nil {
		return true
	}
	return net.ParseIP(stripPort(h)) != nil
}

// isLocalhostHost 判定 host 是否 localhost 形态（"localhost" 或
// "*.localhost"；端口与 IPv6 括号先剥离）。
func isLocalhostHost(host string) bool {
	h := stripPort(host)
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// validateImageRegistryHost 按 config functions.image 的准入规则校验 host，
// 失败返回 InvalidArgument 且明示命中规则与放行通道（设计 §3）：
//  1. 白名单优先放行：allowed_registries 非空且精确/后缀命中 = 运维显式
//     登记（含内网 registry 直连 IP 的显式白名单场景）；
//  2. allow_insecure = 全放行（自托管内网 registry 的显式开关）；
//  3. 拒绝 IP 字面量与 localhost/*.localhost；
//  4. 白名单非空且未命中 → 拒绝（正向白名单语义）。
func validateImageRegistryHost(host string, allowedRegistries []string, allowInsecure bool) error {
	if hostAllowedByWhitelist(host, allowedRegistries) {
		return nil
	}
	if allowInsecure {
		return nil
	}
	if isIPLiteralHost(host) {
		return status.Errorf(codes.InvalidArgument,
			"image reference host %q is an IP literal: refused; allowlist it in functions.image.allowed_registries or set functions.image.allow_insecure for self-hosted registries",
			host)
	}
	if isLocalhostHost(host) {
		return status.Errorf(codes.InvalidArgument,
			"image reference host %q is a localhost address: refused; allowlist it in functions.image.allowed_registries or set functions.image.allow_insecure for self-hosted registries",
			host)
	}
	if len(allowedRegistries) > 0 {
		return status.Errorf(codes.InvalidArgument,
			"image reference host %q is not in functions.image.allowed_registries", host)
	}
	return nil
}

// hostAllowedByWhitelist 白名单判定：含端口 host 整体精确匹配，或剥离端口
// 后的域与条目精确/子域后缀匹配（条目 "example.com" 命中 "example.com" 与
// "registry.example.com:5000"，不命中 "badexample.com"）。白名单为空恒 false
// （空 = 不设白名单，不在此放行）。
func hostAllowedByWhitelist(host string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	bare := stripPort(host)
	for _, entry := range allowed {
		e := strings.ToLower(strings.TrimSpace(entry))
		if e == "" {
			continue
		}
		if host == e { // 精确：含端口整体匹配
			return true
		}
		if bare == e || strings.HasSuffix(bare, "."+e) { // 域精确/子域后缀
			return true
		}
	}
	return false
}
