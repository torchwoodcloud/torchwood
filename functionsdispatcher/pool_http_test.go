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
// X-Tw-Timeout-Seconds 下发（runner per-request 超时来源）。调用身份三件
//（runner v5）：source/invoking_user_id/project_id 经 X-Tw-Source /
// X-Tw-Invoking-User-Id / X-Tw-Project-Id 下发——client 链路（source=client
// + 真实用户）与 event 链路（source 含 trigger 前缀 + 空用户）双覆盖；空
// invoking_user_id 不发 header（runner 侧 ctx.invokingUserId 落空串）。
func TestHTTPRunner_InvokeHeaders(t *testing.T) {
	cases := []struct {
		name             string
		executionID      string
		source           string
		invokingUserID   string
		wantExecID       string
		wantSource       string
		wantInvokingUser string
		wantTimeoutS     string
	}{
		{
			name:             "execution id 下发",
			executionID:      "exec-42",
			wantExecID:       "exec-42",
			wantTimeoutS:     "7",
			wantInvokingUser: "",
		},
		{
			name:             "execution id 空则不发（本切片常态）",
			executionID:      "",
			wantExecID:       "",
			wantTimeoutS:     "7",
			wantInvokingUser: "",
		},
		{
			name:             "client 来源身份：source=client + 真实用户",
			executionID:      "exec-c1",
			source:           "client",
			invokingUserID:   "user-1",
			wantExecID:       "exec-c1",
			wantSource:       "client",
			wantInvokingUser: "user-1",
			wantTimeoutS:     "7",
		},
		{
			name:             "event 来源身份：trigger 前缀 + 空用户不发 header",
			executionID:      "exec-e1",
			source:           "event:trg-evt-1",
			invokingUserID:   "",
			wantExecID:       "exec-e1",
			wantSource:       "event:trg-evt-1",
			wantInvokingUser: "",
			wantTimeoutS:     "7",
		},
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
				ProjectID:      "p1",
				ExecutionToken: "twx_tok",
				ExecutionID:    tc.executionID,
				Source:         tc.source,
				InvokingUserID: tc.invokingUserID,
			}, 7*time.Second)
			require.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, tc.wantExecID, got.Get("X-Tw-Execution-Id"))
			require.Equal(t, tc.wantTimeoutS, got.Get("X-Tw-Timeout-Seconds"))
			require.Equal(t, "twx_tok", got.Get("X-Tw-Execution-Token"))
			// 调用身份三件（runner v5）：ctx.source/invokingUserId/projectId 来源。
			require.Equal(t, tc.wantSource, got.Get("X-Tw-Source"))
			require.Equal(t, tc.wantInvokingUser, got.Get("X-Tw-Invoking-User-Id"))
			require.Equal(t, "p1", got.Get("X-Tw-Project-Id"), "project_id 恒随行（网络寻址字段复用）")
		})
	}
}
