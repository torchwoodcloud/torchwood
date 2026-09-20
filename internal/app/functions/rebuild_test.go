package functions

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 本文件覆盖镜像缺失自动重建（rebuild.go，执行链自愈）：错误识别（code +
// 稳定标记双重门槛）、开关 presence 语义、在途去重、非 ready 让路、源物化
// 分流（盘上 zip / git 重物化 + ContextSHA256 复核 / image 源免 zip / zip
// 源快照丢失不可重建）、以及同步执行路径的端到端触发。

func imageMissingErr(image string) error {
	return status.Errorf(codes.FailedPrecondition,
		"%s %q on this node (rebuild required): Error response from daemon: No such image",
		domainfunctions.ImageMissingMarker, image)
}

// rebuildUC 组装带 ready 部署的用例聚合。zipSource=true 时预写盘上 zip
// （模拟物化快照健在）；git/image 源不预写（模拟环境重建后快照丢失）。
func rebuildUC(t *testing.T, exec *mockExecutor, cfg *config.AppConfig, sourceType string) (*Functions, *mockRepo, *domainfunctions.Function, *domainfunctions.Deployment) {
	t.Helper()
	repo := newMockRepo()
	fn := &domainfunctions.Function{ID: "fn_1", ProjectID: "p1", Runtime: "node-24.0", TimeoutSeconds: 10, Enabled: true}
	require.NoError(t, repo.CreateFunction(context.Background(), fn))
	dep := &domainfunctions.Deployment{
		ID:         "dep_1",
		FunctionID: fn.ID,
		ProjectID:  fn.ProjectID,
		Status:     domainfunctions.DeploymentStatusReady,
		Runtime:    "node-24.0",
		SourceType: sourceType,
	}
	if sourceType == domainfunctions.DeploymentSourceGit {
		dep.SourceURL = "https://git.example.com/acme/widget.git"
		dep.SourceRef = "0123456789abcdef0123456789abcdef01234567"
		dep.SourceDir = "functions/greet"
		dep.ContextSHA256 = newFakePacker().checksum
	}
	require.NoError(t, repo.CreateDeployment(context.Background(), dep))
	if sourceType == domainfunctions.DeploymentSourceZip {
		require.NoError(t, writeZip(zipPath(fn.ProjectID, fn.ID, dep.ID), []byte("PK\x03\x04fake-zip")))
	}
	t.Cleanup(func() { _ = removeZip(fn.ProjectID, fn.ID, dep.ID) })
	uc := NewFunctions(cfg, exec, repo, newMockQueue())
	return uc, repo, fn, dep
}

func TestIsImageMissingExecErr(t *testing.T) {
	require.True(t, isImageMissingExecErr(imageMissingErr("img")))
	require.False(t, isImageMissingExecErr(nil))
	// 同 code 缺稳定标记（dispatcher url 未配置等形态）不识别。
	require.False(t, isImageMissingExecErr(status.Error(codes.FailedPrecondition, "functions.dispatcher.url is not configured")))
	// 标记在但 code 不对（普通 spawn 失败）不识别。
	require.False(t, isImageMissingExecErr(status.Errorf(codes.ResourceExhausted, "%s: boom", domainfunctions.ImageMissingMarker)))
	require.False(t, isImageMissingExecErr(status.Error(codes.Internal, "daemon down")))
}

func TestRebuildOnMissingImage_PresenceSemantics(t *testing.T) {
	// 未配置（cfg 空 / dispatcher 段缺省）→ 默认开启。
	uc, _, _, _ := rebuildUC(t, newMockExecutor(nil, nil), &config.AppConfig{}, domainfunctions.DeploymentSourceZip)
	require.True(t, uc.rebuildOnMissingImage())

	// 显式 false → 关闭（maybe 在 prepare 之前短路）。
	off := &config.AppConfig{Functions: &config.Functions{
		Dispatcher: &config.Functions_Dispatcher{RebuildOnMissingImage: proto.Bool(false)},
	}}
	ucOff, _, fnOff, depOff := rebuildUC(t, newMockExecutor(nil, nil), off, domainfunctions.DeploymentSourceZip)
	require.False(t, ucOff.rebuildOnMissingImage())
	ucOff.maybeRebuildMissingImage(context.Background(), imageMissingErr("img"), fnOff, depOff)
	require.Equal(t, 0, ucOff.executor.(*mockExecutor).builds, "开关关闭不得触发重建")

	// 显式 true → 开启。
	on := &config.AppConfig{Functions: &config.Functions{
		Dispatcher: &config.Functions_Dispatcher{RebuildOnMissingImage: proto.Bool(true)},
	}}
	ucOn, _, _, _ := rebuildUC(t, newMockExecutor(nil, nil), on, domainfunctions.DeploymentSourceZip)
	require.True(t, ucOn.rebuildOnMissingImage())
}

func TestPrepareImageMissingRebuild_ZipOnDisk(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	exec.buildNodeID = "node-1"
	uc, repo, fn, dep := rebuildUC(t, exec, nil, domainfunctions.DeploymentSourceZip)

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.NotNil(t, build, "盘上 zip 健在：直接重建")
	build()
	require.Equal(t, 1, exec.builds)
	require.Equal(t, "dep_1", exec.specs[0].DeploymentID)
	// build_node 落在重建副本（避免与执行路径并发变异调用方指针）——断言走
	// repo 落库视图。
	cur, gerr := repo.GetDeployment(context.Background(), fn.ProjectID, fn.ID, dep.ID)
	require.NoError(t, gerr)
	require.Equal(t, "node-1", cur.BuildNode, "重建刷新 build_node（镜像新所在节点）")
	require.Equal(t, domainfunctions.DeploymentStatusReady, cur.Status)
}

func TestPrepareImageMissingRebuild_GitRematerializesViaPacker(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	uc, _, fn, dep := rebuildUC(t, exec, nil, domainfunctions.DeploymentSourceGit)
	packer := newFakePacker() // checksum 默认值 = dep.ContextSHA256（helper 对齐）
	uc.packer = packer

	_, statErr := os.Stat(zipPath(fn.ProjectID, fn.ID, dep.ID))
	require.True(t, os.IsNotExist(statErr), "前置：git 源无盘上 zip")

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.NotNil(t, build)
	build()
	// 重物化以行内源快照为输入：URL + 钉死 commit SHA（非原始 ref）+ 子目录。
	require.Len(t, packer.got, 1)
	require.Equal(t, dep.SourceURL, packer.got[0].URL)
	require.Equal(t, dep.SourceRef, packer.got[0].Ref, "重物化按钉死 SHA（可复现锚）")
	require.Equal(t, dep.SourceDir, packer.got[0].Directory)
	require.Equal(t, 1, exec.builds)
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
	_, statErr = os.Stat(zipPath(fn.ProjectID, fn.ID, dep.ID))
	require.NoError(t, statErr, "重物化后盘上 zip 就绪（后续补构建可用）")
}

func TestPrepareImageMissingRebuild_GitChecksumMismatchRejects(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	uc, _, fn, dep := rebuildUC(t, exec, nil, domainfunctions.DeploymentSourceGit)
	packer := newFakePacker()
	packer.checksum = "drifted-checksum" // 仓库历史改写形态
	uc.packer = packer

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.Nil(t, build, "checksum 复核失败拒绝以漂移源覆盖快照（D9）")
	require.Equal(t, 0, exec.builds)
}

func TestPrepareImageMissingRebuild_GitWithoutPackerSkips(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	uc, _, fn, dep := rebuildUC(t, exec, nil, domainfunctions.DeploymentSourceGit)
	require.Nil(t, uc.packer, "前置：旧构造 packer 未注入")

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.Nil(t, build)
	require.Equal(t, 0, exec.builds)
}

func TestPrepareImageMissingRebuild_ZipSourceSnapshotLostSkips(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	uc, _, fn, dep := rebuildUC(t, exec, nil, domainfunctions.DeploymentSourceZip)
	require.NoError(t, removeZip(fn.ProjectID, fn.ID, dep.ID), "前置：模拟环境重建后快照丢失")

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.Nil(t, build, "zip 源原始字节不在平台存储内，无法自动重建")
	require.Equal(t, 0, exec.builds)
}

func TestPrepareImageMissingRebuild_ImageSourceImports(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	exec.importDigest = "sha256:aa"
	uc, _, fn, dep := rebuildUC(t, exec, nil, domainfunctions.DeploymentSourceImage)
	dep.SourceURL = "ghcr.io/acme/fn:v1"
	dep.SourceRef = "sha256:bb"

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.NotNil(t, build, "image 源无需 zip：分流为幂等 ImportImage")
	build()
	require.Equal(t, 1, exec.imports)
	require.Equal(t, "sha256:bb", exec.importSpecs[0].ExpectedDigest, "预期 digest = 行内 source_ref（幂等复检）")
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
}

func TestPrepareImageMissingRebuild_NonReadyLetsPass(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	uc, _, fn, dep := rebuildUC(t, exec, nil, domainfunctions.DeploymentSourceZip)
	dep.Status = domainfunctions.DeploymentStatusBuilding

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.Nil(t, build, "非 ready = worker 补构建或在途重建已接管，让路")
	require.Equal(t, 0, exec.builds)
}

func TestPrepareImageMissingRebuild_InFlightDedupe(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	uc, _, fn, dep := rebuildUC(t, exec, nil, domainfunctions.DeploymentSourceZip)

	first := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.NotNil(t, first)
	require.Nil(t, uc.prepareImageMissingRebuild(context.Background(), fn, dep), "在途去重：并发命中只触发一次")
	require.Equal(t, 0, exec.builds)

	first()
	require.NotNil(t, uc.prepareImageMissingRebuild(context.Background(), fn, dep), "重建完成后去重键释放")
}

func TestCreateExecution_ImageMissingTriggersRebuild(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	require.NoError(t, writeZip(zipPath("p1", "fn_1", "dep_ready"), []byte("PK\x03\x04fake-zip")))
	t.Cleanup(func() { _ = removeZip("p1", "fn_1", "dep_ready") })
	exec := newMockExecutor(nil, imageMissingErr("torchwood-funcs/func-fn_1-dep_ready"))
	uc := newTestUC(exec, repo, newMockQueue())

	rec, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{
		ProjectID:  "p1",
		FunctionID: "fn_1",
		Data:       `{"a":1}`,
	})
	require.Error(t, err, "当次执行仍以原始错误失败（不等待分钟级构建）")
	require.Contains(t, status.Convert(err).Message(), domainfunctions.ImageMissingMarker)
	require.Equal(t, domainfunctions.ExecutionStatusFailed, rec.Status)

	require.Eventually(t, func() bool { return exec.builds == 1 },
		2*time.Second, 10*time.Millisecond, "执行错误路径必须触发后台重建")
	require.Eventually(t, func() bool {
		cur, gerr := repo.GetDeployment(context.Background(), "p1", "fn_1", "dep_ready")
		return gerr == nil && cur != nil && cur.Status == domainfunctions.DeploymentStatusReady
	}, 2*time.Second, 10*time.Millisecond, "重建完成后执行面恢复 ready")
}
