package queryproto

import (
	"testing"

	"github.com/stretchr/testify/require"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

func TestFromProto_EqMatchesParseEqual(t *testing.T) {
	parsed, err := query.Parse(`equal("a","b")`)
	require.NoError(t, err)

	ast, err := FromProto(&sharedv1.Query{
		Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: &sharedv1.Comparison{
			Attribute: "a",
			Values:    []string{"b"},
		}}},
	})
	require.NoError(t, err)
	require.NotNil(t, ast.Filter)
	require.Equal(t, parsed.Filter.Op, ast.Filter.Op)
	require.Equal(t, parsed.Filter.Attribute, ast.Filter.Attribute)
	require.Equal(t, parsed.Filter.Values, ast.Filter.Values)
}

func TestFromProto_OrTree(t *testing.T) {
	ast, err := FromProto(&sharedv1.Query{
		Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_Or{Or: &sharedv1.FilterList{
			Filters: []*sharedv1.Filter{
				{Expr: &sharedv1.Filter_Eq{Eq: &sharedv1.Comparison{Attribute: "status", Values: []string{"a"}}}},
				{Expr: &sharedv1.Filter_Eq{Eq: &sharedv1.Comparison{Attribute: "status", Values: []string{"b"}}}},
			},
		}}},
	})
	require.NoError(t, err)
	require.NotNil(t, ast.Filter)
	require.Equal(t, query.OpOr, ast.Filter.Op)
	require.Len(t, ast.Filter.Children, 2)
	require.Equal(t, query.OpEqual, ast.Filter.Children[0].Op)
	require.Equal(t, "status", ast.Filter.Children[0].Attribute)
}

func TestFromProto_EmptyComparisonValues(t *testing.T) {
	_, err := FromProto(&sharedv1.Query{
		Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_Gt{Gt: &sharedv1.Comparison{
			Attribute: "$id",
		}}},
	})
	require.Error(t, err)

	_, err = FromProto(&sharedv1.Query{
		Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: &sharedv1.Comparison{
			Attribute: "title",
			Values:    nil,
		}}},
	})
	require.Error(t, err)
}

func TestFromProto_LeafCountCap(t *testing.T) {
	filters := make([]*sharedv1.Filter, query.MaxQueries+1)
	for i := range filters {
		filters[i] = &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: &sharedv1.Comparison{
			Attribute: "a",
			Values:    []string{"1"},
		}}}
	}
	_, err := FromProto(&sharedv1.Query{
		Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_Or{Or: &sharedv1.FilterList{Filters: filters}}},
	})
	require.Error(t, err)
}

// comparison 返回带 attribute/values 的 Comparison（测试捷径）。
func comparison(attr string, values ...string) *sharedv1.Comparison {
	return &sharedv1.Comparison{Attribute: attr, Values: values}
}

// TestFromProto_EveryOperatorRoundTrip（C7 单 AST）：Filter oneof 的每个算子
// 分支 → AST 节点断言（算子/属性/值），覆盖 §4.1 算子全集。
func TestFromProto_EveryOperatorRoundTrip(t *testing.T) {
	cmp := func(expr *sharedv1.Filter) *query.Query {
		t.Helper()
		ast, err := FromProto(&sharedv1.Query{Filter: expr})
		require.NoError(t, err)
		require.NotNil(t, ast.Filter)
		return ast
	}
	cases := []struct {
		name string
		expr *sharedv1.Filter
		op   string
		attr string
		vals []string
	}{
		{"eq", &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: comparison("a", "b")}}, query.OpEqual, "a", []string{"b"}},
		{"ne", &sharedv1.Filter{Expr: &sharedv1.Filter_Ne{Ne: comparison("a", "b")}}, query.OpNotEqual, "a", []string{"b"}},
		{"lt", &sharedv1.Filter{Expr: &sharedv1.Filter_Lt{Lt: comparison("a", "5")}}, query.OpLessThan, "a", []string{"5"}},
		{"lte", &sharedv1.Filter{Expr: &sharedv1.Filter_Lte{Lte: comparison("a", "5")}}, query.OpLessThanEqual, "a", []string{"5"}},
		{"gt", &sharedv1.Filter{Expr: &sharedv1.Filter_Gt{Gt: comparison("a", "5")}}, query.OpGreaterThan, "a", []string{"5"}},
		{"gte", &sharedv1.Filter{Expr: &sharedv1.Filter_Gte{Gte: comparison("a", "5")}}, query.OpGreaterThanEqual, "a", []string{"5"}},
		{"in", &sharedv1.Filter{Expr: &sharedv1.Filter_In{In: comparison("a", "x", "y")}}, query.OpIn, "a", []string{"x", "y"}},
		{"contains", &sharedv1.Filter{Expr: &sharedv1.Filter_Contains{Contains: comparison("a", "jo")}}, query.OpContains, "a", []string{"jo"}},
		{"not_contains", &sharedv1.Filter{Expr: &sharedv1.Filter_NotContains{NotContains: comparison("a", "jo")}}, query.OpNotContains, "a", []string{"jo"}},
		{"starts_with", &sharedv1.Filter{Expr: &sharedv1.Filter_StartsWith{StartsWith: comparison("a", "jo")}}, query.OpStartsWith, "a", []string{"jo"}},
		{"not_starts_with", &sharedv1.Filter{Expr: &sharedv1.Filter_NotStartsWith{NotStartsWith: comparison("a", "jo")}}, query.OpNotStartsWith, "a", []string{"jo"}},
		{"ends_with", &sharedv1.Filter{Expr: &sharedv1.Filter_EndsWith{EndsWith: comparison("a", "jo")}}, query.OpEndsWith, "a", []string{"jo"}},
		{"not_ends_with", &sharedv1.Filter{Expr: &sharedv1.Filter_NotEndsWith{NotEndsWith: comparison("a", "jo")}}, query.OpNotEndsWith, "a", []string{"jo"}},
		{"search", &sharedv1.Filter{Expr: &sharedv1.Filter_Search{Search: comparison("a", "hi")}}, query.OpSearch, "a", []string{"hi"}},
		{"not_search", &sharedv1.Filter{Expr: &sharedv1.Filter_NotSearch{NotSearch: comparison("a", "hi")}}, query.OpNotSearch, "a", []string{"hi"}},
		{"between", &sharedv1.Filter{Expr: &sharedv1.Filter_Between{Between: comparison("a", "1", "9")}}, query.OpBetween, "a", []string{"1", "9"}},
		{"not_between", &sharedv1.Filter{Expr: &sharedv1.Filter_NotBetween{NotBetween: comparison("a", "1", "9")}}, query.OpNotBetween, "a", []string{"1", "9"}},
		{"is_null", &sharedv1.Filter{Expr: &sharedv1.Filter_IsNull{IsNull: comparison("a")}}, query.OpIsNull, "a", []string{}},
		{"is_not_null", &sharedv1.Filter{Expr: &sharedv1.Filter_IsNotNull{IsNotNull: comparison("a")}}, query.OpIsNotNull, "a", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ast := cmp(tc.expr)
			require.Equal(t, tc.op, ast.Filter.Op)
			require.Equal(t, tc.attr, ast.Filter.Attribute)
			require.Equal(t, tc.vals, ast.Filter.Values)
		})
	}
}

// select/orders/分页字段随 Query 顶层解码。
func TestFromProto_QueryTopLevelFields(t *testing.T) {
	ast, err := FromProto(&sharedv1.Query{
		Filter:    &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: comparison("a", "b")}},
		Orders:    []*sharedv1.Order{{Attribute: "$createdAt", Desc: true}},
		PageSize:  25,
		PageToken: "ka:doc-1",
		Select:    []string{"$id", "title"},
	})
	require.NoError(t, err)
	require.Equal(t, []query.Order{{Attribute: "$createdAt", Desc: true}}, ast.Orders)
	require.Equal(t, int32(25), ast.PageSize)
	require.Equal(t, "ka:doc-1", ast.PageToken)
	require.Equal(t, []string{"$id", "title"}, ast.Selects)
}

// 值数量约束按算子分流：between 恰 2、is_null 零值、eq 至少 1。
func TestFromProto_ValueArity(t *testing.T) {
	_, err := FromProto(&sharedv1.Query{Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_Between{
		Between: comparison("a", "1"),
	}}})
	require.ErrorContains(t, err, "exactly 2")

	_, err = FromProto(&sharedv1.Query{Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_Between{
		Between: comparison("a", "1", "2", "3"),
	}}})
	require.ErrorContains(t, err, "exactly 2")

	_, err = FromProto(&sharedv1.Query{Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_IsNull{
		IsNull: comparison("a", "x"),
	}}})
	require.ErrorContains(t, err, "no values")

	_, err = FromProto(&sharedv1.Query{Filter: &sharedv1.Filter{Expr: &sharedv1.Filter_NotBetween{
		NotBetween: comparison("a", "1"),
	}}})
	require.ErrorContains(t, err, "exactly 2")
}

// and/or 深度 ≤8（§4.1；单 AST 会话从 16 收紧）。N 层嵌套 → 最深叶在
// 深度 N：8 层通过、9 层拒绝。
func TestFromProto_DepthLimit(t *testing.T) {
	build := func(depth int) *sharedv1.Filter {
		leaf := &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: comparison("a", "1")}}
		cur := leaf
		for i := 0; i < depth; i++ {
			cur = &sharedv1.Filter{Expr: &sharedv1.Filter_And{And: &sharedv1.FilterList{Filters: []*sharedv1.Filter{cur}}}}
		}
		return cur
	}
	_, err := FromProto(&sharedv1.Query{Filter: build(query.MaxDepth + 1)})
	require.ErrorContains(t, err, "nesting")

	ast, err := FromProto(&sharedv1.Query{Filter: build(query.MaxDepth)})
	require.NoError(t, err)
	require.NotNil(t, ast.Filter)
}

// vectorSearch 返回带 attribute/values 的 VectorSearch（测试捷径）。
func vectorSearch(attr string, values ...float64) *sharedv1.VectorSearch {
	return &sharedv1.VectorSearch{Attribute: attr, Values: values, Metric: sharedv1.DistanceMetric_DISTANCE_METRIC_COSINE}
}

// KNN 组合约束（B2 更新）：与 orders 互斥（距离承载排序）；page_token 放开
// ——多页 KNN 由服务端发放 kvc: 距离游标，codec 透传（形态校验在 infra）。
func TestFromProto_VectorSearchCombinations(t *testing.T) {
	// orders + vector_search：拒绝。
	_, err := FromProto(&sharedv1.Query{
		VectorSearch: vectorSearch("emb", 1, 0, 0),
		Orders:       []*sharedv1.Order{{Attribute: "$createdAt"}},
	})
	require.ErrorContains(t, err, "cannot be combined with orders")

	// page_token + vector_search：合法（B2 多页 KNN）。
	ast, err := FromProto(&sharedv1.Query{
		VectorSearch: vectorSearch("emb", 1, 0, 0),
		PageSize:     5,
		PageToken:    "kvc:3ff0000000000000:doc-1",
	})
	require.NoError(t, err)
	require.NotNil(t, ast.VectorSearch)
	require.Equal(t, "kvc:3ff0000000000000:doc-1", ast.PageToken)

	// filter + vector_search + page_token：可组合（AND）。
	ast, err = FromProto(&sharedv1.Query{
		Filter:       &sharedv1.Filter{Expr: &sharedv1.Filter_Eq{Eq: comparison("grp", "hit")}},
		VectorSearch: vectorSearch("emb", 1, 0, 0),
		PageToken:    "kvc:3ff0000000000000:doc-1",
	})
	require.NoError(t, err)
	require.NotNil(t, ast.VectorSearch)
	require.NotNil(t, ast.Filter)
}

// ef_search（B7）的 presence 语义：缺省不设位；设置后值透传（取值域校验在
// infra 管道，codec 只透传"是否设置"）。
func TestFromProto_VectorSearchEfSearchPresence(t *testing.T) {
	ast, err := FromProto(&sharedv1.Query{VectorSearch: vectorSearch("emb", 1, 0, 0)})
	require.NoError(t, err)
	require.NotNil(t, ast.VectorSearch)
	require.Nil(t, ast.VectorSearch.EfSearch, "unset ef_search must stay nil")

	ef := int32(200)
	ast, err = FromProto(&sharedv1.Query{
		VectorSearch: &sharedv1.VectorSearch{
			Attribute: "emb", Values: []float64{1, 0, 0}, EfSearch: &ef,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, ast.VectorSearch.EfSearch)
	require.Equal(t, ef, *ast.VectorSearch.EfSearch)
}
