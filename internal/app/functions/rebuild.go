package functions

import (
	"context"
	"os"
	"strings"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 镜像缺失自动重建（执行链自愈）：local 路由模式下部署镜像只存在于其构建
// 节点，宿主镜像被清理（prune -a / 磁盘压力清理）或环境迁移会让 Postgres 的
// ready 状态与宿主 docker 的镜像存量漂移——执行 spawn 以「No such image」
// 失败且不自愈（dispatcher/routing.go 决策 5 留现场语义）。dispatcher 侧把
// 该失败识别为 FailedPrecondition + ImageMissingMarker 类型化上抛（pool 对
// 其 fail-fast 不烧队首超时），server/worker 在执行错误路径凭「code + 标记」
// 识别并触发本文件的异步重建。源物化分流：
//   - zip/git 源：盘上 zip 命中直接重建；git 源缺失时按行内源快照（URL +
//     钉死 commit SHA + 子目录）经 packer 重新物化并复核 ContextSHA256
//     （D9 可复现锚——钉死 SHA 下重物化应逐字节一致，不一致 = 仓库历史
//     改写，拒绝以漂移源覆盖快照）；
//   - image 源：无需 zip，buildDeployment 按 SourceType 分流为幂等
//     ImportImage（预期 digest = 行内 source_ref）；
//   - zip 源：原始字节不在平台任何存储内，无法自动重建——记录日志，执行
//     错误文案（rebuild required）引导 redeploy。
//
// 当次执行不等待重建（构建分钟级，同步调用方不得被连坐）：以原始错误失败，
// 重建期间后续执行以「no ready deployment」fail-fast（building 非 ready，
// selectDeployment 拒绝），完成后执行面自愈。开关 functions.dispatcher.
// rebuild_on_missing_image 未配置 = 默认开启，显式 false 关闭。

// rebuildKey 是在途重建去重键（进程内；并发执行同时命中同一缺失镜像只触发
// 一次重建）。
type rebuildKey struct {
	projectID, functionID, deploymentID string
}

// isImageMissingExecErr 判定执行错误是否为 dispatcher 的镜像缺失类型化错误
// （code + 稳定标记双重门槛——FailedPrecondition 在 dispatcher 链路另有
// 「url 未配置」等形态，缺标记不触发重建）。
func isImageMissingExecErr(execErr error) bool {
	return execErr != nil &&
		status.Code(execErr) == codes.FailedPrecondition &&
		strings.Contains(status.Convert(execErr).Message(), domainfunctions.ImageMissingMarker)
}

// maybeRebuildMissingImage 在执行错误路径识别镜像缺失并触发后台重建
// （best-effort，不改变当次执行的失败结果与返回值）。
func (f *Functions) maybeRebuildMissingImage(ctx context.Context, execErr error, fn *domainfunctions.Function, dep *domainfunctions.Deployment) {
	if !f.rebuildOnMissingImage() || !isImageMissingExecErr(execErr) {
		return
	}
	if build := f.prepareImageMissingRebuild(ctx, fn, dep); build != nil {
		go build()
	}
}

// prepareImageMissingRebuild 执行触发前置（在途去重 → 现读部署状态 → 源
// 物化），返回后台重建闭包；nil = 不触发（让路 / 无法物化 / 已有在途）。
// 闭包单独返回供测试同步执行；生产路径由 maybeRebuildMissingImage 起
// goroutine（buildDeployment 内部 WithoutCancel + build_timeout 封顶）。
func (f *Functions) prepareImageMissingRebuild(ctx context.Context, fn *domainfunctions.Function, dep *domainfunctions.Deployment) func() {
	key := rebuildKey{dep.ProjectID, dep.FunctionID, dep.ID}
	f.rebuildMu.Lock()
	if f.rebuilding == nil {
		f.rebuilding = make(map[rebuildKey]struct{})
	}
	if _, busy := f.rebuilding[key]; busy {
		f.rebuildMu.Unlock()
		return nil
	}
	f.rebuilding[key] = struct{}{}
	f.rebuildMu.Unlock()
	release := func() {
		f.rebuildMu.Lock()
		delete(f.rebuilding, key)
		f.rebuildMu.Unlock()
	}

	// 现读现状（dep 可能已被并发路径翻转）；非 ready = worker 补构建或在途
	// 重建已接管，让路。
	cur, err := f.repo.GetDeployment(ctx, dep.ProjectID, dep.FunctionID, dep.ID)
	if err != nil || cur == nil {
		release()
		return nil
	}
	if cur.Status != domainfunctions.DeploymentStatusReady {
		release()
		return nil
	}

	path := zipPath(dep.ProjectID, dep.FunctionID, dep.ID)
	if _, err := os.Stat(path); err != nil {
		switch cur.SourceType {
		case domainfunctions.DeploymentSourceGit:
			if err := f.rematerializeGitZip(ctx, cur, path); err != nil {
				f.logger().Warn("functions: image-missing rebuild skipped (git source re-materialization failed)",
					"project", dep.ProjectID, "function", dep.FunctionID, "deployment", dep.ID, "error", err)
				release()
				return nil
			}
		case domainfunctions.DeploymentSourceImage:
			// 无需 zip：buildDeployment 按 SourceType 分流为幂等 ImportImage。
		default:
			f.logger().Warn("functions: image-missing rebuild skipped (zip source snapshot lost; redeploy required)",
				"project", dep.ProjectID, "function", dep.FunctionID, "deployment", dep.ID)
			release()
			return nil
		}
	}

	// 快照传递：闭包不复用调用方的 dep 指针（避免与执行路径并发变异；
	// buildDeployment 会就地写 status/updated_at/build_node）。
	rebuildDep := *cur
	return func() {
		defer release()
		buildErr := f.buildDeployment(context.WithoutCancel(ctx), fn, &rebuildDep, path)
		if buildErr != nil {
			f.logger().Warn("functions: image-missing rebuild failed",
				"project", dep.ProjectID, "function", dep.FunctionID, "deployment", dep.ID,
				"status", rebuildDep.Status, "error", buildErr)
			return
		}
		f.logger().Info("functions: image-missing rebuild finished",
			"project", dep.ProjectID, "function", dep.FunctionID, "deployment", dep.ID,
			"status", rebuildDep.Status)
	}
}

// rematerializeGitZip 按部署行内源快照（URL + 钉死 commit SHA + 子目录）经
// packer 重新物化 zip 代码包并复核 ContextSHA256。私有仓库因凭证不落库
// （D8）可能 re-clone 失败——报错即停，与 worker 补拉私有镜像的声明边界
// 同口径。
func (f *Functions) rematerializeGitZip(ctx context.Context, dep *domainfunctions.Deployment, path string) error {
	if f.packer == nil {
		return errPackerUnavailable()
	}
	_, checksum, zip, err := f.packer.PackGit(ctx, domainfunctions.GitSource{
		URL:       dep.SourceURL,
		Ref:       dep.SourceRef,
		Directory: dep.SourceDir,
	})
	if err != nil {
		return err
	}
	if checksum != dep.ContextSHA256 {
		return status.Errorf(codes.FailedPrecondition,
			"re-fetched git source checksum %q does not match pinned %q (repository history changed; redeploy required)",
			checksum, dep.ContextSHA256)
	}
	return writeZip(path, zip)
}

// rebuildOnMissingImage 解析 functions.dispatcher.rebuild_on_missing_image
// （optional bool presence 语义，「默认 true 可关」）：dispatcher 段未配置、
// 字段未设置 → 默认开启；显式 false → 关闭。先例：verifyBuildEnabled。
func (f *Functions) rebuildOnMissingImage() bool {
	if f.cfg == nil {
		return true
	}
	d := f.cfg.GetFunctions().GetDispatcher()
	return d == nil || d.RebuildOnMissingImage == nil || d.GetRebuildOnMissingImage()
}
