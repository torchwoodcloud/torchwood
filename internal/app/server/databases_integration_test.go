package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/documents"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDatabases_AcceptanceChain covers manual checklist §4.14–4.18:
// create database → collection → attribute → index, then delete in reverse order.
func TestDatabases_AcceptanceChain(t *testing.T) {
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

	const (
		dbID    = "app"
		collID  = "posts"
		attrKey = "title"
		indexID = "idx_title"
	)

	require.NoError(t, uc.CreateDatabase(ctx, projectID, dbID, "Application DB"))

	dbs, err := uc.ListDatabases(ctx, projectID)
	require.NoError(t, err)
	require.NotEmpty(t, dbs)

	gotDB, err := uc.GetDatabase(ctx, projectID, dbID)
	require.NoError(t, err)
	require.NotNil(t, gotDB)
	require.Equal(t, dbID, gotDB.ID)

	require.NoError(t, uc.CreateCollection(ctx, projectID, dbID, collID, "Posts", nil, nil, nil, true))

	colls, _, _, err := uc.ListCollections(ctx, projectID, dbID, databases.ListQuery{})
	require.NoError(t, err)
	require.Len(t, colls, 1)

	gotColl, err := uc.GetCollection(ctx, projectID, dbID, collID)
	require.NoError(t, err)
	require.Equal(t, collID, gotColl.ID)

	require.NoError(t, uc.CreateAttribute(ctx, projectID, dbID, collID, databases.Attribute{
		ID:   attrKey,
		Key:  attrKey,
		Type: "string",
		Size: 256,
	}))

	require.NoError(t, uc.CreateIndex(ctx, projectID, dbID, collID, databases.Index{
		ID:         indexID,
		Type:       "unique",
		Attributes: []string{attrKey},
	}))

	gotColl, err = uc.GetCollection(ctx, projectID, dbID, collID)
	require.NoError(t, err)
	require.Len(t, gotColl.Attributes, 1)
	require.Equal(t, attrKey, gotColl.Attributes[0].Key)
	require.Len(t, gotColl.Indexes, 1)
	require.Equal(t, indexID, gotColl.Indexes[0].ID)

	require.NoError(t, uc.DeleteCollection(ctx, projectID, dbID, collID))
	gotColl, err = uc.GetCollection(ctx, projectID, dbID, collID)
	require.NoError(t, err)
	require.Nil(t, gotColl)

	require.NoError(t, uc.DeleteDatabase(ctx, projectID, dbID))
	gotDB, err = uc.GetDatabase(ctx, projectID, dbID)
	require.NoError(t, err)
	require.Nil(t, gotDB)
}

// TestDatabases_CreateCollection_DocumentSecurityFalse: 显式 document_security=false
// 必须落库为 false（bun 不再因 default tag 把 false 剔除为 DB 默认 TRUE）。
func TestDatabases_CreateCollection_DocumentSecurityFalse(t *testing.T) {
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

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))

	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "open_coll", "Open", nil, nil, nil, false))
	openColl, err := uc.GetCollection(ctx, projectID, "app", "open_coll")
	require.NoError(t, err)
	require.False(t, openColl.DocumentSecurity, "document_security=false 必须原样落库")

	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "secure_coll", "Secure", nil, nil, nil, true))
	secureColl, err := uc.GetCollection(ctx, projectID, "app", "secure_coll")
	require.NoError(t, err)
	require.True(t, secureColl.DocumentSecurity, "document_security=true 行为不变")
}

// TestDatabases_ServerCreateDocument_EmptyPermissions (#1): Server API 创建文档
// 不带 permissions 时不再展开为默认集合权限（文档级权限为空）；显式传入保持原行为。
func TestDatabases_ServerCreateDocument_EmptyPermissions(t *testing.T) {
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

	created, _, err := uc.CreateDocument(ctx, projectID, "app", "posts", "", map[string]any{"title": "no perms"}, nil, principal, "")
	require.NoError(t, err)
	// 2026-08 回归修订：空 ACE 占位绑定创建者凭证角色（keys 保留读写删），
	// 不再是无人可匹配的 __private__；guest/any 仍被剔除、集合回落仍关闭。
	require.Len(t, created.Permissions, 3)
	require.Equal(t, "keys", created.Permissions[0].Role)
	require.Equal(t, "keys", created.Permissions[1].Role)
	require.Equal(t, "keys", created.Permissions[2].Role)

	// 创建者读回可用（回归核心断言：Server CRUD 往返）。
	got, err := uc.GetDocument(ctx, projectID, "app", "posts", created.ID, principal)
	require.NoError(t, err)
	require.Equal(t, "no perms", got.Data["title"])
	// guest 不可见（C1 目标保持；阶段③包 C：NotFound 取代 PermissionDenied）。
	_, err = uc.GetDocument(ctx, projectID, "app", "posts", created.ID, databases.Principal{})
	require.Equal(t, codes.NotFound, status.Code(err))

	explicit, _, err := uc.CreateDocument(ctx, projectID, "app", "posts", "", map[string]any{"title": "explicit"}, []databases.Permission{
		{Type: "read", Role: "any"},
	}, principal, "")
	require.NoError(t, err)
	require.Len(t, explicit.Permissions, 1)
	require.Equal(t, "read", explicit.Permissions[0].Type)
	require.Equal(t, "any", explicit.Permissions[0].Role)
}

// TestDatabases_ListDocuments_NextPageToken (#5): NextPageToken 可续页且与 offset
// 语义一致、无重叠。
func TestDatabases_ListDocuments_NextPageToken(t *testing.T) {
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
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "n", Key: "n", Type: "integer"},
	}, nil, nil, true))

	const total = 12
	for i := 0; i < total; i++ {
		_, _, err := uc.CreateDocument(ctx, projectID, "app", "docs", fmt.Sprintf("doc-%04d", i), map[string]any{"n": i}, nil, principal, "")
		require.NoError(t, err)
	}

	page1res, err := uc.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		Queries:  []string{`orderAsc("$id")`, `limit(10)`},
		PageSize: 10,
	}, principal)
	require.NoError(t, err)
	page1, total1, next := page1res.Documents, page1res.TotalCount, page1res.NextPageToken
	require.Equal(t, int64(total), total1)
	require.Len(t, page1, 10)
	require.NotEmpty(t, next)
	ids1 := docIDsOf(page1)

	page2res, err := uc.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		Queries:   []string{`orderAsc("$id")`, `limit(10)`},
		PageSize:  10,
		PageToken: next,
	}, principal)
	require.NoError(t, err)
	page2, total2, next2 := page2res.Documents, page2res.TotalCount, page2res.NextPageToken
	// R5 J3-2（C2 阶段①收敛后对 keyset 续页生效）：续页不再执行精确 COUNT
	//（total=0=unknown，proto 合法语义）；精确 total 仅首页返回。
	require.Equal(t, int64(0), total2)
	require.Len(t, page2, 2)
	require.Empty(t, next2)
	ids2 := docIDsOf(page2)
	for _, id := range ids2 {
		require.NotContains(t, ids1, id, "page 2 must not overlap page 1")
	}

	// keyset-only（C2 阶段①）：首页 token 必为 ka:/kb: 形态；offset() 算子
	// 与旧 offset 族 token 一律拒绝。
	require.Contains(t, next, "ka:")
	_, err = uc.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "$id"}}, PageSize: 10, Offset: 10},
	}, principal)
	require.Error(t, err)
	require.Contains(t, status.Convert(err).Message(), "cursor pagination")
}

// TestDatabases_ListCollections_Pagination (#10): page_size/page_token 生效，
// NextPageToken 可续页，三页覆盖全部集合且无重叠。
func TestDatabases_ListCollections_Pagination(t *testing.T) {
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

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))

	const total = 12
	for i := 0; i < total; i++ {
		require.NoError(t, uc.CreateCollection(ctx, projectID, "app", fmt.Sprintf("coll_%02d", i), fmt.Sprintf("Collection %02d", i), nil, nil, nil, true))
		time.Sleep(2 * time.Millisecond)
	}

	var all []string
	pageToken := ""
	for page := 0; page < 3; page++ {
		pageColls, totalCount, next, err := uc.ListCollections(ctx, projectID, "app", databases.ListQuery{PageSize: 5, PageToken: pageToken})
		require.NoError(t, err)
		require.Equal(t, int64(total), totalCount)
		switch page {
		case 0, 1:
			require.Len(t, pageColls, 5)
			require.NotEmpty(t, next)
		case 2:
			require.Len(t, pageColls, 2)
			require.Empty(t, next)
		}
		for _, c := range pageColls {
			require.NotContains(t, all, c.ID, "collection must not repeat across pages")
			all = append(all, c.ID)
		}
		pageToken = next
	}
	require.Len(t, all, total)
}

// TestDatabases_CreateDocument_PermissionTemplates (#2a): user:{id}/group:{id}
// 模板在权限校验前替换为调用者真实角色并落库（B1 重写）：
// 场景 1 — 文档权限含 update:user:alice，alice/持有 user:alice 的调用者可更新权限
// （文档级优先下模板展开为 update:user:alice，update 检查命中）；
// 场景 2 — 文档权限仅含 read（无 update），更新权限应被拒（B1 文档级优先：
// "仅 read 权限即改权限" 不再被集合级 update 兜底放行）。
func TestDatabases_CreateDocument_PermissionTemplates(t *testing.T) {
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

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "user:alice"},
		{Type: "read", Role: "user:alice"},
		{Type: "update", Role: "user:alice"},
	}, true))

	userPrincipal := databases.Principal{Roles: []string{"users", "user:alice"}}
	perms, err := databases.ParsePermissionStrings([]string{"read:user:{id}", "update:user:{id}"})
	require.NoError(t, err)
	created, _, err := uc.CreateDocument(ctx, projectID, "app", "docs", "", map[string]any{"title": "t"}, perms, userPrincipal, "")
	require.NoError(t, err)
	require.Len(t, created.Permissions, 2)
	require.Equal(t, "read", created.Permissions[0].Type)
	require.Equal(t, "user:alice", created.Permissions[0].Role)
	require.Equal(t, "update", created.Permissions[1].Type)
	require.Equal(t, "user:alice", created.Permissions[1].Role)

	// 场景 1：文档权限含 update:user:alice → 持有 user:alice 的调用者可更新权限
	// （模板 group:{id} 展开为 group:t1）。
	groupPrincipal := databases.Principal{Roles: []string{"users", "user:alice", "group:t1"}}
	upPerms, err := databases.ParsePermissionStrings([]string{"update:group:{id}"})
	require.NoError(t, err)
	updated, _, err := uc.UpdateDocument(ctx, projectID, "app", "docs", created.ID, nil, upPerms, nil, nil, groupPrincipal, &created.Version, "")
	require.NoError(t, err)
	require.Len(t, updated.Permissions, 1)
	require.Equal(t, "update", updated.Permissions[0].Type)
	require.Equal(t, "group:t1", updated.Permissions[0].Role)

	// 场景 2：文档权限仅含 read（无 update）→ 更新权限被拒（B1 文档级优先，
	// 集合级 update:user:alice 不再兜底；grantable 校验使用 alice 持有的角色）。
	readOnly, _, err := uc.CreateDocument(ctx, projectID, "app", "docs", "", map[string]any{"title": "ro"}, []databases.Permission{
		{Type: "read", Role: "user:alice"},
	}, userPrincipal, "")
	require.NoError(t, err)
	ownUpPerms, err := databases.ParsePermissionStrings([]string{"update:user:{id}"})
	require.NoError(t, err)
	_, _, err = uc.UpdateDocument(ctx, projectID, "app", "docs", readOnly.ID, nil, ownUpPerms, nil, nil, userPrincipal, &readOnly.Version, "")
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// TestDatabases_BulkDocuments_MaxOperations (A4): app 层 Bulk 条数超上限
// （documents.MaxBulkOperations+1）→ InvalidArgument，不触达 docDB。
func TestDatabases_BulkDocuments_MaxOperations(t *testing.T) {
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
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "docs", "Docs", nil, nil, nil, true))

	ids := make([]string, documents.MaxBulkOperations+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("doc-%04d", i)
	}
	_, _, err := uc.BulkUpdateDocuments(ctx, projectID, "app", "docs", ids, map[string]any{"title": "x"}, nil, principal, "")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, _, err = uc.BulkDeleteDocuments(ctx, projectID, "app", "docs", ids, principal, "")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestDatabases_CreateDocument_TypeMismatch (A6): 写入类型不匹配（string 写入
// BIGINT 列）→ PG 22P02 → app 层 MapDocumentDBError 映射为 InvalidArgument。
func TestDatabases_CreateDocument_TypeMismatch(t *testing.T) {
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
		{ID: "views", Key: "views", Type: "integer"},
	}, nil, nil, true))

	_, _, err := uc.CreateDocument(ctx, projectID, "app", "posts", "", map[string]any{"views": "abc"}, nil, principal, "")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 合法写入不受影响。
	created, _, err := uc.CreateDocument(ctx, projectID, "app", "posts", "", map[string]any{"views": 42}, nil, principal, "")
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
}

func docIDsOf(docs []databases.Document) []string {
	out := make([]string, len(docs))
	for i := range docs {
		out[i] = docs[i].ID
	}
	return out
}

// TestDatabases_UpdateCollection 覆盖 Console「集合设置」路径：改名、停用/启用；
// 停用后非系统主体读写被拒，系统主体放行。
func TestDatabases_UpdateCollection(t *testing.T) {
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

	disabled := true
	require.NoError(t, uc.UpdateCollection(ctx, projectID, "app", "posts", databases.CollectionPatch{
		Name:     "Articles",
		Disabled: &disabled,
	}, principal))

	got, err := uc.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Articles", got.Name)
	require.True(t, got.Disabled)

	// 停用后普通主体读写被拒（PermissionDenied），系统主体不受影响。
	_, _, err = uc.CreateDocument(ctx, projectID, "app", "posts", "", map[string]any{"title": "x"}, nil, principal, "")
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, _, err = uc.CreateDocument(ctx, projectID, "app", "posts", "", map[string]any{"title": "x"}, nil, databases.SystemPrincipal, "")
	require.NoError(t, err)

	// 重新启用后恢复。
	disabled = false
	require.NoError(t, uc.UpdateCollection(ctx, projectID, "app", "posts", databases.CollectionPatch{
		Disabled: &disabled,
	}, principal))
	got, err = uc.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.False(t, got.Disabled)
	_, _, err = uc.CreateDocument(ctx, projectID, "app", "posts", "", map[string]any{"title": "y"}, nil, principal, "")
	require.NoError(t, err)
}

// TestDatabases_DeleteAttribute_DeleteIndex 覆盖 Schema 清理路径（B4 两段语义）：
// 删除 attribute = 置 deprecated（读屏蔽写拒收、契约条目仍在、同 key 重建拒绝），
// RetireAttribute = 物理删列（条目移除、可再建同名列）；删除 index 同步 DROP
// INDEX（可再建同名索引）。
func TestDatabases_DeleteAttribute_DeleteIndex(t *testing.T) {
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
		{ID: "views", Key: "views", Type: "integer"},
	}, nil, nil, true))
	require.NoError(t, uc.CreateIndex(ctx, projectID, "app", "posts", databases.Index{
		ID:         "idx_views",
		Type:       "key",
		Attributes: []string{"views"},
	}))

	require.NoError(t, uc.DeleteIndex(ctx, projectID, "app", "posts", "idx_views"))
	got, err := uc.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Len(t, got.Indexes, 0)

	// 段一：DeleteAttribute = deprecated（契约条目仍在）。
	require.NoError(t, uc.DeleteAttribute(ctx, projectID, "app", "posts", "views"))
	got, err = uc.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Len(t, got.Attributes, 2)
	require.Equal(t, databases.AttrStatusDeprecated, got.Attributes[1].StatusOrDefault())
	// deprecated 期间同 key 重建拒绝（条目仍在契约中）。
	err = uc.CreateAttribute(ctx, projectID, "app", "posts", databases.Attribute{
		ID: "views", Key: "views", Type: "integer",
	})
	require.Error(t, err)

	// 段二：RetireAttribute = 物理删列（条目移除）。
	require.NoError(t, uc.RetireAttribute(ctx, projectID, "app", "posts", "views"))
	got, err = uc.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Len(t, got.Attributes, 1)
	require.Equal(t, "title", got.Attributes[0].Key)

	// 删除后重建同名 attribute 与 index 成功（表结构已同步清理）。
	require.NoError(t, uc.CreateAttribute(ctx, projectID, "app", "posts", databases.Attribute{
		ID:   "views",
		Key:  "views",
		Type: "integer",
	}))
	require.NoError(t, uc.CreateIndex(ctx, projectID, "app", "posts", databases.Index{
		ID:         "idx_views",
		Type:       "key",
		Attributes: []string{"views"},
	}))
	got, err = uc.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Len(t, got.Attributes, 2)
	require.Len(t, got.Indexes, 1)

	// 新列可正常写入（验证 DROP COLUMN 后重建）。
	_, _, err = uc.CreateDocument(ctx, projectID, "app", "posts", "", map[string]any{"title": "t", "views": 7}, nil, principal, "")
	require.NoError(t, err)
}

// TestDatabases_Document_Increment 覆盖字段自增路径：原子增减、0 增量无效、
// 只传 increment 不覆盖 data。
func TestDatabases_Document_Increment(t *testing.T) {
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
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "stats", "Stats", []databases.Attribute{
		{ID: "views", Key: "views", Type: "integer"},
		{ID: "title", Key: "title", Type: "string", Size: 64},
	}, nil, nil, true))

	created, _, err := uc.CreateDocument(ctx, projectID, "app", "stats", "", map[string]any{"views": 10, "title": "hello"}, nil, principal, "")
	require.NoError(t, err)

	// +5 → 15
	updated, _, err := uc.UpdateDocument(ctx, projectID, "app", "stats", created.ID, nil, nil, map[string]int64{"views": 5}, nil, principal, &created.Version, "")
	require.NoError(t, err)
	require.EqualValues(t, 15, updated.Data["views"])
	require.Equal(t, "hello", updated.Data["title"], "increment 不覆盖其他字段")

	// -3 → 12
	updated, _, err = uc.UpdateDocument(ctx, projectID, "app", "stats", created.ID, nil, nil, map[string]int64{"views": -3}, nil, principal, &updated.Version, "")
	require.NoError(t, err)
	require.EqualValues(t, 12, updated.Data["views"])

	// 0 增量无字段可更新 → InvalidArgument（前端 Console 已过滤 0 增量）
	_, _, err = uc.UpdateDocument(ctx, projectID, "app", "stats", created.ID, nil, nil, map[string]int64{"views": 0}, nil, principal, &updated.Version, "")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 空 data/permissions/increment/array_updates 四选一校验
	_, _, err = uc.UpdateDocument(ctx, projectID, "app", "stats", created.ID, nil, nil, nil, nil, principal, &updated.Version, "")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestDatabases_DeleteAttribute_CascadesDependentIndex（B8 → B4 两段语义）：
// 删除属性（deprecated 段一）时依赖索引保留（可回滚）；RetireAttribute 段二
// 物理删列时同事务级联清理依赖索引（catalog 条目 + 物理 PG index）。
func TestDatabases_DeleteAttribute_CascadesDependentIndex(t *testing.T) {
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

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
	}, []databases.Index{
		{ID: "idx_views", Type: "key", Attributes: []string{"views"}},
	}, nil, true))

	// 段一：deprecated——可回滚语义，依赖索引保留。
	require.NoError(t, uc.DeleteAttribute(ctx, projectID, "app", "posts", "views"))
	got, err := uc.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Len(t, got.Indexes, 1, "段一 deprecated 保留依赖索引（可回滚）")

	// 段二：retire——物理删列 + 级联清理依赖索引（catalog 条目 + 物理索引）。
	require.NoError(t, uc.RetireAttribute(ctx, projectID, "app", "posts", "views"))
	got, err = uc.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Len(t, got.Indexes, 0, "依赖索引应随 retire 级联删除")
	require.Len(t, got.Attributes, 1)

	// 同名索引可重建（catalog 行与物理索引均已清理）。
	require.NoError(t, uc.CreateIndex(ctx, projectID, "app", "posts", databases.Index{
		ID:         "idx_views",
		Type:       "key",
		Attributes: []string{"title"},
	}))
	got, err = uc.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Len(t, got.Indexes, 1)
}
