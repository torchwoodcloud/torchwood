// 数组列端到端（阶段③-b 预决策 1/2/3）：array=true 属性落地 PG 原生 T[] 列，
// 读写往返、containsAny/All 语义矩阵、四写算子（含与 increment 组合与 OCC
// 交互）、非数组列用数组算子拒绝、数组列索引形态（GIN array_ops / unique
// 拒绝）。
package documentdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

// anyPerms 返回 any 的读写删三元组（RLS 可见性：测试集合无集合级权限，
// 文档 _acl 承载可见性）。
func anyPerms() []databases.Permission {
	return []databases.Permission{
		{Type: "read", Role: "any"},
		{Type: "update", Role: "any"},
		{Type: "delete", Role: "any"},
	}
}

func setupArrayCollection(ctx context.Context, t *testing.T) (databases.DocumentDB, string) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "items", "Items", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
		{ID: "tags", Key: "tags", Type: "string", Array: true},
		{ID: "nums", Key: "nums", Type: "integer", Array: true},
		{ID: "prices", Key: "prices", Type: "float", Array: true},
		{ID: "flags", Key: "flags", Type: "boolean", Array: true},
		{ID: "times", Key: "times", Type: "datetime", Array: true},
	}, nil, nil, true))
	return docDB, projectID
}

// TestArrayColumns_DDLAndRoundtrip：五类元素类型的物理列形态（udt_name）与
// 读写往返（写入字面量推断、读回 JSON 数组）。
func TestArrayColumns_DDLAndRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupArrayCollection(ctx, t)

	// 物理列类型断言：information_schema udt_name 的数组类型带 "_" 前缀。
	var udtTags, udtNums, udtPrices, udtFlags, udtTimes string
	db := docDB.(*postgresDocumentDB)
	err := db.conn(ctx).QueryRowContext(ctx, `
		SELECT
			MAX(udt_name) FILTER (WHERE column_name = 'tags'),
			MAX(udt_name) FILTER (WHERE column_name = 'nums'),
			MAX(udt_name) FILTER (WHERE column_name = 'prices'),
			MAX(udt_name) FILTER (WHERE column_name = 'flags'),
			MAX(udt_name) FILTER (WHERE column_name = 'times')
		FROM information_schema.columns
		WHERE table_schema = 'tw_`+projectID+`_app' AND table_name = (
			SELECT physical_name FROM catalog_collections
			WHERE project_id = ? AND database_id = 'app' AND collection_id = 'items')`,
		projectID).Scan(&udtTags, &udtNums, &udtPrices, &udtFlags, &udtTimes)
	require.NoError(t, err)
	require.Equal(t, "_text", udtTags)
	require.Equal(t, "_int8", udtNums)
	require.Equal(t, "_float8", udtPrices)
	require.Equal(t, "_bool", udtFlags)
	require.Equal(t, "_timestamptz", udtTimes)

	created, err := docDB.CreateDocument(ctx, projectID, "app", "items", databases.Document{
		Data: map[string]any{
			"title":  "hello",
			"tags":   []any{"a", "b"},
			"nums":   []any{int64(1), 2},
			"prices": []any{1.5, 2.25},
			"flags":  []any{true, false},
			"times":  []any{"2026-01-02T03:04:05Z"},
		},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)

	got, err := docDB.GetDocument(ctx, projectID, "app", "items", created.ID, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Equal(t, []any{"a", "b"}, got.Data["tags"])
	require.Equal(t, []any{float64(1), float64(2)}, got.Data["nums"])
	require.Equal(t, []any{1.5, 2.25}, got.Data["prices"])
	require.Equal(t, []any{true, false}, got.Data["flags"])
	require.Len(t, got.Data["times"], 1)

	// data 通道整列替换（字面量 + 目标列推断）。
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: created.ID, Data: map[string]any{"tags": []any{"x", "y"}}},
		ExpectedVersion: got.Version,
	}, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Equal(t, []any{"x", "y"}, updated.Data["tags"])
}

// TestArrayColumns_ContainsSemantics：containsAny（交集非空）/ containsAll
// （子集）语义矩阵——交集命中、无交集、子集、空数组列、NULL 列。
func TestArrayColumns_ContainsSemantics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupArrayCollection(ctx, t)

	seed := []struct {
		id   string
		tags []any
	}{
		{"ab", []any{"a", "b"}},
		{"bc", []any{"b", "c"}},
		{"empty", []any{}},
		{"null", nil},
	}
	for _, s := range seed {
		data := map[string]any{"title": s.id}
		if s.tags != nil {
			data["tags"] = s.tags
		}
		_, err := docDB.CreateDocument(ctx, projectID, "app", "items", databases.Document{ID: s.id, Data: data}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}
	principal := databases.Principal{Roles: []string{"any"}}

	listIDs := func(f *query.Filter) []string {
		t.Helper()
		list, err := docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{AST: &query.Query{Filter: f}}, principal)
		require.NoError(t, err)
		ids := make([]string, 0, len(list.Documents))
		for _, d := range list.Documents {
			ids = append(ids, d.ID)
		}
		return ids
	}

	require.ElementsMatch(t, []string{"ab", "bc"}, listIDs(query.ContainsAny("tags", "b")))
	require.ElementsMatch(t, []string{"ab"}, listIDs(query.ContainsAny("tags", "a")))
	require.Empty(t, listIDs(query.ContainsAny("tags", "zzz")))
	require.ElementsMatch(t, []string{"ab", "bc"}, listIDs(query.ContainsAny("tags", "a", "c")))
	require.ElementsMatch(t, []string{"ab"}, listIDs(query.ContainsAll("tags", "a", "b")))
	// 子集：列含更多元素同样命中。
	require.ElementsMatch(t, []string{"ab", "bc"}, listIDs(query.ContainsAll("tags", "b")))
	require.Empty(t, listIDs(query.ContainsAll("tags", "a", "c")))
	// 空数组列与 NULL 列不命中任何值集（containsAny/All 对其恒 false）。
	require.ElementsMatch(t, []string{"ab", "bc"}, listIDs(query.ContainsAny("tags", "a", "b", "c")))
	require.Empty(t, listIDs(query.ContainsAll("tags", "zzz")))

	// 数值数组（cast BIGINT[]）路径同样工作。
	_, err := docDB.CreateDocument(ctx, projectID, "app", "items", databases.Document{
		ID: "n1", Data: map[string]any{"title": "n1", "nums": []any{int64(1), 2}},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
	list, err := docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{AST: &query.Query{Filter: query.ContainsAny("nums", "2")}}, principal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	require.Equal(t, "n1", list.Documents[0].ID)

	// 标量列用数组算子 → InvalidArgument（白名单拒绝，系统列同样）。
	for _, f := range []*query.Filter{query.ContainsAny("title", "a"), query.ContainsAll("$id", "x")} {
		_, err := docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{AST: &query.Query{Filter: f}}, principal)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "filter=%s", f.Op)
	}
}

// TestArrayColumns_WriteOperators：四写算子端到端——append/prepend/remove
// （移空后空数组非 NULL）/unique（保序去重），与 increment 同语句组合、
// OCC 不匹配拒绝且不落任何变更。
func TestArrayColumns_WriteOperators(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupArrayCollection(ctx, t)

	_, err := docDB.CreateDocument(ctx, projectID, "app", "items", databases.Document{
		ID:   "op1",
		Data: map[string]any{"title": "op", "views": int64(0), "tags": []any{"b"}},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
	principal := databases.Principal{Roles: []string{"any"}}
	got, err := docDB.GetDocument(ctx, projectID, "app", "items", "op1", databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Equal(t, []any{"b"}, got.Data["tags"])

	// append + increment 同一 UPDATE。
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "op1"},
		Increment:       map[string]int64{"views": 3},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{"c"}}},
		ExpectedVersion: got.Version,
	}, principal)
	require.NoError(t, err)
	require.Equal(t, []any{"b", "c"}, updated.Data["tags"])
	require.Equal(t, float64(3), updated.Data["views"])

	// prepend。
	updated, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "op1"},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpPrepend, Values: []string{"a"}}},
		ExpectedVersion: updated.Version,
	}, principal)
	require.NoError(t, err)
	require.Equal(t, []any{"a", "b", "c"}, updated.Data["tags"])

	// remove：移空后为空数组（非 NULL）。
	updated, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "op1"},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpRemove, Values: []string{"a", "b", "c"}}},
		ExpectedVersion: updated.Version,
	}, principal)
	require.NoError(t, err)
	require.Equal(t, []any{}, updated.Data["tags"])

	// append 到空数组（重复值保留——append 不去重）+ unique 去重（保首次出现序）。
	updated, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "op1"},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{"z", "y", "z", "y", "x"}}},
		ExpectedVersion: updated.Version,
	}, principal)
	require.NoError(t, err)
	require.Equal(t, []any{"z", "y", "z", "y", "x"}, updated.Data["tags"])

	updated, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "op1"},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpUnique}},
		ExpectedVersion: updated.Version,
	}, principal)
	require.NoError(t, err)
	require.Equal(t, []any{"z", "y", "x"}, updated.Data["tags"])

	// 数值数组 remove（BIGINT[] cast 路径）。
	_, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "op1", Data: map[string]any{"nums": []any{int64(1), 2, 3}}},
		ExpectedVersion: updated.Version,
	}, principal)
	require.NoError(t, err)
	gotN, err := docDB.GetDocument(ctx, projectID, "app", "items", "op1", databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	updated, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "op1"},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"nums": {Op: databases.ArrayUpdateOpRemove, Values: []string{"2"}}},
		ExpectedVersion: gotN.Version,
	}, principal)
	require.NoError(t, err)
	require.Equal(t, []any{float64(1), float64(3)}, updated.Data["nums"])

	// OCC 不匹配：array update 被拒且不落任何变更。
	_, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "op1"},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{"nope"}}},
		ExpectedVersion: gotN.Version, // 旧版本
	}, principal)
	require.ErrorIs(t, err, databases.ErrVersionMismatch)
	final, err := docDB.GetDocument(ctx, projectID, "app", "items", "op1", databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Equal(t, []any{"z", "y", "x"}, final.Data["tags"], "OCC 拒绝后数组不得变化")

	// 非数组列的 array update / data 同列冲突 → InvalidArgument。
	_, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "op1"},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"title": {Op: databases.ArrayUpdateOpAppend, Values: []string{"x"}}},
		ExpectedVersion: final.Version,
	}, principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "op1", Data: map[string]any{"tags": []any{"dup"}}},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{"x"}}},
		ExpectedVersion: final.Version,
	}, principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestArrayColumns_WriteOperatorsSetFamily：转出 POC B1 四算子——intersect
// （交集去重保 col 首次出现序）/ diff（差集保序不去重，与 remove 同构）/
// filter（受限形态 = remove 等价）/ insert（0 基定点插入，越界尾插）。
// NULL 列语义锁定：读改写类（intersect/diff/filter）保真，添加类（insert）
// 归一为空数组；校验拒绝（insert index 缺省/负值/values≠1，交集差集过滤
// values=0）；BIGINT[] cast 路径；与 increment 同语句组合；OCC 拒绝不落变更。
func TestArrayColumns_WriteOperatorsSetFamily(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupArrayCollection(ctx, t)
	principal := databases.Principal{Roles: []string{"any"}}

	create := func(id string, data map[string]any) {
		t.Helper()
		_, err := docDB.CreateDocument(ctx, projectID, "app", "items", databases.Document{ID: id, Data: data}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}
	updateTags := func(id string, upd databases.ArrayUpdate, version int64) databases.Document {
		t.Helper()
		updated, err := docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
			Document:        databases.Document{ID: id},
			ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": upd},
			ExpectedVersion: version,
		}, principal)
		require.NoError(t, err)
		return updated
	}

	// intersect：{b,a,b,c} ∩ {a,b,z} → {b,a}（去重 + col 首次出现序，非参数序）。
	create("si", map[string]any{"title": "si", "tags": []any{"b", "a", "b", "c"}})
	updated := updateTags("si", databases.ArrayUpdate{Op: databases.ArrayUpdateOpIntersect, Values: []string{"a", "b", "z"}}, 1)
	require.Equal(t, []any{"b", "a"}, updated.Data["tags"])
	// 交集移空 → 空数组（非 NULL）。
	updated = updateTags("si", databases.ArrayUpdate{Op: databases.ArrayUpdateOpIntersect, Values: []string{"zzz"}}, updated.Version)
	require.Equal(t, []any{}, updated.Data["tags"])

	// diff：{b,a,b,c} - {b} → {a,c}（保序）；未命中值不去重（{a,c} - {zzz} 原样）。
	create("sd", map[string]any{"title": "sd", "tags": []any{"b", "a", "b", "c"}})
	updated = updateTags("sd", databases.ArrayUpdate{Op: databases.ArrayUpdateOpDiff, Values: []string{"b"}}, 1)
	require.Equal(t, []any{"a", "c"}, updated.Data["tags"])
	updated = updateTags("sd", databases.ArrayUpdate{Op: databases.ArrayUpdateOpDiff, Values: []string{"zzz"}}, updated.Version)
	require.Equal(t, []any{"a", "c"}, updated.Data["tags"])

	// filter（受限形态）：与 remove 等价——移除等于任一 values 的元素。
	create("sf", map[string]any{"title": "sf", "tags": []any{"x", "y", "x", "z"}})
	updated = updateTags("sf", databases.ArrayUpdate{Op: databases.ArrayUpdateOpFilter, Values: []string{"x", "q"}}, 1)
	require.Equal(t, []any{"y", "z"}, updated.Data["tags"])
	// 移空 → 空数组。
	updated = updateTags("sf", databases.ArrayUpdate{Op: databases.ArrayUpdateOpFilter, Values: []string{"y", "z"}}, updated.Version)
	require.Equal(t, []any{}, updated.Data["tags"])

	// insert：0 基定点插入，其后元素顺移；index == len 与越界均尾插。
	create("sn", map[string]any{"title": "sn", "tags": []any{"a", "c"}})
	updated = updateTags("sn", databases.ArrayUpdate{Op: databases.ArrayUpdateOpInsert, Values: []string{"x"}, Index: int32p(1)}, 1)
	require.Equal(t, []any{"a", "x", "c"}, updated.Data["tags"])
	updated = updateTags("sn", databases.ArrayUpdate{Op: databases.ArrayUpdateOpInsert, Values: []string{"p"}, Index: int32p(0)}, updated.Version)
	require.Equal(t, []any{"p", "a", "x", "c"}, updated.Data["tags"])
	updated = updateTags("sn", databases.ArrayUpdate{Op: databases.ArrayUpdateOpInsert, Values: []string{"q"}, Index: int32p(4)}, updated.Version)
	require.Equal(t, []any{"p", "a", "x", "c", "q"}, updated.Data["tags"])
	updated = updateTags("sn", databases.ArrayUpdate{Op: databases.ArrayUpdateOpInsert, Values: []string{"z"}, Index: int32p(99)}, updated.Version)
	require.Equal(t, []any{"p", "a", "x", "c", "q", "z"}, updated.Data["tags"])

	// NULL 列：intersect/diff/filter 保真（保持 NULL），insert 归一为单元素数组。
	create("snull", map[string]any{"title": "snull"})
	nullDoc, err := docDB.GetDocument(ctx, projectID, "app", "items", "snull", principal)
	require.NoError(t, err)
	require.Nil(t, nullDoc.Data["tags"])
	updated = updateTags("snull", databases.ArrayUpdate{Op: databases.ArrayUpdateOpIntersect, Values: []string{"a"}}, nullDoc.Version)
	require.Nil(t, updated.Data["tags"], "intersect 对 NULL 列保真")
	updated = updateTags("snull", databases.ArrayUpdate{Op: databases.ArrayUpdateOpDiff, Values: []string{"a"}}, updated.Version)
	require.Nil(t, updated.Data["tags"], "diff 对 NULL 列保真")
	updated = updateTags("snull", databases.ArrayUpdate{Op: databases.ArrayUpdateOpFilter, Values: []string{"a"}}, updated.Version)
	require.Nil(t, updated.Data["tags"], "filter 对 NULL 列保真")
	updated = updateTags("snull", databases.ArrayUpdate{Op: databases.ArrayUpdateOpInsert, Values: []string{"v"}, Index: int32p(0)}, updated.Version)
	require.Equal(t, []any{"v"}, updated.Data["tags"], "insert 对 NULL 列归一为空数组")

	// 数值数组（BIGINT[] cast 路径）：intersect 去重 + 与 increment 同语句组合。
	create("sn1", map[string]any{"title": "n1", "nums": []any{int64(1), 2, 2, 3}, "views": int64(0)})
	gotN, err := docDB.GetDocument(ctx, projectID, "app", "items", "sn1", principal)
	require.NoError(t, err)
	updated, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "sn1"},
		Increment:       map[string]int64{"views": 5},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"nums": {Op: databases.ArrayUpdateOpIntersect, Values: []string{"2", "9"}}},
		ExpectedVersion: gotN.Version,
	}, principal)
	require.NoError(t, err)
	require.Equal(t, []any{float64(2)}, updated.Data["nums"])
	require.Equal(t, float64(5), updated.Data["views"])

	// OCC 不匹配：intersect 被拒且不落变更。
	_, err = docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "sn1"},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"nums": {Op: databases.ArrayUpdateOpIntersect, Values: []string{"7"}}},
		ExpectedVersion: gotN.Version, // 旧版本
	}, principal)
	require.ErrorIs(t, err, databases.ErrVersionMismatch)
	final, err := docDB.GetDocument(ctx, projectID, "app", "items", "sn1", principal)
	require.NoError(t, err)
	require.Equal(t, []any{float64(2)}, final.Data["nums"], "OCC 拒绝后数组不得变化")

	// 校验拒绝：insert 的 index/values 约束与集合类 values >= 1（语义校验先于
	// OCC 判定，版本取当前值即可）。
	cur, err := docDB.GetDocument(ctx, projectID, "app", "items", "sn", principal)
	require.NoError(t, err)
	for name, upd := range map[string]databases.ArrayUpdate{
		"insert 缺 index":     {Op: databases.ArrayUpdateOpInsert, Values: []string{"v"}},
		"insert 负 index":     {Op: databases.ArrayUpdateOpInsert, Values: []string{"v"}, Index: int32p(-1)},
		"insert 多 values":    {Op: databases.ArrayUpdateOpInsert, Values: []string{"u", "v"}, Index: int32p(0)},
		"intersect 空 values": {Op: databases.ArrayUpdateOpIntersect},
		"diff 空 values":      {Op: databases.ArrayUpdateOpDiff},
		"filter 空 values":    {Op: databases.ArrayUpdateOpFilter},
	} {
		_, err := docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
			Document:        databases.Document{ID: "sn"},
			ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": upd},
			ExpectedVersion: cur.Version,
		}, principal)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "case=%s", name)
	}
}

// int32p 是 insert 算子 0 基 index 的便捷构造。
func int32p(v int32) *int32 { return &v }

// TestArrayColumns_IndexForms：数组列 key 索引自动 GIN array_ops；unique 与
// fulltext 对数组列拒绝；多列含数组列拒绝。
func TestArrayColumns_IndexForms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupArrayCollection(ctx, t)

	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "items", databases.Index{
		ID: "tags_key", Type: "key", Attributes: []string{"tags"},
	}))
	db := docDB.(*postgresDocumentDB)
	var idxDef string
	err := db.conn(ctx).QueryRowContext(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = 'tw_`+projectID+`_app' AND indexname LIKE '%tags_key'`).Scan(&idxDef)
	require.NoError(t, err)
	// array_ops 是数组列的默认 opclass，pg_indexes 的 indexdef 会省略它——
	// 形态断言以 USING gin 为准（DDL 源头显式 array_ops 见 createCollectionIndex）。
	require.Contains(t, idxDef, "USING gin")

	err = docDB.CreateIndex(ctx, projectID, "app", "items", databases.Index{
		ID: "tags_uniq", Type: "unique", Attributes: []string{"tags"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "unique indexes do not support array attributes")

	err = docDB.CreateIndex(ctx, projectID, "app", "items", databases.Index{
		ID: "tags_ft", Type: "fulltext", Attributes: []string{"tags"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	err = docDB.CreateIndex(ctx, projectID, "app", "items", databases.Index{
		ID: "mix_key", Type: "key", Attributes: []string{"tags", "title"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "single-column")

	// 数组属性本身可经 CreateAttribute 补挂（元素类型白名单内）。
	require.NoError(t, docDB.CreateAttribute(ctx, projectID, "app", "items", databases.Attribute{
		ID: "extra", Key: "extra", Type: "datetime", Array: true,
	}))
	err = docDB.CreateAttribute(ctx, projectID, "app", "items", databases.Attribute{
		ID: "bad", Key: "bad", Type: "json", Array: true,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "array supports string, integer, float, boolean, datetime")
}
