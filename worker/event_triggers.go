// 数据库事件触发器的 worker 消费侧（functions v3 切片 D，docs/design/
// functions-v3.md §4.2/D12；对抗审查修正：停机补投一期必做）。
//
// 链路：outbox 表 ──(既有 OutboxWorker，零改动)──▶ Redis Stream
// torchwood:events ──▶ functions-triggers 消费组（本文件，worker 进程）
//
//	→ XREADGROUP → 反序列化 Envelope → 进程内匹配器（周期快照，零查库）
//	→ 命中触发器逐条 InvokeTrigger（async，Source=event:{trigger_id}）
//	→ XACK（入队成功后）。
//
// 停机补投（D12）：StreamTrimmer 的 XTRIM 不理会消费组进度，停机超过裁剪
// 窗口后消费组会静默跳到现存最老条目。worker 启动时（消费循环起来前）以
// Redis 自管水位 torchwood:fnevent:lastseq 与 Stream 现存首条 seq 比较，
// 存在裁剪缺口即从 outbox 表按 seq ∈ (lastseq, first_seq_in_stream] 分批
// 补投（上界收在 Stream 现存首条——与恢复后的正常消费重叠投递由
// at-least-once + 函数幂等吸收）。
package worker

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
)

const (
	// eventIndexRefreshInterval 是订阅匹配器快照的刷新周期（10–30s 档；
	// 触发器创建/启停到生效的传播窗口 = 本值 + 扫描时长，窗口内的事件对
	// 新触发器不补投——快照语义，§12.5 文档明示）。
	eventIndexRefreshInterval = 15 * time.Second

	// eventReadGroupCount / eventReadGroupBlock 对齐 realtime subscriber
	// 的批量口径；Block 1s 兼顾优雅退出（对齐 dequeuePollInterval 先例）。
	eventReadGroupCount = 64
	eventReadGroupBlock = time.Second

	// eventClaimMinIdle 是 PEL 认领的最小 idle：XACK 前崩溃的在途条目由
	// 下一轮 XAUTOCLAIM 重投（重投重复由函数 event_id/seq 幂等吸收）。
	eventClaimMinIdle = time.Minute

	// eventBackfillBatch 是补投的单批行数（D12：停机一天的全量补投按批
	// 限量推进，共享异步通道既有信号量兜底——风暴语义与 cron
	// catch_up_once 同款）。
	eventBackfillBatch = 500
	// eventBackfillTimeout 是补投单批扫描的语句超时（bunrepo 读 5s 档 ×2）。
	eventBackfillTimeout = 10 * time.Second

	// eventScanBudget 是单轮快照扫描的触发器总预算（异常膨胀防御；
	// 超出截断到预算——索引是快照不是队列，余量下轮带出）。
	eventScanBudget = 1000

	// eventMaxBackoff 是 Redis 断线重试退避上限（对齐 realtime subscriber）。
	eventMaxBackoff = 30 * time.Second
)

// 事件触发器投递指标（v3 Observability；前缀 torchwood_，包内自注册，
// projectschema/outbox 同模式）。
var (
	// eventDeliveriesTotal 是投递计数：**只记命中的触发器**（no_match 不记
	// ——OQ9 收口：一期不做精确限流，本指标速率即风暴告警锚点）。
	eventDeliveriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_functions_event_deliveries_total",
		Help: "Event trigger deliveries matched and enqueued, by project, function and result (ok|enqueue_error).",
	}, []string{"project", "function", "result"})
	// eventBackfillTotal 是停机补投计数（对抗审查补充——补投发生即告警
	// 锚点：停机窗口可视）。
	eventBackfillTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_functions_event_backfill_total",
		Help: "Event trigger outbox backfill rows, by project and result (ok|error).",
	}, []string{"project", "result"})
)

func init() {
	prometheus.MustRegister(eventDeliveriesTotal, eventBackfillTotal)
}

// eventTriggerConsumer 是 functions-triggers 消费组与订阅匹配器的载体。
// 投递保证：at-least-once（补投与正常路径重叠由函数幂等吸收，幂等键推荐
// 来源 = data 投影的 event_id/seq，v3 §4.2）。诚实边界：outbox 表 24h
// 清理窗口之外的极端停机（>24h）才真正丢失（§12.5 明示）。
type eventTriggerConsumer struct {
	functions *appfunctions.Functions
	rdb       *redis.Client
	db        *clients.Database
	logger    *slog.Logger
	// consumer 是消费组内成员名（hostname:pid，多副本分摊条目）。
	consumer string

	// index 是进程内订阅匹配器：周期快照整体换入（atomic.Pointer），
	// 消费路径零查库（v3 §4.2）。
	index atomic.Pointer[domainfunctions.EventTriggerIndex]
	// watermarkScript 原子推进消费水位（单调不回退——多副本共组时 ACK
	// 交错，回退会让 gap 判定误报停机缺口）。
	watermarkScript *redis.Script
}

func newEventTriggerConsumer(functions *appfunctions.Functions, rdb *redis.Client, db *clients.Database, logger *slog.Logger) *eventTriggerConsumer {
	if logger == nil {
		logger = slog.Default()
	}
	hostname, _ := os.Hostname()
	return &eventTriggerConsumer{
		functions: functions,
		rdb:       rdb,
		db:        db,
		logger:    logger,
		consumer:  hostname + ":" + strconv.Itoa(os.Getpid()),
		// 单调推进：仅新值更大才写（EVAL 原子；KEYS[1]=水位键 ARGV[1]=seq）。
		watermarkScript: redis.NewScript(
			`local cur = tonumber(redis.call('GET', KEYS[1]) or '0') ` +
				`if tonumber(ARGV[1]) > cur then redis.call('SET', KEYS[1], ARGV[1]) end return 1`),
	}
}

// Run 阻塞运行：首轮快照刷新（失败重试，不拿残缺快照起消费）→ 停机补投
// （D12：消费循环起来前）→ 快照周期刷新 + 消费循环。ctx 取消即返回。
func (c *eventTriggerConsumer) Run(ctx context.Context) error {
	for {
		err := c.refreshIndex(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		c.logger.Error("event trigger index refresh failed; retrying", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
	if err := c.ensureGroup(ctx, "$"); err != nil {
		// 组建立失败不阻断进程（消费会话每轮重试）；补投依赖水位而非组，
		// 照常执行。
		c.logger.Error("event trigger consumer group create failed", "error", err)
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

// refreshIndex 拉取全量快照并原子换入；任一项目扫描失败时**不换入**
// （残缺快照会在水位推进后把「匹配不到」变成永久丢事件——保留上一轮
// 完整快照，最多滞后一个刷新周期；错误契约见 app 层
// RefreshEventTriggerIndex 注释）。
func (c *eventTriggerConsumer) refreshIndex(ctx context.Context) error {
	idx, err := c.functions.RefreshEventTriggerIndex(ctx, eventScanBudget)
	if err != nil {
		return err
	}
	c.index.Store(idx)
	return nil
}

// indexLoop 周期刷新订阅匹配器快照。
func (c *eventTriggerConsumer) indexLoop(ctx context.Context) {
	ticker := time.NewTicker(eventIndexRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.refreshIndex(ctx); err != nil {
				c.logger.Warn("event trigger index refresh failed; keeping previous snapshot", "error", err)
			}
		}
	}
}

// ensureGroup 建立消费组（XGROUP MKSTREAM）。首建 ID：`$`（从现在起消费）；
// 先于本调用的历史条目由停机补投路径经水位判定补齐（见 backfill）——
// 与 realtime subscriber「新组从 $ 起步」同一取舍，正确性在水位 + outbox。
// start="0"（NOGROUP 自愈）时整窗重放：组被异常销毁后宁可重放（幂等吸收）
// 也不静默跳过。
func (c *eventTriggerConsumer) ensureGroup(ctx context.Context, start string) error {
	err := c.rdb.XGroupCreateMkStream(ctx, domainshared.EventsStream, domainshared.EventsGroupFunctionsTriggers, start).Err()
	if err != nil && strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

// consumeLoop 消费会话：XAUTOCLAIM 回收卡死 PEL → XREADGROUP 读新条目 →
// 匹配投递 → 批量 XACK → 推进水位。断线指数退避重连，不退出进程。
func (c *eventTriggerConsumer) consumeLoop(ctx context.Context) {
	backoff := time.Second
	for {
		if err := c.consumeSession(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logger.Error("event trigger consume session failed", "error", err)
			// NOGROUP = 组被外部销毁（防御；idle sweeper 只针对实例组，
			// 正常不触达本组）——从 0 重建组整窗重放。
			if strings.Contains(err.Error(), "NOGROUP") {
				if gerr := c.ensureGroup(ctx, "0"); gerr != nil {
					c.logger.Error("event trigger consumer group recreate failed", "error", gerr)
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

func (c *eventTriggerConsumer) consumeSession(ctx context.Context) error {
	if err := c.ensureGroup(ctx, "$"); err != nil {
		return err
	}
	if err := c.claimStale(ctx); err != nil {
		return err
	}
	streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    domainshared.EventsGroupFunctionsTriggers,
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
			// 批内先投递后统一 XACK；XACK 失败条目留 PEL，由
			// eventClaimMinIdle 窗口后重投（at-least-once）。
			if aerr := c.rdb.XAck(ctx, domainshared.EventsStream, domainshared.EventsGroupFunctionsTriggers, ackIDs...).Err(); aerr != nil {
				c.logger.Error("event trigger xack failed; entries stay in PEL", "count", len(ackIDs), "error", aerr)
			} else if maxSeq > 0 {
				c.advanceWatermark(ctx, maxSeq)
			}
		}
	}
	return nil
}

// claimStale 回收本组 PEL 中 idle 超过 eventClaimMinIdle 的条目（崩溃残留），
// 按普通消息处理。NOGROUP 上抛触发组自愈；其余失败仅记日志（下轮重试）。
func (c *eventTriggerConsumer) claimStale(ctx context.Context) error {
	claimed, _, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   domainshared.EventsStream,
		Group:    domainshared.EventsGroupFunctionsTriggers,
		Consumer: c.consumer,
		MinIdle:  eventClaimMinIdle,
		Start:    "0-0",
		Count:    eventReadGroupCount,
	}).Result()
	if err != nil {
		if strings.Contains(err.Error(), "NOGROUP") {
			return err
		}
		c.logger.Warn("event trigger xautoclaim failed", "error", err)
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
	if err := c.rdb.XAck(ctx, domainshared.EventsStream, domainshared.EventsGroupFunctionsTriggers, ackIDs...).Err(); err != nil {
		c.logger.Error("event trigger xack (claim) failed", "count", len(ackIDs), "error", err)
	} else if maxSeq > 0 {
		c.advanceWatermark(ctx, maxSeq)
	}
	return nil
}

// processEntry 处理单条 Stream 条目：解码信封 → 匹配投递。返回条目 seq
// （毒消息/未知序返回 0，不参与水位推进）。毒消息（空载荷/解码失败）直接
// 丢弃（ACK 不重投——对齐 realtime subscriber 口径）。
func (c *eventTriggerConsumer) processEntry(ctx context.Context, msg *redis.XMessage) int64 {
	raw, _ := msg.Values["payload"].(string)
	if raw == "" {
		c.logger.Error("event trigger stream empty payload", "stream_id", msg.ID)
		return 0
	}
	ev, err := infraevents.UnmarshalEnvelope([]byte(raw))
	if err != nil {
		c.logger.Error("event trigger stream malformed payload", "stream_id", msg.ID, "error", err)
		return 0
	}
	c.deliver(ctx, &ev)
	return ev.Seq
}

// deliver 匹配并投递单个信封（命中触发器逐条异步入队）。经济事件（v3
// §5.1）/未知事件形态/无命中均静默通过（照常推进水位；no_match 不记投递
// 指标——OQ9：只记命中的）。enqueue 失败时条目仍会被调用方 ACK：该事件
// 对本订阅者的投递延迟至停机补投路径（不再是永久丢失，v3 §4.2）。
func (c *eventTriggerConsumer) deliver(ctx context.Context, ev *domainevents.Envelope) {
	if ev.IsEconomy() {
		return
	}
	op := domainfunctions.EventOpFromEnvelope(ev.Event)
	if op == "" {
		return
	}
	subs := c.index.Load().Match(ev.ProjectID, ev.DatabaseID, ev.CollectionID, op)
	if len(subs) == 0 {
		return
	}
	// data 投影（§4.2）：ID + 摘要进 data，全量按 document_id 用
	// databases:read 回读——execution principal + RLS 链路原样，事件触发
	// 不豁免任何权限检查（v3 Security #4）。全部命中触发器共享同一份投影。
	data, err := domainfunctions.BuildEventInvocationData(ev)
	if err != nil {
		c.logger.Error("event invocation data marshal failed", "event_id", ev.EventID, "error", err)
		return
	}
	for _, s := range subs {
		_, ierr := c.functions.InvokeTrigger(ctx, appfunctions.InvokeTriggerCommand{
			ProjectID:  s.ProjectID,
			FunctionID: s.FunctionID,
			Data:       data,
			Async:      true,
			Source:     domainfunctions.TriggerTypeEvent + ":" + s.TriggerID,
			// 投影预算即 data 上限：恒 32KB（v1 env 通道同样收在 32KB；
			// 超预算只可能来自函数 variables 挤占 env 合并预算 → 400
			// enqueue_error，风暴由异步通道既有信号量兜底）。
			BodyLimitBytes: domainfunctions.EventDataBudgetBytes,
		})
		if ierr != nil {
			eventDeliveriesTotal.WithLabelValues(s.ProjectID, s.FunctionID, "enqueue_error").Inc()
			appfunctions.ObserveInvoke(s.ProjectID, s.FunctionID, domainfunctions.TriggerTypeEvent, appfunctions.InvokeResultError)
			c.logger.Warn("event trigger enqueue failed", "project_id", s.ProjectID,
				"function_id", s.FunctionID, "trigger_id", s.TriggerID, "event_id", ev.EventID, "error", ierr)
			continue
		}
		eventDeliveriesTotal.WithLabelValues(s.ProjectID, s.FunctionID, "ok").Inc()
		appfunctions.ObserveInvoke(s.ProjectID, s.FunctionID, domainfunctions.TriggerTypeEvent, appfunctions.InvokeResultOK)
	}
}

// ——停机补投（D12 对抗审查升格，一期必做）——

// backfill 检测停机裁剪缺口并从 outbox 表补投：
//  1. 读自管水位 W（torchwood:fnevent:lastseq，每批 ACK 后推进）；
//  2. 读 Stream 现存首条的信封 seq F（XADD 条目 ID 自动生成、不含 seq
//     语义，故从首条 payload 读起）；
//  3. F > W+1 → (W, F] 之间存在被 XTRIM 裁掉、消费组从未投递的条目 →
//     从 outbox 按 seq ∈ (W, F] 分批补投（上界收在 Stream 现存首条——
//     F 本身仍在 Stream 中，正常消费会再投一次，重叠由函数幂等吸收）。
//
// 诚实边界（§12.5 文档明示）：outbox 行 24h 清理窗口之外的极端停机
// （>24h）区间行已被清理，才真正丢失（与 WS 订阅 EVENTS.RESUME_EXPIRED
// 同一口径）。**补投发生即告警锚点**：eventBackfillTotal 非零 = 存在
// 停机窗口（本函数同时打 Warn 日志）。
func (c *eventTriggerConsumer) backfill(ctx context.Context) {
	watermark := c.getWatermark(ctx)
	first := c.streamFirstSeq(ctx)
	if first <= 0 || first <= watermark+1 {
		return
	}
	c.logger.Warn("event trigger delivery gap detected; backfilling from outbox",
		"watermark", watermark, "stream_first_seq", first,
		"hint", "worker downtime exceeded the stream trim window (functions-v3 §4.2/D12)")

	cursor := watermark
	for cursor < first && ctx.Err() == nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventBackfillTimeout)
		rows, err := infraevents.ScanOutboxSeqRange(sctx, c.db, cursor, first, eventBackfillBatch)
		cancel()
		if err != nil {
			c.logger.Error("event trigger backfill scan failed", "cursor", cursor, "error", err)
			return // 下次启动重试（水位未推进，缺口仍在）
		}
		if len(rows) == 0 {
			break // 区间行已被 24h 清理裁掉（诚实边界）：尽力而为
		}
		maxSeq := cursor
		for i := range rows {
			row := &rows[i]
			delivered := false
			if ev, err := infraevents.UnmarshalEnvelope(row.Payload); err != nil {
				c.logger.Error("event trigger backfill payload malformed", "event_id", row.EventID, "error", err)
			} else {
				ev.Seq = row.Seq
				c.deliver(ctx, &ev)
				delivered = true
			}
			result := "ok"
			if !delivered {
				result = "error"
			}
			eventBackfillTotal.WithLabelValues(row.ProjectID, result).Inc()
			if row.Seq > maxSeq {
				maxSeq = row.Seq
			}
		}
		c.advanceWatermark(ctx, maxSeq)
		cursor = maxSeq
	}
}

// getWatermark 读自管消费水位（键缺失 = 0：从未消费过 → 全区间按缺口
// 处理——首次部署对存量 Stream 的一次性全量核对，无订阅时零投递纯扫描）。
func (c *eventTriggerConsumer) getWatermark(ctx context.Context) int64 {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	v, err := c.rdb.Get(wctx, domainshared.FunctionsEventLastSeqKey).Result()
	if err != nil {
		return 0 // redis.Nil（缺失）或瞬断：按 0 起步，advance 单调纠偏
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// advanceWatermark 原子推进消费水位（Lua 单调：仅新值更大才写）。
func (c *eventTriggerConsumer) advanceWatermark(ctx context.Context, seq int64) {
	if seq <= 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := c.watermarkScript.Run(wctx, c.rdb, []string{domainshared.FunctionsEventLastSeqKey}, seq).Err(); err != nil {
		c.logger.Warn("event trigger watermark advance failed", "seq", seq, "error", err)
	}
}

// streamFirstSeq 读 Stream 现存首条的信封 seq（XRANGE 头部一条）；Stream
// 为空或条目 seq 未知（0）返回 0——无从判定缺口，跳过补投。
func (c *eventTriggerConsumer) streamFirstSeq(ctx context.Context) int64 {
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
