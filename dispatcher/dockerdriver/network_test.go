package dockerdriver

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 本文件覆盖 EnsureProjectNetwork 的 attach 幂等/陈旧 endpoint 自愈（2026-09-14
// dev 事故回归：dispatcher 容器非优雅重建后 tw-func-* 网络残留同名 endpoint，
// attach 确定性失败，monsters 项目函数队列永久卡死）。54b666b 用例原样适配
// （callback 配置位迁移到 functions.docker.callback_container）。

// staleEndpointErrMsg 与真实 daemon 报错同形（libnetwork endpoint 名冲突，
// 文本自 moby 2016 起稳定）。
const staleEndpointErrMsg = "Error response from daemon: endpoint with name dispatcher-1 already exists in network tw-func-monsters-int"

// fakeNetClient 是 networkClient 的可编程 fake：记录调用流水，按
// "connect/<containerID>"、"disconnect/<containerID>" 弹出预置错误（耗尽后
// 默认成功），驱动 attach 路径的确定性单测。NetworkInspect 恒成功（网络
// 已存在路径，创建分支另有 daemon 吞错语义，不在本组用例内）。
type fakeNetClient struct {
	mu    sync.Mutex
	errs  map[string][]error
	calls []string
}

func (f *fakeNetClient) pop(kind, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := kind + "/" + id
	seq := f.errs[key]
	if len(seq) == 0 {
		return nil
	}
	err := seq[0]
	f.errs[key] = seq[1:]
	return err
}

func (f *fakeNetClient) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeNetClient) NetworkInspect(_ context.Context, networkID string, _ network.InspectOptions) (network.Inspect, error) {
	f.record("inspect " + networkID)
	return network.Inspect{}, nil
}

func (f *fakeNetClient) NetworkCreate(_ context.Context, name string, _ network.CreateOptions) (network.CreateResponse, error) {
	f.record("create " + name)
	return network.CreateResponse{}, nil
}

func (f *fakeNetClient) NetworkConnect(_ context.Context, networkID, containerID string, _ *network.EndpointSettings) error {
	f.record("connect " + networkID + " " + containerID)
	return f.pop("connect", containerID)
}

func (f *fakeNetClient) NetworkDisconnect(_ context.Context, networkID, containerID string, force bool) error {
	call := "disconnect " + networkID + " " + containerID
	if force {
		call += " force"
	}
	f.record(call)
	return f.pop("disconnect", containerID)
}

// newHealTestDaemon 构造注入 fake 的 daemon（selfContainerID 非空 = 容器内
// 模式，走自 attach 路径）。
func newHealTestDaemon(f *fakeNetClient, callbackContainer string) *daemon {
	cfg := &config.AppConfig{
		Functions: &config.Functions{
			Docker: &config.Functions_Docker{},
		},
	}
	if callbackContainer != "" {
		cfg.Functions.Docker.CallbackContainer = callbackContainer
	}
	return &daemon{cfg: cfg, netCli: f, selfContainerID: "selfc"}
}

// TestEnsureProjectNetwork_HealsStaleEndpoint 事故主回归：自 attach 撞上
// 陈旧 endpoint 名冲突时，必须 force disconnect 摘除残留记录并重连成功，
// 而非把确定性失败抛回池（抛回 = 请求在队列里无限重试）。
func TestEnsureProjectNetwork_HealsStaleEndpoint(t *testing.T) {
	t.Parallel()
	f := &fakeNetClient{errs: map[string][]error{
		"connect/selfc": {errors.New(staleEndpointErrMsg)},
	}}
	d := newHealTestDaemon(f, "")

	name, err := d.EnsureProjectNetwork(context.Background(), "monsters", true)
	require.NoError(t, err)
	require.Equal(t, "tw-func-monsters-int", name)
	require.Equal(t, []string{
		"inspect tw-func-monsters-int",
		"connect tw-func-monsters-int selfc",
		"disconnect tw-func-monsters-int selfc force",
		"connect tw-func-monsters-int selfc",
	}, f.calls)
}

// TestEnsureProjectNetwork_AlreadyConnectedSwallowed 已在网类冲突（文本/
// Conflict 类）维持幂等吞掉，不得触发 disconnect。
func TestEnsureProjectNetwork_AlreadyConnectedSwallowed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"attached-text", errors.New("Error response from daemon: container selfc is already attached to network tw-func-monsters-int")},
		{"conflict-class", errdefs.ErrConflict},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeNetClient{errs: map[string][]error{
				"connect/selfc": {tc.err},
			}}
			d := newHealTestDaemon(f, "")

			_, err := d.EnsureProjectNetwork(context.Background(), "monsters", true)
			require.NoError(t, err)
			require.Equal(t, []string{
				"inspect tw-func-monsters-int",
				"connect tw-func-monsters-int selfc",
			}, f.calls)
		})
	}
}

// TestEnsureProjectNetwork_UnrelatedErrorPropagates 无关错误原样上抛，不得
// 误触发 disconnect 自愈。
func TestEnsureProjectNetwork_UnrelatedErrorPropagates(t *testing.T) {
	t.Parallel()
	f := &fakeNetClient{errs: map[string][]error{
		"connect/selfc": {errors.New("network tw-func-monsters-int unreachable")},
	}}
	d := newHealTestDaemon(f, "")

	_, err := d.EnsureProjectNetwork(context.Background(), "monsters", true)
	require.Error(t, err)
	require.ErrorContains(t, err, "attach dispatcher to network")
	require.Equal(t, []string{
		"inspect tw-func-monsters-int",
		"connect tw-func-monsters-int selfc",
	}, f.calls)
}

// TestEnsureProjectNetwork_HealDisconnectFailureSurfaces disconnect 失败
// （非 NotFound）不得盲目重连，错误须携带自愈语义上抛。
func TestEnsureProjectNetwork_HealDisconnectFailureSurfaces(t *testing.T) {
	t.Parallel()
	f := &fakeNetClient{errs: map[string][]error{
		"connect/selfc":    {errors.New(staleEndpointErrMsg)},
		"disconnect/selfc": {errors.New("connection refused")},
	}}
	d := newHealTestDaemon(f, "")

	_, err := d.EnsureProjectNetwork(context.Background(), "monsters", true)
	require.Error(t, err)
	require.ErrorContains(t, err, "heal stale endpoint")
	require.ErrorContains(t, err, "disconnect")
	require.Equal(t, []string{
		"inspect tw-func-monsters-int",
		"connect tw-func-monsters-int selfc",
		"disconnect tw-func-monsters-int selfc force",
	}, f.calls)
}

// TestEnsureProjectNetwork_HealDisconnectNotFoundTolerated disconnect 报
// NotFound = 残留记录已被并发 healer/外部摘除，视为已愈，重连应照常进行。
func TestEnsureProjectNetwork_HealDisconnectNotFoundTolerated(t *testing.T) {
	t.Parallel()
	f := &fakeNetClient{errs: map[string][]error{
		"connect/selfc":    {errors.New(staleEndpointErrMsg)},
		"disconnect/selfc": {errdefs.ErrNotFound},
	}}
	d := newHealTestDaemon(f, "")

	_, err := d.EnsureProjectNetwork(context.Background(), "monsters", true)
	require.NoError(t, err)
	require.Equal(t, []string{
		"inspect tw-func-monsters-int",
		"connect tw-func-monsters-int selfc",
		"disconnect tw-func-monsters-int selfc force",
		"connect tw-func-monsters-int selfc",
	}, f.calls)
}

// TestEnsureProjectNetwork_CallbackContainerHealWarnsOnly callback 容器
// attach 走同一 ensureConnected：陈旧 endpoint 冲突自愈成功则不告警不阻断
// （自愈失败仍只降级为告警——函数可能无需回访平台）。
func TestEnsureProjectNetwork_CallbackContainerHealWarnsOnly(t *testing.T) {
	t.Parallel()
	f := &fakeNetClient{errs: map[string][]error{
		"connect/cbc": {errors.New(staleEndpointErrMsg)},
	}}
	d := newHealTestDaemon(f, "cbc")

	_, err := d.EnsureProjectNetwork(context.Background(), "monsters", true)
	require.NoError(t, err)
	require.Equal(t, []string{
		"inspect tw-func-monsters-int",
		"connect tw-func-monsters-int selfc",
		"connect tw-func-monsters-int cbc",
		"disconnect tw-func-monsters-int cbc force",
		"connect tw-func-monsters-int cbc",
	}, f.calls)
}
