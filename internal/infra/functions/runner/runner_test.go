package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
)

// nodeContents / goContents 构造 DockerfileFor 的入参载体（探测产物
// infrafunctions.SourceContents 的模板投影，字段映射见 dispatcher
// daemon 调用点）。
func nodeContents(nodeDeps, hasLockfile bool) SourceContents {
	return SourceContents{Runtime: "node-18.0", NodeDeps: nodeDeps, HasLockfile: hasLockfile}
}

func goContents(modify func(*SourceContents)) SourceContents {
	c := SourceContents{
		Runtime:      "go-1.26",
		GoModulePath: "example.com/fn",
		GoHasSum:     true,
	}
	if modify != nil {
		modify(&c)
	}
	return c
}

// TestDockerfileFor_NodeGolden 逐字节锁定 node 分支两个模板（无依赖一次性
// COPY / 带依赖分层 npm ci）：Go 分支引入（go-1.26 模板资产，设计
// functions-runtimes-and-sources.md §1）不得触碰存量 node 语义——node 用户
// 重建镜像产物必须与改造前逐字节一致（模板版本不 bump 的前提）。
func TestDockerfileFor_NodeGolden(t *testing.T) {
	goldenLegacy := "FROM node:18-alpine\n" +
		"WORKDIR /app\n" +
		"COPY . .\n" +
		"USER node\n" +
		"ENV TW_RUNNER_PORT=18080\n" +
		"CMD [\"node\",\".tw-runner.js\"]\n"
	goldenLayered := "FROM node:18-alpine\n" +
		"WORKDIR /app\n" +
		"COPY package.json package-lock.json* ./\n" +
		"RUN npm ci --omit=dev --ignore-scripts\n" +
		"COPY . .\n" +
		"USER node\n" +
		"ENV TW_RUNNER_PORT=18080\n" +
		"CMD [\"node\",\".tw-runner.js\"]\n"
	df, err := DockerfileFor(nodeContents(false, false))
	if err != nil {
		t.Fatalf("legacy template: %v", err)
	}
	if df != goldenLegacy {
		t.Errorf("legacy template drifted:\nwant:\n%s\ngot:\n%s", goldenLegacy, df)
	}
	df, err = DockerfileFor(nodeContents(true, true))
	if err != nil {
		t.Fatalf("layered template: %v", err)
	}
	if df != goldenLayered {
		t.Errorf("layered template drifted:\nwant:\n%s\ngot:\n%s", goldenLayered, df)
	}
}

// TestDockerfileFor_NodeTemplate 锁定模板形态：CMD = runner（现行模板无
// ENTRYPOINT，用户入口从 CMD 移交 runner）；构建期不执行用户代码的不变量由
// 「仅 COPY + npm ci --ignore-scripts」保持。
func TestDockerfileFor_NodeTemplate(t *testing.T) {
	df, err := DockerfileFor(nodeContents(false, false))
	if err != nil {
		t.Fatalf("node template: %v", err)
	}
	if !strings.Contains(df, "FROM node:18-alpine") {
		t.Errorf("base image mismatch:\n%s", df)
	}
	if strings.Contains(strings.ToUpper(df), "ENTRYPOINT") {
		t.Errorf("template must not introduce ENTRYPOINT:\n%s", df)
	}
	wantCmd := `CMD ["node",".tw-runner.js"]`
	if !strings.Contains(df, wantCmd) {
		t.Errorf("CMD must be the runner (want %s):\n%s", wantCmd, df)
	}
	if !strings.Contains(df, "COPY . .") {
		t.Errorf("runner 由 build context COPY 进入镜像：\n%s", df)
	}
}

// TestDockerfileFor_NodeLayeredWithDeps 带依赖 + lockfile → 分层模板（v3
// §3.1/D11 平台代装）：清单先行 COPY、npm ci --omit=dev --ignore-scripts、
// 再 COPY 全部代码（lockfile 不变命中 Docker 层缓存）；CMD/ENV 与旧模板
// 逐字节一致——本切片不改 runner 协议、不 bump 模板版本（产物等价）。
func TestDockerfileFor_NodeLayeredWithDeps(t *testing.T) {
	legacy, err := DockerfileFor(nodeContents(false, false))
	if err != nil {
		t.Fatalf("legacy template: %v", err)
	}
	df, err := DockerfileFor(nodeContents(true, true))
	if err != nil {
		t.Fatalf("layered template: %v", err)
	}
	if !strings.Contains(df, "COPY package.json package-lock.json* ./\n") {
		t.Errorf("layered template must COPY manifests first:\n%s", df)
	}
	if !strings.Contains(df, "RUN npm ci --omit=dev --ignore-scripts\n") {
		t.Errorf("layered template must install with npm ci --omit=dev --ignore-scripts:\n%s", df)
	}
	// 分层顺序：npm ci 在全量 COPY 之前（层缓存正确性）。
	if strings.Index(df, "npm ci") > strings.Index(df, "COPY . .") {
		t.Errorf("npm ci must precede the full COPY for layer caching:\n%s", df)
	}
	// USER 之后的部分（ENV + CMD）与旧模板逐字节一致（模板版本不 bump 的前提）。
	i := strings.Index(legacy, "USER node\n")
	if !strings.HasSuffix(df, legacy[i:]) {
		t.Errorf("ENV/CMD tail must be byte-identical to legacy template:\nlegacy tail:\n%s\ngot:\n%s", legacy[i:], df)
	}
}

// TestDockerfileFor_NodeDepsWithoutLockfile 带依赖无 lockfile → 构建期报错，
// 错误信息指向提交 lockfile（v3 §3.1 lockfile 强制，确定性构建不变量）。
func TestDockerfileFor_NodeDepsWithoutLockfile(t *testing.T) {
	_, err := DockerfileFor(nodeContents(true, false))
	if err == nil {
		t.Fatal("dependencies without package-lock.json must fail the build")
	}
	if !strings.Contains(err.Error(), "package-lock.json") || !strings.Contains(err.Error(), "npm install 会生成") {
		t.Errorf("error must point at committing a lockfile: %v", err)
	}
}

// TestDockerfileFor_PythonExplicitError 常驻路径下 python 明确报错（探测
// 保留、构建期拒绝；v1 docker 执行器已移除，报错不得引导切换执行器）。
func TestDockerfileFor_PythonExplicitError(t *testing.T) {
	_, err := DockerfileFor(SourceContents{Runtime: "python-3.11"})
	if err == nil {
		t.Fatal("python on the resident executor must fail explicitly")
	}
	if !strings.Contains(err.Error(), "node-only") {
		t.Errorf("错误应说明常驻执行器仅支持 node： %v", err)
	}
	if strings.Contains(err.Error(), "executor=") {
		t.Errorf("错误不应引导配置执行器选择（已无选择面）： %v", err)
	}
}

// TestDockerfileFor_UnknownRuntime 未知运行时拒绝。
func TestDockerfileFor_UnknownRuntime(t *testing.T) {
	if _, err := DockerfileFor(SourceContents{Runtime: "deno-2.0"}); err == nil {
		t.Fatal("unknown runtime must be rejected")
	}
}

// TestDockerfileFor_GoGolden go 分支两形态快照（五期 5b：用户持有根 main 包，
// 构建 = `go build .`，设计 functions-runtimes-and-sources.md 顶部立项段）：
// 无 vendor（-mod=readonly + go mod download 层 + go.sum 通配）与 vendor
// （-mod=vendor + 免 download 层）。多阶段：golang 构建段 CGO_ENABLED=0、
// alpine 运行段 + ca-certificates + 非 root 数字 UID + CMD = 平台产物
// tw-app（「构建期不执行用户代码」不变量：go build 只编译不执行）。
func TestDockerfileFor_GoGolden(t *testing.T) {
	goldenNoVendor := "FROM golang:1.26-alpine AS build\n" +
		"WORKDIR /src\n" +
		"ENV CGO_ENABLED=0\n" +
		"ENV GOFLAGS=-mod=readonly\n" +
		"COPY go.mod go.sum* ./\n" +
		"RUN go mod download\n" +
		"COPY . .\n" +
		"RUN go build -trimpath -ldflags=\"-s -w\" -o /out/tw-app .\n" +
		"\n" +
		"FROM alpine:3.22\n" +
		"RUN apk add --no-cache ca-certificates\n" +
		"COPY --from=build /out/tw-app /tw-app\n" +
		"USER 65534:65534\n" +
		"ENV TW_RUNNER_PORT=18080\n" +
		"CMD [\"/tw-app\"]\n"
	goldenVendor := "FROM golang:1.26-alpine AS build\n" +
		"WORKDIR /src\n" +
		"ENV CGO_ENABLED=0\n" +
		"ENV GOFLAGS=-mod=vendor\n" +
		"COPY go.mod go.sum* ./\n" +
		"COPY . .\n" +
		"RUN go build -trimpath -ldflags=\"-s -w\" -o /out/tw-app .\n" +
		"\n" +
		"FROM alpine:3.22\n" +
		"RUN apk add --no-cache ca-certificates\n" +
		"COPY --from=build /out/tw-app /tw-app\n" +
		"USER 65534:65534\n" +
		"ENV TW_RUNNER_PORT=18080\n" +
		"CMD [\"/tw-app\"]\n"

	df, err := DockerfileFor(goContents(nil))
	if err != nil {
		t.Fatalf("no-vendor template: %v", err)
	}
	if df != goldenNoVendor {
		t.Errorf("no-vendor template drifted:\nwant:\n%s\ngot:\n%s", goldenNoVendor, df)
	}

	// vendor 形态：纯 stdlib vendor 项目合法无 go.sum（vendor + require 非空
	// + 无 go.sum 亦放行——-mod=vendor 不消费 go.sum）。
	df, err = DockerfileFor(goContents(func(c *SourceContents) {
		c.HasVendor = true
		c.GoHasSum = false
		c.GoHasRequires = true
	}))
	if err != nil {
		t.Fatalf("vendor template: %v", err)
	}
	if df != goldenVendor {
		t.Errorf("vendor template drifted:\nwant:\n%s\ngot:\n%s", goldenVendor, df)
	}

	// 不变量抽查（两形态公共面）：go.sum 必须带 * 通配（纯 stdlib 函数合法
	// 无 go.sum，COPY 任一源缺失即失败）；构建目标 = zip 根 main 包（.
	// 而非 ./twmain——平台零注入）；CMD 恒为平台产物（用户入口不进 CMD）；
	// 无 ENTRYPOINT；无 twmain 痕迹（五期 5b 概念已删）。
	for _, df := range []string{goldenNoVendor, goldenVendor} {
		if !strings.Contains(df, "COPY go.mod go.sum* ./\n") {
			t.Errorf("go.sum 必须带通配（node package-lock.json* 先例同构）:\n%s", df)
		}
		if !strings.Contains(df, `RUN go build -trimpath -ldflags="-s -w" -o /out/tw-app .`+"\n") {
			t.Errorf("构建目标必须是根 main 包（go build .）:\n%s", df)
		}
		if strings.Contains(df, "twmain") {
			t.Errorf("twmain 生成机制已删除，模板不得出现该概念:\n%s", df)
		}
		if !strings.HasSuffix(df, "CMD [\"/tw-app\"]\n") {
			t.Errorf("CMD 必须是平台产物 tw-app:\n%s", df)
		}
		if strings.Contains(strings.ToUpper(df), "ENTRYPOINT") {
			t.Errorf("template must not introduce ENTRYPOINT:\n%s", df)
		}
	}
}

// TestDockerfileFor_GoMissingGoSum require 非空且无 go.sum 且无 vendor →
// 构建期报错（对齐 node 缺 lockfile 同构口径）；错误文案指向 go mod tidy。
func TestDockerfileFor_GoMissingGoSum(t *testing.T) {
	_, err := DockerfileFor(goContents(func(c *SourceContents) {
		c.GoHasRequires = true
		c.GoHasSum = false
	}))
	if err == nil {
		t.Fatal("external deps without go.sum must fail the build")
	}
	if !strings.Contains(err.Error(), "go.sum") || !strings.Contains(err.Error(), "go mod tidy") {
		t.Errorf("error must point at go mod tidy: %v", err)
	}

	// require 为空的纯 stdlib 函数合法无 go.sum。
	if _, err := DockerfileFor(goContents(func(c *SourceContents) {
		c.GoHasSum = false
	})); err != nil {
		t.Fatalf("stdlib-only function without go.sum must build: %v", err)
	}
}

// TestTemplateVersion_v5 模板版本单一事实源（v5 = 调用身份 ctx 三件，
// mlbridge fn-rpc 设计 §2.5：RunnerTemplateVersion 4 → 5，runner.go 编译期
// 引用防漂移；并发降级判定基准固定在 MinConcurrencyTemplateVersion=3，
// 不随本版本漂移）。Go 分支两次变更均不 bump（D5/OQ2 裁决 + 五期 5b 构建
// 管道简化：零存量 go deployment、node 语义未变、CMD/ENV 产物等价）——
// 五期 5b 起 Go 契约参考实现移交 sdk/go/functions，平台模板不再承载 Go
// 协议实现。本阶段确认常量维持 5。
func TestTemplateVersion_v5(t *testing.T) {
	if TemplateVersion != 5 {
		t.Fatalf("TemplateVersion = %d, want 5 (Go 分支新增不改存量语义，不 bump)", TemplateVersion)
	}
	if domainfunctions.MinConcurrencyTemplateVersion != 3 {
		t.Fatalf("MinConcurrencyTemplateVersion = %d, want 3（v5 版本 bump 不得把存量 v3 deployment 误降级）", domainfunctions.MinConcurrencyTemplateVersion)
	}
}

var runnerTestPort atomic.Int64

func init() { runnerTestPort.Store(18100) }

// testNodePath 解析测试用 node 可执行文件：优先 TORCHWOOD_TEST_NODE（本机
// PATH 上的 node 可能是宿主应用自带的旧版——v4 fetch 面要求 Node 18+ 原生
// Request/Response 全局，生产镜像 node:18-alpine 恒满足）。
func testNodePath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("TORCHWOOD_TEST_NODE"); p != "" {
		return p
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available on this machine")
	}
	return nodePath
}

// startRunnerProcess 启动 runner 进程并返回 base URL（不等待就绪——加载
// 失败常驻 not-ready 的用例用此形态）。
func startRunnerProcess(t *testing.T, indexJS string, extraEnv ...string) string {
	t.Helper()
	nodePath := testNodePath(t)
	dir := t.TempDir()
	// runner.js 落到临时目录（模拟构建期 COPY），旁边放用户模块。
	if err := os.WriteFile(filepath.Join(dir, ".tw-runner.js"), NodeRunnerJS(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(indexJS), 0o600); err != nil {
		t.Fatal(err)
	}

	port := runnerTestPort.Add(1)
	cmd := exec.CommandContext(context.Background(), nodePath, ".tw-runner.js") // #nosec G204 -- nodePath 来自 exec.LookPath("node")，测试自建 runner
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TW_RUNNER_PORT="+fmt.Sprintf("%d", port))
	cmd.Env = append(cmd.Env, extraEnv...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node runner: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// startRunner 用本机 node 真跑 runner 并等待 health 就绪。
func startRunner(t *testing.T, indexJS string, extraEnv ...string) string {
	t.Helper()
	base := startRunnerProcess(t, indexJS, extraEnv...)
	client := &http.Client{Timeout: 15 * time.Second}

	// health 探针：deadline 驱动（node 冷启动在 CI 慢环境可超 2s，
	// 固定 50×50ms 窗口曾导致 CI 必败）。
	ready := false
	deadline := time.Now().Add(15 * time.Second)
	for !ready && time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/_tw/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("runner never became healthy")
	}
	return base
}

// runnerEnvelope 是 runner 响应封套（result 为用户 main 返回值的任意 JSON，
// 必须用 RawMessage 承接——同 dispatcher 侧 invokeResult 的解析契约）。
// Status/Headers/BodyB64/Truncated 是 v4 fetch 风格扩展（v3 §2.2）。
type runnerEnvelope struct {
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

func invokeJSON(t *testing.T, base string, payload string, headers map[string]string) (int, runnerEnvelope) {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body runnerEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return resp.StatusCode, body
}

func healthInflight(t *testing.T, base string) int {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/_tw/health", nil)
	resp, err := client.Do(req)
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

// TestRunnerSmoke_MainContract v3 主契约：main(data, ctx)、token 双通道
// （ctx 并发安全 + process.env 同步兼容）、错误封套、async 返回值。
func TestRunnerSmoke_MainContract(t *testing.T) {
	base := startRunner(t, `module.exports.main = function (data, ctx) {
  if (data.n === 1) {
    return { doubled: data.n * 2, ctxToken: ctx.executionToken, envToken: process.env.TW_EXECUTION_TOKEN };
  }
  if (data.n === 2) { throw new Error("boom from user code"); }
  if (data.n === 3) { return new Promise((resolve) => setTimeout(() => resolve({ async: true }), 20)); }
  if (data.n === 4) { return { execId: ctx.executionId, apiBase: ctx.apiBaseUrl }; }
  return {};
};`, "TW_API_BASE_URL=http://tw-api.internal:8080")

	// 1) main(data, ctx)：ctx.executionToken 与 header 一致；process.env 同步
	//    通道兼容（同步 main 读取安全）。
	status, body := invokeJSON(t, base, `{"n":1}`, map[string]string{"X-Tw-Execution-Token": "twx_abc"})
	if status != 200 || !body.Ok {
		t.Fatalf("status = %d, body = %+v", status, body)
	}
	if !strings.Contains(string(body.Result), `"doubled":2`) {
		t.Fatalf("unexpected body: %+v", body)
	}
	if !strings.Contains(string(body.Result), `"ctxToken":"twx_abc"`) || !strings.Contains(string(body.Result), `"envToken":"twx_abc"`) {
		t.Fatalf("token 必须经 ctx 与 process.env 双通道可达: %s", body.Result)
	}

	// 2) 用户代码抛错 → 500 {"ok":false,...}（实例保留，进程不退出）。
	status, body = invokeJSON(t, base, `{"n":2}`, nil)
	if status != 500 {
		t.Fatalf("status = %d, body = %+v", status, body)
	}
	if body.Ok || !strings.Contains(body.Error, "boom from user code") {
		t.Fatalf("unexpected body: %+v", body)
	}

	// 3) async 返回值（Promise resolve）。
	status, body = invokeJSON(t, base, `{"n":3}`, nil)
	if status != 200 || !strings.Contains(string(body.Result), `"async":true`) {
		t.Fatalf("status = %d, body = %+v", status, body)
	}

	// 4) ctx.executionId（分发 header）与 ctx.apiBaseUrl（TW_API_BASE_URL 投影）。
	status, body = invokeJSON(t, base, `{"n":4}`, map[string]string{
		"X-Tw-Execution-Id": "exec-42",
	})
	if status != 200 {
		t.Fatalf("status = %d, body = %+v", status, body)
	}
	if !strings.Contains(string(body.Result), `"execId":"exec-42"`) ||
		!strings.Contains(string(body.Result), `"apiBase":"http://tw-api.internal:8080"`) {
		t.Fatalf("ctx.executionId/apiBaseUrl 缺失: %s", body.Result)
	}
}

// TestRunnerSmoke_RequestLogBucketing console 捕获按请求分桶（v3 §1.2）：
// 响应的 stdout 只含本请求输出，不含其他请求的（实例级混流是 v2 语义，
// 已变更为 per-request tail）。
func TestRunnerSmoke_RequestLogBucketing(t *testing.T) {
	base := startRunner(t, `module.exports.main = function (data) {
  console.log("LOG-" + data.tag);
  return { tag: data.tag };
};`)

	status, body := invokeJSON(t, base, `{"tag":"alpha"}`, nil)
	if status != 200 {
		t.Fatalf("status = %d, body = %+v", status, body)
	}
	if !strings.Contains(body.Stdout, "LOG-alpha") {
		t.Fatalf("本请求 stdout 必须含自身输出: %+v", body)
	}

	// 第二请求：只见自身输出——若为实例级混流会同时看到 LOG-alpha。
	status, body = invokeJSON(t, base, `{"tag":"beta"}`, nil)
	if status != 200 {
		t.Fatalf("status = %d, body = %+v", status, body)
	}
	if !strings.Contains(body.Stdout, "LOG-beta") {
		t.Fatalf("本请求 stdout 必须含自身输出: %+v", body)
	}
	if strings.Contains(body.Stdout, "LOG-alpha") {
		t.Fatalf("per-request 分桶：不得泄漏其他请求输出（v3 §1.2）: %+v", body)
	}
}

// TestRunnerSmoke_PerRequestTimeout per-request 超时（v3 §1.2）：main promise
// 未决议 → 到点回 500 封套（error 注明 timed out）并放弃等待；inflight 按
// 请求生命周期释放（health 归零）；超时后实例仍健康可继续服务。
func TestRunnerSmoke_PerRequestTimeout(t *testing.T) {
	base := startRunner(t, `let calls = 0;
module.exports.main = function (data) {
  calls++;
  if (data.hang) { return new Promise(() => {}); } // 永不决议
  return { calls };
};`)

	// 在途计数：挂起请求期间 health 上报 inflight=1。
	done := make(chan struct{})
	go func() {
		defer close(done)
		status, body := invokeJSON(t, base, `{"hang":true}`, map[string]string{"X-Tw-Timeout-Seconds": "1"})
		if status != 500 {
			t.Errorf("per-request 超时必须回 500, got %d: %+v", status, body)
			return
		}
		if !strings.Contains(body.Error, "timed out") {
			t.Errorf("超时封套 error 必须注明 timed out: %+v", body)
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
		t.Fatalf("挂起请求期间 health inflight = %d, want 1", got)
	}

	// 到点（1s）超时封套返回；放弃等待后 inflight 归零。
	<-done
	if got := healthInflight(t, base); got != 0 {
		t.Fatalf("超时放弃后 health inflight = %d, want 0（inflight 按请求生命周期释放）", got)
	}

	// 实例仍健康：后续请求正常服务（诚实声明：被放弃的用户 main 可能仍在
	// 跑，但不影响新请求）。
	status, body := invokeJSON(t, base, `{}`, nil)
	if status != 200 || !strings.Contains(string(body.Result), `"calls":`) {
		t.Fatalf("超时后实例必须仍可服务: %d %+v", status, body)
	}
}

// TestRunnerSmoke_HealthInflight health 探针的 inflight 上报（v2 既有不变，
// 并发视角回归）。
func TestRunnerSmoke_HealthInflight(t *testing.T) {
	base := startRunner(t, `module.exports.main = function () {
  return new Promise((resolve) => setTimeout(() => resolve({ ok: true }), 300));
};`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		status, _ := invokeJSON(t, base, `{}`, nil)
		if status != 200 {
			t.Errorf("status = %d", status)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	observed := 0
	for time.Now().Before(deadline) {
		observed = healthInflight(t, base)
		if observed == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if observed != 1 {
		t.Fatalf("在途期间 health inflight = %d, want 1", observed)
	}
	<-done
	if got := healthInflight(t, base); got != 0 {
		t.Fatalf("完成后 health inflight = %d, want 0", got)
	}
}

// TestRunnerSmoke_IdentityCtxFields v5 调用身份三件：分发 header
// x-tw-source / x-tw-invoking-user-id / x-tw-project-id → ctx.source /
// invokingUserId / projectId。client 链路（source=client + 真实用户）与
// event 链路（source 含 trigger 前缀 + 无用户）双覆盖；header 全缺省时
// source 回落 "server"（v5 前分发链路兼容），invokingUserId/projectId 落空串。
func TestRunnerSmoke_IdentityCtxFields(t *testing.T) {
	base := startRunner(t, `module.exports.main = function (data, ctx) {
  return { source: ctx.source, invokingUserId: ctx.invokingUserId, projectId: ctx.projectId };
};`)

	assertResult := func(headers map[string]string, wants []string) {
		t.Helper()
		status, body := invokeJSON(t, base, `{}`, headers)
		if status != 200 || !body.Ok {
			t.Fatalf("invoke failed: %d %+v", status, body)
		}
		for _, want := range wants {
			if !strings.Contains(string(body.Result), want) {
				t.Fatalf("result 缺少 %s: %s", want, body.Result)
			}
		}
	}

	// client 链路：三 header 齐备 → ctx 三件原样可达。
	assertResult(map[string]string{
		"X-Tw-Source":           "client",
		"X-Tw-Invoking-User-Id": "user-1",
		"X-Tw-Project-Id":       "p1",
	}, []string{`"source":"client"`, `"invokingUserId":"user-1"`, `"projectId":"p1"`})

	// event 链路：source 含 trigger 前缀、无用户 header → invokingUserId 空串。
	assertResult(map[string]string{
		"X-Tw-Source":     "event:trg-evt-1",
		"X-Tw-Project-Id": "p1",
	}, []string{`"source":"event:trg-evt-1"`, `"invokingUserId":""`, `"projectId":"p1"`})

	// header 全缺省（v5 前分发链路）：source 回落 "server"，其余空串。
	assertResult(nil, []string{`"source":"server"`, `"invokingUserId":""`, `"projectId":""`})
}
