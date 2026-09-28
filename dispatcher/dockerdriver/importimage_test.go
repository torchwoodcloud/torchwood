package dockerdriver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/dispatcher"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖 ImportImage 的 docker 编排（三期阶段三，设计 §3；54b666b 用例
// 原样适配）：fake imageClient 驱动 inspect→pull→tag→remove 调用序列与失败
// 路径的确定性验证（host 校验 / ExpectedDigest 本地命中零 pull / 本地引用
// 直导 / digest 一致性 / 原始引用删除 / 凭证 RegistryAuth 形态 / 强制契约
// 验证 spawn 失败含日志尾），不依赖真实 daemon。

// digest64 构造恒定 64 hex 字符的 sha256 digest 形态字符串（测试可读性：
// digest64("cd") = "sha256:cdcd..." 共 64 字符）。
func digest64(seed string) string {
	s := strings.ToLower(seed)
	for len(s) < 64 {
		s += s
	}
	return "sha256:" + s[:64]
}

// streamBody 是 pull 响应流的静态 body。
func streamBody(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

// fakeImageClient 是 imageClient 的可编程 fake：记录调用流水（"pull <ref>" /
// "inspect <name>" / "tag <src>-><target>" / "remove <name>"），按 key 弹出
// 预置错误（耗尽后默认成功），images 表模拟本地镜像（pull 成功按 pullAdds
// 落表，模拟远端内容进本地）。
type fakeImageClient struct {
	mu    sync.Mutex
	calls []string
	errs  map[string][]error
	// images 是本地镜像表（name → inspect 结果）；预置 = 本地已有镜像。
	images map[string]image.InspectResponse
	// pullAdds 是 pull(ref) 成功后落入本地表的 inspect 结果（模拟远端 pull）。
	pullAdds map[string]image.InspectResponse
	// pullStreams 按 ref 弹出自定义响应体（缺省空流 = pull 成功无错误）。
	pullStreams map[string]string
	// lastPullRef/lastPullAuth 断言 RegistryAuth 构造。
	lastPullRef  string
	lastPullAuth string
}

func newFakeImageClient() *fakeImageClient {
	return &fakeImageClient{
		errs:        map[string][]error{},
		images:      map[string]image.InspectResponse{},
		pullAdds:    map[string]image.InspectResponse{},
		pullStreams: map[string]string{},
	}
}

func (f *fakeImageClient) pop(kind, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := kind + "/" + key
	seq := f.errs[k]
	if len(seq) == 0 {
		return nil
	}
	err := seq[0]
	f.errs[k] = seq[1:]
	return err
}

func (f *fakeImageClient) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeImageClient) ImagePull(_ context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	f.lastPullRef = ref
	f.lastPullAuth = opts.RegistryAuth
	f.mu.Unlock()
	f.record("pull " + ref)
	if err := f.pop("pull", ref); err != nil {
		return nil, err
	}
	f.mu.Lock()
	if add, ok := f.pullAdds[ref]; ok {
		f.images[ref] = add
	}
	body := f.pullStreams[ref]
	f.mu.Unlock()
	return streamBody(body), nil
}

func (f *fakeImageClient) ImageInspect(_ context.Context, imageID string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	f.record("inspect " + imageID)
	if err := f.pop("inspect", imageID); err != nil {
		return image.InspectResponse{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ins, ok := f.images[imageID]
	if !ok {
		return image.InspectResponse{}, errdefs.ErrNotFound
	}
	return ins, nil
}

func (f *fakeImageClient) ImageTag(_ context.Context, source, target string) error {
	f.record("tag " + source + "->" + target)
	if err := f.pop("tag", source+"->"+target); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[target] = f.images[source]
	return nil
}

func (f *fakeImageClient) ImageRemove(_ context.Context, imageID string, _ image.RemoveOptions) ([]image.DeleteResponse, error) {
	f.record("remove " + imageID)
	if err := f.pop("remove", imageID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, imageID)
	return nil, nil
}

// fakeImportDaemon 是 dispatcher.Daemon 的验证 spawn 面 fake：记录 spawn/
// stop/remove 调用（固定返回 cid-1，探针恒过 = 验证必成功），EnsureProjectNetwork
// 走真实网络名解析（egress 选网断言）。镜像原语路径不会被调用（importImage
// 只消费 imageClient 与 Daemon 的验证面）。
type fakeImportDaemon struct {
	mu           sync.Mutex
	spawnCount   int
	lastSpawn    dispatcher.SpawnOptions
	lastNetwork  string
	networkFlags map[string]bool
	stopped      []string
	removed      []string
	logs         map[string]string
}

func newFakeImportDaemon() *fakeImportDaemon {
	return &fakeImportDaemon{
		networkFlags: map[string]bool{},
		logs:         map[string]string{},
	}
}

func (d *fakeImportDaemon) EnsureProjectNetwork(_ context.Context, projectID string, untrusted bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.networkFlags[projectID] = untrusted
	if untrusted {
		name, err := infrafunctions.ResolveInternalNetworkName(testImportCfg(), projectID)
		if err != nil {
			return "", err
		}
		d.lastNetwork = name
		return name, nil
	}
	name, err := infrafunctions.ResolveNetworkName(testImportCfg(), projectID)
	if err != nil {
		return "", err
	}
	d.lastNetwork = name
	return name, nil
}

func (d *fakeImportDaemon) SpawnInstance(_ context.Context, opts dispatcher.SpawnOptions) (dispatcher.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.spawnCount++
	d.lastSpawn = opts
	return dispatcher.Instance{ContainerID: "cid-1", IP: "192.0.2.10"}, nil
}

func (d *fakeImportDaemon) InspectInstance(_ context.Context, _ string) (bool, string, error) {
	return true, "192.0.2.10", nil
}

func (d *fakeImportDaemon) StopInstance(_ context.Context, containerID string, _ time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = append(d.stopped, containerID)
	return nil
}

func (d *fakeImportDaemon) RemoveInstance(_ context.Context, containerID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removed = append(d.removed, containerID)
	return nil
}

func (d *fakeImportDaemon) InstanceLogsTail(_ context.Context, containerID string, _ int64) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.logs[containerID], nil
}

func (d *fakeImportDaemon) BuildImage(context.Context, dispatcher.BuildImageOptions) error {
	return errors.New("unexpected BuildImage call in import test")
}

func (d *fakeImportDaemon) RemoveImage(context.Context, string, string) error {
	return errors.New("unexpected RemoveImage call in import test")
}

func (d *fakeImportDaemon) ImportImage(context.Context, dispatcher.ImportImageOptions) (string, error) {
	return "", errors.New("unexpected ImportImage call in import test")
}

// fakeImportProbe 是验证探针的可编程 fake：前 fails 次 Health 返回错误
// （fails < 0 = 永远失败），之后成功。
type fakeImportProbe struct{ fails int }

func (p *fakeImportProbe) Health(_ context.Context, _ string) error {
	if p.fails != 0 {
		return errors.New("connection refused")
	}
	return nil
}

// testImportCfg 是导入用例的空 registry 配置（缺省 torchwood-funcs）。
func testImportCfg() *config.AppConfig {
	return &config.AppConfig{Functions: &config.Functions{
		Docker: &config.Functions_Docker{},
	}}
}

// importTestDaemon 组装 importImage 的测试夹具。
func importTestDaemon() (*fakeImportDaemon, *fakeImageClient, *config.AppConfig) {
	return newFakeImportDaemon(), newFakeImageClient(), testImportCfg()
}

func importOpts() dispatcher.ImportImageOptions {
	return dispatcher.ImportImageOptions{
		ProjectID:    "p1",
		FunctionID:   "fn1",
		DeploymentID: "dep1",
		Reference:    "ghcr.io/acme/greet:v1",
		Env:          map[string]string{"GREETING": "hi"},
	}
}

// importTarget 是空 registry 配置下的平台镜像名（与 ImageName 同式）。
const importTarget = "torchwood-funcs/func-fn1-dep1"

// TestImportImage_HappyPathSequence 首次导入主链路（引用本地不存在）：inspect
// 引用（本地直导判定，NotFound）→ pull（匿名 RegistryAuth）→ inspect 引用 →
// digest = RepoDigests[0] 的 @sha256 部分 → tag 进平台命名 → 删原始引用标签
// → 强制契约验证 spawn（验证镜像 = 平台镜像名）→ 返回 digest。
func TestImportImage_HappyPathSequence(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	digest := digest64("cd")
	imgs.pullAdds["ghcr.io/acme/greet:v1"] = image.InspectResponse{
		ID:          "sha256:imgid",
		RepoDigests: []string{"ghcr.io/acme/greet@" + digest},
	}

	got, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, importOpts(), 50*time.Millisecond, 1000)
	require.NoError(t, err)
	require.Equal(t, digest, got, "digest = RepoDigests[0] 的 @sha256 部分")

	require.Equal(t, []string{
		"inspect ghcr.io/acme/greet:v1",
		"pull ghcr.io/acme/greet:v1",
		"inspect ghcr.io/acme/greet:v1",
		"tag ghcr.io/acme/greet:v1->" + importTarget,
		"remove ghcr.io/acme/greet:v1",
	}, imgs.calls, "调用序列必须为 inspect→pull→inspect→tag→remove")

	// 凭证空 = 匿名 pull（RegistryAuth 空串）。
	require.Equal(t, "", imgs.lastPullAuth)

	// 强制契约验证 spawn：验证镜像 = 平台镜像名，池外实例用完即删。
	require.Equal(t, importTarget, d.lastSpawn.Image)
	require.Contains(t, d.lastSpawn.Env, "GREETING=hi", "验证 spawn 携带函数 variables")
	require.Equal(t, []string{"cid-1"}, d.stopped)
	require.Equal(t, []string{"cid-1"}, d.removed)
}

// TestImportImage_LocalReferenceSkipsPull 本地引用直导（本地构建/本地 tag
// 场景，任务口径「reference 就是本地 tag」）：引用本地已存在 → 跳过 pull
// （零网络操作），digest 取本地内容，照常 retag/删原始引用/契约验证。
func TestImportImage_LocalReferenceSkipsPull(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	digest := digest64("cd")
	imgs.images["ghcr.io/acme/greet:v1"] = image.InspectResponse{
		ID:          "sha256:imgid",
		RepoDigests: []string{"ghcr.io/acme/greet@" + digest},
	}

	got, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, importOpts(), 50*time.Millisecond, 1000)
	require.NoError(t, err)
	require.Equal(t, digest, got)
	require.Equal(t, []string{
		"inspect ghcr.io/acme/greet:v1",
		"tag ghcr.io/acme/greet:v1->" + importTarget,
		"remove ghcr.io/acme/greet:v1",
	}, imgs.calls, "本地直导跳过 pull")
	require.Equal(t, "", imgs.lastPullRef, "本地直导不得发起 pull")
}

// TestImportImage_RegistryAuthShape 凭证构造断言：username/token → base64
// JSON {"username","password","serveraddress":""}（docker RegistryAuth 惯例
// 形态）。
func TestImportImage_RegistryAuthShape(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	imgs.pullAdds["ghcr.io/acme/greet:v1"] = image.InspectResponse{
		RepoDigests: []string{"ghcr.io/acme/greet@" + digest64("cd")},
	}
	opts := importOpts()
	opts.RegistryUsername = "user"
	opts.RegistryToken = "tok"

	_, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, opts, 50*time.Millisecond, 1000)
	require.NoError(t, err)

	raw, err := base64.StdEncoding.DecodeString(imgs.lastPullAuth)
	require.NoError(t, err, "RegistryAuth 必须是 base64")
	var auth map[string]string
	require.NoError(t, json.Unmarshal(raw, &auth))
	require.Equal(t, map[string]string{"username": "user", "password": "tok", "serveraddress": ""}, auth)
}

// TestImportImage_HostValidationShortCircuits host 校验失败：InvalidArgument
// 且零 docker 调用（校验在任何 docker 操作之前，本地直导路径同样受约束）。
func TestImportImage_HostValidationShortCircuits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		reference string
	}{
		{"ipv4-literal", "192.168.1.5:5000/acme/app:v1"},
		{"localhost", "localhost:5000/acme/app:v1"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, imgs, cfg := importTestDaemon()
			opts := importOpts()
			opts.Reference = tc.reference

			_, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, opts, 50*time.Millisecond, 1000)
			require.Error(t, err)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Empty(t, imgs.calls, "host 校验失败不得发起任何 docker 调用")
			require.Zero(t, d.spawnCount, "host 校验失败不得 spawn 验证实例")
		})
	}
}

// TestImportImage_ExpectedDigestLocalHitSkipsPull 幂等补拉（worker 补构建 /
// ready 复检）严格命中：ExpectedDigest 在本地平台镜像 RepoDigests 命中 →
// 仅 inspect 平台镜像、零 pull、零 retag（平台镜像已在）、直接契约验证并
// 返回预期 digest。
func TestImportImage_ExpectedDigestLocalHitSkipsPull(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	digest := digest64("cd")
	imgs.images[importTarget] = image.InspectResponse{
		ID:          "sha256:imgid",
		RepoDigests: []string{"ghcr.io/acme/greet@" + digest},
	}
	opts := importOpts()
	opts.ExpectedDigest = digest

	got, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, opts, 50*time.Millisecond, 1000)
	require.NoError(t, err)
	require.Equal(t, digest, got)
	require.Equal(t, []string{"inspect " + importTarget}, imgs.calls, "本地命中零 pull 零 retag")
	require.Equal(t, importTarget, d.lastSpawn.Image, "本地命中路径同样强制契约验证")
}

// TestImportImage_ExpectedDigestSteadyStateHit 幂等稳态（首次导入自身留下的
// 状态）：原始引用删除时 registry manifest digest 关联随之摘除（RepoDigests
// 空）且 Image ID ≠ manifest digest——本地内容不可证伪；平台命名 tag 存在
// 即视为命中（零 pull 承诺），不依赖可达 registry。
func TestImportImage_ExpectedDigestSteadyStateHit(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	imgs.images[importTarget] = image.InspectResponse{ID: digest64("config-digest")}
	opts := importOpts()
	opts.ExpectedDigest = digest64("manifest-digest")

	got, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, opts, 50*time.Millisecond, 1000)
	require.NoError(t, err)
	require.Equal(t, digest64("manifest-digest"), got)
	require.Equal(t, []string{"inspect " + importTarget}, imgs.calls, "稳态命中零 pull")
}

// TestImportImage_ExpectedDigestProvableDriftRepulls 可证漂移：本地平台镜像
// RepoDigests 非空且全不命中预期 digest → 不视为命中 → 重拉对账，重拉结果
// 与预期不一致 → InvalidArgument（防 tag 漂移），不 retag 不验证。
func TestImportImage_ExpectedDigestProvableDriftRepulls(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	imgs.images[importTarget] = image.InspectResponse{
		ID:          "sha256:other-config",
		RepoDigests: []string{"ghcr.io/acme/greet@" + digest64("stale")},
	}
	imgs.pullAdds["ghcr.io/acme/greet:v1"] = image.InspectResponse{
		RepoDigests: []string{"ghcr.io/acme/greet@" + digest64("other")},
	}
	opts := importOpts()
	opts.ExpectedDigest = digest64("expected")

	_, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, opts, 50*time.Millisecond, 1000)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "tag drift")
	require.Equal(t, []string{
		"inspect " + importTarget,
		"inspect ghcr.io/acme/greet:v1",
		"pull ghcr.io/acme/greet:v1",
		"inspect ghcr.io/acme/greet:v1",
	}, imgs.calls, "可证漂移重拉对账，digest 不一致即拒绝")
	require.Zero(t, d.spawnCount, "digest 不一致不得进入契约验证")
}

// TestImportImage_ReferenceDigestTagDrift 引用自带 @sha256 与实际拉到内容
// 不一致：InvalidArgument（设计 §3 digest 钉死防 tag 漂移），不 retag。
func TestImportImage_ReferenceDigestTagDrift(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	imgs.pullAdds["ghcr.io/acme/greet@sha256:aaa"] = image.InspectResponse{
		RepoDigests: []string{"ghcr.io/acme/greet@" + digest64("zzz")},
	}
	opts := importOpts()
	opts.Reference = "ghcr.io/acme/greet@sha256:aaa"

	_, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, opts, 50*time.Millisecond, 1000)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "digest")
	require.NotContains(t, strings.Join(imgs.calls, ";"), "tag ", "digest 不一致不得 retag")
}

// TestImportImage_PullStreamError pull 流内错误（registry 认证失败/引用不
// 存在）：InvalidArgument 且错误消息透传（第一现场），不 retag。
func TestImportImage_PullStreamError(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	imgs.pullStreams["ghcr.io/acme/greet:v1"] = `{"errorDetail":{"message":"pull access denied"},"error":"pull access denied for acme/greet"}`

	_, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, importOpts(), 50*time.Millisecond, 1000)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "pull access denied")
	require.Equal(t, []string{
		"inspect ghcr.io/acme/greet:v1",
		"pull ghcr.io/acme/greet:v1",
	}, imgs.calls)
}

// TestImportImage_VerifyFailureCarriesLogTail 契约验证失败（BYO 镜像未实现
// runner 契约）：错误含容器日志尾部（第一现场），验证实例无论成败被回收。
func TestImportImage_VerifyFailureCarriesLogTail(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	imgs.pullAdds["ghcr.io/acme/greet:v1"] = image.InspectResponse{
		RepoDigests: []string{"ghcr.io/acme/greet@" + digest64("cd")},
	}
	d.logs["cid-1"] = "exec /tw-app: no such file or directory"

	_, err := importImage(context.Background(), d, imgs, &fakeImportProbe{fails: -1}, cfg, importOpts(), 50*time.Millisecond, 1000)
	require.Error(t, err)
	require.Contains(t, err.Error(), "verification failed")
	require.Contains(t, err.Error(), "no such file or directory", "错误必须携带容器日志尾部")
	require.Equal(t, []string{"cid-1"}, d.stopped)
	require.Equal(t, []string{"cid-1"}, d.removed)
}

// TestImportImage_ReferenceEqualsTargetSkipsRemove 本地 tag 直导场景
// （reference 就是平台镜像名）：tag 幂等无害，删除跳过（防自删）。
func TestImportImage_ReferenceEqualsTargetSkipsRemove(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	imgs.pullAdds[importTarget] = image.InspectResponse{
		RepoDigests: []string{"docker.io/library/greet@" + digest64("cd")},
	}
	opts := importOpts()
	opts.Reference = importTarget

	got, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, opts, 50*time.Millisecond, 1000)
	require.NoError(t, err)
	require.Equal(t, digest64("cd"), got)
	require.Equal(t, []string{
		"inspect " + importTarget,
		"pull " + importTarget,
		"inspect " + importTarget,
		"tag " + importTarget + "->" + importTarget,
	}, imgs.calls, "reference == target 时跳过删除")
}

// TestImportImage_EgressNetworkSelection untrusted 函数的验证实例挂 internal
// 变体网络（与执行同路，A1 同款约束贯通导入链）。
func TestImportImage_EgressNetworkSelection(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	imgs.pullAdds["ghcr.io/acme/greet:v1"] = image.InspectResponse{
		RepoDigests: []string{"ghcr.io/acme/greet@" + digest64("cd")},
	}
	opts := importOpts()
	opts.EgressUntrusted = true

	_, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, opts, 50*time.Millisecond, 1000)
	require.NoError(t, err)
	require.Equal(t, true, d.networkFlags["p1"])
	require.Equal(t, "tw-func-p1-int", d.lastNetwork)
	require.Equal(t, "tw-func-p1-int", d.lastSpawn.Network)
}

// TestImportImage_ReferenceInspectErrorPropagates 引用 inspect 非 NotFound
// 错误原样上抛（daemon 故障 fail-fast，不静默转 pull）。
func TestImportImage_ReferenceInspectErrorPropagates(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	imgs.errs["inspect/ghcr.io/acme/greet:v1"] = []error{context.DeadlineExceeded}

	_, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, importOpts(), 50*time.Millisecond, 1000)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestImportImage_LocalInspectErrorPropagates ExpectedDigest 本地 inspect 非
// NotFound 错误原样上抛（fail-fast：daemon 不可达时后续操作也必败）。
func TestImportImage_LocalInspectErrorPropagates(t *testing.T) {
	t.Parallel()
	d, imgs, cfg := importTestDaemon()
	imgs.errs["inspect/"+importTarget] = []error{context.DeadlineExceeded}
	opts := importOpts()
	opts.ExpectedDigest = digest64("cd")

	_, err := importImage(context.Background(), d, imgs, &fakeImportProbe{}, cfg, opts, 50*time.Millisecond, 1000)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
