package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 本文件覆盖四期 4b M1 镜像全局化端到端（真实 daemon + 本地 registry:2）：
// 构建后 push → registry catalog 确认入仓 → 删本地镜像 → EnsureImage（registry
// 模式 spawn 前置钩子）自动 pull → 镜像可执行。与既有集成门控同口径：
// daemon 不可达跳过；宿主→容器 bridge IP 的 IP 路由型断言（完整 pool.Dispatch
// 执行链）仅 Linux 宿主（Docker Desktop VM 上容器 IP 不可宿主路由），非 Linux
// 宿主以 sibling 容器探针在容器内实跑取证（与 TestIntegration_ImageReadableAs
// TemplateUser 同拓扑）。
//
// M1 顺序裁决（build → verify → push，验证失败不污染 registry）由 verify 失败
// 用例在 catalog 上实证：BuildImage(Verify=true) 失败后 catalog 不得出现该镜像
// ——verify 失败形态在两类宿主上等价（Linux = init 阻塞探针超时；Windows =
// 探针本就不可达），结果同为验证失败，不 push。

// startLocalRegistry 起一次性本地 registry:2（127.0.0.1:<动态端口>），等待
// /v2/ 就绪后返回宿主端口。daemon 经宿主环回地址 push/pull（127.0.0.0/8 是
// docker 的 insecure registry 豁免段，无需 TLS/daemon 配置；Docker Desktop
// WSL2 后端实测 daemon 可达宿主发布端口）。
func startLocalRegistry(t *testing.T) int {
	t.Helper()
	cli := requireDockerClient(t)
	ctx := context.Background()

	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	pullCtx, pullCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer pullCancel()
	pr, err := cli.ImagePull(pullCtx, "registry:2", image.PullOptions{})
	require.NoError(t, err, "pull registry:2")
	_, _ = io.Copy(io.Discard, pr)
	_ = pr.Close()

	created, err := cli.ContainerCreate(ctx,
		&container.Config{Image: "registry:2"},
		&container.HostConfig{
			PortBindings: nat.PortMap{
				"5000/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "0"}},
			},
		}, nil, nil, "tw-it-registry-"+tag)
	require.NoError(t, err)
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_ = cli.ContainerRemove(rmCtx, created.ID, container.RemoveOptions{Force: true})
	})
	require.NoError(t, cli.ContainerStart(ctx, created.ID, container.StartOptions{}))

	ins, err := cli.ContainerInspect(ctx, created.ID)
	require.NoError(t, err)
	bindings := ins.NetworkSettings.Ports["5000/tcp"]
	require.NotEmpty(t, bindings, "registry:2 必须有宿主端口绑定")
	hostPort, err := strconv.Atoi(bindings[0].HostPort)
	require.NoError(t, err)

	deadline := time.Now().Add(30 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("http://127.0.0.1:%d/v2/", hostPort), nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return hostPort
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("local registry not ready on port %d: %v", hostPort, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// registryCatalog 拉取 registry catalog（入仓确认面）。
func registryCatalog(t *testing.T, port int) []string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/v2/_catalog", port), nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Repositories []string `json:"repositories"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.Repositories
}

// registryHostConfig 构造 registry 路由模式配置：functions.docker.registry
// 指向本地仓 + routing_mode=registry + registry_push=true（M1 全量形态）。
func registryHostConfig(t *testing.T, port int) *config.AppConfig {
	t.Helper()
	cfg := testDispatcherConfig(t)
	cfg.Functions.Docker.Registry = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.Functions.Dispatcher.RoutingMode = config.FunctionsRoutingModeRegistry
	cfg.Functions.Dispatcher.RegistryPush = true
	return cfg
}

// requireImageLocal 断言镜像在本节点存在/不存在（pull 证据面）。
func requireImageLocal(t *testing.T, cli *client.Client, ref string, want bool) {
	t.Helper()
	_, err := cli.ImageInspect(context.Background(), ref)
	if err != nil {
		require.True(t, errdefs.IsNotFound(err), "inspect %q: %v", ref, err)
	}
	require.Equal(t, want, err == nil, "镜像 %q 本地存在性不符", ref)
}

// TestIntegration_RegistryPushPullE2E M1 主链路（真实 daemon + registry:2）：
//  1. registry_push=true 构建（build → push）→ catalog 确认镜像入仓；
//  2. verify 失败（永不就绪函数）→ BuildImage 整体失败且 catalog 无该镜像
//     （顺序裁决：验证失败不污染 registry）；
//  3. 删本地镜像 → EnsureImage 自动 pull 回本地（registry 模式冷启动前置钩子）；
//     pull 失败（仓内无此引用）以含 pull 摘要的明确错误收场；
//  4. 执行面：Linux 宿主走完整 pool.Dispatch（spawn 前 pull → spawn → 执行）；
//     非 Linux 宿主以 sibling 容器探针实跑拉取下来的镜像（health + invoke 取证）。
func TestIntegration_RegistryPushPullE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if !dockerAvailable(t) {
		t.Skip("docker daemon unavailable")
	}
	port := startLocalRegistry(t)
	cfg := registryHostConfig(t, port)
	d := NewDockerDaemon(cfg)
	cli := requireDockerClient(t)

	// —— 断言 1：registry_push=true 构建后镜像入仓（build → push）——
	fnID, depID := "fnreg", fmt.Sprintf("dep%d", time.Now().UnixNano())
	imageRef := infrafunctions.ImageName(cfg, fnID, depID)
	cleanupImage(t, d, fnID, depID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	zip := makeEntryZip(t, "index.js", "module.exports.main = (data) => ({ got: data.n, pulled: true });")
	t.Logf("build+push %s (verify off)", imageRef)
	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "regeit", FunctionID: fnID, DeploymentID: depID,
		Zip: zip, Runtime: "node-18.0",
	}), "registry_push=true 的构建必须成功（build → push 全链）")
	catalog := registryCatalog(t, port)
	require.Contains(t, catalog, fmt.Sprintf("func-%s-%s", fnID, depID),
		"构建产物必须已 push 入仓（catalog 确认）")
	t.Logf("catalog after build: %v", catalog)
	requireImageLocal(t, cli, imageRef, true)

	// —— 断言 2：verify 失败不污染 registry（build → verify(失败) → 无 push）——
	// boot 预算 3s：失败判定快速收敛；verify 必须 Construct 独立 daemon——
	// bootTimeout 在构造期解析。
	failCfg := registryHostConfig(t, port)
	failCfg.Functions.Dispatcher.BootTimeout = "3s"
	dFail := NewDockerDaemon(failCfg)
	failFn, failDep := "fnregfail", fmt.Sprintf("dep%d", time.Now().UnixNano())
	cleanupImage(t, dFail, failFn, failDep)

	failZip := makeEntryZip(t, "index.js", "while (true) {}\nmodule.exports.main = () => ({});")
	buildErr := dFail.BuildImage(ctx, BuildImageOptions{
		ProjectID: "regeit", FunctionID: failFn, DeploymentID: failDep,
		Zip: failZip, Runtime: "node-18.0",
		Verify: true,
	})
	require.Error(t, buildErr, "永不就绪的函数必须在验证门失败（deployment failed）")
	t.Logf("verify failed as expected: %v", truncateForLog(buildErr.Error()))
	catalog = registryCatalog(t, port)
	require.NotContains(t, catalog, fmt.Sprintf("func-%s-%s", failFn, failDep),
		"验证失败的构建产物不得 push 入仓（顺序裁决：build → verify → push）")

	// —— 断言 3：删本地 → EnsureImage 自动 pull 回来；仓内无此引用 → 明确错误 ——
	require.NoError(t, d.RemoveImage(ctx, fnID, depID))
	requireImageLocal(t, cli, imageRef, false)

	require.NoError(t, d.EnsureImage(ctx, imageRef), "registry 模式冷启动前置钩子（本地 miss → pull）必须成功")
	requireImageLocal(t, cli, imageRef, true)

	missingRef := fmt.Sprintf("127.0.0.1:%d/func-nopull-missing", port)
	err := d.EnsureImage(ctx, missingRef)
	require.Error(t, err, "仓内无此引用的 pull 必须以明确错误收场")
	require.Contains(t, err.Error(), `docker pull "`, "pull 失败错误必须携带 pull 摘要")
	t.Logf("ensure miss error (expected): %v", truncateForLog(err.Error()))

	// —— 断言 4：执行面 ——
	if runtimeGOOS() == "linux" {
		// Linux 宿主：完整 pool.Dispatch 执行链（registry 路由模式 + spawn 前
		// pull——镜像此刻已被删除，spawn 能成功即证明 pull 在 spawn 前发生）。
		requireIPRoutingHost(t)
		require.NoError(t, d.RemoveImage(ctx, fnID, depID))
		requireImageLocal(t, cli, imageRef, false)

		reg := newFakeRegistry()
		pool := NewPoolManager(d, reg, PoolConfig{
			RoutingMode:      config.FunctionsRoutingModeRegistry,
			BootTimeout:      90 * time.Second,
			QueueHeadTimeout: 90 * time.Second,
		})
		cleanupPool(t, pool, "regeit", fnID)

		resp, perr := pool.Dispatch(ctx, ExecuteRequest{
			Image: imageRef, ProjectID: "regeit", FunctionID: fnID, DeploymentID: depID,
			Runtime: "node-18.0", Spec: "shared-1x", TimeoutSeconds: 30,
			Data: `{"n":41}`,
		})
		require.NoError(t, perr)
		require.Equal(t, "ok", resp.Status)
		require.Contains(t, resp.Response, `"got":41`)
		require.Contains(t, resp.Response, `"pulled":true`)
		requireImageLocal(t, cli, imageRef, true)

		pool.DrainForDeployment(ctx, "regeit", fnID, "none", 5*time.Second)
		waitPoolDrained(t, pool, ctx)
		return
	}

	// 非 Linux 宿主（Docker Desktop VM）：宿主进程→容器 IP 不可路由，完整
	// Dispatch 链跳过（与既有用例同口径）；拉取下来的镜像在 sibling 容器内
	// 实跑取证：health 就绪 + invoke 返回函数结果。
	t.Logf("host OS %q: pool dispatch path requires Linux; verifying pulled image runs via sibling probe", runtimeGOOS())
	requireImageLocal(t, cli, imageRef, true)

	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	netName := "tw-it-reg-net-" + tag
	_, nerr := cli.NetworkCreate(ctx, netName, network.CreateOptions{Driver: "bridge"})
	require.NoError(t, nerr)
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_ = cli.NetworkRemove(rmCtx, netName)
	})

	runnerName := "tw-it-reg-runner-" + tag
	created, cerr := cli.ContainerCreate(ctx,
		&container.Config{Image: imageRef}, // 模板 CMD（node .tw-runner.js）原样运行
		&container.HostConfig{NetworkMode: container.NetworkMode(netName)},
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {Aliases: []string{"tw-reg-runner"}},
		}},
		nil, runnerName)
	require.NoError(t, cerr)
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_ = cli.ContainerRemove(rmCtx, created.ID, container.RemoveOptions{Force: true})
	})
	require.NoError(t, cli.ContainerStart(ctx, created.ID, container.StartOptions{}))

	// sibling 探针：health 就绪后 invoke 一次（POST TW_DATA JSON），断言函数
	// 结果——拉取的镜像不只是存在，而是真的可执行。
	probeScript := fmt.Sprintf(
		"for i in $(seq 1 90); do wget -q -O - http://tw-reg-runner:%d/_tw/health 2>/dev/null | grep -q ready && break; sleep 1; done; "+
			"wget -q -O - --post-data='{\"n\":41}' http://tw-reg-runner:%d/ && echo",
		runnerPort, runnerPort)
	code, out := runOneShot(t, ctx, cli, "tw-it-reg-probe-"+tag,
		&container.Config{
			Image:      imageRef,
			Entrypoint: []string{"sh", "-c", probeScript},
		},
		&container.HostConfig{NetworkMode: container.NetworkMode(netName)})
	require.Equal(t, 0, code, "拉取镜像的 sibling 探针（health + invoke）必须成功:\n%s", out)
	t.Logf("sibling probe output: %s", out)
	require.Contains(t, out, `"got":41`, "invoke 结果必须来自拉取下来的镜像:\n%s", out)
	require.Contains(t, out, `"pulled":true`, "invoke 结果必须来自拉取下来的镜像:\n%s", out)
}

// truncateForLog 收敛日志长度（错误可能携带 64KB 日志尾）。
func truncateForLog(s string) string {
	if len(s) > 512 {
		return s[:512] + "...(truncated)"
	}
	return s
}
