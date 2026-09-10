package shared

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/events"
)

// QueueFunctionsExecutions 是函数异步执行的队列名（Redis Stream，至少一次）。
const QueueFunctionsExecutions = "torchwood:queue:functions-executions"

// 事件脊柱 Stream 与函数事件触发器消费（v2 阶段④ §4.5 + functions-v3 §4.2）。
const (
	// EventsStream 是文档写事件投递的 Redis Stream：worker outbox 领取后
	// XADD 完整信封 JSON（含 acl + seq）；server 每实例一消费组 + 函数事件
	// 触发器消费组（见下）各自 XREADGROUP 消费全量。
	EventsStream = "torchwood:events"
	// EventsGroupFunctionsTriggers 是函数事件触发器的消费组名（v3 切片 D）：
	// 独立于 server 实例组——outbox 主投递路径（WS 扇出）零侵入，XACK 在
	// 入队成功后。
	EventsGroupFunctionsTriggers = "functions-triggers"
	// FunctionsEventLastSeqKey 是函数事件触发的自管消费水位（Redis 键）：
	// 每批 XACK 后推进（单调不回退）。Stream 条目 ID 是 XADD 自动生成
	// （不含 seq 语义），停机补投的 gap 判定依赖该水位而非消费组
	// last-delivered-id（v3 §4.2/D12 对抗审查修正）。
	FunctionsEventLastSeqKey = "torchwood:fnevent:lastseq"
)

// Queue 是异步任务队列端口（A7 修复：至少一次）。
// Dequeue 返回的 ack Token 需在处理成功后 Ack，否则消息在 PEL/inflight 超时后重投。
type Queue interface {
	Enqueue(ctx context.Context, queue string, payload []byte) error
	// Dequeue 阻塞等待任务；timeout<=0 时阻塞直到有任务或 ctx 取消。
	// 无任务时 payload==nil 且 ack=="".
	Dequeue(ctx context.Context, queue string, timeout time.Duration) (payload []byte, ack string, err error)
	Ack(ctx context.Context, queue string, ack string) error
	// Trim 近似裁剪 stream 到 maxLen 以内（XTRIM MAXLEN ~，P1-15：未裁剪
	// 的 at-least-once stream 内存单调增长）。周期性低频调用即可。
	Trim(ctx context.Context, queue string, maxLen int64) error
}

// EventPublisher 是用户集合文档写事件的 transactional outbox 端口
// （v2 设计 §3.2）。调用方应在 uow.Run 内 Publish，与业务写同一工作单元；
// 实现可从 ctx 读取连接。未在工作单元内则自行短事务插入。
type EventPublisher interface {
	Publish(ctx context.Context, ev events.Envelope) error
}

// RealtimeTransport 是 outbox → server Hub 的最后一跳（阶段④ B3：Redis
// Stream）。实现：XADD torchwood:events（完整信封 JSON，含 seq）；每个
// server 实例一个消费组（组名 = 实例 ID）各自消费全量。worker 进程只负责
// Enqueue 与周期 Trim，不持有 WebSocket。
type RealtimeTransport interface {
	Enqueue(ctx context.Context, ev events.Envelope) error
	// Trim 周期裁剪投递 Stream（XTRIM MAXLEN ~）：Stream 只是投递通道，
	// 重放窗口在 outbox 表（published 24h 清理 >> 1h 重放承诺）。
	Trim(ctx context.Context) error
}

// RealtimeSendBuffer 是每连接发送缓冲（帧数，阶段④水位断开）：慢客户端
// 落后超过该水位即带因断开（resync + last_seq），不再丢帧续命。
const RealtimeSendBuffer = 1024

// RealtimeFanout 仅存在于 cmd/server 进程：消费事件 Stream（每实例一
// 消费组）并写入 Hub（Hub.Dispatch → 标 outbox published_at）。
type RealtimeFanout interface {
	Run(ctx context.Context) error
}

// RealtimeConn 是 Hub 侧的连接句柄。Send 只承载出站帧
// （{"type":"event","channel":...,"payload":ClientPayload()}，无 acl）。
// TrySend 是唯一入队路径：满水位触发 OnSlow（resync 断开）而非丢帧。
type RealtimeConn struct {
	ID            string
	PlatformAdmin bool
	DocPrincipal  databases.Principal
	// ProjectID 是连接归属项目（B-1 隔离锚点）：WS 握手已校验
	// hello.project_id 与凭证一致（admin 绑定 + end_user 等值断言），此处
	// 即信任锚。Hub 对文档事件按 ev.ProjectID == ProjectID 等值过滤扇出
	//（频道名/ topic 不含 project 维度，隔离由消费端 project 过滤保证）。
	// 空串 fail-closed：不投递任何文档事件。
	ProjectID string
	Send      chan map[string]any

	// lastSeq 是该连接最后成功入队帧的 seq（单调上抬，resync close
	// reason 的数据源；redispatch 重投旧事件不会使游标回退）。
	lastSeq atomic.Int64
	// slowClosed 置位后连接视为已断开（后续入队直接失败）。
	slowClosed atomic.Bool
	// OnSlow 由 handler 侧注入：满水位时被调用恰一次（参数 = lastSeq），
	// 实现必须非阻塞。nil 时 TrySend 退化为旧丢帧语义（测试桩兼容）。
	OnSlow func(lastSeq int64)
}

// TrySend 非阻塞入队一帧；成功入队后才上抬 lastSeq 游标（断开 reason 的
// seq 必须是「确实已入队」的最后一帧——失败帧不计入，否则客户端从该
// seq 续传会漏掉未投递帧）。返回 false 表示连接已慢断开（或本次触发
// 断开）。OnSlow 为 nil（旧语义）时满载丢帧并返回 true——调用方以返回
// 值区分「已入队」与「断开」，丢帧路径由调用方自行计数。
func (c *RealtimeConn) TrySend(frame map[string]any, seq int64) bool {
	if c.slowClosed.Load() {
		return false
	}
	select {
	case c.Send <- frame:
		c.RaiseLastSeq(seq)
		return true
	default:
		if c.OnSlow != nil {
			if c.slowClosed.CompareAndSwap(false, true) {
				c.OnSlow(c.lastSeq.Load())
			}
			return false
		}
		// 旧语义（无 OnSlow）：丢帧续命，连接保持。
		return true
	}
}

// SlowClosed 报告连接是否已被慢水位断开。
func (c *RealtimeConn) SlowClosed() bool { return c.slowClosed.Load() }

// LastSeq 返回连接游标（最后入队帧的 seq）。
func (c *RealtimeConn) LastSeq() int64 { return c.lastSeq.Load() }

// RaiseLastSeq 单调上抬游标（门控积压路径使用：帧尚未入 Send 但已确定
// 会刷入，断开 reason 应覆盖）。
func (c *RealtimeConn) RaiseLastSeq(seq int64) {
	if seq <= 0 {
		return
	}
	for {
		cur := c.lastSeq.Load()
		if seq <= cur || c.lastSeq.CompareAndSwap(cur, seq) {
			return
		}
	}
}

// RealtimeHub 是 server 进程内订阅表端口（v2 设计 §4.5）：WS handler
// 经它注册/摘除订阅，RealtimeFanout 实现侧按频道 + ACL 快照扇出。
// 只存在于 cmd/server 进程。
type RealtimeHub interface {
	// Subscribe 把 conn 注册到频道（幂等：同一连接重复订阅不重复计数）。
	Subscribe(channel string, conn *RealtimeConn)
	// Unsubscribe 把 conn 从单个频道移除。
	Unsubscribe(channel, connID string)
	// Remove 把 conn 从全部频道移除（连接关闭时调用）。
	Remove(connID string)
	// BeginReplay 把连接置为重放门控态（阶段④ last_seq）：Dispatch 到该
	// 连接的帧积压在 backlog，不入 Send。必须在 Subscribe 之前调用——
	// 保证「门控 → 订阅 → 查 outbox 补发」之间无漏帧窗口。
	BeginReplay(conn *RealtimeConn)
	// EndReplay 结束门控：调用方已把补发帧写入 Send 后，把 backlog 中
	// 未见（seen 为补发批的 event_id 集）的帧按序刷入，连接回到实时态。
	EndReplay(conn *RealtimeConn, seen map[string]struct{})
}
