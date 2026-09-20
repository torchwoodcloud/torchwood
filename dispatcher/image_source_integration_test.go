package dispatcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
)

// —— 镜像源 BYO docker 集成（三期阶段 4/4 收尾，设计
// docs/design/functions-runtimes-and-sources.md §3）——
//
// 门控与既有 IP 路由型 e2e 同一口径（requireIPRoutingHost + daemon 探测，
// daemon_integration_test.go 同款）：验证 spawn 健康探针与池执行都依赖测试
// 进程 → 容器 bridge IP 的 Linux 直连路由；真实取证方式 = 挂 docker.sock 的
// golang:1.26-alpine 容器内跑 go test（Linux 宿主语义）。
//
// fixture 途径（任务口径 b，不依赖网络拉基础镜像）：BuildImage 产物即契约
// 镜像（平台模板 = runner 协议参考实现，docker/functions-runtime-node 同源），
// docker tag 成带域名引用名后经 daemon.ImportImage 走「本地引用直导」路径
//（inspect 本地命中 → 零 pull → digest 钉死回落 Image ID → retag 进平台命名
// → 删原始引用 → 强制契约验证 spawn）。registry host 准入（imageref.go）与
// 调用序列/失败路径的确定性验证由单元测试覆盖（imageref_test.go /
// daemon_import_test.go），本文件是真实 daemon 形态对照。

// imageE2EIndexJS 是契约镜像的函数入口（封套断言锚点 via=image）。
const imageE2EIndexJS = "module.exports.main = (data) => ({ got: data.n, via: 'image' });\n"

// newImageE2EPool 组装镜像源 e2e 的池（预算与既有 IP 路由型 e2e 同款：
// CI 慢环境冷启动可超 30s，见 TestIntegration_DispatcherBuildSpawnDispatch 注释）。
func newImageE2EPool(d Daemon) *PoolManager {
	return NewPoolManager(d, newFakeRegistry(), PoolConfig{
		BootTimeout:      90 * time.Second,
		QueueHeadTimeout: 90 * time.Second,
	})
}

// cleanupRawReference 挂带域名原始引用的兜底清理（正常流程被 ImportImage
// 摘除；断言失败路径可能残留本地标签，容忍式删除）。
func cleanupRawReference(t *testing.T, cli *client.Client, ref string) {
	t.Helper()
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_, _ = cli.ImageRemove(rmCtx, ref, image.RemoveOptions{Force: true})
	})
}

// TestIntegration_ImageSourceFullChainE2E 本期旗舰用例：镜像源全链——现制
// 契约镜像（BuildImage 产物）→ docker tag 带域名引用 → ImportImage 本地直导
// （host 准入放行 / digest 钉死 / retag 进平台命名 / 原始引用删除 / 强制契约
// 验证 spawn）→ 池 spawn 执行 → 断言封套。附带 host 准入真实形态子断言
// （IP 字面量拒收先于一切 docker 操作，零副作用）。
func TestIntegration_ImageSourceFullChainE2E(t *testing.T) {
	requireIPRoutingHost(t)
	cli := requireDockerClient(t)
	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)
	pool := newImageE2EPool(d)

	srcFn, srcDep := "fnimgsrc", fmt.Sprintf("dep%d", time.Now().UnixNano())
	impFn, impDep := "fnimgimp", fmt.Sprintf("dep%d", time.Now().UnixNano()+1)
	platformRef := infrafunctions.ImageName(cfg, impFn, impDep)
	cleanupImage(t, d, srcFn, srcDep)
	cleanupImage(t, d, impFn, impDep)
	cleanupPool(t, pool, "imgeit", impFn)
	rawRef := "smoke.example.com/greet:v1"
	cleanupRawReference(t, cli, rawRef)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// —— 现制契约镜像（BuildImage；验证门由 ImportImage 的强制 spawn 承担，
	// 构建期不重复验证）——
	zip := makeEntryZip(t, "index.js", imageE2EIndexJS)
	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "imgeit", FunctionID: srcFn, DeploymentID: srcDep,
		Zip: zip, Runtime: "node-18.0",
	}), "契约镜像现制（BuildImage 产物即 runner 协议实现）必须成功")

	// —— host 准入真实形态：IP 字面量 registry host 拒收（名称级校验先于
	// 一切 docker 操作——本调用不产生任何镜像/容器残留，零副作用）——
	_, err := d.ImportImage(ctx, ImportImageOptions{
		ProjectID: "imgeit", FunctionID: impFn, DeploymentID: impDep,
		Reference: "127.0.0.1:5000/acme/greet:v1",
	})
	require.Error(t, err, "IP 字面量 registry host 必须被拒收（imageref 准入基线）")
	require.Contains(t, err.Error(), "IP literal")

	// —— docker tag 带域名引用 → 本地引用直导导入 ——
	require.NoError(t, cli.ImageTag(ctx, infrafunctions.ImageName(cfg, srcFn, srcDep), rawRef))
	digest, err := d.ImportImage(ctx, ImportImageOptions{
		ProjectID: "imgeit", FunctionID: impFn, DeploymentID: impDep,
		Reference: rawRef,
	})
	require.NoError(t, err, "契约镜像直导必须通过强制契约验证 spawn")
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest, "钉死 digest 必须是 sha256 形态（落 source_ref）")

	// retag 后平台镜像存在；本地直导不经 registry → 无 RepoDigests，钉死
	// digest 回落 Image ID（内容寻址，daemon.go pinnedDigest 声明的回落口径）。
	impIns, err := cli.ImageInspect(ctx, platformRef)
	require.NoError(t, err, "平台镜像必须已存在（retag 进平台命名，池 spawn/RemoveImage 零改动）")
	require.Equal(t, impIns.ID, digest, "本地直导的钉死 digest = Image ID（内容寻址回落）")

	// 原始引用已删除（防本地 daemon 残留标签）。
	_, err = cli.ImageInspect(ctx, rawRef)
	require.True(t, errdefs.IsNotFound(err), "原始引用必须在 retag 后删除，err=%v", err)

	// 验证实例已回收（此刻池尚未 spawn，凡挂平台镜像的容器只能是验证残留）。
	requireNoLeftoverVerifyContainers(t, cli, platformRef)

	// —— 池 spawn 执行（复用既有执行断言路径：冷启动 → 健康握手 → 分发）——
	resp, err := pool.Dispatch(ctx, ExecuteRequest{ // #nosec G101 -- 测试夹具伪凭证
		Image: platformRef, ProjectID: "imgeit", FunctionID: impFn, DeploymentID: impDep,
		// runtime=image：BYO 镜像函数的专用 runtime ID（ListRuntimes 增项，
		// 设计 §3）；池分发只消费 Image/Spec，runtime 值原样透传。
		Runtime: "image", Spec: "shared-1x", TimeoutSeconds: 30,
		Data:           `{"n":7}`,
		ExecutionToken: "twx_it-token",
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.Contains(t, resp.Response, `"got":7`)
	require.Contains(t, resp.Response, `"via":"image"`, "镜像源函数的执行封套与构建源完全一致（retag 后全链路零改动）")

	pool.DrainForDeployment(ctx, "imgeit", impFn, "none", 5*time.Second)
	waitPoolDrained(t, pool, ctx)
}

// buildNonContractImage 现制一枚「能挂着但非契约」的镜像：FROM node:18-alpine
// + 空转 CMD——容器持续运行但 :18080 无 runner 监听，健康探针必到点失败。
// 不用裸基础镜像（node:18-alpine 的 REPL CMD 在 stdin 关闭下秒退，会在
// SpawnInstance 的 inspect 上竞态失败、走不到探针路径）；基础镜像本地命中
// 零 pull（node 构建链前置依赖），不触网。
func buildNonContractImage(t *testing.T, ctx context.Context, cli *client.Client, ref string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(
		"FROM node:18-alpine\n"+
			"CMD [\"node\",\"-e\",\"setInterval(function(){},1000)\"]\n"), 0o600))
	tarCtx, err := tarDir(dir)
	require.NoError(t, err)
	resp, err := cli.ImageBuild(ctx, tarCtx, build.ImageBuildOptions{
		Tags: []string{ref}, Dockerfile: "Dockerfile", Remove: true,
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, buildErr := infrafunctions.ReadBuildOutput(resp.Body)
	require.NoError(t, buildErr, "非契约镜像现制必须成功（基础镜像本地命中）")
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer rmCancel()
		_, _ = cli.ImageRemove(rmCtx, ref, image.RemoveOptions{Force: true})
	})
}

// TestIntegration_ImageSourceNonContractRejected 非契约镜像拒收：挂着但无
// runner 的镜像经本地 tag 直导导入 → 强制契约验证 spawn（config verify_build
// 关不掉——ImportImage 无开关）到点判失败，错误含 verification failed；
// 验证实例已回收，平台镜像（已 retag）由 RemoveImage 幂等清理兜底，原始
// 引用已摘除。
func TestIntegration_ImageSourceNonContractRejected(t *testing.T) {
	requireIPRoutingHost(t)
	cli := requireDockerClient(t)
	// boot 预算 3s：失败判定快速收敛（生产默认 60s，语义相同，
	// GoVerifyFailureCapturesLogTail 同款手法）。
	cfg := testDispatcherConfig(t)
	cfg.Functions.Dispatcher.BootTimeout = "3s"
	d := NewDockerDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	fnID, depID := "fnbadimg", fmt.Sprintf("dep%d", time.Now().UnixNano())
	platformRef := infrafunctions.ImageName(cfg, fnID, depID)
	cleanupImage(t, d, fnID, depID)
	rawRef := "smoke.example.com/noncontract:v1"
	cleanupRawReference(t, cli, rawRef)

	buildNonContractImage(t, ctx, cli, rawRef)

	_, err := d.ImportImage(ctx, ImportImageOptions{
		ProjectID: "imgeit", FunctionID: fnID, DeploymentID: depID,
		Reference: rawRef,
	})
	require.Error(t, err, "非契约镜像必须在强制验证 spawn 被拒收（deployment 将标 failed）")
	msg := err.Error()
	require.Contains(t, msg, "verification failed")

	// 验证失败发生在 retag 之后：平台镜像已存在（残留由 RemoveImage 幂等
	// 兜底——调用方失败清理分流，deployments_image.go），原始引用已摘除。
	_, err = cli.ImageInspect(ctx, platformRef)
	require.NoError(t, err, "验证失败前平台镜像已 retag 进平台命名")
	_, err = cli.ImageInspect(ctx, rawRef)
	require.True(t, errdefs.IsNotFound(err), "原始引用必须在验证 spawn 之前已删除")

	requireNoLeftoverVerifyContainers(t, cli, platformRef)
}

// TestIntegration_ImageSourceDigestPinIdempotentResummon digest 钉死与幂等
// 补拉（设计 §3「本地镜像在则零操作；镜像被外部删除的边缘场景重 pull」，
// worker 补构建 image 分流的 M7 补拉语义）：
//  1. 首次导入钉死 digest1；
//  2. 幂等复检（ExpectedDigest 命中）→ 本地平台镜像持有 → **零 pull**——
//     Reference 用不可解析域名（.invalid 保留 TLD 保证 NXDOMAIN）：实现若
//     违反零 pull 承诺去拉该引用，DNS 失败必炸，成功即零 pull 的真实实证；
//  3. 本地 miss（新部署无平台镜像、无 ExpectedDigest）→ 真实重拉触网 →
//     不可达 registry 显式报错（错误含 docker pull）。
func TestIntegration_ImageSourceDigestPinIdempotentResummon(t *testing.T) {
	requireIPRoutingHost(t)
	cli := requireDockerClient(t)
	cfg := testDispatcherConfig(t)
	d := NewDockerDaemon(cfg)

	srcFn, srcDep := "fnimgpin", fmt.Sprintf("dep%d", time.Now().UnixNano())
	impFn, impDep := "fnimgpin2", fmt.Sprintf("dep%d", time.Now().UnixNano()+1)
	missFn, missDep := "fnimgmiss", fmt.Sprintf("dep%d", time.Now().UnixNano()+2)
	cleanupImage(t, d, srcFn, srcDep)
	cleanupImage(t, d, impFn, impDep)
	cleanupImage(t, d, missFn, missDep)
	rawRef := "smoke.example.com/greet:v2"
	cleanupRawReference(t, cli, rawRef)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// —— 现制契约镜像 + tag + 首次导入（digest 钉死）——
	zip := makeEntryZip(t, "index.js", imageE2EIndexJS)
	require.NoError(t, d.BuildImage(ctx, BuildImageOptions{
		ProjectID: "imgeit", FunctionID: srcFn, DeploymentID: srcDep,
		Zip: zip, Runtime: "node-18.0",
	}))
	require.NoError(t, cli.ImageTag(ctx, infrafunctions.ImageName(cfg, srcFn, srcDep), rawRef))
	digest1, err := d.ImportImage(ctx, ImportImageOptions{
		ProjectID: "imgeit", FunctionID: impFn, DeploymentID: impDep,
		Reference: rawRef,
	})
	require.NoError(t, err, "首次导入必须成功并钉死 digest")

	// —— 幂等复检：ExpectedDigest 命中 → 零 pull（不可达引用从未被触碰）——
	// 与 worker 补构建 / ready 门禁复检同构：同部署（impFn/impDep）再导入。
	resummonRef := "nonexistent.invalid/acme/greet:v2"
	digest2, err := d.ImportImage(ctx, ImportImageOptions{
		ProjectID: "imgeit", FunctionID: impFn, DeploymentID: impDep,
		Reference:      resummonRef,
		ExpectedDigest: digest1,
	})
	require.NoError(t, err, "ExpectedDigest 命中本地平台镜像时必须零 pull（不可达 registry 不阻断幂等复检）")
	require.Equal(t, digest1, digest2, "幂等复检返回钉死 digest 原值")

	// —— 本地 miss → 真实重拉：不可达 registry 显式报错（不静默装好）——
	_, err = d.ImportImage(ctx, ImportImageOptions{
		ProjectID: "imgeit", FunctionID: missFn, DeploymentID: missDep,
		Reference: resummonRef,
	})
	require.Error(t, err, "本地 miss 的补拉必须真实触网（私有无凭证/不可达失败显式报错，声明边界）")
	require.Contains(t, err.Error(), "docker pull")
}
