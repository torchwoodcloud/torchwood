package dockerdriver

// 本文件是 BYO 镜像源的导入编排（54b666b importImage 语义适配：单机
// docker 形态，pull → digest 钉死 → retag 平台命名 → 强制契约验证 spawn
// → 返回钉死 digest；调用方落 deployment.source_ref）。多节点 registry
// 路由的补拉/推送面（M7/EnsureImage）随 IMPL-T2-3 退役不复归。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/image"
	"github.com/torchwoodcloud/torchwood/dispatcher"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// importImage 是 ImportImage 的编排实现（镜像源免构建路径）：
//
//	host 名称级校验 → 取得钉死内容（ExpectedDigest 本地命中零 pull；引用本地
//	已存在 = 本地构建/本地 tag 直导场景直接用本地内容；否则 pull，RegistryAuth
//	内联单次转发）→ digest 钉死（引用带 @sha256 时校验一致，防 tag 漂移）→
//	retag 进平台命名（「镜像名 = 平台命名」不变式，池 spawn/RemoveImage 零
//	改动）→ 删原始引用标签（防本地 daemon 残留；reference 即 target 时跳过）
//	→ 强制契约验证 spawn（config verify_build 关不掉——设计 §3「强制」）→
//	返回钉死 digest。
//
// pull 超时 = 调用方 ctx：首次导入随请求 ctx，worker 补构建 / ready 门禁
// 复检经 build_timeout 预算（WithoutCancel 解耦）。
func importImage(ctx context.Context, d dispatcher.Daemon, imgs imageClient, probe dispatcher.HealthProber, cfg *config.AppConfig, opts dispatcher.ImportImageOptions, bootTimeout time.Duration, maxRequests int) (string, error) {
	// 1) registry host 名称级校验（名称级校验 + 白名单 + allow_insecure 显式
	//    开关；失败 InvalidArgument 明示命中规则）。先于一切 docker 操作——
	//    本地直导路径同样受准入口径约束。
	if err := dispatcher.ValidateImageRegistryHost(dispatcher.ParseImageReferenceHost(opts.Reference),
		cfg.GetFunctions().GetImage().GetAllowedRegistries(),
		cfg.GetFunctions().GetImage().GetAllowInsecure()); err != nil {
		return "", err
	}

	target := infrafunctions.ImageName(cfg, opts.FunctionID, opts.DeploymentID)

	// 2) 幂等（「本地命中则零 pull」）：ExpectedDigest 非空且本地平台镜像可
	//    判定持有时零 pull/retag 直接契约验证并确认（worker 补拉 / ready
	//    门禁复检）。非 NotFound 的 inspect 错误原样上抛（daemon 不可达时
	//    后续操作也必败，fail-fast）。
	if opts.ExpectedDigest != "" {
		ins, err := imgs.ImageInspect(ctx, target)
		switch {
		case err == nil && localImageMatches(ins, opts.ExpectedDigest):
			if err := verifyImported(ctx, d, probe, opts, target, bootTimeout, maxRequests); err != nil {
				return "", err
			}
			return opts.ExpectedDigest, nil
		case err != nil && !errdefs.IsNotFound(err):
			return "", fmt.Errorf("inspect platform image %q: %w", target, err)
		}
	}

	// 3) 取得钉死内容：引用本地已存在（本地构建/本地 tag 直导场景）直接用
	//    本地内容——跳过 pull，不触网（内容陈旧风险由部署者承担，digest 钉
	//    死值在 source_url/source_ref 上诚实可见）；未命中（NotFound）才
	//    pull（凭证空 = 匿名；RegistryAuth 是 base64 JSON 单次转发 daemon，
	//    不落库不落日志）。pull 失败流内 JSON error（registry 认证失败/引用
	//    不存在在此冒出）。
	refIns, err := imgs.ImageInspect(ctx, opts.Reference)
	switch {
	case errdefs.IsNotFound(err):
		pullResp, pullErr := imgs.ImagePull(ctx, opts.Reference, image.PullOptions{RegistryAuth: registryAuth(opts.RegistryUsername, opts.RegistryToken)})
		if pullErr != nil {
			return "", fmt.Errorf("docker pull %q: %w", opts.Reference, pullErr)
		}
		if _, streamErr := infrafunctions.ReadBuildOutput(pullResp); streamErr != nil {
			_ = pullResp.Close()
			return "", status.Errorf(codes.InvalidArgument, "docker pull %q failed: %s",
				opts.Reference, infrafunctions.TruncateBuildLog(streamErr.Error()))
		}
		_ = pullResp.Close()
		refIns, err = imgs.ImageInspect(ctx, opts.Reference)
		if err != nil {
			return "", fmt.Errorf("inspect %q after pull: %w", opts.Reference, err)
		}
	case err != nil:
		return "", fmt.Errorf("inspect %q: %w", opts.Reference, err)
	}

	// 4) digest 钉死：提取 RepoDigest 的 @sha256 部分；引用自带 @sha256
	//    校验一致（tag 漂移 → InvalidArgument）；调用方预期 digest 不一致
	//    同样拒绝。
	digest, err := pinnedDigest(opts.Reference, refIns)
	if err != nil {
		return "", err
	}
	if opts.ExpectedDigest != "" && digest != opts.ExpectedDigest {
		return "", status.Errorf(codes.InvalidArgument,
			"pulled image digest %q does not match expected digest %q (tag drift; redeploy pinning the exact digest)",
			digest, opts.ExpectedDigest)
	}

	// 5) retag 进平台命名并删除原始引用标签（retag 后全链路零改动且删除
	//    语义干净）。原始引用删除防本地 daemon 残留；本地构建/本地 tag 场景
	//    reference 可能就是平台镜像名自身（相同则跳过）；同镜像多标签下
	//    Force=false 只摘标签不删数据，NotFound 容忍（引用已不存在 = 已无
	//    残留）。
	if err := imgs.ImageTag(ctx, opts.Reference, target); err != nil {
		return "", fmt.Errorf("tag %q -> %q: %w", opts.Reference, target, err)
	}
	if opts.Reference != target {
		if _, err := imgs.ImageRemove(ctx, opts.Reference, image.RemoveOptions{Force: false}); err != nil && !errdefs.IsNotFound(err) {
			return "", fmt.Errorf("remove original reference %q: %w", opts.Reference, err)
		}
	}

	// 6) 强制契约验证 spawn + 返回钉死 digest。
	if err := verifyImported(ctx, d, probe, opts, target, bootTimeout, maxRequests); err != nil {
		return "", err
	}
	return digest, nil
}

// verifyImported 对导入后的平台镜像执行强制契约验证 spawn（与 build 验证
// 同一机制：池外实例、带函数 variables、egress 选网、失败日志尾回收）。
func verifyImported(ctx context.Context, d dispatcher.Daemon, probe dispatcher.HealthProber, opts dispatcher.ImportImageOptions, image string, bootTimeout time.Duration, maxRequests int) error {
	return dispatcher.SpawnVerifyInstance(ctx, d, probe, dispatcher.BuildImageOptions{
		ProjectID:       opts.ProjectID,
		FunctionID:      opts.FunctionID,
		DeploymentID:    opts.DeploymentID,
		Env:             opts.Env,
		EgressUntrusted: opts.EgressUntrusted,
	}, image, bootTimeout, maxRequests)
}

// registryAuth 构造 docker RegistryAuth（base64 JSON 形态
// {"username","password","serveraddress":""}；凭证空 = 匿名 pull，返回空串
// ——空 RegistryAuth 时 daemon 走匿名）。一次性凭证只在内存存在。
func registryAuth(username, token string) string {
	if username == "" && token == "" {
		return ""
	}
	raw, err := json.Marshal(map[string]string{
		"username":      username,
		"password":      token,
		"serveraddress": "",
	})
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// localImageMatches 判定本地平台镜像是否命中预期 digest（幂等零 pull 的
// 判定核心）：
//   - RepoDigests 任一条目命中，或 Image ID 直接过命中 → 命中（严格可证；
//     ID 命中覆盖本地构建/本地 tag 场景——未经 registry 的镜像无
//     RepoDigest，Image ID（config digest）是唯一内容寻址钉死值）；
//   - RepoDigests 非空且全不命中 → 不命中（可证的本地内容漂移 → 走重拉
//     对账，比对后拒绝）；
//   - RepoDigests 为空且 ID 不命中 → 命中：这是首次导入自身的稳态——原始
//     引用已删除，registry manifest digest 关联随原始引用摘除，而 Image ID
//     ≠ manifest digest，本地内容无法严格证伪。平台命名 tag 属平台专属
//     命名空间（外部改动不在威胁模型），其内容在首次导入时已钉死 digest
//     并通过契约验证——target 存在即视为幂等命中（「本地命中零 pull」承诺
//     的可实现形态；补拉语义只在镜像真正缺失时触发）。
func localImageMatches(ins image.InspectResponse, digest string) bool {
	for _, rd := range ins.RepoDigests {
		if digestFromRepoDigest(rd) == digest {
			return true
		}
	}
	if ins.ID == digest {
		return true
	}
	if len(ins.RepoDigests) > 0 {
		return false // 可证漂移：本地内容确定性不匹配 → 走重拉对账
	}
	// 不可证伪态（首次导入稳态）：target 存在即视为幂等命中；退化 inspect
	//（无 ID 无 RepoDigests）不视为命中，落回 pull 路径。
	return ins.ID != ""
}

// pinnedDigest 从 pull 后的 inspect 结果提取钉死 digest：优先在 RepoDigests
// 中命中引用自带 @sha256 的条目（多仓库镜像的 RepoDigests 顺序不稳定，防
// 误判）；无自带 digest 时取 RepoDigests[0]；RepoDigests 为空（本地构建/
// docker load 的镜像不经 registry）回落镜像 ID——内容寻址钉死值仍成立，
// registry 摘要缺失对调用方诚实可见。
func pinnedDigest(reference string, ins image.InspectResponse) (string, error) {
	refDigest := referenceDigest(reference)
	if refDigest != "" {
		for _, rd := range ins.RepoDigests {
			if digestFromRepoDigest(rd) == refDigest {
				return refDigest, nil
			}
		}
		if ins.ID == refDigest {
			return refDigest, nil
		}
		return "", status.Errorf(codes.InvalidArgument,
			"image reference digest %q does not match any pulled repo digests %v (tag drift)",
			refDigest, ins.RepoDigests)
	}
	if len(ins.RepoDigests) > 0 {
		return digestFromRepoDigest(ins.RepoDigests[0]), nil
	}
	if ins.ID != "" {
		return ins.ID, nil
	}
	return "", status.Errorf(codes.Internal, "image %q inspect returned no digest to pin", reference)
}

// referenceDigest 提取引用中的 digest 部分（"repo@sha256:..." →
// "sha256:..."；无 digest 引用返回空串）。
func referenceDigest(reference string) string {
	if i := strings.Index(reference, "@"); i >= 0 {
		return reference[i+1:]
	}
	return ""
}

// digestFromRepoDigest 提取 RepoDigest 条目的 digest 部分
// （"alpine@sha256:..." → "sha256:..."）。
func digestFromRepoDigest(repoDigest string) string {
	if i := strings.Index(repoDigest, "@"); i >= 0 {
		return repoDigest[i+1:]
	}
	return repoDigest
}
