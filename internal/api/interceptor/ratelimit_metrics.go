package interceptor

import (
	"github.com/prometheus/client_golang/prometheus"
)

// 通用限流指标（前缀 torchwood_，注册到默认注册表，/metrics 由
// internal/infra/server 的 MetricsServer 暴露）。Round4 J5-1：把限流的
// 「正常拒绝」与「基础设施错误」分开计数——前者是业务面 429，后者意味着
// Redis 异常（结合熔断放行观测降级窗口）。
var (
	// RateLimitRejectedTotal 是因超过窗口阈值被拒绝的请求计数（ResourceExhausted）。
	RateLimitRejectedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torchwood_ratelimit_rejected_total",
		Help: "Requests rejected by the generic rate limiter (window exceeded).",
	})
	// RateLimitInfraErrorTotal 是 limiter 后端（Redis）检查失败的计数。
	// 非零即需关注：熔断器会据此进入短窗放行（fail-open），告警应联动。
	RateLimitInfraErrorTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torchwood_ratelimit_infra_error_total",
		Help: "Rate limiter backend errors (Redis unavailable); circuit breaker feed.",
	})
)

func init() {
	prometheus.MustRegister(RateLimitRejectedTotal, RateLimitInfraErrorTotal)
}
