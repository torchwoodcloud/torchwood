package functions

import domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"

// runtimes 静态运行时表：node-18.0 → node:18-alpine（入口 index.js 的 main）；
// python-3.11 → python:3.11-alpine（入口 main.py 的 main）。entrypoint 字段 MVP
// 仅占位，执行入口固定（见实现方案 §8）。
var runtimes = []domainfunctions.RuntimeInfo{
	{ID: "node-18.0", Name: "Node.js 18", Entrypoint: "index.main"},
	{ID: "python-3.11", Name: "Python 3.11", Entrypoint: "main.main"},
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
