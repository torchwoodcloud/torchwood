package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ---- runner v4：Web 标准 fetch 接口（docs/design/functions-v3.md §2.1–§2.4）----

// skipWithoutFetchAPI 本机 node < 18 时跳过 fetch 面（生产镜像
// node:18-alpine 恒有原生 Request/Response 全局；v3 §2.1）。
func skipWithoutFetchAPI(t *testing.T) {
	t.Helper()
	// testNodePath 来自测试旗标/环境（CI 显式注入），非用户输入。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, testNodePath(t), "-e", "console.log(typeof Request)").Output() // #nosec G204 -- 测试用 node 路径来自受控注入
	if err != nil || strings.TrimSpace(string(out)) != "function" {
		t.Skip("node lacks native Request global (requires Node 18+)")
	}
}

// triggerEnvelopeHeader 构造分发封套元数据 header（base64 JSON，不含 body；
// 与 dispatcher triggerEnvelopeHeader 同构）。
func triggerEnvelopeHeader(method, path, rawQuery string, headers map[string][]string) string {
	meta, _ := json.Marshal(map[string]any{
		"method":    method,
		"path":      path,
		"raw_query": rawQuery,
		"headers":   headers,
	})
	return base64.StdEncoding.EncodeToString(meta)
}

// invokeRaw POST 原始字节并返回 HTTP 状态 + 解析封套（含 v4 fetch 扩展字段）。
func invokeRaw(t *testing.T, base string, body []byte, headers map[string]string) (int, runnerEnvelope) {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var env runnerEnvelope
	_ = json.Unmarshal(raw, &env)
	return resp.StatusCode, env
}

// runnerEnvelope 的 v4 fetch 风格扩展字段（封套 {ok, status, headers,
// body_base64, truncated}，v3 §2.2）。

// TestRunnerFetch_EntryDetection 入口探测（v4 §2.1，优先级固定）：仅 fetch →
// fetch 风格；仅 main → main 风格；两者皆有 → fetch 胜出；皆无 → 常驻
// not-ready（health 503）且 invoke 报加载错误。
// fetchBodyText 解码 fetch 风格封套的 body_base64（fetch 封套无 result 字段
// ——函数响应经 status/headers/body_base64 承载）。
func fetchBodyText(env runnerEnvelope) string {
	raw, err := base64.StdEncoding.DecodeString(env.BodyB64)
	if err != nil {
		return "<decode error>"
	}
	return string(raw)
}

func TestRunnerFetch_EntryDetection(t *testing.T) {
	skipWithoutFetchAPI(t)
	// 仅 fetch。
	base := startRunner(t, `module.exports.fetch = async (request, env) => Response.json({ style: "fetch", url: request.url });`)
	status, body := invokeJSON(t, base, `{"a":1}`, nil)
	if status != 200 || !body.Ok || !strings.Contains(fetchBodyText(body), `"style":"fetch"`) {
		t.Fatalf("fetch-only module must run fetch style: %d %+v", status, body)
	}

	// fetch + main 并存 → fetch 胜出（优先级固定）。
	base = startRunner(t, `module.exports.main = async () => ({ style: "main" });
module.exports.fetch = async () => Response.json({ style: "fetch" });`)
	status, body = invokeJSON(t, base, `{}`, nil)
	if status != 200 || !strings.Contains(fetchBodyText(body), `"style":"fetch"`) {
		t.Fatalf("fetch must take precedence when both exported: %d %+v", status, body)
	}

	// 皆无 → 加载错误：health 常驻 not-ready（等待进程监听后探到 503）、
	// invoke 500 + 指引信息。
	base = startRunnerProcess(t, `module.exports = {};`)
	client := &http.Client{Timeout: 5 * time.Second}
	var notReady bool
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/_tw/health", nil)
		if err != nil {
			t.Fatalf("build health request: %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusServiceUnavailable {
				notReady = true
				break
			}
			t.Fatalf("module without entry must stay not-ready (503), got %d", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !notReady {
		t.Fatal("runner process never came up")
	}
	status, body = invokeJSON(t, base, `{}`, nil)
	if status != 500 || body.Ok || !strings.Contains(body.Error, "index.js must export main or fetch") {
		t.Fatalf("加载错误必须指明 main or fetch: %d %+v", status, body)
	}
}

// TestRunnerFetch_RequestReconstruction 封套还原（v4 §2.3）：path/raw_query/
// headers/body（二进制无损）各归其位；URL query 经 searchParams 可读——微信
// SSV 验签形态。
func TestRunnerFetch_RequestReconstruction(t *testing.T) {
	skipWithoutFetchAPI(t)
	base := startRunner(t, `module.exports.fetch = async (request) => {
  const u = new URL(request.url);
  const raw = new Uint8Array(await request.arrayBuffer());
  let b64 = ""; for (const b of raw) b64 += String.fromCharCode(b);
  return Response.json({
    method: request.method,
    path: u.pathname,
    signature: u.searchParams.get("signature") || "",
    timestamp: u.searchParams.get("timestamp") || "",
    sig256: request.headers.get("x-hub-signature-256") || "",
    contentType: request.headers.get("content-type") || "",
    bodyBytesB64: btoa(b64),
  });
};`)

	payload := []byte{0x00, 0x01, 0xff, 0xfe, 'h', 'i'} // 含非法 UTF-8 序列
	hdr := triggerEnvelopeHeader(http.MethodPost, "/f/p1/tok1", "signature=abc&timestamp=9", map[string][]string{
		"content-type":        {"application/xml"},
		"x-hub-signature-256": {"sha"},
	})
	status, body := invokeRaw(t, base, payload, map[string]string{"X-Tw-Trigger-Envelope": hdr})
	if status != 200 || !body.Ok {
		t.Fatalf("fetch invoke failed: %d %+v", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(fetchBodyText(body)), &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got["method"] != http.MethodPost || got["path"] != "/f/p1/tok1" {
		t.Fatalf("method/path 还原错误: %+v", got)
	}
	if got["signature"] != "abc" || got["timestamp"] != "9" {
		t.Fatalf("raw_query 必须回到 query string: %+v", got)
	}
	if got["sig256"] != "sha" || got["contentType"] != "application/xml" {
		t.Fatalf("headers 原样透传: %+v", got)
	}
	wantB64 := base64.StdEncoding.EncodeToString(payload)
	if got["bodyBytesB64"] != wantB64 {
		t.Fatalf("body 二进制无损（非法 UTF-8 序列直达）: got %v want %v", got["bodyBytesB64"], wantB64)
	}
}

// TestRunnerFetch_NoEnvelope TW_DATA 路径（v4 §2.2 invoke/cron 语义）：无封套
// header → Request = POST http://function/ body = TW_DATA JSON。
func TestRunnerFetch_NoEnvelope(t *testing.T) {
	skipWithoutFetchAPI(t)
	base := startRunner(t, `module.exports.fetch = async (request, env) => {
  const data = await request.json();
  return Response.json({ url: request.url, method: request.method, data, execId: env.EXECUTION_ID, token: env.EXECUTION_TOKEN, apiBase: env.API_BASE_URL });
};`, "TW_API_BASE_URL=http://tw-api.internal:8080")
	status, body := invokeJSON(t, base, `{"n":7}`, map[string]string{
		"X-Tw-Execution-Token": "twx_tok",
		"X-Tw-Execution-Id":    "exec-9",
	})
	if status != 200 || !body.Ok {
		t.Fatalf("invoke failed: %d %+v", status, body)
	}
	for _, want := range []string{
		`"url":"http://function/"`, `"method":"POST"`, `"n":7`,
		`"execId":"exec-9"`, `"token":"twx_tok"`, `"apiBase":"http://tw-api.internal:8080"`,
	} {
		if !strings.Contains(fetchBodyText(body), want) {
			t.Fatalf("result 缺少 %s: %s", want, fetchBodyText(body))
		}
	}
}

// TestRunnerFetch_EnvPerRequest fetch 风格 env 是请求级参数（v4 §2.1）：并发
// 请求各自的 env 三件不串号（结构性保障的运行时抽样验证）。
func TestRunnerFetch_EnvPerRequest(t *testing.T) {
	skipWithoutFetchAPI(t)
	base := startRunner(t, `module.exports.fetch = async (request, env) => {
  await new Promise((r) => setTimeout(r, 50));
  const data = await request.json();
  return Response.json({ tag: data.tag, token: env.EXECUTION_TOKEN });
};`)
	done := make(chan string, 2)
	for _, tc := range []struct{ tag, token string }{{"a", "tok-a"}, {"b", "tok-b"}} {
		go func(tag, token string) {
			status, body := invokeJSON(t, base, `{"tag":"`+tag+`"}`, map[string]string{"X-Tw-Execution-Token": token})
			if status != 200 || !body.Ok {
				t.Errorf("invoke %s failed: %d %+v", tag, status, body)
				done <- ""
				return
			}
			done <- fetchBodyText(body)
		}(tc.tag, tc.token)
	}
	for i := 0; i < 2; i++ {
		res := <-done
		if strings.Contains(res, `"tag":"a"`) && !strings.Contains(res, `"token":"tok-a"`) {
			t.Fatalf("env 串号（a 拿到他人 token）: %s", res)
		}
		if strings.Contains(res, `"tag":"b"`) && !strings.Contains(res, `"token":"tok-b"`) {
			t.Fatalf("env 串号（b 拿到他人 token）: %s", res)
		}
	}
}

// TestRunnerFetch_ResponseEnvelope Response 封套（v4 §2.2/§2.4）：status/
// headers/body_base64 恒在、hop-by-hop 与平台头被滤、64KB 截断标 truncated、
// 非 Response 返回值报错。
func TestRunnerFetch_ResponseEnvelope(t *testing.T) {
	skipWithoutFetchAPI(t)
	base := startRunner(t, `module.exports.fetch = async (request) => {
  const data = request.url.endsWith("?case=binary") ? new Uint8Array([0, 1, 2, 255]) : new TextEncoder().encode("hello");
  const headers = { "Content-Type": "application/xml", "X-Custom-One": "v1", "Connection": "close", "Content-Length": "9999", "Date": "bad", "Server": "fake" };
  return new Response(data, { status: 201, headers });
};`)

	status, body := invokeRaw(t, base, []byte(`{}`), nil)
	if status != 200 || !body.Ok {
		t.Fatalf("invoke failed: %d %+v", status, body)
	}
	if body.Status != 201 {
		t.Fatalf("封套必须携带函数 HTTP status: %+v", body)
	}
	if body.Headers["content-type"] != "application/xml" || body.Headers["x-custom-one"] != "v1" {
		t.Fatalf("函数设置的响应头必须透传: %+v", body.Headers)
	}
	for _, blocked := range []string{"connection", "content-length", "date", "server", "host"} {
		if _, ok := body.Headers[blocked]; ok {
			t.Fatalf("hop-by-hop/平台头 %s 必须被滤: %+v", blocked, body.Headers)
		}
	}
	raw, err := base64.StdEncoding.DecodeString(body.BodyB64)
	if err != nil || string(raw) != "hello" {
		t.Fatalf("body_base64 无损解码: %q err=%v", body.BodyB64, err)
	}
	if body.Truncated {
		t.Fatalf("未截断响应不得标 truncated: %+v", body)
	}

	// 二进制 body 无损 + 全缓冲（query 触发 binary 分支——raw_query 进 URL）。
	hdr := triggerEnvelopeHeader(http.MethodGet, "/", "case=binary", nil)
	status, body = invokeRaw(t, base, nil, map[string]string{"X-Tw-Trigger-Envelope": hdr})
	if status != 200 || !body.Ok || body.Status != 201 {
		t.Fatalf("binary invoke failed: %d %+v", status, body)
	}
	raw, err = base64.StdEncoding.DecodeString(body.BodyB64)
	if err != nil || len(raw) != 4 || raw[3] != 0xff {
		t.Fatalf("二进制 body_base64 无损: %q err=%v", body.BodyB64, err)
	}
}

// TestRunnerFetch_BodyTruncated body 超 64KB 截断（v4 §2.4，对二进制同样生效）。
func TestRunnerFetch_BodyTruncated(t *testing.T) {
	skipWithoutFetchAPI(t)
	base := startRunner(t, `module.exports.fetch = async () => new Response(new Uint8Array(70 * 1024).fill(7), { status: 200 });`)
	status, body := invokeRaw(t, base, []byte(`{}`), nil)
	if status != 200 || !body.Ok {
		t.Fatalf("invoke failed: %d %+v", status, body)
	}
	raw, err := base64.StdEncoding.DecodeString(body.BodyB64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw) != 64*1024 {
		t.Fatalf("body 必须截断到 64KB: %d", len(raw))
	}
	if !body.Truncated {
		t.Fatalf("截断必须标 truncated: %+v", body)
	}
}

// TestRunnerFetch_NonResponseReturn fetch handler 返回非 Response → 500 错误
// 封套（与 main 风格错误形态一致）。
func TestRunnerFetch_NonResponseReturn(t *testing.T) {
	skipWithoutFetchAPI(t)
	base := startRunner(t, `module.exports.fetch = async () => ({ not: "a response" });`)
	status, body := invokeJSON(t, base, `{}`, nil)
	if status != 500 || body.Ok || !strings.Contains(body.Error, "must return a Response") {
		t.Fatalf("非 Response 返回值必须报错: %d %+v", status, body)
	}
}

// TestRunnerFetch_FetchThrow fetch handler 抛错 → 500 {ok:false}（错误语义与
// main 风格一致；dispatcher 侧映射函数失败）。
func TestRunnerFetch_FetchThrow(t *testing.T) {
	skipWithoutFetchAPI(t)
	base := startRunner(t, `module.exports.fetch = async () => { throw new Error("fetch boom"); };`)
	status, body := invokeJSON(t, base, `{}`, nil)
	if status != 500 || body.Ok || !strings.Contains(body.Error, "fetch boom") {
		t.Fatalf("fetch 抛错必须回错误封套: %d %+v", status, body)
	}
}

// TestRunnerFetch_TimeoutAndBucketing fetch 风格 per-request 超时与日志分桶
// （v3 §1.2 基建两风格一致）：超时回 500 注明 timed out、inflight 归零、
// stdout 仅本请求输出。
func TestRunnerFetch_TimeoutAndBucketing(t *testing.T) {
	skipWithoutFetchAPI(t)
	base := startRunner(t, `module.exports.fetch = async (request) => {
  const hang = new URL(request.url).search === "?hang=1";
  console.log("FETCH-LOG");
  if (hang) { return new Promise(() => {}); }
  await new Promise((r) => setTimeout(r, 20));
  return Response.json({ done: true });
};`)
	hdr := triggerEnvelopeHeader(http.MethodGet, "/", "hang=1", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		status, body := invokeRaw(t, base, nil, map[string]string{
			"X-Tw-Trigger-Envelope": hdr,
			"X-Tw-Timeout-Seconds":  "1",
		})
		if status != 500 {
			t.Errorf("fetch 超时必须回 500, got %d: %+v", status, body)
		}
		if !strings.Contains(body.Error, "timed out") || !strings.Contains(body.Error, "fetch()") {
			t.Errorf("超时封套须注明 fetch() timed out: %+v", body)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if healthInflight(t, base) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := healthInflight(t, base); got != 1 {
		t.Fatalf("挂起 fetch 期间 inflight = %d, want 1", got)
	}
	<-done
	if got := healthInflight(t, base); got != 0 {
		t.Fatalf("超时放弃后 inflight = %d, want 0", got)
	}

	// 正常请求：分桶日志只见本请求输出；超时残留不影响新请求。
	status, body := invokeRaw(t, base, nil, map[string]string{
		"X-Tw-Trigger-Envelope": triggerEnvelopeHeader(http.MethodGet, "/", "", nil),
	})
	if status != 200 || !body.Ok || !strings.Contains(body.Stdout, "FETCH-LOG") {
		t.Fatalf("正常 fetch 必须成功且带本请求日志: %d %+v", status, body)
	}
}

// TestRunnerMain_EnvelopeInjection main 风格封套注入（v4 §2.3「与现状等价」/
// D9 双轨）：封套 header + 原始 body 重组 TW_DATA——函数看到的 data 与 v3
// handler 构建的封套逐字段同构。
func TestRunnerMain_EnvelopeInjection(t *testing.T) {
	skipWithoutFetchAPI(t)
	base := startRunner(t, `module.exports.main = async (data) => ({
  method: data.method, path: data.path, raw_query: data.raw_query,
  sig: (data.headers["x-wx-signature"] || [])[0],
  body: data.body, body_b64: data.body_base64,
});`)
	payload := []byte(`<xml>raw-body</xml>`)
	hdr := triggerEnvelopeHeader(http.MethodPost, "/f/p1/t", "signature=zz", map[string][]string{
		"x-wx-signature": {"sig1"},
	})
	status, body := invokeRaw(t, base, payload, map[string]string{"X-Tw-Trigger-Envelope": hdr})
	if status != 200 || !body.Ok {
		t.Fatalf("main 风格封套注入失败: %d %+v", status, body)
	}
	for _, want := range []string{
		`"method":"POST"`, `"path":"/f/p1/t"`, `"raw_query":"signature=zz"`,
		`"sig":"sig1"`, `"body":"<xml>raw-body</xml>"`,
		`"body_b64":"` + base64.StdEncoding.EncodeToString(payload) + `"`,
	} {
		if !strings.Contains(string(body.Result), want) {
			t.Fatalf("重组 TW_DATA 缺少 %s: %s", want, body.Result)
		}
	}
}

// TestRunnerMain_NoEnvelopeRegression main 风格无封套回归：TW_DATA 照旧
// （v3 现状逐字段不变）。
func TestRunnerMain_NoEnvelopeRegression(t *testing.T) {
	base := startRunner(t, `module.exports.main = async (data) => ({ got: data.a });`)
	status, body := invokeJSON(t, base, `{"a":42}`, nil)
	if status != 200 || !strings.Contains(string(body.Result), `"got":42`) {
		t.Fatalf("main 风格 TW_DATA 回归: %d %+v", status, body)
	}
}
