package realtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
)

// economyFrame 从连接缓冲收一帧并断言频道。
func economyFrame(t *testing.T, conn *Conn, wantChannel string) map[string]any {
	t.Helper()
	select {
	case frame := <-conn.Send:
		require.Equal(t, "event", frame["type"])
		require.Equal(t, wantChannel, frame["channel"])
		return frame["payload"].(map[string]any)
	case <-time.After(time.Second):
		t.Fatal("expected economy frame on accounts channel")
		return nil
	}
}

// TestHubDispatchEconomyChannel 验证经济事件（D17）只扇出显式 Channel
// （accounts.{userId}），无 acl 过滤、帧含 domain 字段；未订阅该频道的
// 连接不收任何帧。
func TestHubDispatchEconomyChannel(t *testing.T) {
	hub := NewHub(nil)
	owner := &Conn{ID: "c1", Send: make(chan map[string]any, 4)}
	other := &Conn{ID: "c2", Send: make(chan map[string]any, 4)}
	// DocPrincipal 留空：经济事件不得走文档 ACL（空 ACL 会全拒）。
	hub.Subscribe("accounts.u1", owner)
	hub.Subscribe("accounts.u2", other)

	hub.Dispatch(events.Envelope{
		EventID:   "evt_e1",
		Event:     "payments.orders.paid",
		ProjectID: "p1",
		Domain:    "payments",
		Channel:   "accounts.u1",
		CreatedAt: time.Now(),
		Attrs: map[string]any{
			"order_id": "o1",
			"amount":   int64(1999),
			"currency": "USD",
		},
	})

	payload := economyFrame(t, owner, "accounts.u1")
	require.Equal(t, "payments", payload["domain"])
	require.Equal(t, "payments.orders.paid", payload["event"])
	require.Equal(t, "o1", payload["order_id"])
	require.Equal(t, int64(1999), payload["amount"])
	select {
	case frame := <-other.Send:
		t.Fatalf("other user channel must not receive event, got %v", frame)
	default:
	}

	// 重复 event_id 不二次扇出（去重窗）。
	hub.Dispatch(events.Envelope{
		EventID: "evt_e1", Event: "payments.orders.paid", ProjectID: "p1",
		Domain: "payments", Channel: "accounts.u1", CreatedAt: time.Now(),
	})
	select {
	case frame := <-owner.Send:
		t.Fatalf("duplicate event_id must be deduped, got %v", frame)
	default:
	}
	hub.Remove(owner.ID)
	hub.Remove(other.ID)
}

// TestHubDispatchDocumentUnchangedWithEconomyFields 验证文档事件扇出
// 行为不因经济扩展改变：仍按集合 + 文档双频道、走 ACL 过滤。
func TestHubDispatchDocumentUnchangedWithEconomyFields(t *testing.T) {
	hub := NewHub(nil)
	conn := &Conn{
		ID: "c1",
		// B-1 项目隔离：文档事件按连接归属项目等值过滤，须与事件一致。
		ProjectID:    "p1",
		DocPrincipal: shared.RealtimeConn{}.DocPrincipal, // zero value ok
		Send:         make(chan map[string]any, 4),
	}
	hub.Subscribe("databases.db1.collections.coll1", conn)

	// PlatformAdmin 旁路：空 ACL 也可见（v2 语义）。
	conn.PlatformAdmin = true
	hub.Dispatch(events.Envelope{
		EventID:      "evt_d1",
		Event:        events.EventDocumentsCreate,
		ProjectID:    "p1",
		DatabaseID:   "db1",
		CollectionID: "coll1",
		DocumentID:   "doc1",
		Version:      1,
		CreatedAt:    time.Now(),
	})
	payload := economyFrame(t, conn, "databases.db1.collections.coll1")
	require.NotContains(t, payload, "domain")
	require.NotContains(t, payload, "channel")
	hub.Remove(conn.ID)
}

// TestHubDispatch_DedupWindowSlidesOnHit (P1-13)：去重窗口随命中滑动——
// 同一 event 在窗口边缘被 redispatch 重发时不得重新扇出（此前窗口从首见
// 起算，标记持续失败 >dedupWindow 后客户端会收到可见重复帧）。
//
// J6-4：用顺序定时器替代固定 sleep。Go 定时器只会迟到不会早到，且第二个
// 定时器在第一个触发后才启动，因此两次派发的「间隔」精确成立、与负载无关。
// 断言只依赖间隔关系（窗口 W=3s）：命中距首见 2s<W；再派发距上次命中
// 2s<W（须仍去重）；距首见合计 4s>W（无滑动则早已过期）——两侧各留 ≥1s
// 构造性余量。
func TestHubDispatch_DedupWindowSlidesOnHit(t *testing.T) {
	orig := dedupWindow
	dedupWindow = 3 * time.Second
	defer func() { dedupWindow = orig }()

	hub := NewHub(nil)
	conn := &Conn{ID: "c1", Send: make(chan map[string]any, 4)}
	hub.Subscribe("accounts.u1", conn)
	defer hub.Remove(conn.ID)

	env := events.Envelope{
		EventID: "evt_slide", Event: "payments.orders.paid", ProjectID: "p1",
		Domain: "payments", Channel: "accounts.u1", CreatedAt: time.Now(),
	}
	hub.Dispatch(env) // 首见 t0
	select {
	case <-conn.Send:
	default:
		t.Fatal("首见事件必须扇出")
	}

	hit := time.NewTimer(2 * time.Second)
	defer hit.Stop()
	<-hit.C
	hub.Dispatch(env) // t0+2s 命中（刷新窗口）

	slide := time.NewTimer(2 * time.Second)
	defer slide.Stop()
	<-slide.C
	hub.Dispatch(env) // t0+4s：距首见已超窗，但距上次命中仅 2s——必须仍去重
	select {
	case frame := <-conn.Send:
		t.Fatalf("窗口滑动后不得重复扇出, got %v", frame)
	default:
	}
}
