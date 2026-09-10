package functions

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// netTestExecutor 构造指定网络配置的执行器。
func netTestExecutor(t *testing.T, network string) *DockerExecutor {
	t.Helper()
	cfg := &config.AppConfig{
		Functions: &config.Functions{
			Executor: "docker",
			Docker: &config.Functions_Docker{
				Host:     client.DefaultDockerHost,
				Network:  network,
				Registry: "torchwood-funcs-test",
			},
		},
	}
	d := NewDockerExecutor(cfg)
	require.NotNil(t, d)
	return d
}

// TestResolveNetwork_PerProjectDefault：未配置全局网络时按项目派生独立网络名。
func TestResolveNetwork_PerProjectDefault(t *testing.T) {
	d := netTestExecutor(t, "")
	name, err := d.resolveNetwork("shop")
	require.NoError(t, err)
	require.Equal(t, perProjectNetworkPrefix+"shop", name)

	// 不同项目得到不同网络（隔离的前提）。
	name2, err := d.resolveNetwork("other")
	require.NoError(t, err)
	require.NotEqual(t, name, name2)
}

// TestResolveNetwork_ExplicitOptIn：显式配置的全局网络优先生效（opt-in）。
func TestResolveNetwork_ExplicitOptIn(t *testing.T) {
	d := netTestExecutor(t, "torchwood-functions-global")
	for _, pid := range []string{"shop", "other"} {
		name, err := d.resolveNetwork(pid)
		require.NoError(t, err)
		require.Equal(t, "torchwood-functions-global", name)
	}
}

// TestResolveNetwork_FailClosed：空 projectID / 非法 projectID 拒绝，
// 不回落共享或 none 网络。
func TestResolveNetwork_FailClosed(t *testing.T) {
	d := netTestExecutor(t, "")
	_, err := d.resolveNetwork("")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = d.resolveNetwork("Bad_ID")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestResolveInternalNetworkName（P2 egress 默认 deny）：internal 变体网络名
// = 常规网络名 + "-int"；校验与常规解析同源（fail-closed）。
func TestResolveInternalNetworkName(t *testing.T) {
	d := netTestExecutor(t, "")

	name, err := ResolveInternalNetworkName(d.cfg, "shop")
	require.NoError(t, err)
	require.Equal(t, perProjectNetworkPrefix+"shop"+perProjectInternalNetworkSuffix, name)

	// 显式全局网络配置同样加后缀（trusted/untrusted 分网不因 opt-in 失效）。
	d2 := netTestExecutor(t, "torchwood-functions-global")
	name, err = ResolveInternalNetworkName(d2.cfg, "shop")
	require.NoError(t, err)
	require.Equal(t, "torchwood-functions-global-int", name)

	// 非法 projectID 拒绝（fail-closed 与常规解析一致）。
	_, err = ResolveInternalNetworkName(d.cfg, "")
	require.Error(t, err)
	_, err = ResolveInternalNetworkName(d.cfg, "Bad_ID")
	require.Error(t, err)
}

// TestDockerExecutor_PerProjectNetworkIsolation：项目 A 与项目 B 的函数容器
// 分别挂载到 tw-func-<a> / tw-func-<b>，网络拓扑互不相通（Round4 J5-4）。
// 需要 TORCHWOOD_RUN_DOCKER_TESTS=1 且本地 daemon 可用。
func TestDockerExecutor_PerProjectNetworkIsolation(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("docker not available")
	}
	d := netTestExecutor(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	zipPath := writeZipTemp(t, makeZip(t, map[string]string{"index.js": nodeHello}))
	require.NoError(t, d.Build(ctx, "fn_net", "dep_1", zipPath))
	t.Cleanup(func() { _ = d.RemoveImage(context.Background(), "fn_net", "dep_1") })

	cli, err := d.client()
	require.NoError(t, err)
	cleanupNets := func() {
		ctxBg := context.Background()
		for _, p := range []string{"neta", "netb"} {
			if resp, err := cli.NetworkInspect(ctxBg, perProjectNetworkPrefix+p, network.InspectOptions{}); err == nil {
				_ = cli.NetworkRemove(ctxBg, resp.ID)
			}
		}
	}
	t.Cleanup(cleanupNets)

	// 两个项目各执行一次：ensureNetwork 应分别创建两个网络。
	for _, p := range []string{"neta", "netb"} {
		res, err := d.Execute(ctx, domainfunctions.Execution{
			FunctionID:   "fn_net",
			DeploymentID: "dep_1",
			ProjectID:    p,
			Runtime:      "node-18.0",
			Spec:         "shared-1x",
			Timeout:      60,
			Data:         `{}`,
		})
		require.NoError(t, err, p)
		require.Equal(t, 0, res.StatusCode, p)
	}

	netA, err := cli.NetworkInspect(ctx, perProjectNetworkPrefix+"neta", network.InspectOptions{})
	require.NoError(t, err, "tw-func-neta 应已创建")
	netB, err := cli.NetworkInspect(ctx, perProjectNetworkPrefix+"netb", network.InspectOptions{})
	require.NoError(t, err, "tw-func-netb 应已创建")
	require.NotEqual(t, netA.ID, netB.ID, "不同项目的函数网络必须相互独立")

	// 容器已随执行结束清理，这里再验证一次无残留挂载在对方网络上的容器。
	listFilters := filters.NewArgs()
	listFilters.Add("label", "deprecated")
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: listFilters})
	require.NoError(t, err)
	for _, c := range containers {
		require.NotContains(t, c.Names[0], "fn_net", "不应有 fn_net 容器残留")
	}
}
