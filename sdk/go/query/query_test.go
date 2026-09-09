package query

import (
	"testing"

	"github.com/stretchr/testify/require"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// TestBuilders_ConstructFilterArms：每个构造器产出正确的 oneof 分支与值。
func TestBuilders_ConstructFilterArms(t *testing.T) {
	cases := []struct {
		name   string
		got    *sharedv1.Filter
		attr   string
		values []string
	}{
		{"eq", Eq("a", "b"), "a", []string{"b"}},
		{"eq multi", Eq("a", "x", "y"), "a", []string{"x", "y"}},
		{"ne", Ne("a", "b"), "a", []string{"b"}},
		{"lt", Lt("a", "5"), "a", []string{"5"}},
		{"lte", Lte("a", "5"), "a", []string{"5"}},
		{"gt", Gt("a", "5"), "a", []string{"5"}},
		{"gte", Gte("a", "5"), "a", []string{"5"}},
		{"in", In("a", "x", "y"), "a", []string{"x", "y"}},
		{"contains", Contains("a", "jo"), "a", []string{"jo"}},
		{"notContains", NotContains("a", "jo"), "a", []string{"jo"}},
		{"startsWith", StartsWith("a", "jo"), "a", []string{"jo"}},
		{"notStartsWith", NotStartsWith("a", "jo"), "a", []string{"jo"}},
		{"endsWith", EndsWith("a", "jo"), "a", []string{"jo"}},
		{"notEndsWith", NotEndsWith("a", "jo"), "a", []string{"jo"}},
		{"search", Search("a", "hi"), "a", []string{"hi"}},
		{"notSearch", NotSearch("a", "hi"), "a", []string{"hi"}},
		{"between", Between("a", "1", "9"), "a", []string{"1", "9"}},
		{"notBetween", NotBetween("a", "1", "9"), "a", []string{"1", "9"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.got.GetEq()
			if c == nil {
				// 非 eq 分支：从任一 getter 取 Comparison。
				c = firstComparison(t, tc.got)
			}
			require.Equal(t, tc.attr, c.GetAttribute())
			require.Equal(t, tc.values, c.GetValues())
		})
	}

	isNull := IsNull("a")
	require.NotNil(t, isNull.GetIsNull())
	require.Empty(t, isNull.GetIsNull().GetValues())
	isNotNull := IsNotNull("a")
	require.NotNil(t, isNotNull.GetIsNotNull())

	// And/Or：坍缩与组树。
	a, b := Eq("a", "1"), Eq("b", "2")
	require.Equal(t, a, And(a))
	require.Nil(t, And())
	require.Len(t, And(a, b).GetAnd().GetFilters(), 2)
	require.Len(t, Or(a, b).GetOr().GetFilters(), 2)
	require.Equal(t, a, And(nil, a))
}

func firstComparison(t *testing.T, f *sharedv1.Filter) *sharedv1.Comparison {
	t.Helper()
	switch e := f.GetExpr().(type) {
	case *sharedv1.Filter_Ne:
		return e.Ne
	case *sharedv1.Filter_Lt:
		return e.Lt
	case *sharedv1.Filter_Lte:
		return e.Lte
	case *sharedv1.Filter_Gt:
		return e.Gt
	case *sharedv1.Filter_Gte:
		return e.Gte
	case *sharedv1.Filter_In:
		return e.In
	case *sharedv1.Filter_Contains:
		return e.Contains
	case *sharedv1.Filter_NotContains:
		return e.NotContains
	case *sharedv1.Filter_StartsWith:
		return e.StartsWith
	case *sharedv1.Filter_NotStartsWith:
		return e.NotStartsWith
	case *sharedv1.Filter_EndsWith:
		return e.EndsWith
	case *sharedv1.Filter_NotEndsWith:
		return e.NotEndsWith
	case *sharedv1.Filter_Search:
		return e.Search
	case *sharedv1.Filter_NotSearch:
		return e.NotSearch
	case *sharedv1.Filter_Between:
		return e.Between
	case *sharedv1.Filter_NotBetween:
		return e.NotBetween
	}
	t.Fatalf("unexpected expr %v", f.GetExpr())
	return nil
}

// TestBuilder_Chain：链式构造 Query 的各字段。
func TestBuilder_Chain(t *testing.T) {
	q := New().
		Filter(And(Eq("status", "open"), Gt("priority", "2"))).
		OrderDesc("$createdAt").
		OrderAsc("title").
		Select("$id", "title").
		PageSize(25).
		PageToken("ka:doc-9").
		Build()
	require.Len(t, q.GetFilter().GetAnd().GetFilters(), 2)
	require.Len(t, q.GetOrders(), 2)
	require.True(t, q.GetOrders()[0].GetDesc())
	require.False(t, q.GetOrders()[1].GetDesc())
	require.Equal(t, []string{"$id", "title"}, q.GetSelect())
	require.Equal(t, int32(25), q.GetPageSize())
	require.Equal(t, "ka:doc-9", q.GetPageToken())

	// VectorSearch 构造器（B7）：ef_search 缺省不设位（presence 语义——服务端
	// 维持 pgvector 缺省 40），EfSearch 设置后透传。
	defaultVS := VectorSearch("emb", 1, 0, 0).Build()
	require.Nil(t, defaultVS.EfSearch, "default builder must not emit ef_search")
	require.Nil(t, defaultVS.MaxDistance)
	tuned := VectorSearch("emb", 1, 0, 0).
		MetricL2().
		MaxDistance(0.5).
		EfSearch(200).
		Build()
	require.NotNil(t, tuned.EfSearch)
	require.Equal(t, int32(200), tuned.GetEfSearch())
	require.NotNil(t, tuned.GetMaxDistance())
	require.Equal(t, float64(0.5), tuned.GetMaxDistance())
	require.Equal(t, sharedv1.DistanceMetric_DISTANCE_METRIC_L2, tuned.GetMetric())
}

// TestFromDSL_EveryOperator：DSL sugar 覆盖全部算子（文法与 pkg/query 同源）。
func TestFromDSL_EveryOperator(t *testing.T) {
	cases := []struct {
		dsl  string
		attr string
		vals []string
	}{
		{`equal("a","b")`, "a", []string{"b"}},
		{`equal("a",["x","y"])`, "a", []string{"x", "y"}},
		{`notEqual("a","b")`, "a", []string{"b"}},
		{`lessThan("a",5)`, "a", []string{"5"}},
		{`lessThanEqual("a",5)`, "a", []string{"5"}},
		{`greaterThan("a",5)`, "a", []string{"5"}},
		{`greaterThanEqual("a",5)`, "a", []string{"5"}},
		{`in("a",["x","y"])`, "a", []string{"x", "y"}},
		{`contains("a","jo")`, "a", []string{"jo"}},
		{`notContains("a","jo")`, "a", []string{"jo"}},
		{`startsWith("a","jo")`, "a", []string{"jo"}},
		{`notStartsWith("a","jo")`, "a", []string{"jo"}},
		{`endsWith("a","jo")`, "a", []string{"jo"}},
		{`notEndsWith("a","jo")`, "a", []string{"jo"}},
		{`search("a","hi")`, "a", []string{"hi"}},
		{`notSearch("a","hi")`, "a", []string{"hi"}},
		{`between("a",1,9)`, "a", []string{"1", "9"}},
		{`notBetween("a",1,9)`, "a", []string{"1", "9"}},
	}
	for _, tc := range cases {
		t.Run(tc.dsl, func(t *testing.T) {
			q, err := FromDSL(tc.dsl)
			require.NoError(t, err)
			c := q.GetFilter().GetEq()
			if c == nil {
				c = firstComparison(t, q.GetFilter())
			}
			require.Equal(t, tc.attr, c.GetAttribute())
			require.Equal(t, tc.vals, c.GetValues())
		})
	}

	// isNull/isNotNull。
	q, err := FromDSL(`isNull("a")`)
	require.NoError(t, err)
	require.NotNil(t, q.GetFilter().GetIsNull())
	q, err = FromDSL(`isNotNull("a")`)
	require.NoError(t, err)
	require.NotNil(t, q.GetFilter().GetIsNotNull())
}

// TestFromDSL_MergeAndOrdersPaging：多串隐式 AND 合并 + 排序/分页/投影。
func TestFromDSL_MergeAndOrdersPaging(t *testing.T) {
	q, err := FromDSL(
		`equal("status","open")`,
		`greaterThan("priority",2)`,
		`orderDesc("$createdAt")`,
		`limit(25)`,
		`select(["$id","title"])`,
	)
	require.NoError(t, err)
	require.Len(t, q.GetFilter().GetAnd().GetFilters(), 2)
	require.Len(t, q.GetOrders(), 1)
	require.Equal(t, "$createdAt", q.GetOrders()[0].GetAttribute())
	require.Equal(t, int32(25), q.GetPageSize())
	require.Equal(t, []string{"$id", "title"}, q.GetSelect())

	// cursorAfter/Before → keyset page token。
	q, err = FromDSL(`cursorAfter("doc-1")`)
	require.NoError(t, err)
	require.Equal(t, "ka:doc-1", q.GetPageToken())
	q, err = FromDSL(`cursorBefore("doc-1")`)
	require.NoError(t, err)
	require.Equal(t, "kb:doc-1", q.GetPageToken())

	// offset 显式拒绝（keyset-only）。
	_, err = FromDSL(`offset(10)`)
	require.ErrorContains(t, err, "cursor")

	// 转义：引号与反斜杠穿透。
	q, err = FromDSL(`equal("title","a \"quoted\" value")`)
	require.NoError(t, err)
	require.Equal(t, []string{`a "quoted" value`}, q.GetFilter().GetEq().GetValues())

	// 非法输入。
	_, err = FromDSL(`bogus("a")`)
	require.ErrorContains(t, err, "unsupported query operator")
	_, err = FromDSL(`not-a-query`)
	require.ErrorContains(t, err, "invalid query format")
}
