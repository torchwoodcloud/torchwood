// Package packer 是 git 部署源的打包通路（二期阶段二，2026-09-17
// owner 裁决：独立 packer 进程，推翻「server 进程内物化」与
// 「dispatcher 进程内 fetch」两案；docs/design/functions-runtimes-and-sources.md
// §2）。
//
// 职责：专职承载不可信 git 输入的重资源操作——浅克隆 + worktree 核算 +
// 子目录物化为 zip，把 url@ref[:directory] 归一为与 zip 源同构的代码包交
// 回调用方（zip 流向反转：packer → server 落既有 zipPath）。server/worker
// 零 git 流量；clone 的内存尖峰/磁盘消耗全部收敛在本进程——它死了只有
// git 部署不可用（重启即恢复），API/函数执行/node 构建无感（设计 §2
// 资源画像与隔离声明）。
//
// 无状态进程、无 Redis/DB/docker 依赖、可独立重启/扩缩（多副本无亲和
// 需求——zip 由调用方 server 落构建亲和节点本地盘）；单请求内存上界
// ≈ 物化 zip 预算（默认 50MiB）+ base64 膨胀（×4/3）。
package packer

import "time"

// ——打包预算（两级预算 + 条目上限，设计 §2「物化」）——
//
// 字节预算（worktree ≤200MiB 磁盘侧 / zip ≤50MiB 传输侧）与并发上限来自
// config（functions.packer.max_repo_bytes / max_zip_bytes / concurrency），
// 不设编译期常量；这里只放条目上限与 config 缺省值。
const (
	// MaxPackEntries 是单次物化的条目（文件）上限：git worktree 是真实
	// 文件，宽于 zip 上传通道的 1000 反炸弹声明侧预检；dispatcher 侧
	// ExtractZip 的构建路径用注入的放宽 limits（条目对齐本值）消费物化
	// zip。>5000 条的典型 vendor 项目仍受限，属声明边界（设计 §2）。
	MaxPackEntries = 5000

	// DefaultMaxRepoBytes / DefaultMaxZipBytes / DefaultFetchTimeout /
	// DefaultConcurrency 是 config 未配置（或非法/<=0）时的缺省预算，
	// 与 config.proto 注释一一对应。
	DefaultMaxRepoBytes = 200 << 20
	DefaultMaxZipBytes  = 50 << 20
	DefaultFetchTimeout = 120 * time.Second
	DefaultConcurrency  = 4

	// defaultAddr 是 packer HTTP 监听缺省地址（与 dispatcher 缺省
	// ":9070" 错开）。
	defaultAddr = ":9071"
)

// PackRequest 是 POST /v1/pack/git 入参，与 domain GitSource 值对象
// （internal/domain/functions/sourcepack.go）同构：把 url@ref 的 directory
// 子目录物化为 zip。Username/Token 是一次性 Basic 凭证——仅本次请求内存、
// 不落库不写日志不回显（D8）。
type PackRequest struct {
	URL       string `json:"url"`
	Ref       string `json:"ref,omitempty"`       // branch/tag/40 位 hex；空 = HEAD
	Directory string `json:"directory,omitempty"` // 仓库内子目录 = 构建上下文根；空 = 根目录
	Username  string `json:"username,omitempty"`  // 可选 Basic 用户名；空回落字面量 "git"
	Token     string `json:"token,omitempty"`     // 一次性凭证（PAT）
}

// PackResponse 是出参：CommitSHA 是解析后钉死的提交（部署审计四件之一，
// 分支后续移动不影响已部署内容）；Checksum 是 zip 字节的 hex sha256；
// ZipBase64 ≤ max_zip_bytes（默认 50MiB，base64 后 ≈67MiB）。
type PackResponse struct {
	CommitSHA string `json:"commit_sha"`
	Checksum  string `json:"checksum"`
	ZipBase64 string `json:"zip_base64"`
}
