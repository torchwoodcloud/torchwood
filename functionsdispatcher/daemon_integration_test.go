package functionsdispatcher

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// dockerAvailable 探测本机 docker daemon（不可达则跳过；CI 与本地均可能无 daemon）。
func dockerAvailable(t *testing.T) bool {
	t.Helper()
	host := os.Getenv("TORCHWOOD_FUNCTIONS_DOCKER_HOST")
	if host == "" {
		host = client.DefaultDockerHost
	}
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Logf("docker client error: %v, skipping", err)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Logf("docker daemon unavailable: %v, skipping", err)
		return false
	}
	return true
}

// testDispatcherConfig 构造指向本机 daemon 的配置。
func testDispatcherConfig(t *testing.T) *config.AppConfig {
	t.Helper()
	host := os.Getenv("TORCHWOOD_FUNCTIONS_DOCKER_HOST")
	if host == "" {
		host = client.DefaultDockerHost
	}
	return &config.AppConfig{
		Functions: &config.Functions{
			Executor: "dispatcher",
			Docker: &config.Functions_Docker{
				Host:     host,
				Registry: "torchwood-funcs-dispatch-test",
			},
			Dispatcher: &config.Functions_Dispatcher{
				Addr: ":0",
			},
		},
	}
}

// makeEntryZip 构造含单个入口文件的最小 zip。
func makeEntryZip(t *testing.T, name, code string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	require.NoError(t, err)
	_, err = w.Write([]byte(code))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// TestIntegration_DispatcherBuildSpawnDispatch 真实 daemon 端到端：
// runner 模板构建 → 常驻实例 spawn → 健康握手 → 分发执行 → 复用 → 清理。
// daemon 不可达时 t.Skip；Windows/macOS 宿主（Docker Desktop VM）上容器
// bridge IP 对宿主不可路由——dispatcher 的直连分发通路依赖 Linux 桥网络
// 语义（生产拓扑 = dokploy compose 内的 dispatcher 容器自 attach，见
// 08-functions.md），故非 Linux 宿主同样跳过。
func TestIntegration_DispatcherBuildSpawnDispatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if runtimeGOOS() != "linux" {
		t.Skipf("host OS %q: container bridge IPs are not host-routable outside Linux (Docker Desktop VM); dispatcher dispatch path requires the Linux/dokploy topology", runtimeGOOS())
	}
	if !dockerAvailable(t) {
		t.Skip("docker daemon unavailable")
	}
	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)
	reg := newFakeRegistry()
	pool := NewPoolManager(d, reg, PoolConfig{
		BootTimeout: 90 * time.Second,
		// 队首超时须 ≥ BootTimeout：Dispatch 的冷启动（build 后容器启动 +
		// node 模块加载）阻塞在 trySpawn，CI 慢环境 30s 窗口会先于健康
		// 握手到点（曾致 CI 必败）。
		QueueHeadTimeout: 90 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	zip := makeEntryZip(t, "index.js", "module.exports.main = (data) => ({ got: data.n, runner: true });")
	require.NoError(t, d.BuildImage(ctx, "fnit", "depit", zip))

	req := ExecuteRequest{
		// ProjectID 须满足 ^[a-z][a-z0-9]{0,27}$（diag 诊断段曾实证：连字符
		// 触发 EnsureProjectNetwork 的 ID 校验拒绝，被 Dispatch 吞错路径
		// 包装成"无实例可领"）。
		ProjectID:      "dispatchit",
		FunctionID:     "fnit",
		DeploymentID:   "depit",
		Runtime:        "node-18.0",
		Spec:           "shared-1x",
		TimeoutSeconds: 30,
		Image:          "torchwood-funcs-dispatch-test/func-fnit-depit",
		Data:           `{"n":41}`,
		ExecutionToken: "twx_it-token",
	}

	// ——诊断段：绕过 Dispatch 的吞错路径，暴露冷启动每一步的真实结果——
	diagPolicy := pool.applyDefaults(PoolPolicy{})
	diagRec, diagErr := pool.spawnInstance(ctx, req, diagPolicy)
	if diagErr != nil {
		t.Logf("diag: spawnInstance failed: %v", diagErr)
	} else {
		t.Logf("diag: spawned id=%s ip=%s", diagRec.ContainerID, diagRec.IP)
		probeStart := time.Now()
		for i := 0; i < 8; i++ {
			pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
			perr := newHTTPRunner().Health(pctx, diagRec.IP)
			pcancel()
			t.Logf("diag: host->container health probe #%d (%.1fs): err=%v", i+1, time.Since(probeStart).Seconds(), perr)
			if perr == nil {
				break
			}
			time.Sleep(1 * time.Second)
		}
		running, ip, ierr := d.InspectInstance(ctx, diagRec.ContainerID)
		t.Logf("diag: inspect running=%v ip=%q err=%v", running, ip, ierr)
		pool.killInstance(ctx, FunctionRef{ProjectID: req.ProjectID, FunctionID: req.FunctionID}, diagRec)
	}

	resp, err := pool.Dispatch(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.Contains(t, resp.Response, `"got":41`)
	require.Contains(t, resp.Response, `"runner":true`)
	require.Positive(t, resp.DurationMS)
	require.Equal(t, 1, pool.ResidentTotal())

	// 复用热实例：不再 spawn。
	_, err = pool.Dispatch(ctx, req)
	require.NoError(t, err)

	// 收尾：清空池（idle 回收在 TTL 之前主动触发，避免测试容器残留）。
	pool.DrainForDeployment(ctx, "p", "fnit", "none", 5*time.Second)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if pool.ResidentTotal() == 0 {
			break
		}
		pool.Reaper(ctx)
		time.Sleep(200 * time.Millisecond)
	}
	require.Equal(t, 0, pool.ResidentTotal(), "测试实例必须清理干净")
	require.NoError(t, d.RemoveImage(ctx, "fnit", "depit"))
}

// runtimeGOOS 返回宿主操作系统（独立包装便于语义注释集中）。
func runtimeGOOS() string { return runtime.GOOS }

// TestIntegration_DispatcherBuild_PythonRejected v2 构建对 python 明确报错
// （解压后按入口文件判定 runtime：main.py → python-3.11 → 拒绝）。
func TestIntegration_DispatcherBuild_PythonRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if !dockerAvailable(t) {
		t.Skip("docker daemon unavailable")
	}
	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	zip := makeEntryZip(t, "main.py", "def main(data):\n    return {}\n")
	err := d.BuildImage(ctx, "fnpy", "deppy", zip)
	require.Error(t, err, "python on v2 must fail explicitly at build time")
	t.Logf("python build error (expected): %v", err)
}
