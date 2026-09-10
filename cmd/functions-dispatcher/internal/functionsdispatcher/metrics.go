package functionsdispatcher

import (
	"github.com/prometheus/client_golang/prometheus"
)

// 执行器 v2 指标（包内自注册，projectschema 同模式；设计 §6 Observability）：
//   - 冷启动（池 0→1）计数 + 初始化时长直方图（对标 Lambda
//     initializationDuration）；
//   - 池水位（ready/booting/draining）gauge；
//   - function_resident_uptime_ms（按实例存活累计——保温成本显式计费口径）；
//   - 分发请求时长直方图 + 排队等待直方图 + 排队丢弃/超时计数。
var (
	ColdStartsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_functions_cold_starts_total",
		Help: "Resident instance cold starts (pool 0->1 scale-out) per function.",
	}, []string{"project", "function"})

	InitDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "torchwood_functions_init_duration_seconds",
		Help:    "Resident instance boot-to-ready duration (module load included).",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 14), // 10ms .. ~82s
	}, []string{"project", "function"})

	PoolReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "torchwood_functions_pool_ready",
		Help: "Idle resident instances ready to serve, per function.",
	}, []string{"project", "function"})

	PoolBooting = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "torchwood_functions_pool_booting",
		Help: "Resident instances currently booting, per function.",
	}, []string{"project", "function"})

	PoolDraining = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "torchwood_functions_pool_draining",
		Help: "Resident instances draining (max_requests reached or deployment superseded).",
	}, []string{"project", "function"})

	ResidentUptimeMS = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_function_resident_uptime_ms",
		Help: "Accumulated resident instance uptime in ms (billing footprint for warm pools).",
	}, []string{"project", "function"})

	DispatchDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "torchwood_functions_dispatch_duration_seconds",
		Help:    "Runner invocation duration (dispatch hot path execution leg).",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 14), // 5ms .. ~41s
	}, []string{"project", "function"})

	QueueWaitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "torchwood_functions_dispatch_queue_wait_seconds",
		Help:    "Time a dispatch request waits for a claimable instance (0 on instant claim).",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
	}, []string{"project", "function"})

	DispatchQueueDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_functions_dispatch_queue_dropped_total",
		Help: "Dispatch requests rejected immediately because the bounded queue was full.",
	}, []string{"project", "function"})

	DispatchQueueTimeouts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_functions_dispatch_queue_timeouts_total",
		Help: "Dispatch requests that exhausted the queue head timeout without an instance.",
	}, []string{"project", "function"})
)

func init() {
	prometheus.MustRegister(
		ColdStartsTotal,
		InitDurationSeconds,
		PoolReady,
		PoolBooting,
		PoolDraining,
		ResidentUptimeMS,
		DispatchDurationSeconds,
		QueueWaitSeconds,
		DispatchQueueDropped,
		DispatchQueueTimeouts,
	)
}
