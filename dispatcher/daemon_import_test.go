package dispatcher

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/fleetly/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/fleetly/shared/v1"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeTaskClient 是 fleetlyTaskClient 的可编程 fake（确定性驱动任务/构建/
// 错误映射路径，不依赖真实平台）。语义：
//   - CreateTask 按 createErr/createFail 上抛，否则落 tasks 表（默认 queued；
//     autoRunning=true 时直接 running）；
//   - GetTask 读 tasks 表；不存在的任务返回 NotFound（任务生命周期断言面）；
//   - StopTask/DeleteTask 记流水并删表（幂等面）；
//   - BuildFromUpload 记录 name/dockerfile/上下文 tar 字节，返回 buildResp。
type fakeTaskClient struct {
	mu sync.Mutex

	createErr    error
	autoRunning  bool
	createStatus string // 非空 = CreateTask 初始状态（默认 queued/autoRunning）
	failReason   string // GetTask 状态为 failed 时填充 Error（确定性失败原因断言）
	pinnedImage  string // 非空 = CreateTask 的钉定镜像（模拟平台解析结果）
	createReqs   []*serverv1.CreateTaskRequest
	createResp   *serverv1.CreateTaskResponse
	tasks        map[string]*serverv1.TaskView
	getErr       error
	ensureErr    error
	ensureReqs   []*serverv1.EnsureTaskNetworkRequest
	ensureResp   *serverv1.EnsureTaskNetworkResponse
	stopReqs     []string
	stopErr      error
	deleteReqs   []string
	deleteErr    error
	buildErr     error
	buildResp    *serverv1.BuildFromUploadResponse
	buildName    string
	buildDocker  string
	buildTar     []byte
	buildCalls   int
	nextTaskID   int
	statusSeq    map[string][]string // taskID -> 依次返回的状态（耗尽后取末位）
}

func newFakeTaskClient() *fakeTaskClient {
	return &fakeTaskClient{
		tasks:       map[string]*serverv1.TaskView{},
		statusSeq:   map[string][]string{},
		autoRunning: true,
	}
}

func (f *fakeTaskClient) EnsureTaskNetwork(_ context.Context, req *serverv1.EnsureTaskNetworkRequest) (*serverv1.EnsureTaskNetworkResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureReqs = append(f.ensureReqs, req)
	if f.ensureErr != nil {
		return nil, f.ensureErr
	}
	if f.ensureResp != nil {
		return f.ensureResp, nil
	}
	name := "fleetly-taskgroup-" + req.GetRef()
	return &serverv1.EnsureTaskNetworkResponse{Name: name, Internal: req.GetInternal()}, nil
}

func (f *fakeTaskClient) CreateTask(_ context.Context, req *serverv1.CreateTaskRequest) (*serverv1.CreateTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createReqs = append(f.createReqs, req)
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createResp != nil {
		return f.createResp, nil
	}
	f.nextTaskID++
	id := fmt.Sprintf("task-%d", f.nextTaskID)
	task := &serverv1.TaskView{
		Id:          id,
		Name:        req.GetName(),
		Image:       req.GetImage(),
		Env:         req.GetEnv(),
		Scope:       req.GetScope(),
		TtlSeconds:  req.GetTtlSeconds(),
		CpuMillis:   req.GetCpuMillis(),
		MemoryBytes: req.GetMemoryBytes(),
		Service:     "fleetly-task-" + id,
		DnsName:     "fleetly-task-" + id,
		Status:      "queued",
		CreatedAt:   timestamppb.Now(),
	}
	if f.autoRunning {
		task.Status = "running"
		task.StartedAt = timestamppb.Now()
	}
	if f.createStatus != "" {
		task.Status = f.createStatus
	}
	if f.pinnedImage != "" {
		task.Image = f.pinnedImage
	}
	f.tasks[id] = task
	return &serverv1.CreateTaskResponse{Task: cloneTask(task)}, nil
}

func (f *fakeTaskClient) GetTask(_ context.Context, req *serverv1.GetTaskRequest) (*serverv1.GetTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	task, ok := f.tasks[req.GetId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task not found: %s", req.GetId())
	}
	if seq := f.statusSeq[req.GetId()]; len(seq) > 0 {
		task.Status = seq[0]
		if len(seq) > 1 {
			f.statusSeq[req.GetId()] = seq[1:]
		}
	}
	if task.Status == "failed" && f.failReason != "" {
		task.Error = f.failReason
	}
	return &serverv1.GetTaskResponse{Task: cloneTask(task)}, nil
}

func (f *fakeTaskClient) StopTask(_ context.Context, req *serverv1.StopTaskRequest) (*serverv1.StopTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopReqs = append(f.stopReqs, req.GetId())
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	task, ok := f.tasks[req.GetId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task not found: %s", req.GetId())
	}
	task.Status = "stopping"
	return &serverv1.StopTaskResponse{Task: cloneTask(task)}, nil
}

func (f *fakeTaskClient) DeleteTask(_ context.Context, req *serverv1.DeleteTaskRequest) (*serverv1.DeleteTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteReqs = append(f.deleteReqs, req.GetId())
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if _, ok := f.tasks[req.GetId()]; !ok {
		return nil, status.Errorf(codes.NotFound, "task not found: %s", req.GetId())
	}
	delete(f.tasks, req.GetId())
	return &serverv1.DeleteTaskResponse{Id: req.GetId()}, nil
}

func (f *fakeTaskClient) BuildFromUpload(_ context.Context, name, dockerfile string, contextTar io.Reader) (*serverv1.BuildFromUploadResponse, error) {
	f.mu.Lock()
	f.buildCalls++
	f.buildName = name
	f.buildDocker = dockerfile
	f.mu.Unlock()
	raw, err := io.ReadAll(contextTar)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.buildTar = raw
	f.mu.Unlock()
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	if f.buildResp != nil {
		return f.buildResp, nil
	}
	return &serverv1.BuildFromUploadResponse{Build: &serverv1.BuildView{
		Id:          "build-1",
		Status:      "succeeded",
		ImageRef:    "registry.example.com/apps/" + name + "@sha256:deadbeef",
		ImageDigest: "sha256:deadbeef",
	}}, nil
}

func cloneTask(task *serverv1.TaskView) *serverv1.TaskView {
	return proto.Clone(task).(*serverv1.TaskView)
}

// taskDetailErr 构造带 fleetly 错误信封 detail 的 gRPC status（与 fleetly
// apperr 的传输形态同构——错误码提取路径断言面）。
func taskDetailErr(code, message string) error {
	st := status.New(codes.Internal, message)
	withDetail, err := st.WithDetails(&sharedv1.ErrorResponse{Code: code, Message: message})
	if err != nil {
		return st.Err()
	}
	return withDetail.Err()
}

// importTestConfig 组装 ImportImage/BuildImage 链的最小配置（registry 命名
// 前缀 + 空 image 白名单 + fleetly 端点已装配由注入 fake 承担）。
func importTestConfig() *config.AppConfig {
	return &config.AppConfig{Functions: &config.Functions{
		Docker:     &config.Functions_Docker{},
		Dispatcher: &config.Functions_Dispatcher{BootTimeout: "2s"},
		Fleetly:    &config.Functions_Fleetly{Endpoint: "fake:1", Token: "tok", App: "torchwood", NetworkMembers: []string{"dispatcher", "server"}},
	}}
}

// newImportTestDaemon 组装 ImportImage 测试夹具：fake fleetly 客户端 +
// 内存映射表 + 毫秒级探针轮询。
func newImportTestDaemon(t *testing.T) (*fleetlyDaemon, *fakeTaskClient, *fakeImageRefStore) {
	t.Helper()
	cli := newFakeTaskClient()
	refs := newFakeImageRefStore()
	d := newFleetlyDaemon(importTestConfig(), refs, cli, nil)
	d.pollInterval = time.Millisecond
	d.probe = &fakeHealth{} // 默认探针就绪；失败路径用例按需覆盖
	return d, cli, refs
}

func importOpts() ImportImageOptions {
	return ImportImageOptions{
		ProjectID:    "p1",
		FunctionID:   "fn1",
		DeploymentID: "dep1",
		Reference:    "ghcr.io/acme/greet:v1",
		Env:          map[string]string{"GREETING": "hi"},
	}
}

// TestImportImage_ResolvesPinsAndRecordsMapping 首次导入主链路：host 校验 →
// 验证任务（fleetly CreateTask 解析/拉取/钉定）→ digest 一致性 → 契约验证
// spawn 通过 → 登记逻辑名映射 → 返回钉死 digest；验证任务被 Stop+Delete。
func TestImportImage_ResolvesPinsAndRecordsMapping(t *testing.T) {
	d, cli, refs := newImportTestDaemon(t)
	cli.pinnedImage = "ghcr.io/acme/greet@sha256:abc123"

	digest, err := d.ImportImage(context.Background(), importOpts())
	require.NoError(t, err)
	require.Equal(t, "sha256:abc123", digest)

	// 验证任务载荷：镜像 = 用户引用、scope = 项目 task-group、env 携带函数
	// variables（TW_DATA/TW_EXECUTION_TOKEN 不注入）。
	require.Len(t, cli.createReqs, 1)
	req := cli.createReqs[0]
	require.Equal(t, "ghcr.io/acme/greet:v1", req.GetImage())
	require.Equal(t, "task-group", req.GetScope().GetKind())
	require.Equal(t, "pp1", req.GetScope().GetRef())
	require.False(t, req.GetScope().GetInternal())
	require.Equal(t, "hi", req.GetEnv()["GREETING"])
	require.NotContains(t, req.GetEnv(), "TW_DATA")
	require.Equal(t, int64(taskTTLSeconds), req.GetTtlSeconds())

	// 映射登记：逻辑名 → 平台钉定引用。
	got, err := refs.LoadImageRef(context.Background(), "torchwood-funcs/func-fn1-dep1")
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/acme/greet@sha256:abc123", got)

	// 验证任务回收（Stop+Delete）。
	require.Equal(t, []string{"task-1"}, cli.stopReqs)
	require.Equal(t, []string{"task-1"}, cli.deleteReqs)
}

// TestImportImage_HostValidationShortCircuits host 校验失败：InvalidArgument
// 且零平台调用（校验在任何平台操作之前）。
func TestImportImage_HostValidationShortCircuits(t *testing.T) {
	cases := []struct {
		name      string
		reference string
	}{
		{"ipv4-literal", "192.168.1.5:5000/acme/app:v1"},
		{"localhost", "localhost:5000/acme/app:v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, cli, _ := newImportTestDaemon(t)
			opts := importOpts()
			opts.Reference = tc.reference

			_, err := d.ImportImage(context.Background(), opts)
			require.Error(t, err)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Empty(t, cli.ensureReqs, "host 校验失败不得触达平台")
			require.Empty(t, cli.createReqs)
		})
	}
}

// TestImportImage_ExpectedDigestMappingHitSkipsPlatform 幂等快速路径：映射
// 已持有一致 digest → 零平台往返（无 ensure/无 create），直接确认。
func TestImportImage_ExpectedDigestMappingHitSkipsPlatform(t *testing.T) {
	d, cli, refs := newImportTestDaemon(t)
	require.NoError(t, refs.SaveImageRef(context.Background(),
		"torchwood-funcs/func-fn1-dep1", "ghcr.io/acme/greet@sha256:abc123"))
	opts := importOpts()
	opts.ExpectedDigest = "sha256:abc123"

	digest, err := d.ImportImage(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, "sha256:abc123", digest)
	require.Empty(t, cli.ensureReqs, "映射命中不得触达平台")
	require.Empty(t, cli.createReqs)
}

// TestImportImage_ExpectedDigestTagDriftRejected 可证漂移：平台解析结果与
// 预期 digest 不一致 → InvalidArgument（防 tag 漂移），验证任务仍被回收。
func TestImportImage_ExpectedDigestTagDriftRejected(t *testing.T) {
	d, cli, _ := newImportTestDaemon(t)
	cli.pinnedImage = "ghcr.io/acme/greet@sha256:other"
	opts := importOpts()
	opts.ExpectedDigest = "sha256:expected"

	_, err := d.ImportImage(context.Background(), opts)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "tag drift")
	require.Equal(t, []string{"task-1"}, cli.deleteReqs, "漂移拒绝路径验证任务仍须回收")
}

// TestImportImage_NoDigestResolvedRejected 平台解析结果无 manifest digest
// （纯 tag/本地引用）：显式拒绝——source_ref 契约是 digest 钉定值。
func TestImportImage_NoDigestResolvedRejected(t *testing.T) {
	d, cli, _ := newImportTestDaemon(t)
	cli.pinnedImage = "fleetly-local/greet:tag-only"

	_, err := d.ImportImage(context.Background(), importOpts())
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "manifest digest")
}

// TestImportImage_VerifyFailureCarriesTaskStatus 契约验证失败（镜像未实现
// runner 契约）：错误含任务台账失败现场（status/error），验证任务无论成败
// 被回收。
func TestImportImage_VerifyFailureCarriesTaskStatus(t *testing.T) {
	d, cli, _ := newImportTestDaemon(t)
	cli.pinnedImage = "ghcr.io/acme/greet@sha256:abc123"
	// 探针恒失败 + 短验证窗口：确定性走失败路径（不依赖真实网络）。
	d.bootTimeout = 20 * time.Millisecond
	d.probe = &fakeHealth{fails: -1}

	_, err := d.ImportImage(context.Background(), importOpts())
	require.Error(t, err)
	require.Contains(t, err.Error(), "verification failed")
	require.Contains(t, err.Error(), "task task-1 status=running", "错误必须携带任务台账失败现场")
	require.Equal(t, []string{"task-1"}, cli.stopReqs)
	require.Equal(t, []string{"task-1"}, cli.deleteReqs)
}

// TestImportImage_PlatformErrorPropagates 平台解析失败（如镜像不可得）：
// 错误经 mapFleetlyError 分类（E_IMAGE_PULL_FAILED → FailedPrecondition +
// ImageMissingMarker），不落映射。
func TestImportImage_PlatformErrorPropagates(t *testing.T) {
	d, cli, refs := newImportTestDaemon(t)
	cli.createErr = taskDetailErr("E_IMAGE_PULL_FAILED", "task image ghcr.io/acme/greet:v1 is not available")

	_, err := d.ImportImage(context.Background(), importOpts())
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "deployment image missing")
	got, lerr := refs.LoadImageRef(context.Background(), "torchwood-funcs/func-fn1-dep1")
	require.NoError(t, lerr)
	require.Empty(t, got, "导入失败不得落映射")
}

// TestImportImage_UntrustedUsesInternalScope untrusted 函数的验证任务挂
// internal 变体 task-group 网（A1 同路约束贯通导入链）。
func TestImportImage_UntrustedUsesInternalScope(t *testing.T) {
	d, cli, _ := newImportTestDaemon(t)
	cli.pinnedImage = "ghcr.io/acme/greet@sha256:abc123"
	opts := importOpts()
	opts.EgressUntrusted = true

	_, err := d.ImportImage(context.Background(), opts)
	require.NoError(t, err)
	require.Len(t, cli.ensureReqs, 1)
	require.True(t, cli.ensureReqs[0].GetInternal())
	require.Equal(t, "qp1", cli.ensureReqs[0].GetRef())
	require.True(t, cli.createReqs[0].GetScope().GetInternal())
}

// TestImportImage_StreamingBuildNotInvolved 校验 ImportImage 不触构建面
// （镜像源免构建路径的边界）。
func TestImportImage_StreamingBuildNotInvolved(t *testing.T) {
	d, cli, _ := newImportTestDaemon(t)
	cli.pinnedImage = "ghcr.io/acme/greet@sha256:abc123"
	_, err := d.ImportImage(context.Background(), importOpts())
	require.NoError(t, err)
	require.Zero(t, cli.buildCalls, "导入路径不得触达 build API")
}

// ——tar 条目读取断言助手（BuildImage 上下文流断言用）——

// readTarEntries 读取 tar 流的条目名集合与文件内容。
func readTarEntries(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(strings.NewReader(string(raw)))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeDir {
			out[hdr.Name] = "<dir>"
			continue
		}
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		out[hdr.Name] = string(b)
	}
	return out
}
