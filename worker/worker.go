package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/redis/go-redis/v9"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

// workerConcurrency 是单进程并发消费 goroutine 数（XREADGROUP 消费组多
// 消费者互斥由 Redis 保证，任务间无顺序依赖）。
const workerConcurrency = 4

// dequeuePollInterval 是 XREADGROUP Block 轮询超时（配合优雅退出，取消后 1s 内返回）。
const dequeuePollInterval = time.Second

// maxProcessAttempts 是消费瞬时失败的最大重试次数（超限后兜底标 failed）。
const maxProcessAttempts = 3

// orphanRecoverInterval 是孤儿恢复的周期（P0.5：从「启动跑一次」升级为
// 周期任务）。扫描范围 queued/building/running，staleAfter 按行内
// timeout_seconds + 120s 宽限判定（NULL 回退 1h）。
const orphanRecoverInterval = time.Minute

// orphanStaleFallback 是 timeout_seconds 为 NULL 的存量行的回退口径
// （v1 兼容；P0.5 起新行均带快照）。
const orphanStaleFallback = time.Hour

// pruneInterval 是执行记录保留策略的周期清理间隔（P0.5：同步热路径的
// Prune 调用移除后，由本 ticker 低频驱动；失败仅记日志）。
const pruneInterval = 10 * time.Minute

// cronScanInterval 是 cron 触发器调度循环的扫描周期（P1 触发器模块）：
// 每分钟 ClaimDueCron（先 CAS 后入队）——misfire 判定宽限 90s 与本周期
// 同源（domainfunctions.CronMisfireGrace）。
const cronScanInterval = time.Minute

// cronClaimBudget 是单轮 cron 领取的全局预算（跨项目扣减，镜像
// recoverOrphanBatch 的轮转游标模式；补跑风暴由异步通道 + run 信号量兜底）。
const cronClaimBudget = 100

// Worker 消费函数异步执行队列（torchwood:queue:functions-executions）。
type Worker struct {
	functions *appfunctions.Functions
	queue     domainshared.Queue
	logger    *slog.Logger
	workers   int
	// events 是数据库事件触发器消费组（v3 切片 D；nil = 未装配——最小
	// 测试构造 NewWorker 不带事件消费）。
	events *eventTriggerConsumer

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWorker creates the functions execution queue consumer.
func NewWorker(functions *appfunctions.Functions, queue domainshared.Queue, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		functions: functions,
		queue:     queue,
		logger:    logger,
		workers:   workerConcurrency,
	}
}

// NewWorkerWithEventTriggers 是 Wire 装配入口（v3 切片 D，§4.2）：在
// NewWorker 之上注入事件触发器消费组（Redis Stream torchwood:events 的
// functions-triggers 消费组 + 停机补投）。测试侧仍用 NewWorker
// （events nil = 不启动事件消费 goroutine）。
func NewWorkerWithEventTriggers(functions *appfunctions.Functions, queue domainshared.Queue, logger *slog.Logger, rdb *redis.Client, db *clients.Database) *Worker {
	w := NewWorker(functions, queue, logger)
	w.events = newEventTriggerConsumer(functions, rdb, db, logger)
	return w
}

func (w *Worker) Name() string { return "functions-worker" }

func (w *Worker) Init(ctx lynx.AppContext) error {
	return nil
}

func (w *Worker) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.cancel = cancel
	w.mu.Unlock()

	// 孤儿恢复（P0.5 周期任务）：启动即跑一轮，随后每分钟对账——
	// queued/building/running 超 staleAfter 判 failed（兜底 Redis 重启丢
	// 任务、server/worker 崩溃孤儿；含 v1 既有洞：同步快路径崩溃留 queued
	// 永不入队）。全局预算 recoverOrphanBatch + 轮转游标由 app 层持有。
	w.wg.Go(func() { w.recoverLoop(runCtx) })

	// 保留策略周期清理（P0.5：Prune 移出同步热路径后的低频驱动）。
	w.wg.Go(func() { w.pruneLoop(runCtx) })

	// cron 触发器调度循环（P1）：每分钟领取到期触发并入队异步执行。
	w.wg.Go(func() { w.cronLoop(runCtx) })

	// 数据库事件触发器消费循环（v3 切片 D，§4.2/D12）：functions-triggers
	// 消费组 + 订阅匹配器快照 + 停机补投。关停随 runCtx 取消优雅退出
	// （XREADGROUP Block 1s 内返回，对齐 consume 的退出延迟）。
	if w.events != nil {
		w.wg.Go(func() { w.eventLoop(runCtx) })
	}

	for i := 0; i < w.workers; i++ {
		w.wg.Go(func() {
			w.consume(runCtx)
		})
	}
	w.logger.Info("worker started", "workers", w.workers)

	// Start 必须阻塞到应用上下文取消（lynx 把 Start 作为 run.Group actor，
	// 立即返回会被判定为服务完成而触发关停）。
	<-ctx.Done()
	return nil
}

// recoverLoop 周期孤儿恢复（启动即跑一轮；间隔 orphanRecoverInterval）。
func (w *Worker) recoverLoop(ctx context.Context) {
	runOnce := func() {
		recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		recovered, err := w.functions.RecoverOrphanExecutions(recoverCtx, orphanStaleFallback)
		if err != nil {
			w.logger.Warn("recover orphan executions failed", "error", err)
			return
		}
		if recovered > 0 {
			w.logger.Info("recovered orphan executions", "count", recovered)
		}
	}
	runOnce()
	ticker := time.NewTicker(orphanRecoverInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// pruneLoop 周期执行记录保留策略清理（跨全部 active 项目的函数；低频）。
func (w *Worker) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			pruned, err := w.functions.PruneOldExecutions(pruneCtx)
			cancel()
			if err != nil {
				w.logger.Warn("prune old executions failed", "error", err)
				continue
			}
			if pruned > 0 {
				w.logger.Info("pruned old executions", "functions", pruned)
			}
		}
	}
}

// cronLoop 周期 cron 触发器调度（启动即跑一轮——worker 重启后 catch_up_once
// 在首轮扫描即收敛；间隔 cronScanInterval）。单轮失败仅记日志，下一轮
// 重试；领取/CAS 语义见 domainfunctions.TriggerRepo.ClaimDueCron。
func (w *Worker) cronLoop(ctx context.Context) {
	runOnce := func() {
		cronCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 50*time.Second)
		defer cancel()
		dispatched, err := w.functions.DispatchDueCronTriggers(cronCtx, time.Now(), cronClaimBudget)
		if err != nil {
			w.logger.Warn("cron trigger dispatch failed", "error", err)
			return
		}
		if dispatched > 0 {
			w.logger.Info("dispatched cron trigger executions", "count", dispatched)
		}
	}
	runOnce()
	ticker := time.NewTicker(cronScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// eventLoop 数据库事件触发器消费（v3 切片 D）：阻塞至 ctx 取消，内部
// 含快照刷新 / 停机补投 / XREADGROUP 消费（见 event_triggers.go）。
func (w *Worker) eventLoop(ctx context.Context) {
	if err := w.events.Run(ctx); err != nil && ctx.Err() == nil {
		w.logger.Error("event trigger consumer stopped with error", "error", err)
	}
}

func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	cancel := w.cancel
	w.cancel = nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	w.logger.Info("worker stopped")
	return nil
}

func (w *Worker) consume(ctx context.Context) {
	for {
		payload, ack, err := w.queue.Dequeue(ctx, domainshared.QueueFunctionsExecutions, dequeuePollInterval)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.logger.Error("dequeue failed", "error", err)
			time.Sleep(time.Second)
			continue
		}
		if payload == nil {
			continue
		}
		ackDone := func() {
			if ack != "" {
				_ = w.queue.Ack(ctx, domainshared.QueueFunctionsExecutions, ack)
			}
		}
		if err := w.functions.ProcessExecutionPayload(ctx, payload); err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, appfunctions.ErrInvalidQueuePayload) {
				w.logger.Error("discarding invalid queue payload", "error", err)
				ackDone()
				continue
			}
			w.logger.Error("process execution failed", "error", err)
			// 瞬时失败重抛回队，最多 maxProcessAttempts 次；超限兜底标 failed。
			if next, ok := requeue(payload); ok {
				if qerr := w.queue.Enqueue(ctx, domainshared.QueueFunctionsExecutions, next); qerr != nil {
					w.logger.Error("re-enqueue failed", "error", qerr)
				}
				ackDone()
			} else {
				ackDone()
				w.failPayload(ctx, payload, "worker retries exhausted")
			}
			continue
		}
		ackDone()
	}
}

// requeue 将瞬时失败的任务重抛回队：解析 payload 内嵌 attempt 计数并 +1，
// 重新 marshal 后由调用方 Enqueue。队列消息是重试计数的唯一事实来源，
// worker 重启/多副本不会清零或重复计数（B2/R07-P3-8，替代旧的进程内存 map）。
// 超限（attempt > maxProcessAttempts）返回 ok=false，由调用方走 failPayload
// 兜底标记 failed；旧消息无 attempt 字段（json.Unmarshal 视为 0）时首次重试
// 即为 attempt=1。
func requeue(payload []byte) (next []byte, ok bool) {
	var m retryMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		// 防御分支：consume 仅对 ProcessExecutionPayload 解析成功的 payload
		// 调用本函数，正常不可达；视为超限避免坏消息无限重试。
		return nil, false
	}
	m.Attempt++
	if m.Attempt > maxProcessAttempts {
		return nil, false
	}
	next, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return next, true
}

// retryMessage 是 worker 侧解析队列 payload 的完整字段集合（与 functions 包
// queueMessage 的 json 字段名一致，JSON 往返无损：execution_id/function_id/
// project_id/data/attempt；用于 requeue 重抛与 failPayload 兜底标记 failed）。
type retryMessage struct {
	ExecutionID string `json:"execution_id"`
	FunctionID  string `json:"function_id"`
	ProjectID   string `json:"project_id"`
	Data        string `json:"data,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
}

// failPayload 解析 payload 并兜底标记执行失败（best-effort）。
func (w *Worker) failPayload(ctx context.Context, payload []byte, reason string) {
	var m retryMessage
	if err := json.Unmarshal(payload, &m); err != nil || m.ExecutionID == "" {
		w.logger.Error("cannot parse payload to mark failed", "error", err, "payload", string(payload))
		return
	}
	if err := w.functions.MarkExecutionFailed(ctx, m.ProjectID, m.FunctionID, m.ExecutionID, reason); err != nil {
		w.logger.Error("mark execution failed", "execution_id", m.ExecutionID, "error", err)
	}
}
