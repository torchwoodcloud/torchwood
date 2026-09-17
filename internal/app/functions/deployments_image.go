package functions

import (
	"context"
	"time"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件承载 BYO 镜像部署源分支（三期阶段 1，设计
// docs/design/functions-runtimes-and-sources.md §3）：形状校验（防御性
// 复核层；完整 host guard / registry 白名单 / digest 钉死在阶段 2 dispatcher
// 侧）→ GetFunction（含源/运行时互斥校验，D7 双向）→ executor.ImportImage
//（INSERT 之前——失败无行，与 git 源 pack→INSERT 同构）→ INSERT 行
//（source_ref = 钉死 digest、template_version = 0）→ buildDeployment（image
// 分流：信号量 → building → 幂等 ImportImage 复检 → ready）。
//
// INSERT 时机裁决：source 列 INSERT 期写全、之后不可变（UpdateDeployment
// 列白名单有意不含），digest 必须在 INSERT 前已知——故首次 ImportImage 在
// 落库之前执行；导入成功而 INSERT 失败时残留的平台镜像由 RemoveImage 幂等
// 清理兜底（与 git 源 INSERT 失败清 zip 同类）。

// maxImageSourceReferenceBytes 是镜像引用长度上限（与 proto
// ImageSource.image max_len 同值；server 侧纵深复核——坏形状在调 executor
// 前拒绝）。
const maxImageSourceReferenceBytes = 500

// validateImageSource 对镜像源做 server 侧形状校验（防御性复核层）：本阶段
// 只做引用非空与长度上限；host 段形状、拨号点 IP guard（SSRF）与
// allowed_registries 白名单在阶段 2 dispatcher 侧实施（设计 §3 安全基线）。
func validateImageSource(src *domainfunctions.ImageSource) error {
	if src.Reference == "" {
		return status.Error(codes.InvalidArgument, "image source reference is required")
	}
	if len(src.Reference) > maxImageSourceReferenceBytes {
		return status.Errorf(codes.InvalidArgument, "image source reference exceeds %d bytes", maxImageSourceReferenceBytes)
	}
	return nil
}

// createDeploymentFromImage 是 CreateDeployment 的 BYO 镜像源分支（三期
// 阶段 1，设计 §3）。失败清理分流：形状/互斥校验失败或首次 ImportImage
// 失败 → 无行（请求级失败不留残骸）；INSERT 失败 → 无行 + RemoveImage 兜底；
// buildDeployment 内的导入复检失败收敛为 failed 状态（ready 门禁拒绝）。
func (f *Functions) createDeploymentFromImage(ctx context.Context, cmd CreateDeploymentCommand, src *domainfunctions.ImageSource) (*domainfunctions.Deployment, error) {
	if err := validateImageSource(src); err != nil {
		return nil, err
	}
	fn, err := f.repo.GetFunction(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	// 源/运行时互斥（D7 双向）：image 源仅 runtime = image 的函数接受。
	if err := validateSourceRuntimePair(fn.Runtime, domainfunctions.DeploymentSourceImage); err != nil {
		return nil, err
	}
	// 首次 ImportImage（INSERT 之前，与 git pack→INSERT 同构）：digest 钉死
	// 是 source 列不可变写入的前提；一次性凭证仅随本 spec 进入调用栈。
	depID := idgen.UUID().String()
	spec, err := f.importImageSpec(ctx, fn, depID, src, "")
	if err != nil {
		return nil, err
	}
	digest, err := f.executor.ImportImage(ctx, spec)
	if err != nil {
		return nil, err
	}
	if digest == "" {
		return nil, status.Error(codes.Internal, "executor returned an empty image digest")
	}
	now := time.Now()
	dep := &domainfunctions.Deployment{
		ID:         depID,
		FunctionID: cmd.FunctionID,
		ProjectID:  cmd.ProjectID,
		// image 源无 zip 字节流，size 恒 0（镜像体积不在控制面记录）。
		Size:   0,
		Status: domainfunctions.DeploymentStatusPending,
		// template_version = 0（未知模板，D12）：BYO 镜像由用户自持 runner
		// 契约实现，模板版本无意义；既有 < MinConcurrencyTemplateVersion
		// 判定据此自动把 concurrency>1 降为 1——是否真支持并发无从验证，
		// fail-safe 白拿。
		TemplateVersion: 0,
		// 源投影：source_url = 原始引用、source_ref = 钉死 digest、source_dir
		// 恒空（git 专有列）；凭证字段在投影上不存在（D8）。
		SourceType: domainfunctions.DeploymentSourceImage,
		SourceURL:  src.Reference,
		SourceRef:  digest,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := f.repo.CreateDeployment(ctx, dep); err != nil {
		_ = f.executor.RemoveImage(ctx, cmd.FunctionID, depID)
		return nil, err
	}
	// ready 门禁（与 zip/git 源同路进入 buildDeployment；image 分流为幂等
	// ImportImage 复检——spec 带预期 digest = 行内 source_ref）。返回 err =
	// 请求级失败（信号量满/状态写回失败）：删行，不留残骸。
	if err := f.buildDeployment(ctx, fn, dep, ""); err != nil {
		_ = f.repo.DeleteDeployment(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID)
		_ = f.executor.RemoveImage(ctx, cmd.FunctionID, dep.ID)
		return nil, err
	}
	return dep, nil
}
