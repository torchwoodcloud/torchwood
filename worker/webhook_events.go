// Webhooks 订阅的 worker 消费侧（functions-v4.md §6，D5）：与函数事件
// 触发器（event_triggers.go）同款模式——Redis Stream torchwood:events 的
// 独立消费组 webhooks（XREADGROUP、at-least-once、停机补投经 outbox 按
// seq 区间回放，水位键 torchwood:wh:lastseq），差异仅在匹配载体与投递
// 去向：
//
//	outbox 表 ──(既有 OutboxWorker)──▶ torchwood:events ──▶ webhooks 消费组
//		→ XREADGROUP → 反序列化 Envelope → 进程内订阅匹配器（15s 快照）
//		→ 命中 webhook 建投递行（CloudEvents 信封整体入 payload，即时到期）
//		→ XACK（建行成功后）；实际 HTTP 投递由到期扫描 loop（每 1min，
//		ClaimDue CAS → HTTPNotifier → 梯度重投/DLQ）异步完成。
//
// 与 event_triggers.go 的结构平行（不共享代码）是有意取舍：前者深度绑定
// 函数执行通道（InvokeTrigger/审计/限频语义），抽公共骨架会引入双向耦合；
// 两份消费循环各自演进、行为互不牵连。
package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	appwebhooks "github.com/torchwoodcloud/torchwood/internal/app/webhooks"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	domainwebhooks "github.com/torchwoodcloud/torchwood/internal/domain/webhooks"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
)

// webhookEventConsumer 是 webhooks 消费组与订阅匹配器的载体。投递保证：
// at-least-once（消费组 PEL 认领 + 停机补投；重复由投递行 (webhook_id,
// event_id) 唯一索引吸收——建行即幂等，无需消费端幂等键）。诚实边界与
// 函数事件触发器同口径：outbox 24h 清理窗口之外的极端停机才真正丢失。
type webhookEventConsumer struct {
	svc    *appwebhooks.Service
	rdb    *redis.Client
	db     *clients.Database
	logger *slog.Logger
	// consumer 是消费组内成员名（hostname:pid，多副本分摊条目）。
	consumer string
	// index 是进程内订阅匹配器：周期快照整体换入（atomic.Pointer），
	// 消费路径零查库。
	index atomic.Pointer[domainwebhooks.SubscriptionIndex]
	// watermarkScript 原子推进消费水位（单调不回退——多副本共组时 ACK
	// 交错，回退会让 gap 判定误报停机缺口）。
	watermarkScript *redis.Script
}

func newWebhookEventConsumer(svc *appwebhooks.Service, rdb *redis.Client, db *clients.Database, logger *slog.Logger) *webhookEventConsumer {
	if logger == nil {
		logger = slog.Default()
	}
	hostname, _ := os.Hostname()
	return &webhookEventConsumer{
		svc:      svc,
		rdb:      rdb,
		db:       db,
		logger:   logger,
		consumer: hostname + ":" + strconv.Itoa(os.Getpid()),
		watermarkScript: redis.NewScript(
			`local cur = tonumber(redis.call('GET', KEYS[1]) or '0') ` +
				`if tonumber(ARGV[1]) > cur then redis.call('SET', KEYS[1], ARGV[1]) end return 1`),
	}
}

// Run 阻塞运行：首轮快照刷新（失败重试，不拿残缺快照起消费）→ 消费组
// 建立 → 停机补投（消费循环起来前）→ 快照周期刷新 + 消费循环。ctx 取消
// 即返回。
func (c *webhookEventConsumer) Run(ctx context.Context) error {
	for {
		err := c.refreshIndex(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		c.logger.Error("webhook subscription index refresh failed; retrying", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
	if err := c.ensureGroup(ctx, "$"); err != nil {
		// 组建立失败不阻断进程（消费会话每轮重试）；补投依赖水位而非组。
		c.logger.Error("webhook consumer group create failed", "error", err)
	}
	c.backfill(ctx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.indexLoop(ctx)
	}()
	defer wg.Wait()
	c.consumeLoop(ctx)
	return nil
}

// refreshIndex 拉取全量订阅快照并原子换入；任一项目扫描失败时**不换入**
// （残缺快照会在水位推进后把「匹配不到」变成永久丢事件——保留上一轮完整
// 快照，最多滞后一个刷新周期；错误契约见 app 层 RefreshSubscriptionIndex
// 注释）。
func (c *webhookEventConsumer) refreshIndex(ctx context.Context) error {
	idx, err := c.svc.RefreshSubscriptionIndex(ctx, webhookScanBudget)
	if err != nil {
		return err
	}
	c.index.Store(idx)
	return nil
}

func (c *webhookEventConsumer) indexLoop(ctx context.Context) {
	ticker := time.NewTicker(webhookIndexRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.refreshIndex(ctx); err != nil {
				c.logger.Warn("webhook subscription index refresh failed; keeping previous snapshot", "error", err)
			}
		}
	}
}

// ensureGroup 建立消费组（XGROUP MKSTREAM）。首建 ID：`$`（从现在起消费）；
// 先于本调用的历史条目由停机补投路径经水位判定补齐。start="0"（NOGROUP
// 自愈）时整窗重放：组被异常销毁后宁可重放（投递行幂等吸收）也不静默跳过。
func (c *webhookEventConsumer) ensureGroup(ctx context.Context, start string) error {
	err := c.rdb.XGroupCreateMkStream(ctx, domainshared.EventsStream, domainshared.EventsGroupWebhooks, start).Err()
	if err != nil && strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

func (c *webhookEventConsumer) consumeLoop(ctx context.Context) {
	backoff := time.Second
	for {
		if err := c.consumeSession(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logger.Error("webhook consume session failed", "error", err)
			if strings.Contains(err.Error(), "NOGROUP") {
				if gerr := c.ensureGroup(ctx, "0"); gerr != nil {
					c.logger.Error("webhook consumer group recreate failed", "error", gerr)
				}
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff *= 2
			if backoff > eventMaxBackoff {
				backoff = eventMaxBackoff
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		backoff = time.Second
	}
}

func (c *webhookEventConsumer) consumeSession(ctx context.Context) error {
	if err := c.ensureGroup(ctx, "$"); err != nil {
		return err
	}
	if err := c.claimStale(ctx); err != nil {
		return err
	}
	streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    domainshared.EventsGroupWebhooks,
		Consumer: c.consumer,
		Streams:  []string{domainshared.EventsStream, ">"},
		Count:    eventReadGroupCount,
		Block:    eventReadGroupBlock,
	}).Result()
	if err != nil {
		if err == redis.Nil { // BLOCK 超时：正常心跳
			return nil
		}
		return err
	}
	for _, st := range streams {
		var ackIDs []string
		maxSeq := int64(0)
		for i := range st.Messages {
			msg := &st.Messages[i]
			seq := c.processEntry(ctx, msg)
			ackIDs = append(ackIDs, msg.ID)
			if seq > maxSeq {
				maxSeq = seq
			}
		}
		if len(ackIDs) > 0 {
			// 批内先建行后统一 XACK；XACK 失败条目留 PEL，由
			// eventClaimMinIdle 窗口后重投（at-least-once；建行幂等）。
			if aerr := c.rdb.XAck(ctx, domainshared.EventsStream, domainshared.EventsGroupWebhooks, ackIDs...).Err(); aerr != nil {
				c.logger.Error("webhook xack failed; entries stay in PEL", "count", len(ackIDs), "error", aerr)
			} else if maxSeq > 0 {
				c.advanceWatermark(ctx, maxSeq)
			}
		}
	}
	return nil
}

// claimStale 回收本组 PEL 中 idle 超时条目（崩溃残留），按普通消息处理。
func (c *webhookEventConsumer) claimStale(ctx context.Context) error {
	claimed, _, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   domainshared.EventsStream,
		Group:    domainshared.EventsGroupWebhooks,
		Consumer: c.consumer,
		MinIdle:  eventClaimMinIdle,
		Start:    "0-0",
		Count:    eventReadGroupCount,
	}).Result()
	if err != nil {
		if strings.Contains(err.Error(), "NOGROUP") {
			return err
		}
		c.logger.Warn("webhook xautoclaim failed", "error", err)
		return nil
	}
	if len(claimed) == 0 {
		return nil
	}
	var ackIDs []string
	maxSeq := int64(0)
	for i := range claimed {
		msg := &claimed[i]
		seq := c.processEntry(ctx, msg)
		ackIDs = append(ackIDs, msg.ID)
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	if err := c.rdb.XAck(ctx, domainshared.EventsStream, domainshared.EventsGroupWebhooks, ackIDs...).Err(); err != nil {
		c.logger.Error("webhook xack (claim) failed", "count", len(ackIDs), "error", err)
	} else if maxSeq > 0 {
		c.advanceWatermark(ctx, maxSeq)
	}
	return nil
}

// processEntry 处理单条 Stream 条目：解码信封 → 匹配建行。返回条目 seq
// （毒消息/未知序返回 0，不参与水位推进）。毒消息直接丢弃（ACK 不重投）。
func (c *webhookEventConsumer) processEntry(ctx context.Context, msg *redis.XMessage) int64 {
	raw, _ := msg.Values["payload"].(string)
	if raw == "" {
		c.logger.Error("webhook stream empty payload", "stream_id", msg.ID)
		return 0
	}
	ev, err := infraevents.UnmarshalEnvelope([]byte(raw))
	if err != nil {
		c.logger.Error("webhook stream malformed payload", "stream_id", msg.ID, "error", err)
		return 0
	}
	c.deliver(ctx, &ev)
	return ev.Seq
}

// deliver 匹配并建行：系统行为事件（Domain 非空）走 deliverSystem；文档
// 事件走订阅匹配；未知形态/无命中静默通过（照常推进水位）。建行失败时
// 条目仍会被调用方 ACK：该事件对本订阅者的投递延迟至停机补投路径。
func (c *webhookEventConsumer) deliver(ctx context.Context, ev *domainevents.Envelope) {
	if ev.Domain != "" {
		c.deliverSystem(ctx, ev)
		return
	}
	op := domainfunctions.EventOpFromEnvelope(ev.Event)
	if op == "" {
		return
	}
	whIDs := c.index.Load().Match(ev.ProjectID, ev.DatabaseID, ev.CollectionID, op)
	if len(whIDs) == 0 {
		return
	}
	// data 投影（与函数事件触发器同预算同形态：ID + 摘要进 data ≤32KB）。
	data, err := domainfunctions.BuildEventInvocationData(ev)
	if err != nil {
		c.logger.Error("webhook event invocation data marshal failed", "event_id", ev.EventID, "error", err)
		return
	}
	c.enqueue(ctx, ev, whIDs, data)
}

// deliverSystem 投递系统行为事件：按事件全名查系统订阅索引。目录外事件
// 静默通过（产生侧坏数据防御），照常推进水位。
func (c *webhookEventConsumer) deliverSystem(ctx context.Context, ev *domainevents.Envelope) {
	if !domainevents.IsSystemEventName(ev.Event) {
		return
	}
	whIDs := c.index.Load().MatchSystem(ev.ProjectID, ev.Event)
	if len(whIDs) == 0 {
		return
	}
	data, err := domainfunctions.BuildSystemEventInvocationData(ev)
	if err != nil {
		c.logger.Error("webhook system event invocation data marshal failed", "event_id", ev.EventID, "error", err)
		return
	}
	c.enqueue(ctx, ev, whIDs, data)
}

// enqueue 为命中订阅建投递行（即时到期；实际 HTTP 投递由到期扫描 loop
// 领取执行）。
func (c *webhookEventConsumer) enqueue(ctx context.Context, ev *domainevents.Envelope, whIDs []string, data string) {
	n, err := c.svc.CreateDeliveries(ctx, appwebhooks.CreateDeliveriesCommand{
		ProjectID:  ev.ProjectID,
		WebhookIDs: whIDs,
		Envelope:   ev,
		Data:       data,
	})
	if err != nil {
		webhookEventDeliveriesTotal.WithLabelValues(ev.ProjectID, "enqueue_error").Inc()
		c.logger.Warn("webhook delivery rows create failed", "project_id", ev.ProjectID,
			"event_id", ev.EventID, "webhooks", len(whIDs), "error", err)
		return
	}
	// n = 新建行数（重复投递被 (webhook_id, event_id) 幂等吸收时不计数——
	// 指标语义 = 实际新投递面）。
	result := "ok"
	if n == 0 {
		result = "dedup"
	}
	webhookEventDeliveriesTotal.WithLabelValues(ev.ProjectID, result).Inc()
}

// ——停机补投（与 event_triggers 同款水位机制，水位键 torchwood:wh:lastseq）——

// backfill 检测停机裁剪缺口并从 outbox 表补投：读自管水位 W → 读 Stream
// 现存首条 seq F → F > W+1 即存在被 XTRIM 裁掉、本组从未消费的条目 → 按
// seq ∈ (W, F] 分批补投（上界收在 Stream 现存首条，重叠由投递行幂等吸收）。
// 补投发生即告警锚点（webhookEventBackfillTotal 非零 + Warn 日志）。
func (c *webhookEventConsumer) backfill(ctx context.Context) {
	watermark, err := c.getWatermark(ctx)
	if err != nil {
		// 瞬断等读取失败：本轮跳过补投（水位留待下轮）。按 0 起步会把
		// (0, first] 全窗误判为停机缺口。
		c.logger.Warn("webhook watermark read failed; skipping backfill this round", "error", err)
		return
	}
	first := c.streamFirstSeq(ctx)
	if first <= 0 || first <= watermark+1 {
		return
	}
	c.logger.Warn("webhook delivery gap detected; backfilling from outbox",
		"watermark", watermark, "stream_first_seq", first,
		"hint", "worker downtime exceeded the stream trim window (functions-v4 §6)")

	cursor := watermark
	for cursor < first && ctx.Err() == nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventBackfillTimeout)
		rows, err := infraevents.ScanOutboxSeqRange(sctx, c.db, cursor, first, eventBackfillBatch)
		cancel()
		if err != nil {
			c.logger.Error("webhook backfill scan failed", "cursor", cursor, "error", err)
			return // 下次启动重试（水位未推进，缺口仍在）
		}
		if len(rows) == 0 {
			break // 区间行已被 24h 清理窗口裁掉（诚实边界）：尽力而为
		}
		maxSeq := cursor
		for i := range rows {
			row := &rows[i]
			delivered := false
			if ev, err := infraevents.UnmarshalEnvelope(row.Payload); err != nil {
				c.logger.Error("webhook backfill payload malformed", "event_id", row.EventID, "error", err)
			} else {
				ev.Seq = row.Seq
				c.deliver(ctx, &ev)
				delivered = true
			}
			result := "ok"
			if !delivered {
				result = "error"
			}
			webhookEventBackfillTotal.WithLabelValues(row.ProjectID, result).Inc()
			if row.Seq > maxSeq {
				maxSeq = row.Seq
			}
		}
		c.advanceWatermark(ctx, maxSeq)
		cursor = maxSeq
	}
}

// getWatermark 读自管消费水位。键缺失（redis.Nil）= 合法 0：从未消费过 →
// 全区间按缺口处理（首次部署对存量 Stream 的一次性全量核对，无订阅时零
// 建行纯扫描）。其他错误上抛给调用方跳过本轮补投。
func (c *webhookEventConsumer) getWatermark(ctx context.Context) (int64, error) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	v, err := c.rdb.Get(wctx, domainshared.WebhooksEventLastSeqKey).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// advanceWatermark 原子推进消费水位（Lua 单调：仅新值更大才写）。
func (c *webhookEventConsumer) advanceWatermark(ctx context.Context, seq int64) {
	if seq <= 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := c.watermarkScript.Run(wctx, c.rdb, []string{domainshared.WebhooksEventLastSeqKey}, seq).Err(); err != nil {
		c.logger.Warn("webhook watermark advance failed", "seq", seq, "error", err)
	}
}

// streamFirstSeq 读 Stream 现存首条的信封 seq（XRANGE 头部一条）；Stream
// 为空或条目 seq 未知返回 0——无从判定缺口，跳过补投。
func (c *webhookEventConsumer) streamFirstSeq(ctx context.Context) int64 {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	entries, err := c.rdb.XRangeN(rctx, domainshared.EventsStream, "-", "+", 1).Result()
	if err != nil || len(entries) == 0 {
		return 0
	}
	raw, _ := entries[0].Values["payload"].(string)
	if raw == "" {
		return 0
	}
	ev, err := infraevents.UnmarshalEnvelope([]byte(raw))
	if err != nil {
		return 0
	}
	return ev.Seq
}

// ——事件消费指标——

var (
	// webhookEventDeliveriesTotal 是事件→投递行建行计数：只记命中的订阅
	//（no_match 不记）；result ∈ ok|dedup|enqueue_error。
	webhookEventDeliveriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_webhook_event_deliveries_total",
		Help: "Webhook delivery rows created from stream events, by project and result (ok|dedup|enqueue_error).",
	}, []string{"project", "result"})
	// webhookEventBackfillTotal 是停机补投计数（补投发生即告警锚点）。
	webhookEventBackfillTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_webhook_event_backfill_total",
		Help: "Webhook outbox backfill rows, by project and result (ok|error).",
	}, []string{"project", "result"})
)

func init() {
	prometheus.MustRegister(webhookEventDeliveriesTotal, webhookEventBackfillTotal)
}
