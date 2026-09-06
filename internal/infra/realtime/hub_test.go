package realtime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/events"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
)

func testEnvelope() events.Envelope {
	return events.Envelope{
		EventID:      "01J-test-event",
		Event:        events.EventDocumentsUpdate,
		ProjectID:    "default",
		DatabaseID:   "app",
		CollectionID: "posts",
		DocumentID:   "p1",
		Version:      2,
		CreatedAt:    time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
		Data: &databases.Document{
			ID:        "p1",
			Data:      map[string]any{"title": "t"},
			CreatedAt: time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
			Version:   2,
		},
		ACL: events.ACLSnapshot{
			DocumentSecurity: true,
			CollectionPermissions: []databases.Permission{
				{Type: "read", Role: "any"},
			},
			DocumentPermissions: []databases.Permission{
				{Type: "read", Role: "user:u1"},
			},
			DocHasPerms: true,
		},
	}
}

func newTestConn(id string, principal databases.Principal) *Conn {
	// ProjectID 默认 "default"（与 testEnvelope 的 ProjectID 一致）：B-1 起
	// Hub 对文档事件按连接归属项目等值过滤；跨项目场景在 hub_isolation_test
	// 显式覆盖。
	return &Conn{ID: id, ProjectID: "default", DocPrincipal: principal, Send: make(chan map[string]any, shared.RealtimeSendBuffer)}
}

// TestHub_DispatchFiltersByVisibleTo：非 admin 按写前/写后 _perms 过滤；
// platform admin 旁路 _perms 收全部事件。
func TestHub_DispatchFiltersByVisibleTo(t *testing.T) {
	t.Parallel()
	h := NewHub(nil)

	reader := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})
	stranger := newTestConn("u2", databases.Principal{Roles: []string{"users", "user:u2"}})
	admin := newTestConn("adm", databases.Principal{Roles: []string{"admin"}})
	admin.PlatformAdmin = true

	ch := testEnvelope().CollectionChannel()
	h.Subscribe(ch, reader)
	h.Subscribe(ch, stranger)
	h.Subscribe(ch, admin)

	h.Dispatch(testEnvelope())

	// reader（user:u1 命中 doc perms）与 admin 各收 1 帧（集合频道）；
	// stranger 无 read 权不投。
	require.Len(t, drain(t, reader.Send), 1)
	require.Len(t, drain(t, stranger.Send), 0)
	require.Len(t, drain(t, admin.Send), 1)
}

// TestHub_DispatchDeliversToBothChannels：一条事件按集合 + 文档两个频道
// fan-out；连接只收到已订阅的那一侧。
func TestHub_DispatchDeliversToBothChannels(t *testing.T) {
	t.Parallel()
	h := NewHub(nil)
	ev := testEnvelope()

	collOnly := newTestConn("c", databases.Principal{Roles: []string{"users", "user:u1"}})
	docOnly := newTestConn("d", databases.Principal{Roles: []string{"users", "user:u1"}})
	both := newTestConn("b", databases.Principal{Roles: []string{"users", "user:u1"}})

	h.Subscribe(ev.CollectionChannel(), collOnly)
	h.Subscribe(ev.DocumentChannel(), docOnly)
	h.Subscribe(ev.CollectionChannel(), both)
	h.Subscribe(ev.DocumentChannel(), both)

	h.Dispatch(ev)

	require.Len(t, drain(t, collOnly.Send), 1)
	require.Len(t, drain(t, docOnly.Send), 1)
	require.Len(t, drain(t, both.Send), 2)

	// 帧结构：type=event + channel + payload（ClientPayload 无 acl）。
	for _, conn := range []*Conn{both, collOnly} {
		for _, raw := range drain(t, conn.Send) {
			frame, ok := raw.(map[string]any)
			require.True(t, ok)
			require.Equal(t, "event", frame["type"])
			payload, ok := frame["payload"].(map[string]any)
			require.True(t, ok)
			_, hasACL := payload["acl"]
			require.False(t, hasACL, "出站帧不得含 acl")
			require.Equal(t, ev.EventID, payload["event_id"])
		}
	}
}

// TestHub_DispatchDedupsEventID：回收重放 / PEL 重投的同一 event_id
// 只扇出一次。
func TestHub_DispatchDedupsEventID(t *testing.T) {
	t.Parallel()
	h := NewHub(nil)
	conn := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})
	h.Subscribe(testEnvelope().CollectionChannel(), conn)

	h.Dispatch(testEnvelope())
	h.Dispatch(testEnvelope()) // 同一 event_id 重投
	require.Len(t, drain(t, conn.Send), 1)

	other := testEnvelope()
	other.EventID = "01J-test-event-other"
	h.Dispatch(other) // 不同 event_id
	require.Len(t, drain(t, conn.Send), 1)
}

// TestHub_SlowConsumerDropsEvent：发送 chan 满载时丢事件不阻塞 Dispatch。
func TestHub_SlowConsumerDropsEvent(t *testing.T) {
	t.Parallel()
	h := NewHub(nil)
	conn := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})
	conn.Send = make(chan map[string]any, 1)
	h.Subscribe(testEnvelope().CollectionChannel(), conn)

	for i := 0; i < 5; i++ {
		ev := testEnvelope()
		ev.EventID = ev.EventID + string(rune('0'+i))
		h.Dispatch(ev) // 不阻塞
	}
	require.Len(t, drain(t, conn.Send), 1)
}

// TestHub_UnsubscribeAndRemove：Unsubscribe 单频道、Remove 全频道摘除，
// 订阅 gauge 随之回退。
func TestHub_UnsubscribeAndRemove(t *testing.T) {
	t.Parallel()
	h := NewHub(nil)
	ev := testEnvelope()
	conn := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})

	h.Subscribe(ev.CollectionChannel(), conn)
	h.Subscribe(ev.DocumentChannel(), conn)
	require.Equal(t, 2, h.subCount)

	h.Unsubscribe(ev.CollectionChannel(), conn.ID)
	require.Equal(t, 1, h.subCount)

	h.Remove(conn.ID)
	require.Equal(t, 0, h.subCount)
	require.Empty(t, h.channels)
}

// TestHub_OutboundJSONHasNoACL：帧 JSON 序列化后无 acl / collection_permissions。
func TestHub_OutboundJSONHasNoACL(t *testing.T) {
	t.Parallel()
	h := NewHub(nil)
	conn := newTestConn("u1", databases.Principal{Roles: []string{"users", "user:u1"}})
	h.Subscribe(testEnvelope().CollectionChannel(), conn)
	h.Dispatch(testEnvelope())

	for _, raw := range drain(t, conn.Send) {
		data, err := json.Marshal(raw)
		require.NoError(t, err)
		require.NotContains(t, string(data), "acl")
		require.NotContains(t, string(data), "collection_permissions")
	}
}

func drain(t *testing.T, ch chan map[string]any) []any {
	t.Helper()
	var out []any
	for {
		select {
		case frame := <-ch:
			out = append(out, frame)
		default:
			return out
		}
	}
}
