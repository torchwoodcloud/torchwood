package functionsdispatcher

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---- v3 切片 C：触发器封套分发通道与 fetch 风格响应（functions-v3.md §2.2/§2.3）----

// envelopeTestServer 起一个监听 18080 的假 runner（httpRunner 固定拼该端口，
// 端口被占则跳过——同 pool_http_test 既有形态）。
func envelopeTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(handler))
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:18080")
	if err != nil {
		t.Skipf("runner port 18080 occupied: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// TestHTTPRunner_TriggerEnvelopeMode 封套模式分发（v3 §2.3）：TriggerEnvelope
// 非空 → ①X-Tw-Trigger-Envelope header = base64(JSON 元数据)；②HTTP body =
// RawBody（base64 形态先解码）；③Content-Type 不再伪称 application/json；
// Data（TW_DATA 封套）不上分发通道。
func TestHTTPRunner_TriggerEnvelopeMode(t *testing.T) {
	var mu sync.Mutex
	var gotHeader http.Header
	var gotBody []byte
	srv := envelopeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	})

	h := &httpRunner{hc: srv.Client()}
	env := &domainfunctions.TriggerEnvelope{
		Method:   http.MethodPost,
		Path:     "/f/p1/tok1",
		RawQuery: "signature=abc",
		Headers:  map[string][]string{"content-type": {"application/xml"}},
	}
	rawBody := []byte(`<xml>raw</xml>`)
	res, err := h.Invoke(context.Background(), "127.0.0.1", ExecuteRequest{
		Data:            `{"method":"POST","body":"<xml>raw</xml>"}`, // 封套 JSON 必须被忽略
		TriggerEnvelope: env,
		RawBodyIsB64:    true,
		RawBody:         []byte(base64.StdEncoding.EncodeToString(rawBody)),
		ExecutionToken:  "twx_tok",
		ExecutionID:     "exec-1",
	}, 5*time.Second)
	require.NoError(t, err)
	require.True(t, res.Ok)

	mu.Lock()
	defer mu.Unlock()
	got := gotHeader.Get("X-Tw-Trigger-Envelope")
	require.NotEmpty(t, got, "封套元数据必须经独立 header 传递")
	meta, err := base64.StdEncoding.DecodeString(got)
	require.NoError(t, err)
	var decoded domainfunctions.TriggerEnvelope
	require.NoError(t, json.Unmarshal(meta, &decoded))
	require.Equal(t, *env, decoded, "封套元数据 base64 JSON 保真")
	require.Equal(t, rawBody, gotBody, "HTTP body 必须是触发器原始 body（base64 已解码）")
	require.NotEqual(t, "application/json", gotHeader.Get("Content-Type"), "原始 body 不得伪称 JSON")
	require.Equal(t, "twx_tok", gotHeader.Get("X-Tw-Execution-Token"))
	require.Equal(t, "exec-1", gotHeader.Get("X-Tw-Execution-Id"))
}

// TestHTTPRunner_FetchStyleEnvelope fetch 风格响应封套解析（v3 §2.2）：
// status/headers/body_base64/truncated 进 invokeResult。
func TestHTTPRunner_FetchStyleEnvelope(t *testing.T) {
	respEnvelope := `{"ok":true,"status":201,"headers":{"content-type":"application/xml"},"body_base64":"` +
		base64.StdEncoding.EncodeToString([]byte("<xml/>")) + `","truncated":false,"stdout":"o","stderr":"e"}`
	srv := envelopeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(respEnvelope))
	})

	h := &httpRunner{hc: srv.Client()}
	res, err := h.Invoke(context.Background(), "127.0.0.1", ExecuteRequest{Data: `{}`}, time.Second)
	require.NoError(t, err)
	require.True(t, res.Ok)
	require.Equal(t, 201, res.FnStatus)
	require.Equal(t, map[string]string{"content-type": "application/xml"}, res.Headers)
	raw, err := base64.StdEncoding.DecodeString(res.BodyB64)
	require.NoError(t, err)
	require.Equal(t, "<xml/>", string(raw))
}

// TestDispatch_TriggerEnvelopeTooLarge 封套 header 上限（v3 §2.3 对抗审查
// 修正：编码后 12KB，超限 InvalidArgument）——入口校验先于实例认领。
func TestDispatch_TriggerEnvelopeTooLarge(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{healthy: true}
	pool := newTestPool(d, reg, runner, nil)

	big := &domainfunctions.TriggerEnvelope{
		Method:  http.MethodPost,
		Path:    "/f/p1/tok1",
		Headers: map[string][]string{"x-big": {strings.Repeat("a", 16<<10)}},
	}
	_, err := pool.Dispatch(context.Background(), ExecuteRequest{
		Image: "img", ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1",
		TimeoutSeconds: 5, Data: `{}`, TriggerEnvelope: big,
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, 0, d.spawnCount, "超限拒绝不得触发实例创建")
	require.Equal(t, 0, runner.invokeCount, "超限拒绝不得触达实例")

	// 正常体积封套可通过校验（spawn 路径继续走 fake）。
	_, err = pool.Dispatch(context.Background(), ExecuteRequest{
		Image: "img", ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1",
		TimeoutSeconds: 5, Data: `{}`,
		TriggerEnvelope: &domainfunctions.TriggerEnvelope{Method: http.MethodPost, Path: "/"},
	})
	require.NoError(t, err)
}

// TestExecuteOn_FetchStyleResponse executeOn 的 fetch 风格结果映射（v3 §2.2）：
// FnStatus → StatusCode、body 双通道（Response 文本 + ResponseB64 无损）、
// headers 透传；main 风格（FnStatus=0）保持 Response = result 原样。
func TestExecuteOn_FetchStyleResponse(t *testing.T) {
	d := newFakeDaemon()
	reg := newFakeRegistry()
	runner := &fakeRunner{healthy: true}
	runner.invokeFn = func(string, string) (*invokeResult, error) {
		return &invokeResult{
			HTTPStatus: 200,
			Ok:         true,
			FnStatus:   201,
			Headers:    map[string]string{"content-type": "application/xml"},
			BodyB64:    base64.StdEncoding.EncodeToString([]byte(`<xml>ok</xml>`)),
			Stdout:     "s",
		}, nil
	}
	pool := newTestPool(d, reg, runner, nil)

	resp, err := pool.Dispatch(context.Background(), ExecuteRequest{
		Image: "img", ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1",
		TimeoutSeconds: 5, Data: `{}`,
		TriggerEnvelope: &domainfunctions.TriggerEnvelope{Method: http.MethodPost, Path: "/"},
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.Equal(t, 201, resp.StatusCode, "fetch 风格 StatusCode 承载函数 HTTP status")
	require.Equal(t, "<xml>ok</xml>", resp.Response, "Response 为解码文本")
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(`<xml>ok</xml>`)), resp.ResponseB64)
	require.Equal(t, map[string]string{"content-type": "application/xml"}, resp.HTTPHeaders)

	// main 风格：runner 封套无 status → FnStatus=0 → 退出码语义位（恒 0），
	// Response 仍是 result JSON 文本。
	runner2 := &fakeRunner{healthy: true}
	pool2 := newTestPool(d, reg, runner2, nil)
	resp, err = pool2.Dispatch(context.Background(), ExecuteRequest{
		Image: "img", ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1",
		TimeoutSeconds: 5, Data: `{"k":1}`,
		TriggerEnvelope: &domainfunctions.TriggerEnvelope{Method: http.MethodPost, Path: "/"}, // D9：恒填充但 main 风格 runner
	})
	require.NoError(t, err)
	require.Equal(t, 0, resp.StatusCode)
	require.Equal(t, `{"ok":1}`, resp.Response, "main 风格 Response = result 原文")
	require.Empty(t, resp.ResponseB64)
	require.Nil(t, resp.HTTPHeaders)
}
