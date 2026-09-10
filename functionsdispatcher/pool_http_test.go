package functionsdispatcher

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestHTTPRunner_EnvelopeParsing httpRunner 的响应封套解析：result 是用户
// main 的任意 JSON 返回值——对象/数组/标量都必须原样进 Result（JSON 文本）。
// 曾因 envelope.Result 声明为 string，对象返回值触发 UnmarshalTypeError 被
// 静默忽略：ok=true 而 result 丢失，执行结果静默变空（CI e2e 实证）。
func TestHTTPRunner_EnvelopeParsing(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantOk     bool
		wantResult string
	}{
		{
			name:       "object result is preserved verbatim",
			body:       `{"ok":true,"result":{"got":41,"runner":true},"stdout":"","stderr":""}`,
			wantOk:     true,
			wantResult: `{"got":41,"runner":true}`,
		},
		{
			name:       "array result",
			body:       `{"ok":true,"result":[1,2,3]}`,
			wantOk:     true,
			wantResult: `[1,2,3]`,
		},
		{
			name:       "scalar result stays a JSON string literal",
			body:       `{"ok":true,"result":"hello"}`,
			wantOk:     true,
			wantResult: `"hello"`,
		},
		{
			name:       "null result",
			body:       `{"ok":true,"result":null}`,
			wantOk:     true,
			wantResult: `null`,
		},
		{
			name:       "error envelope",
			body:       `{"ok":false,"error":"boom","stdout":"s","stderr":"e"}`,
			wantOk:     false,
			wantResult: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// httpRunner.Invoke 固定拼 runnerPort(18080)，故 server 须监听
			// 同端口；端口被占则跳过（ephemeral 探测不引入重试逻辑）。
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:18080")
			if err != nil {
				t.Skipf("runner port 18080 occupied: %v", err)
			}
			srv.Listener = ln
			srv.Start()
			defer srv.Close()

			h := &httpRunner{hc: srv.Client()}
			res, err := h.Invoke(context.Background(), "127.0.0.1", ExecuteRequest{Data: `{}`}, time.Second)
			require.NoError(t, err)
			require.Equal(t, tc.wantOk, res.Ok)
			require.Equal(t, tc.wantResult, res.Result)
		})
	}
}

// TestHTTPRunner_InvokeHeaders Invoke 的分发 header 通道（v3 §1.2）：
// execution id 非空 → X-Tw-Execution-Id 下发；空 → 不发；函数超时恒经
// X-Tw-Timeout-Seconds 下发（runner per-request 超时来源）。
func TestHTTPRunner_InvokeHeaders(t *testing.T) {
	cases := []struct {
		name         string
		executionID  string
		wantExecID   string
		wantTimeoutS string
	}{
		{name: "execution id 下发", executionID: "exec-42", wantExecID: "exec-42", wantTimeoutS: "7"},
		{name: "execution id 空则不发（本切片常态）", executionID: "", wantExecID: "", wantTimeoutS: "7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var got http.Header
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				got = r.Header.Clone()
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
			}))
			ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:18080")
			if err != nil {
				t.Skipf("runner port 18080 occupied: %v", err)
			}
			srv.Listener = ln
			srv.Start()
			defer srv.Close()

			h := &httpRunner{hc: srv.Client()}
			_, err = h.Invoke(context.Background(), "127.0.0.1", ExecuteRequest{
				Data:           `{}`,
				ExecutionToken: "twx_tok",
				ExecutionID:    tc.executionID,
			}, 7*time.Second)
			require.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, tc.wantExecID, got.Get("X-Tw-Execution-Id"))
			require.Equal(t, tc.wantTimeoutS, got.Get("X-Tw-Timeout-Seconds"))
			require.Equal(t, "twx_tok", got.Get("X-Tw-Execution-Token"))
		})
	}
}
