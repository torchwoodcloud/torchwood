package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// realtimeTestServer 是协议级假服务端（对齐 internal/api/realtime 的帧
// 格式）：记录每次 hello 的 token 与全部控制帧，暴露当前连接供测试
// 主动推事件 / 关连接。
type realtimeTestServer struct {
	mu         sync.Mutex
	subscribes []string

	url    string
	hellos chan string          // 每次握手的 access_token
	frames chan map[string]any  // 客户端控制帧（subscribe/unsubscribe/ping）
	conns  chan *websocket.Conn // 每次握手成功后的服务端连接
}

func newRealtimeTestServer(t *testing.T) *realtimeTestServer {
	t.Helper()
	s := &realtimeTestServer{
		hellos: make(chan string, 8),
		frames: make(chan map[string]any, 32),
		conns:  make(chan *websocket.Conn, 8),
	}
	ts := httptest.NewServer(http.HandlerFunc(s.serveWS))
	s.url = ts.URL
	t.Cleanup(ts.Close)
	return s
}

func (s *realtimeTestServer) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()

	_, data, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var hello struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &hello); err != nil || hello.Type != "hello" || hello.ProjectID == "" {
		_ = writeServerFrame(conn, map[string]any{"type": "error", "code": "UNAUTHENTICATED", "message": "bad hello"})
		_ = conn.Close(websocket.StatusPolicyViolation, "UNAUTHENTICATED")
		return
	}
	s.hellos <- hello.AccessToken
	if err := writeServerFrame(conn, map[string]any{"type": "hello_ok", "connection_id": "conn-x"}); err != nil {
		return
	}
	s.conns <- conn

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f map[string]any
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		s.frames <- f
		switch f["type"] {
		case "subscribe":
			s.mu.Lock()
			s.subscribes = append(s.subscribes, f["channel"].(string))
			s.mu.Unlock()
			_ = writeServerFrame(conn, map[string]any{"type": "subscribed", "id": f["id"], "channel": f["channel"]})
		case "ping":
			_ = writeServerFrame(conn, map[string]any{"type": "pong"})
		}
	}
}

func writeServerFrame(conn *websocket.Conn, f map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, _ := json.Marshal(f)
	return conn.Write(ctx, websocket.MessageText, data)
}

func recvOf[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("等待服务端帧超时")
		var zero T
		return zero
	}
}

// realtimeTokens 初始 token（远期不过期，避免触发 refreshIfExpiring）。
func realtimeTokens() *clientv1.TokenBundle {
	return &clientv1.TokenBundle{
		AccessToken:  "jwt-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    timestamppb.New(time.Unix(1893456000, 0)),
	}
}

func TestRealtime_HelloSubscribeEvent(t *testing.T) {
	srv := newRealtimeTestServer(t)
	c, _ := newTestClient(t, WithProjectID("proj-1"), WithInitialTokens(realtimeTokens()))

	rt, err := c.ConnectRealtime(context.Background(), srv.url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	require.Equal(t, "jwt-1", recvOf(t, srv.hellos))
	serverConn := recvOf(t, srv.conns)

	events := make(chan RealtimeEvent, 4)
	rt.Subscribe("databases.app.collections.posts", func(ev RealtimeEvent) { events <- ev })
	f := recvOf(t, srv.frames)
	require.Equal(t, "subscribe", f["type"])
	require.Equal(t, "databases.app.collections.posts", f["channel"])

	// 服务端推事件帧 → handler 收到（payload 无 acl）。
	require.NoError(t, writeServerFrame(serverConn, map[string]any{
		"type":    "event",
		"channel": "databases.app.collections.posts",
		"payload": map[string]any{"event_id": "e1", "document_id": "d1"},
	}))
	ev := recvOf(t, events)
	require.Equal(t, "databases.app.collections.posts", ev.Channel)
	require.Equal(t, "e1", ev.Payload["event_id"])

	// 服务端 ping → 客户端回 pong。
	require.NoError(t, writeServerFrame(serverConn, map[string]any{"type": "ping"}))
	f = recvOf(t, srv.frames)
	require.Equal(t, "pong", f["type"])
}

func TestRealtime_TokenExpiredReconnectResubscribes(t *testing.T) {
	srv := newRealtimeTestServer(t)
	c, fake := newTestClient(t, WithProjectID("proj-1"), WithInitialTokens(realtimeTokens()))
	// 刷新后的新 token（fakeAccount.RefreshToken 要求 refresh token 为 refresh-1）。
	fake.tokens = &clientv1.TokenBundle{
		AccessToken:  "jwt-2",
		RefreshToken: "refresh-1",
		ExpiresAt:    timestamppb.New(time.Unix(1893456000, 0)),
	}

	rt, err := c.ConnectRealtime(context.Background(), srv.url, WithRealtimeBackoff(time.Millisecond, 5*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	require.Equal(t, "jwt-1", recvOf(t, srv.hellos))
	conn1 := recvOf(t, srv.conns)
	rt.Subscribe("databases.app.collections.posts", func(RealtimeEvent) {})
	rt.Subscribe("databases.app.collections.comments", func(RealtimeEvent) {})
	recvOf(t, srv.frames) // subscribe posts
	recvOf(t, srv.frames) // subscribe comments

	// JWT 到期：服务端以 StatusPolicyViolation + token_expired 关连接。
	require.NoError(t, conn1.Close(websocket.StatusPolicyViolation, "token_expired"))

	// 重连：先强制刷新（refreshCalls=1），hello 带新 token。
	require.Equal(t, "jwt-2", recvOf(t, srv.hellos))
	require.Equal(t, int32(1), fake.refreshCalls.Load())
	recvOf(t, srv.conns)

	// 重订全部频道（不补历史）。
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		f := recvOf(t, srv.frames)
		require.Equal(t, "subscribe", f["type"])
		got[f["channel"].(string)] = true
	}
	require.True(t, got["databases.app.collections.posts"])
	require.True(t, got["databases.app.collections.comments"])
}

func TestRealtime_HandshakeErrorSurfaces(t *testing.T) {
	srv := newRealtimeTestServer(t)
	// 无 ProjectID：直接报错。
	c, _ := newTestClient(t, WithInitialTokens(realtimeTokens()))
	_, err := c.ConnectRealtime(context.Background(), srv.url)
	require.Error(t, err)

	// hello 缺 project_id 的场景由服务端 error 帧覆盖（见 serveWS），这里
	// 断言坏 endpoint 报错。
	c2, _ := newTestClient(t, WithProjectID("proj-1"), WithInitialTokens(realtimeTokens()))
	_, err = c2.ConnectRealtime(context.Background(), "ftp://x")
	require.Error(t, err)
}

func TestRealtime_CloseStopsReconnect(t *testing.T) {
	srv := newRealtimeTestServer(t)
	c, _ := newTestClient(t, WithProjectID("proj-1"), WithInitialTokens(realtimeTokens()))

	rt, err := c.ConnectRealtime(context.Background(), srv.url, WithRealtimeBackoff(time.Millisecond, 5*time.Millisecond))
	require.NoError(t, err)
	recvOf(t, srv.hellos)
	recvOf(t, srv.conns)

	require.NoError(t, rt.Close())
	// 主动关闭后不再重连：hellos 通道应保持为空。
	select {
	case tok := <-srv.hellos:
		t.Fatalf("close 后不应重连，收到 hello token %q", tok)
	case <-time.After(100 * time.Millisecond):
	}
}
