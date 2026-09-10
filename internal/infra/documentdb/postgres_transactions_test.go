package documentdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
)

func setupTxTest(t *testing.T) (databases.DocumentDB, string, string) {
	t.Helper()
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 128},
	}, nil, []databases.Permission{
		{Type: "create", Role: "keys"},
		{Type: "read", Role: "keys"},
		{Type: "update", Role: "keys"},
		{Type: "delete", Role: "keys"},
	}, true))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "members", "Members", []databases.Attribute{
		{ID: "email", Key: "email", Type: "string", Size: 128},
	}, []databases.Index{
		{ID: "uq_email", Type: "unique", Attributes: []string{"email"}},
	}, []databases.Permission{{Type: "create", Role: "keys"}}, true))
	return docDB, projectID, "app"
}

var txKeys = databases.Principal{Roles: []string{"keys"}, KeyID: "ktx"}

// ATOMIC：任一 op 失败整批回滚（第一个 op 的写入不留痕），错误带域码与 op index。
func TestExecuteTransactions_AtomicRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, dbID := setupTxTest(t)
	seed := []databases.Permission{{Type: "read", Role: "keys"}, {Type: "update", Role: "keys"}}
	v2 := int64(2)

	_, err := docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "one"}, Permissions: seed},
		// update 不存在的文档 → ErrDocumentNotFound → 整批回滚。
		{Type: databases.TransactionOpUpdate, CollectionID: "notes", DocumentID: "missing", Data: map[string]any{"title": "x"}, ExpectedVersion: &v2},
	}, databases.TransactionModeAtomic, txKeys)
	require.Error(t, err)
	oe := databases.AsOpError(err)
	require.NotNil(t, oe)
	require.Equal(t, 1, oe.Index)
	require.Equal(t, databases.ErrCodeNotFound, databases.ErrorDomainCode(oe.Err))

	// 第一个 op 已回滚：n1 不存在。
	got, err := docDB.GetDocument(ctx, projectID, dbID, "notes", "n1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.Nil(t, got)
}

// ATOMIC 正向链路：create v1 → update（CAS v1→v2）→ upsert 插入 → 全部留痕。
func TestExecuteTransactions_AtomicMixedChain(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, dbID := setupTxTest(t)
	seed := []databases.Permission{{Type: "read", Role: "keys"}, {Type: "update", Role: "keys"}, {Type: "delete", Role: "keys"}}
	v1 := int64(1)

	results, err := docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "one"}, Permissions: seed},
		{Type: databases.TransactionOpUpdate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "two"}, ExpectedVersion: &v1},
		{Type: databases.TransactionOpUpsert, CollectionID: "members", DocumentID: "m1", Data: map[string]any{"email": "a@b.c"}, ConflictColumns: []string{"email"}},
	}, databases.TransactionModeAtomic, txKeys)
	require.NoError(t, err)
	require.Len(t, results, 3)
	require.True(t, results[0].OK)
	require.Equal(t, int64(1), results[0].Version)
	require.True(t, results[1].OK)
	require.Equal(t, int64(2), results[1].Version)
	require.True(t, results[2].OK)
	require.Equal(t, "m1", results[2].DocumentID)

	note, err := docDB.GetDocument(ctx, projectID, dbID, "notes", "n1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, "two", note.Data["title"])
	require.Equal(t, "key:ktx", note.CreatedBy) // 归因贯穿批路径
}

// PARTIAL：失败 op 记录 per-op 结果（域码），已成功不回滚，后续 op 继续。
func TestExecuteTransactions_Partial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, dbID := setupTxTest(t)
	seed := []databases.Permission{{Type: "read", Role: "keys"}, {Type: "update", Role: "keys"}}
	v2 := int64(2)

	results, err := docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "one"}, Permissions: seed},
		{Type: databases.TransactionOpUpdate, CollectionID: "notes", DocumentID: "missing", Data: map[string]any{"title": "x"}, ExpectedVersion: &v2},
		{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "n2", Data: map[string]any{"title": "two"}, Permissions: seed},
	}, databases.TransactionModePartial, txKeys)
	require.NoError(t, err)
	require.Len(t, results, 3)
	require.True(t, results[0].OK)
	require.False(t, results[1].OK)
	require.Equal(t, databases.ErrCodeNotFound, results[1].ErrCode)
	require.True(t, results[2].OK)

	// 已成功 op 留痕。
	got, err := docDB.GetDocument(ctx, projectID, dbID, "notes", "n1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.NotNil(t, got)
	got2, err := docDB.GetDocument(ctx, projectID, dbID, "notes", "n2", databases.SystemPrincipal)
	require.NoError(t, err)
	require.NotNil(t, got2)
}

// PARTIAL 权限拒绝：批内越权 op 记录 PERMISSION_DENIED 域码，其余照常。
func TestExecuteTransactions_PartialPermissionDenied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, dbID := setupTxTest(t)
	seed := []databases.Permission{{Type: "read", Role: "keys"}, {Type: "update", Role: "keys"}}
	// guests 对 notes 无任何权限。
	guest := databases.Principal{Roles: []string{"guests"}}

	results, err := docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "g1", Data: map[string]any{"title": "x"}, Permissions: seed},
	}, databases.TransactionModePartial, guest)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.False(t, results[0].OK)
	require.Equal(t, databases.ErrCodePermissionDenied, results[0].ErrCode)

	got, err := docDB.GetDocument(ctx, projectID, dbID, "notes", "g1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.Nil(t, got)
}

// expected_version 三态（Phase 1 裁决②）：缺省（nil）→ update 盲写 +1
// （LWW）/ delete 报 version_required；显式 0 → InvalidArgument
// （DOCUMENT.VERSION_INVALID，不再与缺省混同）。
func TestExecuteTransactions_ExpectedVersionStates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, dbID := setupTxTest(t)
	seed := []databases.Permission{{Type: "read", Role: "keys"}, {Type: "update", Role: "keys"}, {Type: "delete", Role: "keys"}}

	// update 缺省 → 盲写 +1（LWW）。
	results, err := docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "one"}, Permissions: seed},
		{Type: databases.TransactionOpUpdate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "two"}},
	}, databases.TransactionModeAtomic, txKeys)
	require.NoError(t, err)
	require.True(t, results[1].OK)
	require.Equal(t, int64(2), results[1].Version)

	// update 显式 0 → InvalidArgument / DOCUMENT.VERSION_INVALID（ATOMIC 整批回滚）。
	zero := int64(0)
	_, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpUpdate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "x"}, ExpectedVersion: &zero},
	}, databases.TransactionModeAtomic, txKeys)
	require.Error(t, err)
	oe := databases.AsOpError(err)
	require.NotNil(t, oe)
	require.Equal(t, databases.ErrCodeVersionInvalid, databases.ErrorDomainCode(oe.Err))

	// delete 显式 0 → 同码；delete 缺省 → version_required（不支持盲删）。
	_, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpDelete, CollectionID: "notes", DocumentID: "n1", ExpectedVersion: &zero},
	}, databases.TransactionModeAtomic, txKeys)
	require.Error(t, err)
	oe = databases.AsOpError(err)
	require.NotNil(t, oe)
	require.Equal(t, databases.ErrCodeVersionInvalid, databases.ErrorDomainCode(oe.Err))

	_, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpDelete, CollectionID: "notes", DocumentID: "n1"},
	}, databases.TransactionModeAtomic, txKeys)
	require.Error(t, err)
	oe = databases.AsOpError(err)
	require.NotNil(t, oe)
	require.Equal(t, databases.ErrCodeVersionRequired, databases.ErrorDomainCode(oe.Err))

	// 正确值 → 删除成功（当前版本：create v1 → 盲写 update v2）。
	v2 := int64(2)
	results, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpDelete, CollectionID: "notes", DocumentID: "n1", ExpectedVersion: &v2},
	}, databases.TransactionModeAtomic, txKeys)
	require.NoError(t, err)
	require.True(t, results[0].OK)
}

// 输入校验：空批/超限/非法类型/非法模式。
func TestExecuteTransactions_Validation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, dbID := setupTxTest(t)

	_, err := docDB.ExecuteTransactions(ctx, projectID, dbID, nil, databases.TransactionModeAtomic, txKeys)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: "bogus", CollectionID: "notes", DocumentID: "x", Data: map[string]any{"title": "t"}},
	}, databases.TransactionModeAtomic, txKeys)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "x", Data: map[string]any{"title": "t"}},
	}, databases.TransactionMode("weird"), txKeys)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// 转出 POC B1：execute-tx op 的 array_updates——update op 数组原子更新与
// increment 同语句组合；仅 update 消费（其余类型忽略）；ATOMIC 失败回滚；
// PARTIAL 记录 per-op 结果。
func TestExecuteTransactions_ArrayUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, dbID := setupTxTest(t)
	require.NoError(t, docDB.CreateCollection(ctx, projectID, dbID, "items", "Items", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 128},
		{ID: "views", Key: "views", Type: "integer"},
		{ID: "tags", Key: "tags", Type: "string", Array: true},
	}, nil, []databases.Permission{
		{Type: "create", Role: "keys"},
		{Type: "read", Role: "keys"},
		{Type: "update", Role: "keys"},
		{Type: "delete", Role: "keys"},
	}, true))
	seed := []databases.Permission{{Type: "read", Role: "keys"}, {Type: "update", Role: "keys"}}
	v1 := int64(1)

	// create + update（array append + intersect on 第二列 + increment 同语句）同批。
	results, err := docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "items", DocumentID: "i1",
			Data: map[string]any{"title": "one", "tags": []any{"a"}, "views": int64(0)}, Permissions: seed},
		{Type: databases.TransactionOpUpdate, CollectionID: "items", DocumentID: "i1",
			Increment:       map[string]int64{"views": 3},
			ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{"b", "c"}}},
			ExpectedVersion: &v1},
	}, databases.TransactionModeAtomic, txKeys)
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.True(t, results[0].OK)
	require.True(t, results[1].OK)
	require.Equal(t, int64(2), results[1].Version)

	got, err := docDB.GetDocument(ctx, projectID, dbID, "items", "i1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, []any{"a", "b", "c"}, got.Data["tags"])
	require.Equal(t, float64(3), got.Data["views"])

	// ATOMIC：op 数组更新命中未知属性 → InvalidArgument 整批回滚（首个 op 不留痕）。
	_, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "items", DocumentID: "i2",
			Data: map[string]any{"title": "two"}, Permissions: seed},
		{Type: databases.TransactionOpUpdate, CollectionID: "items", DocumentID: "i1",
			ArrayUpdates: map[string]databases.ArrayUpdate{"nope": {Op: databases.ArrayUpdateOpAppend, Values: []string{"x"}}}},
	}, databases.TransactionModeAtomic, txKeys)
	require.Error(t, err)
	oe := databases.AsOpError(err)
	require.NotNil(t, oe)
	require.Equal(t, 1, oe.Index)
	gone, err := docDB.GetDocument(ctx, projectID, dbID, "items", "i2", databases.SystemPrincipal)
	require.NoError(t, err)
	require.Nil(t, gone, "ATOMIC 回滚后批内前序 op 不得留痕")

	// ATOMIC：数组更新与 data 同列冲突 → op 级 InvalidArgument。
	stale := int64(2)
	_, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpUpdate, CollectionID: "items", DocumentID: "i1",
			Data:            map[string]any{"tags": []any{"dup"}},
			ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{"x"}}},
			ExpectedVersion: &stale},
	}, databases.TransactionModeAtomic, txKeys)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// ATOMIC：OCC 不匹配（携带数组更新）→ version_conflict 整批回滚，数组不变。
	_, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpUpdate, CollectionID: "items", DocumentID: "i1",
			ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpIntersect, Values: []string{"zzz"}}},
			ExpectedVersion: &v1}, // 旧版本
	}, databases.TransactionModeAtomic, txKeys)
	require.ErrorIs(t, err, databases.ErrVersionMismatch)
	unchanged, err := docDB.GetDocument(ctx, projectID, dbID, "items", "i1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, []any{"a", "b", "c"}, unchanged.Data["tags"])

	// PARTIAL：数组更新 op 失败记录 per-op 结果，后续 op 继续。
	results, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpUpdate, CollectionID: "items", DocumentID: "i1",
			ArrayUpdates: map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpInsert, Values: []string{"v"}}}}, // 缺 index
		{Type: databases.TransactionOpCreate, CollectionID: "items", DocumentID: "i3",
			Data: map[string]any{"title": "three"}, Permissions: seed},
	}, databases.TransactionModePartial, txKeys)
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.False(t, results[0].OK, "insert 缺 index 的 op 应失败")
	require.Contains(t, results[0].ErrMessage, "requires index")
	require.True(t, results[1].OK)

	// 非法 op 名（filter 为受限合法名，bogus 不是）→ InvalidArgument。
	_, err = docDB.ExecuteTransactions(ctx, projectID, dbID, []databases.TransactionOp{
		{Type: databases.TransactionOpUpdate, CollectionID: "items", DocumentID: "i1",
			ArrayUpdates: map[string]databases.ArrayUpdate{"tags": {Op: "bogus"}}},
	}, databases.TransactionModeAtomic, txKeys)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
