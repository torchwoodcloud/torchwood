package documentdb

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

func timeNow() time.Time { return time.Unix(1700000000, 0).UTC() }

// 每算子编译单测（C7 单 AST）：AST 构造 → SQL 断言。DSL 解析路径的等价性
// 由 pkg/query 与 pkg/query/proto 的测试保证（客户端语法糖）。

func TestBuildAppwriteQuery_EveryComparisonOperator(t *testing.T) {
	cases := []struct {
		name   string
		filter *query.Filter
		where  string
		args   []any
	}{
		{"eq single", query.Eq("a", "b"), `d."a" = ?`, []any{"b"}},
		{"eq multi → IN", query.Eq("a", "b", "c"), `d."a" IN (?, ?)`, []any{"b", "c"}},
		{"ne single", query.Ne("a", "b"), `d."a" != ?`, []any{"b"}},
		{"ne multi → NOT IN", query.Ne("a", "b", "c"), `d."a" NOT IN (?, ?)`, []any{"b", "c"}},
		{"lt", query.Lt("a", "5"), `d."a" < ?`, []any{"5"}},
		{"lte", query.Lte("a", "5"), `d."a" <= ?`, []any{"5"}},
		{"gt", query.Gt("a", "5"), `d."a" > ?`, []any{"5"}},
		{"gte", query.Gte("a", "5"), `d."a" >= ?`, []any{"5"}},
		{"in", query.In("a", "x", "y"), `d."a" IN (?, ?)`, []any{"x", "y"}},
		{"contains", query.Contains("a", "jo"), `d."a" ILIKE ? ESCAPE '\'`, []any{"%jo%"}},
		{"notContains", query.NotContains("a", "jo"), `d."a" NOT ILIKE ? ESCAPE '\'`, []any{"%jo%"}},
		{"startsWith", query.StartsWith("a", "jo"), `d."a" ILIKE ? ESCAPE '\'`, []any{"jo%"}},
		{"notStartsWith", query.NotStartsWith("a", "jo"), `d."a" NOT ILIKE ? ESCAPE '\'`, []any{"jo%"}},
		{"endsWith", query.EndsWith("a", "jo"), `d."a" ILIKE ? ESCAPE '\'`, []any{"%jo"}},
		{"notEndsWith", query.NotEndsWith("a", "jo"), `d."a" NOT ILIKE ? ESCAPE '\'`, []any{"%jo"}},
		{"search", query.Search("a", "hi"), `to_tsvector('simple', d."a"::text) @@ plainto_tsquery('simple', ?)`, []any{"hi"}},
		{"notSearch", query.NotSearch("a", "hi"), `NOT (to_tsvector('simple', d."a"::text) @@ plainto_tsquery('simple', ?))`, []any{"hi"}},
		{"between", query.Between("a", "1", "9"), `d."a" BETWEEN ? AND ?`, []any{"1", "9"}},
		{"notBetween", query.NotBetween("a", "1", "9"), `d."a" NOT BETWEEN ? AND ?`, []any{"1", "9"}},
		{"isNull", query.IsNull("a"), `d."a" IS NULL`, nil},
		{"isNotNull", query.IsNotNull("a"), `d."a" IS NOT NULL`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, args, _, err := buildAppwriteQuery(&query.Query{Filter: tc.filter}, nil)
			require.NoError(t, err)
			require.Equal(t, tc.where, where)
			if tc.args == nil {
				require.Empty(t, args)
			} else {
				require.Equal(t, tc.args, args)
			}
		})
	}
}

// contains 族通配符转义在 not* 变体同样生效（escapeLikePattern 复用）。
func TestBuildAppwriteQuery_NotLikeEscapesWildcards(t *testing.T) {
	where, args, _, err := buildAppwriteQuery(&query.Query{Filter: query.NotContains("a", "50%_off")}, nil)
	require.NoError(t, err)
	require.Equal(t, `d."a" NOT ILIKE ? ESCAPE '\'`, where)
	require.Equal(t, []any{`%50\%\_off%`}, args)
}

func TestBuildAppwriteQuery_OrCompilesToSQLOr(t *testing.T) {
	where, args, _, err := buildAppwriteQuery(&query.Query{
		Filter: query.Or(query.Eq("status", "a"), query.Eq("status", "b")),
	}, nil)
	require.NoError(t, err)
	require.Equal(t, `(d."status" = ? OR d."status" = ?)`, where)
	require.Equal(t, []any{"a", "b"}, args)
	require.NotContains(t, where, " AND ")
}

// TestBuildAppwriteQuery_CustomOrderHasIDTiebreaker：自定义排序必须以
// _id 收尾且与 cursor 续页路径同构（重复排序键的全序确定性 + keyset 各页
// 同序；跨页不丢不重的机制保证）。
func TestBuildAppwriteQuery_CustomOrderHasIDTiebreaker(t *testing.T) {
	_, _, orderSQL, err := buildAppwriteQuery(&query.Query{Orders: []query.Order{{Attribute: "status"}}}, nil)
	require.NoError(t, err)
	require.Equal(t, `ORDER BY d."status" ASC, d._id ASC`, orderSQL)

	_, _, orderSQL, err = buildAppwriteQuery(&query.Query{Orders: []query.Order{{Attribute: "priority", Desc: true}}}, nil)
	require.NoError(t, err)
	require.Equal(t, `ORDER BY d."priority" DESC, d._id DESC`, orderSQL)

	// 默认排序（无显式 orders）同样带 _id。
	_, _, orderSQL, err = buildAppwriteQuery(&query.Query{Filter: query.Eq("status", "open")}, nil)
	require.NoError(t, err)
	require.Equal(t, `ORDER BY d._created_at DESC, d._id DESC`, orderSQL)
}

// TestBuildAppwriteQuery_TotalFilterParamsLimit：跨 filter 绑定参数累计上限
// （单 filter ≤1000 不封总量：100 叶 × 1000 值可积 10 万参数，超 PG 65535
// 语句参数上限后以运行时错误暴露）。
func TestBuildAppwriteQuery_TotalFilterParamsLimit(t *testing.T) {
	makeFilter := func(n int) *query.Filter {
		values := make([]string, n)
		for i := range values {
			values[i] = fmt.Sprintf("v%d", i)
		}
		return query.In("status", values...)
	}

	// 3 × 700 = 2100 > 2000 → InvalidArgument。
	over := &query.Query{Filter: query.And(makeFilter(700), makeFilter(700), makeFilter(700))}
	_, _, _, err := buildAppwriteQuery(over, nil)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())

	// 3 × 600 = 1800 ≤ 2000 → 通过。
	ok := &query.Query{Filter: query.And(makeFilter(600), makeFilter(600), makeFilter(600))}
	where, args, _, err := buildAppwriteQuery(ok, nil)
	require.NoError(t, err)
	require.NotEmpty(t, where)
	require.Len(t, args, 1800)
}

func TestBuildAppwriteQuery_AndTree(t *testing.T) {
	where, args, _, err := buildAppwriteQuery(&query.Query{
		Filter: query.And(query.Eq("a", "1"), query.Eq("b", "2")),
	}, nil)
	require.NoError(t, err)
	require.Equal(t, `(d."a" = ? AND d."b" = ?)`, where)
	require.Equal(t, []any{"1", "2"}, args)
}

func TestBuildAppwriteQuery_EmptyValuesInvalidArgument(t *testing.T) {
	_, _, _, err := buildAppwriteQuery(&query.Query{
		Filter: &query.Filter{Op: query.OpGreaterThan, Attribute: "n"},
	}, nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, _, _, err = buildAppwriteQuery(&query.Query{
		Filter: &query.Filter{Op: query.OpEqual, Attribute: "a"},
	}, nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestBuildAppwriteQuery_ArrayOperators（阶段③-b 预决策 2 门禁）：containsAny
// 编译 &&（交集非空）、containsAll 编译 @>（子集），参数按列元素类型 cast
// （pgTextArray 字面量 + ?::T[]）；arrayTypes 缺席（标量列/系统列/未声明列）
// 编译期兜底拒绝（validateQueryFields 白名单先行）。
func TestBuildAppwriteQuery_ArrayOperators(t *testing.T) {
	arrTypes := map[string]string{"tags": "TEXT[]", "nums": "BIGINT[]"}

	where, args, _, err := buildAppwriteQuery(&query.Query{
		Filter: query.ContainsAny("tags", "a", "b"),
	}, arrTypes)
	require.NoError(t, err)
	require.Equal(t, `d."tags" && ?::TEXT[]`, where)
	require.Equal(t, []any{`{"a","b"}`}, args)

	where, args, _, err = buildAppwriteQuery(&query.Query{
		Filter: query.ContainsAll("nums", "1", "2", "3"),
	}, arrTypes)
	require.NoError(t, err)
	require.Equal(t, `d."nums" @> ?::BIGINT[]`, where)
	require.Equal(t, []any{`{"1","2","3"}`}, args)

	// 标量列 / 未声明列 / 白名单缺席 → InvalidArgument（fail-closed 兜底）。
	_, _, _, err = buildAppwriteQuery(&query.Query{Filter: query.ContainsAny("title", "a")}, arrTypes)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, _, _, err = buildAppwriteQuery(&query.Query{Filter: query.ContainsAll("tags", "a")}, nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 空值 → InvalidArgument。
	_, _, _, err = buildAppwriteQuery(&query.Query{
		Filter: &query.Filter{Op: query.OpContainsAny, Attribute: "tags"},
	}, arrTypes)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestBuildArrayParts（阶段③-b 预决策 3）：四写算子的 SET 表达式形态断言
// （append/prepend 的 NULL 归一、remove 的空数组兜底、unique 的保序去重），
// 与 data/increment 同 SET 子句组合、非法输入拒绝。
func TestBuildArrayParts(t *testing.T) {
	attrs := []databases.Attribute{
		{Key: "tags", Type: "string", Array: true},
		{Key: "nums", Type: "integer", Array: true},
		{Key: "title", Type: "string"},
	}

	// append / prepend：COALESCE 把 NULL 列归一为空数组再拼接。
	parts, args, err := buildArrayParts(map[string]databases.ArrayUpdate{
		"tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{"x"}},
	}, nil, attrs)
	require.NoError(t, err)
	require.Equal(t, []string{`"tags" = COALESCE("tags", '{}'::TEXT[]) || ?::TEXT[]`}, parts)
	require.Equal(t, []any{`{"x"}`}, args)

	parts, args, err = buildArrayParts(map[string]databases.ArrayUpdate{
		"nums": {Op: databases.ArrayUpdateOpPrepend, Values: []string{"1", "2"}},
	}, nil, attrs)
	require.NoError(t, err)
	require.Equal(t, []string{`"nums" = ?::BIGINT[] || COALESCE("nums", '{}'::BIGINT[])`}, parts)
	require.Equal(t, []any{`{"1","2"}`}, args)

	// remove：CASE NULL 保持 + COALESCE 空数组兜底（移空后非 NULL）。
	parts, args, err = buildArrayParts(map[string]databases.ArrayUpdate{
		"tags": {Op: databases.ArrayUpdateOpRemove, Values: []string{"x"}},
	}, nil, attrs)
	require.NoError(t, err)
	require.Equal(t, []string{
		`"tags" = CASE WHEN "tags" IS NULL THEN NULL ELSE COALESCE((SELECT array_agg(e) FROM unnest("tags") e WHERE e <> ALL(?::TEXT[])), '{}'::TEXT[]) END`,
	}, parts)
	require.Equal(t, []any{`{"x"}`}, args)

	// unique：保首次出现序去重（WITH ORDINALITY + min(o)），无参数。
	parts, args, err = buildArrayParts(map[string]databases.ArrayUpdate{
		"tags": {Op: databases.ArrayUpdateOpUnique},
	}, nil, attrs)
	require.NoError(t, err)
	require.Equal(t, []string{
		`"tags" = CASE WHEN "tags" IS NULL THEN NULL ELSE (SELECT array_agg(e ORDER BY o) FROM (SELECT e, min(o) AS o FROM unnest("tags") WITH ORDINALITY AS u(e, o) GROUP BY e) s) END`,
	}, parts)
	require.Empty(t, args)

	// 拒绝面：标量列 / 未声明列 / data 同列冲突 / 未知 op / 空值。
	_, _, err = buildArrayParts(map[string]databases.ArrayUpdate{
		"title": {Op: databases.ArrayUpdateOpAppend, Values: []string{"x"}},
	}, nil, attrs)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, _, err = buildArrayParts(map[string]databases.ArrayUpdate{
		"ghost": {Op: databases.ArrayUpdateOpAppend, Values: []string{"x"}},
	}, nil, attrs)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, _, err = buildArrayParts(map[string]databases.ArrayUpdate{
		"tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{"x"}},
	}, map[string]any{"tags": []any{"y"}}, attrs)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, _, err = buildArrayParts(map[string]databases.ArrayUpdate{
		"tags": {Op: "bogus", Values: []string{"x"}},
	}, nil, attrs)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, _, err = buildArrayParts(map[string]databases.ArrayUpdate{
		"tags": {Op: databases.ArrayUpdateOpAppend},
	}, nil, attrs)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestAstFrom_MergesPageFields(t *testing.T) {
	ast, err := astFrom(databases.Query{
		PageSize:  7,
		PageToken: "ka:doc-1",
		AST:       &query.Query{Filter: query.Eq("a", "1")},
	})
	require.NoError(t, err)
	require.Equal(t, int32(7), ast.PageSize)
	require.Equal(t, "ka:doc-1", ast.PageToken)

	// AST 自带分页优先。
	ast, err = astFrom(databases.Query{
		PageSize: 9,
		AST:      &query.Query{Filter: query.Eq("a", "1"), PageSize: 3},
	})
	require.NoError(t, err)
	require.Equal(t, int32(3), ast.PageSize)
}

func TestAstFrom_EmptyGtEq(t *testing.T) {
	_, err := astFrom(databases.Query{AST: &query.Query{Filter: &query.Filter{Op: query.OpGreaterThan, Attribute: "n"}}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = astFrom(databases.Query{AST: &query.Query{Filter: &query.Filter{Op: query.OpEqual, Attribute: "a"}}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// buildKeysetPredicate 的 SQL 形（C2 完成态）：统一方向 → 行比较；混合方向
// → 逐键 OR 展开；op 随 after/before 与键方向翻转。
func TestBuildKeysetPredicate_UniformRowComparison(t *testing.T) {
	keys := []sortKey{{field: "priority", dir: "DESC"}, {field: "title", dir: "DESC"}}
	sqlText, args, err := buildKeysetPredicate(keys, []any{7, "t1"}, "doc-1", "after")
	require.NoError(t, err)
	require.Equal(t, `(d."priority", d."title", d._id) < (?, ?, ?)`, sqlText)
	require.Equal(t, []any{7, "t1", "doc-1"}, args)

	sqlText, args, err = buildKeysetPredicate(keys, []any{7, "t1"}, "doc-1", "before")
	require.NoError(t, err)
	require.Equal(t, `(d."priority", d."title", d._id) > (?, ?, ?)`, sqlText)
	require.Equal(t, []any{7, "t1", "doc-1"}, args)

	ascKeys := []sortKey{{field: "age", dir: "ASC"}}
	sqlText, args, err = buildKeysetPredicate(ascKeys, []any{30}, "d1", "after")
	require.NoError(t, err)
	require.Equal(t, `(d."age", d._id) > (?, ?)`, sqlText)
	require.Equal(t, []any{30, "d1"}, args)
}

func TestBuildKeysetPredicate_MixedDirectionORExpansion(t *testing.T) {
	keys := []sortKey{{field: "priority", dir: "DESC"}, {field: "title", dir: "ASC"}}
	sqlText, args, err := buildKeysetPredicate(keys, []any{7, "t1"}, "doc-1", "after")
	require.NoError(t, err)
	// after：DESC 键取 <、ASC 键取 >；_id 方向随首键（DESC → <）。
	require.Equal(t,
		`((d."priority" < ?) OR (d."priority" = ? AND d."title" > ?) OR (d."priority" = ? AND d."title" = ? AND d._id < ?))`,
		sqlText)
	require.Equal(t, []any{7, 7, "t1", 7, "t1", "doc-1"}, args)

	sqlText, args, err = buildKeysetPredicate(keys, []any{7, "t1"}, "doc-1", "before")
	require.NoError(t, err)
	require.Equal(t,
		`((d."priority" > ?) OR (d."priority" = ? AND d."title" < ?) OR (d."priority" = ? AND d."title" = ? AND d._id > ?))`,
		sqlText)
	require.Equal(t, []any{7, 7, "t1", 7, "t1", "doc-1"}, args)
}

func TestBuildKeysetPredicate_DefaultSingleKeyBackcompat(t *testing.T) {
	// 无显式排序（默认 _created_at DESC）与单键时代同形态。
	keys := []sortKey{{field: "_created_at", dir: "DESC"}}
	sqlText, args, err := buildKeysetPredicate(keys, []any{timeNow()}, "d1", "after")
	require.NoError(t, err)
	require.Equal(t, `(d."_created_at", d._id) < (?, ?)`, sqlText)
	require.Len(t, args, 2)
}
