package dispatcher

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/fleetly/server/v1"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeImageRefStore 是 imageRefStore 的内存实现（Redis 映射面 fake）。
type fakeImageRefStore struct {
	mu     sync.Mutex
	images map[string]string
	err    error
}

func newFakeImageRefStore() *fakeImageRefStore {
	return &fakeImageRefStore{images: map[string]string{}}
}

func (s *fakeImageRefStore) SaveImageRef(_ context.Context, logicalName, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.images[logicalName] = ref
	return nil
}

func (s *fakeImageRefStore) LoadImageRef(_ context.Context, logicalName string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	return s.images[logicalName], nil
}

func (s *fakeImageRefStore) DeleteImageRef(_ context.Context, logicalName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	delete(s.images, logicalName)
	return nil
}

// spawnTestDaemon 组装 SpawnInstance/构建链的测试夹具。
func spawnTestDaemon(t *testing.T) (*fleetlyDaemon, *fakeTaskClient, *fakeImageRefStore) {
	t.Helper()
	cli := newFakeTaskClient()
	refs := newFakeImageRefStore()
	d := newFleetlyDaemon(importTestConfig(), refs, cli, nil)
	d.pollInterval = time.Millisecond
	d.probe = &fakeHealth{}
	return d, cli, refs
}

// TestTaskGroupRefVariants 作用域 ref 派生：p<project> / q<project> 两变体
// 结构不相交（首字符区分；后缀方案会让以 u 结尾的项目与不可信变体撞名），
// 非法 project id fail-closed。
func TestTaskGroupRefVariants(t *testing.T) {
	trusted, err := taskGroupRef("monsters", false)
	require.NoError(t, err)
	require.Equal(t, "pmonsters", trusted)

	untrusted, err := taskGroupRef("monsters", true)
	require.NoError(t, err)
	require.Equal(t, "qmonsters", untrusted)

	// 碰撞反证：以 q 开头的项目 ID 与「不可信变体」互不相等（首字符区分）。
	otherTrusted, err := taskGroupRef("qmonsters", false)
	require.NoError(t, err)
	require.NotEqual(t, untrusted, otherTrusted)

	_, err = taskGroupRef("Bad_ID", false)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestEnsureProjectNetworkMembersOncePerNetwork 挂靠声明每网一次：首次
// ensure 携带配置成员（app + service 列表），成功后再 ensure 不再携带；
// 网络名与作用域投影登记（SpawnInstance 消费）。
func TestEnsureProjectNetworkMembersOncePerNetwork(t *testing.T) {
	d, cli, _ := spawnTestDaemon(t)
	ctx := context.Background()

	name, err := d.EnsureProjectNetwork(ctx, "p1", false)
	require.NoError(t, err)
	require.Equal(t, "fleetly-taskgroup-pp1", name)
	require.Len(t, cli.ensureReqs, 1)
	req := cli.ensureReqs[0]
	require.Equal(t, "pp1", req.GetRef())
	require.False(t, req.GetInternal())
	require.Len(t, req.GetMembers(), 2)
	require.Equal(t, "torchwood", req.GetMembers()[0].GetApp())
	require.Equal(t, "dispatcher", req.GetMembers()[0].GetService())
	require.Equal(t, "server", req.GetMembers()[1].GetService())

	// 二次 ensure：同网（同 ref）幂等，不再携带成员（每网一次语义）。
	_, err = d.EnsureProjectNetwork(ctx, "p1", false)
	require.NoError(t, err)
	require.Len(t, cli.ensureReqs, 2)
	require.Empty(t, cli.ensureReqs[1].GetMembers(), "成员声明每网一次（成功即不再重发）")

	// 不可信变体：独立 ref + internal=true，成员声明独立计一次。
	_, err = d.EnsureProjectNetwork(ctx, "p1", true)
	require.NoError(t, err)
	require.Equal(t, "qp1", cli.ensureReqs[2].GetRef())
	require.True(t, cli.ensureReqs[2].GetInternal())
	require.Len(t, cli.ensureReqs[2].GetMembers(), 2)
}

// TestEnsureProjectNetworkFailureRetriesMembers 挂靠声明失败（如成员 app
// 在途部署 409）不标记已声明——下次 ensure 重发成员（fail-closed 重试）。
func TestEnsureProjectNetworkFailureRetriesMembers(t *testing.T) {
	d, cli, _ := spawnTestDaemon(t)
	cli.ensureErr = status.Error(codes.FailedPrecondition, "app torchwood has a deployment in flight")
	_, err := d.EnsureProjectNetwork(context.Background(), "p1", false)
	require.Error(t, err)

	cli.ensureErr = nil
	_, err = d.EnsureProjectNetwork(context.Background(), "p1", false)
	require.NoError(t, err)
	require.Len(t, cli.ensureReqs[1].GetMembers(), 2, "失败后重试必须重发成员声明")
}

// TestSpawnInstanceCreatesTaskWithScopeEnvAndResources CreateTask 载荷全量
// 断言：镜像映射解析、scope 引用、env（用户变量 + TW_MAX_REQUESTS /
// TW_DRAIN_TIMEOUT_MS）、TTL 兜底、资源规格映射、可读名。
func TestSpawnInstanceCreatesTaskWithScopeEnvAndResources(t *testing.T) {
	d, cli, refs := spawnTestDaemon(t)
	ctx := context.Background()
	require.NoError(t, refs.SaveImageRef(ctx, "torchwood-funcs/func-fn1-dep1",
		"registry.example.com/apps/func-fn1-dep1@sha256:abc"))
	network, err := d.EnsureProjectNetwork(ctx, "p1", false)
	require.NoError(t, err)

	inst, err := d.SpawnInstance(ctx, SpawnOptions{
		ProjectID:   "p1",
		FunctionID:  "fn1",
		Image:       "torchwood-funcs/func-fn1-dep1",
		Network:     network,
		Env:         []string{"GREETING=hi", "EMPTY="},
		Spec:        "shared-2x",
		MaxRequests: 42,
		Name:        "tw-fn-p1-fn1-abcdef",
	})
	require.NoError(t, err)
	require.Equal(t, "task-1", inst.ContainerID)
	require.Equal(t, "fleetly-task-task-1", inst.IP, "句柄地址 = 任务稳定 DNS 名")

	req := cli.createReqs[0]
	require.Equal(t, "registry.example.com/apps/func-fn1-dep1@sha256:abc", req.GetImage(), "spawn 镜像必须经映射解析为平台产物引用")
	require.Equal(t, "tw-fn-p1-fn1-abcdef", req.GetName())
	require.Equal(t, "task-group", req.GetScope().GetKind())
	require.Equal(t, "pp1", req.GetScope().GetRef())
	require.False(t, req.GetScope().GetInternal())
	require.Equal(t, int64(taskTTLSeconds), req.GetTtlSeconds(), "TTL = 平台上限兜底（池语义自持生命周期）")
	require.Equal(t, int64(1000), req.GetCpuMillis(), "shared-2x = 1.0 CPU")
	require.Equal(t, int64(512<<20), req.GetMemoryBytes())
	require.Equal(t, "hi", req.GetEnv()["GREETING"])
	require.Equal(t, "42", req.GetEnv()["TW_MAX_REQUESTS"])
	require.Equal(t, "10000", req.GetEnv()["TW_DRAIN_TIMEOUT_MS"])
}

// TestSpawnInstanceMappingMissPassesReferenceThrough 映射未命中：原样引用
// 交平台解析（平台侧失败 → 上层映射为镜像缺失，触发重建自愈）。
func TestSpawnInstanceMappingMissPassesReferenceThrough(t *testing.T) {
	d, cli, _ := spawnTestDaemon(t)
	ctx := context.Background()
	network, err := d.EnsureProjectNetwork(ctx, "p1", false)
	require.NoError(t, err)

	_, err = d.SpawnInstance(ctx, SpawnOptions{
		ProjectID: "p1", FunctionID: "fn1", Image: "torchwood-funcs/func-fn1-dep1", Network: network, Spec: "shared-1x",
	})
	require.NoError(t, err)
	require.Equal(t, "torchwood-funcs/func-fn1-dep1", cli.createReqs[0].GetImage())
}

// TestSpawnInstanceAwaitsRunning 平台收敛等待：queued → running（按拍轮询，
// statusSeq 确定性驱动）。
func TestSpawnInstanceAwaitsRunning(t *testing.T) {
	d, cli, _ := spawnTestDaemon(t)
	ctx := context.Background()
	cli.autoRunning = false
	cli.statusSeq["task-1"] = []string{"queued", "queued", "running"}
	network, err := d.EnsureProjectNetwork(ctx, "p1", false)
	require.NoError(t, err)

	inst, err := d.SpawnInstance(ctx, SpawnOptions{ProjectID: "p1", FunctionID: "fn1", Image: "img", Network: network, Spec: "shared-1x"})
	require.NoError(t, err)
	require.Equal(t, "task-1", inst.ContainerID)
}

// TestSpawnInstanceFailedTaskSurfacesReason 平台收敛失败（镜像拉取失败/
// 容器失败）：上抛平台台账原因并尽力回收任务。
func TestSpawnInstanceFailedTaskSurfacesReason(t *testing.T) {
	d, cli, _ := spawnTestDaemon(t)
	ctx := context.Background()
	cli.autoRunning = false
	cli.statusSeq["task-1"] = []string{"queued", "failed"}
	cli.failReason = "pull access denied"
	network, err := d.EnsureProjectNetwork(ctx, "p1", false)
	require.NoError(t, err)

	_, err = d.SpawnInstance(ctx, SpawnOptions{ProjectID: "p1", FunctionID: "fn1", Image: "img", Network: network, Spec: "shared-1x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "pull access denied")
	require.Contains(t, cli.deleteReqs, "task-1", "收敛失败的任务必须尽力回收")
}

// TestSpawnInstanceQuotaExceededFailsClosed 守卫③：平台配额触顶
// （E_TASK_QUOTA_EXCEEDED）→ ResourceExhausted 上抛（fail-closed，不吞、
// 不落实例记录、不进入排队等待）。
func TestSpawnInstanceQuotaExceededFailsClosed(t *testing.T) {
	d, cli, _ := spawnTestDaemon(t)
	cli.createErr = taskDetailErr("E_TASK_QUOTA_EXCEEDED", "task quota exceeded (concurrent tasks 16)")
	network, err := d.EnsureProjectNetwork(context.Background(), "p1", false)
	require.NoError(t, err)

	_, err = d.SpawnInstance(context.Background(), SpawnOptions{ProjectID: "p1", FunctionID: "fn1", Image: "img", Network: network, Spec: "shared-1x"})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))

	// 池级传播：ResourceExhausted 立即上抛（不等队首超时）。
	reg := newFakeRegistry()
	runner := &fakeRunner{}
	runner.healthy = true
	cfg := DefaultPoolConfig()
	cfg.PollInterval = time.Millisecond
	cfg.QueueHeadTimeout = 5 * time.Second
	pool := newPoolManager(d, reg, runner, cfg)
	start := time.Now()
	_, err = pool.Dispatch(context.Background(), dispatchReq())
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Less(t, time.Since(start), time.Second, "配额触顶必须 fail-fast，不得烧满队首超时")
}

// TestSpawnInstanceImageMissingMapsToRebuild 镜像不可得（E_IMAGE_PULL_FAILED）
// → FailedPrecondition + ImageMissingMarker（server 侧重建链路判据）。
func TestSpawnInstanceImageMissingMapsToRebuild(t *testing.T) {
	d, cli, _ := spawnTestDaemon(t)
	cli.createErr = taskDetailErr("E_IMAGE_PULL_FAILED", "task image x is not available")
	network, err := d.EnsureProjectNetwork(context.Background(), "p1", false)
	require.NoError(t, err)

	_, err = d.SpawnInstance(context.Background(), SpawnOptions{ProjectID: "p1", FunctionID: "fn1", Image: "img", Network: network, Spec: "shared-1x"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "deployment image missing")
	require.Contains(t, err.Error(), "rebuild required")
}

// TestInspectStopRemoveLifecycle 实例生命周期：Inspect 只认 running（终态/
// queued 均 false，任务不存在返回错误——池按幽灵清理）；Stop/Delete 幂等
// （NotFound 容忍）。
func TestInspectStopRemoveLifecycle(t *testing.T) {
	d, cli, _ := spawnTestDaemon(t)
	ctx := context.Background()
	cli.tasks["running-1"] = &serverv1.TaskView{Id: "running-1", Status: "running", DnsName: "fleetly-task-running-1"}
	cli.tasks["stopped-1"] = &serverv1.TaskView{Id: "stopped-1", Status: "stopped"}

	running, dns, err := d.InspectInstance(ctx, "running-1")
	require.NoError(t, err)
	require.True(t, running)
	require.Equal(t, "fleetly-task-running-1", dns)

	running, _, err = d.InspectInstance(ctx, "stopped-1")
	require.NoError(t, err)
	require.False(t, running)

	_, _, err = d.InspectInstance(ctx, "missing-1")
	require.Error(t, err, "任务不存在必须显式错误（池幽灵清理判据）")

	require.NoError(t, d.StopInstance(ctx, "running-1"))
	require.NoError(t, d.StopInstance(ctx, "missing-1"), "停止幂等：不存在视为已停止")
	require.NoError(t, d.RemoveInstance(ctx, "running-1"))
	require.NoError(t, d.RemoveInstance(ctx, "missing-1"), "删除幂等：不存在视为成功")
}

// TestInstanceLogsTailReportsTaskStatus 失败现场 = 任务台账投影（诚实边界：
// 容器日志在平台 VL，机具令牌无日志读面）。
func TestInstanceLogsTailReportsTaskStatus(t *testing.T) {
	d, cli, _ := spawnTestDaemon(t)
	cli.tasks["task-1"] = &serverv1.TaskView{
		Id: "task-1", Status: "failed", Error: "exec /tw-app: no such file or directory", StopReason: "exited",
	}
	tail, err := d.InstanceLogsTail(context.Background(), "task-1", maxLogTailBytes)
	require.NoError(t, err)
	require.Contains(t, tail, "status=failed")
	require.Contains(t, tail, "exec /tw-app: no such file or directory")
	require.Contains(t, tail, "VictoriaLogs", "诚实披露日志归属（机具令牌无日志读面）")
}

// TestFleetlyErrorCodeExtraction 错误码提取：信封 detail 优先，字符串前缀
// 兜底（无 detail 的 apperr 文本形态），无关错误返回空。
func TestFleetlyErrorCodeExtraction(t *testing.T) {
	require.Equal(t, "E_TASK_QUOTA_EXCEEDED", fleetlyErrorCode(taskDetailErr("E_TASK_QUOTA_EXCEEDED", "quota")))
	require.Equal(t, "E_IMAGE_PULL_FAILED",
		fleetlyErrorCode(status.Error(codes.Internal, "E_IMAGE_PULL_FAILED: task image x is not available")))
	require.Empty(t, fleetlyErrorCode(status.Error(codes.Internal, "something else")))
	require.Empty(t, fleetlyErrorCode(nil))
}

// TestMapFleetlyErrorCodes 错误分类映射矩阵（池可分类语义的唯一登记点）。
func TestMapFleetlyErrorCodes(t *testing.T) {
	cases := []struct {
		code string
		want codes.Code
	}{
		{"E_TASK_QUOTA_EXCEEDED", codes.ResourceExhausted},
		{"E_IMAGE_PULL_FAILED", codes.FailedPrecondition},
		{"E_TASK_UNSUPPORTED", codes.InvalidArgument},
		{"E_OTHER", codes.Internal},
	}
	for _, tc := range cases {
		got := mapFleetlyError("op", taskDetailErr(tc.code, "m"))
		require.Equal(t, tc.want, status.Code(got), "code=%s", tc.code)
	}
}

// TestUploadImageName 上传镜像仓组件名派生：小写化/非法字符折叠/长度收敛；
// function+deployment 全空形态 fail-closed。
func TestUploadImageName(t *testing.T) {
	name, err := uploadImageName("Fn_1", "DEP-1")
	require.NoError(t, err)
	require.Equal(t, "func-fn_1-dep-1", name)

	long, err := uploadImageName(strings.Repeat("a", 80), strings.Repeat("b", 80))
	require.NoError(t, err)
	require.LessOrEqual(t, len(long), 100)
	require.NotEmpty(t, long)

	_, err = uploadImageName("", "")
	require.Error(t, err)
}

// TestBuildImageUploadsRenderedContextAndRecordsMapping BuildImage 主链路：
// 渲染上下文（.tw-runner.js + Dockerfile）→ tar 流 → BuildFromUpload
// （name/Dockerfile 入口）→ 登记逻辑名映射；Verify=false 不触验证 spawn。
func TestBuildImageUploadsRenderedContextAndRecordsMapping(t *testing.T) {
	d, cli, refs := spawnTestDaemon(t)
	zip := makeEntryZipFiles(t, map[string]string{
		"index.js":          "module.exports.main = () => ({})",
		"package.json":      `{"dependencies": {"left-pad": "1.3.0"}}`,
		"package-lock.json": `{"lockfileVersion": 1}`,
	})

	err := d.BuildImage(context.Background(), BuildImageOptions{
		ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1",
		Zip: zip, Runtime: "node-18.0",
	})
	require.NoError(t, err)

	require.Equal(t, 1, cli.buildCalls)
	require.Equal(t, "func-fn1-dep1", cli.buildName)
	require.Equal(t, "Dockerfile", cli.buildDocker)
	entries := readTarEntries(t, cli.buildTar)
	require.Contains(t, entries, ".tw-runner.js", "node 分支必须携带 runner 脚本")
	require.Contains(t, entries, "Dockerfile")
	require.Contains(t, entries, "index.js")

	got, err := refs.LoadImageRef(context.Background(), "torchwood-funcs/func-fn1-dep1")
	require.NoError(t, err)
	require.Equal(t, "registry.example.com/apps/func-fn1-dep1@sha256:deadbeef", got)
	require.Empty(t, cli.createReqs, "Verify=false 不得触发验证 spawn")
}

// TestBuildImageVerifyFailureFailsBuild Verify=true：构建成功但验证 spawn
// 失败 → 构建失败上抛（映射保留——产物引用仍可复用）。
func TestBuildImageVerifyFailureFailsBuild(t *testing.T) {
	d, cli, refs := spawnTestDaemon(t)
	d.bootTimeout = 20 * time.Millisecond
	d.probe = &fakeHealth{fails: -1}
	zip := makeEntryZipFiles(t, map[string]string{"index.js": "module.exports.main = () => ({})"})

	err := d.BuildImage(context.Background(), BuildImageOptions{
		ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1",
		Zip: zip, Runtime: "node-18.0", Verify: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "verification failed")
	require.Len(t, cli.createReqs, 1, "验证 spawn 必须创建验证任务")
	got, lerr := refs.LoadImageRef(context.Background(), "torchwood-funcs/func-fn1-dep1")
	require.NoError(t, lerr)
	require.NotEmpty(t, got, "构建成功即登记映射（验证失败不污染引用）")
}

// TestBuildImageFailureDoesNotRecordMapping 平台构建失败：不落映射（上层
// 以构建失败收场）。
func TestBuildImageFailureDoesNotRecordMapping(t *testing.T) {
	d, cli, refs := spawnTestDaemon(t)
	cli.buildErr = status.Error(codes.Internal, "build failed: invalid Dockerfile")
	zip := makeEntryZipFiles(t, map[string]string{"index.js": "module.exports = {}"})

	err := d.BuildImage(context.Background(), BuildImageOptions{
		ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1",
		Zip: zip, Runtime: "node-18.0",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "fleetly build failed")
	got, lerr := refs.LoadImageRef(context.Background(), "torchwood-funcs/func-fn1-dep1")
	require.NoError(t, lerr)
	require.Empty(t, got)
}

// TestBuildImageEmptyImageRefFails 平台构建成功但未回引用：显式失败（不落
// 空映射——后续 spawn 无从解析）。
func TestBuildImageEmptyImageRefFails(t *testing.T) {
	d, cli, _ := spawnTestDaemon(t)
	cli.buildResp = &serverv1.BuildFromUploadResponse{Build: &serverv1.BuildView{Id: "b1", Status: "succeeded"}}
	zip := makeEntryZipFiles(t, map[string]string{"index.js": "module.exports = {}"})

	err := d.BuildImage(context.Background(), BuildImageOptions{
		ProjectID: "p1", FunctionID: "fn1", DeploymentID: "dep1",
		Zip: zip, Runtime: "node-18.0",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no image reference")
}

// TestRemoveImageDropsMapping RemoveImage 幂等删除映射（平台产物 GC 归平台）。
func TestRemoveImageDropsMapping(t *testing.T) {
	d, _, refs := spawnTestDaemon(t)
	ctx := context.Background()
	require.NoError(t, refs.SaveImageRef(ctx, "torchwood-funcs/func-fn1-dep1", "ref-1"))

	require.NoError(t, d.RemoveImage(ctx, "fn1", "dep1"))
	got, err := refs.LoadImageRef(ctx, "torchwood-funcs/func-fn1-dep1")
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, d.RemoveImage(ctx, "fn1", "dep1"), "删除幂等")
}

// TestSpawnInstanceUnknownNetworkFailsExplicit 调用顺序违约（未经 ensure 的
// 网络名）：显式失败，不猜测 ref/变体。
func TestSpawnInstanceUnknownNetworkFailsExplicit(t *testing.T) {
	d, _, _ := spawnTestDaemon(t)
	_, err := d.SpawnInstance(context.Background(), SpawnOptions{
		ProjectID: "p1", FunctionID: "fn1", Image: "img", Network: "fleetly-taskgroup-unknown", Spec: "shared-1x",
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "not known to this dispatcher process")
}

// TestFleetlyClientUnavailableWithoutEndpoint 未装配 fleetly 端点：调用面
// 显式 FailedPrecondition（不静默降级）。
func TestFleetlyClientUnavailableWithoutEndpoint(t *testing.T) {
	cfg := &config.AppConfig{Functions: &config.Functions{Docker: &config.Functions_Docker{}}}
	d := NewFleetlyDaemon(cfg, newFakeImageRefStore())
	_, err := d.EnsureProjectNetwork(context.Background(), "p1", false)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "functions.fleetly.endpoint")
}
