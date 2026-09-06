package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/infra/documentdb"
	"github.com/torchwooddev/torchwood/internal/testutil"
	"github.com/torchwooddev/torchwood/pkg/query"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDatabases_DocumentCRUD covers P1 Sprint 1 document API use cases.
func TestDatabases_DocumentCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	principal := databases.Principal{Roles: []string{"keys"}}

	const (
		dbID   = "app"
		collID = "posts"
	)
	require.NoError(t, uc.CreateDatabase(ctx, projectID, dbID, "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, dbID, collID, "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
	}, nil, nil, true))

	created, _, err := uc.CreateDocument(ctx, projectID, dbID, collID, "", map[string]any{
		"title": "Hello Torchwood",
		"views": 1,
	}, databases.DefaultCollectionPermissions(), principal, "")
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "Hello Torchwood", created.Data["title"])

	got, err := uc.GetDocument(ctx, projectID, dbID, collID, created.ID, principal)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)

	updated, _, err := uc.UpdateDocument(ctx, projectID, dbID, collID, created.ID, map[string]any{
		"views": 99,
	}, nil, nil, nil, principal, &created.Version, "")
	require.NoError(t, err)
	require.Equal(t, float64(99), updated.Data["views"])
	require.Equal(t, int64(2), updated.Version)

	listRes, err := uc.ListDocuments(ctx, projectID, dbID, collID, databases.Query{
		AST: &query.Query{Filter: query.Eq("title", "Hello Torchwood"), Orders: []query.Order{{Attribute: "$createdAt", Desc: true}}},
	}, principal)
	require.NoError(t, err)
	require.Equal(t, int64(1), listRes.TotalCount)
	require.Len(t, listRes.Documents, 1)

	count, err := uc.CountDocuments(ctx, projectID, dbID, collID, databases.Query{AST: &query.Query{Filter: query.Eq("title", "Hello Torchwood")}}, principal)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)

	_, delErr := uc.DeleteDocument(ctx, projectID, dbID, collID, created.ID, principal, &updated.Version, "")
	require.NoError(t, delErr)
	_, err = uc.GetDocument(ctx, projectID, dbID, collID, created.ID, principal)
	require.Error(t, err)
}

// TestDatabases_UpsertDocument (T2): UpsertDocument inserts a new document
// when no row matches the conflict columns and updates it when one does.
func TestDatabases_UpsertDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	principal := databases.Principal{Roles: []string{"keys"}}

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "members", "Members", []databases.Attribute{
		{ID: "email", Key: "email", Type: "string", Size: 256},
		{ID: "name", Key: "name", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "uq_email", Type: "unique", Attributes: []string{"email"}},
	}, nil, true))

	upserted, _, err := uc.UpsertDocument(ctx, projectID, "app", "members", "m1", map[string]any{
		"email": "a@example.com",
		"name":  "Alice",
	}, []string{"email"}, databases.DefaultCollectionPermissions(), principal, "")
	require.NoError(t, err)
	require.Equal(t, "m1", upserted.ID)
	require.Equal(t, "Alice", upserted.Data["name"])

	updated, _, err := uc.UpsertDocument(ctx, projectID, "app", "members", "m1", map[string]any{
		"email": "a@example.com",
		"name":  "Alice Updated",
	}, []string{"email"}, databases.DefaultCollectionPermissions(), principal, "")
	require.NoError(t, err)
	require.Equal(t, "m1", updated.ID)
	require.Equal(t, "Alice Updated", updated.Data["name"])

	got, err := uc.GetDocument(ctx, projectID, "app", "members", updated.ID, principal)
	require.NoError(t, err)
	require.Equal(t, "Alice Updated", got.Data["name"])
}

func TestDatabases_UpsertDocument_Validation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	principal := databases.Principal{Roles: []string{"keys"}}

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	_, _, err := uc.UpsertDocument(ctx, projectID, "app", "posts", "", nil, []string{"title"}, nil, principal, "")
	st, _ := status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())

	_, _, err = uc.UpsertDocument(ctx, projectID, "app", "posts", "", map[string]any{"title": "t"}, nil, nil, principal, "")
	st, _ = status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

// TestDatabases_UpsertDocument_EmptyACESeed：keys 主体空 ACE upsert 的插入支与
// 更新支都必须能读回、可改（回归：原实现种 read:__private__，且更新支把目标行
// ACL 整体替换为 __private__，keys 自己创建的文档被锁死）。集合只授 create:keys
// 不授 read，读回必须依赖文档级种子 ACE，精确复现锁死面。
func TestDatabases_UpsertDocument_EmptyACESeed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	principal := databases.Principal{Roles: []string{"keys"}}

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "members", "Members", []databases.Attribute{
		{ID: "email", Key: "email", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "uq_email", Type: "unique", Attributes: []string{"email"}},
	}, []databases.Permission{{Type: "create", Role: "keys"}}, true))

	// 插入支：空 ACE 种子为 read/update/delete:keys，读回与修改不依赖集合级 read。
	upserted, _, err := uc.UpsertDocument(ctx, projectID, "app", "members", "m1", map[string]any{
		"email": "seed@example.com",
	}, []string{"email"}, nil, principal, "")
	require.NoError(t, err)
	require.Equal(t, "m1", upserted.ID)
	requirePermsMatchRoles(t, upserted.Permissions, "keys")

	got, err := uc.GetDocument(ctx, projectID, "app", "members", "m1", principal)
	require.NoError(t, err)
	require.Equal(t, "seed@example.com", got.Data["email"])
	requirePermsMatchRoles(t, got.Permissions, "keys")

	updated, _, err := uc.UpdateDocument(ctx, projectID, "app", "members", "m1", map[string]any{
		"email": "seed2@example.com",
	}, nil, nil, nil, principal, &got.Version, "")
	require.NoError(t, err)
	require.Equal(t, "seed2@example.com", updated.Data["email"])

	// 更新支：conflict 命中已有行，种子不得把目标行 ACL 替换为 __private__。
	updatedAgain, _, err := uc.UpsertDocument(ctx, projectID, "app", "members", "m2", map[string]any{
		"email": "seed2@example.com",
	}, []string{"email"}, nil, principal, "")
	require.NoError(t, err)
	require.Equal(t, "m1", updatedAgain.ID)
	requirePermsMatchRoles(t, updatedAgain.Permissions, "keys")

	gotAgain, err := uc.GetDocument(ctx, projectID, "app", "members", "m1", principal)
	require.NoError(t, err)
	require.Equal(t, int64(3), gotAgain.Version)

	_, _, err = uc.UpdateDocument(ctx, projectID, "app", "members", "m1", map[string]any{
		"email": "seed3@example.com",
	}, nil, nil, nil, principal, &gotAgain.Version, "")
	require.NoError(t, err)
}

// requirePermsMatchRoles 断言 perms 恰好是 role 的 read/update/delete 三元组
// （seedDocumentPermissions 的种子形态，不含 __private__）。
func requirePermsMatchRoles(t *testing.T, perms []databases.Permission, role string, msgAndArgs ...any) {
	t.Helper()
	require.Len(t, perms, 3, msgAndArgs...)
	got := map[string]string{}
	for _, p := range perms {
		got[p.Type] = p.Role
	}
	for _, typ := range []string{"read", "update", "delete"} {
		require.Equalf(t, role, got[typ], "permission %s:%s missing creator seed", typ, role)
	}
}

// TestDatabases_UpdateDocument_OCCConflictCarriesCurrentVersion（B10，redesign
// §10.1）：OCC 冲突响应的 ErrorInfo metadata 携带 current_version 且等于探测
// 时的实际 _version——Agent 直接取该值合并重试，无需额外读回。
func TestDatabases_UpdateDocument_OCCConflictCarriesCurrentVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	principal := databases.Principal{Roles: []string{"keys"}}

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	created, _, err := uc.CreateDocument(ctx, projectID, "app", "posts", "d1", map[string]any{
		"title": "v1",
	}, nil, principal, "")
	require.NoError(t, err)
	require.Equal(t, int64(1), created.Version)

	// 过期版本 99 → VERSION_CONFLICT，metadata.current_version = "1"。
	stale := int64(99)
	_, _, err = uc.UpdateDocument(ctx, projectID, "app", "posts", "d1", map[string]any{
		"title": "stale",
	}, nil, nil, nil, principal, &stale, "")
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	st := status.Convert(err)
	require.Contains(t, st.Message(), databases.ErrCodeVersionConflict)
	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			info = i
		}
	}
	require.NotNil(t, info, "错误体必须携带 ErrorInfo")
	require.Equal(t, databases.ErrCodeVersionConflict, info.Reason)
	require.Contains(t, info.Metadata, "current_version", "冲突错误体必须带 current_version")
	require.Equal(t, "1", info.Metadata["current_version"])

	// 冲突未覆盖并发写：行仍是 v1；取实际值合并重试成功。
	got, err := uc.GetDocument(ctx, projectID, "app", "posts", "d1", principal)
	require.NoError(t, err)
	require.Equal(t, "v1", got.Data["title"])
	updated, _, err := uc.UpdateDocument(ctx, projectID, "app", "posts", "d1", map[string]any{
		"title": "v1-merged",
	}, nil, nil, nil, principal, &got.Version, "")
	require.NoError(t, err)
	require.Equal(t, "v1-merged", updated.Data["title"])
}

// keyPrincipal 构造 API key 文档主体（DocPrincipal 投影形态：keys 承载
// scope/API 面，key:<id> 承载数据隔离身份，KeyID 供写入归因）。
func keyPrincipal(id string) databases.Principal {
	return databases.Principal{Roles: []string{"keys", "key:" + id}, KeyID: id}
}

// TestDatabases_PerKeyDocumentIsolation（B14 完成判据，C6 决议）：默认私有——
// keyA 空 ACE 种子建的文档 keyB 不可见（Get=NotFound 防枚举、List/Count 过滤）；
// 显式授予 read:key:<keyB> ACE 后 keyB 可见（只读，写被 policy 拒绝）；keyA
// 自身读写删全权不变；execute-tx 的空 permissions create op 走同一种子。
func TestDatabases_PerKeyDocumentIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	keyA, keyB := keyPrincipal("ka"), keyPrincipal("kb")

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	// 集合只授 create:keys（无集合级 read）：读回必须依赖文档级种子 ACE，
	// 精确锁定 per-key 私有语义。
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 128},
	}, nil, []databases.Permission{{Type: "create", Role: "keys"}}, true))

	// keyA 空 ACE 创建 → 种子绑 key:ka 三连（非共享 keys）。
	created, _, err := uc.CreateDocument(ctx, projectID, "app", "notes", "doc-a", map[string]any{
		"title": "by-key-a",
	}, nil, keyA, "")
	require.NoError(t, err)
	requirePermsMatchRoles(t, created.Permissions, "key:ka")

	// keyB 全路径不可见：Get=NotFound（防枚举）、List 不含、Count=0。
	_, err = uc.GetDocument(ctx, projectID, "app", "notes", "doc-a", keyB)
	require.Equal(t, codes.NotFound, status.Code(err), "keyB 不可见必须 NotFound（防枚举）")
	listB, err := uc.ListDocuments(ctx, projectID, "app", "notes", databases.Query{}, keyB)
	require.NoError(t, err)
	require.Empty(t, listB.Documents, "keyB 的 List 不得包含 keyA 私有文档")
	countB, err := uc.CountDocuments(ctx, projectID, "app", "notes", databases.Query{}, keyB)
	require.NoError(t, err)
	require.Zero(t, countB)

	// keyA 自身全权不变：读改删往返（种子三连语义）。
	got, err := uc.GetDocument(ctx, projectID, "app", "notes", "doc-a", keyA)
	require.NoError(t, err)
	require.Equal(t, "by-key-a", got.Data["title"])
	updated, _, err := uc.UpdateDocument(ctx, projectID, "app", "notes", "doc-a", map[string]any{
		"title": "by-key-a-v2",
	}, nil, nil, nil, keyA, &got.Version, "")
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Version)

	// 显式授予跨 key 协作：keyA 追加 read:key:kb ACE（keys 主体 privileged，
	// 授予校验跳过——与既有 keys 授予面一致）。
	_, _, err = uc.UpdateDocument(ctx, projectID, "app", "notes", "doc-a", nil,
		[]databases.Permission{
			{Type: "read", Role: "key:ka"},
			{Type: "update", Role: "key:ka"},
			{Type: "delete", Role: "key:ka"},
			{Type: "read", Role: "key:kb"},
		}, nil, nil, keyA, &updated.Version, "")
	require.NoError(t, err)

	// keyB 可见（只读）：Get 成功；写被 policy 拒绝（可见 + version 相符 → PERMISSION_DENIED）。
	granted, err := uc.GetDocument(ctx, projectID, "app", "notes", "doc-a", keyB)
	require.NoError(t, err, "显式授予 read:key:kb 后 keyB 可见（跨 key 协作）")
	require.Equal(t, "by-key-a-v2", granted.Data["title"])
	v3 := int64(3)
	_, _, err = uc.UpdateDocument(ctx, projectID, "app", "notes", "doc-a", map[string]any{
		"title": "by-key-b",
	}, nil, nil, nil, keyB, &v3, "")
	require.Equal(t, codes.PermissionDenied, status.Code(err), "read-only 授予不可写（policy 拒绝）")

	// keyA 全权仍在：自锁授权面不变，keyA 可删自身文档。
	_, err = uc.DeleteDocument(ctx, projectID, "app", "notes", "doc-a", keyA, &v3, "")
	require.NoError(t, err)
}

// TestDatabases_PerKeySeedViaExecuteTransactions（B14）：execute-tx 的
// create/upsert 空 permissions op 与单文档 API 同种子（R1 per-op 豁免结构
// 不变，种子值收敛为 key:<id>）——keyA 经事务批建文档，keyB 不可见。
func TestDatabases_PerKeySeedViaExecuteTransactions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	keyA, keyB := keyPrincipal("ka"), keyPrincipal("kb")

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 128},
	}, nil, []databases.Permission{{Type: "create", Role: "keys"}}, true))

	results, _, err := uc.ExecuteTransactions(ctx, projectID, "app", []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "tx-a",
			Data: map[string]any{"title": "tx-by-key-a"}},
	}, databases.TransactionModeAtomic, keyA, "")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.True(t, results[0].OK)

	got, err := uc.GetDocument(ctx, projectID, "app", "notes", "tx-a", keyA)
	require.NoError(t, err)
	requirePermsMatchRoles(t, got.Permissions, "key:ka", "execute-tx 空 permissions op 种子绑 key:<id>")

	_, err = uc.GetDocument(ctx, projectID, "app", "notes", "tx-a", keyB)
	require.Equal(t, codes.NotFound, status.Code(err), "execute-tx 种子文档对其他 key 同样不可见")
}
