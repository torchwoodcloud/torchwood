package runner

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDockerfileFor_NodeTemplate 锁定 v2 模板形态：CMD = runner（现行模板无
// ENTRYPOINT，用户入口从 CMD 移交 runner）；构建期不执行用户代码的不变量由
// 「仅 COPY」保持。
func TestDockerfileFor_NodeTemplate(t *testing.T) {
	df, err := DockerfileFor("node-18.0")
	if err != nil {
		t.Fatalf("node template: %v", err)
	}
	if !strings.Contains(df, "FROM node:18-alpine") {
		t.Errorf("base image mismatch:\n%s", df)
	}
	if strings.Contains(strings.ToUpper(df), "ENTRYPOINT") {
		t.Errorf("v2 template must not introduce ENTRYPOINT:\n%s", df)
	}
	wantCmd := `CMD ["node",".tw-runner.js"]`
	if !strings.Contains(df, wantCmd) {
		t.Errorf("CMD must be the runner (want %s):\n%s", wantCmd, df)
	}
	if !strings.Contains(df, "COPY . .") {
		t.Errorf("runner 由 build context COPY 进入镜像：\n%s", df)
	}
}

// TestDockerfileFor_PythonExplicitError v2 路径下 python 明确报错（不静默
// 回落 v1 CMD——resident 语义下 v1 CMD 跑完即退，实例永远不会 ready）。
func TestDockerfileFor_PythonExplicitError(t *testing.T) {
	_, err := DockerfileFor("python-3.11")
	if err == nil {
		t.Fatal("python on v2 must fail explicitly")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("错误应引导回退 v1： %v", err)
	}
}

// TestDockerfileFor_UnknownRuntime 未知运行时拒绝。
func TestDockerfileFor_UnknownRuntime(t *testing.T) {
	if _, err := DockerfileFor("deno-2.0"); err == nil {
		t.Fatal("unknown runtime must be rejected")
	}
}

// TestRunnerSmoke_MainContract 用本机 node 真跑 runner：加载 index.js、
// main(TW_DATA) 契约、token header → process.env、错误封套、health 探针。
// node 不可用时跳过（CI 的 node 镜像测试另走 docker 集成路径）。
func TestRunnerSmoke_MainContract(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available on this machine")
	}
	dir := t.TempDir()
	// runner.js 落到临时目录（模拟构建期 COPY），旁边放用户模块。
	if err := os.WriteFile(filepath.Join(dir, ".tw-runner.js"), NodeRunnerJS(), 0o600); err != nil {
		t.Fatal(err)
	}
	indexJS := `let lastToken = null;
module.exports.main = function (data) {
  lastToken = process.env.TW_EXECUTION_TOKEN || null;
  if (data.n === 1) { return { doubled: data.n * 2, token: lastToken }; }
  if (data.n === 2) { throw new Error("boom from user code"); }
  if (data.n === 3) { return new Promise((resolve) => setTimeout(() => resolve({ async: true }), 20)); }
  return {};
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(indexJS), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(context.Background(), nodePath, ".tw-runner.js") // #nosec G204 -- nodePath 来自 exec.LookPath("node")，测试自建 runner
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TW_RUNNER_PORT=0")
	// 固定端口（子进程独立网络栈，18080 冲突概率低；重试绑定由用例重跑兜底）。
	cmd.Env = append(cmd.Env, "TW_RUNNER_PORT=18099")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node runner: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	base := "http://127.0.0.1:18099"
	client := &http.Client{Timeout: 5 * time.Second}

	// health 探针（模块同步 require，应立即可用）。
	ready := false
	for i := 0; i < 50; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/_tw/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				_ = resp.Body.Close()
				ready = true
				break
			}
			_ = resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("runner never became healthy")
	}

	invoke := func(payload string, token string) (int, string) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-Tw-Execution-Token", token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("invoke: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body := make([]byte, 8192)
		n, _ := resp.Body.Read(body)
		return resp.StatusCode, string(body[:n])
	}

	// 1) main(TW_DATA) 正常路径 + token header → process.env 注入。
	status, body := invoke(`{"n":1}`, "twx_abc")
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"doubled":2`) {
		t.Fatalf("unexpected body: %s", body)
	}
	if !strings.Contains(body, `"token":"twx_abc"`) {
		t.Fatalf("token header must reach process.env.TW_EXECUTION_TOKEN: %s", body)
	}

	// 2) 用户代码抛错 → 500 {"ok":false,...}（实例保留，进程不退出）。
	status, body = invoke(`{"n":2}`, "")
	if status != 500 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "boom from user code") {
		t.Fatalf("unexpected body: %s", body)
	}

	// 3) async 返回值（Promise resolve）。
	status, body = invoke(`{"n":3}`, "")
	if status != 200 || !strings.Contains(body, `"async":true`) {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	// 4) health 在服务过请求后仍 ready（进程未因用户错误退出）。
	hreq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/_tw/health", nil)
	resp, err := client.Do(hreq)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health after errors: %v %v", err, resp)
	}
	_ = resp.Body.Close()
}
