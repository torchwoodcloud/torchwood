package query

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// R11 共享 golden 语料（SDK 侧）：与根模块 pkg/query 的 golden_test.go 消费
// **同一份** ../../../pkg/query/testdata/dsl_ast_golden.json（Go 测试以包目录
// 为工作目录，跨模块相对路径读文件不受模块边界限制）。条目格式、中立 JSON
// 语义与分歧仲裁规则见根侧测试注释（goldenEntry/goldenOverride 逐字段同构）。
//
// 本侧中立形态映射差异：FromDSL 的 cursorAfter/Before 糖编码为 ka:/kb:
// page token，映射回 cursorAfter/cursorBefore；offset 按文档面 keyset-only
// 拒绝（语料以 sdk override 声明）。
type goldenEntry struct {
	DSL   []string        `json:"dsl"`
	AST   json.RawMessage `json:"ast"`
	Error string          `json:"error"`
	Root  *goldenOverride `json:"root"`
	SDK   *goldenOverride `json:"sdk"`
}

type goldenOverride struct {
	AST   json.RawMessage `json:"ast"`
	Error string          `json:"error"`
}

func (e goldenEntry) expectation(sdkSide bool) (json.RawMessage, string) {
	ov := e.Root
	if sdkSide {
		ov = e.SDK
	}
	if ov != nil {
		return ov.AST, ov.Error
	}
	return e.AST, e.Error
}

func loadGolden(t *testing.T) []goldenEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "query", "testdata", "dsl_ast_golden.json"))
	require.NoError(t, err, "共享 golden 语料不可读（与根模块 pkg/query 同源）")
	var entries []goldenEntry
	require.NoError(t, json.Unmarshal(b, &entries))
	require.NotEmpty(t, entries)
	return entries
}

// TestFromDSLGolden：SDK 侧消费共享语料（FromDSL → 中立 JSON / 错误子串）。
func TestFromDSLGolden(t *testing.T) {
	for i, e := range loadGolden(t) {
		name := fmt.Sprintf("%02d_%s", i, truncate(strings.Join(e.DSL, " ")))
		t.Run(name, func(t *testing.T) {
			expAST, expErr := e.expectation(true)
			got, err := FromDSL(e.DSL...)
			if expErr != "" {
				require.Error(t, err, "golden 期望错误 %q", expErr)
				require.Contains(t, err.Error(), expErr)
				return
			}
			require.NoError(t, err)
			assertNeutralEqual(t, expAST, sdkNeutral(got))
		})
	}
}

func truncate(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '"' || r == '\n' || r == '\t' {
			return '_'
		}
		return r
	}, s)
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

func assertNeutralEqual(t *testing.T, expected json.RawMessage, actual map[string]any) {
	t.Helper()
	exp := "{}"
	if len(expected) > 0 {
		exp = string(expected)
	}
	act, err := json.Marshal(actual)
	require.NoError(t, err)
	require.JSONEq(t, exp, string(act), "中立形态与 golden 不符：got %s", string(act))
}

// sdkNeutral：*sharedv1.Query → 中立 JSON（oneof 分支 → DSL 算子名；
// ka:/kb: page token → cursorAfter/cursorBefore）。
func sdkNeutral(q *sharedv1.Query) map[string]any {
	if q == nil {
		return map[string]any{}
	}
	m := map[string]any{}
	if q.GetFilter() != nil {
		m["filter"] = sdkNeutralFilter(q.GetFilter())
	}
	if orders := q.GetOrders(); len(orders) > 0 {
		list := make([]map[string]any, 0, len(orders))
		for _, o := range orders {
			om := map[string]any{"attribute": o.GetAttribute()}
			if o.GetDesc() {
				om["desc"] = true
			}
			list = append(list, om)
		}
		m["orders"] = list
	}
	if sel := q.GetSelect(); len(sel) > 0 {
		m["selects"] = append([]string{}, sel...)
	}
	if q.GetPageSize() != 0 {
		m["pageSize"] = q.GetPageSize()
	}
	// FromDSL 的 cursor 糖 ↔ 中立 cursor 字段（ka:/kb: 前缀还原）。
	switch tok := q.GetPageToken(); {
	case strings.HasPrefix(tok, "ka:"):
		m["cursorAfter"] = strings.TrimPrefix(tok, "ka:")
	case strings.HasPrefix(tok, "kb:"):
		m["cursorBefore"] = strings.TrimPrefix(tok, "kb:")
	case tok != "":
		m["pageToken"] = tok
	}
	return m
}

// sdkNeutralFilter：proto oneof 分支名 → DSL 算子名（与 pkg/query 常量同词）。
func sdkNeutralFilter(f *sharedv1.Filter) map[string]any {
	var op, attr string
	var values []string
	var children []*sharedv1.Filter
	switch e := f.GetExpr().(type) {
	case *sharedv1.Filter_Eq:
		op, attr, values = "equal", e.Eq.GetAttribute(), e.Eq.GetValues()
	case *sharedv1.Filter_Ne:
		op, attr, values = "notEqual", e.Ne.GetAttribute(), e.Ne.GetValues()
	case *sharedv1.Filter_Lt:
		op, attr, values = "lessThan", e.Lt.GetAttribute(), e.Lt.GetValues()
	case *sharedv1.Filter_Lte:
		op, attr, values = "lessThanEqual", e.Lte.GetAttribute(), e.Lte.GetValues()
	case *sharedv1.Filter_Gt:
		op, attr, values = "greaterThan", e.Gt.GetAttribute(), e.Gt.GetValues()
	case *sharedv1.Filter_Gte:
		op, attr, values = "greaterThanEqual", e.Gte.GetAttribute(), e.Gte.GetValues()
	case *sharedv1.Filter_In:
		op, attr, values = "in", e.In.GetAttribute(), e.In.GetValues()
	case *sharedv1.Filter_Contains:
		op, attr, values = "contains", e.Contains.GetAttribute(), e.Contains.GetValues()
	case *sharedv1.Filter_NotContains:
		op, attr, values = "notContains", e.NotContains.GetAttribute(), e.NotContains.GetValues()
	case *sharedv1.Filter_StartsWith:
		op, attr, values = "startsWith", e.StartsWith.GetAttribute(), e.StartsWith.GetValues()
	case *sharedv1.Filter_NotStartsWith:
		op, attr, values = "notStartsWith", e.NotStartsWith.GetAttribute(), e.NotStartsWith.GetValues()
	case *sharedv1.Filter_EndsWith:
		op, attr, values = "endsWith", e.EndsWith.GetAttribute(), e.EndsWith.GetValues()
	case *sharedv1.Filter_NotEndsWith:
		op, attr, values = "notEndsWith", e.NotEndsWith.GetAttribute(), e.NotEndsWith.GetValues()
	case *sharedv1.Filter_Search:
		op, attr, values = "search", e.Search.GetAttribute(), e.Search.GetValues()
	case *sharedv1.Filter_NotSearch:
		op, attr, values = "notSearch", e.NotSearch.GetAttribute(), e.NotSearch.GetValues()
	case *sharedv1.Filter_ContainsAny:
		op, attr, values = "containsAny", e.ContainsAny.GetAttribute(), e.ContainsAny.GetValues()
	case *sharedv1.Filter_ContainsAll:
		op, attr, values = "containsAll", e.ContainsAll.GetAttribute(), e.ContainsAll.GetValues()
	case *sharedv1.Filter_Between:
		op, attr, values = "between", e.Between.GetAttribute(), e.Between.GetValues()
	case *sharedv1.Filter_NotBetween:
		op, attr, values = "notBetween", e.NotBetween.GetAttribute(), e.NotBetween.GetValues()
	case *sharedv1.Filter_IsNull:
		op, attr = "isNull", e.IsNull.GetAttribute()
	case *sharedv1.Filter_IsNotNull:
		op, attr = "isNotNull", e.IsNotNull.GetAttribute()
	case *sharedv1.Filter_And:
		op, children = "and", e.And.GetFilters()
	case *sharedv1.Filter_Or:
		op, children = "or", e.Or.GetFilters()
	default:
		op = "unknown"
	}
	if children != nil {
		list := make([]map[string]any, 0, len(children))
		for _, c := range children {
			list = append(list, sdkNeutralFilter(c))
		}
		return map[string]any{"op": op, "children": list}
	}
	node := map[string]any{"op": op, "attribute": attr}
	if len(values) > 0 {
		node["values"] = append([]string{}, values...)
	}
	return node
}
