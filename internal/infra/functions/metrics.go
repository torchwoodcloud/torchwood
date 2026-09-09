package functions

import (
	"github.com/prometheus/client_golang/prometheus"
)

// egressClassTotal 是函数执行容器的 egress 网络分类计数（P2 安全切片，
// 设计 Security #6 / Observability）：trusted = 常规网络（出网放开）、
// untrusted = internal 变体网络（出网全 deny）。v1 执行器与 v2 dispatcher
// 的实例创建路径都打点——不可信面的暴露范围可观测（应与 client_callable /
// 有触发器函数的执行量吻合）。
var egressClassTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "torchwood_functions_egress_class_total",
	Help: "Function container creations by egress network class (trusted/untrusted).",
}, []string{"project", "class"})

func init() {
	prometheus.MustRegister(egressClassTotal)
}

// observeEgressClass 记录一次容器创建的 egress 分类（best-effort）。
func observeEgressClass(projectID string, untrusted bool) {
	ObserveEgressClass(projectID, untrusted)
}

// ObserveEgressClass 是导出版（functions-dispatcher 的 v2 spawn 路径共用
// 同一指标；best-effort，不影响主链路）。
func ObserveEgressClass(projectID string, untrusted bool) {
	if projectID == "" {
		return
	}
	class := "trusted"
	if untrusted {
		class = "untrusted"
	}
	egressClassTotal.WithLabelValues(projectID, class).Inc()
}
