package functions

import (
	"fmt"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 运行时表单一事实源在 domain（internal/domain/functions/runtime.go，含
// Family/BaseImage/Status 全量记录；本包只做投影与校验，不再持有表副本
// ——消灭 ID↔基座镜像两处漂移，docs/design/functions-runtime-selection.md §1）。

// runtimeExists 判断 runtime ID 是否受支持。
func runtimeExists(id string) bool {
	_, ok := domainfunctions.RuntimeByID(id)
	return ok
}

// validateRuntimeSelectable 是运行时的「新构建入口」状态门（生命周期
// 状态机，functions-runtime-selection.md §6）：eol 拒绝（错误文案列出
// 可选 active runtime），deprecated 允许（过渡态，客户端按投影字段提示）。
// 仅约束新的构建入口（Create/UpdateFunction、新 CreateDeployment）；
// 存量 ready deployment 执行面与平台内部补构建不设门。
func validateRuntimeSelectable(id string) error {
	rec, ok := domainfunctions.RuntimeByID(id)
	if !ok {
		return status.Errorf(codes.InvalidArgument, "unsupported runtime %q", id)
	}
	if rec.Status == domainfunctions.RuntimeStatusEOL {
		return status.Errorf(codes.InvalidArgument, "runtime %q is end-of-life and no longer accepts new functions or deployments; available runtimes: %s",
			id, activeRuntimeIDs())
	}
	return nil
}

// activeRuntimeIDs 返回 active runtime ID 的逗号串（错误文案用）。
func activeRuntimeIDs() string {
	out := ""
	for _, r := range domainfunctions.Runtimes() {
		if r.Status == domainfunctions.RuntimeStatusActive {
			if out != "" {
				out += ", "
			}
			out += fmt.Sprintf("%q", r.ID)
		}
	}
	return out
}

// defaultEntrypoint 返回 runtime 的缺省 entrypoint（MVP 仅占位）。
func defaultEntrypoint(runtime string) string {
	if rec, ok := domainfunctions.RuntimeByID(runtime); ok {
		return rec.Entrypoint
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

var specifications = []domainfunctions.SpecificationInfo{
	{ID: "shared-1x", CPU: "0.5", Memory: "256m"},
	{ID: "shared-2x", CPU: "1", Memory: "512m"},
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

// ListRuntimes 返回受支持的运行时列表（domain 表投影，表序 = 展示序）。
func (f *Functions) ListRuntimes() []domainfunctions.RuntimeInfo {
	return domainfunctions.Runtimes()
}

// ListSpecifications 返回受支持的规格列表。
func (f *Functions) ListSpecifications() []domainfunctions.SpecificationInfo {
	out := make([]domainfunctions.SpecificationInfo, len(specifications))
	copy(out, specifications)
	return out
}
