package functions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testToken     = "twx_test_token"
	testExecID    = "exec-1"
	testSource    = "client"
	testUserID    = "user-9"
	testProjectID = "proj-7"
	testAPIBase   = "http://api.internal:8080"
)

// dispatchHeaders 返回完整身份六件（测试用；apiBaseUrl 走 env）。
func dispatchHeaders(extra map[string]string) map[string]string {
	h := map[string]string{
		"X-Tw-Execution-Token":  testToken,
		"X-Tw-Execution-Id":     testExecID,
		"X-Tw-Source":           testSource,
		"X-Tw-Invoking-User-Id": testUserID,
		"X-Tw-Project-Id":       testProjectID,
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func wantIdentity(t *testing.T, got Identity) {
	t.Helper()
	if got.ExecutionToken != testToken ||
		got.ExecutionID != testExecID ||
		got.Source != testSource ||
		got.InvokingUserID != testUserID ||
		got.ProjectID != testProjectID ||
		got.APIBaseURL != testAPIBase {
		t.Fatalf("identity = %+v, want token=%s exec=%s source=%s user=%s project=%s base=%s",
			got, testToken, testExecID, testSource, testUserID, testProjectID, testAPIBase)
	}
}

// —— StartInvoke：decode / encode / identity 注入 ——

type pingReq struct {
	Ping string `json:"ping"`
}

type pongResp struct {
	Pong string `json:"pong"`
}

func TestStartInvokeRoundTrip(t *testing.T) {
	t.Setenv("TW_API_BASE_URL", testAPIBase)
	var gotReq pingReq
	handler := invokeHandler(func(ctx context.Context, req pingReq) (pongResp, error) {
		gotReq = req
		wantIdentity(t, FromContext(ctx))
		return pongResp{Pong: req.Ping}, nil
	})
	ts := newTestServer(t, handler)

	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", dispatchHeaders(nil), `{"ping":"a"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if gotReq.Ping != "a" {
		t.Fatalf("decoded req = %+v, want ping=a", gotReq)
	}
	// 成功封套逐字节（含恒空 stdout/stderr）。
	want := `{"ok":true,"result":{"pong":"a"},"stdout":"","stderr":""}`
	if body != want {
		t.Fatalf("body = %s, want %s", body, want)
	}
}

func TestStartInvokeUnknownHeadersIgnored(t *testing.T) {
	t.Setenv("TW_API_BASE_URL", testAPIBase)
	handler := invokeHandler(func(ctx context.Context, _ struct{}) (struct{}, error) {
		wantIdentity(t, FromContext(ctx)) // 未知 header 不影响已知通道
		return struct{}{}, nil
	})
	ts := newTestServer(t, handler)
	h := dispatchHeaders(map[string]string{
		"X-Tw-Future-Feature": "whatever",   // 协议演进：未知 x-tw-* header
		"X-Tw-Another-One":    "still fine", // 一律忽略
	})
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", h, `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
}

func TestStartInvokeDecodeError500(t *testing.T) {
	handler := invokeHandler(func(_ context.Context, _ struct{ N int }) (struct{}, error) {
		return struct{}{}, nil
	})
	ts := newTestServer(t, handler)
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", nil, `{"n":"not-an-int"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (decode shape error)", resp.StatusCode)
	}
	if msg, _ := decodeEnvelope(t, body)["error"].(string); !strings.Contains(msg, "decode TW_DATA into request") {
		t.Fatalf("error = %q", msg)
	}
}

func TestStartInvokeEmptyBodyZeroValue(t *testing.T) {
	var got struct{ N int }
	handler := invokeHandler(func(_ context.Context, req struct{ N int }) (struct{ N int }, error) {
		got = req // 空 body → "{}" → 零值安全
		return req, nil
	})
	ts := newTestServer(t, handler)
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	if got.N != 0 {
		t.Fatalf("req = %+v, want zero value", got)
	}
	if body != `{"ok":true,"result":{"N":0},"stdout":"","stderr":""}` {
		t.Fatalf("body = %s", body)
	}
}

func TestStartInvokeHandlerError500(t *testing.T) {
	handler := invokeHandler(func(_ context.Context, _ struct{}) (struct{}, error) {
		return struct{}{}, errors.New("boom: business failure")
	})
	ts := newTestServer(t, handler)
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", nil, `{}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	env := decodeEnvelope(t, body)
	if env["ok"] != false || env["error"] != "boom: business failure" {
		t.Fatalf("envelope = %s", body)
	}
	if _, has := env["result"]; has {
		t.Fatalf("error envelope must omit result: %s", body)
	}
}

func TestStartInvokePanic500(t *testing.T) {
	handler := invokeHandler(func(_ context.Context, _ struct{}) (struct{}, error) {
		panic("boom: panic failure")
	})
	ts := newTestServer(t, handler)
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", nil, `{}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (panic captured)", resp.StatusCode)
	}
	if msg, _ := decodeEnvelope(t, body)["error"].(string); !strings.Contains(msg, "panic: boom: panic failure") {
		t.Fatalf("error = %q, want panic message", msg)
	}
	// panic 不杀实例：下一个请求照常服务。
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/", nil, `{}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("second request status = %d, want 500 (instance alive)", resp.StatusCode)
	}
	_, body = doJSON(t, http.MethodGet, ts.URL+healthPath, nil, "")
	if served := decodeEnvelope(t, body)["served"]; served != float64(2) {
		t.Fatalf("served = %v, want 2", served)
	}
}

// —— main 风格封套注入（HTTP 触发器打在 main 风格函数上） ——

func TestStartInvokeEnvelopeInjection(t *testing.T) {
	envelope := base64Of(`{"method":"put","path":"/api/x","raw_query":"a=1","headers":{"x-one":["1"]}}`)
	handler := invokeHandler(func(ctx context.Context, req envelopeData) (envelopeData, error) {
		return req, nil
	})
	ts := newTestServer(t, handler)
	h := dispatchHeaders(map[string]string{"X-Tw-Trigger-Envelope": envelope})
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", h, "raw-trigger-body")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	env := decodeEnvelope(t, body)
	result, _ := env["result"].(map[string]any)
	if result == nil {
		t.Fatalf("missing result: %s", body)
	}
	for k, want := range map[string]any{
		"method": "put", "path": "/api/x", "raw_query": "a=1",
		"body": "raw-trigger-body", "body_base64": base64Of("raw-trigger-body"),
	} {
		if result[k] != want {
			t.Fatalf("result[%s] = %v, want %v", k, result[k], want)
		}
	}
	headers, _ := result["headers"].(map[string]any)
	if headers == nil || headers["x-one"] == nil {
		t.Fatalf("result.headers = %v, want x-one present", result["headers"])
	}
}

// —— StartCron：CronTick 解析 ——

func TestStartCronTickParsing(t *testing.T) {
	var got CronTick
	ts := newTestServer(t, cronAdapter(func(_ context.Context, tick CronTick) error {
		got = tick
		return nil
	}))
	h := map[string]string{"X-Tw-Source": "cron:nightly"}
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", h,
		`{"type":"cron","trigger_id":"nightly","scheduled_for":"2026-09-18T01:02:03Z"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	want := time.Date(2026, 9, 18, 1, 2, 3, 0, time.UTC)
	if got.TriggerID != "nightly" || !got.ScheduledFor.Equal(want) {
		t.Fatalf("tick = %+v, want nightly @ %s", got, want)
	}
	if body != `{"ok":true,"result":null,"stdout":"","stderr":""}` {
		t.Fatalf("body = %s", body)
	}
}

func TestStartCronBadShape500(t *testing.T) {
	ts := newTestServer(t, cronAdapter(func(_ context.Context, _ CronTick) error { return nil }))
	// 语法合法但缺 trigger_id → 500（形状错误）；语法错误 → 400（twData）。
	cases := []struct {
		body   string
		status int
		substr string
	}{
		{`{"scheduled_for":"2026-09-18T01:02:03Z"}`, http.StatusInternalServerError, "missing trigger_id"},
		{`{"trigger_id":"n","scheduled_for":"yesterday"}`, http.StatusInternalServerError, "invalid scheduled_for"},
		{`{bad json`, http.StatusBadRequest, "invalid TW_DATA JSON"},
	}
	for i, tc := range cases {
		resp, body := doJSON(t, http.MethodPost, ts.URL+"/", nil, tc.body)
		if resp.StatusCode != tc.status {
			t.Fatalf("case %d status = %d, want %d (%s)", i, resp.StatusCode, tc.status, body)
		}
		if msg, _ := decodeEnvelope(t, body)["error"].(string); !strings.Contains(msg, tc.substr) {
			t.Fatalf("case %d error = %q, want contains %q", i, msg, tc.substr)
		}
	}
}

// —— StartEvent：DocumentChange 解析（含 data 嵌套） ——

func TestStartEventDocumentChangeParsing(t *testing.T) {
	var got DocumentChange
	ts := newTestServer(t, eventAdapter(func(_ context.Context, ch DocumentChange) error {
		got = ch
		return nil
	}))
	h := map[string]string{"X-Tw-Source": "event:sub-1"}
	data := `{"type":"event","event":"documents.created","event_id":"ev-1","seq":42,` +
		`"database_id":"app","collection_id":"users","document_id":"doc-1","version":7,` +
		`"data":{"id":"doc-1","data":{"name":"Ann","age":3},"permissions":["read"]},` +
		`"envelope_truncated":false,"data_truncated":false}`
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", h, data)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	wantDoc := map[string]any{"name": "Ann", "age": float64(3)}
	if got.Event != "documents.created" || got.DatabaseID != "app" ||
		got.CollectionID != "users" || got.DocumentID != "doc-1" || got.Version != 7 ||
		got.Document.ID != "doc-1" || got.Document.Data["name"] != "Ann" ||
		got.Document.Data["age"] != wantDoc["age"] {
		t.Fatalf("change = %+v", got)
	}
}

func TestStartEventDeleteNoData(t *testing.T) {
	var got DocumentChange
	ts := newTestServer(t, eventAdapter(func(_ context.Context, ch DocumentChange) error {
		got = ch
		return nil
	}))
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/",
		map[string]string{"X-Tw-Source": "event:sub-1"},
		`{"event":"documents.deleted","database_id":"app","collection_id":"users","document_id":"doc-1","data_truncated":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	if got.Document.ID != "" || got.Document.Data != nil {
		t.Fatalf("document = %+v, want zero value (no data projection)", got.Document)
	}
	if got.Event != "documents.deleted" || got.DocumentID != "doc-1" {
		t.Fatalf("change = %+v", got)
	}
}

func TestStartEventBadShape500(t *testing.T) {
	ts := newTestServer(t, eventAdapter(func(_ context.Context, _ DocumentChange) error { return nil }))
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", nil, `{"event":"documents.created"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", resp.StatusCode, body)
	}
	if msg, _ := decodeEnvelope(t, body)["error"].(string); !strings.Contains(msg, "missing database_id") {
		t.Fatalf("error = %q", msg)
	}
}

// —— StartHTTP：封套还原 + 头过滤 + 截断 ——

func TestStartHTTPEnvelopeRestored(t *testing.T) {
	envelope := base64Of(`{"method":"put","path":"/api/x","raw_query":"a=1&b=2","headers":{"x-one":["1"],"x-two":["2","3"]}}`)
	var gotMethod, gotPath, gotQuery, gotOne, gotTwo, gotBody, gotToken string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotOne = r.Header.Get("X-One")
		gotTwo = strings.Join(r.Header.Values("X-Two"), "|")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		gotToken = FromContext(r.Context()).ExecutionToken
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Reply", "hi")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("done"))
	})
	ts := newTestServer(t, fetchAdapter{inner: handler})

	h := dispatchHeaders(map[string]string{"X-Tw-Trigger-Envelope": envelope})
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", h, "hello-trigger")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %s, want PUT (uppercased)", gotMethod)
	}
	if gotPath != "/api/x" || gotQuery != "a=1&b=2" {
		t.Fatalf("url = %s?%s, want /api/x?a=1&b=2", gotPath, gotQuery)
	}
	if gotOne != "1" || gotTwo != "2|3" {
		t.Fatalf("headers x-one=%q x-two=%q", gotOne, gotTwo)
	}
	if gotBody != "hello-trigger" {
		t.Fatalf("body = %q, want trigger raw body", gotBody)
	}
	if gotToken != testToken {
		t.Fatalf("request ctx identity token = %q, want %q", gotToken, testToken)
	}

	env := decodeEnvelope(t, body)
	if env["ok"] != true || env["status"] != float64(http.StatusCreated) {
		t.Fatalf("envelope = %s", body)
	}
	if b64, _ := env["body_base64"].(string); decoded(b64) != "done" {
		t.Fatalf("body_base64 = %q", env["body_base64"])
	}
	headers, _ := env["headers"].(map[string]any)
	if headers["content-type"] != "text/plain" || headers["x-reply"] != "hi" {
		t.Fatalf("headers = %v, want content-type + x-reply", headers)
	}
	for _, blocked := range []string{"content-length", "date", "server", "host", "connection"} {
		if _, has := headers[blocked]; has {
			t.Fatalf("headers = %v, must exclude %q", headers, blocked)
		}
	}
	if env["truncated"] != false {
		t.Fatalf("truncated = %v, want false", env["truncated"])
	}
}

func TestStartHTTPDefaultRequest(t *testing.T) {
	var gotMethod, gotURL, gotCT, gotBody string
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotMethod, gotURL = r.Method, r.URL.String()
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
	})
	ts := newTestServer(t, fetchAdapter{inner: handler})
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", nil, `{"k":"v"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	if gotMethod != http.MethodPost || gotURL != "http://function/" {
		t.Fatalf("request = %s %s, want POST http://function/", gotMethod, gotURL)
	}
	if gotCT != "application/json" || gotBody != `{"k":"v"}` {
		t.Fatalf("ct=%q body=%q", gotCT, gotBody)
	}
	if env := decodeEnvelope(t, body); env["status"] != float64(200) {
		t.Fatalf("fetch envelope status = %v (recorder default 200)", env["status"])
	}
}

func TestStartHTTPHeaderFilterAndTruncation(t *testing.T) {
	big := strings.Repeat("B", 70_000)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Length", "9999") // 黑名单：不进封套
		h.Set("Date", "fake-date")
		h.Set("Server", "fake-server")
		h.Set("Host", "fake-host")
		h.Set("Connection", "close")
		h.Set("Transfer-Encoding", "identity")
		h.Set("Keep-Alive", "timeout=5")
		h.Set("Upgrade", "websocket")
		h.Set("Proxy-Connection", "close")
		h.Set("Te", "trailers")
		h.Set("Trailer", "x")
		h.Set("Content-Type", "application/octet-stream")
		h.Add("Set-Cookie", "a=1")
		h.Add("Set-Cookie", "b=2") // 同名多值合并（combined 值语义）
		_, _ = w.Write([]byte(big))
	})
	ts := newTestServer(t, fetchAdapter{inner: handler})
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", nil, `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	env := decodeEnvelope(t, body)
	if env["truncated"] != true {
		t.Fatalf("truncated = %v, want true (>64KB)", env["truncated"])
	}
	decodedBody := decoded(env["body_base64"].(string))
	if len(decodedBody) != maxOutputBytes {
		t.Fatalf("body length = %d, want %d", len(decodedBody), maxOutputBytes)
	}
	headers, _ := env["headers"].(map[string]any)
	for _, blocked := range []string{
		"connection", "keep-alive", "proxy-connection", "te", "trailer",
		"transfer-encoding", "upgrade", "content-length", "host", "date", "server",
	} {
		if _, has := headers[blocked]; has {
			t.Fatalf("headers = %v, must exclude %q", headers, blocked)
		}
	}
	if headers["content-type"] != "application/octet-stream" {
		t.Fatalf("content-type = %v", headers["content-type"])
	}
	if headers["set-cookie"] != "a=1, b=2" {
		t.Fatalf("set-cookie = %v, want combined", headers["set-cookie"])
	}
}

func decoded(b64 string) string {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "<decode error>"
	}
	return string(raw)
}

// —— Mux：按 source 前缀分发 + 未注册源 500 ——

func TestMuxDispatchBySource(t *testing.T) {
	var invoked string
	var cronTick CronTick
	var eventCh DocumentChange
	var fetchedPath string

	mux := NewMux()
	mux.Invoke(func(_ context.Context, data json.RawMessage) (any, error) {
		invoked = string(data)
		return map[string]string{"echo": string(data)}, nil
	})
	mux.Cron(func(_ context.Context, tick CronTick) error {
		cronTick = tick
		return nil
	})
	mux.Event(func(_ context.Context, ch DocumentChange) error {
		eventCh = ch
		return nil
	})
	mux.Fetch(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchedPath = r.URL.Path
		_, _ = w.Write([]byte("fetched"))
	}))
	ts := newTestServer(t, mux)

	// server / client → Invoke。
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", map[string]string{"X-Tw-Source": "server"}, `{"n":1}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"echo":"{\"n\":1}"`) {
		t.Fatalf("invoke: status %d body %s", resp.StatusCode, body)
	}
	if invoked != `{"n":1}` {
		t.Fatalf("invoked data = %q", invoked)
	}
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/", map[string]string{"X-Tw-Source": "client"}, `{"n":2}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"echo":"{\"n\":2}"`) {
		t.Fatalf("client invoke status = %d body %s", resp.StatusCode, body)
	}
	if invoked != `{"n":2}` {
		t.Fatalf("invoked data = %q (source client)", invoked)
	}

	// cron:{id} → Cron。
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/", map[string]string{"X-Tw-Source": "cron:daily"},
		`{"type":"cron","trigger_id":"daily","scheduled_for":"2026-09-18T00:00:00Z"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cron: status %d body %s", resp.StatusCode, body)
	}
	if cronTick.TriggerID != "daily" {
		t.Fatalf("cronTick = %+v", cronTick)
	}

	// event:{id} → Event。
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/", map[string]string{"X-Tw-Source": "event:sub-9"},
		`{"event":"documents.created","database_id":"db","collection_id":"c","document_id":"d1"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event: status %d body %s", resp.StatusCode, body)
	}
	if eventCh.DocumentID != "d1" {
		t.Fatalf("eventCh = %+v", eventCh)
	}

	// http:{id} → Fetch（封套还原）。
	envelope := base64Of(`{"method":"get","path":"/hook/42","raw_query":"","headers":{}}`)
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/", map[string]string{
		"X-Tw-Source":           "http:hook-1",
		"X-Tw-Trigger-Envelope": envelope,
	}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch: status %d body %s", resp.StatusCode, body)
	}
	if fetchedPath != "/hook/42" {
		t.Fatalf("fetched path = %q", fetchedPath)
	}
	fenv := decodeEnvelope(t, body)
	if fenv["ok"] != true || decoded(fenv["body_base64"].(string)) != "fetched" {
		t.Fatalf("fetch envelope = %s", body)
	}
}

func TestMuxUnregisteredSource500(t *testing.T) {
	mux := NewMux()
	mux.Invoke(func(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil })
	ts := newTestServer(t, mux)

	cases := []struct {
		source string
		want   string
	}{
		{"cron:x", "no Cron handler registered"},
		{"event:y", "no Event handler registered"},
		{"http:z", "no Fetch handler registered"},
	}
	for _, tc := range cases {
		resp, body := doJSON(t, http.MethodPost, ts.URL+"/", map[string]string{"X-Tw-Source": tc.source}, `{}`)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("source %s status = %d, want 500 (%s)", tc.source, resp.StatusCode, body)
		}
		if msg, _ := decodeEnvelope(t, body)["error"].(string); !strings.Contains(msg, tc.want) {
			t.Fatalf("source %s error = %q, want contains %q", tc.source, msg, tc.want)
		}
	}

	// 无 Invoke：server 面也 500。
	empty := NewMux()
	ets := newTestServer(t, empty)
	resp, body := doJSON(t, http.MethodPost, ets.URL+"/", map[string]string{"X-Tw-Source": "server"}, `{}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("bare mux status = %d, want 500", resp.StatusCode)
	}
	if msg, _ := decodeEnvelope(t, body)["error"].(string); !strings.Contains(msg, "no Invoke handler registered") {
		t.Fatalf("error = %q", msg)
	}
}

func TestMuxServeHTTPDirectWithoutListen(t *testing.T) {
	mux := NewMux()
	mux.Invoke(func(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil })
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if msg := decodeEnvelope(t, rec.Body.String())["error"]; !strings.Contains(msg.(string), "functions.Listen") {
		t.Fatalf("error = %v, want mentions functions.Listen", msg)
	}
}

// —— Start 系列真实绑定冒烟（TW_RUNNER_PORT 生效；Listen 阻塞语义） ——

func TestStartInvokeBindsAndServes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	t.Setenv("TW_RUNNER_PORT", strconv.Itoa(port))
	t.Setenv("TW_MAX_REQUESTS", "0") // 冒烟不触发自退出

	started := make(chan error, 1)
	go func() {
		started <- StartInvoke(func(_ context.Context, req pingReq) (pongResp, error) {
			return pongResp{Pong: strings.ToUpper(req.Ping)}, nil
		})
	}()

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("server did not become ready within 5s")
		}
		resp, err := client.Get(base + healthPath)
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if decodeEnvelope(t, string(raw))["ready"] != true {
					t.Fatalf("health not ready: %s", raw)
				}
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	httpResp, err := client.Post(base+"/", "application/json", strings.NewReader(`{"ping":"go"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(httpResp.Body)
	_ = httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"pong":"GO"`) {
		t.Fatalf("dispatch = %d %s", httpResp.StatusCode, raw)
	}
	// Listen 阻塞不返回（goroutine 残留至测试二进制结束——有意为之）。
	select {
	case err := <-started:
		t.Fatalf("Listen returned early: %v", err)
	default:
	}
}
