package serverhttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/stretchr/testify/require"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeTriggerInvoker 是 TriggerInvoker 的测试实现：记录 InvokeTrigger 入参
// 并可注入失败。
type fakeTriggerInvoker struct {
	trg    *domainfunctions.Trigger
	rec    *domainfunctions.ExecutionRecord
	err    error
	calls  []appfunctions.InvokeTriggerCommand
	limitN int
}

func (f *fakeTriggerInvoker) GetHTTPTriggerByToken(_ context.Context, projectID, token string) (*domainfunctions.Trigger, error) {
	// 与 app 层 GetHTTPTriggerByToken 同语义：仅命启用中的 http 触发器
	// （禁用/类型不符/跨项目 → nil，handler 按 404 处理）。
	if f.trg != nil && f.trg.ProjectID == projectID && f.trg.Token == token &&
		f.trg.Enabled && f.trg.Type == domainfunctions.TriggerTypeHTTP {
		return f.trg, nil
	}
	return nil, nil
}

func (f *fakeTriggerInvoker) InvokeTrigger(_ context.Context, cmd appfunctions.InvokeTriggerCommand) (*domainfunctions.ExecutionRecord, error) {
	f.calls = append(f.calls, cmd)
	if f.err != nil {
		return nil, f.err
	}
	return f.rec, nil
}

func (f *fakeTriggerInvoker) MaxTriggerBodyLimit(configured int) int {
	if f.limitN > 0 {
		return f.limitN
	}
	if configured <= 0 {
		return domainfunctions.DefaultHTTPBodyLimitBytes
	}
	return configured
}

// fakeTriggerLimiter 是 TriggerIPLimiter 的测试实现。
type fakeTriggerLimiter struct {
	allowed    bool
	err        error
	retryAfter time.Duration
	calls      []string
}

func (f *fakeTriggerLimiter) AllowTriggerIP(_ context.Context, ip string) (bool, time.Duration, error) {
	f.calls = append(f.calls, ip)
	if f.err != nil {
		return false, 0, f.err
	}
	return f.allowed, f.retryAfter, nil
}

func newTriggerTestHandler(t *testing.T, invoker TriggerInvoker, limiter TriggerIPLimiter) (*FunctionTriggersHandler, *runtime.ServeMux) {
	t.Helper()
	h, err := NewFunctionTriggersHandler(invoker, limiter, &config.AppConfig{Security: &config.Security{}}, nil)
	require.NoError(t, err)
	mux := runtime.NewServeMux()
	h.Register(mux)
	return h, mux
}

func httpTrigger(t *testing.T) *domainfunctions.Trigger {
	t.Helper()
	return &domainfunctions.Trigger{
		ID: "trg-1", ProjectID: "p1", FunctionID: "fn_1",
		Type: domainfunctions.TriggerTypeHTTP, Enabled: true, Token: "tok1",
		Config: domainfunctions.TriggerConfig{ResponseMode: domainfunctions.ResponseModeSync},
	}
}

func doTrigger(mux *runtime.ServeMux, method, path, body string, hdr http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestFunctionTriggersHandler_EchoHandshake GET + handshake=echo：直接回显
// echostr（200, application/json, Cache-Control: no-store），不 invoke。
func TestFunctionTriggersHandler_EchoHandshake(t *testing.T) {
	trg := httpTrigger(t)
	trg.Config.Handshake = domainfunctions.HandshakeEcho
	inv := &fakeTriggerInvoker{trg: trg}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodGet, "/f/p1/tok1?echostr=abc123&timestamp=1", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	require.JSONEq(t, `{"echostr":"abc123"}`, rec.Body.String())
	require.Len(t, inv.calls, 0, "echo 握手不 invoke")
}

// TestFunctionTriggersHandler_NonEchoGet405 非 echo 模式的 GET 按 method
// 不允许处理。
func TestFunctionTriggersHandler_NonEchoGet405(t *testing.T) {
	trg := httpTrigger(t)
	inv := &fakeTriggerInvoker{trg: trg}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodGet, "/f/p1/tok1?echostr=x", "", nil)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.Equal(t, "POST", rec.Header().Get("Allow"))
	require.Len(t, inv.calls, 0)
}

// TestFunctionTriggersHandler_TokenMiss404 token 未命中 404 不泄露存在性。
func TestFunctionTriggersHandler_TokenMiss404(t *testing.T) {
	inv := &fakeTriggerInvoker{trg: httpTrigger(t)}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/wrong-token", `<xml/>`, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	// 跨项目探测同形态。
	rec = doTrigger(mux, http.MethodPost, "/f/p2/tok1", `<xml/>`, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Len(t, inv.calls, 0, "未命中不触发执行")
}

// TestFunctionTriggersHandler_SyncPassthrough sync 模式透传函数响应。
func TestFunctionTriggersHandler_SyncPassthrough(t *testing.T) {
	trg := httpTrigger(t)
	inv := &fakeTriggerInvoker{trg: trg, rec: &domainfunctions.ExecutionRecord{
		Status: domainfunctions.ExecutionStatusCompleted, Response: `{"result":"ok"}`,
	}}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1?a=1&b=2", `<xml>x</xml>`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, `{"result":"ok"}`, rec.Body.String())
	require.Len(t, inv.calls, 1)
	require.False(t, inv.calls[0].Async, "sync 模式同步执行")
	require.Equal(t, "http:trg-1", inv.calls[0].Source)
}

// TestFunctionTriggersHandler_AsyncAckOrderRedLine async_ack 顺序红线：
// 入队失败（InvokeTrigger 返回错误）→ 5xx 且绝不写 200（先 200 后入队的
// 抖动窗口 = 事件永久丢失）。
func TestFunctionTriggersHandler_AsyncAckOrderRedLine(t *testing.T) {
	trg := httpTrigger(t)
	trg.Config = domainfunctions.TriggerConfig{
		ResponseMode: domainfunctions.ResponseModeAsyncAck,
		AckBody:      `{"is_valid":true}`,
	}
	inv := &fakeTriggerInvoker{trg: trg, err: status.Error(codes.Unavailable, "enqueue failed")}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", `<xml/>`, nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "入队失败一律 5xx 让微信重试")
	require.Empty(t, rec.Body.String(), "失败路径不回 ack_body")
}

// TestFunctionTriggersHandler_AsyncAckSuccess async_ack 成功：入队成功后才
// 200 + 配置的 ack_body。
func TestFunctionTriggersHandler_AsyncAckSuccess(t *testing.T) {
	trg := httpTrigger(t)
	trg.Config = domainfunctions.TriggerConfig{
		ResponseMode: domainfunctions.ResponseModeAsyncAck,
		AckBody:      `{"is_valid":true}`,
	}
	inv := &fakeTriggerInvoker{trg: trg, rec: &domainfunctions.ExecutionRecord{Status: domainfunctions.ExecutionStatusQueued}}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", `<xml/>`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"is_valid":true}`, rec.Body.String())
	require.True(t, inv.calls[0].Async)
}

// TestFunctionTriggersHandler_BodyLimit413 body 超限 413。
func TestFunctionTriggersHandler_BodyLimit413(t *testing.T) {
	trg := httpTrigger(t)
	trg.Config.BodyLimitBytes = 64
	inv := &fakeTriggerInvoker{trg: trg}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", strings.Repeat("x", 200), nil)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Len(t, inv.calls, 0, "超限请求不触发执行")
}

// TestFunctionTriggersHandler_HeaderWhitelist 封套 headers 白名单：x-*
// 大小写不敏感放行、content-type 放行、非白名单头剥除；raw_query 必须透传
// （微信 SSV 验签参数在 query）；body 双通道无损。
func TestFunctionTriggersHandler_HeaderWhitelist(t *testing.T) {
	trg := httpTrigger(t)
	inv := &fakeTriggerInvoker{trg: trg, rec: &domainfunctions.ExecutionRecord{Status: domainfunctions.ExecutionStatusCompleted}}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	hdr := http.Header{}
	hdr.Set("X-WX-Signature", "sig-1")        // x-* 大写 → 白名单（大小写不敏感）
	hdr.Set("Wechatpay-Serial", "ser-1")      // wechatpay-* → 白名单
	hdr.Set("X-Hub-Signature-256", "sha")     // 枚举验签头（x-* 已覆盖）
	hdr.Set("Authorization", "Bearer secret") // 非白名单 → 剥除
	hdr.Set("User-Agent", "UA/1.0")           // 非白名单 → 剥除
	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1?signature=abc&timestamp=9", `<xml>body-data</xml>`, hdr)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, inv.calls, 1)

	var envelope struct {
		Method   string              `json:"method"`
		Path     string              `json:"path"`
		RawQuery string              `json:"raw_query"`
		Headers  map[string][]string `json:"headers"`
		Body     string              `json:"body"`
		BodyB64  string              `json:"body_base64"`
	}
	require.NoError(t, json.Unmarshal([]byte(inv.calls[0].Data), &envelope))
	require.Equal(t, http.MethodPost, envelope.Method)
	require.Equal(t, "/f/p1/tok1", envelope.Path)
	require.Equal(t, "signature=abc&timestamp=9", envelope.RawQuery, "raw_query 必须透传（SSV 验签参数在 query）")
	require.Equal(t, []string{"sig-1"}, envelope.Headers["x-wx-signature"], "x-* 小写键放行")
	require.Equal(t, []string{"ser-1"}, envelope.Headers["wechatpay-serial"])
	require.Equal(t, []string{"sha"}, envelope.Headers["x-hub-signature-256"])
	require.NotContains(t, envelope.Headers, "authorization", "非白名单头剥除（凭证不透传）")
	require.NotContains(t, envelope.Headers, "user-agent")
	require.Equal(t, []string{"application/xml"}, envelope.Headers["content-type"], "content-type 在白名单内")
	require.Equal(t, `<xml>body-data</xml>`, envelope.Body)
	raw, err := base64.StdEncoding.DecodeString(envelope.BodyB64)
	require.NoError(t, err)
	require.Equal(t, `<xml>body-data</xml>`, string(raw), "body_base64 无损")
}

// TestFunctionTriggersHandler_PerIPQuota429 per-IP 限频触发 429 + Retry-After。
func TestFunctionTriggersHandler_PerIPQuota429(t *testing.T) {
	inv := &fakeTriggerInvoker{trg: httpTrigger(t)}
	lim := &fakeTriggerLimiter{allowed: false, retryAfter: 30 * time.Second}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", "x", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "31", rec.Header().Get("Retry-After"))
	require.Len(t, inv.calls, 0, "限频拒绝不触发执行")
	require.Len(t, lim.calls, 1, "限频先于 token 查找")
}

// TestFunctionTriggersHandler_RateLimiterErrorFailsClosed 限频器故障
// fail-closed（503 让回调方重试）。
func TestFunctionTriggersHandler_RateLimiterErrorFailsClosed(t *testing.T) {
	inv := &fakeTriggerInvoker{trg: httpTrigger(t)}
	lim := &fakeTriggerLimiter{err: errors.New("redis down")}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", "x", nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Len(t, inv.calls, 0)
}

// TestFunctionTriggersHandler_SyncTimeout504 sync 执行超时映射 504。
func TestFunctionTriggersHandler_SyncTimeout504(t *testing.T) {
	trg := httpTrigger(t)
	inv := &fakeTriggerInvoker{trg: trg, err: status.Error(codes.DeadlineExceeded, "execution timed out")}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", "x", nil)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
}

// TestFunctionTriggersHandler_SyncFunctionFailure502 sync 模式函数执行失败
// （exit != 0）→ 502 透传响应体。
func TestFunctionTriggersHandler_SyncFunctionFailure502(t *testing.T) {
	trg := httpTrigger(t)
	inv := &fakeTriggerInvoker{trg: trg, rec: &domainfunctions.ExecutionRecord{
		Status: domainfunctions.ExecutionStatusFailed, Response: `{"err":"boom"}`,
	}}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", "x", nil)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "boom")
}

// TestFunctionTriggersHandler_DisabledTrigger404 禁用触发器对外 404（与
// 不存在同形态，不泄露存在性）。
func TestFunctionTriggersHandler_DisabledTrigger404(t *testing.T) {
	trg := httpTrigger(t)
	trg.Enabled = false
	inv := &fakeTriggerInvoker{trg: trg}
	lim := &fakeTriggerLimiter{allowed: true}
	_, mux := newTriggerTestHandler(t, inv, lim)

	rec := doTrigger(mux, http.MethodPost, "/f/p1/tok1", "x", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}
