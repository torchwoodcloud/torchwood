package dockerdriver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/dispatcher"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 本文件覆盖常驻实例生命周期四原语（Spawn/Inspect/Stop/Remove/LogsTail）的
// 驱动侧确定性单测——有事故史的面（2026-09-14 attach、2026-09-29 探针）回归
// 不再全押 dind E2E。镜像缺失类型化上抛（rebuild 自愈链源头）、加固配置、
// 信号映射、IP 提取、stdcopy 解复用在此钉死；真实 daemon 状态机收敛语义
// 仍由 TW_E2E_DOCKER e2e 兜底。

var (
	_ containerClient = (*fakeCreateClient)(nil)
	_ lifecycleClient = (*fakeLifecycleClient)(nil)
	_                 = lifecycleTestDaemon
)

// fakeCreateClient 是 containerClient 的可编程 fake：捕获 create 载荷
// （env/加固配置/名字），按字段注入错误，记录 remove 流水。
type fakeCreateClient struct {
	mu        sync.Mutex
	createErr error
	startErr  error

	lastCfg     *container.Config
	lastHostCfg *container.HostConfig
	lastName    string
	removes     []string
}

func (f *fakeCreateClient) ContainerCreate(_ context.Context, cfg *container.Config, hostCfg *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCfg, f.lastHostCfg, f.lastName = cfg, hostCfg, name
	if f.createErr != nil {
		return container.CreateResponse{}, f.createErr
	}
	return container.CreateResponse{ID: "c1"}, nil
}

func (f *fakeCreateClient) ContainerStart(_ context.Context, _ string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startErr
}

func (f *fakeCreateClient) ContainerRemove(_ context.Context, containerID string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, containerID)
	return nil
}

// fakeLifecycleClient 是 lifecycleClient 的可编程 fake。
type fakeLifecycleClient struct {
	mu          sync.Mutex
	inspectResp container.InspectResponse
	inspectErr  error
	stopErr     error
	removeErr   error
	logsReader  io.ReadCloser
	logsErr     error

	lastStop    container.StopOptions
	removeCalls []string
	logsOpts    *container.LogsOptions
}

func (f *fakeLifecycleClient) ContainerInspect(_ context.Context, _ string) (container.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspectResp, f.inspectErr
}

func (f *fakeLifecycleClient) ContainerStop(_ context.Context, _ string, opts container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStop = opts
	return f.stopErr
}

func (f *fakeLifecycleClient) ContainerRemove(_ context.Context, containerID string, opts container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls = append(f.removeCalls, containerID)
	return f.removeErr
}

func (f *fakeLifecycleClient) ContainerLogs(_ context.Context, _ string, opts container.LogsOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsOpts = &opts
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	return f.logsReader, nil
}

// muxFrame 编码一条 stdcopy 多路复用帧（8 字节头 = 流类型 + BE 长度）。
func muxFrame(stream byte, payload string) []byte {
	buf := make([]byte, 8+len(payload))
	buf[0] = stream
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(payload)))
	copy(buf[8:], payload)
	return buf
}

// runningInspect / notRunningInspect 构造 inspect 响应（State 挂在嵌入的
// ContainerJSONBase 上；单网络挂载，IP 空 = 无地址形态）。
func runningInspect(ip string) container.InspectResponse {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Running: true}},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"tw-func-p1": {IPAddress: ip},
		}},
	}
}

func notRunningInspect() container.InspectResponse {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Running: false}},
	}
}

func lifecycleTestDaemon(c *fakeCreateClient, l *fakeLifecycleClient) *daemon {
	return &daemon{
		cfg:          &config.AppConfig{Functions: &config.Functions{Docker: &config.Functions_Docker{}}},
		containerCli: c,
		lifecycleCli: l,
	}
}

func spawnOpts() dispatcher.SpawnOptions {
	return dispatcher.SpawnOptions{
		ProjectID:   "p1",
		FunctionID:  "fn1",
		Image:       "torchwood-funcs/func-fn1-dep1",
		Network:     "tw-func-p1",
		Env:         []string{"GREETING=hi"},
		Spec:        "shared-1x",
		MaxRequests: 1000,
		Name:        "tw-fn-p1-fn1-abcdef",
	}
}

// TestSpawnInstance_HappyPathAndHardening 健全路径：create+start+inspect
// 返回实例；加固配置（CapDrop ALL / no-new-privileges / 只读 rootfs /
// tmpfs /tmp / pids 上限）与控制键注入（TW_MAX_REQUESTS/TW_DRAIN_TIMEOUT_MS）
// 逐项断言。
func TestSpawnInstance_HappyPathAndHardening(t *testing.T) {
	c := &fakeCreateClient{}
	l := &fakeLifecycleClient{inspectResp: runningInspect("172.19.0.5")}
	d := lifecycleTestDaemon(c, l)

	inst, err := d.SpawnInstance(context.Background(), spawnOpts())
	require.NoError(t, err)
	require.Equal(t, "c1", inst.ContainerID)
	require.Equal(t, "172.19.0.5", inst.IP)
	require.Empty(t, c.removes, "健康路径无失败清理")

	// 加固与资源约束（54b666b 语义）。
	hc := c.lastHostCfg
	require.Equal(t, container.NetworkMode("tw-func-p1"), hc.NetworkMode)
	require.Equal(t, []string{"ALL"}, []string(hc.CapDrop))
	require.Equal(t, []string{"no-new-privileges"}, []string(hc.SecurityOpt))
	require.True(t, hc.ReadonlyRootfs)
	require.Contains(t, hc.Tmpfs, "/tmp")
	require.NotNil(t, hc.Resources.PidsLimit)
	require.Equal(t, int64(512), *hc.Resources.PidsLimit)
	require.NotNil(t, c.lastCfg.StopTimeout)
	require.Equal(t, 10, *c.lastCfg.StopTimeout)

	// env：用户变量透传 + 控制键（AppendRunnerControlEnv 单点注入）。
	require.Contains(t, c.lastCfg.Env, "GREETING=hi")
	require.Contains(t, c.lastCfg.Env, "TW_MAX_REQUESTS=1000")
	require.Contains(t, c.lastCfg.Env, "TW_DRAIN_TIMEOUT_MS=10000")
	require.Equal(t, "tw-fn-p1-fn1-abcdef", c.lastName)
}

// TestSpawnInstance_ImageMissingTypedUpcast 事故面回归：镜像缺失必须以
// FailedPrecondition + ImageMissingMarker 类型化上抛（server 侧据此触发自动
// 重建），而非裸 NotFound 烧满队首超时。
func TestSpawnInstance_ImageMissingTypedUpcast(t *testing.T) {
	c := &fakeCreateClient{createErr: fmt.Errorf("Error response from daemon: No such image: torchwood-funcs/func-fn1-dep1:latest: %w", errdefs.ErrNotFound)}
	l := &fakeLifecycleClient{}
	d := lifecycleTestDaemon(c, l)

	_, err := d.SpawnInstance(context.Background(), spawnOpts())
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, domainfunctions.ImageMissingMarker)
	require.ErrorContains(t, err, "rebuild required")
	require.Empty(t, c.removes, "create 失败无容器可清理")
}

// TestSpawnInstance_ImageMissingLookalikeNotUpcast NotFound 但非镜像缺失
// 文案（网络缺失等形态）不走类型化上抛——普通包装错误。
func TestSpawnInstance_ImageMissingLookalikeNotUpcast(t *testing.T) {
	c := &fakeCreateClient{createErr: fmt.Errorf("network tw-func-p1 not found: %w", errdefs.ErrNotFound)}
	l := &fakeLifecycleClient{}
	d := lifecycleTestDaemon(c, l)

	_, err := d.SpawnInstance(context.Background(), spawnOpts())
	require.Error(t, err)
	require.NotEqual(t, codes.FailedPrecondition, status.Code(err), "非镜像缺失 NotFound 不映射 FailedPrecondition")
}

// TestSpawnInstance_StartFailureCleansUp start 失败必须清理半成品容器。
func TestSpawnInstance_StartFailureCleansUp(t *testing.T) {
	c := &fakeCreateClient{startErr: errors.New("oci runtime error")}
	l := &fakeLifecycleClient{}
	d := lifecycleTestDaemon(c, l)

	_, err := d.SpawnInstance(context.Background(), spawnOpts())
	require.Error(t, err)
	require.ErrorContains(t, err, "start resident instance")
	require.Equal(t, []string{"c1"}, c.removes, "start 失败清理半成品容器")
}

// TestSpawnInstance_UnhealthyCleansUp inspect 判不运行 / 无 IP 均清理并报
// Internal（不留无 IP 的半成品实例）。
func TestSpawnInstance_UnhealthyCleansUp(t *testing.T) {
	cases := []struct {
		name string
		resp container.InspectResponse
	}{
		{"not-running", notRunningInspect()},
		{"no-ip", runningInspect("")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeCreateClient{}
			l := &fakeLifecycleClient{inspectResp: tc.resp}
			d := lifecycleTestDaemon(c, l)

			_, err := d.SpawnInstance(context.Background(), spawnOpts())
			require.Error(t, err)
			require.Equal(t, codes.Internal, status.Code(err))
			require.Equal(t, []string{"c1"}, c.removes)
		})
	}
}

// TestStopInstance_SignalMapping timeout <= 0 直接 SIGKILL（请求超时/崩溃
// 回收路径，池全路径实际走法）；timeout > 0 秒化宽限（drain 形态）。
func TestStopInstance_SignalMapping(t *testing.T) {
	c := &fakeCreateClient{}
	l := &fakeLifecycleClient{}
	d := lifecycleTestDaemon(c, l)

	require.NoError(t, d.StopInstance(context.Background(), "c1", 0))
	require.Nil(t, l.lastStop.Timeout)
	require.Equal(t, "SIGKILL", l.lastStop.Signal)

	require.NoError(t, d.StopInstance(context.Background(), "c1", 5*time.Second))
	require.NotNil(t, l.lastStop.Timeout)
	require.Equal(t, 5, *l.lastStop.Timeout)
	require.Empty(t, l.lastStop.Signal)

	// 不足 1s 的宽限按 1s 下限（daemon 侧 0 = 立即杀语义）。
	require.NoError(t, d.StopInstance(context.Background(), "c1", 100*time.Millisecond))
	require.NotNil(t, l.lastStop.Timeout)
	require.Equal(t, 1, *l.lastStop.Timeout)

	// NotFound 幂等吞掉；其他错误包装上抛。
	l.stopErr = errdefs.ErrNotFound
	require.NoError(t, d.StopInstance(context.Background(), "c1", 0))
	l.stopErr = errors.New("daemon unreachable")
	require.ErrorContains(t, d.StopInstance(context.Background(), "c1", 0), "stop resident instance")
}

// TestRemoveInstance_ForceAndIdempotent 强删恒 Force；NotFound 幂等吞掉。
func TestRemoveInstance_ForceAndIdempotent(t *testing.T) {
	c := &fakeCreateClient{}
	l := &fakeLifecycleClient{}
	d := lifecycleTestDaemon(c, l)

	require.NoError(t, d.RemoveInstance(context.Background(), "c1"))
	require.Equal(t, []string{"c1"}, l.removeCalls)

	l.removeErr = errdefs.ErrNotFound
	require.NoError(t, d.RemoveInstance(context.Background(), "ghost"))
	l.removeErr = errors.New("daemon unreachable")
	require.ErrorContains(t, d.RemoveInstance(context.Background(), "c1"), "remove resident instance")
}

// TestInspectInstance_FirstNonEmptyIP 多网络挂载时取首个非空地址；未运行
// 返回 (false, "", nil) 不报错。
func TestInspectInstance_FirstNonEmptyIP(t *testing.T) {
	c := &fakeCreateClient{}
	l := &fakeLifecycleClient{inspectResp: runningInspect("172.19.0.7")}
	d := lifecycleTestDaemon(c, l)

	running, ip, err := d.InspectInstance(context.Background(), "c1")
	require.NoError(t, err)
	require.True(t, running)
	require.Equal(t, "172.19.0.7", ip)

	l.inspectResp = notRunningInspect()
	running, ip, err = d.InspectInstance(context.Background(), "c1")
	require.NoError(t, err)
	require.False(t, running)
	require.Empty(t, ip)
}

// TestInstanceLogsTail_DemuxAndTailLimit stdcopy 多路复用流解复用合并
// stdout/stderr；超限取末尾 limit 字节（失败现场在尾部）；Logs 选项钉死
// ShowStdout/ShowStderr + Tail 200。
func TestInstanceLogsTail_DemuxAndTailLimit(t *testing.T) {
	c := &fakeCreateClient{}
	l := &fakeLifecycleClient{logsReader: io.NopCloser(strings.NewReader(
		string(muxFrame(0, "stdout line\n")) + string(muxFrame(1, "stderr line\n")),
	))}
	d := lifecycleTestDaemon(c, l)

	out, err := d.InstanceLogsTail(context.Background(), "c1", 1024)
	require.NoError(t, err)
	require.Contains(t, out, "stdout line\n")
	require.Contains(t, out, "stderr line\n")
	require.Equal(t, "200", l.logsOpts.Tail)
	require.True(t, l.logsOpts.ShowStdout)
	require.True(t, l.logsOpts.ShowStderr)

	// 尾部截断：payload 20 字节、limit 8 → 保留末 8 字节。
	long := strings.Repeat("x", 20)
	l.logsReader = io.NopCloser(strings.NewReader(string(muxFrame(0, long))))
	out, err = d.InstanceLogsTail(context.Background(), "c1", 8)
	require.NoError(t, err)
	require.Equal(t, long[len(long)-8:], out)

	// Logs 失败包装上抛。
	l.logsReader = nil
	l.logsErr = errdefs.ErrNotFound
	_, err = d.InstanceLogsTail(context.Background(), "ghost", 1024)
	require.ErrorContains(t, err, "container logs")
}
