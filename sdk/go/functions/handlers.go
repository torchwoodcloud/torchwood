package functions

// handler 注册面（Mux）与单源便捷封装（Start*）：全部构建在 v4 统一入口
// NewServer 之上（同协议、验签必经）。分发维度收敛为协议轴——invoke /
// http.request 走 RPC 轴 method（rpc.go），cron / event 走事件轴 CloudEvents
// type（cloudevents.go）；旧 runner 契约的 TW_DATA 明文分发与 x-tw-source
// 前缀路由已退役。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// CronHandler 是 cron 触发源的处理签名（Mux.Cron 注册；StartCron 单源糖）。
type CronHandler func(ctx context.Context, tick CronTick) error

// EventHandler 是事件触发源的处理签名（Mux.Event 注册；StartEvent 单源糖）。
type EventHandler func(ctx context.Context, ch DocumentChange) error

// maxOutputBytes 是 http.request 的 fetch 语义响应 body 截断上限（64KB，
// 平台 maxOutputBytes 口径；整体响应另有平台侧 1MiB 读取上限兜底）。
const maxOutputBytes = 64 << 10

// —— 多源显式组合 ——

// muxSnapshot 是 Mux 的分发面快照（分发期单次读取，避免持锁回调 handler）。
type muxSnapshot struct {
	invoke func(ctx context.Context, data json.RawMessage) (any, error)
	cron   CronHandler
	event  EventHandler
	fetch  http.Handler
}

// Mux 是多触发源显式组合器（单端点多轴；复用 ServeMux/chi 心智）：纯注册
// 面（非 http.Handler），由 NewServer / Listen 消费——对应轴 handler 未注册
// → RPC 轴 -32601 / 事件轴 400。零值不可用（经 NewMux 构建），重复注册后者
// 覆盖。
type Mux struct {
	mu     sync.RWMutex
	invoke func(ctx context.Context, data json.RawMessage) (any, error)
	cron   CronHandler
	event  EventHandler
	fetch  http.Handler
}

// NewMux 构建多源组合器。
func NewMux() *Mux { return &Mux{} }

// Invoke 注册 invoke 源（RPC 轴 method=invoke：client / server 调用面）。
// 非 generic（Go 方法不能带类型参数）：data 为函数载荷原始 JSON（params；
// 缺省空对象）；单源强类型请用 StartInvoke。
func (m *Mux) Invoke(fn func(ctx context.Context, data json.RawMessage) (any, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invoke = fn
}

// Cron 注册 cron 触发源（事件轴 type=cron.tick，任意 trigger id 命中）。
func (m *Mux) Cron(fn CronHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cron = fn
}

// Event 注册事件触发源（事件轴 type=事件目录名，任意 trigger id 命中）。
func (m *Mux) Event(fn EventHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.event = fn
}

// Fetch 注册 HTTP 触发源（RPC 轴 method=http.request，任意 trigger id 命中）。
func (m *Mux) Fetch(h http.Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fetch = h
}

func (m *Mux) snapshot() muxSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return muxSnapshot{invoke: m.invoke, cron: m.cron, event: m.event, fetch: m.fetch}
}

// —— 单源便捷封装（= NewMux 注册 + Listen；验签密钥必传） ——

// invokeAdapter 把强类型 handler 适配为 Mux.Invoke 形态（params 经
// json.Unmarshal 解码为 Req；解码形状不匹配 → error → -32000 error 对象）。
func invokeAdapter[Req, Resp any](fn func(ctx context.Context, req Req) (Resp, error)) func(ctx context.Context, data json.RawMessage) (any, error) {
	return func(ctx context.Context, data json.RawMessage) (any, error) {
		var req Req
		if err := json.Unmarshal(data, &req); err != nil {
			return nil, fmt.Errorf("decode invoke params into request: %w", err)
		}
		return fn(ctx, req)
	}
}

// StartInvoke 以 invoke 单源启动常驻服务并阻塞（便捷封装 = NewMux + Invoke
// + Listen）：params 经 json.Unmarshal 解码为 Req（缺省空对象 → 零值），
// 返回值 marshal 进 result；解码形状不匹配 / handler 返回 error / panic →
// error 对象（-32000 / -32602）。secrets 为函数侧签名密钥集（轮换窗口内新
// 旧并存），至少一个（空集 fail-closed）。
func StartInvoke[Req, Resp any](fn func(ctx context.Context, req Req) (Resp, error), secrets ...string) error {
	mux := NewMux()
	mux.Invoke(invokeAdapter(fn))
	return Listen(mux, secrets...)
}

// StartCron 以 cron 单源启动常驻服务并阻塞：CloudEvents cron.tick 通知的
// data 解析为 CronTick；返回 error 或 panic → 400（投递失败信号，进重投）。
func StartCron(fn CronHandler, secrets ...string) error {
	mux := NewMux()
	mux.Cron(fn)
	return Listen(mux, secrets...)
}

// StartEvent 以事件单源启动常驻服务并阻塞：CloudEvents 通知的 data 解析为
// DocumentChange；返回 error 或 panic → 400（投递失败信号，进重投）。
func StartEvent(fn EventHandler, secrets ...string) error {
	mux := NewMux()
	mux.Event(fn)
	return Listen(mux, secrets...)
}

// StartHTTP 以 fetch 单源启动常驻服务并阻塞：http.request 分发还原为真
// *http.Request（url = http://trigger{path}?{raw_query}，headers 原样），
// 响应捕获为 httpResult（status/headers/body_base64）。
func StartHTTP(h http.Handler, secrets ...string) error {
	mux := NewMux()
	mux.Fetch(h)
	return Listen(mux, secrets...)
}

// —— fetch 风格（RPC 轴 http.request）请求还原与响应捕获 ——

// responseHeaderBlocklist 是 fetch 语义结果的响应头过滤清单（协议 §4.1，
// 与平台 HTTPResult「Headers 已滤 hop-by-hop」口径一致）：hop-by-hop + 平台
// 头。content-length 由分发链路按实际 body 重算；date/server/host 不得由
// 用户代码冒充；其余头（含 content-type）原样透传。
var responseHeaderBlocklist = map[string]bool{
	"connection":        true,
	"keep-alive":        true,
	"proxy-connection":  true,
	"te":                true,
	"trailer":           true,
	"transfer-encoding": true,
	"upgrade":           true,
	"content-length":    true,
	"host":              true,
	"date":              true,
	"server":            true,
}

// invokeFetch 安全执行 fetch handler：按封套还原触发器请求（ctx 携带执行身
// 份）→ 录制响应 → 捕获为 httpResult。还原失败 / handler panic / 捕获失败
// 统一归 error（上层映射 -32000 "http request failed"）。
func invokeFetch(ctx context.Context, fetch http.Handler, env *triggerEnvelope, body []byte) (result httpResult, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result, err = httpResult{}, fmt.Errorf("fetch handler panic: %v", rec)
		}
	}()
	req, err := buildTriggerRequest(ctx, env, body)
	if err != nil {
		return httpResult{}, err
	}
	rec := httptest.NewRecorder()
	fetch.ServeHTTP(rec, req)
	return fetchResult(rec), nil
}

// buildTriggerRequest 还原 fetch 风格请求（http.request params.envelope）：
// url = http://trigger{path}?{raw_query}、headers 原样、body = 解码后的触发
// 器原始 body（GET/HEAD 不允许 body）；method 大写化、缺省 GET。
func buildTriggerRequest(ctx context.Context, env *triggerEnvelope, body []byte) (*http.Request, error) {
	method := strings.ToUpper(env.Method)
	if method == "" {
		method = http.MethodGet
	}
	rawURL := "http://trigger" + orRoot(env.Path)
	if env.RawQuery != "" {
		rawURL += "?" + env.RawQuery
	}
	var reader io.Reader
	if method != http.MethodGet && method != http.MethodHead && len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for k, vals := range env.Headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

func orRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// fetchResult 把捕获的响应序列化为 httpResult（协议 §4.1 result 形状）：
// 64KB 截断 + 头过滤 + body_base64 无损。
func fetchResult(rec *httptest.ResponseRecorder) httpResult {
	body := rec.Body.Bytes()
	if len(body) > maxOutputBytes {
		body = body[:maxOutputBytes]
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
	return httpResult{
		Status:  rec.Code,
		Headers: headers,
		BodyB64: base64.StdEncoding.EncodeToString(body),
	}
}
