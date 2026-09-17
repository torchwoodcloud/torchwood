package gorunner

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// ---- 渲染产物真编译 + 运行协议断言基建 ----
//
// 门槛模型（设计测试策略）：模板渲染产物在临时目录 go build 通过是最低
// 门槛；能跑则跑行为断言（health / 常规 POST / 封套还原 / 超时 /
// TW_MAX_REQUESTS 退出 / SIGTERM drain）。二进制按用户源码缓存复用（一次
// 构建、多测试共享），临时目录在 TestMain 收尾清理。

// twappRoot 是包级临时目录（构建产物缓存根），TestMain 收尾清理。
var twappRoot string

func TestMain(m *testing.M) {
	var err error
	twappRoot, err = os.MkdirTemp("", "gorunner-twapp-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(twappRoot)
	os.Exit(code)
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// goTool 解析本机 go 工具链（GOROOT/bin/go 优先，PATH 兜底）。
func goTool(t *testing.T) string {
	t.Helper()
	if goroot := runtime.GOROOT(); goroot != "" {
		p := filepath.Join(goroot, "bin", "go"+exeSuffix())
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}
	return p
}

var (
	twappMu    sync.Mutex
	twappCache = map[string]string{} // key(入口+用户源码) → 二进制路径
)

// buildTwmainCached 渲染 twmain bootstrap + 用户根包到临时 module 并
// go build（模板产物可编译是最低门槛；同源码多测试共享产物）。
func buildTwmainCached(t *testing.T, userSource string, entryKind Entry) string {
	t.Helper()
	key := strconv.Itoa(int(entryKind)) + ":" + userSource
	twappMu.Lock()
	defer twappMu.Unlock()
	if bin, ok := twappCache[key]; ok {
		return bin
	}
	dir, err := os.MkdirTemp(twappRoot, "mod-*")
	if err != nil {
		t.Fatalf("mkdir module dir: %v", err)
	}
	goMod := "module twtest.fn\n\ngo 1.24\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fn.go"), []byte(userSource), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeGo, mainGo, err := RenderBootstrap("twtest.fn", entryKind)
	if err != nil {
		t.Fatalf("render bootstrap: %v", err)
	}
	twDir := filepath.Join(dir, "twmain")
	if err := os.MkdirAll(twDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(twDir, "runtime.go"), runtimeGo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(twDir, "main.go"), mainGo, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "tw-app"+exeSuffix())
	cmd := exec.Command(goTool(t), "build", "-o", bin, "./twmain")
	cmd.Dir = dir
	// 清空宿主 GOFLAGS / 显式 CGO_ENABLED=0：与构建模板口径一致（纯 stdlib
	// 用户代码无需网络与 cgo）。
	cmd.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build rendered twmain failed: %v\n%s", buildErr, out)
	}
	twappCache[key] = bin
	return bin
}

var protocolTestPort atomic.Int64

func init() { protocolTestPort.Store(18500) }

// startTwapp 启动构建产物并等待 health 就绪，返回 base URL 与进程句柄
// （drain 用例需要发信号）。
func startTwapp(t *testing.T, bin string, extraEnv ...string) (string, *exec.Cmd) {
	t.Helper()
	port := protocolTestPort.Add(1)
	cmd := exec.Command(bin) // #nosec G304 -- 测试自建产物路径（twappRoot 下）
	cmd.Env = append(os.Environ(),
		"TW_RUNNER_PORT="+strconv.Itoa(int(port)),
		"TW_API_BASE_URL=http://tw-api.internal:8080",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tw-app: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitHealth(t, base)
	return base, cmd
}

func waitHealth(t *testing.T, base string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/_tw/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("tw-app never became healthy")
}

// twEnvelope 是 runner 响应封套（与 gorunner 协议对齐；result 用 RawMessage
// 承接任意 JSON）。
type twEnvelope struct {
	Ok        bool              `json:"ok"`
	Result    json.RawMessage   `json:"result"`
	Stdout    string            `json:"stdout"`
	Stderr    string            `json:"stderr"`
	Error     string            `json:"error"`
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	BodyB64   string            `json:"body_base64"`
	Truncated bool              `json:"truncated"`
}

// tryInvoke POST 原始字节并解析封套；连接层错误原样返回（自退出/拒绝
// 连接用例需要区分）。
func tryInvoke(base string, body []byte, headers map[string]string) (int, twEnvelope, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodPost, base+"/", strings.NewReader(string(body)))
	if err != nil {
		return 0, twEnvelope{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, twEnvelope{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var env twEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return resp.StatusCode, twEnvelope{}, err
	}
	return resp.StatusCode, env, nil
}

func invoke(t *testing.T, base string, body []byte, headers map[string]string) (int, twEnvelope) {
	t.Helper()
	status, env, err := tryInvoke(base, body, headers)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return status, env
}

func healthInflightTw(t *testing.T, base string) int {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/_tw/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Inflight int `json:"inflight"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	return body.Inflight
}

// triggerEnvelopeHeader 构造分发封套元数据 header（base64 JSON，与
// dispatcher 侧构建形态同构）。
func triggerEnvelopeHeader(method, path, rawQuery string, headers map[string][]string) string {
	meta, _ := json.Marshal(map[string]any{
		"method":    method,
		"path":      path,
		"raw_query": rawQuery,
		"headers":   headers,
	})
	return base64.StdEncoding.EncodeToString(meta)
}

// ---- 最低门槛：渲染产物可编译 + health ----

func TestGoRuntime_BuildMinimal(t *testing.T) {
	bin := buildTwmainCached(t, srcMainOnly, EntryMain)
	base, _ := startTwapp(t, bin)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/_tw/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Ok       bool `json:"ok"`
		Ready    bool `json:"ready"`
		Inflight int  `json:"inflight"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !body.Ok || !body.Ready || body.Inflight != 0 {
		t.Fatalf("health mismatch: %d %+v", resp.StatusCode, body)
	}
	_ = buildTwmainCached(t, srcFetchOnly, EntryFetch) // fetch 形态产物同样必须可编译
}

// ---- main 风格契约 ----

const srcMainOnly = `package fn

import "errors"

func Main(data map[string]any, ctx map[string]string) (any, error) {
	switch data["n"] {
	case float64(1):
		return map[string]any{
			"doubled": data["n"],
			"token":   ctx["executionToken"],
			"exec":    ctx["executionId"],
			"apiBase": ctx["apiBaseUrl"],
			"source":  ctx["source"],
			"uid":     ctx["invokingUserId"],
			"pid":     ctx["projectId"],
		}, nil
	case float64(2):
		return nil, errors.New("boom from user code")
	case float64(3):
		panic("boom panic")
	}
	return map[string]any{}, nil
}
`

func TestGoRuntime_MainContract(t *testing.T) {
	bin := buildTwmainCached(t, srcMainOnly, EntryMain)
	base, _ := startTwapp(t, bin)

	// 1) TW_DATA → Main(data, ctx)：调用身份六件经分发 header / env 可达；
	//    stdout/stderr 恒空串（Go 无 per-request 捕获，D15）。
	status, env := invoke(t, base, []byte(`{"n":1}`), map[string]string{
		"X-Tw-Execution-Token":  "twx_abc",
		"X-Tw-Execution-Id":     "exec-42",
		"X-Tw-Source":           "client",
		"X-Tw-Invoking-User-Id": "user-1",
		"X-Tw-Project-Id":       "p1",
	})
	if status != 200 || !env.Ok {
		t.Fatalf("status = %d, env = %+v", status, env)
	}
	result := string(env.Result)
	for _, want := range []string{
		`"doubled":1`, `"token":"twx_abc"`, `"exec":"exec-42"`,
		`"apiBase":"http://tw-api.internal:8080"`, `"source":"client"`,
		`"uid":"user-1"`, `"pid":"p1"`,
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("result 缺少 %s: %s", want, result)
		}
	}
	if env.Stdout != "" || env.Stderr != "" {
		t.Fatalf("Go 封套 stdout/stderr 必须恒空串: %+v", env)
	}

	// 2) 用户错误 → 500 {ok:false}（实例保留，进程不退出）。
	status, env = invoke(t, base, []byte(`{"n":2}`), nil)
	if status != 500 || env.Ok || !strings.Contains(env.Error, "boom from user code") {
		t.Fatalf("error envelope mismatch: %d %+v", status, env)
	}

	// 3) 用户 panic → 500 错误封套（panic 不得杀掉常驻实例）。
	status, env = invoke(t, base, []byte(`{"n":3}`), nil)
	if status != 500 || env.Ok || !strings.Contains(env.Error, "boom panic") {
		t.Fatalf("panic envelope mismatch: %d %+v", status, env)
	}

	// 4) 非法 TW_DATA → 400 invalid request（node JSON.parse 错误同款形态）。
	status, env = invoke(t, base, []byte(`not-json`), nil)
	if status != 400 || env.Ok || !strings.Contains(env.Error, "invalid request") {
		t.Fatalf("invalid TW_DATA mismatch: %d %+v", status, env)
	}

	// 5) 未知路径/方法 → 404。
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/other")
	if err != nil {
		t.Fatalf("GET /other: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /other = %d, want 404", resp.StatusCode)
	}
}

// ---- per-request 超时 ----

const srcMainHang = `package fn

import "time"

func Main(data map[string]any, _ map[string]string) (any, error) {
	if _, ok := data["hang"]; ok {
		time.Sleep(10 * time.Minute) // 永不决议（测试视野内）
	}
	return map[string]any{"ok": true}, nil
}
`

func TestGoRuntime_PerRequestTimeout(t *testing.T) {
	bin := buildTwmainCached(t, srcMainHang, EntryMain)
	base, _ := startTwapp(t, bin)

	// 在途计数：挂起请求期间 health 上报 inflight=1。
	done := make(chan struct{})
	go func() {
		defer close(done)
		status, env := invoke(t, base, []byte(`{"hang":true}`), map[string]string{
			"X-Tw-Timeout-Seconds": "1",
		})
		if status != 500 || env.Ok || !strings.Contains(env.Error, "timed out") {
			t.Errorf("超时封套 mismatch: %d %+v", status, env)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if healthInflightTw(t, base) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := healthInflightTw(t, base); got != 1 {
		t.Fatalf("挂起请求期间 inflight = %d, want 1", got)
	}

	// 到点（1s）超时封套返回；放弃等待后 inflight 归零（按请求生命周期释放，
	// 残跑 goroutine 不计入）。
	<-done
	if got := healthInflightTw(t, base); got != 0 {
		t.Fatalf("超时放弃后 inflight = %d, want 0", got)
	}

	// 实例仍健康可继续服务。
	status, env := invoke(t, base, []byte(`{}`), nil)
	if status != 200 || !env.Ok {
		t.Fatalf("超时后实例必须仍可服务: %d %+v", status, env)
	}
}

// ---- fetch 风格：封套还原 + Response 封套 ----

const srcFetchOnly = `package fn

import (
	"encoding/json"
	"io"
	"net/http"
)

func Fetch(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Custom-One", "v1")
	w.Header().Set("Connection", "close") // hop-by-hop：必须被滤
	w.Header().Set("Server", "fake")      // 平台头：必须被滤
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"method":        r.Method,
		"url":           r.URL.String(),
		"path":          r.URL.Path,
		"query":         r.URL.RawQuery,
		"sig":           r.Header.Get("X-Hub-Signature"),
		"body":          string(body),
		"idSource":      r.Header.Get("X-Tw-Source"),
		"idToken":       r.Header.Get("X-Tw-Execution-Token"),
		"idUser":        r.Header.Get("X-Tw-Invoking-User-Id"),
		"idTriggerHdr":  r.Header.Get("X-Tw-Trigger-Envelope") != "",
	})
}
`

func TestGoRuntime_FetchEnvelope(t *testing.T) {
	bin := buildTwmainCached(t, srcFetchOnly, EntryFetch)
	base, _ := startTwapp(t, bin)

	// 1) 触发器封套还原：path/raw_query/headers/body 各归其位；响应封套
	//    status/headers/body_base64 恒在，hop-by-hop 与平台头被滤。
	hdr := triggerEnvelopeHeader(http.MethodPost, "/f/p1/tok1", "signature=abc&ts=9",
		map[string][]string{"x-hub-signature": {"sha"}, "x-tw-source": {"spoofed"}})
	status, env := invoke(t, base, []byte(`<xml>raw-body</xml>`), map[string]string{
		"X-Tw-Trigger-Envelope": hdr,
		"X-Tw-Source":           "client",
		"X-Tw-Execution-Token":  "tok-1",
		"X-Tw-Invoking-User-Id": "u-1",
	})
	if status != 200 || !env.Ok || env.Status != http.StatusCreated {
		t.Fatalf("fetch invoke mismatch: %d %+v", status, env)
	}
	if env.Headers["content-type"] != "application/json" || env.Headers["x-custom-one"] != "v1" {
		t.Fatalf("函数设置的响应头必须透传: %+v", env.Headers)
	}
	for _, blocked := range []string{"connection", "server", "date", "host", "content-length"} {
		if _, ok := env.Headers[blocked]; ok {
			t.Fatalf("hop-by-hop/平台头 %s 必须被滤: %+v", blocked, env.Headers)
		}
	}
	bodyRaw, err := base64.StdEncoding.DecodeString(env.BodyB64)
	if err != nil {
		t.Fatalf("decode body_base64: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(bodyRaw, &got); err != nil {
		t.Fatalf("decode fetch body: %v", err)
	}
	if got["method"] != http.MethodPost || got["path"] != "/f/p1/tok1" {
		t.Fatalf("method/path 还原错误: %+v", got)
	}
	if got["query"] != "signature=abc&ts=9" {
		t.Fatalf("raw_query 必须回到 query string: %+v", got)
	}
	if got["sig"] != "sha" {
		t.Fatalf("封套 headers 必须还原进 Request: %+v", got)
	}
	if got["body"] != "<xml>raw-body</xml>" {
		t.Fatalf("封套模式下 body = 分发 HTTP body 本身: %+v", got)
	}
	// 身份头通道（与 main 风格 ctx 信息等价）：分发 x-tw-* 头还原进 Request；
	// 封套触发头撞名时身份头胜出（平台身份不得被触发方伪造）。
	if got["idSource"] != "client" || got["idToken"] != "tok-1" || got["idUser"] != "u-1" {
		t.Fatalf("fetch 身份头必须可达: %+v", got)
	}
	if got["idTriggerHdr"] != false {
		t.Fatalf("协议内 header（trigger-envelope）不得进用户 Request: %+v", got)
	}
	if env.Truncated {
		t.Fatalf("未截断响应不得标 truncated: %+v", env)
	}

	// 2) 无封套：Request = POST http://function/ body=TW_DATA。
	status, env = invoke(t, base, []byte(`{"twdata":true}`), nil)
	if status != 200 || !env.Ok {
		t.Fatalf("no-envelope invoke mismatch: %d %+v", status, env)
	}
	bodyRaw, err = base64.StdEncoding.DecodeString(env.BodyB64)
	if err != nil {
		t.Fatalf("decode body_base64: %v", err)
	}
	if err := json.Unmarshal(bodyRaw, &got); err != nil {
		t.Fatalf("decode fetch body: %v", err)
	}
	if got["url"] != "http://function/" || got["method"] != http.MethodPost {
		t.Fatalf("无封套 Request 语义: %+v", got)
	}
	if !strings.Contains(got["body"].(string), `"twdata":true`) {
		t.Fatalf("无封套 body = TW_DATA: %+v", got)
	}
	// 无封套同样携带身份头通道；source 缺省回落 "server"（与 node env 同语义）。
	if got["idSource"] != "server" {
		t.Fatalf("无封套 source 必须回落 server: %+v", got)
	}
}

// TestGoRuntime_FetchBodyTruncated body 超 64KB 截断标 truncated（对二进制
// 同样生效）。
func TestGoRuntime_FetchBodyTruncated(t *testing.T) {
	const src = `package fn

import (
	"bytes"
	"net/http"
)

func Fetch(w http.ResponseWriter, _ *http.Request) {
	w.Write(bytes.Repeat([]byte{7}, 70*1024))
}
`
	bin := buildTwmainCached(t, src, EntryFetch)
	base, _ := startTwapp(t, bin)
	status, env := invoke(t, base, []byte(`{}`), nil)
	if status != 200 || !env.Ok || !env.Truncated {
		t.Fatalf("truncated mismatch: %d %+v", status, env)
	}
	bodyRaw, err := base64.StdEncoding.DecodeString(env.BodyB64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(bodyRaw) != 64*1024 {
		t.Fatalf("body 必须截断到 64KB: %d", len(bodyRaw))
	}
}

// ---- TW_MAX_REQUESTS 计数自退出 ----

func TestGoRuntime_MaxRequestsExit(t *testing.T) {
	const src = `package fn

func Main(_ map[string]any, _ map[string]string) (any, error) {
	return map[string]any{"served": true}, nil
}
`
	bin := buildTwmainCached(t, src, EntryMain)
	base, cmd := startTwapp(t, bin, "TW_MAX_REQUESTS=2")

	// 第 1 个请求正常服务。
	if status, env := invoke(t, base, []byte(`{}`), nil); status != 200 || !env.Ok {
		t.Fatalf("request 1 mismatch: %d %+v", status, env)
	}

	// 第 2 个请求触发自退出：响应可能因进程即时退出而丢失（诚实语义——
	// node process.exit 同款），只要求进程随后退出。
	exited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(exited)
	}()
	_, _, _ = tryInvoke(base, []byte(`{}`), nil) // 触发计数到点（响应可丢）
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("TW_MAX_REQUESTS 到点进程未退出")
	}

	// 退出后新连接被拒。
	if _, _, err := tryInvoke(base, []byte(`{}`), nil); err == nil {
		t.Fatal("自退出后新请求必须被拒（dispatcher 检测退出后补位）")
	}
}

// ---- SIGTERM drain（POSIX 专属：Windows 无 SIGTERM 投递面）----

func TestGoRuntime_Drain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM not deliverable on windows")
	}
	const src = `package fn

import "time"

func Main(data map[string]any, _ map[string]string) (any, error) {
	if _, ok := data["slow"]; ok {
		time.Sleep(1500 * time.Millisecond)
	}
	return map[string]any{"done": true}, nil
}
`
	bin := buildTwmainCached(t, src, EntryMain)
	base, cmd := startTwapp(t, bin, "TW_DRAIN_TIMEOUT_MS=10000")

	// 在途请求（1.5s）。
	inflightDone := make(chan struct{})
	go func() {
		defer close(inflightDone)
		status, env := invoke(t, base, []byte(`{"slow":true}`), nil)
		if status != 200 || !env.Ok {
			t.Errorf("drain 中在途请求必须正常完成: %d %+v", status, env)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if healthInflightTw(t, base) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := healthInflightTw(t, base); got != 1 {
		t.Fatalf("在途期间 inflight = %d, want 1", got)
	}

	// SIGTERM → 停止接新请求、等在途完成后退出。
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case <-inflightDone:
		// 在途请求完成（drain 语义核心：不杀在途）。
	case <-time.After(10 * time.Second):
		t.Fatal("drain 未等待在途请求完成")
	}

	// 在途完成后进程退出；新连接被拒。
	exited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("drain 完成后进程未退出")
	}
	if _, _, err := tryInvoke(base, []byte(`{}`), nil); err == nil {
		t.Fatal("drain 完成后新请求必须被拒")
	}
}

// TestGoRuntime_UnknownXTwHeadersIgnored 协议演进宪法：未知的 x-tw-* header
// 一律忽略（分发侧先于本实现演进不炸协议）。
func TestGoRuntime_UnknownXTwHeadersIgnored(t *testing.T) {
	bin := buildTwmainCached(t, srcMainOnly, EntryMain)
	base, _ := startTwapp(t, bin)
	status, env := invoke(t, base, []byte(`{"n":1}`), map[string]string{
		"X-Tw-Unknown-Future-Header": "whatever",
		"X-Tw-Source":                "client",
	})
	if status != 200 || !env.Ok {
		t.Fatalf("未知 x-tw-* header 必须被忽略: %d %+v", status, env)
	}
}
