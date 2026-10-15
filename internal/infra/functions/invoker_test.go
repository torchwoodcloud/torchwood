package functions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testSecret = "whsec_test"

// newInvokerFixture 起一个回环 httptest 服务并注入非守卫 client（守卫
// 拦 loopback，测试面绕行——出站守卫由 ssrfguard 包自测覆盖）。
func newInvokerFixture(t *testing.T, handler http.HandlerFunc) (*HTTPInvoker, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewHTTPInvoker(srv.Client()), srv
}

func baseInvocation(method, url string) functions.Invocation {
	return functions.Invocation{
		FunctionID:     "fn_test",
		ProjectID:      "prj_test",
		TargetURL:      url,
		TargetSecret:   testSecret,
		Method:         method,
		Data:           `{"hello":"world"}`,
		ExecutionID:    "exec_1",
		Source:         "client",
		TimeoutSeconds: 5,
	}
}

// TestInvokeHappyPath 服务方按协议验签 + 断言身份头/method/id，返回
// result；invoker 侧 Response 为 result 的 JSON 文本、无函数错误。
func TestInvokeHappyPath(t *testing.T) {
	invoker, srv := newInvokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("server read body: %v", err)
		}
		if err := functions.VerifySignatureHeader(r.Header.Get(functions.HeaderSignature), raw, []string{testSecret}, time.Now()); err != nil {
			t.Errorf("signature verify: %v", err)
		}
		for hdr, want := range map[string]string{
			functions.HeaderExecutionToken:  "twx_token",
			functions.HeaderSource:          "client",
			functions.HeaderInvokingUserID:  "usr_1",
			functions.HeaderProjectID:       "prj_test",
			functions.HeaderProtocolVersion: "1",
		} {
			if got := r.Header.Get(hdr); got != want {
				t.Errorf("header %s = %q, want %q", hdr, got, want)
			}
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			ID      string          `json:"id"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Method != functions.MethodInvoke || req.ID != "exec_1" || req.JSONRPC != "2.0" {
			t.Errorf("rpc request = %+v", req)
		}
		if string(req.Params) != `{"hello":"world"}` {
			t.Errorf("params = %s, want payload verbatim", req.Params)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"exec_1","result":{"answer":42}}`))
	})
	inv := baseInvocation(functions.MethodInvoke, srv.URL)
	inv.ExecutionToken = "twx_token"
	inv.InvokingUserID = "usr_1"

	res, err := invoker.Invoke(context.Background(), inv)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.FunctionError != nil {
		t.Fatalf("FunctionError = %v, want nil", res.FunctionError)
	}
	if res.Response != `{"answer":42}` {
		t.Fatalf("Response = %q", res.Response)
	}
	if res.DurationMS < 0 {
		t.Fatalf("DurationMS = %d", res.DurationMS)
	}
}

// io_ReadAll 读全 body（测试辅助，避免与标准库命名冲突）。
func io_ReadAll(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

// TestInvokeFunctionError 函数应用层错误是合法结果（err nil）。
func TestInvokeFunctionError(t *testing.T) {
	invoker, srv := newInvokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"exec_1","error":{"code":-32000,"message":"boom"}}`))
	})
	res, err := invoker.Invoke(context.Background(), baseInvocation(functions.MethodInvoke, srv.URL))
	if err != nil {
		t.Fatalf("Invoke: %v（函数错误不得映射为送达失败）", err)
	}
	if res.FunctionError == nil || res.FunctionError.Code != -32000 || res.FunctionError.Message != "boom" {
		t.Fatalf("FunctionError = %+v", res.FunctionError)
	}
}

// TestInvokeHTTPResult http.request 的 result 解码为透传面
// （StatusCode/Headers/ResponseB64）。
func TestInvokeHTTPResult(t *testing.T) {
	invoker, srv := newInvokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var req functions.RPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != functions.MethodHTTPRequest {
			t.Errorf("method = %q", req.Method)
		}
		var params functions.HTTPRequestParams
		raw, _ := json.Marshal(req.Params)
		_ = json.Unmarshal(raw, &params)
		if params.Envelope == nil || params.Envelope.Method != "POST" {
			t.Errorf("envelope = %+v", params.Envelope)
		}
		if got, _ := base64.StdEncoding.DecodeString(params.BodyB64); string(got) != "<xml>ok</xml>" {
			t.Errorf("body_b64 = %q", params.BodyB64)
		}
		body := base64.StdEncoding.EncodeToString([]byte("created"))
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"exec_1","result":{"status":201,"headers":{"x-custom":"1"},"body_b64":"%s"}}`, body)
	})
	inv := baseInvocation(functions.MethodHTTPRequest, srv.URL)
	inv.TriggerEnvelope = &functions.TriggerEnvelope{Method: "POST", Path: "/cb"}
	inv.RawBody = []byte("<xml>ok</xml>")

	res, err := invoker.Invoke(context.Background(), inv)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.StatusCode != 201 || res.Headers["x-custom"] != "1" || res.ResponseB64 == "" || res.Response != "created" {
		t.Fatalf("result = %+v", res)
	}
}

// TestInvokePlatformErrors 平台语义失败矩阵：非 2xx（Unavailable）、
// 429（ResourceExhausted）、超时（DeadlineExceeded）、协议不符（Unavailable）。
func TestInvokePlatformErrors(t *testing.T) {
	t.Run("endpoint 500", func(t *testing.T) {
		invoker, srv := newInvokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "upstream broke", http.StatusInternalServerError)
		})
		_, err := invoker.Invoke(context.Background(), baseInvocation(functions.MethodInvoke, srv.URL))
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("code = %v, want Unavailable (%v)", status.Code(err), err)
		}
	})
	t.Run("endpoint 429", func(t *testing.T) {
		invoker, srv := newInvokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
		})
		_, err := invoker.Invoke(context.Background(), baseInvocation(functions.MethodInvoke, srv.URL))
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("code = %v, want ResourceExhausted (%v)", status.Code(err), err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		invoker, srv := newInvokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte(`{"result":1}`))
		})
		inv := baseInvocation(functions.MethodInvoke, srv.URL)
		inv.TimeoutSeconds = 0 // 走调用方 ctx 预算
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := invoker.Invoke(ctx, inv)
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("code = %v, want DeadlineExceeded (%v)", status.Code(err), err)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		invoker, srv := newInvokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		})
		_, err := invoker.Invoke(context.Background(), baseInvocation(functions.MethodInvoke, srv.URL))
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("code = %v, want Unavailable (%v)", status.Code(err), err)
		}
	})
}

// TestInvokeRotation 轮换窗口：请求头带 v1+v2 双签名，持旧钥可验。
func TestInvokeRotation(t *testing.T) {
	invoker, srv := newInvokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io_ReadAll(r)
		if err := functions.VerifySignatureHeader(r.Header.Get(functions.HeaderSignature), raw, []string{"whsec_old"}, time.Now()); err != nil {
			t.Errorf("old-secret verify (rotation window): %v", err)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"exec_1","result":null}`))
	})
	inv := baseInvocation(functions.MethodInvoke, srv.URL)
	inv.TargetPreviousSecret = "whsec_old"
	if _, err := invoker.Invoke(context.Background(), inv); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
}
