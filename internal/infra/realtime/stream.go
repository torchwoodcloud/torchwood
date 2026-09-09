package realtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
)

const (
	// eventsStream 是事件投递的 Redis Stream（阶段④ B3，§4.5）：
	// worker dispatcher XADD 完整信封 JSON（含 acl + seq），每个 server
	// 实例一个消费组（见 subscriber.go）XREADGROUP 消费后 XACK。
	// 回到 Stream 是有意决策：消费组模式同样解决多副本可见性，且换来
	// 不丢帧与位点回放（A6 的 Pub/Sub 广播在阶段④被取代）。
	eventsStream = "torchwood:events"
)

// eventsStreamMaxLen 是周期 XTRIM 水位（近似裁剪）：Stream 只是投递
// 通道，不承担重放（重放窗口在 outbox 表）；100k 条覆盖消费组最大
// 在途积压 + 实例重启追平窗口，内存有界。var 而非 const：测试覆写
// 缩小水位验证裁剪语义（同 hub.dedupWindow 先例）。
var eventsStreamMaxLen int64 = 100000

// streamTransport 是 shared.RealtimeTransport 的 Redis Stream 实现
// （阶段④：XADD 替代 PUBLISH）。
type streamTransport struct {
	client *redis.Client
}

// NewStreamTransport 构造 Stream 写入端。
func NewStreamTransport(client *redis.Client) shared.RealtimeTransport {
	return &streamTransport{client: client}
}

func (t *streamTransport) Enqueue(ctx context.Context, ev events.Envelope) error {
	payload, err := infraevents.MarshalEnvelope(ev)
	if err != nil {
		return err
	}
	// WHY: XADD 不带 MAXLEN——未投递消息不得被近似裁剪丢弃（积压治理
	// 交给 Trim 的低频周期调用，水位见 eventsStreamMaxLen）。
	return t.client.XAdd(ctx, &redis.XAddArgs{
		Stream: eventsStream,
		Values: map[string]any{"payload": string(payload)},
	}).Err()
}

func (t *streamTransport) Trim(ctx context.Context) error {
	return t.client.XTrimMaxLenApprox(ctx, eventsStream, eventsStreamMaxLen, 0).Err()
}

// sweepPendingScanLimit 是 PEL 核对的单组条目拉取上限：PEL 超过该值无法
// 穷举 min(idle)，保守保留该组（孤儿组的 PEL 只含实例崩溃时的在途条目，
// 量级极小，正常不会触达）。
const sweepPendingScanLimit = 1000

// SweepIdleGroups 销毁 events Stream 上的闲置孤儿消费组（B6，redesign §4.5
// 挂账）：组名 = 实例 ID（hostname:pid），实例崩溃/重启后旧组无人认领、
// 无限累积。判定（全部满足才销毁，保守优先）：
//   - 无活跃成员：组内全部 consumer 的 idle 超过 idle（无 consumer 视为
//     空真）；在线实例的组每 readGroupBlock XREADGROUP 一次，consumer
//     idle 永远贴零，天然不满足；
//   - PEL 已过时：PEL 为空，或全部条目的 idle 超过 idle（PEL 未超时说明
//     仍有人在途投递，保留）；
//   - 闲置：last-delivered-id 落后 Stream 头部超过 idle（按条目 ID 的 ms
//     时间戳差判定——Stream ID 即 Redis 服务器时钟；在线组的组位点贴头部，
//     时间差为条目生成间隔，毫秒级）。
//
// 返回被销毁的组名（调用方记日志/指标）。Stream 不存在（首次 XADD 前）
// 视为无可清理返回空。误删代价受控：组被销毁后消费端 XREADGROUP 报
// NOGROUP → 实例重连重建组，漏投递由 outbox 重放 + 客户端幂等兜底。
func (t *streamTransport) SweepIdleGroups(ctx context.Context, idle time.Duration) ([]string, error) {
	groups, err := t.client.XInfoGroups(ctx, eventsStream).Result()
	if err != nil {
		if streamMissing(err) {
			return nil, nil
		}
		return nil, err
	}
	head, err := t.streamHeadID(ctx)
	if err != nil {
		return nil, err
	}

	var destroyed []string
	for i := range groups {
		g := groups[i]
		if !idleBehindHead(g, head, idle) {
			continue
		}
		stale, err := t.groupIdle(ctx, g.Name, idle)
		if err != nil {
			return destroyed, err
		}
		if !stale {
			continue
		}
		n, err := t.client.XGroupDestroy(ctx, eventsStream, g.Name).Result()
		if err != nil {
			return destroyed, err
		}
		if n == 1 {
			destroyed = append(destroyed, g.Name)
		}
	}
	return destroyed, nil
}

// streamHeadID 返回 Stream 现存最后一条条目的 ID（XREVRANGE 头部一条）；
// Stream 为空返回 ""（无从判定位点落后，调用方跳过全部组）。
func (t *streamTransport) streamHeadID(ctx context.Context) (string, error) {
	entries, err := t.client.XRevRangeN(ctx, eventsStream, "+", "-", 1).Result()
	if err != nil {
		if streamMissing(err) { // 组列表与头部读取之间被清空/裁剪的竞态
			return "", nil
		}
		return "", err
	}
	if len(entries) == 0 {
		return "", nil
	}
	return entries[0].ID, nil
}

// groupIdle 核对组的成员与 PEL 是否全部过时：
//   - 任一 consumer 的 idle 未超过 idle → 活跃，保留（idle < 0 为"无交互
//     证据"——miniredis 对仅 XREADGROUP 过的 consumer 不维护 lastSeen 恒
//     报 -1，真实 Redis 恒 ≥0——不提供保护）；
//   - PEL 中任一条目的 idle 未超过 idle → 有人在途，保留（条目数超过
//     sweepPendingScanLimit 时无法穷举，保守保留）。
func (t *streamTransport) groupIdle(ctx context.Context, group string, idle time.Duration) (bool, error) {
	consumers, err := t.client.XInfoConsumers(ctx, eventsStream, group).Result()
	if err != nil {
		return false, err
	}
	for i := range consumers {
		if consumers[i].Idle >= 0 && consumers[i].Idle <= idle {
			return false, nil
		}
	}

	// PEL 非空时核对全部条目的 idle（孤儿组无人认领，条目 idle 单调增长）。
	pending, err := t.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: eventsStream,
		Group:  group,
		Start:  "-",
		End:    "+",
		Count:  sweepPendingScanLimit,
	}).Result()
	if err != nil {
		return false, err
	}
	for i := range pending {
		if pending[i].Idle <= idle {
			return false, nil
		}
	}
	return true, nil
}

// idleBehindHead 报告「无活跃成员 + 位点落后」两个廉价条件（XINFO GROUPS
// 单次调用即可判定）；成员/PEL 的 idle 核对交给 groupIdle。
func idleBehindHead(g redis.XInfoGroup, head string, idle time.Duration) bool {
	if head == "" {
		return false
	}
	headMs, _, ok := parseStreamIDMs(head)
	if !ok {
		return false
	}
	deliveredMs, _, ok := parseStreamIDMs(g.LastDeliveredID)
	if !ok {
		return false
	}
	return time.Duration(headMs-deliveredMs)*time.Millisecond > idle
}

// parseStreamIDMs 解析 Stream 条目 ID "<ms>-<seq>" 的毫秒时间戳部分。
// 容忍 "<ms>" 简写形态（seq 视为 0；miniredis 对 0 起步组即此形态，
// 真实 Redis 恒为完整形态）。
func parseStreamIDMs(id string) (ms, seq int64, ok bool) {
	msPart, seqPart, found := strings.Cut(id, "-")
	ms, err := strconv.ParseInt(msPart, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if !found {
		return ms, 0, true
	}
	seq, err = strconv.ParseInt(seqPart, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return ms, seq, true
}

// streamMissing 报告错误是否为「Stream key 不存在」（XINFO/XRANGE 对不存在
// key 报 ERR no such key，而非 redis.Nil；真实 Redis 与 miniredis 同形）。
func streamMissing(err error) bool {
	var re redis.Error // go-redis 的 Redis 服务端错误接口（errors.As 可穿透 wrapped）
	if errors.As(err, &re) {
		return strings.Contains(re.Error(), "no such key")
	}
	return false
}

var _ shared.RealtimeTransport = (*streamTransport)(nil)
