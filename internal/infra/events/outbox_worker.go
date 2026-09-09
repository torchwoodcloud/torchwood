package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"
)

const (
	// outboxFallbackPollInterval 是 LISTEN 不可用/漏信号时的兜底轮询间隔
	//（阶段④ §4.5：NOTIFY 是优化项非正确性项——唤醒主路径为 LISTEN，
	// 正确性由本兜底保证）。pgdriver Listener 的健康 ping（5s 无通知即
	// NOTIFY bun:ping）与该值天然重叠。
	outboxFallbackPollInterval = 5 * time.Second
	// outboxBatchSize 是每轮领取上限（FOR UPDATE SKIP LOCKED，256 行——
	// 阶段④ B3：与 Stream 消费端批量同量级，摊薄 NOTIFY 逐次唤醒的固定开销）。
	outboxBatchSize = 256
	// maxOutboxAttempts 是 XADD 失败的最大重试次数，超限迁入死信表。
	maxOutboxAttempts = 10
	// outboxRedispatchAfter 是整进程挂死兜底窗口：dispatched_at 超过
	// 该时长仍未 published（server 无人消费）的行被重新领取再 XADD。
	outboxRedispatchAfter = 2 * time.Minute
	// outboxMaxBackoff 是 XADD 失败后 available_at 退避上限。
	outboxMaxBackoff = time.Minute
	// outboxCleanupInterval 是已发布行/死信行清理周期。
	outboxCleanupInterval = 10 * time.Minute
	// outboxPublishedRetention 是已发布行保留窗口（排障/重放核对；
	// 阶段④重放承诺窗 1h << 24h，无需调整——:changes 与 last_seq 的
	// 重放数据源统一为本表）。
	outboxPublishedRetention = 24 * time.Hour
	// outboxDeadRetention 是死信行保留窗口。
	outboxDeadRetention = 30 * 24 * time.Hour
	// outboxStatementTimeout 是每条 DB 语句的独立超时（W-H per-语句 deadline，
	// 防单条慢查询卡住轮询；事务整体取 2*statement）。
	outboxStatementTimeout = 5 * time.Second
	// outboxIdleGroupAfter 是孤儿消费组判定的闲置阈值（B6，redesign §4.5
	// 挂账）：组名 = 实例 ID（hostname:pid），实例崩溃/重启后旧组无人认领。
	// 消费组的位点/成员/PEL 全部静默超过该时长的组由周期清理销毁，防组
	// 无限累积（ >> cleanup 周期，保证每轮销毁的是"真孤儿"）。
	outboxIdleGroupAfter = time.Hour
)

// outbox 领取与投递指标（前缀 torchwood_，注册到默认注册表，
// worker 进程未挂 /metrics 端点，仅统计计数，供日后接入）。
var (
	outboxPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torchwood_outbox_pending",
		Help: "Number of outbox rows not yet published (published_at IS NULL).",
	})
	outboxPublishTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_outbox_publish_total",
		Help: "Total outbox rows enqueued to the realtime transport.",
	}, []string{"result"})
	outboxPublishLag = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torchwood_outbox_publish_lag_seconds",
		Help:    "Latency between event creation and transport enqueue.",
		Buckets: prometheus.DefBuckets,
	})
	// outboxDead 是死信表当前行数（W-J：死信可观测——非零即需人工介入，
	// 重放工具见修复方案 W-J 残留）。
	outboxDead = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torchwood_outbox_dead",
		Help: "Number of rows in document_events_outbox_dead.",
	})
	// realtimeIdleGroupsDestroyed 是累计销毁的闲置孤儿消费组数（B6：清理
	// 行为可观测；非零说明存在实例崩溃/重建留下的旧组被回收）。
	realtimeIdleGroupsDestroyed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torchwood_realtime_idle_groups_destroyed_total",
		Help: "Total idle consumer groups destroyed from the realtime events stream.",
	})
)

func init() {
	prometheus.MustRegister(outboxPending, outboxPublishTotal, outboxPublishLag, outboxDead,
		realtimeIdleGroupsDestroyed)
}

// OutboxWorker 领取 document_events_outbox 并把完整信封 XADD 到
// Redis Stream（v2 设计 §3.4）。成功只标 dispatched_at，**不**标
// published_at（后者由 server 进程的 RealtimeSubscriber 在 XACK 后标记）。
type OutboxWorker struct {
	db        *clients.Database
	transport shared.RealtimeTransport
	logger    *slog.Logger
}

// NewOutboxWorker 构造 outbox → transport 的领取循环。
func NewOutboxWorker(db *clients.Database, transport shared.RealtimeTransport, logger *slog.Logger) *OutboxWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &OutboxWorker{db: db, transport: transport, logger: logger}
}

// Run 以「LISTEN 唤醒 + 5s 兜底轮询」驱动领取（阶段④ §4.5）；低频 ticker
// 清理已发布/死信行；ctx 取消即返回。LISTEN 走 pgdriver.Listener 的专属
// 连接（不占池），断线由 Listener 内部重连；建立失败仅记录并退化纯轮询。
func (w *OutboxWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(outboxFallbackPollInterval)
	defer ticker.Stop()
	cleanup := time.NewTicker(outboxCleanupInterval)
	defer cleanup.Stop()

	notify := w.listen(ctx)

	// 启动即清一次，避免短命进程错过周期窗口。
	w.cleanupOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-cleanup.C:
			w.cleanupOnce(ctx)
		case _, ok := <-notify:
			if !ok {
				// Listener 关闭（仅 Close 触发，此处等于致命）——退化纯轮询。
				notify = nil
			}
		}
		if err := w.pollOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logger.Error("outbox claim failed", "error", err)
		}
	}
}

// listen 建立专属 LISTEN 连接并返回通知 channel；失败返回 nil（退化纯轮询，
// 正确性由兜底 ticker 保证）。Listener.Channel 自带连接健康探测（5s 无通知
// 经池连接 NOTIFY bun:ping），断线自动重连并重放 LISTEN。
func (w *OutboxWorker) listen(ctx context.Context) <-chan pgdriver.Notification {
	ln := pgdriver.NewListener(w.db.DB)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	if err := ln.Listen(context.Background(), outboxNotifyChannel); err != nil {
		if ctx.Err() == nil {
			w.logger.Error("outbox LISTEN failed; falling back to polling", "error", err)
		}
	}
	return ln.Channel(pgdriver.WithChannelSize(16))
}

// pollOnce 领取一轮：主路径（dispatched_at IS NULL）+ 2min 整进程挂死
// 兜底（dispatched_at 过旧）合并为同一查询（v2 设计 §3.4 领取 SQL）。
func (w *OutboxWorker) pollOnce(ctx context.Context) error {
	func() {
		ctx2, cancel := context.WithTimeout(ctx, outboxStatementTimeout)
		defer cancel()
		if n, err := w.db.Conn(ctx2).NewSelect().Model((*model.DocumentEventsOutbox)(nil)).
			Where("published_at IS NULL").Count(ctx2); err == nil {
			outboxPending.Set(float64(n))
		}
	}()

	rows, err := w.claim(ctx)
	if err != nil {
		return err
	}
	for i := range rows {
		w.dispatch(ctx, &rows[i])
	}
	return nil
}

// claim 在单个事务内 SELECT ... FOR UPDATE SKIP LOCKED 并把领取行标记
// dispatched_at——行锁在整个事务内生效，多副本不会领取同一批行重复 XADD。
// 标记随事务提交持久化；XADD 在事务外进行（不持数据库行锁做 IO）。
// claim 后崩溃的行由 2min redispatch 窗口兜底重发。
func (w *OutboxWorker) claim(ctx context.Context) ([]model.DocumentEventsOutbox, error) {
	var rows []model.DocumentEventsOutbox
	ctx2, cancel := context.WithTimeout(ctx, 2*outboxStatementTimeout)
	defer cancel()
	err := w.db.RunInTx(ctx2, func(txCtx context.Context) error {
		if err := w.db.Conn(txCtx).NewSelect().Model(&rows).
			Column("event_id", "seq", "payload", "channel", "created_at", "attempts").
			Where("published_at IS NULL").
			Where("available_at <= NOW()").
			Where(fmt.Sprintf("(dispatched_at IS NULL OR dispatched_at < NOW() - INTERVAL '%d minutes')",
				int(outboxRedispatchAfter.Minutes()))).
			Order("seq").
			Limit(outboxBatchSize).
			For("UPDATE SKIP LOCKED").
			Scan(txCtx); err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]string, len(rows))
		for i := range rows {
			ids[i] = rows[i].EventID
		}
		_, err := w.db.Conn(txCtx).NewUpdate().Model((*model.DocumentEventsOutbox)(nil)).
			Set("dispatched_at = NOW()").
			Where("event_id IN (?)", bun.List(ids)).
			Exec(txCtx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// idleGroupSweeper 是 shared.RealtimeTransport 的可选扩展（B6）：销毁投递
// Stream 上闲置孤儿消费组。realtime.streamTransport 实现之；测试桩可不必
// 实现（断言失败即跳过，不阻塞 worker 装配）。
type idleGroupSweeper interface {
	SweepIdleGroups(ctx context.Context, idle time.Duration) ([]string, error)
}

// cleanupOnce 删除超过保留窗口的已发布行与死信行（表无限增长治理），
// 并对投递 Stream 做周期 XTRIM（B3：MAXLEN ~ 100k，未投递条目不受影响——
// XADD 不带 MAXLEN，裁剪只发生在低频清理路径）；最后销毁闲置孤儿消费组
// （B6：组名 = 实例 ID，实例崩溃/重启后旧组无人认领，周期回收防无限累积）。
func (w *OutboxWorker) cleanupOnce(ctx context.Context) {
	func() {
		ctx2, cancel := context.WithTimeout(ctx, outboxStatementTimeout)
		defer cancel()
		if n, err := w.db.Conn(ctx2).NewSelect().TableExpr("document_events_outbox_dead").Count(ctx2); err == nil {
			outboxDead.Set(float64(n))
		}
	}()
	func() {
		ctx2, cancel := context.WithTimeout(ctx, outboxStatementTimeout)
		defer cancel()
		if res, err := w.db.Conn(ctx2).NewDelete().Model((*model.DocumentEventsOutbox)(nil)).
			Where("published_at IS NOT NULL").
			Where("published_at < ?", time.Now().Add(-outboxPublishedRetention)).
			Exec(ctx2); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				w.logger.Info("outbox purged published rows", "count", n)
			}
		} else if ctx.Err() == nil {
			w.logger.Error("outbox purge published failed", "error", err)
		}
	}()
	func() {
		ctx2, cancel := context.WithTimeout(ctx, outboxStatementTimeout)
		defer cancel()
		if res, err := w.db.Conn(ctx2).NewRaw(
			`DELETE FROM document_events_outbox_dead WHERE created_at < ?`,
			time.Now().Add(-outboxDeadRetention)).Exec(ctx2); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				w.logger.Info("outbox purged dead rows", "count", n)
			}
		} else if ctx.Err() == nil {
			w.logger.Error("outbox purge dead failed", "error", err)
		}
	}()
	func() {
		ctx2, cancel := context.WithTimeout(ctx, outboxStatementTimeout)
		defer cancel()
		if err := w.transport.Trim(ctx2); err != nil && ctx.Err() == nil {
			w.logger.Error("realtime stream trim failed", "error", err)
		}
	}()
	func() {
		sweeper, ok := w.transport.(idleGroupSweeper)
		if !ok {
			return
		}
		ctx2, cancel := context.WithTimeout(ctx, outboxStatementTimeout)
		defer cancel()
		destroyed, err := sweeper.SweepIdleGroups(ctx2, outboxIdleGroupAfter)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				w.logger.Error("realtime idle group sweep failed", "error", err)
			}
		case len(destroyed) > 0:
			realtimeIdleGroupsDestroyed.Add(float64(len(destroyed)))
			w.logger.Info("realtime idle consumer groups destroyed",
				"groups", destroyed, "idle_after", outboxIdleGroupAfter.String())
		}
	}()
}

// dispatch 把单行信封 XADD 到 Stream。dispatched_at 已在 claim 事务内标记，
// 此处无需回写；XADD 失败走 failRow（归还 dispatched_at 并按退避重试）。
// seq 从行回填进信封再序列化——Stream 条目与下游 WS 帧的 seq 唯一来源。
func (w *OutboxWorker) dispatch(ctx context.Context, row *model.DocumentEventsOutbox) {
	ev, err := UnmarshalEnvelope(row.Payload)
	if err != nil {
		w.logger.Error("outbox row payload malformed", "event_id", row.EventID, "error", err)
		w.failRow(ctx, row, err)
		return
	}
	ev.Seq = row.Seq
	if err := w.transport.Enqueue(ctx, ev); err != nil {
		if ctx.Err() != nil {
			return
		}
		outboxPublishTotal.WithLabelValues("error").Inc()
		w.logger.Error("outbox enqueue failed", "event_id", row.EventID, "error", err)
		w.failRow(ctx, row, err)
		return
	}
	outboxPublishTotal.WithLabelValues("ok").Inc()
	outboxPublishLag.Observe(time.Since(ev.CreatedAt).Seconds())
}

// failRow 记录 XADD 失败：attempts+1、available_at 指数退避（上限 60s）、
// 归还 dispatched_at（claim 时预标记的，置 NULL 恢复"按退避快速重试"语义，
// 不必等 2min redispatch 窗口）；attempts 达到 maxOutboxAttempts 时把行
// 迁入 document_events_outbox_dead。
func (w *OutboxWorker) failRow(ctx context.Context, row *model.DocumentEventsOutbox, cause error) {
	attempts := row.Attempts + 1
	if attempts >= maxOutboxAttempts {
		ctx2, cancel := context.WithTimeout(ctx, 2*outboxStatementTimeout)
		defer cancel()
		if err := w.db.RunInTx(ctx2, func(txCtx context.Context) error {
			if _, err := w.db.Conn(txCtx).NewRaw(`INSERT INTO document_events_outbox_dead
				(event_id, project_id, topic, channel, payload, attempts, last_error, created_at)
				SELECT event_id, project_id, topic, channel, payload, ? AS attempts, ? AS last_error, created_at
				FROM document_events_outbox
				WHERE event_id = ?`,
				attempts, cause.Error(), row.EventID).Exec(txCtx); err != nil {
				return err
			}
			_, err := w.db.Conn(txCtx).NewDelete().Model((*model.DocumentEventsOutbox)(nil)).
				Where("event_id = ?", row.EventID).Exec(txCtx)
			return err
		}); err != nil {
			w.logger.Error("outbox dead-letter failed", "event_id", row.EventID, "error", err)
			return
		}
		outboxPublishTotal.WithLabelValues("dead").Inc()
		w.logger.Warn("outbox event dead-lettered",
			"event_id", row.EventID, "attempts", attempts, "error", cause)
		return
	}
	backoff := time.Duration(1<<attempts) * time.Second
	if backoff > outboxMaxBackoff {
		backoff = outboxMaxBackoff
	}
	func() {
		ctx2, cancel := context.WithTimeout(ctx, outboxStatementTimeout)
		defer cancel()
		if _, err := w.db.Conn(ctx2).NewUpdate().Model((*model.DocumentEventsOutbox)(nil)).
			Set("attempts = attempts + 1").
			Set("available_at = ?", time.Now().Add(backoff)).
			Set("dispatched_at = NULL").
			Where("event_id = ?", row.EventID).Exec(ctx2); err != nil {
			w.logger.Error("mark outbox retry failed", "event_id", row.EventID, "error", err)
		}
	}()
}
