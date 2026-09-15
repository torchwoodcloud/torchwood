package functionsdispatcher

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// TestTarDir_NormalizesModesIndependentOfUmask 权限坏档修复（EACCES 秒退事故）：
// 构建进程 umask 会掩蔽 os.WriteFile/OpenFile 声明的 0644（umask 0077 → 落盘
// 0600），FileInfoHeader 忠实保留磁盘实际 mode 经 COPY 进镜像——模板 USER node
// 读 .tw-runner.js 即 EACCES、容器秒退。tarDir 必须在 tar header 单点归一化：
// 文件恒 0644、目录恒 0755、属主归零，镜像权限与 dispatcher 以何用户/何
// umask 运行解耦。Linux 下置 umask 0077 复现生产掩蔽形态（修复前本测试必红）。
func TestTarDir_NormalizesModesIndependentOfUmask(t *testing.T) {
	old := setTestUmask(0o077)
	defer setTestUmask(old)

	dir := t.TempDir()
	// 写盘用收紧 mode（0600/0750）：直接模拟生产 umask 掩蔽后的磁盘状态
	// （0644 声明值被 umask 0077 掩蔽成 0600 的形态），断言 tarDir 归一化能
	// 向上收回 0644/0755——镜像内权限不依赖磁盘上的实际 mode。
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".tw-runner.js"), []byte("runner"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.js"), []byte("code"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "lib"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", "util.js"), []byte("util"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM x"), 0o600))

	rd, err := tarDir(dir)
	require.NoError(t, err)
	tr := tar.NewReader(rd)
	sawFile, sawDir := 0, 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeDir {
			sawDir++
			require.Equal(t, int64(0o755), hdr.Mode, "dir %s", hdr.Name)
		} else {
			sawFile++
			require.Equal(t, int64(0o644), hdr.Mode, "file %s", hdr.Name)
		}
		require.Zero(t, hdr.Uid, "entry %s", hdr.Name)
		require.Zero(t, hdr.Gid, "entry %s", hdr.Name)
	}
	require.Positive(t, sawFile)
	require.Equal(t, 1, sawDir)
}

// —— EnsureProjectNetwork attach 幂等/陈旧 endpoint 自愈（2026-09-14 dev
// 事故回归：dispatcher 容器非优雅重建后 tw-func-* 网络残留同名 endpoint，
// attach 确定性失败，monsters 项目函数队列永久卡死）——

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

func (f *fakeNetClient) NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error) {
	f.record("inspect " + networkID)
	return network.Inspect{}, nil
}

func (f *fakeNetClient) NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error) {
	f.record("create " + name)
	return network.CreateResponse{}, nil
}

func (f *fakeNetClient) NetworkConnect(ctx context.Context, networkID, containerID string, cfg *network.EndpointSettings) error {
	f.record("connect " + networkID + " " + containerID)
	return f.pop("connect", containerID)
}

func (f *fakeNetClient) NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error {
	call := "disconnect " + networkID + " " + containerID
	if force {
		call += " force"
	}
	f.record(call)
	return f.pop("disconnect", containerID)
}

// newHealTestDaemon 构造注入 fake 的 dockerDaemon（selfContainerID 非空 =
// 容器内模式，走自 attach 路径）。
func newHealTestDaemon(f *fakeNetClient, callbackContainer string) *dockerDaemon {
	cfg := &config.AppConfig{
		Functions: &config.Functions{
			Docker: &config.Functions_Docker{},
		},
	}
	if callbackContainer != "" {
		cfg.Functions.Dispatcher = &config.Functions_Dispatcher{CallbackContainer: callbackContainer}
	}
	return &dockerDaemon{cfg: cfg, netCli: f, selfContainerID: "selfc"}
}

// TestEnsureProjectNetwork_HealsStaleEndpoint 事故主回归：自 attach 撞上
// 陈旧 endpoint 名冲突时，必须 force disconnect 摘除残留记录并重连成功，
// 而非把确定性失败抛回池（抛回 = 请求在队列里无限重试）。
func TestEnsureProjectNetwork_HealsStaleEndpoint(t *testing.T) {
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
	cases := []struct {
		name string
		err  error
	}{
		{"attached-text", errors.New("Error response from daemon: container selfc is already attached to network tw-func-monsters-int")},
		{"conflict-class", errdefs.ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

// TestEnsureProjectNetwork_HealReconnectStillFails 摘除后重连仍失败须上抛
// （不再吞错——下一轮 spawn 重试会再次走完整自愈）。
func TestEnsureProjectNetwork_HealReconnectStillFails(t *testing.T) {
	f := &fakeNetClient{errs: map[string][]error{
		"connect/selfc": {errors.New(staleEndpointErrMsg), errors.New(staleEndpointErrMsg)},
	}}
	d := newHealTestDaemon(f, "")

	_, err := d.EnsureProjectNetwork(context.Background(), "monsters", true)
	require.Error(t, err)
	require.ErrorContains(t, err, "heal stale endpoint")
	require.ErrorContains(t, err, "reconnect")
}

// TestEnsureProjectNetwork_HealDisconnectNotFoundTolerated disconnect 报
// NotFound = 残留记录已被并发 healer/外部摘除，视为已愈，重连应照常进行。
func TestEnsureProjectNetwork_HealDisconnectNotFoundTolerated(t *testing.T) {
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
