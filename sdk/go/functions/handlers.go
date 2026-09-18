package functions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
)

// CronHandler 是 cron 触发源的处理签名（Mux.Cron 注册；StartCron 单源糖）。
type CronHandler func(ctx context.Context, tick CronTick) error

// EventHandler 是事件触发源的处理签名（Mux.Event 注册；StartEvent 单源糖）。
type EventHandler func(ctx context.Context, ch DocumentChange) error

// invokeHandler 是 StartInvoke 的内部形态（Start = Listen(…) 的糖，测试经
// 此直接全链验证）。
func invokeHandler[Req, Resp any](fn func(ctx context.Context, req Req) (Resp, error)) contractHandler {
	return mainAdapter(func(ctx context.Context, data json.RawMessage) (any, error) {
		var req Req
		if err := json.Unmarshal(data, &req); err != nil {
			return nil, fmt.Errorf("decode TW_DATA into request: %w", err)
		}
		return fn(ctx, req)
	})
}

// StartInvoke 以 main 风格启动 invoke 函数并阻塞（等价 Listen(适配后
// handler)）：TW_DATA 经 json.Unmarshal 解码为 Req（分发 body 为空对象时
// Req 取零值），Resp marshal 进 result。TW_DATA 非法 JSON → 400；解码形状
// 不匹配 / handler 返回 error / panic → 500 封套。client/server 面调用
// （source 非 trigger 前缀）编译期契约。
func StartInvoke[Req, Resp any](fn func(ctx context.Context, req Req) (Resp, error)) error {
	return serve(invokeHandler(fn), serveOpts{exit: os.Exit})
}

// StartCron 以 main 风格启动 cron 函数并阻塞：TW_DATA
// （{type:"cron",trigger_id,scheduled_for}）解析为 CronTick；返回 error 或
// panic → 500 封套。
func StartCron(fn CronHandler) error {
	return serve(cronAdapter(fn), serveOpts{exit: os.Exit})
}

// StartEvent 以 main 风格启动事件函数并阻塞：TW_DATA（事件投影，形状对照
// 平台 EventInvocationData）解析为 DocumentChange；返回 error 或 panic →
// 500 封套。
func StartEvent(fn EventHandler) error {
	return serve(eventAdapter(fn), serveOpts{exit: os.Exit})
}

// StartHTTP 以 fetch 风格启动函数并阻塞：每次分发还原为真 *http.Request
// （有触发器封套按封套还原；无封套 = POST http://function/、body = TW_DATA），
// 响应经录制转 fetch 封套 {ok,status,headers,body_base64,truncated}。
func StartHTTP(h http.Handler) error {
	return serve(fetchAdapter{inner: h}, serveOpts{exit: os.Exit})
}

// —— main 风格适配（TW_DATA 进出）——

// mainAdapter 是 main 风格统一内部形态：TW_DATA（RawMessage）→ 结果/
// error。错误语义：TW_DATA 非法 JSON → 400（twData 内裁定）；handler 返回
// error → 500 封套。
type mainAdapter func(ctx context.Context, data json.RawMessage) (any, error)

func (f mainAdapter) serveContract(ctx context.Context, d *dispatchInfo) contractResult {
	data, status, err := twData(d)
	if err != nil {
		return contractResult{status: status, envelope: errEnvelope(err.Error())}
	}
	out, err := f(ctx, data)
	if err != nil {
		return contractResult{status: http.StatusInternalServerError, envelope: errEnvelope(err.Error())}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return contractResult{
			status:   http.StatusInternalServerError,
			envelope: errEnvelope("handler result is not JSON-serializable: " + err.Error()),
		}
	}
	return contractResult{status: http.StatusOK, envelope: &mainEnvelope{OK: true, Result: raw}}
}

// twData 组装 main 风格 TW_DATA（对照 runner.js buildTWData）：无封套 =
// 分发 body 即 TW_DATA JSON（空 → "{}"，零值安全；非法 JSON → 400）；
// 有封套 = 封套注入（元数据 + body 重组，与 runner 逐字段同构）。
func twData(d *dispatchInfo) (json.RawMessage, int, error) {
	if d.envelope == nil {
		if len(strings.TrimSpace(string(d.rawBody))) == 0 {
			return json.RawMessage("{}"), 0, nil
		}
		if !json.Valid(d.rawBody) {
			return nil, http.StatusBadRequest, errors.New("invalid request: invalid TW_DATA JSON")
		}
		return json.RawMessage(d.rawBody), 0, nil
	}
	headers := d.envelope.Headers
	if headers == nil {
		headers = map[string][]string{}
	}
	raw, err := json.Marshal(&envelopeData{
		Method:     d.envelope.Method,
		Path:       d.envelope.Path,
		RawQuery:   d.envelope.RawQuery,
		Headers:    headers,
		Body:       string(d.rawBody), // 非法 UTF-8 由 json.Marshal 归一 U+FFFD（runner best-effort 同款）
		BodyBase64: base64.StdEncoding.EncodeToString(d.rawBody),
	})
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("build TW_DATA envelope: %w", err)
	}
	return raw, 0, nil
}

// envelopeData 是 main 风格封套注入后的 TW_DATA 形状（与 runner buildTWData
// 逐字段同构；HTTP 触发器打在 main 风格函数上时，Req 按此形状解码）。
type envelopeData struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	RawQuery   string              `json:"raw_query"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	BodyBase64 string              `json:"body_base64"`
}

// cronAdapter / eventAdapter 把 typed handler 适配为 main 风格（形状解析
// 失败 → error → 500 封套；JSON 语法错误已在 twData → 400）。
func cronAdapter(fn CronHandler) contractHandler {
	return mainAdapter(func(ctx context.Context, data json.RawMessage) (any, error) {
		tick, err := parseCronTick(data)
		if err != nil {
			return nil, err
		}
		return nil, fn(ctx, tick)
	})
}

func eventAdapter(fn EventHandler) contractHandler {
	return mainAdapter(func(ctx context.Context, data json.RawMessage) (any, error) {
		ch, err := parseDocumentChange(data)
		if err != nil {
			return nil, err
		}
		return nil, fn(ctx, ch)
	})
}

// —— fetch 风格适配（*http.Request 进出）——

type fetchAdapter struct{ inner http.Handler }

func (f fetchAdapter) serveContract(ctx context.Context, d *dispatchInfo) contractResult {
	req, status, err := buildTriggerRequest(ctx, d)
	if err != nil {
		return contractResult{status: status, envelope: errEnvelope(err.Error())}
	}
	rec := httptest.NewRecorder()
	f.inner.ServeHTTP(rec, req)
	return fetchResult(rec)
}

// buildTriggerRequest 还原 fetch 风格请求（对照 runner.js buildRequest）：
// 无封套 → POST http://function/、body = 分发 body（TW_DATA）；有封套 →
// url = http://trigger{path}?{raw_query}、headers 原样、body = 分发 body
// （触发器原始 body；GET/HEAD 不允许 body）。请求 ctx 携带执行身份。
func buildTriggerRequest(ctx context.Context, d *dispatchInfo) (*http.Request, int, error) {
	if d.envelope == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://function/", bytes.NewReader(d.rawBody))
		if err != nil {
			return nil, http.StatusInternalServerError, errors.New("build request: " + err.Error())
		}
		req.Header.Set("Content-Type", "application/json")
		return req, 0, nil
	}
	env := d.envelope
	method := strings.ToUpper(env.Method)
	if method == "" {
		method = http.MethodGet
	}
	rawURL := "http://trigger" + orRoot(env.Path)
	if env.RawQuery != "" {
		rawURL += "?" + env.RawQuery
	}
	var body io.Reader
	if method != http.MethodGet && method != http.MethodHead && len(d.rawBody) > 0 {
		body = bytes.NewReader(d.rawBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("invalid request: " + err.Error())
	}
	for k, vals := range env.Headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	return req, 0, nil
}

func orRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// fetchResult 把捕获的响应序列化为 fetch 封套（对照 runner.js
// finishFetchResponse）：64KB 截断 + 头过滤 + body_base64 无损；HTTP status
// 恒 200（函数自己的 status 在封套 status 字段）。
func fetchResult(rec *httptest.ResponseRecorder) contractResult {
	body := rec.Body.Bytes()
	truncated := false
	if len(body) > maxOutputBytes {
		body = body[:maxOutputBytes]
		truncated = true
	}
	headers := make(map[string]string, len(rec.Header()))
	for name, vals := range rec.Header() {
		ln := strings.ToLower(name)
		if responseHeaderBlocklist[ln] {
			continue
		}
		// 同名多值合并为逗号连接（fetch Headers combined 值语义）。
		headers[ln] = strings.Join(vals, ", ")
	}
	return contractResult{
		status: http.StatusOK,
		envelope: &fetchEnvelope{
			OK:        true,
			Status:    rec.Code,
			Headers:   headers,
			BodyB64:   base64.StdEncoding.EncodeToString(body),
			Truncated: truncated,
		},
	}
}

// —— 多源显式组合 ——

// Mux 是多触发源显式组合器（单二进制多源；复用 ServeMux/chi 心智）：按
// x-tw-source 前缀分发——cron:{id} → Cron、event:{id} → Event、http:{id} →
// Fetch、其余（server/client）→ Invoke；对应源未注册 → 500 明确错误。
// 实现 http.Handler，但仅可在 functions.Listen 内被服务（契约循环经 ctx
// 提供分发信息）；零值可用，重复注册后者覆盖。
type Mux struct {
	mu     sync.RWMutex
	invoke func(ctx context.Context, data json.RawMessage) (any, error)
	cron   CronHandler
	event  EventHandler
	fetch  http.Handler
}

// NewMux 构建多源组合器。
func NewMux() *Mux { return &Mux{} }

// Invoke 注册 invoke 源（server/client 面调用）。非 generic（Go 方法不能带
// 类型参数）：data 为 TW_DATA 原始 JSON；单源强类型请用 StartInvoke。
func (m *Mux) Invoke(fn func(ctx context.Context, data json.RawMessage) (any, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invoke = fn
}

// Cron 注册 cron 触发源（任意 trigger id 命中）。
func (m *Mux) Cron(fn CronHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cron = fn
}

// Event 注册事件触发源（任意 trigger id 命中）。
func (m *Mux) Event(fn EventHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.event = fn
}

// Fetch 注册 HTTP 触发源（任意 trigger id 命中）。
func (m *Mux) Fetch(h http.Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fetch = h
}

func (m *Mux) serveContract(ctx context.Context, d *dispatchInfo) contractResult {
	source := FromContext(ctx).Source
	m.mu.RLock()
	invoke, cron, event, fetch := m.invoke, m.cron, m.event, m.fetch
	m.mu.RUnlock()
	notRegistered := func(name string) contractResult {
		return contractResult{
			status:   http.StatusInternalServerError,
			envelope: errEnvelope(fmt.Sprintf("no %s handler registered (source %q)", name, source)),
		}
	}
	switch {
	case strings.HasPrefix(source, "cron:"):
		if cron == nil {
			return notRegistered("Cron")
		}
		return cronAdapter(cron).serveContract(ctx, d)
	case strings.HasPrefix(source, "event:"):
		if event == nil {
			return notRegistered("Event")
		}
		return eventAdapter(event).serveContract(ctx, d)
	case strings.HasPrefix(source, "http:"):
		if fetch == nil {
			return notRegistered("Fetch")
		}
		return fetchAdapter{inner: fetch}.serveContract(ctx, d)
	default:
		if invoke == nil {
			return notRegistered("Invoke")
		}
		return mainAdapter(invoke).serveContract(ctx, d)
	}
}

// ServeHTTP 实现 http.Handler（Listen(mux) 的入口形态）。脱离 Listen 直接
// 调用（无契约上下文）→ 500 明确错误。
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d, ok := dispatchFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errEnvelope("functions: Mux must be served via functions.Listen"))
		return
	}
	res := m.serveContract(r.Context(), d)
	writeJSON(w, res.status, res.envelope)
}
