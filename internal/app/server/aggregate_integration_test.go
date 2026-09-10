package server

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 聚合语义（redesign §4.1 + §10.5 P1；§11-J D1）：
//   - 权限 golden：聚合一律在权限过滤后的可见行集上执行，不可见行的值
//     不进聚合、group_by 键不泄露；
//   - 数值白名单：非声明数值属性拒绝；group_by 须为已声明属性；
//   - 空集：sum=0，avg/min/max 无值；
//   - 类型化（预决策 5）：integer 属性 sum/min/max → int64；avg 恒 double；
//     float 属性恒 double。
func newAggregateTestSetup(t *testing.T) (context.Context, *Databases, string, func()) {
	t.Helper()
	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)

	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	// 集合级只授 create；读走文档级 ACE（documentSecurity）——这是 D1 权限
	// 过滤链的最严形态。
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
		{ID: "score", Key: "score", Type: "float"},
		{ID: "topic", Key: "topic", Type: "string", Size: 64},
	}, nil, []databases.Permission{{Type: "create", Role: "users"}}, true))

	return ctx, uc, projectID, func() {
		cleanup()
		_ = db.Close()
	}
}

func userPrincipal(id string) databases.Principal {
	return databases.Principal{Roles: []string{"users", "user:" + id}}
}

// seedAggregateDocs 建三篇文档：u1 可见两篇（views 10/30、topic a/b），
// 仅 u2 可见一篇（views 100、topic secret）。
func seedAggregateDocs(t *testing.T, ctx context.Context, uc *Databases, projectID string) {
	t.Helper()
	mk := func(docID, title, topic string, views int64, owner string) {
		_, _, err := uc.CreateDocument(ctx, projectID, "app", "posts", docID, map[string]any{
			"title": title, "views": views, "score": float64(views) / 10, "topic": topic,
		}, []databases.Permission{
			{Type: "read", Role: "user:" + owner},
			{Type: "update", Role: "user:" + owner},
			{Type: "delete", Role: "user:" + owner},
		}, userPrincipal(owner), "")
		require.NoError(t, err)
	}
	mk("d-u1-a", "Owned A", "a", 10, "u1")
	mk("d-u1-b", "Owned B", "b", 30, "u1")
	mk("d-u2", "Secret", "secret", 100, "u2")
}

// TestAggregateDocuments_PermissionGolden（D1 规范落地测试）：u1 的聚合
// 只统计 u1 可见的两篇；group_by 键只来自可见行。
func TestAggregateDocuments_PermissionGolden(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, cleanup := newAggregateTestSetup(t)
	defer cleanup()
	seedAggregateDocs(t, ctx, uc, projectID)

	u1 := userPrincipal("u1")
	groups, err := uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{}, []databases.AggregateSpec{
		{Function: databases.AggregateSum, Field: "views"},
		{Function: databases.AggregateAvg, Field: "views"},
	}, "", u1)
	require.NoError(t, err)
	require.Len(t, groups, 1, "无 group_by 时恰一组")
	sum := groups[0].Values[0]
	avg := groups[0].Values[1]
	require.Equal(t, databases.AggregateValueInt64, sum.Kind, "integer sum → int64")
	require.Equal(t, int64(40), sum.Int64, "10+30；不可见行（views=100）不得进聚合")
	require.Equal(t, databases.AggregateValueDouble, avg.Kind, "avg 恒 double")
	require.InDelta(t, 20.0, avg.Double, 1e-9)

	// group_by 键只来自可见行（"secret" 不得出现）；组内 sum 为 int64。
	groups, err = uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{}, []databases.AggregateSpec{
		{Function: databases.AggregateSum, Field: "views"},
	}, "topic", u1)
	require.NoError(t, err)
	keys := map[string]int64{}
	for _, g := range groups {
		require.NotNil(t, g.GroupKey)
		require.Len(t, g.Values, 1)
		require.Equal(t, databases.AggregateValueInt64, g.Values[0].Kind)
		keys[*g.GroupKey] = g.Values[0].Int64
	}
	require.Equal(t, map[string]int64{"a": 10, "b": 30}, keys, "group 键不得泄露不可见行")

	// PlatformAdmin 旁路可见全集。
	admin := databases.Principal{PlatformAdmin: true}
	groups, err = uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{}, []databases.AggregateSpec{
		{Function: databases.AggregateSum, Field: "views"},
	}, "", admin)
	require.NoError(t, err)
	require.Equal(t, int64(140), groups[0].Values[0].Int64)

	// AST 过滤在权限过滤之内叠加：u1 + equal(topic,a) → 10。
	groups, err = uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("topic", "a")},
	}, []databases.AggregateSpec{
		{Function: databases.AggregateSum, Field: "views"},
	}, "", u1)
	require.NoError(t, err)
	require.Equal(t, int64(10), groups[0].Values[0].Int64)
}

// TestAggregateDocuments_Validation：非数值属性 / 未声明 group_by / 空聚合集。
func TestAggregateDocuments_Validation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, cleanup := newAggregateTestSetup(t)
	defer cleanup()
	seedAggregateDocs(t, ctx, uc, projectID)

	u1 := userPrincipal("u1")
	_, err := uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{}, []databases.AggregateSpec{
		{Function: databases.AggregateSum, Field: "title"},
	}, "", u1)
	require.Equal(t, codes.InvalidArgument, status.Code(err), "字符串属性不得聚合")

	_, err = uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{}, []databases.AggregateSpec{
		{Function: databases.AggregateSum, Field: "views"},
	}, "undeclared_key", u1)
	require.Equal(t, codes.InvalidArgument, status.Code(err), "group_by 须为已声明属性")

	_, err = uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{}, []databases.AggregateSpec{
		{Function: databases.AggregateFunction("median"), Field: "views"},
	}, "", u1)
	require.Equal(t, codes.InvalidArgument, status.Code(err), "未知聚合函数拒绝")
}

// TestAggregateDocuments_EmptySet：空集语义 sum=0 / avg=min=max 无值。
func TestAggregateDocuments_EmptySet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, cleanup := newAggregateTestSetup(t)
	defer cleanup()

	// 集合无任何可见行（未播种）。
	groups, err := uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{}, []databases.AggregateSpec{
		{Function: databases.AggregateSum, Field: "views"},
		{Function: databases.AggregateAvg, Field: "views"},
		{Function: databases.AggregateMin, Field: "views"},
		{Function: databases.AggregateMax, Field: "score"},
	}, "", userPrincipal("u1"))
	require.NoError(t, err)
	require.Len(t, groups, 1)
	vals := groups[0].Values
	require.Equal(t, databases.AggregateValueInt64, vals[0].Kind, "integer sum 空集 = int64 0")
	require.Zero(t, vals[0].Int64)
	require.Equal(t, databases.AggregateValueNone, vals[1].Kind, "avg 空集无值")
	require.Equal(t, databases.AggregateValueNone, vals[2].Kind, "min 空集无值")
	require.Equal(t, databases.AggregateValueNone, vals[3].Kind, "max 空集无值")

	// group_by 下空集返回空组列表。
	groups, err = uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{}, []databases.AggregateSpec{
		{Function: databases.AggregateSum, Field: "views"},
	}, "topic", userPrincipal("u1"))
	require.NoError(t, err)
	require.Empty(t, groups)
}

// TestAggregateDocuments_IntegerPrecision（预决策 5）：integer sum 超过 2^53
// 仍精确（int64 通道，不经 double）；float 属性聚合恒 double。
func TestAggregateDocuments_IntegerPrecision(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, cleanup := newAggregateTestSetup(t)
	defer cleanup()

	// 两行各 2^53+1（double 无法精确表示该值，int64 通道可精确求和）。
	big := int64(1)<<53 + 1
	for i, owner := range []string{"u1", "u2"} {
		_, _, err := uc.CreateDocument(ctx, projectID, "app", "posts", "d-big-"+string(rune('0'+i)), map[string]any{
			"views": big, "score": 1.5, "topic": "t",
		}, []databases.Permission{
			{Type: "read", Role: "user:" + owner},
		}, userPrincipal(owner), "")
		require.NoError(t, err)
	}

	admin := databases.Principal{PlatformAdmin: true}
	groups, err := uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{}, []databases.AggregateSpec{
		{Function: databases.AggregateSum, Field: "views"},
		{Function: databases.AggregateMin, Field: "views"},
		{Function: databases.AggregateMax, Field: "views"},
		{Function: databases.AggregateAvg, Field: "views"},
		{Function: databases.AggregateSum, Field: "score"},
	}, "", admin)
	require.NoError(t, err)
	vals := groups[0].Values
	require.Equal(t, databases.AggregateValueInt64, vals[0].Kind)
	require.Equal(t, 2*big, vals[0].Int64, "sum > 2^53 精确（2*(2^53+1)=2^54+2）")
	require.Equal(t, big, vals[1].Int64, "integer min → int64")
	require.Equal(t, big, vals[2].Int64, "integer max → int64")
	require.Equal(t, databases.AggregateValueDouble, vals[3].Kind, "avg 恒 double")
	require.InDelta(t, float64(big), vals[3].Double, 1e-6)
	require.Equal(t, databases.AggregateValueDouble, vals[4].Kind, "float 属性 sum → double")
	require.InDelta(t, 3.0, vals[4].Double, 1e-9)
}

// TestAggregateDocuments_Overflow：integer sum 超出 int64 → InvalidArgument
// AGGREGATE.OVERFLOW（SUM(bigint)::int8 的 PG 22003 翻译）。
func TestAggregateDocuments_Overflow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, cleanup := newAggregateTestSetup(t)
	defer cleanup()

	maxInt64 := int64(math.MaxInt64)
	for i, owner := range []string{"u1", "u2"} {
		_, _, err := uc.CreateDocument(ctx, projectID, "app", "posts", "d-of-"+string(rune('0'+i)), map[string]any{
			"views": maxInt64, "topic": "t",
		}, []databases.Permission{
			{Type: "read", Role: "user:" + owner},
		}, userPrincipal(owner), "")
		require.NoError(t, err)
	}

	admin := databases.Principal{PlatformAdmin: true}
	_, err := uc.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{}, []databases.AggregateSpec{
		{Function: databases.AggregateSum, Field: "views"},
	}, "", admin)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), databases.ErrCodeAggregateOverflow)
}
