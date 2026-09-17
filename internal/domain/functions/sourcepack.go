package functions

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
