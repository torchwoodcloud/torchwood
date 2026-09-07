package realtime

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	domainevents "github.com/torchwooddev/torchwood/internal/domain/events"
	"github.com/torchwooddev/torchwood/internal/infra/realtime"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/pkg/jwtparser"
)

// replayDocDB 在 fakeDocDB 之上注入 ListChanges 结果（阶段④重放路径）。
type replayDocDB struct {
	*fakeDocDB
	changes  []databases.DocumentChange
	hasMore  bool
	err      error
	calls    int
	lastOpts databases.ListChangesOptions
}

func (r *replayDocDB) ListChanges(_ context.Context, _, _, _ string, opts databases.ListChangesOptions, _ databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	r.calls++
	r.lastOpts = opts
	return r.changes, r.hasMore, 0, r.err
}

// replayHarness 组装带重放桩的 handler 测试环境。
func replayHarness(t *testing.T, docDB *replayDocDB) (*realtime.Hub, *httptest.Server) {
	t.Helper()
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims: &jwtparser.Claims{
			TokenType: jwtparser.TokenTypeAccess,
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		},
	}
	cfg := &config.AppConfig{}
	hub := realtime.NewHub(nil)
	h, err := NewHandler(cfg, validator, docDB, hub, nil, nil)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return hub, srv
}

func changeFor(seq int64, docID, event string) databases.DocumentChange {
	c := databases.DocumentChange{
		Seq: seq, EventID: "ev-" + docID + "-" + event, Event: event,
		DocumentID: docID, Version: 1, CreatedAt: time.Now(),
	}
	if event != domainevents.EventDocumentsDelete {
		c.Data = &databases.Document{ID: docID, Data: map[string]any{"t": "v"}, Version: 1}
	}
	return c
}

// replayDial 完成 hello 握手。
func replayDial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt-token"))
	var resp struct {
		Type string `json:"type"`
	}
	readTestFrame(t, c, &resp)
	require.Equal(t, "hello_ok", resp.Type)
	return c
}

// TestSubscribe_LastSeqReplaysBeforeLive（阶段④ §4.5）：带 last_seq 的订阅
// 先补发 outbox 事件再确认；补发帧先于 subscribed ack，实时帧在其后——
// 无窗口、无乱序。补发帧含 seq/transaction_id，delete 无 data。
func TestSubscribe_LastSeqReplaysBeforeLive(t *testing.T) {
	docDB := &replayDocDB{fakeDocDB: &fakeDocDB{collections: map[string]*databases.Collection{}}}
	setupCollection(docDB.fakeDocDB, "app", "posts", false, false)
	docDB.changes = []databases.DocumentChange{
		changeFor(6, "p1", domainevents.EventDocumentsCreate),
		changeFor(7, "p1", domainevents.EventDocumentsDelete),
	}
	hub, srv := replayHarness(t, docDB)

	c := replayDial(t, srv)
	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s1", "channel": "databases.app.collections.posts", "last_seq": 5})

	// 补发帧先到。
	var ev1 struct {
		Type    string         `json:"type"`
		Channel string         `json:"channel"`
		Payload map[string]any `json:"payload"`
	}
	readTestFrame(t, c, &ev1)
	require.Equal(t, "event", ev1.Type)
	require.Equal(t, "databases.app.collections.posts", ev1.Channel)
	require.Equal(t, float64(6), ev1.Payload["seq"])
	require.NotNil(t, ev1.Payload["data"])

	var ev2 struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	readTestFrame(t, c, &ev2)
	require.Equal(t, "event", ev2.Type)
	require.Equal(t, float64(7), ev2.Payload["seq"])
	_, hasData := ev2.Payload["data"]
	require.False(t, hasData, "delete 补发帧无 data（tombstone）")

	// 确认帧随后（replayed=2、has_more=false）。
	var ack struct {
		Type     string `json:"type"`
		Replayed int64  `json:"replayed"`
		HasMore  bool   `json:"has_more"`
		Channel  string `json:"channel"`
	}
	readTestFrame(t, c, &ack)
	require.Equal(t, "subscribed", ack.Type)
	require.Equal(t, int64(2), ack.Replayed)
	require.False(t, ack.HasMore)

	require.Equal(t, int64(5), docDB.lastOpts.SinceSeq, "补发查询必须以 last_seq 为游标")
	require.Equal(t, maxReplayChanges, docDB.lastOpts.Limit)

	// 实时帧在确认之后。
	live := domainevents.Envelope{
		EventID: "live-1", Event: domainevents.EventDocumentsUpdate,
		ProjectID: "default", DatabaseID: "app", CollectionID: "posts",
		DocumentID: "p9", Version: 2, CreatedAt: time.Now(), Seq: 8,
		ACL: domainevents.ACLSnapshot{
			DocumentSecurity:    true,
			DocumentPermissions: []databases.Permission{{Type: "read", Role: "user:u1"}},
			DocHasPerms:         true,
		},
	}
	hub.Dispatch(live)
	var ev3 struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	readTestFrame(t, c, &ev3)
	require.Equal(t, "live-1", ev3.Payload["event_id"])
	require.Equal(t, float64(8), ev3.Payload["seq"])
}

// TestSubscribe_HasMoreOnAck：补发超上限 → subscribed 带 has_more=true
// （:changes 续传指引）。
func TestSubscribe_HasMoreOnAck(t *testing.T) {
	docDB := &replayDocDB{fakeDocDB: &fakeDocDB{collections: map[string]*databases.Collection{}}}
	setupCollection(docDB.fakeDocDB, "app", "posts", false, false)
	docDB.changes = []databases.DocumentChange{changeFor(6, "p1", domainevents.EventDocumentsCreate)}
	docDB.hasMore = true
	_, srv := replayHarness(t, docDB)

	c := replayDial(t, srv)
	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s1", "channel": "databases.app.collections.posts", "last_seq": 5})

	var ev struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	readTestFrame(t, c, &ev)
	require.Equal(t, "event", ev.Type)
	var ack struct {
		Type    string `json:"type"`
		HasMore bool   `json:"has_more"`
		NextSeq int64  `json:"next_seq"`
	}
	readTestFrame(t, c, &ack)
	require.Equal(t, "subscribed", ack.Type)
	require.True(t, ack.HasMore)
	// R15：has_more 时确认帧携带续传游标（扫描游标 0 → 回退末条补发事件 seq）。
	require.Equal(t, int64(6), ack.NextSeq)
}

// TestSubscribe_ResumeExpiredErrorFrame：游标过期 → error 帧
// EVENTS.RESUME_EXPIRED，连接保持、订阅未登记。
func TestSubscribe_ResumeExpiredErrorFrame(t *testing.T) {
	docDB := &replayDocDB{fakeDocDB: &fakeDocDB{collections: map[string]*databases.Collection{}}}
	setupCollection(docDB.fakeDocDB, "app", "posts", false, false)
	docDB.err = databases.ErrResumeExpired
	hub, srv := replayHarness(t, docDB)

	c := replayDial(t, srv)
	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s1", "channel": "databases.app.collections.posts", "last_seq": 1})

	expectErrorFrame(t, c, errCodeEventsResumeExpired)

	// 订阅未登记：dispatch 不达。
	hub.Dispatch(domainevents.Envelope{
		EventID: "live-x", Event: domainevents.EventDocumentsUpdate,
		ProjectID: "default", DatabaseID: "app", CollectionID: "posts",
		DocumentID: "p1", Version: 1, CreatedAt: time.Now(),
		ACL: domainevents.ACLSnapshot{DocumentSecurity: true, DocumentPermissions: []databases.Permission{{Type: "read", Role: "user:u1"}}, DocHasPerms: true},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err := c.Read(ctx)
	require.Error(t, err, "窗口内不得收到事件帧")
}

// TestSubscribe_LastSeqRejectedOnAccountsChannel：非 databases 频道带
// last_seq → INVALID_ARGUMENT（补偿仅覆盖文档事件频道）。
func TestSubscribe_LastSeqRejectedOnAccountsChannel(t *testing.T) {
	docDB := &replayDocDB{fakeDocDB: &fakeDocDB{collections: map[string]*databases.Collection{}}}
	_, srv := replayHarness(t, docDB)

	c := replayDial(t, srv)
	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s1", "channel": "accounts.u1", "last_seq": 3})
	expectErrorFrame(t, c, errCodeInvalidArgument)
}

// TestWriteLoop_ResyncCloseFrame（阶段④水位断开接线）：OnSlow → resyncCh →
// writeLoop 以 close reason "resync:<last_seq>" 断开（StatusPolicyViolation）。
// 端到端慢水位由网络背压决定（不可确定性触发），此处用裸 WS 服务器直测
// writeLoop 接线。
func TestWriteLoop_ResyncCloseFrame(t *testing.T) {
	// 裸服务器：接受升级后只维持读循环（回显 close 握手），不写任何帧
	//（writeLoop 由测试独占驱动）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := dial(t, srv)

	st := newConnState(endUserPrincipal("default", "u1"), "default", "user:u1",
		&jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h := &Handler{logger: slog.Default()}
		h.writeLoop(ctx, c, st)
	}()

	// 模拟 Hub 满水位回调（OnSlow 已在 newConnState 接线到 resyncCh）。
	st.hubConn.OnSlow(42)
	ce := expectCloseFrame(t, c)
	require.Equal(t, websocket.StatusPolicyViolation, ce.Code)
	require.Equal(t, "resync:42", ce.Reason)
	cancel()
	<-done
}

// expectCloseFrame 等待 close 帧并返回 CloseError。
func expectCloseFrame(t *testing.T, c *websocket.Conn) websocket.CloseError {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, _, err := c.Read(ctx)
		cancel()
		require.Error(t, err, "等待 close 帧超时")
		var ce websocket.CloseError
		if errors.As(err, &ce) {
			return ce
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected close frame, got: %v", err)
		}
	}
}
