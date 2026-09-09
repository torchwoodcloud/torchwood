package realtime

import (
	"log/slog"
	"sync"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
)

// dedupWindow 是 Hub 按 event_id 去重的时间窗（回收重放 / 崩溃重投
// 的重复条目不再二次扇出；窗口覆盖 2min 兜底 + 30s idle 认领余量）。
// var 而非 const：测试覆写缩短窗口验证刷新语义。
var dedupWindow = 5 * time.Minute

// dedupMax 是 event_id 去重表上限；超限先清理过期条目。
const dedupMax = 4096

// Conn 是 domain 端口 RealtimeConn 的别名（Hub 实现 shared.RealtimeHub
// 端口，连接句柄定义在 domain 层供 api 层握手代码使用）。
type Conn = shared.RealtimeConn

// replayGate 是单连接的 last_seq 重放门控状态（阶段④ §4.5）：
// 门控期间 Dispatch 到该连接的帧积压在 backlog（保序），EndReplay 时
// 去重（补发批已含的 event_id）后按序刷入 Send。
type replayGate struct {
	mu      sync.Mutex
	active  bool
	backlog []gatedFrame
}

type gatedFrame struct {
	eventID string
	frame   map[string]any
	seq     int64
}

// Hub 是单进程内存订阅表：map[channel]map[connID]*Conn（v2 设计
// §4.5）。只存在于 cmd/server 进程。
type Hub struct {
	logger *slog.Logger

	mu       sync.RWMutex
	channels map[string]map[string]*Conn
	subCount int
	dedup    map[string]time.Time

	gateMu sync.Mutex
	gates  map[string]*replayGate
}

// NewHub 构造 Hub。logger 可为 nil（回退 slog.Default）。
func NewHub(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		logger:   logger,
		channels: make(map[string]map[string]*Conn),
		dedup:    make(map[string]time.Time),
		gates:    make(map[string]*replayGate),
	}
}

// Subscribe 把 conn 注册到频道（幂等：同一连接重复订阅不重复计数）。
func (h *Hub) Subscribe(channel string, conn *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.channels[channel]
	if m == nil {
		m = make(map[string]*Conn)
		h.channels[channel] = m
	}
	if _, ok := m[conn.ID]; !ok {
		h.subCount++
	}
	m[conn.ID] = conn
	RealtimeSubscriptions.Set(float64(h.subCount))
}

// Unsubscribe 把 conn 从单个频道移除。
func (h *Hub) Unsubscribe(channel, connID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.channels[channel]
	if m == nil {
		return
	}
	if _, ok := m[connID]; ok {
		delete(m, connID)
		h.subCount--
	}
	if len(m) == 0 {
		delete(h.channels, channel)
	}
	RealtimeSubscriptions.Set(float64(h.subCount))
}

// Remove 把 conn 从全部频道移除（连接关闭时调用），并清理其门控状态。
func (h *Hub) Remove(connID string) {
	h.mu.Lock()
	for ch, m := range h.channels {
		if _, ok := m[connID]; ok {
			delete(m, connID)
			h.subCount--
		}
		if len(m) == 0 {
			delete(h.channels, ch)
		}
	}
	RealtimeSubscriptions.Set(float64(h.subCount))
	h.mu.Unlock()

	h.gateMu.Lock()
	delete(h.gates, connID)
	h.gateMu.Unlock()
}

// BeginReplay 置连接为门控态（幂等；必须先于 Subscribe 调用）。
func (h *Hub) BeginReplay(conn *Conn) {
	h.gateMu.Lock()
	defer h.gateMu.Unlock()
	g := h.gates[conn.ID]
	if g == nil {
		g = &replayGate{active: true}
		h.gates[conn.ID] = g
		return
	}
	g.mu.Lock()
	g.active = true
	g.mu.Unlock()
}

// EndReplay 结束门控并刷入 backlog：seen 为补发批已含的 event_id 集
// （重复跳过），其余按积压序入 Send；满水位走 TrySend 的慢断开路径。
func (h *Hub) EndReplay(conn *Conn, seen map[string]struct{}) {
	h.gateMu.Lock()
	g := h.gates[conn.ID]
	if g != nil {
		delete(h.gates, conn.ID)
	}
	h.gateMu.Unlock()
	if g == nil {
		return
	}
	g.mu.Lock()
	g.active = false
	backlog := g.backlog
	g.backlog = nil
	g.mu.Unlock()

	for _, f := range backlog {
		if _, dup := seen[f.eventID]; dup {
			continue
		}
		conn.TrySend(f.frame, f.seq)
	}
}

// Dispatch 收**完整**信封（含 ev.ACL），按集合 + 文档两个频道扇出。
// 连接只收 PlatformAdmin 旁路或 VisibleTo(ev.ACL) 通过的订阅者；
// 入队帧只用 ev.ClientPayload()（剥掉 acl）。入队统一走 TrySend：
// 满水位带因断开（resync + last_seq），不再丢帧（OnSlow 为 nil 的
// 测试桩退化为旧丢帧语义）。
// B-1（终局安全评审）：频道名/ topic 是全局命名空间（不含 project 维度），
// 跨项目同名集合订阅的是同一频道——文档事件必须先按连接归属项目等值
// 过滤（ev.ProjectID == c.ProjectID），不等直接跳过（在他项目事件上不做
// ACL 评估）。连接归属项目来自 WS 握手信任锚（hello.project_id 已与凭证
// 一致校验）；连接无项目归属（空串，生产握手路径不可达）时 fail-closed
// 不投递。经济事件（Domain 非空，v3 设计 §5.1/D17）不受此过滤：可见性由
// 显式频道本身保证（订阅侧 parseChannel 派发表仅允许本人订阅）。
// 门控中的连接：帧积压进 backlog（EndReplay 统一去重刷入）。
func (h *Hub) Dispatch(ev events.Envelope) {
	if !h.markSeen(ev.EventID) {
		return // 重复 event_id（回收重放 / PEL 重投）
	}
	payload := ev.ClientPayload()
	var channels []string
	if ev.IsEconomy() {
		channels = []string{ev.Channel}
	} else {
		channels = []string{ev.CollectionChannel(), ev.DocumentChannel()}
	}
	for _, ch := range channels {
		h.mu.RLock()
		subs := make([]*Conn, 0, 8)
		for _, c := range h.channels[ch] {
			subs = append(subs, c)
		}
		h.mu.RUnlock()
		for _, c := range subs {
			if !ev.IsEconomy() {
				// B-1 项目隔离先于 ACL 评估：他项目事件不进入本连接的
				// 可见性判定（fail-closed：空 ProjectID 连接一律跳过）。
				if ev.ProjectID != c.ProjectID {
					continue
				}
				if !c.PlatformAdmin && !events.VisibleTo(ev.ACL, c.DocPrincipal) {
					continue
				}
			}
			frame := map[string]any{"type": "event", "channel": ch, "payload": payload}
			if g := h.gateOf(c.ID); g != nil {
				g.mu.Lock()
				if g.active {
					g.backlog = append(g.backlog, gatedFrame{eventID: ev.EventID, frame: frame, seq: ev.Seq})
					g.mu.Unlock()
					// 门控积压同样上抬游标：断开 reason 覆盖已积压帧。
					c.RaiseLastSeq(ev.Seq)
					continue
				}
				g.mu.Unlock()
			}
			if c.TrySend(frame, ev.Seq) {
				RealtimeEventsDeliveredTotal.WithLabelValues(ev.Event).Inc()
			} else if c.SlowClosed() {
				RealtimeEventsDroppedTotal.WithLabelValues("slow_disconnect").Inc()
				h.logger.Warn("realtime slow consumer disconnected for resync",
					"event_id", ev.EventID, "connection_id", c.ID, "channel", ch,
					"last_seq", c.LastSeq())
			}
		}
	}
}

// gateOf 读取连接的门控状态（无锁读指针，active 判断在 g.mu 内）。
func (h *Hub) gateOf(connID string) *replayGate {
	h.gateMu.Lock()
	g := h.gates[connID]
	h.gateMu.Unlock()
	return g
}

// markSeen 记录 event_id 并返回该事件是否首次出现（去重窗口内）。
func (h *Hub) markSeen(eventID string) bool {
	if eventID == "" {
		return true
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.dedup[eventID]; ok {
		// 命中刷新时间戳（P1-13）：redispatch 每 2min 重发同一 event 时窗口
		// 随最新一次见到的时间滑动——否则窗口从首见起算，标记持续失败
		// >dedupWindow 后客户端会收到可见重复帧。
		h.dedup[eventID] = now
		return false
	}
	if len(h.dedup) >= dedupMax {
		for id, t := range h.dedup {
			if now.Sub(t) > dedupWindow {
				delete(h.dedup, id)
			}
		}
		if len(h.dedup) >= dedupMax {
			// 窗口内活跃事件超过上限时放弃登记新 ID（仍视为首次出现）：
			// 查重仍由已登记条目保证，内存不再无界增长。代价是极端洪峰下
			// 未登记的 event_id 重放时可能二次扇出，由客户端按 id 去重兜底
			//（v2 设计 at-least-once 语义本就要求客户端去重）。
			return true
		}
	}
	h.dedup[eventID] = now
	return true
}
