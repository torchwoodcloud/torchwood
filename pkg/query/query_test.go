package query

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		expected Query
	}{
		{
			name: "equal string",
			raw:  `equal("email","a@b.com")`,
			expected: Query{
				Filters: []Filter{{Op: "equal", Attribute: "email", Values: []string{"a@b.com"}}},
			},
		},
		{
			name: "equal array",
			raw:  `equal("status",["active","pending"])`,
			expected: Query{
				Filters: []Filter{{Op: "equal", Attribute: "status", Values: []string{"active", "pending"}}},
			},
		},
		{
			name: "between",
			raw:  `between("age",18,65)`,
			expected: Query{
				Filters: []Filter{{Op: "between", Attribute: "age", Values: []string{"18", "65"}}},
			},
		},
		{
			name: "in array",
			raw:  `in("status",["draft","published"])`,
			expected: Query{
				Filters: []Filter{{Op: "in", Attribute: "status", Values: []string{"draft", "published"}}},
			},
		},
		{
			name: "order desc",
			raw:  `orderDesc("createdAt")`,
			expected: Query{
				Orders: []Order{{Attribute: "createdAt", Desc: true}},
			},
		},
		{
			name: "limit",
			raw:  `limit(25)`,
			expected: Query{
				Limit: 25,
			},
		},
		{
			name: "select",
			raw:  `select(["name","email"])`,
			expected: Query{
				Selects: []string{"name", "email"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.raw)
			require.NoError(t, err)
			require.Equal(t, tc.expected.Filters, q.Filters)
			require.Equal(t, tc.expected.Orders, q.Orders)
			require.Equal(t, tc.expected.Selects, q.Selects)
			require.Equal(t, tc.expected.Limit, q.Limit)
			if tc.name == "limit" {
				require.Equal(t, int32(25), q.PageSize)
			}
			if len(tc.expected.Filters) == 1 {
				require.NotNil(t, q.Filter)
				require.Equal(t, tc.expected.Filters[0], *q.Filter)
			}
		})
	}
}

func TestParseMany(t *testing.T) {
	q, err := ParseMany([]string{
		`equal("status","active")`,
		`greaterThan("age",18)`,
		`orderDesc("createdAt")`,
		`limit(10)`,
		`offset(20)`,
	})
	require.NoError(t, err)
	require.Len(t, q.Filters, 2)
	require.Len(t, q.Orders, 1)
	require.Equal(t, 10, q.Limit)
	require.Equal(t, int32(10), q.PageSize)
	require.Equal(t, 20, q.Offset)
	require.NotNil(t, q.Filter)
	require.Equal(t, OpAnd, q.Filter.Op)
	require.Len(t, q.Filter.Children, 2)
}

// TestParseMany_NoDefaultLimit (F3-2)：未显式指定 limit 时 Limit 保持 0，
// 默认页大小交由 adapter 决定（ListDocuments 用 PageSize 回退）。
func TestParseMany_NoDefaultLimit(t *testing.T) {
	q, err := ParseMany([]string{`equal("status","active")`})
	require.NoError(t, err)
	require.Equal(t, 0, q.Limit)

	q, err = ParseMany(nil)
	require.NoError(t, err)
	require.Equal(t, 0, q.Limit)

	q, err = ParseMany([]string{`limit(0)`})
	require.NoError(t, err)
	require.Equal(t, 0, q.Limit)
}

// TestParse_NegativeLimitOffset (A1): 解析期 fail-fast，负数 limit/offset 直接报错，
// 防止透传到 PG 变成 LIMIT -1（全表返回）。
func TestParse_NegativeLimitOffset(t *testing.T) {
	for _, raw := range []string{`limit(-1)`, `limit(-100)`, `offset(-1)`, `offset(-999)`} {
		t.Run(raw, func(t *testing.T) {
			_, err := Parse(raw)
			require.Error(t, err)
		})
	}
	// 正值不受影响。
	q, err := Parse(`limit(0)`)
	require.NoError(t, err)
	require.Equal(t, 0, q.Limit)
	q, err = Parse(`offset(0)`)
	require.NoError(t, err)
	require.Equal(t, 0, q.Offset)
}

func TestParseMany_InputLimits(t *testing.T) {
	tooMany := make([]string, MaxQueries+1)
	for i := range tooMany {
		tooMany[i] = `limit(1)`
	}
	_, err := ParseMany(tooMany)
	require.Error(t, err)

	long := `equal("title","` + strings.Repeat("a", MaxQueryLen) + `")`
	_, err = ParseMany([]string{long})
	require.Error(t, err)
}

func TestParse_EmptyEqualArray(t *testing.T) {
	_, err := Parse(`equal("title",[])`)
	require.Error(t, err)
}

// TestParse_In：in 算子要求数组值（Appwrite 语义），非数组/空数组拒绝。
func TestParse_In(t *testing.T) {
	_, err := Parse(`in("status","draft")`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "array")

	_, err = Parse(`in("status",[])`)
	require.Error(t, err)

	_, err = Parse(`in("status")`)
	require.Error(t, err)
}

func TestValidate_EmptyGreaterThan(t *testing.T) {
	q := &Query{Filter: &Filter{Op: OpGreaterThan, Attribute: "n"}}
	require.Error(t, q.Validate())
}

// TestParse_NotVariants（C7 预决策 1）：not* 变体族在客户端语法糖层可解析
// （SDK 解析为 AST 后发送；服务端零 DSL 消费）。
func TestParse_NotVariants(t *testing.T) {
	cases := []struct {
		raw    string
		op     string
		values []string
	}{
		{raw: `notContains("name","jo")`, op: OpNotContains, values: []string{"jo"}},
		{raw: `notStartsWith("name","jo")`, op: OpNotStartsWith, values: []string{"jo"}},
		{raw: `notEndsWith("name","jo")`, op: OpNotEndsWith, values: []string{"jo"}},
		{raw: `notSearch("body","hello")`, op: OpNotSearch, values: []string{"hello"}},
		{raw: `notBetween("age",18,65)`, op: OpNotBetween, values: []string{"18", "65"}},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			q, err := Parse(tc.raw)
			require.NoError(t, err)
			require.Len(t, q.Filters, 1)
			require.Equal(t, tc.op, q.Filters[0].Op)
			require.Equal(t, tc.values, q.Filters[0].Values)
		})
	}

	// notBetween 参数数错误 → 解析期拒绝。
	_, err := Parse(`notBetween("age",18)`)
	require.Error(t, err)
}

// TestConstructors：程序化构造器产出与手写等价的 AST 节点。
func TestConstructors(t *testing.T) {
	require.Equal(t, &Filter{Op: OpEqual, Attribute: "a", Values: []string{"x"}}, Eq("a", "x"))
	require.Equal(t, &Filter{Op: OpBetween, Attribute: "n", Values: []string{"1", "9"}}, Between("n", "1", "9"))
	require.Equal(t, &Filter{Op: OpIsNull, Attribute: "d"}, IsNull("d"))

	// And/Or：单子节点坍缩、空入参 nil、多子节点组树。
	a, b := Eq("a", "1"), Eq("b", "2")
	require.Equal(t, a, And(a))
	require.Equal(t, a, Or(a))
	require.Nil(t, And())
	require.Nil(t, Or())
	require.Equal(t, &Filter{Op: OpAnd, Children: []*Filter{a, b}}, And(a, b))
	require.Equal(t, &Filter{Op: OpOr, Children: []*Filter{a, b}}, Or(a, b))
	// nil 子节点被跳过（坍缩同单子节点）。
	require.Equal(t, a, And(nil, a))
}
