package interceptor

import (
	"context"

	grpcapiinterceptor "github.com/lynx-go/grpcapi/interceptor"
)

// trusted_proxy 薄适配（grpcapi 阶段 1，DESIGN §7）：机制本体已上收
// github.com/lynx-go/grpcapi/interceptor，此处保留 torchwood 包内符号
// （类型别名 + 函数委托），供 serverhttp handlers、audit 拦截器与既有
// 测试零改动引用。
//
// 行为差异（有意改进，库版语义）：ResolveClientIP 的 XFF 解析从"直取首跳"
// 改为"自右向左逐跳回溯（第一个不可信地址即客户端）"——客户端伪造注入的
// 前缀跳在此被截断；peer 不可信/无可信配置时仍一律使用 peer 地址，不变。
type TrustedProxies = grpcapiinterceptor.TrustedProxies

// ParseTrustedProxies 解析 CIDR 列表（也接受裸 IP，按 /32、/128 处理）。
// 委托库实现；需要在启动期暴露配置错误的调用方（runtime 装配）应先经此
// 函数校验。
func ParseTrustedProxies(cidrs []string) (*TrustedProxies, error) {
	return grpcapiinterceptor.ParseTrustedProxies(cidrs)
}

// PeerIPFromAddr 从 "host:port" 形式的对端地址提取 IP 部分。
func PeerIPFromAddr(addr string) string {
	return grpcapiinterceptor.PeerIPFromAddr(addr)
}

// PeerIP 从 gRPC 请求上下文提取直连 peer 的 IP；无 peer 信息时返回空串。
func PeerIP(ctx context.Context) string {
	return grpcapiinterceptor.PeerIP(ctx)
}
