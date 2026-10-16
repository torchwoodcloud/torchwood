package functions

import (
	"context"
	"encoding/json"
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
	testToken     = "twx_tok" // 与 rpc_test.go 的 signedRPCRequest 身份头同源
	testSource    = "client"
	testUserID    = "usr_1"
	testProjectID = "prj_1"
	testAPIBase   = "http://api.internal:8080"
)

// wantIdentity 断言身份六件（token/source/user/project 走身份头；executionId
// 走 JSON-RPC id；apiBaseUrl 走部署 env）。
func wantIdentity(t *testing.T, got Identity, execID string) {
	t.Helper()
	if got.ExecutionToken != testToken ||
		got.ExecutionID != execID ||
		got.Source != testSource ||
		got.InvokingUserID != testUserID ||
		got.ProjectID != testProjectID ||
		got.APIBaseURL != testAPIBase {
		t.Fatalf("identity = %+v, want token=%s exec=%s source=%s user=%s project=%s base=%s",
			got, testToken, execID, testSource, testUserID, testProjectID, testAPIBase)
	}
}

// —— StartInvoke 的类型化解码糖（经 v4 统一入口全链验证） ——

type pingReq struct {
	Ping string `json:"ping"`
}

type pongResp struct {
	Pong string `json:"pong"`
}

func TestInvokeAdapterTypedDecode(t *testing.T) {
	t.Setenv(envAPIBaseURL, testAPIBase)
	var gotReq pingReq
	mux := NewMux()
	mux.Invoke(invokeAdapter(func(ctx context.Context, req pingReq) (pongResp, error) {
		gotReq = req
		wantIdentity(t, FromContext(ctx), "exec-1")
		return pongResp{Pong: req.Ping}, nil
	}))

	rec := httptest.NewRecorder()
	NewServer(mux, testSecret).ServeHTTP(rec, signedRPCRequest(t, RPCMethodInvoke, map[string]string{"ping": "a"}, "exec-1", testSecret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("error = %+v", resp.Error)
	}
	if gotReq.Ping != "a" {
		t.Fatalf("decoded req = %+v, want ping=a", gotReq)
	}
	if string(resp.Result) != `{"pong":"a"}` {
		t.Fatalf("result = %s", resp.Result)
	}
}

func TestInvokeAdapterDecodeShapeError(t *testing.T) {
	mux := NewMux()
	mux.Invoke(invokeAdapter(func(_ context.Context, _ struct{ N int }) (struct{}, error) {
		return struct{}{}, nil
	}))
	rec := httptest.NewRecorder()
	NewServer(mux, testSecret).ServeHTTP(rec, signedRPCRequest(t, RPCMethodInvoke, map[string]any{"n": "not-an-int"}, "exec-2", testSecret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（成败在 error 对象）", rec.Code)
	}
	var resp rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != rpcCodeServerRange {
		t.Fatalf("error = %+v, want server range (decode shape error)", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "decode invoke params into request") {
		t.Fatalf("message = %q", resp.Error.Message)
	}
}

// TestInvokeAdapterPanicCaptured：handler panic → -32000 error 对象（不杀
// 进程，栈进消息）。
func TestInvokeAdapterPanicCaptured(t *testing.T) {
	mux := NewMux()
	mux.Invoke(invokeAdapter(func(_ context.Context, _ struct{}) (struct{}, error) {
		panic("boom: panic failure")
	}))
	h := NewServer(mux, testSecret)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedRPCRequest(t, RPCMethodInvoke, nil, "exec-3", testSecret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp rpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != rpcCodeServerRange {
		t.Fatalf("error = %+v, want server range (panic captured)", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "panic: boom: panic failure") {
		t.Fatalf("message = %q, want panic message", resp.Error.Message)
	}

	// panic 不杀实例：下一个请求照常服务。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, signedRPCRequest(t, RPCMethodInvoke, nil, "exec-4", testSecret))
	if rec.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200 (instance alive)", rec.Code)
	}
}

// —— Start* 便捷封装签名（编译期断言：handler + 密钥可变参） ——

var (
	_ func(CronHandler, ...string) error  = StartCron
	_ func(EventHandler, ...string) error = StartEvent
	_ func(http.Handler, ...string) error = StartHTTP
)

// TestStartHTTPWiring：StartHTTP 糖与 Mux.Fetch 注册等价——http.request 分发
// 还原真请求并捕获 httpResult（64KB 截断 + 头过滤语义在 rpc_test 覆盖）。
func TestStartHTTPWiring(t *testing.T) {
	mux := NewMux()
	mux.Fetch(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if FromContext(r.Context()).ExecutionToken != testToken {
			t.Errorf("request ctx identity token missing")
		}
		w.Header().Set("X-Reply", "hi")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("done"))
	}))
	rec := httptest.NewRecorder()
	NewServer(mux, testSecret).ServeHTTP(rec, signedRPCRequest(t, RPCMethodHTTPRequest, httpRequestParams{
		Envelope: &triggerEnvelope{Method: "POST", Path: "/cb", Headers: map[string][]string{"content-type": {"application/xml"}}},
		BodyB64:  "",
	}, "exec-h", testSecret))
	var resp struct {
		Result httpResult `json:"result"`
		Error  *rpcError  `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("error = %+v", resp.Error)
	}
	if resp.Result.Status != http.StatusCreated || resp.Result.Headers["x-reply"] != "hi" {
		t.Fatalf("httpResult = %+v", resp.Result)
	}
}

// —— Start 系列真实绑定冒烟（TW_RUNNER_PORT 生效；Listen 阻塞语义；health
// 探针不走验签） ——

func TestStartInvokeBindsAndServes(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	t.Setenv(envPort, strconv.Itoa(port))
	t.Setenv(envAPIBaseURL, testAPIBase)

	started := make(chan error, 1)
	go func() {
		started <- StartInvoke(func(ctx context.Context, req pingReq) (pongResp, error) {
			wantIdentity(t, FromContext(ctx), "smoke-1")
			return pongResp{Pong: strings.ToUpper(req.Ping)}, nil
		}, testSecret)
	}()

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("server did not become ready within 5s")
		}
		probe, perr := http.NewRequestWithContext(context.Background(), http.MethodGet, base+healthPath, nil)
		if perr != nil {
			t.Fatal(perr)
		}
		resp, err := client.Do(probe) // 探针不走验签
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

	body := `{"jsonrpc":"2.0","method":"invoke","params":{"ping":"go"},"id":"smoke-1"}`
	ts := time.Now().Unix()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(hdrSignature, BuildSignatureHeader(ts, [][2]string{{"v1", ComputeSignature(testSecret, ts, []byte(body))}}))
	// 身份头（平台恒发的四件投影；executionId 走 JSON-RPC id）。
	req.Header.Set(hdrExecutionToken, testToken)
	req.Header.Set(hdrSource, testSource)
	req.Header.Set(hdrInvokingUserID, testUserID)
	req.Header.Set(hdrProjectID, testProjectID)
	httpResp, err := client.Do(req)
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
