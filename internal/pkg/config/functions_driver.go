package config

import (
	"fmt"
	"strings"
)

// 函数执行底座驱动的封闭值集（IMPL-T2-5 双执行底座）：functions.driver
// 的合法取值。仅 dispatcher 进程消费；未设置/未知值由 bootkit 校验层
// fail-closed（启动期拒绝，列出两选项与各自配置键）。string 而非 proto
// enum：config 绑定是结构体驱动逐叶解码（bind.go），enum 的 int32 承载
// 解不了 YAML 字符串；先例 storage.provider / idgen.default_strategy。
const (
	// FunctionsDriverDocker 是 docker 直接执行底座：dispatcher 持
	// docker.sock，函数实例 = 常驻容器（per-project bridge 网络），构建 =
	// 本地 docker build。
	FunctionsDriverDocker = "docker"
	// FunctionsDriverFleetly 是 fleetly 平台执行底座：函数实例 = fleetly
	// Tasks（swarm service 承载），构建 = fleetly build-from-upload。
	FunctionsDriverFleetly = "fleetly"
)

// FleetlyEndpointMode 是 functions.fleetly.endpoint 的传输安全模式（endpoint
// scheme 显式选择——明文与 TLS 双形态都要支持：既有 fleetly 部署的栈内明文
// 通路保持兼容（T-3 双底座并存纪律），TLS 化控制面（platform 模式常开）经
// tls scheme 直连，不再依赖外置明文桥）。
type FleetlyEndpointMode int

const (
	// FleetlyEndpointPlain 是明文 h2c 拨号（裸 host:port，既有形态缺省）。
	FleetlyEndpointPlain FleetlyEndpointMode = iota
	// FleetlyEndpointTLS 是 TLS 拨号（tls:// 前缀）：系统根 CA 校验，
	// ServerName = 所拨主机名（按主机名拨号时对 LE 等公共证书透明验证）。
	FleetlyEndpointTLS
	// FleetlyEndpointTLSInsecure 是 TLS 拨号（tls-insecure:// 前缀）：跳过
	// 服务器证书校验（按 IP 直连等无 SAN 形态；staging/内网口径）。
	FleetlyEndpointTLSInsecure
)

// ParseFleetlyEndpoint 解析 functions.fleetly.endpoint 的传输模式与拨号地址：
// 裸 `host:port` = 明文（既有部署缺省不变）；`tls://host:port` = TLS + 系统
// CA 校验；`tls-insecure://host:port` = TLS + 跳过校验。唯一真源：bootkit
// 启动期校验与 dispatcher 客户端构造都消费本函数——未知 scheme 在启动期
// fail-closed，不留到首次拨号才暴露。空 endpoint 非法（校验层的必填哨兵
// 之外的双保险）。
func ParseFleetlyEndpoint(endpoint string) (string, FleetlyEndpointMode, error) {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return "", FleetlyEndpointPlain, fmt.Errorf("functions.fleetly.endpoint is empty")
	}
	switch {
	case strings.HasPrefix(e, "tls-insecure://"):
		addr := strings.TrimPrefix(e, "tls-insecure://")
		if addr == "" {
			return "", FleetlyEndpointTLSInsecure, fmt.Errorf("functions.fleetly.endpoint %q has no host:port after the tls-insecure:// scheme", endpoint)
		}
		return addr, FleetlyEndpointTLSInsecure, nil
	case strings.HasPrefix(e, "tls://"):
		addr := strings.TrimPrefix(e, "tls://")
		if addr == "" {
			return "", FleetlyEndpointTLS, fmt.Errorf("functions.fleetly.endpoint %q has no host:port after the tls:// scheme", endpoint)
		}
		return addr, FleetlyEndpointTLS, nil
	case strings.Contains(e, "://"):
		return "", FleetlyEndpointPlain, fmt.Errorf("functions.fleetly.endpoint %q uses an unknown scheme: supported forms are host:port (plaintext), tls://host:port (TLS, system CA verification) and tls-insecure://host:port (TLS, skip verification)", endpoint)
	default:
		return e, FleetlyEndpointPlain, nil
	}
}
