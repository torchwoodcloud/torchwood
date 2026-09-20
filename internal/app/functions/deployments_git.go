package functions

import (
	"context"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件承载 git 部署源分支（二期阶段 3，设计
// docs/design/functions-runtimes-and-sources.md §2）：形状校验（防御性
// 复核，protovalidate 已挡大部分）→ SourcePacker.PackGit（pack 在
// deployment 行落库之前——失败路径无行无 zip）→ zip 写既有 zipPath →
// INSERT 行（source 投影 + 钉死 SHA + checksum）→ buildDeployment——与
// zip 源完全同构。构建失败后 zip 保留（重建语义：worker 补构建以盘上
// zip 为输入，不依赖一次性凭证），分流在 buildDeployment 按 SourceType。

// maxGitSourceRefBytes 是 ref 的长度上限（分支/tag 名的 POSIX PATH_MAX
// 保守投影；protovalidate 已有长度约束，此处纵深复核）。
const maxGitSourceRefBytes = 255

// gitSourceRefRe 是 ref 的字符白名单（branch/tag/40 位 hex commit 的字符
// 形态 `[A-Za-z0-9._/-]`；防御性复核——拒绝 shell 元字符与空白进 packer
// refspec）。
var gitSourceRefRe = regexp.MustCompile(`^[A-Za-z0-9._/\-]*$`)

// errPackerUnavailable 是 packer 端口缺失（旧构造未注入）时的统一错误——
// 与 PackerClient 的 url 未配置错误同文案：两者对用户是同一事实——git 源
// 未启用（functions.packer.url 未配置），zip/node 不受影响。
func errPackerUnavailable() error {
	return status.Error(codes.FailedPrecondition, "functions.packer.url is not configured (git deployment source disabled)")
}

// createDeploymentFromGit 是 CreateDeployment 的 git 源分支（设计 §2 app
// 时序）。失败清理分流：pack 失败/写盘失败/落桶失败/INSERT 失败/构建信号
// 量满 → 无行无 zip（请求级失败不留残骸）；buildDeployment 内的构建失败
// 收敛为 failed 状态且 git zip 保留（重建语义，见 buildDeployment）。
func (f *Functions) createDeploymentFromGit(ctx context.Context, cmd CreateDeploymentCommand, src *domainfunctions.GitSource) (*domainfunctions.Deployment, error) {
	// 形状校验：url https（allow_insecure 时放行 http）/ref 白名单/dir 无
	// 穿越段——packer 侧还有完整的 SSRF guard 与 userinfo 拒绝，此处是
	// server 侧的纵深复核（坏形状在调 packer 前拒绝，省一次重资源调用）。
	if err := f.validateGitSource(src); err != nil {
		return nil, err
	}
	fn, err := f.repo.GetFunction(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	// 源/运行时互斥（D7 双向）：git/zip 源对 image runtime 函数拒绝；
	// 运行时状态门：eol runtime 拒绝新的部署构建（functions-runtime-
	// selection.md §6）。
	if err := validateSourceRuntimePair(fn.Runtime, domainfunctions.DeploymentSourceGit); err != nil {
		return nil, err
	}
	if err := validateRuntimeSelectable(fn.Runtime); err != nil {
		return nil, err
	}
	// pack（在行落库之前）：失败路径无行无 zip，与 zip 魔数校验失败同类。
	if f.packer == nil {
		return nil, errPackerUnavailable()
	}
	commitSHA, checksum, zip, err := f.packer.PackGit(ctx, *src)
	if err != nil {
		return nil, err
	}
	if len(zip) == 0 {
		return nil, status.Error(codes.Internal, "packer returned an empty code package")
	}
	now := time.Now()
	dep := &domainfunctions.Deployment{
		ID:         idgen.UUID().String(),
		FunctionID: cmd.FunctionID,
		ProjectID:  cmd.ProjectID,
		Size:       int64(len(zip)),
		Status:     domainfunctions.DeploymentStatusPending,
		// 模板版本化（P0.5）：与 zip 源同口径记录构建所用 runner 模板版本。
		TemplateVersion: domainfunctions.RunnerTemplateVersion,
		// 源投影四件（迁移 000023）：URL/钉死 SHA/子目录 + zip 校验和——
		// 审计四件 = 不可变快照锚；凭证字段在投影上不存在（D8）。
		SourceType:    domainfunctions.DeploymentSourceGit,
		SourceURL:     src.URL,
		SourceRef:     commitSHA,
		SourceDir:     src.Directory,
		ContextSHA256: checksum,
		// runtime 快照（迁移 000025）：与 zip 源同口径（functions-runtime-
		// selection.md §4）。
		Runtime:   fn.Runtime,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 先写盘后落桶再 INSERT：盘上与持久层快照都就绪才开行；任一失败清理
	// zip + 行。
	path := zipPath(cmd.ProjectID, cmd.FunctionID, dep.ID)
	if err := writeZip(path, zip); err != nil {
		_ = removeZip(cmd.ProjectID, cmd.FunctionID, dep.ID)
		return nil, status.Errorf(codes.Internal, "write code package: %v", err)
	}
	if err := f.storeZip(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID, zip); err != nil {
		_ = removeZip(cmd.ProjectID, cmd.FunctionID, dep.ID)
		return nil, status.Errorf(codes.Internal, "persist code package: %v", err)
	}
	if err := f.repo.CreateDeployment(ctx, dep); err != nil {
		f.removeCodePackage(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID)
		return nil, err
	}
	// 同步构建（与 zip 源同路，D11 ctx 解耦不变）。返回 err = 请求级失败
	// （信号量满/状态写回失败）：删行删 zip——git 源的 zip 保留语义只属于
	// 「构建已发生并收敛为 failed」的快照场景，请求被拒绝时不留残骸。
	if err := f.buildDeployment(ctx, fn, dep, path, buildOptions{}); err != nil {
		_ = f.repo.DeleteDeployment(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID)
		f.removeCodePackage(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID)
		return nil, err
	}
	return dep, nil
}

// validateGitSource 对 git 源做 server 侧形状校验（防御性复核层）：
//   - url：必须 https://（functions.packer.allow_insecure=true 时放行
//     http://——自托管内网 forge 场景与 packer 侧 SSRF 放行开关同源）；
//     file:// 仅 development 环境放行（集成测试与本地开发；proto 层
//     pattern 放行两种 scheme 后由本层按环境收紧——非 dev 一律拒绝）；
//   - ref：字符白名单 + 长度上限；空 = HEAD 合法；
//   - directory：仓库内相对路径，Clean 后拒绝 `..` 段与绝对路径；空 = 根。
func (f *Functions) validateGitSource(src *domainfunctions.GitSource) error {
	if src.URL == "" {
		return status.Error(codes.InvalidArgument, "git source url is required")
	}
	allowInsecure := f.cfg.GetFunctions().GetPacker().GetAllowInsecure()
	if !strings.HasPrefix(src.URL, "https://") {
		if strings.HasPrefix(src.URL, "file://") {
			// file:// 仅 development（直读进程 env——与 cmd/server 的
			// TORCHWOOD_ENV 同源；packer 侧同名门控独立生效，两端须同为
			// development 才能走通，任一非 dev 即 fail-closed）。
			if os.Getenv("TORCHWOOD_ENV") != "development" {
				return status.Error(codes.InvalidArgument, "git source url file:// is only allowed in development environment")
			}
		} else if !allowInsecure || !strings.HasPrefix(src.URL, "http://") {
			return status.Error(codes.InvalidArgument, "git source url must use https:// (http is only allowed with functions.packer.allow_insecure)")
		}
	}
	if len(src.Ref) > maxGitSourceRefBytes {
		return status.Errorf(codes.InvalidArgument, "git source ref exceeds %d bytes", maxGitSourceRefBytes)
	}
	if !gitSourceRefRe.MatchString(src.Ref) {
		return status.Error(codes.InvalidArgument, "git source ref may only contain letters, digits, '.', '_', '/', '-'")
	}
	return validateGitDirectory(src.Directory)
}

// validateGitDirectory 校验仓库内子目录形态：拒绝绝对路径、反斜杠分隔符
// 与任何 `..` 穿越段（Clean 归一 `./` 与重复分隔符后逐段复核）。
func validateGitDirectory(dir string) error {
	if dir == "" {
		return nil
	}
	if strings.Contains(dir, "\\") {
		return status.Error(codes.InvalidArgument, "git source directory must use '/' separators")
	}
	if strings.HasPrefix(dir, "/") {
		return status.Error(codes.InvalidArgument, "git source directory must be a relative path inside the repository")
	}
	clean := path.Clean(dir)
	// Clean 归一 "./" 与重复分隔符（"." → 根目录，合法）后逐段拒绝穿越。
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return status.Error(codes.InvalidArgument, "git source directory must not contain '..' segments")
		}
	}
	return nil
}
