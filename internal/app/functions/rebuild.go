package functions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

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
//   - zip/git 源：盘上 zip 命中直接重建；盘缺失时优先从持久层（zip 专用
//     桶，functions.storage）拉回复核行内锚（ContextSHA256 全量复核；空锚
//     存量 zip 部署按 Size 复核），git 源拉回 miss/暂态失败再按行内源快照
//     （URL + 钉死 commit SHA + 子目录）经 packer 重新物化并复核
//     ContextSHA256（D9 可复现锚——钉死 SHA 下重物化应逐字节一致，不一致
//     = 仓库历史改写，拒绝以漂移源覆盖快照）；
//   - image 源：无需 zip，buildDeployment 按 SourceType 分流为幂等
//     ImportImage（预期 digest = 行内 source_ref）；
//   - zip 源且桶内无副本（存量部署/zipStore 未注入）：退回声明边界——记录
//     日志，执行错误文案（rebuild required）引导 redeploy。
//
// 当次执行不等待重建（构建分钟级，同步调用方不得被连坐）：以原始错误失败，
// 重建期间后续执行以「no ready deployment」fail-fast（building 非 ready，
// selectDeployment 拒绝），完成后执行面自愈。开关 functions.dispatcher.
// rebuild_on_missing_image 未配置 = 默认开启，显式 false 关闭。

// rebuildKey 是在途重建去重键（进程内 map + 可选跨进程端口双层；并发执行
// 同时命中同一缺失镜像只触发一次重建——server 与 worker 多副本场景由
// dedup 端口收敛，见 rebuild_dedup.go）。
type rebuildKey struct {
	projectID, functionID, deploymentID string
}

// string 是跨进程去重键的逻辑标识投影（Redis 实现再加 torchwood:fnrebuild:
// 前缀）；组件为平台内部 ID，不含分隔符歧义。
func (k rebuildKey) string() string {
	return k.projectID + "/" + k.functionID + "/" + k.deploymentID
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

// rebuildDedupTTLGrace 是在途去重键 TTL 在构建超时预算之上的余量：构建
// ctx 由 build_timeout 封顶、闭包结束主动 Release，TTL 只是进程崩溃后键
// 残留的兜底（到期自愈链自然解封）。
const rebuildDedupTTLGrace = time.Minute

// prepareImageMissingRebuild 执行触发前置（在途去重 → 现读部署状态 → 源
// 物化），返回后台重建闭包；nil = 不触发（让路 / 无法物化 / 已有在途）。
// 闭包单独返回供测试同步执行；生产路径由 maybeRebuildMissingImage 起
// goroutine（buildDeployment 内部 WithoutCancel + build_timeout 封顶）。
func (f *Functions) prepareImageMissingRebuild(ctx context.Context, fn *domainfunctions.Function, dep *domainfunctions.Deployment) func() {
	release := f.acquireRebuildDedup(ctx, rebuildKey{dep.ProjectID, dep.FunctionID, dep.ID})
	if release == nil {
		return nil
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
		if cur.SourceType != domainfunctions.DeploymentSourceImage {
			// image 源免 zip（buildDeployment 分流为幂等 ImportImage）；zip/git
			// 源盘缺失：持久层拉回优先（restoreDeploymentZip 内含 git 源回退
			// packer 重物化）。
			if err := f.restoreDeploymentZip(ctx, cur, path); err != nil {
				f.logger().Warn("functions: image-missing rebuild skipped (code package unavailable)",
					"project", dep.ProjectID, "function", dep.FunctionID, "deployment", dep.ID, "error", err)
				release()
				return nil
			}
		}
	}

	// 快照传递：闭包不复用调用方的 dep 指针（避免与执行路径并发变异；
	// buildDeployment 会就地写 status/updated_at/build_node）。
	rebuildDep := *cur
	return func() {
		defer release()
		buildErr := f.buildDeployment(context.WithoutCancel(ctx), fn, &rebuildDep, path, buildOptions{rebuild: true})
		if buildErr != nil {
			f.logger().Warn("functions: image-missing rebuild failed (deployment kept ready for retry; code package retained)",
				"project", dep.ProjectID, "function", dep.FunctionID, "deployment", dep.ID,
				"status", rebuildDep.Status, "error", buildErr)
			return
		}
		f.logger().Info("functions: image-missing rebuild finished",
			"project", dep.ProjectID, "function", dep.FunctionID, "deployment", dep.ID,
			"status", rebuildDep.Status)
	}
}

// acquireRebuildDedup 抢占在途重建（双层：进程内 map 恒参与作单进程第一道
// 闸；dedup 端口非 nil 时再抢跨进程键）。返回 release 闭包（本地与远端键
// 一起释放）；nil = 已有在途重建（本地或远端副本），让路。Redis 故障
// fail-open：去重是效率优化不是正确性门槛——缺陷 A 修复后重复构建幂等
// 收敛 ready，阻断自愈链的代价更高。
func (f *Functions) acquireRebuildDedup(ctx context.Context, key rebuildKey) func() {
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

	remote := f.dedup != nil
	if remote {
		acquired, err := f.dedup.TryAcquire(ctx, key.string(), f.buildTimeout()+rebuildDedupTTLGrace)
		if err == nil && !acquired {
			// 其他副本已在途重建：释放本地标记并让路。
			f.releaseLocalRebuild(key)
			return nil
		}
		if err != nil {
			f.logger().Warn("functions: rebuild dedup probe failed; proceeding without cross-process dedupe",
				"project", key.projectID, "function", key.functionID, "deployment", key.deploymentID, "error", err)
		}
	}
	return func() {
		f.releaseLocalRebuild(key)
		if remote {
			// 释放用独立短超时 ctx：闭包在 WithoutCancel 预算上运行，但
			// 让路/早退路径的调用方 ctx 可能随时被取消，释放必须可靠。
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := f.dedup.Release(rctx, key.string()); err != nil {
				f.logger().Warn("functions: rebuild dedup release failed (key expires via TTL)",
					"project", key.projectID, "function", key.functionID, "deployment", key.deploymentID, "error", err)
			}
		}
	}
}

// releaseLocalRebuild 释放进程内去重标记（rebuilding map）。
func (f *Functions) releaseLocalRebuild(key rebuildKey) {
	f.rebuildMu.Lock()
	delete(f.rebuilding, key)
	f.rebuildMu.Unlock()
}

// restoreDeploymentZip 让盘上 zip 缺失的部署重新具备构建输入（zip 源自愈
// 主通路 + git 源省一次 packer 重克隆的捷径）：
//   - 持久层拉回（zipStore 注入时）：复核行内锚后落盘 zipPath；
//   - git 源拉回 miss/暂态失败：回退 packer 重物化（既有通路，checksum
//     复核）——packer 与对象存储互为独立通路，任一暂态故障不连坐；
//   - zip 源拉回 miss/暂态失败：无其他源，返回错误让路（镜像仍缺失，
//     下次执行重进本链；miss = 存量部署或桶副本被删，引导 redeploy）。
//
// image 源不经本函数（调用方已分流）。返回 nil = path 就绪可构建。
func (f *Functions) restoreDeploymentZip(ctx context.Context, dep *domainfunctions.Deployment, path string) error {
	if f.zipStore != nil {
		zip, err := f.zipStore.Get(ctx, dep.ProjectID, dep.FunctionID, dep.ID)
		if err == nil {
			if verr := verifyRestoredZip(dep, zip); verr != nil {
				return verr
			}
			return writeZip(path, zip)
		}
		if !errors.Is(err, domainfunctions.ErrZipNotFound) && dep.SourceType != domainfunctions.DeploymentSourceGit {
			return err
		}
		// ErrZipNotFound，或 git 源的暂态取回失败：git 源回退重物化，zip 源
		// 落下方 miss 分支。
	}
	if dep.SourceType == domainfunctions.DeploymentSourceGit {
		return f.rematerializeGitZip(ctx, dep, path)
	}
	return fmt.Errorf("%w: zip source snapshot lost and no stored copy; redeploy required", domainfunctions.ErrZipNotFound)
}

// verifyRestoredZip 复核持久层拉回的字节与部署行内锚一致，防漂移副本进入
// 构建：ContextSHA256 非空（git 源与新 zip 源）→ sha256 全量复核（与
// rematerializeGitZip 复核同锚，D9）；空锚的存量 zip 部署 → Size 长度复核
// （浅校验兜底：桶内对象由平台写入，完整性威胁模型以长度漂移为界）。
func verifyRestoredZip(dep *domainfunctions.Deployment, zip []byte) error {
	if dep.ContextSHA256 != "" {
		sum := sha256.Sum256(zip)
		if hex.EncodeToString(sum[:]) != dep.ContextSHA256 {
			return status.Errorf(codes.FailedPrecondition,
				"stored code package checksum %q does not match pinned %q (stored copy drifted; redeploy required)",
				hex.EncodeToString(sum[:]), dep.ContextSHA256)
		}
		return nil
	}
	if dep.Size > 0 && int64(len(zip)) != dep.Size {
		return status.Errorf(codes.FailedPrecondition,
			"stored code package size %d does not match recorded %d (stored copy drifted; redeploy required)",
			len(zip), dep.Size)
	}
	return nil
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
