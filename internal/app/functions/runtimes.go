package functions

import domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"

// runtimes 静态运行时表：node-18.0 → node:18-alpine（入口 index.js 的
// main）；go-1.26 → golang:1.26-alpine 多阶段构建 + 平台生成 twmain
// bootstrap（Go 一期，设计 docs/design/functions-runtimes-and-sources.md
// §1；基础镜像 tag 与 runtime ID 同步定稿，升级 = 新 runtime ID、旧 ID 不
// 日落——go1.x 废弃教训）。entrypoint 字段 MVP 仅占位，执行入口固定（见
// 实现方案 §8）。python-3.11 已随 v1 docker 执行器移除（常驻执行器
// node-only，python runner 未实现）；历史 python 函数再次部署将在构建期
// 明确报错。image 为 BYO 镜像专用 runtime（三期阶段 1，设计 §3）：只接受
// ImageSource 部署（源/运行时互斥，§0/D7），平台零构建——entrypoint 无意义
// 占位（入口契约由镜像自身 :18080 runner 协议承载）。
var runtimes = []domainfunctions.RuntimeInfo{
	{ID: "node-18.0", Name: "Node.js 18", Entrypoint: "index.main"},
	{ID: "go-1.26", Name: "Go 1.26", Entrypoint: "Main"},
	{ID: "image", Name: "Bring your own image", Entrypoint: "-"},
}

var specifications = []domainfunctions.SpecificationInfo{
	{ID: "shared-1x", CPU: "0.5", Memory: "256m"},
	{ID: "shared-2x", CPU: "1", Memory: "512m"},
}

// runtimeExists 判断 runtime ID 是否受支持。
func runtimeExists(id string) bool {
	for _, r := range runtimes {
		if r.ID == id {
			return true
		}
	}
	return false
}

// defaultEntrypoint 返回 runtime 的缺省 entrypoint（MVP 仅占位）。
func defaultEntrypoint(runtime string) string {
	for _, r := range runtimes {
		if r.ID == runtime {
			return r.Entrypoint
		}
	}
	return "index.main"
}

// specificationExists 判断 spec ID 是否受支持。
func specificationExists(id string) bool {
	for _, s := range specifications {
		if s.ID == id {
			return true
		}
	}
	return false
}

// specification 返回 spec 的 CPU/Memory 值；不存在时返回零值.

//nolint:unused
func specification(id string) domainfunctions.SpecificationInfo {
	for _, s := range specifications {
		if s.ID == id {
			return s
		}
	}
	return domainfunctions.SpecificationInfo{}
}

// ListRuntimes 返回受支持的运行时列表。
func (f *Functions) ListRuntimes() []domainfunctions.RuntimeInfo {
	out := make([]domainfunctions.RuntimeInfo, len(runtimes))
	copy(out, runtimes)
	return out
}

// ListSpecifications 返回受支持的规格列表。
func (f *Functions) ListSpecifications() []domainfunctions.SpecificationInfo {
	out := make([]domainfunctions.SpecificationInfo, len(specifications))
	copy(out, specifications)
	return out
}
