package functions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// noopHandler 是不做事的 fetch 风格 handler（守卫类测试用）。
var noopHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok"))
})

func newTestServer(t *testing.T, h contractHandler) *httptest.Server {
	t.Helper()
	c := &contract{handler: h, exit: func(int) {}, maxRequests: parseMaxRequests()}
	ts := httptest.NewServer(c)
	t.Cleanup(ts.Close)
	return ts
}

func doJSON(t *testing.T, method, url string, headers map[string]string, body string) (*http.Response, string) {
	t.Helper()
	var reader io.Reader
	if body != "" || method == http.MethodPost {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(raw)
}

func decodeEnvelope(t *testing.T, body string) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", body, err)
	}
	return env
}

// —— 契约守卫：health / 405 / 404 / 413 ——

func TestContractGuards(t *testing.T) {
	ts := newTestServer(t, resolveContract(noopHandler))

	// health：200，就绪恒 ready。
	resp, body := doJSON(t, http.MethodGet, ts.URL+healthPath, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}
	env := decodeEnvelope(t, body)
	if env["ok"] != true || env["ready"] != true {
		t.Fatalf("health envelope = %s, want ok/ready true", body)
	}
	if _, ok := env["served"]; !ok {
		t.Fatalf("health envelope missing served: %s", body)
	}

	// GET / → 405 + Allow: POST。
	resp, body = doJSON(t, http.MethodGet, ts.URL+"/", nil, "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET / status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
		t.Fatalf("GET / Allow = %q, want POST", allow)
	}
	if msg := decodeEnvelope(t, body)["error"]; msg == "" {
		t.Fatalf("405 envelope missing error: %s", body)
	}

	// POST /_tw/health → 405 + Allow: GET。
	resp, _ = doJSON(t, http.MethodPost, ts.URL+healthPath, nil, "{}")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST health status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
		t.Fatalf("POST health Allow = %q, want GET", allow)
	}

	// 未知路径 → 404。
	resp, body = doJSON(t, http.MethodGet, ts.URL+"/nope", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /nope status = %d, want 404", resp.StatusCode)
	}
	if msg := decodeEnvelope(t, body)["error"]; msg != "not found" {
		t.Fatalf("404 envelope error = %v, want not found", msg)
	}

	// body 超 4MB → 413（不计 served）。
	big := strings.Repeat("x", maxBodyBytes+1)
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/", nil, big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", resp.StatusCode)
	}
	if msg := decodeEnvelope(t, body)["error"]; msg != "request body too large" {
		t.Fatalf("413 envelope error = %v", msg)
	}

	// health 的 served 计数不受守卫路径影响（413/404/405 不计数）。
	_, body = doJSON(t, http.MethodGet, ts.URL+healthPath, nil, "")
	if served := decodeEnvelope(t, body)["served"]; served != float64(0) {
		t.Fatalf("served = %v, want 0 (guards are uncounted)", served)
	}
}

// —— main 风格契约：TW_DATA 非法 JSON → 400（计数） ——

func TestContractInvalidTWData400(t *testing.T) {
	handler := mainAdapter(func(_ context.Context, _ json.RawMessage) (any, error) {
		t.Fatal("handler must not run on invalid TW_DATA")
		return nil, nil
	})
	ts := newTestServer(t, handler)
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", nil, "{not json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg, _ := decodeEnvelope(t, body)["error"].(string); !strings.Contains(msg, "invalid TW_DATA JSON") {
		t.Fatalf("error = %q, want contains invalid TW_DATA JSON", msg)
	}
	// 400 计入 served（runner 同款）。
	_, body = doJSON(t, http.MethodGet, ts.URL+healthPath, nil, "")
	if served := decodeEnvelope(t, body)["served"]; served != float64(1) {
		t.Fatalf("served = %v, want 1", served)
	}
}

// —— per-request 超时：到点 500 封套 + 放弃等待 + 残跑不泄漏 ——

func TestContractTimeoutAbandons(t *testing.T) {
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("late"))
		<-release // 超时后仍残跑
	})
	ts := newTestServer(t, fetchAdapter{inner: handler})

	start := time.Now()
	// runner.js 的 Number() 语义：小数秒合法。
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/", map[string]string{"X-Tw-Timeout-Seconds": "0.2"}, "data")
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	msg, _ := decodeEnvelope(t, body)["error"].(string)
	if !strings.Contains(msg, "timed out") {
		t.Fatalf("error = %q, want contains timed out", msg)
	}
	if elapsed > 1200*time.Millisecond {
		t.Fatalf("timeout abandonment took %s, want well under handler runtime", elapsed)
	}

	// 放弃后 inflight 已按请求生命周期释放；残跑 goroutine 结束不产生二次
	// 响应/计数（缓冲 1 channel）。
	_, body = doJSON(t, http.MethodGet, ts.URL+healthPath, nil, "")
	env := decodeEnvelope(t, body)
	if env["inflight"] != float64(0) {
		t.Fatalf("inflight = %v, want 0 (released at abandonment)", env["inflight"])
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	_, body = doJSON(t, http.MethodGet, ts.URL+healthPath, nil, "")
	env = decodeEnvelope(t, body)
	if env["served"] != float64(1) {
		t.Fatalf("served = %v, want 1 (abandoned result dropped)", env["served"])
	}
}

// —— TW_MAX_REQUESTS：每响应后计数达标自退出（Flush 后 Exit） ——

func TestMaxRequestsAutoExit(t *testing.T) {
	t.Setenv("TW_MAX_REQUESTS", "2")
	var mu sync.Mutex
	var exits []int
	c := newContract(noopHandler, func(code int) {
		mu.Lock()
		defer mu.Unlock()
		exits = append(exits, code)
	})
	post := func() {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
		c.ServeHTTP(httptest.NewRecorder(), req)
	}
	post()
	mu.Lock()
	if len(exits) != 0 {
		t.Fatalf("exit called too early: %v", exits)
	}
	mu.Unlock()
	post() // served==2 → 自退出
	mu.Lock()
	defer mu.Unlock()
	if len(exits) != 1 || exits[0] != 0 {
		t.Fatalf("exits = %v, want [0] after 2nd response", exits)
	}
}

func TestMaxRequestsDisabledOnZero(t *testing.T) {
	t.Setenv("TW_MAX_REQUESTS", "0") // 显式 0 = 关闭
	var exits []int
	c := newContract(noopHandler, func(code int) { exits = append(exits, code) })
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
		c.ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(exits) != 0 {
		t.Fatalf("exits = %v, want none (disabled)", exits)
	}
}

// —— SIGTERM drain：停止接新 + 等在途 + 兜底强退 ——

// TestDrainStateRejectsNewRequests 直接驱动 drain 状态机（平台无关；
// SIGTERM 信号投递本身是 Linux/POSIX 语义，端到端形态见
// TestListenSigtermDrainEndToEnd——Windows 跳过）。
func TestDrainStateRejectsNewRequests(t *testing.T) {
	c := newContract(noopHandler, func(int) {})
	c.draining.Store(true)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if msg := decodeEnvelope(t, rec.Body.String())["error"]; msg != "instance draining" {
		t.Fatalf("error = %v, want instance draining", msg)
	}

	// health 在 drain 期仍 200（runner 同款：探活不因 drain 翻脸）。
	hw := httptest.NewRecorder()
	c.ServeHTTP(hw, httptest.NewRequest(http.MethodGet, healthPath, nil))
	if hw.Code != http.StatusOK {
		t.Fatalf("health during drain = %d, want 200", hw.Code)
	}
}

// TestListenSigtermDrainEndToEnd 走真实 serve 循环：health → 慢请求在途 →
// SIGTERM → 停止接新（503）→ 在途完成 → exit(0)。信号投递为 Linux/POSIX
// 语义，Windows 跳过（drain 状态机已由 TestDrainStateRejectsNewRequests
// 平台无关覆盖）。
func TestListenSigtermDrainEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM 不可投递于 Windows（Linux/POSIX 语义）；drain 语义由注入通道测试覆盖")
	}
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("slow-done"))
	})
	sig := make(chan os.Signal, 1)
	addrCh := make(chan string, 1)
	exited := make(chan int, 1)
	t.Setenv("TW_RUNNER_PORT", "0") // 临时端口
	t.Setenv("TW_DRAIN_TIMEOUT_MS", "5000")
	go func() {
		_ = serve(resolveContract(slow), serveOpts{exit: func(code int) { exited <- code }, addrCh: addrCh, sig: sig})
	}()
	base := "http://" + strings.Replace(<-addrCh, "0.0.0.0", "127.0.0.1", 1)

	// 就绪探针。
	resp, _ := doJSON(t, http.MethodGet, base+healthPath, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d, want 200", resp.StatusCode)
	}

	// 在途慢请求（异步）。
	slowDone := make(chan int, 1)
	go func() {
		r, _ := doJSON(t, http.MethodPost, base+"/", nil, `{}`)
		slowDone <- r.StatusCode
	}()
	time.Sleep(50 * time.Millisecond) // 让慢请求进入 handler

	sig <- syscall.SIGTERM

	// 停止接新：新分发请求 503。
	resp, body := doJSON(t, http.MethodPost, base+"/", nil, `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("post-sigterm status = %d, want 503", resp.StatusCode)
	}
	if msg := decodeEnvelope(t, body)["error"]; msg != "instance draining" {
		t.Fatalf("error = %v, want instance draining", msg)
	}

	// 在途请求被等完成后正常收场。
	if code := <-slowDone; code != http.StatusOK {
		t.Fatalf("slow request status = %d, want 200 (drained)", code)
	}
	// drain 完成 → exit(0)。
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not exit within 5s")
	}
}

// —— env 解析单元 ——

func TestParseTimeoutSeconds(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", defaultRequestTimeout},
		{"abc", defaultRequestTimeout},
		{"0", defaultRequestTimeout},
		{"-3", defaultRequestTimeout},
		{"30", 30 * time.Second},
		{"0.2", 200 * time.Millisecond},
		{" 2 ", 2 * time.Second},
	}
	for _, tc := range cases {
		if got := parseTimeoutSeconds(tc.header); got != tc.want {
			t.Errorf("parseTimeoutSeconds(%q) = %s, want %s", tc.header, got, tc.want)
		}
	}
}

func TestParseMaxRequests(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", defaultMaxRequests}, // 缺省 1000（runner.js 同款）
		{"0", 0},
		{"-1", 0},
		{"abc", 0},
		{"5", 5},
	}
	for _, tc := range cases {
		t.Setenv("TW_MAX_REQUESTS", tc.env)
		if got := parseMaxRequests(); got != tc.want {
			t.Errorf("parseMaxRequests(%q) = %d, want %d", tc.env, got, tc.want)
		}
	}
}

func TestDrainTimeout(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultDrainTimeout},
		{"abc", defaultDrainTimeout},
		{"0", defaultDrainTimeout},
		{"250", 250 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Setenv("TW_DRAIN_TIMEOUT_MS", tc.env)
		if got := drainTimeout(); got != tc.want {
			t.Errorf("drainTimeout(%q) = %s, want %s", tc.env, got, tc.want)
		}
	}
}

func TestParseTriggerEnvelope(t *testing.T) {
	if parseTriggerEnvelope("") != nil {
		t.Fatal("empty header must yield nil envelope")
	}
	if parseTriggerEnvelope("!!!not base64") != nil {
		t.Fatal("invalid base64 must yield nil envelope")
	}
	if parseTriggerEnvelope("bm90IGpzb24=") != nil { // "not json"
		t.Fatal("invalid json must yield nil envelope")
	}
	enc := base64Of(`{"method":"put","path":"/x","raw_query":"a=1","headers":{"k":["v"]}}`)
	env := parseTriggerEnvelope(enc)
	if env == nil || env.Method != "put" || env.Path != "/x" || env.RawQuery != "a=1" || len(env.Headers["k"]) != 1 {
		t.Fatalf("envelope = %+v", env)
	}
}

func base64Of(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
