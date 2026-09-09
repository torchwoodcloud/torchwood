// 全局 catalog（阶段②包 A，redesign §4.2/C1/预决策 1）：public 两表落地、
// JSONB 合一契约、GetCollection 单查询、并发建集 AlreadyExists、物理名预留。
package documentdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/app/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
)

// TestCatalogCodec_RoundTrip 锁 JSONB 合一编解码的全字段契约（default 的
// 标量类型、size、required、array、options、index orders、permissions）。
func TestCatalogCodec_RoundTrip(t *testing.T) {
	attrs := []databases.Attribute{
		{ID: "a1", Key: "title", Type: "string", Size: 128, Required: true, Default: "untitled"},
		{ID: "a2", Key: "views", Type: "integer", Default: 42},
		{ID: "a3", Key: "pinned", Type: "boolean", Default: false},
		{ID: "a4", Key: "score", Type: "float", Default: 1.5},
		{ID: "a5", Key: "tags", Type: "json", Array: true, Options: map[string]any{"k": "v"}},
		{ID: "a6", Key: "no_default", Type: "datetime"},
	}
	raw, err := encodeAttributes(attrs)
	require.NoError(t, err)
	got, err := decodeAttributes(raw)
	require.NoError(t, err)
	require.Len(t, got, len(attrs))
	for i := range attrs {
		require.Equal(t, attrs[i].ID, got[i].ID)
		require.Equal(t, attrs[i].Key, got[i].Key)
		require.Equal(t, attrs[i].Type, got[i].Type)
		require.Equal(t, attrs[i].Size, got[i].Size)
		require.Equal(t, attrs[i].Required, got[i].Required)
		require.Equal(t, attrs[i].Array, got[i].Array)
		if attrs[i].Default == nil {
			require.Nil(t, got[i].Default)
		} else {
			require.Equal(t, fmt.Sprint(attrs[i].Default), fmt.Sprint(got[i].Default), "default 标量值读写一致")
		}
		require.Equal(t, attrs[i].Options["k"], got[i].Options["k"])
	}

	idxs := []databases.Index{
		{ID: "i1", Type: "key", Attributes: []string{"title"}},
		{ID: "i2", Type: "unique", Attributes: []string{"views", "title"}, Orders: []string{"ASC", "DESC"}},
	}
	idxRaw, err := encodeIndexes(idxs)
	require.NoError(t, err)
	gotIdxs, err := decodeIndexes(idxRaw)
	require.NoError(t, err)
	require.Equal(t, idxs, gotIdxs)

	perms := []databases.Permission{{Type: "read", Role: "any"}, {Type: "update", Role: "user:u1"}}
	permRaw, err := encodePermissions(perms)
	require.NoError(t, err)
	gotPerms, err := decodePermissions(permRaw)
	require.NoError(t, err)
	require.Equal(t, perms, gotPerms)

	// 空态编码为 []（列默认值形态一致）。
	empty, err := encodeAttributes(nil)
	require.NoError(t, err)
	require.Equal(t, "[]", empty)
}

// TestCatalogGlobal_PublicTablesRows：catalog CRUD 全套落在 public 两表——
// database/collection 行、权限/disabled patch、DeleteCollection/DeleteDatabase
// 级联清理。
func TestCatalogGlobal_PublicTablesRows(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 256, Default: "untitled"}},
		[]databases.Index{{ID: "title_key", Type: "key", Attributes: []string{"title"}}},
		[]databases.Permission{{Type: "read", Role: "any"}}, true))

	// 行落在 public 两表且 JSONB 合一列非空。
	dbCount, err := db.NewSelect().Model((*model.DocumentDatabase)(nil)).
		Where("project_id = ? AND database_id = ?", projectID, "app").Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, dbCount)

	collRow := new(model.DocumentCollection)
	require.NoError(t, db.NewSelect().Model(collRow).
		Where("project_id = ? AND database_id = ? AND collection_id = ?", projectID, "app", "posts").
		Scan(ctx))
	require.Equal(t, "Posts", collRow.Name)
	require.Equal(t, int64(1), collRow.DDLSeq)
	require.Equal(t, int64(1), collRow.SchemaVersion)
	require.Contains(t, collRow.Attrs, `"key": "title"`)
	require.Contains(t, collRow.Attrs, `"default": "untitled"`)
	require.Contains(t, collRow.Indexes, `"id": "title_key"`)
	require.Contains(t, collRow.Permissions, `"role": "any"`)

	// UpdateCollection patch（权限 + disabled）生效于同一行。
	disabled := true
	require.NoError(t, docDB.UpdateCollection(ctx, projectID, "app", "posts", databases.CollectionPatch{
		Permissions: &[]databases.Permission{{Type: "read", Role: "users"}},
		Disabled:    &disabled,
	}))
	got, err := docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.True(t, got.Disabled)
	require.Equal(t, []databases.Permission{{Type: "read", Role: "users"}}, got.Permissions)

	// DeleteCollection → 合一行消失。
	require.NoError(t, docDB.DeleteCollection(ctx, projectID, "app", "posts"))
	n, err := db.NewSelect().Model((*model.DocumentCollection)(nil)).
		Where("project_id = ?", projectID).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, n)

	// DeleteDatabase → database 行消失。
	require.NoError(t, docDB.DeleteDatabase(ctx, projectID, "app"))
	dbCount, err = db.NewSelect().Model((*model.DocumentDatabase)(nil)).
		Where("project_id = ?", projectID).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, dbCount)
}

// queryCountHook 统计 bun 执行的查询条数（GetCollection 单查询断言用）。
type queryCountHook struct {
	mu    sync.Mutex
	count int
}

func (h *queryCountHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	return ctx
}

func (h *queryCountHook) AfterQuery(ctx context.Context, event *bun.QueryEvent) {}

func (h *queryCountHook) snapshot() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// TestGetCollection_SingleQuery：JSONB 合一后 GetCollection 热路径恰为一条
// 查询（四表时代是 3 条：coll + attrs + idxs）。
func TestGetCollection_SingleQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	hook := &queryCountHook{}
	db.AddQueryHook(hook)

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
		[]databases.Attribute{
			{ID: "title", Key: "title", Type: "string", Size: 256},
			{ID: "views", Key: "views", Type: "integer"},
		},
		[]databases.Index{{ID: "title_key", Type: "key", Attributes: []string{"title"}}},
		nil, true))

	before := hook.snapshot()
	got, err := docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Len(t, got.Attributes, 2)
	require.Len(t, got.Indexes, 1)
	require.Equal(t, 1, hook.snapshot()-before, "GetCollection 必须恰为一条 catalog 查询")
}

// TestCreateCollection_ConcurrentSameID_AlreadyExists：并发建同 ID 集合，
// 恰一个成功，其余按主键冲突收敛为 ErrDuplicateKey（→ AlreadyExists）。
func TestCreateCollection_ConcurrentSameID_AlreadyExists(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))

	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
				[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 256}},
				nil, nil, true)
		}(i)
	}
	close(start)
	wg.Wait()

	succeeded, alreadyExists := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case status.Code(shared.MapDocumentDBError(err)) == codes.AlreadyExists:
			alreadyExists++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, n-1, alreadyExists)
}

// TestCreateCollection_PhysicalNameProjection：physical_name = collection_id
// 冗余投影（2026-09-06 勘误，随机物理名退役，运维可读）；sentinel 系统集合
// 同理（静态表名 = 逻辑名）。
func TestCreateCollection_PhysicalNameProjection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", nil, nil, nil, true))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "comments", "Comments", nil, nil, nil, true))
	require.NoError(t, testutil.SeedSystemDocumentCollections(ctx, db, docDB, projectID))

	type row struct {
		CollectionID string
		PhysicalName string
	}
	var rows []row
	require.NoError(t, db.NewSelect().Model((*model.DocumentCollection)(nil)).
		Column("collection_id", "physical_name").
		Where("project_id = ? AND database_id = ?", projectID, "app").
		Scan(ctx, &rows))
	require.Len(t, rows, 2)
	for _, r := range rows {
		require.Equal(t, r.CollectionID, r.PhysicalName, "物理名 = collection_id 冗余投影")
	}

	// sentinel 系统集合：物理名 = 逻辑名（静态表不可改名）。
	var sentinelPhys string
	require.NoError(t, db.NewSelect().Model((*model.DocumentCollection)(nil)).
		Column("physical_name").
		Where("project_id = ? AND database_id = '_' AND collection_id = 'users'", projectID).
		Scan(ctx, &sentinelPhys))
	require.Equal(t, "users", sentinelPhys)

	// 物理寻址字段不出现在 API 形状（domain Collection）里。
	got, err := docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Equal(t, "posts", got.ID)
	_ = time.Now // keep time import if assertions evolve
}
