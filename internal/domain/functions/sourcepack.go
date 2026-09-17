package functions

import "context"

// ——部署源词表（迁移 000023 function_deployments.source_type CHECK 同源；
// docs/design/functions-runtimes-and-sources.md §0）——
const (
	// DeploymentSourceZip 是 zip 内联代码包源（一期既有通道）。
	DeploymentSourceZip = "zip"
	// DeploymentSourceGit 是 git 仓库源（二期）：由独立 functions-packer
	// 服务物化为 zip 落既有构建路径。
	DeploymentSourceGit = "git"
	// DeploymentSourceImage 是 BYO 镜像源（三期启用；词表与 DB CHECK 预留）。
	DeploymentSourceImage = "image"
)

// GitSource 是 git 仓库部署源值对象（二期，设计 §2）：由独立
// functions-packer 服务物化为 zip 落既有 zipPath。Username/Token 是一次性
// 凭证——仅本次请求内存、不落库不回显（D8），持久化层只存投影四件
// （source_url/source_ref/source_dir/context_sha256），本结构体整体不得落库。
type GitSource struct {
	URL       string
	Ref       string // branch/tag/commit；空 = HEAD（服务端钉死为 commit SHA）
	Directory string // 仓库内子目录 = 构建上下文根；空 = 根目录
	Username  string // 可选 Basic 凭证（PAT）；一次性
	Token     string // 一次性凭证
}

// SourcePacker 是 git 部署源的打包端口（二期阶段 3，设计 §2）：由独立
// functions-packer 服务承载不可信 git 输入的重资源操作（浅克隆 + 子目录
// 物化），把 GitSource 归一为与 zip 源同构的代码包交回 server 落既有
// zipPath。zip 流向反转：packer 打好 zip → server 落盘（设计 §2 裁决）。
type SourcePacker interface {
	// PackGit 把 git 部署源打包为 zip 代码包（调 functions-packer 服务）。
	// 返回值：commitSHA 是解析后钉死的提交（落 source_ref，审计四件之一——
	// 分支后续移动不影响已部署内容）；checksum 是 zip 字节的 hex sha256
	// （落 context_sha256）；zip 是代码包字节流（≤ functions.packer.max_zip_bytes）。
	// 凭证只在调用栈内存，实现不得持久化/记日志。
	PackGit(ctx context.Context, src GitSource) (commitSHA, checksum string, zip []byte, err error)
}
