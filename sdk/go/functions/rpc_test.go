package functions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testSecret = "whsec_test"

// signedRPCRequest 组装一个经协议签名的请求（联调/测试辅助口径）。
func signedRPCRequest(t *testing.T, method string, params any, id string, secret string) *http.Request {
	t.Helper()
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: mustJSON(t, params), ID: id})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(hdrSignature, BuildSignatureHeader(ts, [][2]string{{"v1", ComputeSignature(secret, ts, body)}}))
	req.Header.Set(hdrExecutionToken, "twx_tok")
	req.Header.Set(hdrSource, "client")
	req.Header.Set(hdrInvokingUserID, "usr_1")
	req.Header.Set(hdrProjectID, "prj_1")
	return req
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCanonicalSignatureVector 跨仓基准向量：与平台
// internal/domain/functions/protocol_test.go 共用同一 (secret, t, body) →
// hex 断言，任一侧口径漂移即红。
func TestCanonicalSignatureVector(t *testing.T) {
	if got := ComputeSignature("whsec_vector", 1759000000, []byte("abc")); got != canonicalSignatureVector {
		t.Fatalf("signature drifted from canonical vector: %s", got)
	}
}

// TestRPCServerInvokeHappyPath invoke 分发全链：验签过 → Mux.Invoke 收到
// params 原文与身份 → result 回包 + id 回显。
func TestRPCServerInvokeHappyPath(t *testing.T) {
	mux := NewMux()
	var gotParams json.RawMessage
	var gotIdentity Identity
	mux.Invoke(func(ctx context.Context, data json.RawMessage) (any, error) {
		gotParams = data
		gotIdentity = FromContext(ctx)
		return map[string]int{"answer": 42}, nil
	})

	rec := httptest.NewRecorder()
	NewServer(mux, testSecret).ServeHTTP(rec, signedRPCRequest(t, RPCMethodInvoke, map[string]string{"q": "hi"}, "exec_9", testSecret))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（函数成败在 body）", rec.Code)
	}
	var resp struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != "exec_9" {
		t.Fatalf("id echo = %q", resp.ID)
	}
	if resp.Error != nil {
		t.Fatalf("error = %+v, want nil", resp.Error)
	}
	if string(gotParams) != `{"q":"hi"}` {
		t.Fatalf("params = %s", gotParams)
	}
	if gotIdentity.ExecutionToken != "twx_tok" || gotIdentity.Source != "client" ||
		gotIdentity.InvokingUserID != "usr_1" || gotIdentity.ProjectID != "prj_1" {
		t.Fatalf("identity = %+v", gotIdentity)
	}
}

// signedEventRequest 组装一个经协议签名的 CloudEvents 通知请求（事件轴）。
func signedEventRequest(t *testing.T, eventType string, data any, secret string) *http.Request {
	t.Helper()
	body, err := json.Marshal(cloudEvent{
		Specversion: "1.0",
		ID:          "evt_1",
		Source:      "projects/prj_1/triggers/trg_1",
		Type:        eventType,
		Time:        "2026-10-15T00:00:00Z",
		Data:        mustJSON(t, data),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(hdrSignature, BuildSignatureHeader(ts, [][2]string{{"v1", ComputeSignature(secret, ts, body)}}))
	req.Header.Set(hdrExecutionToken, "twx_tok")
	req.Header.Set(hdrProjectID, "prj_1")
	return req
}

// TestEventAxisCloudEvents 事件轴（D10）：cron.tick / documents.* 以
// CloudEvents 通知送达——204 = 送达（无应用层结果）、处理失败/未注册 400
// （投递失败信号）。同一 NewServer 与 RPC 轴共存（形态探测分流）。
func TestEventAxisCloudEvents(t *testing.T) {
	mux := NewMux()
	var cronTick CronTick
	var docChange DocumentChange
	mux.Cron(func(ctx context.Context, tick CronTick) error { cronTick = tick; return nil })
	mux.Event(func(ctx context.Context, ch DocumentChange) error { docChange = ch; return nil })
	h := NewServer(mux, testSecret)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedEventRequest(t, cloudEventTypeCronTick, map[string]string{
		"trigger_id": "trg_1", "scheduled_for": "2026-10-15T00:00:00Z",
	}, testSecret))
	if rec.Code != http.StatusNoContent || cronTick.TriggerID != "trg_1" {
		t.Fatalf("cron dispatch: status=%d tick=%+v, want 204", rec.Code, cronTick)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, signedEventRequest(t, "documents.created", map[string]any{
		"event": "documents.created", "database_id": "db", "collection_id": "col",
		"document_id": "doc_1", "version": 3, "data": map[string]any{"id": "doc_1"},
	}, testSecret))
	if rec.Code != http.StatusNoContent || docChange.DocumentID != "doc_1" {
		t.Fatalf("event dispatch: status=%d change=%+v, want 204", rec.Code, docChange)
	}

	t.Run("handler error is delivery failure", func(t *testing.T) {
		failing := NewMux()
		failing.Cron(func(ctx context.Context, tick CronTick) error { return context.DeadlineExceeded })
		rec := httptest.NewRecorder()
		NewServer(failing, testSecret).ServeHTTP(rec, signedEventRequest(t, cloudEventTypeCronTick, map[string]string{
			"trigger_id": "trg_1", "scheduled_for": "2026-10-15T00:00:00Z",
		}, testSecret))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（进入重投梯度）", rec.Code)
		}
	})

	t.Run("unregistered handler", func(t *testing.T) {
		bare := NewMux()
		rec := httptest.NewRecorder()
		NewServer(bare, testSecret).ServeHTTP(rec, signedEventRequest(t, "documents.created", map[string]any{
			"event": "documents.created", "database_id": "db", "collection_id": "col",
			"document_id": "doc_1", "version": 1,
		}, testSecret))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// TestRPCServerFunctionError 函数 handler 报错 → error 对象（-32000 区段），
// HTTP 仍 200。
func TestRPCServerFunctionError(t *testing.T) {
	mux := NewMux()
	mux.Invoke(func(ctx context.Context, data json.RawMessage) (any, error) {
		return nil, context.DeadlineExceeded
	})
	rec := httptest.NewRecorder()
	NewServer(mux, testSecret).ServeHTTP(rec, signedRPCRequest(t, RPCMethodInvoke, map[string]string{}, "exec_err", testSecret))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != rpcCodeServerRange {
		t.Fatalf("error = %+v, want server-range", resp.Error)
	}
}

// TestRPCServerHTTPRequest http.request 分发：envelope+body 复原请求 →
// Fetch handler → httpResult（status/headers/body_b64）。
func TestRPCServerHTTPRequest(t *testing.T) {
	mux := NewMux()
	mux.Fetch(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-custom", "1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	body := base64.StdEncoding.EncodeToString([]byte("<xml>ok</xml>"))
	rec := httptest.NewRecorder()
	NewServer(mux, testSecret).ServeHTTP(rec, signedRPCRequest(t, RPCMethodHTTPRequest, httpRequestParams{
		Envelope: &triggerEnvelope{Method: "POST", Path: "/cb", Headers: map[string][]string{"content-type": {"application/xml"}}},
		BodyB64:  body,
	}, "exec_h", testSecret))

	var resp struct {
		Result httpResult `json:"result"`
		Error  *rpcError  `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("error = %+v", resp.Error)
	}
	if resp.Result.Status != http.StatusCreated || resp.Result.Headers["x-custom"] != "1" {
		t.Fatalf("httpResult = %+v", resp.Result)
	}
	if decoded, _ := base64.StdEncoding.DecodeString(resp.Result.BodyB64); string(decoded) != "created" {
		t.Fatalf("body = %q", resp.Result.BodyB64)
	}
}

// TestRPCServerRejects 验签失败 401 / 未知 method -32601 / 未注册 handler
// -32601 / 非 JSON-RPC 形态 -32600。
func TestRPCServerRejects(t *testing.T) {
	mux := NewMux()
	h := NewServer(mux, testSecret)

	t.Run("bad signature", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"jsonrpc":"2.0","method":"invoke","id":"x"}`)))
		req.Header.Set(hdrSignature, "t="+time.Now().Format("2006")+",v1=deadbeef")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("no signature", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"jsonrpc":"2.0","method":"invoke","id":"x"}`)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("unknown method", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, signedRPCRequest(t, "wat.method", nil, "x", testSecret))
		var resp rpcResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Error == nil || resp.Error.Code != rpcCodeMethodNotFound {
			t.Fatalf("error = %+v", resp.Error)
		}
	})
	t.Run("not json-rpc", func(t *testing.T) {
		// 无 jsonrpc 也无 specversion → 统一入口判「未识别协议」：传输层 400
		//（无 RPC 信封可承载错误对象）。
		req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"method":"invoke"}`)))
		ts := time.Now().Unix()
		req.Header.Set(hdrSignature, BuildSignatureHeader(ts, [][2]string{{"v1", ComputeSignature(testSecret, ts, []byte(`{"method":"invoke"}`))}}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (unrecognized protocol)", rec.Code)
		}
	})
	t.Run("verify requires secret", func(t *testing.T) {
		rec := httptest.NewRecorder()
		NewServer(mux).ServeHTTP(rec, signedRPCRequest(t, RPCMethodInvoke, nil, "x", testSecret))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (fail-closed)", rec.Code)
		}
	})
}

// TestRPCServerRotation 轮换窗口：头带 v2 旧钥签名，函数侧持旧钥可验。
func TestRPCServerRotation(t *testing.T) {
	mux := NewMux()
	mux.Invoke(func(ctx context.Context, data json.RawMessage) (any, error) { return "ok", nil })

	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: RPCMethodInvoke, ID: "exec_r"})
	ts := time.Now().Unix()
	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set(hdrSignature, BuildSignatureHeader(ts, [][2]string{
		{"v1", ComputeSignature("whsec_new", ts, body)},
		{"v2", ComputeSignature("whsec_old", ts, body)},
	}))
	rec := httptest.NewRecorder()
	NewServer(mux, "whsec_old").ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200（旧钥轮换窗口可验）", rec.Code)
	}
}
