package functions

import (
	"strconv"
	"strings"
	"time"
)

// 函数运行时表（单一事实源）与语言族/生命周期词表。
//
// 本表是「指定运行时」机制的核心（docs/design/functions-runtime-selection.md）：
// app（ListRuntimes 投影与状态门）、runner（DockerfileFor 基座镜像渲染）、
// dispatcher（探测 family 对账）三方只读消费——新增 runtime = 本表加一行
// + 一条 golden 测试，不存在第二处 ID→基座镜像映射。
//
// 版本轴语义（Lambda nodejs20.x 同款）：同 runtime ID 内 minor/patch 由平台
// 滚动升级（换 BaseImage digest，ID 不变、模板结构不变不 bump
// RunnerTemplateVersion）；major 升级 = 新 runtime ID、旧 ID 不日落——旧 ID
// 经 Status 状态机收敛（active → deprecated → eol），eol 只关闭新的构建
// 入口（app 层状态门），存量 ready deployment 执行面不受影响。
//
// ID 语法沿既有 <lang>-<major>.0：.0 是平台管理的 minor/patch 槽位；go 因
// 自身版本形态即 1.26，无槽位后缀，下一个是 go-1.27。表序 = 展示序（新→旧）。

// 运行时语言族词表（探测对账轴，D7 修订——探测产出 family，版本轴来自声明）。
const (
	RuntimeFamilyNode   = "node"
	RuntimeFamilyGo     = "go"
	RuntimeFamilyImage  = "image"
	RuntimeFamilyPython = "python"
)

// 运行时生命周期状态词表（functions-runtime-selection.md §6）。
const (
	RuntimeStatusActive     = "active"
	RuntimeStatusDeprecated = "deprecated"
	RuntimeStatusEOL        = "eol"
)

// RuntimeInfo 描述一个受支持的函数运行时。
type RuntimeInfo struct {
	ID         string // e.g. node-24.0
	Name       string // 展示名
	Entrypoint string // e.g. "index.main"（MVP 占位）
	// Family 是语言族（RuntimeFamily* 词表）：探测对账的比对轴——zip 探测
	// 产出 family，版本轴来自声明的 runtime ID。
	Family string
	// BaseImage 是构建模板基座镜像（tag 形态；patch 滚动 = 换 digest 的 PR）：
	// node family 渲染为 FROM；go family 渲染为多阶段构建段（运行段
	// alpine + ca-certificates 为模板内部细节，不进表）。image family
	// 平台零构建，恒空。
	BaseImage string
	// Status 是生命周期状态（RuntimeStatus* 词表）：eol 拒绝新建函数/新
	// 部署（app 层状态门），deprecated 可用但客户端应提示。
	Status string
	// EolAt 是上游 EOL 日期（如 Node.js 官方时间表）；active 恒 nil。
	EolAt *time.Time
	// IsDefault 标记新建函数的缺省 runtime（node 家族内有且仅有一个）。
	IsDefault bool
}

// runtimes 静态运行时表：node-24.0/node-22.0（active）、node-18.0（eol，
// 2025-04-30 上游 EOL——可跑、拒新建）、go-1.26、image（BYO 镜像专用，
// 源/运行时互斥见 functions-runtimes-and-sources.md §0/D7）。
var runtimes = []RuntimeInfo{
	{ID: "node-24.0", Name: "Node.js 24", Entrypoint: "index.main", Family: RuntimeFamilyNode, BaseImage: "node:24-alpine", Status: RuntimeStatusActive, IsDefault: true},
	{ID: "node-22.0", Name: "Node.js 22", Entrypoint: "index.main", Family: RuntimeFamilyNode, BaseImage: "node:22-alpine", Status: RuntimeStatusActive},
	{ID: "node-18.0", Name: "Node.js 18", Entrypoint: "index.main", Family: RuntimeFamilyNode, BaseImage: "node:18-alpine", Status: RuntimeStatusEOL, EolAt: utcDate(2025, time.April, 30)},
	{ID: "go-1.26", Name: "Go 1.26", Entrypoint: "main", Family: RuntimeFamilyGo, BaseImage: "golang:1.26-alpine", Status: RuntimeStatusActive},
	{ID: "image", Name: "Bring your own image", Entrypoint: "-", Family: RuntimeFamilyImage, Status: RuntimeStatusActive},
}

func utcDate(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &t
}

// Runtimes 返回运行时表的副本（表序 = 展示序，新→旧）。
func Runtimes() []RuntimeInfo {
	out := make([]RuntimeInfo, len(runtimes))
	copy(out, runtimes)
	return out
}

// RuntimeByID 按 ID 查表；不存在时 ok=false（调用方 fail-closed）。
func RuntimeByID(id string) (RuntimeInfo, bool) {
	for _, r := range runtimes {
		if r.ID == id {
			return r, true
		}
	}
	return RuntimeInfo{}, false
}

// FamilyOf 返回 runtime ID 的语言族；未知 ID 返回空串。
func FamilyOf(id string) string {
	if r, ok := RuntimeByID(id); ok {
		return r.Family
	}
	return ""
}

// RuntimeMajor 解析 node family runtime ID 的 major 版本号（node-22.0 →
// 22；engines.node 校验的消费方）。非 node family / ID 形态不符返回
// ok=false。
func RuntimeMajor(id string) (int, bool) {
	r, ok := RuntimeByID(id)
	if !ok || r.Family != RuntimeFamilyNode {
		return 0, false
	}
	rest, found := strings.CutPrefix(r.ID, "node-")
	if !found {
		return 0, false
	}
	majorStr, _, _ := strings.Cut(rest, ".")
	major, err := strconv.Atoi(majorStr)
	if err != nil || major < 0 {
		return 0, false
	}
	return major, true
}

// DefaultRuntimeForFamily 返回该语言族表序首个 active 表项（探测产出
// family 而调用方未携带声明的遗留兼容路径的渲染基准；家族内无 active
// 表项时 ok=false——如 python）。
func DefaultRuntimeForFamily(family string) (RuntimeInfo, bool) {
	for _, r := range runtimes {
		if r.Family == family && r.Status == RuntimeStatusActive {
			return r, true
		}
	}
	return RuntimeInfo{}, false
}

// SpecificationInfo 描述一个受支持的资源规格。
type SpecificationInfo struct {
	ID     string // e.g. shared-1x
	CPU    string // docker --cpus 值
	Memory string // docker --memory 值
}
