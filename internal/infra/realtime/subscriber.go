package realtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	infraevents "github.com/torchwooddev/torchwood/internal/infra/events"
	"github.com/uptrace/bun"
)

const (
	// maxRedisBackoff 是 Redis 断线重试退避上限。
	maxRedisBackoff = 30 * time.Second

	// readGroupCount 是单次 XREADGROUP 的条数上限（批内先全部 Dispatch
	// 再统一 XACK，减少往返）。
	readGroupCount = 64
	// readGroupBlock 是 XREADGROUP 无消息时的阻塞时长；也即 ctx 取消的
	// 最大响应延迟。
	readGroupBlock = 200 * time.Millisecond

	// publishedAtFlushInterval 是 published_at 标记的攒批窗口（Round4 J5-5）：
	// 每事件每副本单行 UPDATE 写放大，攒批为
	// UPDATE ... WHERE event_id IN (...)，200ms 或 32 条先到者触发。
	publishedAtFlushInterval = 200 * time.Millisecond
	// publishedAtBatchSize 是单批 event_id 上限（与 outbox 领取批量同量级）。
	publishedAtBatchSize = 32
	// publishedAtQueueLen 是标记队列容量；打满时丢弃该条标记（事件已扇出，
	// 后果仅是该行延迟到 redispatch 窗口被重投，at-least-once 语义不变）。
	publishedAtQueueLen = 1024
	// publishedAtFlushTimeout 是每批 UPDATE 的独立超时。
	publishedAtFlushTimeout = 5 * time.Second
)

// claimMinIdle 是 PEL 认领的最小 idle：消费（Dispatch 入内存 chan）是
// 毫秒级，卡超过该窗口的 PEL 条目只能来自进程崩溃/挂死，由下一个消费
// 循环 XAUTOCLAIM 重投。重投重复由 Hub 的 event_id 去重窗口 + 客户端
// 幂等去重（at-least-once 契约）兜底。var 而非 const：测试覆写缩短窗口
// 验证认领语义（同 queue.claimMinIdle 先例）。
var claimMinIdle = 15 * time.Minute

// Subscriber 是 server 进程的 Stream 消费者（阶段④ B3：每实例一消费组）：
//
//	worker: XADD torchwood:events（完整信封 JSON，含 seq）
//	Subscriber: XGROUP <instance> → XREADGROUP → Hub.Dispatch → 批量 XACK
//	          → markLoop 攒批 UPDATE outbox SET published_at
//
// WHY 每实例一个消费组（B3 决议）：组间互不影响各自消费全量，替代 A6 的
// Pub/Sub 广播；XACK 为主位点，published_at 回写统一清理与 :changes 数据源。
// 组名 = 实例 ID（hostname:pid）：同机多进程不共组（共组会把消息分给单个
// 消费者导致在线副本静默）；重启即新组、从 "$" 起步——宕机窗口的补齐由
// 客户端断线重连带 last_seq 的 outbox 重放承担（正确性不在 Stream 层）。
type Subscriber struct {
	client *redis.Client
	db     *clients.Database
	hub    *Hub
	logger *slog.Logger

	// instance 是实例 ID = 消费组名（唯一进程标识）。
	instance string

	// markCh 是 published_at 标记队列：processMessage 入队，
	// markLoop 后台攒批落库（Round4 J5-5）。
	markCh chan string
}

// NewRealtimeSubscriber 构造 Stream 消费端。instance 为 hostname:pid，
// 兼作消费组名与消费者名。
func NewRealtimeSubscriber(client *redis.Client, db *clients.Database, hub *Hub, logger *slog.Logger) *Subscriber {
	if logger == nil {
		logger = slog.Default()
	}
	hostname, _ := os.Hostname()
	return &Subscriber{
		client:   client,
		db:       db,
		hub:      hub,
		logger:   logger,
		instance: fmt.Sprintf("%s:%d", hostname, os.Getpid()),
		markCh:   make(chan string, publishedAtQueueLen),
	}
}

// Run 阻塞消费循环；ctx 取消后返回。断线时指数退避重连，不退出进程。
// 另起 goroutine 运行 markLoop（published_at 攒批），返回前等待其排空退出。
func (s *Subscriber) Run(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.markLoop()
	}()
	defer func() {
		close(s.markCh) // 停止入队并让 markLoop 排空后退出
		<-done
	}()

	backoff := time.Second
	for {
		if err := s.consumeLoop(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.logger.Error("realtime consume loop failed", "error", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			backoff *= 2
			if backoff > maxRedisBackoff {
				backoff = maxRedisBackoff
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		backoff = time.Second
	}
}

// consumeLoop 单次消费会话：确保消费组 → XREADGROUP 消费直至 ctx 取消
// 或连接丢失。每轮先 XAUTOCLAIM 回收卡死 PEL（崩溃恢复），再读新消息。
func (s *Subscriber) consumeLoop(ctx context.Context) error {
	if err := s.ensureGroup(ctx); err != nil {
		return err
	}
	for {
		if err := s.claimStale(ctx); err != nil {
			return err
		}
		streams, err := s.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    s.instance,
			Consumer: s.instance,
			Streams:  []string{eventsStream, ">"},
			Count:    readGroupCount,
			Block:    readGroupBlock,
		}).Result()
		if err != nil {
			if err == redis.Nil { // BLOCK 超时：正常心跳，继续（先查 ctx）
				if ctx.Err() != nil {
					return nil
				}
				continue
			}
			return err
		}
		for _, st := range streams {
			// 批内逐条 Dispatch（入内存 chan，毫秒级），随后统一 XACK。
			var ackIDs []string
			for i := range st.Messages {
				if s.processMessage(st.Messages[i]) {
					ackIDs = append(ackIDs, st.Messages[i].ID)
				}
			}
			if len(ackIDs) > 0 {
				if err := s.client.XAck(ctx, eventsStream, s.instance, ackIDs...).Err(); err != nil {
					// 扇出已完成：留在 PEL 由 claimMinIdle 窗口后重投，
					// Hub 去重 + 客户端 event_id 幂等吸收重复。
					s.logger.Warn("realtime xack failed; entries stay in PEL",
						"count", len(ackIDs), "error", err)
				}
			}
		}
	}
}

// ensureGroup 建立本实例的消费组（XGROUP MKSTREAM）。新组从 "$" 起步：
// 新实例此刻尚无客户端，历史条目对它无意义——客户端各自带 last_seq 从
// outbox 重放补齐（正确性不在 Stream 层）。BUSYGROUP = 组已存在，容忍。
func (s *Subscriber) ensureGroup(ctx context.Context) error {
	err := s.client.XGroupCreateMkStream(ctx, eventsStream, s.instance, "$").Err()
	if err == nil || strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

// claimStale 回收本组 PEL 中 idle 超过 claimMinIdle 的条目（进程崩溃/挂死
// 后的 at-least-once 重投），按普通消息处理。
func (s *Subscriber) claimStale(ctx context.Context) error {
	claimed, _, err := s.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   eventsStream,
		Group:    s.instance,
		Consumer: s.instance,
		MinIdle:  claimMinIdle,
		Start:    "0-0",
		Count:    readGroupCount,
	}).Result()
	if err != nil {
		return err
	}
	var ackIDs []string
	for i := range claimed {
		if s.processMessage(claimed[i]) {
			ackIDs = append(ackIDs, claimed[i].ID)
		}
	}
	if len(ackIDs) > 0 {
		if err := s.client.XAck(ctx, eventsStream, s.instance, ackIDs...).Err(); err != nil {
			s.logger.Warn("realtime xack (claim) failed", "count", len(ackIDs), "error", err)
		}
	}
	return nil
}

// processMessage 解码完整信封（含 acl + seq）→ Hub.Dispatch → 入队
// published_at 标记（由 markLoop 攒批落库）。返回 false 的消息（空载荷/
// 解码失败）由调用方直接丢弃（XACK），不重投。队列满载时丢弃标记并告警：
// 事件本身已完成扇出，丢标的后果是该 outbox 行在 redispatch 窗口后被重投
// （at-least-once），不丢事件。
func (s *Subscriber) processMessage(msg redis.XMessage) bool {
	raw, _ := msg.Values["payload"].(string)
	if raw == "" {
		s.logger.Error("realtime stream empty payload", "stream_id", msg.ID)
		return true // 毒消息：丢弃，不重投
	}
	ev, err := infraevents.UnmarshalEnvelope([]byte(raw))
	if err != nil {
		s.logger.Error("realtime stream malformed", "stream_id", msg.ID, "error", err)
		return true // 毒消息：丢弃，不重投
	}
	s.hub.Dispatch(ev)
	select {
	case s.markCh <- ev.EventID:
	default:
		s.logger.Warn("outbox published marker queue full; dropping marker",
			"event_id", ev.EventID)
	}
	return true
}

// markLoop 消费标记队列，按「200ms 或 32 条」批量把 published_at 落库。
// 使用独立 Background ctx + 单批超时：关停排空阶段父 ctx 可能已取消，
// 但最后一批标记仍应尽力写入。失败仅告警（行会被 redispatch 兜底）。
func (s *Subscriber) markLoop() {
	pending := make([]string, 0, publishedAtBatchSize)
	ticker := time.NewTicker(publishedAtFlushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(pending) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), publishedAtFlushTimeout)
		_, err := s.db.Conn(ctx).NewUpdate().Model((*model.DocumentEventsOutbox)(nil)).
			Set("published_at = NOW()").
			Where("event_id IN (?)", bun.List(pending)).Exec(ctx)
		cancel()
		if err != nil {
			s.logger.Error("mark outbox published failed",
				"events", len(pending), "error", err)
		}
		pending = pending[:0]
	}

	for {
		select {
		case id, ok := <-s.markCh:
			if !ok {
				flush()
				return
			}
			pending = append(pending, id)
			if len(pending) >= publishedAtBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

var _ shared.RealtimeFanout = (*Subscriber)(nil)
