package serverhttp

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---- v3 切片 C：HTTP 触发器 sync 完整透传（functions-v3.md §2.2 表「HTTP
// 触发器」行 / §2.3 恒填充 / D10）----

func syncTrigger(t *testing.T) *domainfunctions.Trigger {
	t.Helper()
	trg := httpTrigger(t)
	trg.Config.ResponseMode = domainfunctions.ResponseModeSync
	return trg
}

// TestFunctionTriggersHandler_SyncFetchTransparent fetch 风格 sync 完整透传：
// status 原样（自定义状态码）、headers 透传（content-type 等函数头）、body
// 原字节（二进制无损，ResponseB64 通道）。
func TestFunctionTriggersHandler_SyncFetchTransparent(t *testing.T) {
	trg := syncTrigger(t)
	bodyBytes := []byte{0x00, 0x01, 0xff, 0xfe, '<', 'x', 'm', 'l', '>'}
	inv := &fakeTriggerInvoker{
		trg: trg,
		rec: &domainfunctions.ExecutionRecord{
			Status:      domainfunctions.ExecutionStatusCompleted,
			StatusCode:  201,
			HTTPHeaders: map[string]string{"Content-Type": "application/xml", "X-Custom-Trace": "abc"},
			ResponseB64: base64.StdEncoding.EncodeToString(bodyBytes),
		},
	}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", `<xml/>`, nil)
	require.Equal(t, 201, rec.Code, "函数 HTTP status 原样透传")
	require.Equal(t, "application/xml", rec.Header().Get("Content-Type"), "函数设置的 content-type 透传")
	require.Equal(t, "abc", rec.Header().Get("X-Custom-Trace"))
	require.Equal(t, bodyBytes, rec.Body.Bytes(), "body 原字节（二进制无损）")
	require.Len(t, inv.calls, 1)
}

// TestFunctionTriggersHandler_SyncTransparentHeaderFilter 透传头第二层过滤
// （安全自查①）：hop-by-hop / content-length / host / date / server 不得
// 由函数冒充；Content-Length 由 handler 按实际 body 重算。
func TestFunctionTriggersHandler_SyncTransparentHeaderFilter(t *testing.T) {
	trg := syncTrigger(t)
	inv := &fakeTriggerInvoker{
		trg: trg,
		rec: &domainfunctions.ExecutionRecord{
			Status:     domainfunctions.ExecutionStatusCompleted,
			StatusCode: 200,
			HTTPHeaders: map[string]string{
				"Connection":        "close",
				"Keep-Alive":        "timeout=5",
				"Transfer-Encoding": "chunked",
				"Content-Length":    "9999",
				"Host":              "evil.example",
				"Date":              "Mon, 01 Jan 2035 00:00:00 GMT",
				"Server":            "fake-server",
				"Content-Type":      "text/plain",
			},
			ResponseB64: base64.StdEncoding.EncodeToString([]byte("hi")),
		},
	}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", "x", nil)
	require.Equal(t, 200, rec.Code)
	for _, blocked := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Host", "Date", "Server"} {
		require.Empty(t, rec.Header().Get(blocked), "hop-by-hop/平台头 %s 必须被滤", blocked)
	}
	require.Equal(t, "text/plain", rec.Header().Get("Content-Type"))
	require.Equal(t, "2", rec.Header().Get("Content-Length"), "Content-Length 按实际 body 重算")
	require.Equal(t, "hi", rec.Body.String())
}

// TestFunctionTriggersHandler_SyncFetchEmptyBody fetch 风格空 body（如 204）：
// 透明路径仍然生效（信号 = StatusCode ≥ 100，不依赖 ResponseB64 非空）。
func TestFunctionTriggersHandler_SyncFetchEmptyBody(t *testing.T) {
	trg := syncTrigger(t)
	inv := &fakeTriggerInvoker{
		trg: trg,
		rec: &domainfunctions.ExecutionRecord{
			Status:     domainfunctions.ExecutionStatusCompleted,
			StatusCode: 204,
		},
	}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", "x", nil)
	require.Equal(t, 204, rec.Code)
	require.Empty(t, rec.Body.String())
}

// TestFunctionTriggersHandler_EnvelopeAndRawBodyPassed 恒填充断言（v3 §2.3）：
// handler 构建 TW_DATA 封套（main 风格双轨）的同时，携带封套元数据与原始
// body（RawBody = 未解码请求体；元数据 headers 与 TW_DATA 白名单一致）。
func TestFunctionTriggersHandler_EnvelopeAndRawBodyPassed(t *testing.T) {
	trg := syncTrigger(t)
	inv := &fakeTriggerInvoker{
		trg: trg,
		rec: &domainfunctions.ExecutionRecord{Status: domainfunctions.ExecutionStatusCompleted, Response: `{}`},
	}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1?signature=abc&timestamp=9", `<xml>body-data</xml>`, func() http.Header {
		hdr := http.Header{}
		hdr.Set("X-WX-Signature", "sig-1")
		hdr.Set("X-Hub-Signature-256", "sha")
		return hdr
	}())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, inv.calls, 1)
	cmd := inv.calls[0]

	require.NotNil(t, cmd.TriggerEnvelope, "TriggerEnvelope 恒填充")
	require.Equal(t, http.MethodPost, cmd.TriggerEnvelope.Method)
	require.Equal(t, "/f/p1/tok1", cmd.TriggerEnvelope.Path)
	require.Equal(t, "signature=abc&timestamp=9", cmd.TriggerEnvelope.RawQuery)
	require.Equal(t, []string{"sig-1"}, cmd.TriggerEnvelope.Headers["x-wx-signature"])
	require.Equal(t, []string{"application/xml"}, cmd.TriggerEnvelope.Headers["content-type"])
	require.NotContains(t, cmd.TriggerEnvelope.Headers, "authorization", "白名单外剥除")
	require.Equal(t, `<xml>body-data</xml>`, string(cmd.RawBody), "RawBody 是原始 body 字节")
}

// TestFunctionTriggersHandler_AsyncAckEnvelopeAsync async_ack：封套随异步命令
// 透传，响应行为不变（200 + ack_body）。
func TestFunctionTriggersHandler_AsyncAckEnvelopeAsync(t *testing.T) {
	trg := httpTrigger(t)
	trg.Config.ResponseMode = domainfunctions.ResponseModeAsyncAck
	trg.Config.AckBody = `{"ok":true}`
	inv := &fakeTriggerInvoker{trg: trg}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", `<xml/>`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, inv.calls, 1)
	require.True(t, inv.calls[0].Async)
	require.NotNil(t, inv.calls[0].TriggerEnvelope, "async 路径同样恒填充")
	require.Equal(t, `<xml/>`, string(inv.calls[0].RawBody))
}

// TestFunctionTriggersHandler_SyncFetchFunctionFailure502 fetch 风格函数失败
// （runner ok=false → dispatcher Unknown）→ 502+错误语义（对齐 main 风格
// 502 现状）。
func TestFunctionTriggersHandler_SyncFetchFunctionFailure502(t *testing.T) {
	trg := syncTrigger(t)
	inv := &fakeTriggerInvoker{trg: trg, err: status.Error(codes.Unknown, "function failed")}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", "x", nil)
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

// TestFunctionTriggersHandler_SyncTimeout504_AlreadyCovered 超时 504 回归锚点
// （TimeoutError 不受透传改造影响；主测试见 SyncTimeout504）。
func TestFunctionTriggersHandler_SyncTimeout504_AlreadyCovered(t *testing.T) {
	trg := syncTrigger(t)
	inv := &fakeTriggerInvoker{trg: trg, err: status.Error(codes.DeadlineExceeded, "execution timed out")}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", "x", nil)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
}
