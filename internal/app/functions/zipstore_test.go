package functions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 本文件覆盖部署代码包持久层（domainfunctions.ZipStore，zip 持久桶）的
// app 层接线：写路径双写（writeZip 后落桶、失败整体回滚）、清理路径同步
// 删（removeCodePackage）、以及镜像缺失重建的盘缺失自愈（持久层拉回 +
// 行内锚复核 + git 源回退 packer 重物化）。

// zipCode 是合法 zip 魔数开头的测试代码包。
var zipCode = []byte("PK\x03\x04fake-code-package")

// sha256Hex 是持久层锚的计算口径（与 deployments.go 写入侧同源）。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// zipStoreUC 组装带 fake 持久层的用例聚合（fn 预置到 repo）。
func zipStoreUC(exec *mockExecutor, store *fakeZipStore) (*Functions, *mockRepo) {
	repo := newMockRepo()
	_ = repo.CreateFunction(context.Background(), &domainfunctions.Function{
		ID: "fn_1", ProjectID: "p1", Runtime: "node-24.0", TimeoutSeconds: 10, Enabled: true,
	})
	uc := NewFunctions(&config.AppConfig{}, exec, repo, newMockQueue())
	uc.zipStore = store
	return uc, repo
}

// TestCreateDeployment_ZipSourcePersistsCodePackage zip 源成功链路双写：
// 本地 zipPath + 持久层 Put（同一字节），行内 ContextSHA256 = 上传字节
// sha256（持久层拉回复核的锚）。
func TestCreateDeployment_ZipSourcePersistsCodePackage(t *testing.T) {
	store := &fakeZipStore{}
	uc, _ := zipStoreUC(newMockExecutor(nil, nil), store)

	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1", Code: zipCode,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = removeZip("p1", "fn_1", dep.ID) })

	require.FileExists(t, zipPath("p1", "fn_1", dep.ID), "本地第一层照旧")
	require.Len(t, store.puts, 1, "持久副本随写路径落桶")
	require.Equal(t, "p1", store.puts[0].projectID)
	require.Equal(t, "fn_1", store.puts[0].functionID)
	require.Equal(t, dep.ID, store.puts[0].deploymentID)
	require.Equal(t, zipCode, store.puts[0].zip)
	require.Equal(t, sha256Hex(zipCode), dep.ContextSHA256, "zip 源补填可复现性锚")
}

// TestCreateDeployment_ZipPutFailureRollsBack 落桶失败整体回滚：持久层是
// 重建自愈的源，best-effort 会静默退化回「盘缺失即不可自愈」——请求失败，
// 行与本地 zip 均不残留。
func TestCreateDeployment_ZipPutFailureRollsBack(t *testing.T) {
	store := &fakeZipStore{putErr: errors.New("s3 unavailable")}
	uc, repo := zipStoreUC(newMockExecutor(nil, nil), store)

	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1", Code: zipCode,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "persist code package")
	require.Nil(t, dep)
	require.Empty(t, repo.deployments, "deployment 行回滚")
	// zipRoot 是跨用例共享目录（历史残留子目录常见），只断言本部署路径不存在。
	entries, _ := os.ReadDir(zipRoot())
	for _, e := range entries {
		require.NotEqual(t, "fn_1", e.Name(), "本地 zip 不残留（fn_1 目录未清）")
	}
}

// TestCreateDeployment_ZipBuildFailureRemovesStoredCopy D13 对齐：zip 源
// 构建失败删本地 zip 的语义扩展到持久层（两份同生同灭）。
func TestCreateDeployment_ZipBuildFailureRemovesStoredCopy(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	exec.buildErr = errors.New("build boom")
	store := &fakeZipStore{}
	uc, _ := zipStoreUC(exec, store)

	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1", Code: zipCode,
	})
	require.NoError(t, err, "构建失败收敛为 failed 状态而非请求失败")
	require.Equal(t, domainfunctions.DeploymentStatusFailed, dep.Status)
	require.Len(t, store.puts, 1)
	require.Equal(t, []string{dep.ID}, store.removes, "持久副本随 D13 清理")
	_, statErr := os.Stat(zipPath("p1", "fn_1", dep.ID))
	require.True(t, os.IsNotExist(statErr), "本地 zip 同步清理")
}

// TestCreateDeployment_GitPutFailureNoRowNoZip git 源落桶失败与写盘失败
// 同类：请求级失败不留残骸（无行、无本地 zip）。
func TestCreateDeployment_GitPutFailureNoRowNoZip(t *testing.T) {
	store := &fakeZipStore{putErr: errors.New("s3 unavailable")}
	uc, repo := zipStoreUC(newMockExecutor(nil, nil), store)
	uc.packer = newFakePacker()

	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1", Git: gitSource(),
	})
	require.Error(t, err)
	require.Nil(t, dep)
	require.Empty(t, repo.deployments, "无行")
	// 写盘成功后落桶失败：本地 zip 须清理（按 fn_1 目录缺席断言——zipRoot
	// 跨用例共享，历史残留子目录不相关）。
	entries, _ := os.ReadDir(zipRoot())
	for _, e := range entries {
		require.NotEqual(t, "fn_1", e.Name(), "无本地 zip 残骸（fn_1 目录未清）")
	}
}

// TestDeleteDeployment_RemovesStoredCopy 删除链路清持久层副本（幂等
// best-effort；漏删只留孤儿对象不阻塞删除）。
func TestDeleteDeployment_RemovesStoredCopy(t *testing.T) {
	store := &fakeZipStore{}
	uc, repo := zipStoreUC(newMockExecutor(nil, nil), store)
	require.NoError(t, repo.CreateDeployment(context.Background(), &domainfunctions.Deployment{
		ID: "dep_ready", FunctionID: "fn_1", ProjectID: "p1",
		Status: domainfunctions.DeploymentStatusReady,
	}))
	require.NoError(t, writeZip(zipPath("p1", "fn_1", "dep_ready"), zipCode))
	t.Cleanup(func() { _ = removeZip("p1", "fn_1", "dep_ready") })

	require.NoError(t, uc.DeleteDeployment(serverCtx(), "p1", "fn_1", "dep_ready"))
	require.Equal(t, []string{"dep_ready"}, store.removes)
}

// ——镜像缺失重建：盘缺失自愈（rebuild.go restoreDeploymentZip）——

// rebuildStoreUC 组装带 fake 持久层的 ready 部署用例聚合（无盘上 zip——
// 自愈链路从持久层拉回）。anchor 为 ContextSHA256 行内锚（空 = 存量部署
// 回退 Size 复核）。
func rebuildStoreUC(t *testing.T, exec *mockExecutor, store *fakeZipStore, sourceType, anchor string) (*Functions, *mockRepo, *domainfunctions.Function, *domainfunctions.Deployment) {
	t.Helper()
	repo := newMockRepo()
	fn := &domainfunctions.Function{ID: "fn_1", ProjectID: "p1", Runtime: "node-24.0", TimeoutSeconds: 10, Enabled: true}
	require.NoError(t, repo.CreateFunction(context.Background(), fn))
	dep := &domainfunctions.Deployment{
		ID:            "dep_1",
		FunctionID:    fn.ID,
		ProjectID:     fn.ProjectID,
		Status:        domainfunctions.DeploymentStatusReady,
		Runtime:       "node-24.0",
		SourceType:    sourceType,
		Size:          int64(len(zipCode)),
		ContextSHA256: anchor,
	}
	if sourceType == domainfunctions.DeploymentSourceGit {
		dep.SourceURL = "https://git.example.com/acme/widget.git"
		dep.SourceRef = "0123456789abcdef0123456789abcdef01234567"
		dep.SourceDir = "functions/greet"
	}
	require.NoError(t, repo.CreateDeployment(context.Background(), dep))
	_, statErr := os.Stat(zipPath(fn.ProjectID, fn.ID, dep.ID))
	require.True(t, os.IsNotExist(statErr), "前置：盘上 zip 缺失")
	t.Cleanup(func() { _ = removeZip(fn.ProjectID, fn.ID, dep.ID) })
	uc := NewFunctions(nil, exec, repo, newMockQueue())
	uc.zipStore = store
	return uc, repo, fn, dep
}

// TestRebuild_ZipSourceRestoresFromStore zip 源自愈主通路：盘缺失 → 持久层
// 拉回 → sha256 锚复核 → 落盘 → 重建。
func TestRebuild_ZipSourceRestoresFromStore(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	store := &fakeZipStore{get: zipCode}
	uc, _, fn, dep := rebuildStoreUC(t, exec, store, domainfunctions.DeploymentSourceZip, sha256Hex(zipCode))

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.NotNil(t, build, "持久层命中：拉回后可重建")
	build()
	require.Equal(t, 1, exec.builds)
	require.FileExists(t, zipPath("p1", "fn_1", "dep_1"), "拉回字节落盘（后续补构建可用）")
}

// TestRebuild_ZipSourceLegacySizeAnchor 存量 zip 部署（无 checksum 锚）按
// Size 复核放行。
func TestRebuild_ZipSourceLegacySizeAnchor(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	store := &fakeZipStore{get: zipCode}
	uc, _, fn, dep := rebuildStoreUC(t, exec, store, domainfunctions.DeploymentSourceZip, "")

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.NotNil(t, build)
	build()
	require.Equal(t, 1, exec.builds)
}

// TestRebuild_ZipSourceChecksumMismatchSkips 漂移副本（锚不符）拒绝进入
// 构建（与 git 重物化复核同口径 D9）。
func TestRebuild_ZipSourceChecksumMismatchSkips(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	drifted := []byte("PK\x03\x04drifted-copy-not-matching-anchor")
	store := &fakeZipStore{get: drifted}
	uc, _, fn, dep := rebuildStoreUC(t, exec, store, domainfunctions.DeploymentSourceZip, sha256Hex(zipCode))

	require.Nil(t, uc.prepareImageMissingRebuild(context.Background(), fn, dep))
	require.Equal(t, 0, exec.builds)
}

// TestRebuild_ZipSourceStoreMissSkips 桶内无副本（存量部署/副本被删）：
// zip 源无其他源，退回声明边界（redeploy）。
func TestRebuild_ZipSourceStoreMissSkips(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	store := &fakeZipStore{} // get 为 nil = miss
	uc, _, fn, dep := rebuildStoreUC(t, exec, store, domainfunctions.DeploymentSourceZip, sha256Hex(zipCode))

	require.Nil(t, uc.prepareImageMissingRebuild(context.Background(), fn, dep))
	require.Equal(t, 0, exec.builds)
}

// TestRebuild_ZipSourceTransientFetchErrorSkips 暂态取回失败让路：镜像仍
// 缺失，下次执行重进本链（不误判为不可自愈）。
func TestRebuild_ZipSourceTransientFetchErrorSkips(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	store := &fakeZipStore{getErr: errors.New("network blip")}
	uc, _, fn, dep := rebuildStoreUC(t, exec, store, domainfunctions.DeploymentSourceZip, sha256Hex(zipCode))

	require.Nil(t, uc.prepareImageMissingRebuild(context.Background(), fn, dep))
	require.Equal(t, 0, exec.builds)
}

// TestRebuild_GitSourcePrefersStoredCopy git 源持久层命中：省一次 packer
// 重克隆（packer 零调用），直接拉回重建。
func TestRebuild_GitSourcePrefersStoredCopy(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	store := &fakeZipStore{get: zipCode}
	uc, _, fn, dep := rebuildStoreUC(t, exec, store, domainfunctions.DeploymentSourceGit, sha256Hex(zipCode))
	packer := newFakePacker()
	uc.packer = packer

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.NotNil(t, build)
	build()
	require.Empty(t, packer.got, "持久层命中不走 packer")
	require.Equal(t, 1, exec.builds)
}

// TestRebuild_GitSourceStoreMissRematerializes git 源桶 miss 回退既有
// packer 重物化通路。
func TestRebuild_GitSourceStoreMissRematerializes(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	store := &fakeZipStore{} // miss
	packer := newFakePacker()
	uc, _, fn, dep := rebuildStoreUC(t, exec, store, domainfunctions.DeploymentSourceGit, packer.checksum)
	uc.packer = packer

	build := uc.prepareImageMissingRebuild(context.Background(), fn, dep)
	require.NotNil(t, build)
	build()
	require.Len(t, packer.got, 1, "桶 miss 回退重物化")
	require.Equal(t, 1, exec.builds)
}
