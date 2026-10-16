package functions

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// signedInvoke 组装经协议签名的 JSON-RPC invoke 请求（联调/测试辅助口径）：
// body 必须原样发送（签名覆盖其字节），signature 置入 X-Tw-Signature 头。
func signedInvoke(t *testing.T, secret, id string) (body, signature string) {
	t.Helper()
	body = `{"jsonrpc":"2.0","method":"invoke","id":"` + id + `"}`
	ts := time.Now().Unix()
	signature = BuildSignatureHeader(ts, [][2]string{{"v1", ComputeSignature(secret, ts, []byte(body))}})
	return body, signature
}

func decodeEnvelope(t *testing.T, body string) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", body, err)
	}
	return env
}

// —— 可选探针 /_tw/health（不走验签；部署平台用） ——

func TestHealthProbe(t *testing.T) {
	mux := NewMux()
	mux.Invoke(func(_ context.Context, _ json.RawMessage) (any, error) { return "ok", nil })
	h := NewServer(mux, testSecret)

	// GET → 200 ok/ready（无需签名：部署平台探活不持密钥）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, healthPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.String())
	if env["ok"] != true || env["ready"] != true {
		t.Fatalf("health envelope = %s, want ok/ready true", rec.Body.String())
	}

	// POST → 405 + Allow: GET。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, healthPath, strings.NewReader("{}")))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST health status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("POST health Allow = %q, want GET", allow)
	}

	// 探针之外仍验签必经：未签名 invoke → 401（探针旁路不放大攻击面）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","method":"invoke","id":"x"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned invoke status = %d, want 401", rec.Code)
	}
}

// —— SIGTERM drain：listener 关停（新连接拒绝）+ 等在途完成 + 兜底强退 ——

// TestListenSigtermDrainEndToEnd 走真实 serve 循环：health 探针 → 慢 invoke
// 在途 → SIGTERM → 新连接拒绝 → 在途完成 → exit(0)。信号投递为 Linux/POSIX
// 语义，Windows 跳过。
func TestListenSigtermDrainEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM 不可投递于 Windows（Linux/POSIX 语义）")
	}
	release := make(chan struct{})
	mux := NewMux()
	mux.Invoke(func(_ context.Context, _ json.RawMessage) (any, error) {
		<-release // 慢 handler：drain 必须等它完成
		return "slow-done", nil
	})
	sig := make(chan os.Signal, 1)
	addrCh := make(chan string, 1)
	exited := make(chan int, 1)
	t.Setenv(envPort, "0") // 临时端口
	t.Setenv(envDrainTimeoutMS, "5000")
	go func() {
		_ = serve(NewServer(mux, testSecret), serveOpts{exit: func(code int) { exited <- code }, addrCh: addrCh, sig: sig})
	}()
	// 通配监听的实际地址是 [::]:PORT（Go 双栈偏好，golang/go#7411）而非
	// 0.0.0.0:PORT——按端口提取归一到 loopback（携带 http_proxy 的环境下
	// 探针会被发进本地代理回 502）。
	_, port, _ := net.SplitHostPort(<-addrCh)
	base := "http://127.0.0.1:" + port
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")

	// 就绪探针（不走验签）。
	probeReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+healthPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(probeReq)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d %s, want 200", resp.StatusCode, raw)
	}

	// 在途慢请求（经签名 + 身份头）。
	slowDone := make(chan int, 1)
	go func() {
		body, sig := signedInvoke(t, testSecret, "drain-1")
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/", strings.NewReader(body))
		if err != nil {
			slowDone <- -1
			return
		}
		req.Header.Set(hdrSignature, sig)
		req.Header.Set(hdrExecutionToken, testToken)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			slowDone <- -1 // 连接被拒等传输失败
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		slowDone <- r.StatusCode
	}()
	time.Sleep(100 * time.Millisecond) // 让慢请求进入 handler

	sig <- syscall.SIGTERM

	// 新连接被拒：listener 已关（refused/reset 即 LB 摘实例信号）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		req, rerr := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/", strings.NewReader(`{}`))
		if rerr != nil {
			t.Fatal(rerr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			break // refused/reset：停接新生效
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("post-sigterm connections still accepted within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 放行在途 handler → 响应送达（drain 等待完成）→ exit(0)。
	close(release)
	if code := <-slowDone; code != http.StatusOK {
		t.Fatalf("slow request status = %d, want 200 (drained)", code)
	}
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

func TestListenPort(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", defaultPort},
		{"abc", defaultPort},
		{"-1", defaultPort},
		{"0", 0}, // 0 = 随机端口（临时端口语义合法）
		{"18080", 18080},
	}
	for _, tc := range cases {
		t.Setenv(envPort, tc.env)
		if got := listenPort(); got != tc.want {
			t.Errorf("listenPort(%q) = %d, want %d", tc.env, got, tc.want)
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
		t.Setenv(envDrainTimeoutMS, tc.env)
		if got := drainTimeout(); got != tc.want {
			t.Errorf("drainTimeout(%q) = %s, want %s", tc.env, got, tc.want)
		}
	}
}
