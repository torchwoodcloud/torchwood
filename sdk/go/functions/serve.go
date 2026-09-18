package functions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// 运行契约常量（对照 runner.js 顶部口径）。
const (
	defaultPort           = 18080            // TW_RUNNER_PORT 缺省值
	defaultRequestTimeout = 30 * time.Second // x-tw-timeout-seconds 缺省值
	defaultDrainTimeout   = 10 * time.Second // TW_DRAIN_TIMEOUT_MS 缺省值
	defaultMaxRequests    = 1000             // TW_MAX_REQUESTS 缺省值（runner.js Number(env || 1000)）
	maxBodyBytes          = 4 << 20          // 请求体上限 4MB（触发器封套双编码口径）
	maxOutputBytes        = 64 << 10         // fetch 响应 body 截断上限（平台 maxOutputBytes）
	healthPath            = "/_tw/health"    // 启动探针
	dispatchPath          = "/"              // 分发端点
	serverSourceFallback  = "server"         // x-tw-source 缺省回落
)

// 分发 header（协议公开契约；未知 x-tw-* header 一律忽略——协议演进宪法，
// 见包文档）。
const (
	hdrExecutionToken  = "X-Tw-Execution-Token"
	hdrExecutionID     = "X-Tw-Execution-Id"
	hdrSource          = "X-Tw-Source"
	hdrInvokingUserID  = "X-Tw-Invoking-User-Id"
	hdrProjectID       = "X-Tw-Project-Id"
	hdrTriggerEnvelope = "X-Tw-Trigger-Envelope"
	hdrTimeoutSeconds  = "X-Tw-Timeout-Seconds"
)

// responseHeaderBlocklist 是 fetch 封套的响应头过滤清单（对照 runner.js
// RESPONSE_HEADER_BLOCKLIST）：hop-by-hop + 平台头。content-length 由分发
// 链路按实际 body 重算；date/server/host 不得由用户代码冒充；其余头（含
// content-type）原样透传。
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

// triggerEnvelope 是触发器封套元数据（x-tw-trigger-envelope：base64 JSON，
// 不含 body；形状对照 internal/domain/functions TriggerEnvelope——只读对齐，
// SDK 自持结构体）。解析失败视为不存在（内网可信调用方，runner 同款）。
type triggerEnvelope struct {
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	RawQuery string              `json:"raw_query"`
	Headers  map[string][]string `json:"headers"`
}

func parseTriggerEnvelope(header string) *triggerEnvelope {
	if header == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return nil
	}
	var env triggerEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	return &env
}

// dispatchInfo 是一次分发请求的平台侧信息（body + 封套），由契约循环构建、
// 经参数/ctx 交给风格适配层。
type dispatchInfo struct {
	rawBody  []byte
	envelope *triggerEnvelope
}

// mainEnvelope 是 main 风格响应封套（成功含 result，失败含 error；stdout/
// stderr 恒空串——Go 无 per-request console 捕获，字段保留以对齐 runner
// 响应契约）。
type mainEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Stdout string          `json:"stdout"`
	Stderr string          `json:"stderr"`
	Error  string          `json:"error,omitempty"`
}

func errEnvelope(msg string) *mainEnvelope {
	return &mainEnvelope{OK: false, Error: msg}
}

// fetchEnvelope 是 fetch 风格响应封套（v4 §2.2/§2.4）：status/headers/
// body_base64 恒在（无损二进制）；headers 仅函数设置的（过滤清单见
// responseHeaderBlocklist）；body 超 64KB 截断标 truncated。
type fetchEnvelope struct {
	OK        bool              `json:"ok"`
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	BodyB64   string            `json:"body_base64"`
	Truncated bool              `json:"truncated"`
	Stdout    string            `json:"stdout"`
	Stderr    string            `json:"stderr"`
}

// healthEnvelope 是启动探针响应（就绪后恒 ready；served/inflight 观测水位）。
type healthEnvelope struct {
	OK       bool  `json:"ok"`
	Ready    bool  `json:"ready"`
	Served   int64 `json:"served"`
	Inflight int64 `json:"inflight"`
}

// contractHandler 是风格适配层的统一内部形态：一次分发 → 响应封套。main
// 风格（StartInvoke/StartCron/StartEvent/Mux.Invoke）与 fetch 风格
// （StartHTTP/Mux.Fetch）各有一实现；Mux 按源前缀逐请求择一。
type contractHandler interface {
	serveContract(ctx context.Context, d *dispatchInfo) contractResult
}

type contractResult struct {
	status   int // 200 成功 / 400 TW_DATA 解析失败 / 500 执行失败
	envelope any // *mainEnvelope 或 *fetchEnvelope
}

// contract 是 runner 契约循环：health 探针、分发守卫（drain/体量上限）、
// 身份注入、per-request 超时放弃、panic 捕获、TW_MAX_REQUESTS 自退出。
// 它本身是 http.Handler，Listen 用它包裹用户的 handler。
type contract struct {
	handler     contractHandler
	exit        func(int)
	maxRequests int
	served      atomic.Int64
	inflight    atomic.Int64
	draining    atomic.Bool
}

// newContract 构建契约循环（TW_MAX_REQUESTS 读 env；exit 注入供测试）。
func newContract(h http.Handler, exit func(int)) *contract {
	if exit == nil {
		exit = os.Exit
	}
	return &contract{handler: resolveContract(h), exit: exit, maxRequests: parseMaxRequests()}
}

// resolveContract 判定风格：实现 serveContract（Mux）→ 契约形态直通；裸
// http.Handler → fetch 风格（StartHTTP 同款语义）。
func resolveContract(h http.Handler) contractHandler {
	if ch, ok := h.(contractHandler); ok {
		return ch
	}
	return fetchAdapter{inner: h}
}

func (c *contract) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch path := r.URL.Path; {
	case path == healthPath && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, &healthEnvelope{
			OK:       true,
			Ready:    true, // Start/Mux 编译期注册，进程活着即就绪
			Served:   c.served.Load(),
			Inflight: c.inflight.Load(),
		})
		return
	case path == healthPath:
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope("method not allowed (probe is GET "+healthPath+")"))
		return
	case path != dispatchPath:
		writeJSON(w, http.StatusNotFound, errEnvelope("not found"))
		return
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, errEnvelope("method not allowed (dispatch is POST "+dispatchPath+")"))
		return
	}

	if c.draining.Load() {
		// drain 中不再接新请求（调用方重试到其他实例，runner 同款）。
		writeJSON(w, http.StatusServiceUnavailable, errEnvelope("instance draining"))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errEnvelope("read request body failed: "+err.Error()))
		return
	}
	if len(body) > maxBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, errEnvelope("request body too large"))
		return
	}

	// 分发信息 + 身份六件（未知 x-tw-* header 天然被忽略——只读已知项）。
	d := &dispatchInfo{rawBody: body, envelope: parseTriggerEnvelope(r.Header.Get(hdrTriggerEnvelope))}
	id := Identity{
		ExecutionToken: r.Header.Get(hdrExecutionToken),
		APIBaseURL:     os.Getenv("TW_API_BASE_URL"),
		ExecutionID:    r.Header.Get(hdrExecutionID),
		Source:         r.Header.Get(hdrSource),
		InvokingUserID: r.Header.Get(hdrInvokingUserID),
		ProjectID:      r.Header.Get(hdrProjectID),
	}
	if id.Source == "" {
		id.Source = serverSourceFallback
	}
	ctx := WithIdentity(r.Context(), id)
	ctx = withDispatch(ctx, d)

	c.inflight.Add(1)
	// 结果 channel 缓冲 1：超时放弃后残跑 goroutine 写入不阻塞、不泄漏
	//（残跑结果被 GC，对齐 runner「inflight 按请求生命周期释放」）。
	resCh := make(chan contractResult, 1)
	go func() {
		defer func() {
			// panic 捕获 → 500 封套，不杀实例（含超时放弃后残跑期 panic——
			// 写缓冲 channel 后静默）。
			if rec := recover(); rec != nil {
				resCh <- contractResult{
					status:   http.StatusInternalServerError,
					envelope: errEnvelope(fmt.Sprintf("panic: %v\n\n%s", rec, debug.Stack())),
				}
			}
		}()
		resCh <- c.handler.serveContract(ctx, d)
	}()

	timeout := parseTimeoutSeconds(r.Header.Get(hdrTimeoutSeconds))
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-resCh:
		c.finish(w, res)
	case <-timer.C:
		// 到点放弃等待：回 500 封套，handler goroutine 残跑（诚实声明与
		// Lambda 同款；僵尸负载兜底在 dispatcher 侧超时熔断）。
		c.finish(w, contractResult{
			status: http.StatusInternalServerError,
			envelope: errEnvelope(fmt.Sprintf(
				"function timed out after %gs (runner abandoned the request; the function may keep running until the instance is recycled)",
				timeout.Seconds())),
		})
	}
}

// finish 收账（inflight-1 / served+1）并写响应；Flush 后按 TW_MAX_REQUESTS
// 自退出（dispatcher 检测退出补位）。
func (c *contract) finish(w http.ResponseWriter, res contractResult) {
	c.inflight.Add(-1)
	served := c.served.Add(1)
	writeJSON(w, res.status, res.envelope)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if c.maxRequests > 0 && served == int64(c.maxRequests) && !c.draining.Load() {
		c.exit(0)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		b = []byte(`{"ok":false,"error":"marshal response envelope failed"}`)
		status = http.StatusInternalServerError
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// parseTimeoutSeconds 解析 per-request 超时 header（秒，可带小数；缺省/非法
// → 30s，runner 同款）。
func parseTimeoutSeconds(header string) time.Duration {
	v, err := strconv.ParseFloat(strings.TrimSpace(header), 64)
	if err != nil || v <= 0 {
		return defaultRequestTimeout
	}
	return time.Duration(v * float64(time.Second))
}

// parseMaxRequests 解析 TW_MAX_REQUESTS：缺省 1000；显式 0/负值/非法 = 关闭
// 自退出（对照 runner.js Number(env || 1000) 的 NaN 语义）。
func parseMaxRequests() int {
	v := os.Getenv("TW_MAX_REQUESTS")
	if v == "" {
		return defaultMaxRequests
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// drainTimeout 解析 TW_DRAIN_TIMEOUT_MS（缺省 10000）。
func drainTimeout() time.Duration {
	if v := os.Getenv("TW_DRAIN_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return defaultDrainTimeout
}

// serveOpts 是 serve 的测试注入口（生产 Listen 全部零值）。
type serveOpts struct {
	exit   func(int)        // 缺省 os.Exit
	addrCh chan<- string    // 监听地址就绪通知（测试）
	sig    <-chan os.Signal // 信号源注入（nil = 安装真实 SIGTERM/SIGINT）
}

// Listen 以 runner 契约启动服务并阻塞（语义同 http.ListenAndServe）：绑定
// TW_RUNNER_PORT（缺省 18080）、SIGTERM/SIGINT → drain（停止接新 + 等在途
// + TW_DRAIN_TIMEOUT_MS 兜底强退，进程退出，正常不返回）。绑定失败返回
// error。多源组合经 Listen(mux)。
func Listen(h http.Handler) error {
	return serve(resolveContract(h), serveOpts{exit: os.Exit})
}

func serve(ch contractHandler, opts serveOpts) error {
	c := &contract{handler: ch, exit: opts.exit, maxRequests: parseMaxRequests()}
	if c.exit == nil {
		c.exit = os.Exit
	}
	port := defaultPort
	if v := os.Getenv("TW_RUNNER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p >= 0 {
			port = p
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return err
	}
	if opts.addrCh != nil {
		opts.addrCh <- ln.Addr().String()
	}
	srv := &http.Server{Handler: c, ReadHeaderTimeout: 10 * time.Second}

	sig := opts.sig
	if sig == nil {
		s := make(chan os.Signal, 1)
		signal.Notify(s, syscall.SIGTERM, os.Interrupt)
		defer signal.Stop(s)
		sig = s
	}
	// drained：Serve 结束后放行信号 goroutine（测试注入形态下进程不退出，
	// 需让 goroutine 可收尾，避免泄漏阻塞测试二进制退出）。
	drained := make(chan struct{})
	go func() {
		select {
		case <-sig:
		case <-drained:
			return
		}
		c.draining.Store(true) // 停止接新：新分发请求立即 503
		ctx, cancel := context.WithTimeout(context.Background(), drainTimeout())
		defer cancel()
		_ = srv.Shutdown(ctx) // 等在途完成；到点兜底强退
		c.exit(0)
	}()

	err = srv.Serve(ln)
	close(drained)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
