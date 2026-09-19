package dispatcher

import (
	"fmt"
	"strconv"
	"strings"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// engines.node 范围与所选 runtime 的 major 相交判定
// （docs/design/functions-runtime-selection.md §5）：`package.json` 的
// engines.node 是 Node 生态运行时声明的正规位置（npm 自身消费它）——
// 本地/线上版本漂移在部署期显式化，与 D11「构建期不执行用户代码」正交
// （纯读取 + 比较，零新执行面）。校验点在 prepareBuildContext（与 D7'
// family 对账同处、先于模板渲染）。
//
// 实现为零依赖的 npm 语义子集（major 粒度相交性，非完整 semver 求值器）：
// 支持 `||` 或组、比较符 = >= > <= <、^ ~、x-range（22 / 22.x / 22.* /
// *）、连字符 range（a - b）；prerelease/构建元数据剥离忽略；无法解析的
// token 按放行处理（本检查是 DX 护栏不是安全边界，误拒伤部署、误放只是
// 退化回无 engines 的现状）。

// ver 是 (major, minor, patch) 三元组，按字典序比较。
type ver struct{ maj, min, pat int }

func verLess(a, b ver) bool {
	if a.maj != b.maj {
		return a.maj < b.maj
	}
	if a.min != b.min {
		return a.min < b.min
	}
	return a.pat < b.pat
}

func verEq(a, b ver) bool { return a == b }

// bound 是一个比较边界：方向（isUpper）+ 端点值 + 是否含端点。
type bound struct {
	v         ver
	inclusive bool
	isUpper   bool
}

// partialVer 是解析产物的部分版本：v 为已知位补零的三元组，parts 是声明
// 的位数（1 = "22"、2 = "22.3"、3 = 完整），partial = 位数 < 3。
type partialVer struct {
	v       ver
	parts   int
	partial bool
}

// nextPartial 把部分版本上取整到下一个版本（npm 规范：`22` 的开上界 =
// 23.0.0，`22.3` 的开上界 = 22.4.0）。
func (p partialVer) nextPartial() ver {
	if p.parts == 1 {
		return ver{p.v.maj + 1, 0, 0}
	}
	return ver{p.v.maj, p.v.min + 1, 0}
}

// checkNodeEngines 对 node family 部署执行 engines.node 校验：声明 range
// 与 runtime major 的版本空间不相交 → InvalidArgument，错误文案列出可选
// node runtime。enginesNode 为空（未声明）或非 node family 时零作用。
func checkNodeEngines(enginesNode, renderRuntime string) error {
	if enginesNode == "" {
		return nil
	}
	major, ok := domainfunctions.RuntimeMajor(renderRuntime)
	if !ok {
		return nil // 非 node family：engines 是 node 生态声明位，不消费
	}
	if nodeEnginesAllowsMajor(enginesNode, major) {
		return nil
	}
	available := ""
	for _, r := range domainfunctions.Runtimes() {
		if r.Family == domainfunctions.RuntimeFamilyNode && r.Status == domainfunctions.RuntimeStatusActive {
			if available != "" {
				available += ", "
			}
			available += fmt.Sprintf("%q", r.ID)
		}
	}
	return status.Errorf(codes.InvalidArgument,
		"package.json engines.node %q excludes runtime %q (Node.js %d); redeploy with a matching runtime: %s",
		enginesNode, renderRuntime, major, available)
}

// nodeEnginesAllowsMajor 报告 range 是否与 major M 的版本空间
// [M.0.0, (M+1).0.0) 相交（按 `||` 或组任一命中即放行）。
func nodeEnginesAllowsMajor(engines string, major int) bool {
	for _, alt := range strings.Split(engines, "||") {
		bounds, ok := parseNodeRange(alt)
		if !ok {
			return true // 无法解析的 alternative：放行（护栏不是安全边界）
		}
		if rangeAllowsMajor(bounds, major) {
			return true
		}
	}
	return false
}

// rangeAllowsMajor 判定单个 AND 组的边界集与 [M.0.0, (M+1).0.0) 是否相交：
// 区间交集非空（lower < upper，或相等且双侧含端点）。
func rangeAllowsMajor(bounds []bound, major int) bool {
	lower := ver{major, 0, 0}
	upper := ver{major + 1, 0, 0}
	lowerIncl, upperIncl := true, false // 基准空间 [M.0.0, (M+1).0.0)
	for _, b := range bounds {
		if !b.isUpper {
			// 取更严的下界：值更大者；同值时不含端点更严。
			if verLess(lower, b.v) || (verEq(lower, b.v) && !b.inclusive) {
				lower, lowerIncl = b.v, b.inclusive
			}
		} else {
			// 取更严的上界：值更小者；同值时不含端点更严。
			if verLess(b.v, upper) || (verEq(b.v, upper) && !b.inclusive) {
				upper, upperIncl = b.v, b.inclusive
			}
		}
	}
	if verLess(lower, upper) {
		return true
	}
	return verEq(lower, upper) && lowerIncl && upperIncl
}

// parseNodeRange 解析单个 AND 组（空白分隔的 token 序列，含连字符 range
// `a - b`）为边界集；无法解析的 token 返回 ok=false（调用方放行）。
func parseNodeRange(rng string) ([]bound, bool) {
	tokens := strings.Fields(strings.TrimSpace(rng))
	if len(tokens) == 0 {
		return nil, true // 空 alternative（如尾随 "||"）：放行
	}
	var bounds []bound
	for i := 0; i < len(tokens); {
		if tokens[i] == "-" {
			return nil, false // 缺左端点
		}
		// 连字符 range：`1.2.3 - 2.3`（npm 语义：右端点按部分版本上取整
		// 为开区间，完整版本为闭区间）。先看 ahead 再解析——左端点是裸
		// 版本，比较符 token（如 `>=18`）不走本分支。
		if i+2 < len(tokens) && tokens[i+1] == "-" {
			lo, loOK := parseVer(tokens[i])
			hi, hiOK := parseVer(tokens[i+2])
			if !loOK || !hiOK {
				return nil, false
			}
			bounds = append(bounds, bound{v: lo.v, inclusive: true})
			if hi.partial {
				bounds = append(bounds, bound{v: hi.nextPartial(), inclusive: false, isUpper: true})
			} else {
				bounds = append(bounds, bound{v: hi.v, inclusive: true, isUpper: true})
			}
			i += 3
			continue
		}
		bs, ok := parseComparator(tokens[i])
		if !ok {
			return nil, false
		}
		bounds = append(bounds, bs...)
		i++
	}
	return bounds, true
}

// parseComparator 解析单个比较 token 为边界集；无边界（`*`）返回
// (nil, true)；无法解析返回 (_, false)。
func parseComparator(tok string) ([]bound, bool) {
	switch {
	case strings.HasPrefix(tok, ">="):
		p, ok := parseVer(strings.TrimPrefix(tok, ">="))
		if !ok {
			return nil, false
		}
		return []bound{{v: p.v, inclusive: true}}, true
	case strings.HasPrefix(tok, ">"):
		p, ok := parseVer(strings.TrimPrefix(tok, ">"))
		if !ok {
			return nil, false
		}
		if p.partial {
			// >22（bare major）≡ >=23.0.0（部分版本下取整为闭区间）。
			return []bound{{v: p.nextPartial(), inclusive: true}}, true
		}
		return []bound{{v: p.v, inclusive: false}}, true
	case strings.HasPrefix(tok, "<="):
		p, ok := parseVer(strings.TrimPrefix(tok, "<="))
		if !ok {
			return nil, false
		}
		if p.partial {
			// <=22 ≡ <23.0.0（部分版本上取整为开区间）。
			return []bound{{v: p.nextPartial(), inclusive: false, isUpper: true}}, true
		}
		return []bound{{v: p.v, inclusive: true, isUpper: true}}, true
	case strings.HasPrefix(tok, "<"):
		p, ok := parseVer(strings.TrimPrefix(tok, "<"))
		if !ok {
			return nil, false
		}
		return []bound{{v: p.v, inclusive: false, isUpper: true}}, true
	case tok == "*" || tok == "x" || tok == "X":
		return nil, true
	case strings.HasPrefix(tok, "^"), strings.HasPrefix(tok, "~"):
		p, ok := parseVer(tok[1:])
		if !ok {
			return nil, false
		}
		return caretOrTilde(p, tok[0] == '^'), true
	case strings.HasPrefix(tok, "!="):
		// 排除点破坏区间模型：保守放行该 token（护栏非安全边界）。
		return nil, true
	case strings.HasPrefix(tok, "="), strings.HasPrefix(tok, "v"):
		p, ok := parseVer(strings.TrimPrefix(strings.TrimPrefix(tok, "="), "v"))
		if !ok {
			return nil, false
		}
		return xRange(p), true
	default:
		p, ok := parseVer(tok)
		if !ok {
			return nil, false
		}
		return xRange(p), true
	}
}

// caretOrTilde 展开 ^ / ~ 语义（npm 规范）：^ 锚定最左非零位，~ 有 minor
// 时锚定 minor；bare major 形态（^22 / ~22）= x-range [22.0.0, 23.0.0)。
func caretOrTilde(p partialVer, caret bool) []bound {
	if p.partial && p.parts == 1 {
		return xRange(p)
	}
	upper := bound{inclusive: false, isUpper: true}
	switch {
	case caret && p.v.maj > 0:
		upper.v = ver{p.v.maj + 1, 0, 0}
	case caret && p.v.min > 0:
		upper.v = ver{p.v.maj, p.v.min + 1, 0}
	case caret:
		upper.v = ver{p.v.maj, p.v.min, p.v.pat + 1}
	case p.parts >= 2:
		upper.v = ver{p.v.maj, p.v.min + 1, 0}
	default:
		upper.v = ver{p.v.maj + 1, 0, 0}
	}
	return []bound{{v: p.v, inclusive: true}, upper}
}

// xRange 把部分版本转为 [下界, 上界) 两个边界：22 → [22.0.0, 23.0.0)；
// 22.3 → [22.3.0, 22.4.0)；完整版本 → 单点闭区间（下界+上界同值含端点）。
func xRange(p partialVer) []bound {
	if !p.partial {
		return []bound{
			{v: p.v, inclusive: true},
			{v: p.v, inclusive: true, isUpper: true},
		}
	}
	return []bound{
		{v: p.v, inclusive: true},
		{v: p.nextPartial(), inclusive: false, isUpper: true},
	}
}

// parseVer 解析版本 token 为 partialVer：剥离前缀 `v`、prerelease/构建
// 元数据（首个 `-` / `+` 之后），`.` 分段取 1-3 段数值；含 `*`/`x`/`X`
// 段按已声明位数截断；无数值段 → ok=false。
func parseVer(tok string) (partialVer, bool) {
	s := tok
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	maj, min, pat := 0, 0, 0
	parts := 0
	for i, seg := range strings.Split(s, ".") {
		if i >= 3 {
			break
		}
		if seg == "*" || seg == "x" || seg == "X" || seg == "" {
			break
		}
		n, err := strconv.Atoi(seg)
		if err != nil {
			return partialVer{}, false
		}
		switch i {
		case 0:
			maj = n
		case 1:
			min = n
		case 2:
			pat = n
		}
		parts = i + 1
	}
	if parts == 0 {
		return partialVer{}, false
	}
	return partialVer{v: ver{maj, min, pat}, parts: parts, partial: parts < 3}, true
}
