package dispatcher

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
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
	return makeEntryZipFiles(t, map[string]string{name: code})
}

// makeEntryZipFiles 构造含多文件（含子目录路径）的最小 zip：镜像权限回归
// 需要同时覆盖文件读取与子目录遍历（目录 x 位）。
func makeEntryZipFiles(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(files[name]))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// requireDockerClient 返回已协商的 docker client（daemon 不可达则跳过测试）。
func requireDockerClient(t *testing.T) *client.Client {
	t.Helper()
	if !dockerAvailable(t) {
		t.Skip("docker daemon unavailable")
	}
	host := os.Getenv("TORCHWOOD_FUNCTIONS_DOCKER_HOST")
	if host == "" {
		host = client.DefaultDockerHost
	}
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	require.NoError(t, err)
	return cli
}

// runOneShot 创建并启动一次性容器，等其退出，返回退出码与合并日志
// （stdcopy 解复用 stdout/stderr；容器清理挂 t.Cleanup）。
func runOneShot(t *testing.T, ctx context.Context, cli *client.Client, name string, cfg *container.Config, hostCfg *container.HostConfig) (int, string) {
	t.Helper()
	created, err := cli.ContainerCreate(ctx, cfg, hostCfg, &network.NetworkingConfig{}, nil, name)
	require.NoError(t, err)
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_ = cli.ContainerRemove(rmCtx, created.ID, container.RemoveOptions{Force: true})
	})
	require.NoError(t, cli.ContainerStart(ctx, created.ID, container.StartOptions{}))
	waitCh, errCh := cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	var code int
	select {
	case w := <-waitCh:
		code = int(w.StatusCode)
	case err := <-errCh:
		t.Fatalf("one-shot container %s: %v", name, err)
	case <-ctx.Done():
		t.Fatalf("one-shot container %s timed out: %v", name, ctx.Err())
	}
	logs, err := cli.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	require.NoError(t, err)
	defer func() { _ = logs.Close() }()
	var buf bytes.Buffer
	_, _ = stdcopy.StdCopy(&buf, &buf, logs)
	return code, buf.String()
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
	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "dispatchit", FunctionID: "fnit", DeploymentID: "depit",
		Zip: zip, Runtime: "node-18.0",
	}))

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
	// ProjectID 必须与执行请求一致（曾误写 "p"——drain 扫不到实际池，
	// 实例残留致 ResidentTotal 清零断言失败）。
	pool.DrainForDeployment(ctx, req.ProjectID, req.FunctionID, "none", 5*time.Second)
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
	err := d.BuildImage(ctx, BuildImageOptions{FunctionID: "fnpy", DeploymentID: "deppy", Zip: zip})
	require.Error(t, err, "python on v2 must fail explicitly at build time")
	t.Logf("python build error (expected): %v", err)
}

// dispatcherDeps* 与 v1 集成用例（internal/infra/functions
// docker_integration_test.go）同源的最小依赖清单：ms@2.1.3 零依赖纯 JS 包
// （无生命周期脚本，与 --ignore-scripts 恒定语义兼容），integrity 取自
// registry 真实值；测试夹具跨包不可导出，各自持有一份。
const dispatcherDepsIndexJS = "module.exports.main = (data) => ({ ok: true });\n"

const dispatcherDepsPackageJSON = `{
  "name": "torchwood-fn-deps",
  "version": "1.0.0",
  "private": true,
  "dependencies": { "ms": "2.1.3" }
}
`

const dispatcherDepsLockfile = `{
  "name": "torchwood-fn-deps",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "torchwood-fn-deps",
      "version": "1.0.0",
      "dependencies": { "ms": "2.1.3" }
    },
    "node_modules/ms": {
      "version": "2.1.3",
      "resolved": "https://registry.npmjs.org/ms/-/ms-2.1.3.tgz",
      "integrity": "sha512-6FlzubTLZG3J2a/NVCAleEhjzq5oxgHyaCU9yYXvcLsvoVaHJq/s5xXI6/XXP6tz7R9xAOtHnSO/tXtF3WRTlA=="
    }
  }
}
`

// TestIntegration_DispatcherBuild_WithDependencies 带依赖 + lockfile 走 v3
// 默认构建路径（dockerDaemon.BuildImage → 分层模板）端到端真跑（v3 §3.1/D11
// 平台代装）：镜像 history 必须含真实执行的 `npm ci --omit=dev
// --ignore-scripts` 层——证明分层模板被选用且 npm ci 真实拉包，而非模板选错
// 静默走旧模板。依赖在容器内的可执行性由 v1 端到端用例覆盖（同源 extractZip
// 校验与同构模板）。
func TestIntegration_DispatcherBuild_WithDependencies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if !dockerAvailable(t) {
		t.Skip("docker daemon unavailable")
	}
	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)
	cli := requireDockerClient(t)

	fnID := "fndeps"
	depID := fmt.Sprintf("dep%d", time.Now().UnixNano())
	imageRef := infrafunctions.ImageName(cfg, fnID, depID)
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_ = d.RemoveImage(rmCtx, fnID, depID)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	zip := makeEntryZipFiles(t, map[string]string{
		"index.js":          dispatcherDepsIndexJS,
		"package.json":      dispatcherDepsPackageJSON,
		"package-lock.json": dispatcherDepsLockfile,
	})
	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		FunctionID: fnID, DeploymentID: depID, Zip: zip, Runtime: "node-18.0",
	}),
		"带依赖 + lockfile 的 dispatcher 构建必须成功（平台代装）")

	history, err := cli.ImageHistory(ctx, imageRef)
	require.NoError(t, err)
	foundNPMLayer := false
	for _, h := range history {
		if strings.Contains(h.CreatedBy, "npm ci --omit=dev --ignore-scripts") {
			foundNPMLayer = true
			break
		}
	}
	require.True(t, foundNPMLayer, "镜像必须包含 npm ci 分层（平台代装模板），history:\n%s", layerCommands(history))
}

// layerCommands 汇总镜像各层命令（测试断言失败时的诊断输出）。
func layerCommands(history []image.HistoryResponseItem) string {
	var sb strings.Builder
	for _, h := range history {
		sb.WriteString(h.CreatedBy)
		sb.WriteString("\n")
	}
	return sb.String()
}

// TestIntegration_ImageReadableAsTemplateUser 镜像权限坏档回归（EACCES 秒退
// 事故）：构建成功 ≠ 镜像可运行——本次事故正是「构建态 ready、运行态起不来」，
// 构建校验缺了运行态这一环。构建部署镜像后：
//  1. 以镜像模板 USER（node，非 root，不覆盖 User）断言 .tw-runner.js 与用户
//     代码（含子目录）可读；
//  2. 以模板 CMD 起 runner，从同网 sibling 容器断言 /_tw/health 探针可达
//     （宿主→容器 IP 在 Docker Desktop VM 上不可路由，跨容器探测与生产拓扑
//     同构——user-defined bridge + 桥内直连 runner 端口；故本测试不限定
//     Linux 宿主，非 Linux 的 Docker Desktop 一样可跑）。
//
// Linux 上先置进程 umask 0077 复现生产掩蔽形态（0644 声明值被掩蔽成 0600 落盘，
// 修复前 tar 原样带进镜像）；修复后镜像内权限与构建进程状态解耦，任意 umask/
// 任意 dispatcher 用户产出恒可读镜像。
func TestIntegration_ImageReadableAsTemplateUser(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	cli := requireDockerClient(t)

	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)
	restoreUmask := setTestUmask(0o077)
	defer restoreUmask()

	fnID := "fnperm"
	depID := fmt.Sprintf("dep%d", time.Now().UnixNano())
	image := infrafunctions.ImageName(cfg, fnID, depID)
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_ = d.RemoveImage(rmCtx, fnID, depID)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	zip := makeEntryZipFiles(t, map[string]string{
		"index.js":    "const { util } = require('./lib/util');\nmodule.exports.main = () => ({ u: util });\n",
		"lib/util.js": "module.exports = { util: 42 };\n",
	})
	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		FunctionID: fnID, DeploymentID: depID, Zip: zip, Runtime: "node-18.0",
	}))

	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	// —— 断言 1：模板 USER 可读 runner 与用户代码（含子目录遍历） ——
	code, out := runOneShot(t, ctx, cli, "tw-it-perm-read-"+tag,
		&container.Config{
			Image: image,
			Entrypoint: []string{"sh", "-c",
				"ls -l /app/.tw-runner.js /app/index.js /app/lib/util.js; " +
					"test -r /app/.tw-runner.js && test -r /app/index.js && test -r /app/lib/util.js"},
		}, nil)
	require.Equal(t, 0, code, "模板 USER 必须可读 runner 与用户代码（EACCES 秒退回归）:\n%s", out)
	t.Logf("image file listing (as template user): %s", out)

	// —— 断言 2：runner 容器起得来，健康探针跨容器可达 ——
	netName := "tw-it-perm-net-" + tag
	_, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{Driver: "bridge"})
	require.NoError(t, err)
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_ = cli.NetworkRemove(rmCtx, netName)
	})

	runnerName := "tw-it-perm-runner-" + tag
	created, err := cli.ContainerCreate(ctx,
		&container.Config{Image: image}, // 模板 CMD（node .tw-runner.js）原样运行
		&container.HostConfig{NetworkMode: container.NetworkMode(netName)},
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {Aliases: []string{"tw-perm-runner"}},
		}},
		nil, runnerName)
	require.NoError(t, err)
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_ = cli.ContainerRemove(rmCtx, created.ID, container.RemoveOptions{Force: true})
	})
	require.NoError(t, cli.ContainerStart(ctx, created.ID, container.StartOptions{}))

	// 探测器从同一桥网络经容器别名直连 runner 端口（node:18-alpine 自带
	// busybox wget）；90s 上限对齐池 BootTimeout——CI 慢环境 node 启动可超
	// 30s（TestIntegration_DispatcherBuildSpawnDispatch 注释实证）。
	probeCode, probeOut := runOneShot(t, ctx, cli, "tw-it-perm-probe-"+tag,
		&container.Config{
			Image: image,
			Entrypoint: []string{"sh", "-c",
				"for i in $(seq 1 90); do " +
					"wget -q -O - http://tw-perm-runner:18080/_tw/health 2>/dev/null | grep -q ready && exit 0; " +
					"sleep 1; done; echo 'health probe timed out'; exit 1"},
		},
		&container.HostConfig{NetworkMode: container.NetworkMode(netName)})
	require.Equal(t, 0, probeCode, "runner 健康探针必须可达（镜像起不来 = 部署即坏，构建态不可见）:\n%s", probeOut)
}

// —— 部署后验证 spawn e2e（Go 一期阶段 3，设计
// docs/design/functions-runtimes-and-sources.md §1/D10）——
//
// 门控与 TestIntegration_DispatcherBuildSpawnDispatch 同一口径：验证探针与
// 池执行都依赖宿主进程 → 容器 bridge IP 的 Linux 直连路由（生产拓扑 =
// dispatcher 容器自 attach，非 Linux 宿主的 Docker Desktop 上容器 IP 不可
// 宿主路由，跳过）。IP 路由型用例另见各测试注释。

// go e2e fixture：module 根 = zip 根，非 main 的任意包名（D3 生成路线）。
const goE2EMod = "module example.com/hello\n\ngo 1.26\n"

// goMainE2ESrc main 风格契约入口：返回固定封套供 e2e 断言（ctx.source 经
// 分发 header 缺省回落 "server"，runner v5 契约）。
const goMainE2ESrc = `package hello

func Main(data map[string]any, ctx map[string]string) (any, error) {
	return map[string]any{"got": data["n"], "runtime": "go", "src": ctx["source"]}, nil
}
`

// goFetchE2ESrc fetch 风格契约入口：自定义 status/header/body 供 fetch 封套
// 断言（status/headers/body_base64，v4 §2.2）。
const goFetchE2ESrc = `package hello

import "net/http"

func Fetch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Tw-E2E", "go-fetch")
	w.WriteHeader(http.StatusTeapot)
	_, _ = w.Write([]byte("brew:" + r.URL.Query().Get("q")))
}
`

// goBlockedE2ESrc 编译通过但 health 永不就绪：init 永久阻塞 → runner 永不
// 监听 → 验证 spawn 必须在 boot 预算内判失败，且错误携带容器日志尾部
// （init 的 println 是第一现场）；Main 合法保证失败只能来自运行期而非探测/
// 编译。注意阻塞形态必须带 pending timer（永久 Sleep）——裸 select{} 会被
// runtime 全局死锁检测直接杀进程（唯一 goroutine 全眠 → fatal error
// deadlock，容器秒退），走不到 health 探针路径（本套件首次实跑实证）。
const goBlockedE2ESrc = `package blocked

import (
	"fmt"
	"time"
)

func init() {
	fmt.Println("tw-verify-boot-blocked: init is blocking forever")
	for {
		time.Sleep(time.Hour)
	}
}

func Main(data map[string]any, ctx map[string]string) (any, error) {
	return map[string]any{"ok": true}, nil
}
`

// requireIPRoutingHost 汇总 IP 路由型 e2e 门控：short 模式与非 Linux 宿主
// 跳过（理由见文件头 TestIntegration_DispatcherBuildSpawnDispatch 注释），
// daemon 不可达跳过。
func requireIPRoutingHost(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if runtimeGOOS() != "linux" {
		t.Skipf("host OS %q: container bridge IPs are not host-routable outside Linux (Docker Desktop VM); dispatcher dispatch path requires the Linux/dokploy topology", runtimeGOOS())
	}
}

// cleanupImage 挂测试结束的镜像清理（独立 ctx，对齐 dockerCleanupTimeout）。
func cleanupImage(t *testing.T, d Daemon, fnID, depID string) {
	t.Helper()
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_ = d.RemoveImage(rmCtx, fnID, depID)
	})
}

// requireNoLeftoverVerifyContainers 断言 daemon 上不存在仍挂本镜像的容器：
// 验证实例无论成败必须用完即删（池外语义）。在池 spawn 之前调用可把验证
// 残留与池实例区分开（池实例同镜像，但由 drain/reaper 显式清理并有
// ResidentTotal 归零断言兜底）。
func requireNoLeftoverVerifyContainers(t *testing.T, cli *client.Client, imageRef string) {
	t.Helper()
	list, err := cli.ContainerList(context.Background(), container.ListOptions{All: true})
	require.NoError(t, err)
	for _, c := range list {
		require.NotEqual(t, imageRef, c.Image, "验证实例容器必须已回收（池外用完即删），发现残留: %s %v", c.ID[:12], c.Names)
	}
}

// waitPoolDrained 收尾清空池（idle 回收在 TTL 之前主动触发；断言与既有
// 用例同款：ProjectID 必须与执行请求一致）。
func waitPoolDrained(t *testing.T, pool *PoolManager, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if pool.ResidentTotal() == 0 {
			return
		}
		pool.Reaper(ctx)
		time.Sleep(200 * time.Millisecond)
	}
	require.Equal(t, 0, pool.ResidentTotal(), "测试实例必须清理干净")
}

// cleanupPool 挂测试结束的池清理兜底（t.Cleanup 在断言失败路径同样执行
// ——首轮实跑教训：FailNow 会跳过测试体内的 drain，遗留运行中的池容器）。
func cleanupPool(t *testing.T, pool *PoolManager, projectID, fnID string) {
	t.Helper()
	t.Cleanup(func() {
		pool.DrainForDeployment(context.Background(), projectID, fnID, "none", 5*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		waitPoolDrained(t, pool, ctx)
	})
}

// TestIntegration_GoMainFunctionE2E go main 风格端到端：BuildImage（verify
// 默认开 = 验证 spawn 内联通过）→ 断言验证实例已回收 → 真实 spawn 执行一次
// → 断言 main 封套（result JSON）。
func TestIntegration_GoMainFunctionE2E(t *testing.T) {
	requireIPRoutingHost(t)
	cli := requireDockerClient(t)
	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)
	reg := newFakeRegistry()
	pool := NewPoolManager(d, reg, PoolConfig{
		BootTimeout:      90 * time.Second,
		QueueHeadTimeout: 90 * time.Second,
	})

	fnID, depID := "fngomain", fmt.Sprintf("dep%d", time.Now().UnixNano())
	imageRef := infrafunctions.ImageName(cfg, fnID, depID)
	cleanupImage(t, d, fnID, depID)
	cleanupPool(t, pool, "goeit", fnID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	zip := makeEntryZipFiles(t, map[string]string{"go.mod": goE2EMod, "main.go": goMainE2ESrc})
	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "goeit", FunctionID: fnID, DeploymentID: depID,
		Zip: zip, Runtime: "go-1.26",
		Env:    map[string]string{"GREETING": "e2e"},
		Verify: true,
	}), "go main 风格构建 + 验证 spawn 必须成功")

	// 验证实例已回收（此刻池尚未 spawn，凡挂本镜像的容器只能是验证残留）。
	requireNoLeftoverVerifyContainers(t, cli, imageRef)

	// 真实 spawn 执行（复用既有执行断言路径：池冷启动 → 健康握手 → 分发）。
	resp, err := pool.Dispatch(ctx, ExecuteRequest{
		Image: imageRef, ProjectID: "goeit", FunctionID: fnID, DeploymentID: depID,
		Runtime: "go-1.26", Spec: "shared-1x", TimeoutSeconds: 30,
		Data:           `{"n":41}`,
		ExecutionToken: "twx_it-token",
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.Contains(t, resp.Response, `"got":41`)
	require.Contains(t, resp.Response, `"runtime":"go"`)
	require.Contains(t, resp.Response, `"src":"server"`, "ctx.source 经分发 header 透传（缺省回落 server）")
	// 不断言 DurationMS > 0：Go 函数亚毫秒完成，毫秒精度下可为 0（node 专属
	// 假设，见既有用例）。

	pool.DrainForDeployment(ctx, "goeit", fnID, "none", 5*time.Second)
	waitPoolDrained(t, pool, ctx)
}

// TestIntegration_GoFetchFunctionE2E go fetch 风格端到端：触发器封套分发 →
// gorunner 还原为真 *http.Request → fetch 封套（status/headers/body_base64）
// 断言（v4 §2.2 语义在 Go 实现上的回归锚）。
func TestIntegration_GoFetchFunctionE2E(t *testing.T) {
	requireIPRoutingHost(t)
	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)
	reg := newFakeRegistry()
	pool := NewPoolManager(d, reg, PoolConfig{
		BootTimeout:      90 * time.Second,
		QueueHeadTimeout: 90 * time.Second,
	})

	fnID, depID := "fngofetch", fmt.Sprintf("dep%d", time.Now().UnixNano())
	imageRef := infrafunctions.ImageName(cfg, fnID, depID)
	cleanupImage(t, d, fnID, depID)
	cleanupPool(t, pool, "goeft", fnID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	zip := makeEntryZipFiles(t, map[string]string{"go.mod": goE2EMod, "main.go": goFetchE2ESrc})
	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "goeft", FunctionID: fnID, DeploymentID: depID,
		Zip: zip, Runtime: "go-1.26",
		Verify: true,
	}), "go fetch 风格构建 + 验证 spawn 必须成功")

	// AST 探测 Fetch 优先：验证实例的启动行应为 style=fetch（借执行前的
	// 一次性容器日志无法观测——已删除；此处以 fetch 封套行为断言兜底）。
	resp, err := pool.Dispatch(ctx, ExecuteRequest{
		Image: imageRef, ProjectID: "goeft", FunctionID: fnID, DeploymentID: depID,
		Runtime: "go-1.26", Spec: "shared-1x", TimeoutSeconds: 30,
		Data: `{}`,
		TriggerEnvelope: &domainfunctions.TriggerEnvelope{
			Method:   http.MethodGet,
			Path:     "/f/goeft/tok1",
			RawQuery: "q=42",
			Headers:  map[string][]string{"x-e2e": {"1"}},
		},
		ExecutionToken: "twx_it-token",
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.Equal(t, 418, resp.StatusCode, "fetch 封套 status 承载函数 HTTP status")
	require.Equal(t, "brew:42", resp.Response, "Response 为 body 解码文本")
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("brew:42")), resp.ResponseB64)
	require.Equal(t, "go-fetch", resp.HTTPHeaders["x-tw-e2e"], "自定义响应头随封套回传（键小写化）")

	pool.DrainForDeployment(ctx, "goeft", fnID, "none", 5*time.Second)
	waitPoolDrained(t, pool, ctx)
}

// TestIntegration_NodeVerifyStillGreenE2E node 路径回归（OQ1「node 同开」
// 锚点）：verify=true 下 node zip 构建 + 验证 spawn + 执行仍全绿——Go 新增
// 验证门不得破坏存量 node 构建链。
func TestIntegration_NodeVerifyStillGreenE2E(t *testing.T) {
	requireIPRoutingHost(t)
	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)
	reg := newFakeRegistry()
	pool := NewPoolManager(d, reg, PoolConfig{
		BootTimeout:      90 * time.Second,
		QueueHeadTimeout: 90 * time.Second,
	})

	fnID, depID := "fnnodevt", fmt.Sprintf("dep%d", time.Now().UnixNano())
	imageRef := infrafunctions.ImageName(cfg, fnID, depID)
	cleanupImage(t, d, fnID, depID)
	cleanupPool(t, pool, "nodeit", fnID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	zip := makeEntryZip(t, "index.js", "module.exports.main = (data) => ({ node: true, got: data.n });")
	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "nodeit", FunctionID: fnID, DeploymentID: depID,
		Zip: zip, Runtime: "node-18.0",
		Verify: true,
	}), "verify=true 下 node 构建 + 验证 spawn 必须成功")

	resp, err := pool.Dispatch(ctx, ExecuteRequest{
		Image: imageRef, ProjectID: "nodeit", FunctionID: fnID, DeploymentID: depID,
		Runtime: "node-18.0", Spec: "shared-1x", TimeoutSeconds: 30,
		Data:           `{"n":42}`,
		ExecutionToken: "twx_it-token",
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.Contains(t, resp.Response, `"node":true`)
	require.Contains(t, resp.Response, `"got":42`)

	pool.DrainForDeployment(ctx, "nodeit", fnID, "none", 5*time.Second)
	waitPoolDrained(t, pool, ctx)
}

// TestIntegration_GoVerifyFailureCapturesLogTail 验证失败路径端到端：编译
// 通过但 health 永不就绪（init 阻塞）→ BuildImage 报错且错误信息含容器日志
// 尾部片段（第一现场）；untrusted 函数走 internal 变体网络（egress 分类与
// 执行一致，A1）且验证容器已回收。
func TestIntegration_GoVerifyFailureCapturesLogTail(t *testing.T) {
	requireIPRoutingHost(t)
	cli := requireDockerClient(t)
	// boot 预算 3s：失败判定快速收敛（生产默认 60s，语义相同）。
	cfg := testDispatcherConfig(t)
	cfg.Functions.Dispatcher.BootTimeout = "3s"
	d := NewDockerDaemon(cfg)

	fnID, depID := "fngofail", fmt.Sprintf("dep%d", time.Now().UnixNano())
	imageRef := infrafunctions.ImageName(cfg, fnID, depID)
	cleanupImage(t, d, fnID, depID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	zip := makeEntryZipFiles(t, map[string]string{"go.mod": goE2EMod, "main.go": goBlockedE2ESrc})
	err := d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "goefail", FunctionID: fnID, DeploymentID: depID,
		Zip: zip, Runtime: "go-1.26",
		Env:             map[string]string{"GREETING": "e2e"},
		EgressUntrusted: true, // 验证实例必须挂 internal 变体网络（A1）
		Verify:          true,
	})
	require.Error(t, err, "编译通过但永不就绪的函数必须在验证门被拦截（deployment 将标 failed）")
	msg := err.Error()
	require.Contains(t, msg, "verification failed")
	require.Contains(t, msg, "tw-verify-boot-blocked", "错误必须携带容器日志尾部（init 阻塞的第一现场输出）:\n%s", msg)

	// 验证失败容器已回收（此刻无池实例，凡挂本镜像的容器只能是验证残留）。
	requireNoLeftoverVerifyContainers(t, cli, imageRef)
}

// TestIntegration_GoMissingEntryRejected 非契约镜像路径：双缺（无 Fetch 无
// Main）的 go zip 在 prepareBuildContext 构建期报错（先于 docker build，
// 单测 TestPrepareBuildContext_GoMissingEntry 的真实 daemon 形态对照）。
func TestIntegration_GoMissingEntryRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if !dockerAvailable(t) {
		t.Skip("docker daemon unavailable")
	}
	d := NewDockerDaemon(testDispatcherConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	zip := makeEntryZipFiles(t, map[string]string{
		"go.mod":  goE2EMod,
		"util.go": "package hello\n\nfunc A() {}\n",
	})
	err := d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "goemiss", FunctionID: "fngomiss", DeploymentID: "depmiss",
		Zip: zip, Runtime: "go-1.26",
		Verify: true,
	})
	require.Error(t, err, "非契约入口必须在构建期被拒收（不会产出跑不起来的镜像）")
	require.Contains(t, err.Error(), "must export Fetch or Main")
}
