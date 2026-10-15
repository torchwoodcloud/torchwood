// Package ssrfguard 提供拨号点 SSRF 防护的出站 HTTP 客户端。
//
// 原型抽自 packer/gitpack.go（Functions v4 设计 docs/design/functions-v4.md §5：
// 统一出站投递引擎——函数 endpoint 调用与 webhook 投递共享本防护）。核心设计：
// 校验实施在 **connect 系统调用时拿到的真实 IP** 上（dialer Control 钩子），
// 而非请求前 DNS 预解析——解析与连接之间换 DNS 记录的 rebinding 攻击因此失效
//（多 A 记录每连接触发一次校验；TLS 叠在受控拨号之上，不设 DialTLSContext；
// 重定向后的拨号同样经 Control，天然覆盖）。
//
// 信任边界：AllowInsecure 仅供自托管内网目标（packer 的内网 git 服务）使用；
// 用户配置的任意 URL（函数 endpoint、webhook 接收方）必须恒 false——此类目标
// 比内网 git 更不可信，不提供放行开关。
package ssrfguard

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// Options 是守卫客户端的构造参数。
type Options struct {
	// AllowInsecure 整体放行（自托管内网目标）。见包注释的信任边界。
	AllowInsecure bool
	// Timeout 是客户端级单请求总超时；零值 = 不设客户端超时（调用方以 ctx
	// 控制预算——投递引擎按业务超时裁剪）。
	Timeout time.Duration
}

// NewClient 返回带拨号点校验的 *http.Client：基于 DefaultTransport 克隆
//（保留代理/TLS 缺省），仅替换 DialContext 为带 Control 钩子的 dialer。
// 代理场景下 Control 看到的是代理地址（拨号终点即代理，代理本身必须可信，
// 与出网代理的既有信任模型一致）。
func NewClient(opts Options) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return CheckDialAddress(address, opts.AllowInsecure)
		},
	}
	tr.DialContext = dialer.DialContext
	return &http.Client{Transport: tr, Timeout: opts.Timeout}
}

// CheckDialAddress 是拨号点校验本体：默认拒绝 loopback / private /
// link-local / 未指定地址（169.254.169.254 云元数据端点等），出现主机名
// 说明走错钩子、fail-closed。allowInsecure 时整体放行。
func CheckDialAddress(address string, allowInsecure bool) error {
	if allowInsecure {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("ssrf guard: non-ip dial address %q", host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("ssrf guard: dial to %q is not allowed (loopback/private/link-local)", host)
	}
	return nil
}
