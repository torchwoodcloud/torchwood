package functions

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
)

// ——观测补全（P0.5 子步骤 4；包内自注册，projectschema/dispatcher 同模式）——
//
// 执行时长/排队时长直方图（设计 Observability：补齐现状无 functions 专属
// Prometheus 指标的缺口；source 维度 server 暂时单值，client/http/cron 留
// 枚举位随 P1/P2 打开）。冷启动与池水位指标在 functions-dispatcher 包内。
var (
	executionDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "torchwood_functions_execution_duration_seconds",
		Help:    "Function execution duration (executor leg), by trigger source and outcome.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 16), // 5ms .. ~164s
	}, []string{"project", "function", "source", "status"})

	executionQueueWaitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "torchwood_functions_queue_wait_seconds",
		Help:    "Async queue wait (queued->running), by project/function.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 18),
	}, []string{"project", "function"})

	executionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_functions_executions_total",
		Help: "Function executions by trigger source and final status.",
	}, []string{"project", "function", "source", "status"})

	// invokeTotal 是调用面入口计数（P1 触发器模块，设计 Observability）：
	// 触发器 handler / cron dispatch 的每次调用尝试按 result 记账——
	// ok（sync 完成 / 异步已入队）、quota（per-IP 限频 429）、echo（GET 握手
	// 回显，探测行为可观测）、not_found（token 未命中 404）、error、timeout。
	invokeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_functions_invoke_total",
		Help: "Function invoke attempts at trigger/client entrypoints, by source and result.",
	}, []string{"project", "function", "source", "result"})
)

func init() {
	prometheus.MustRegister(executionDurationSeconds, executionQueueWaitSeconds, executionsTotal, invokeTotal)
}

// source 词表：server 当前唯一取值（CreateExecution 内部路径）；http/cron
// 随 P1 触发器打开（trigger_source 列前缀），client 为 P2 客户端调用面
// （InvokeFunction；trigger_source 列恒等 "client"）。
const (
	executionSourceServer = "server"
	executionSourceHTTP   = "http"
	executionSourceCron   = "cron"
	executionSourceClient = "client"
)

// invoke result 词表（torchwood_functions_invoke_total 的 result 维度；
// 导出供 serverhttp 触发器 handler 共用）。
const (
	InvokeResultOK       = "ok"
	InvokeResultQuota    = "quota"
	InvokeResultEcho     = "echo"
	InvokeResultNotFound = "not_found"
	InvokeResultError    = "error"
	InvokeResultTimeout  = "timeout"
)

// metricSource 把 trigger_source 列值（http:{id} / cron:{id} / 空）折叠为
// 指标 source 枚举（http / cron / server）。
func metricSource(triggerSource string) string {
	if triggerSource == "" {
		return executionSourceServer
	}
	if idx := indexByteStr(triggerSource, ':'); idx > 0 {
		return triggerSource[:idx]
	}
	return triggerSource
}

func indexByteStr(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// ObserveInvoke 记录一次触发器/客户端入口调用（导出供 serverhttp handler
// 使用；best-effort，不影响主链路）。
func ObserveInvoke(projectID, functionID, source, result string) {
	if projectID == "" || functionID == "" {
		return
	}
	invokeTotal.WithLabelValues(projectID, functionID, source, result).Inc()
}

// observeExecution 记录一次执行的总时长与终态（best-effort，不影响主链路）。
func (f *Functions) observeExecution(projectID, functionID, source string, started time.Time, status string) {
	if projectID == "" || functionID == "" {
		return
	}
	if status != domainfunctions.ExecutionStatusCompleted && status != domainfunctions.ExecutionStatusFailed {
		return
	}
	executionDurationSeconds.WithLabelValues(projectID, functionID, source, status).
		Observe(time.Since(started).Seconds())
	executionsTotal.WithLabelValues(projectID, functionID, source, status).Inc()
}

// observeQueueWait 记录异步排队时长（queued→running）。
func observeQueueWait(projectID, functionID string, waited time.Duration) {
	if projectID == "" || functionID == "" || waited < 0 {
		return
	}
	executionQueueWaitSeconds.WithLabelValues(projectID, functionID).Observe(waited.Seconds())
}

// executorV2 报告是否选择 dispatcher 执行器（v2 常驻执行模型）：run 信号量
// 对 v2 执行跳过（池由 dispatcher 内部管控），v1 回退模式保留信号量。
func (f *Functions) executorV2() bool {
	return f.cfg.GetFunctions().GetExecutor() == "dispatcher"
}

// PruneOldExecutions 是执行记录保留分级的全项目周期入口（P0.5 起 worker
// 低频 ticker 驱动；P2 Q7 保留分级拆两条删除路径）：
//   - server 面来源（trigger_source = ”）：条数式，每函数保留最近
//     pruneKeepRecent（100）条；
//   - client/http/cron 来源：时间窗保留 pruneTriggerRetention（48h ≥ 2× 最长
//     限频窗口 day）——这些行是限频 DB 降级路径的窗口内计数依据，条数式
//     裁剪会在高量日造成少计超发（设计 §5 交互修正）。
func (f *Functions) PruneOldExecutions(ctx context.Context) (int, error) {
	if f.projects == nil {
		return 0, nil
	}
	all, err := f.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	pruned := 0
	olderThan := time.Now().Add(-pruneTriggerRetention)
	for i := range all {
		if all[i].Status != "active" {
			continue
		}
		fns, err := f.repo.ListFunctions(ctx, all[i].ID)
		if err != nil {
			continue
		}
		for j := range fns {
			ok := f.repo.PruneOldExecutionsInProject(ctx, all[i].ID, fns[j].ID, pruneKeepRecent) == nil
			// 两条删除路径互不误伤（server 条数式 / trigger 时间窗），任一
			// 失败不影响另一条。
			okTime := f.repo.PruneTriggerExecutionsInProject(ctx, all[i].ID, fns[j].ID, olderThan) == nil
			if ok && okTime {
				pruned++
			}
		}
	}
	return pruned, nil
}
