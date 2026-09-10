package documentdb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/app/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/events"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

func TestPostgresDocumentDatabase_CRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)

	// Create a custom database and collection.
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "notes", "Notes", nil, nil, nil, true))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
	}, []databases.Index{
		{ID: "title_key", Type: "key", Attributes: []string{"title"}},
	}, nil, true))

	// Create document.
	created, err := docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
		Data: map[string]any{
			"title": "Hello World",
			"views": 42,
		},
	}, []databases.Permission{
		{Type: "read", Role: "any"},
		{Type: "update", Role: "any"},
		{Type: "delete", Role: "any"},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)

	// Get document.
	got, err := docDB.GetDocument(ctx, projectID, "app", "posts", created.ID, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Equal(t, "Hello World", got.Data["title"])

	// Update document.
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "posts", databases.DocumentUpdate{
		Document: databases.Document{
			ID: got.ID,
			Data: map[string]any{
				"views": 100,
			},
		},
		ExpectedVersion: got.Version,
	}, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Equal(t, float64(100), updated.Data["views"])

	// List with typed AST query（C7 单 AST）.
	list, err := docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Gt("views", "50"), Orders: []query.Order{{Attribute: "$createdAt", Desc: true}}},
	}, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	require.Equal(t, int64(1), list.TotalCount)

	// Count.
	count, err := docDB.CountDocuments(ctx, projectID, "app", "posts", databases.Query{AST: &query.Query{Filter: query.Eq("title", "Hello World")}}, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Equal(t, int64(1), count)

	// Delete.
	require.NoError(t, docDB.DeleteDocument(ctx, projectID, "app", "posts", created.ID, databases.DeleteOptions{ExpectedVersion: updated.Version}, databases.Principal{Roles: []string{"any"}}))
	got2, err := docDB.GetDocument(ctx, projectID, "app", "posts", created.ID, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Nil(t, got2)
}

// TestPostgresDocumentDatabase_UpsertDocument (T2): UpsertDocument inserts when
// no row matches the conflict columns and updates (data/_updated_at/_updated_by
// + permissions replaced) when one does. conflictColumns must match a unique
// index on the collection table.
func TestPostgresDocumentDatabase_UpsertDocument(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "members", "Members", []databases.Attribute{
		{ID: "channel_id", Key: "channel_id", Type: "string", Size: 64},
		{ID: "user_id", Key: "user_id", Type: "string", Size: 64},
		{ID: "last_read_seq", Key: "last_read_seq", Type: "integer"},
	}, []databases.Index{
		{ID: "member_key", Type: "unique", Attributes: []string{"channel_id", "user_id"}},
	}, nil, true))

	// First upsert: no matching row → insert.
	upserted, err := docDB.UpsertDocument(ctx, projectID, "app", "members", databases.Document{
		ID: "m1",
		Data: map[string]any{
			"channel_id":    "ch1",
			"user_id":       "u1",
			"last_read_seq": 10,
		},
	}, []string{"channel_id", "user_id"}, []databases.Permission{
		{Type: "read", Role: "user:u1"},
		{Type: "update", Role: "user:u1"},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, float64(10), upserted.Data["last_read_seq"])

	// Second upsert: row matches the conflict columns → update.
	upserted, err = docDB.UpsertDocument(ctx, projectID, "app", "members", databases.Document{
		ID: "m1",
		Data: map[string]any{
			"channel_id":    "ch1",
			"user_id":       "u1",
			"last_read_seq": 42,
		},
	}, []string{"channel_id", "user_id"}, []databases.Permission{
		{Type: "read", Role: "user:u1"},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, float64(42), upserted.Data["last_read_seq"])

	// GetDocument confirms the data was updated and permissions replaced.
	got, err := docDB.GetDocument(ctx, projectID, "app", "members", "m1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, float64(42), got.Data["last_read_seq"])
	require.Len(t, got.Permissions, 1)
	require.Equal(t, databases.Permission{Type: "read", Role: "user:u1"}, got.Permissions[0])
}

// TestPostgresDocumentDatabase_UpsertDocument_KeyseedPermsReadable：keys 主体以
// 文档级 read/update/delete:keys（app 层 seedDocumentPermissions 的种子形态）走
// upsert 插入支与更新支后，读回与修改不依赖集合级权限（回归配套：app 层原空 ACE
// 种子 read:__private__ 会锁死文档，本测试锁定 infra 侧 _perms 持久化与判定链路）。
func TestPostgresDocumentDatabase_UpsertDocument_KeyseedPermsReadable(t *testing.T) {
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
	// 集合仅授 create:keys（无 read）：读回必须依赖文档级种子 ACE。
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "members", "Members", []databases.Attribute{
		{ID: "email", Key: "email", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "email_key", Type: "unique", Attributes: []string{"email"}},
	}, []databases.Permission{
		{Type: "create", Role: "keys"},
	}, true))

	keys := databases.Principal{Roles: []string{"keys"}}
	seed := []databases.Permission{
		{Type: "read", Role: "keys"},
		{Type: "update", Role: "keys"},
		{Type: "delete", Role: "keys"},
	}

	// 插入支：keys 可读回、可改。
	upserted, err := docDB.UpsertDocument(ctx, projectID, "app", "members", databases.Document{
		ID:   "m1",
		Data: map[string]any{"email": "k@example.com"},
	}, []string{"email"}, seed, keys)
	require.NoError(t, err)
	require.Equal(t, "m1", upserted.ID)

	got, err := docDB.GetDocument(ctx, projectID, "app", "members", "m1", keys)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "k@example.com", got.Data["email"])

	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "members", databases.DocumentUpdate{
		Document:        databases.Document{ID: "m1", Data: map[string]any{"email": "k2@example.com"}},
		ExpectedVersion: got.Version,
	}, keys)
	require.NoError(t, err)
	require.Equal(t, "k2@example.com", updated.Data["email"])

	// 更新支：conflict 命中已有行，同种子 perms 替换后仍可读、可改。
	updatedAgain, err := docDB.UpsertDocument(ctx, projectID, "app", "members", databases.Document{
		ID:   "m2",
		Data: map[string]any{"email": "k2@example.com"},
	}, []string{"email"}, seed, keys)
	require.NoError(t, err)
	require.Equal(t, "m1", updatedAgain.ID)
	require.Equal(t, int64(3), updatedAgain.Version)

	gotAgain, err := docDB.GetDocument(ctx, projectID, "app", "members", "m1", keys)
	require.NoError(t, err)
	require.NotNil(t, gotAgain)

	_, err = docDB.UpdateDocument(ctx, projectID, "app", "members", databases.DocumentUpdate{
		Document:        databases.Document{ID: "m1", Data: map[string]any{"email": "k3@example.com"}},
		ExpectedVersion: gotAgain.Version,
	}, keys)
	require.NoError(t, err)
}

// TestPostgresDocumentDatabase_IdentifierLengthGuard：infra 层长度二道防线——
// 直调 adapter（绕过 app 层校验）也不得让超长标识符到达 PG（63 字节静默截断
// 会让两个仅超长部分不同的名字映射同一物理对象）。
func TestPostgresDocumentDatabase_IdentifierLengthGuard(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	// 64 字节集合 ID（超 PG 63）。
	err := docDB.CreateCollection(ctx, projectID, "app", strings.Repeat("c", 64), "Too long", nil, nil, nil, true)
	requireInvalidArg(t, err)

	// 64 字节属性 key。
	err = docDB.CreateAttribute(ctx, projectID, "app", "posts", databases.Attribute{
		ID: strings.Repeat("k", 64), Key: strings.Repeat("k", 64), Type: "string", Size: 64,
	})
	requireInvalidArg(t, err)

	// 索引名拼接超限：coll=posts（5）+ idx=58 → idx_posts_<58> = 68 > 63。
	err = docDB.CreateIndex(ctx, projectID, "app", "posts", databases.Index{
		ID: strings.Repeat("i", 58), Type: "key", Attributes: []string{"title"},
	})
	requireInvalidArg(t, err)

	// CreateCollection 内联索引的组合校验（各段 ≤63 但拼接 >63）。
	err = docDB.CreateCollection(ctx, projectID, "app", strings.Repeat("c", 40), "Combo", nil, []databases.Index{
		{ID: strings.Repeat("i", 40), Type: "key", Attributes: []string{"x"}},
	}, nil, true)
	requireInvalidArg(t, err)
}

func requireInvalidArg(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "error should carry gRPC status, got %v", err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

// TestPostgresDocumentDocuments_KeyAttribution：API key 主体的写入归因——
// _created_by/_updated_by 落 "key:<id>"（redesign §10.2-1；原实现 keys-only
// 主体审计列为空，Agent 行为不可追责）。user 主体语义不变（裸 id）。
func TestPostgresDocumentDocuments_KeyAttribution(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 64},
	}, nil, []databases.Permission{
		{Type: "create", Role: "keys"},
		{Type: "read", Role: "keys"},
		{Type: "update", Role: "keys"},
		{Type: "create", Role: "any"},
	}, true))

	keys := databases.Principal{Roles: []string{"keys"}, KeyID: "k123"}
	created, err := docDB.CreateDocument(ctx, projectID, "app", "notes", databases.Document{
		Data: map[string]any{"title": "by agent"},
	}, nil, keys)
	require.NoError(t, err)
	require.Equal(t, "key:k123", created.CreatedBy)
	require.Equal(t, "key:k123", created.UpdatedBy)

	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "notes", databases.DocumentUpdate{
		Document:        databases.Document{ID: created.ID, Data: map[string]any{"title": "by agent v2"}},
		ExpectedVersion: created.Version,
	}, keys)
	require.NoError(t, err)
	require.Equal(t, "key:k123", updated.CreatedBy)
	require.Equal(t, "key:k123", updated.UpdatedBy)

	// user 主体语义不变：存裸 user id（Create 同时落 _created_by/_updated_by）。
	user := databases.Principal{Roles: []string{"user:u1"}}
	createdUser, err := docDB.CreateDocument(ctx, projectID, "app", "notes", databases.Document{
		Data: map[string]any{"title": "by user"},
	}, []databases.Permission{
		{Type: "read", Role: "user:u1"},
		{Type: "update", Role: "user:u1"},
	}, user)
	require.NoError(t, err)
	require.Equal(t, "u1", createdUser.CreatedBy)
	require.Equal(t, "u1", createdUser.UpdatedBy)
}

// 单元：归因优先级——user: 角色优先于 KeyID。
func TestUserIDFromPrincipal_Priority(t *testing.T) {
	require.Equal(t, "u1", userIDFromPrincipal(databases.Principal{
		Roles: []string{"keys", "user:u1"}, KeyID: "k9",
	}))
	require.Equal(t, "key:k9", userIDFromPrincipal(databases.Principal{
		Roles: []string{"keys"}, KeyID: "k9",
	}))
	require.Empty(t, userIDFromPrincipal(databases.Principal{Roles: []string{"keys"}}))
}

// 单元：conflictColumns 必须无序命中一个 unique 索引；key/非 unique 类型不算。
func TestValidateConflictColumns(t *testing.T) {
	coll := &databases.Collection{ID: "members", Indexes: []databases.Index{
		{ID: "uq", Type: "unique", Attributes: []string{"channel_id", "user_id"}},
		{ID: "plain", Type: "key", Attributes: []string{"email"}},
	}}
	require.NoError(t, validateConflictColumns(coll, []string{"user_id", "channel_id"}))
	require.NoError(t, validateConflictColumns(coll, []string{"channel_id", "user_id"}))

	err := validateConflictColumns(coll, []string{"email"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unique index")

	// 子集/超集不算命中。
	require.Error(t, validateConflictColumns(coll, []string{"channel_id"}))
	require.Error(t, validateConflictColumns(coll, []string{"channel_id", "user_id", "extra"}))
	require.NoError(t, validateConflictColumns(nil, []string{"any"}))
}

// 集成：非 Bypass 主体 upsert 的 conflictColumns 未命中 unique 索引 →
// InvalidArgument（前置校验，先于 PG 42P10）。
func TestPostgresDocumentDatabase_UpsertConflictColumnsPrecheck(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "members", "Members", []databases.Attribute{
		{ID: "email", Key: "email", Type: "string", Size: 256},
		{ID: "name", Key: "name", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "uq_email", Type: "unique", Attributes: []string{"email"}},
	}, []databases.Permission{
		{Type: "create", Role: "keys"},
	}, true))

	keys := databases.Principal{Roles: []string{"keys"}, KeyID: "k1"}
	_, err := docDB.UpsertDocument(ctx, projectID, "app", "members", databases.Document{
		ID:   "m1",
		Data: map[string]any{"email": "a@b.c", "name": "n"},
	}, []string{"name"}, nil, keys)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
	require.Contains(t, st.Message(), "unique index")

	// 命中 unique 索引：正常通过。
	_, err = docDB.UpsertDocument(ctx, projectID, "app", "members", databases.Document{
		ID:   "m1",
		Data: map[string]any{"email": "a@b.c", "name": "n"},
	}, []string{"email"}, nil, keys)
	require.NoError(t, err)
}

// TestPostgresDocumentDocuments_MultiKeyCursor（C2 完成态）：多排序键完整
// keyset 游标——ORDER BY = 全部排序键 + _id tiebreaker（方向随首键）；统一
// 方向走行比较谓词、混合方向走逐键 OR 展开；cursor 以 docID 定位 + 服务端
// 查行取全部键值；跨页不丢不重（确定性用例：同键多行 + 混合方向）。
func TestPostgresDocumentDocuments_MultiKeyCursor(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "items", "Items", []databases.Attribute{
		{ID: "priority", Key: "priority", Type: "integer"},
		{ID: "title", Key: "title", Type: "string", Size: 64},
		{ID: "seq", Key: "seq", Type: "integer"},
	}, nil, []databases.Permission{
		{Type: "create", Role: "any"},
		{Type: "read", Role: "any"},
	}, true))

	anyReader := databases.Principal{Roles: []string{"any"}}
	// priority 有重复（同键多行跨页是 keyset 的难点），title/seq 全序互补。
	docs := [][2]any{
		{7, "t0"}, {7, "t1"}, {7, "t2"},
		{5, "t3"}, {5, "t4"},
		{3, "t5"}, {3, "t6"}, {3, "t7"},
	}
	for i, d := range docs {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "items", databases.Document{
			Data: map[string]any{"priority": d[0], "title": d[1], "seq": i},
		}, nil, anyReader)
		require.NoError(t, err)
	}

	// paginate 用给定 orders 全量翻页，返回按序 docID 拼起来的序列。
	paginate := func(orders []query.Order) []string {
		t.Helper()
		var got []string
		q := databases.Query{AST: &query.Query{Orders: orders, PageSize: 2}}
		for {
			page, err := docDB.ListDocuments(ctx, projectID, "app", "items", q, anyReader)
			require.NoError(t, err)
			for _, d := range page.Documents {
				got = append(got, fmt.Sprintf("%v/%v", d.Data["title"], d.Data["seq"]))
			}
			if page.NextPageToken == "" || len(page.Documents) == 0 {
				break
			}
			q.PageToken = page.NextPageToken
		}
		return got
	}
	// 直接读全序（单页大 limit）作为期望值。
	full := func(orders []query.Order) []string {
		t.Helper()
		page, err := docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{
			AST: &query.Query{Orders: orders, PageSize: 100},
		}, anyReader)
		require.NoError(t, err)
		out := make([]string, 0, len(page.Documents))
		for _, d := range page.Documents {
			out = append(out, fmt.Sprintf("%v/%v", d.Data["title"], d.Data["seq"]))
		}
		return out
	}

	seen := func(seq []string) bool { // 不丢不重
		uniq := map[string]int{}
		for _, s := range seq {
			uniq[s]++
		}
		return len(uniq) == len(docs) && func() bool {
			for _, c := range uniq {
				if c != 1 {
					return false
				}
			}
			return true
		}()
	}

	// ① 双键统一方向（DESC+DESC → 行比较谓词）。
	uniformOrders := []query.Order{{Attribute: "priority", Desc: true}, {Attribute: "title", Desc: true}}
	require.Equal(t, full(uniformOrders), paginate(uniformOrders))
	require.True(t, seen(paginate(uniformOrders)), "统一方向跨页不丢不重")

	// ② 双键混合方向（priority DESC + title ASC → OR 展开谓词）。
	mixedOrders := []query.Order{{Attribute: "priority", Desc: true}, {Attribute: "title"}}
	require.Equal(t, full(mixedOrders), paginate(mixedOrders))
	require.True(t, seen(paginate(mixedOrders)), "混合方向跨页不丢不重")

	// ③ 三键混合方向（priority ASC + title DESC + seq ASC）。
	triOrders := []query.Order{{Attribute: "priority"}, {Attribute: "title", Desc: true}, {Attribute: "seq"}}
	require.Equal(t, full(triOrders), paginate(triOrders))
	require.True(t, seen(paginate(triOrders)), "三键混合方向跨页不丢不重")

	// 单键 cursor 路径回归（独立单键链路：单键首页 + 单键续页）。
	singlePage1, err := docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "priority", Desc: true}}, PageSize: 2},
	}, anyReader)
	require.NoError(t, err)
	require.Len(t, singlePage1.Documents, 2)
	singleLast := singlePage1.Documents[len(singlePage1.Documents)-1].ID
	single, err := docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "priority", Desc: true}}, PageSize: 2, CursorAfter: singleLast},
	}, anyReader)
	require.NoError(t, err)
	require.NotEmpty(t, single.Documents)
}

// TestPostgresDocumentDocuments_NullCursorKeyRejected（预决策 4）：cursor 行
// 的排序键值含 NULL → InvalidArgument（消息明示先过滤）；不静默产出丢行
// 的续页。
func TestPostgresDocumentDocuments_NullCursorKeyRejected(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "items", "Items", []databases.Attribute{
		{ID: "priority", Key: "priority", Type: "integer"},
	}, nil, []databases.Permission{
		{Type: "create", Role: "any"},
		{Type: "read", Role: "any"},
	}, true))

	anyReader := databases.Principal{Roles: []string{"any"}}
	// n1 的 priority 未设置（列保持 NULL）。
	_, err := docDB.CreateDocument(ctx, projectID, "app", "items", databases.Document{
		ID:   "n1",
		Data: map[string]any{},
	}, nil, anyReader)
	require.NoError(t, err)
	_, err = docDB.CreateDocument(ctx, projectID, "app", "items", databases.Document{
		ID:   "n2",
		Data: map[string]any{"priority": 1},
	}, nil, anyReader)
	require.NoError(t, err)

	// cursor 定位在 NULL 键行 → InvalidArgument，消息提示先过滤。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "priority"}}, PageSize: 1, CursorAfter: "n1"},
	}, anyReader)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "NULL")
	require.Contains(t, status.Convert(err).Message(), "filter")

	// 过滤后（isNotNull）正常分页。
	page, err := docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{
		AST: &query.Query{Filter: query.IsNotNull("priority"), Orders: []query.Order{{Attribute: "priority"}}, PageSize: 5},
	}, anyReader)
	require.NoError(t, err)
	require.Len(t, page.Documents, 1)
	require.Equal(t, "n2", page.Documents[0].ID)
}

// TestPostgresDocumentDatabase_AttributeDefaultValueCatalog：default 与 DDL 同源
// 落 catalog 且 GetCollection/ListCollections 读回（回归：物理列 DEFAULT 生效但
// catalog 不落库、读不回，契约断裂）。
func TestPostgresDocumentDatabase_AttributeDefaultValueCatalog(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 128, Default: "untitled"},
		{ID: "views", Key: "views", Type: "integer", Default: 42},
	}, nil, nil, true))

	findAttr := func(coll *databases.Collection, key string) databases.Attribute {
		for _, a := range coll.Attributes {
			if a.Key == key {
				return a
			}
		}
		t.Fatalf("attribute %q not found", key)
		return databases.Attribute{}
	}

	got, err := docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	// JSONB 合一列保留 default 的标量类型（string/int/bool/float 读写一致）；
	// API 面（proto string default_value）经 fmt.Sprint 输出不变。
	title := findAttr(got, "title")
	require.Equal(t, "untitled", title.Default)
	require.Equal(t, 128, title.Size, "size 经 JSONB 契约读回")
	views := findAttr(got, "views")
	require.InDelta(t, 42, views.Default, 0, "integer default 读回保持数值")
	require.Equal(t, "42", fmt.Sprint(views.Default), "API 面字符串形态不变")

	// CreateAttribute 路径同样落库并读回。
	require.NoError(t, docDB.CreateAttribute(ctx, projectID, "app", "posts", databases.Attribute{
		ID: "pinned", Key: "pinned", Type: "boolean", Default: true,
	}))
	got, err = docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Equal(t, true, findAttr(got, "pinned").Default)
	require.Equal(t, "true", fmt.Sprint(findAttr(got, "pinned").Default))

	// ListCollections 路径读回。
	list, _, err := docDB.ListCollections(ctx, projectID, "app", databases.ListQuery{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "untitled", findAttr(&list[0], "title").Default)
	require.Equal(t, 128, findAttr(&list[0], "title").Size)
}

// TestPostgresDocumentDatabase_UpdateCollection_AtomicityAndAudit：权限替换与
// 字段更新同事务生效；权限-only 变更同样刷 updated_at（原实现两条 UPDATE 非
// 同事务、权限-only 不刷审计列）。
func TestPostgresDocumentDatabase_UpdateCollection_AtomicityAndAudit(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", nil, nil, []databases.Permission{
		{Type: "read", Role: "any"},
	}, true))

	before, err := docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)

	// 权限 + 字段一次提交：两者都生效。
	require.NoError(t, docDB.UpdateCollection(ctx, projectID, "app", "posts", databases.CollectionPatch{
		Name:        "Renamed",
		Permissions: &[]databases.Permission{{Type: "read", Role: "keys"}},
	}))
	got, err := docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Equal(t, "Renamed", got.Name)
	require.Equal(t, []databases.Permission{{Type: "read", Role: "keys"}}, got.Permissions)
	require.True(t, got.UpdatedAt.After(before.UpdatedAt), "field update must bump updated_at")

	// 权限-only：同样刷 updated_at。
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, docDB.UpdateCollection(ctx, projectID, "app", "posts", databases.CollectionPatch{
		Permissions: &[]databases.Permission{{Type: "read", Role: "users"}},
	}))
	got2, err := docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Equal(t, []databases.Permission{{Type: "read", Role: "users"}}, got2.Permissions)
	require.True(t, got2.UpdatedAt.After(got.UpdatedAt), "permission-only update must bump updated_at")

	// 空 patch：no-op，不刷审计列。
	require.NoError(t, docDB.UpdateCollection(ctx, projectID, "app", "posts", databases.CollectionPatch{}))
	got3, err := docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Equal(t, got2.UpdatedAt, got3.UpdatedAt)
}

// TestPostgresDocumentDocuments_TiebreakerPagination：自定义排序键全部相同的多行，
// offset 续页与 keyset cursor 续页都必须不丢不重（重复排序键的全序由 _id
// tiebreaker 保证；offset 路径曾缺 _id 导致跨页丢行）。
func TestPostgresDocumentDocuments_TiebreakerPagination(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "items", "Items", []databases.Attribute{
		{ID: "priority", Key: "priority", Type: "integer"},
		{ID: "title", Key: "title", Type: "string", Size: 64},
	}, nil, []databases.Permission{
		{Type: "read", Role: "any"},
		{Type: "create", Role: "any"},
	}, true))

	anyReader := databases.Principal{Roles: []string{"any"}}
	const n = 5
	inserted := map[string]bool{}
	for i := 0; i < n; i++ {
		created, err := docDB.CreateDocument(ctx, projectID, "app", "items", databases.Document{
			Data: map[string]any{"priority": 7, "title": fmt.Sprintf("t%d", i)},
		}, nil, anyReader)
		require.NoError(t, err)
		inserted[created.ID] = true
	}

	collect := func(pages [][]databases.Document) map[string]int {
		seen := map[string]int{}
		for _, docs := range pages {
			for _, d := range docs {
				seen[d.ID]++
			}
		}
		return seen
	}
	assertNoLossNoDup := func(t *testing.T, seen map[string]int) {
		t.Helper()
		require.Len(t, seen, n, "pagination must return every document exactly once")
		for id, c := range seen {
			require.Equalf(t, 1, c, "document %s appeared %d times across pages", id, c)
		}
	}

	// keyset cursor 续页：首页 + cursorAfter 翻完。
	var keysetPages [][]databases.Document
	page1, err := docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "priority", Desc: true}}, PageSize: 2},
	}, anyReader)
	require.NoError(t, err)
	require.Len(t, page1.Documents, 2)
	keysetPages = append(keysetPages, page1.Documents)

	last := page1.Documents[len(page1.Documents)-1].ID
	for i := 0; i < 3; i++ {
		page, err := docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{
			AST: &query.Query{Orders: []query.Order{{Attribute: "priority", Desc: true}}, PageSize: 2, CursorAfter: last},
		}, anyReader)
		require.NoError(t, err)
		if len(page.Documents) == 0 {
			break
		}
		keysetPages = append(keysetPages, page.Documents)
		last = page.Documents[len(page.Documents)-1].ID
	}
	assertNoLossNoDup(t, collect(keysetPages))

	// keyset token 续页（C2 阶段①收敛后的唯一续页形态）：首页满页发 ka:
	// token，token 翻完全集；不再有 offset 续页路径。
	var tokenPages [][]databases.Document
	tp, err := docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "priority", Desc: true}}, PageSize: 2},
	}, anyReader)
	require.NoError(t, err)
	require.Len(t, tp.Documents, 2)
	require.NotEmpty(t, tp.NextPageToken, "满页必须发续页 token")
	require.Contains(t, tp.NextPageToken, "ka:", "keyset-only：只发 ka:/kb: token")
	tokenPages = append(tokenPages, tp.Documents)
	for tp.NextPageToken != "" {
		tp, err = docDB.ListDocuments(ctx, projectID, "app", "items", databases.Query{
			AST:       &query.Query{Orders: []query.Order{{Attribute: "priority", Desc: true}}, PageSize: 2},
			PageToken: tp.NextPageToken,
		}, anyReader)
		require.NoError(t, err)
		tokenPages = append(tokenPages, tp.Documents)
	}
	assertNoLossNoDup(t, collect(tokenPages))
}

// TestPostgresDocumentDatabase_UpsertDocument_PrivilegeEscalationRejected
// (P0-1): a principal holding only collection-level create must not be able to
// update another user's row by submitting a new _id whose conflict columns
// collide with an existing row.
func TestPostgresDocumentDatabase_UpsertDocument_PrivilegeEscalationRejected(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "users", "Users", []databases.Attribute{
		{ID: "email", Key: "email", Type: "string", Size: 256},
		{ID: "name", Key: "name", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "email_key", Type: "unique", Attributes: []string{"email"}},
	}, []databases.Permission{
		// 集合仅授予 create:users：攻击者（users）可创建新文档，但没有任何
		// update 权限——越权路径若命中他人行，必须被文档级 update 校验拦下。
		{Type: "create", Role: "users"},
	}, true))

	// 文档 A：系统主体创建，权限只授予 owner，不含攻击者。
	_, err := docDB.CreateDocument(ctx, projectID, "app", "users", databases.Document{
		ID: "doc-a",
		Data: map[string]any{
			"email": "a@x.com",
			"name":  "original",
		},
	}, []databases.Permission{
		{Type: "read", Role: "user:owner"},
		{Type: "update", Role: "user:owner"},
		{Type: "delete", Role: "user:owner"},
	}, databases.SystemPrincipal)
	require.NoError(t, err)

	// 攻击者：仅持集合级 create 权限（默认 create:users），无任何文档级权限。
	attacker := databases.Principal{Roles: []string{"users"}}
	_, err = docDB.UpsertDocument(ctx, projectID, "app", "users", databases.Document{
		ID: "doc-b",
		Data: map[string]any{
			"email": "a@x.com", // 命中 doc-a 的唯一索引 → 实际会 UPDATE doc-a
			"name":  "hacked",
		},
	}, []string{"email"}, nil, attacker)
	// 阶段③包 C：doc-a 对攻击者不可见（预查经 SELECT policy 过滤 ⇒ 视为纯
	// 插入）→ 撞唯一索引被拒。安全不变量不变：写入被拒；错误码从
	// PermissionDenied 变为 DuplicateKey（唯一键存在性泄露是 §3.2 #13 已知
	// 豁免面）。
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPermissionDenied) || errors.Is(err, ErrDuplicateKey),
		"越权 upsert 必须被拒（PermissionDenied 或经唯一索引 DuplicateKey），got: %v", err)

	// 文档 A 必须未被修改。
	got, err := docDB.GetDocument(ctx, projectID, "app", "users", "doc-a", databases.SystemPrincipal)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "original", got.Data["name"])
}

// TestPostgresDocumentDatabase_UpsertDocument_ConcurrentRace (P0-1 并发版)：
// victim 与 attacker 并发 upsert 同一冲突值。advisory lock 串行化后：
// attacker 先执行 → 插入自己的行，随后 victim 的 upsert 覆盖为 victim 数据；
// attacker 后执行 → 预查命中 victim 行 → update 权限拒绝（ErrPermissionDenied）。
// 两种交错下最终数据都必须保持 victim 的值，attacker 无法改写。
func TestPostgresDocumentDatabase_UpsertDocument_ConcurrentRace(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "users", "Users", []databases.Attribute{
		{ID: "email", Key: "email", Type: "string", Size: 256},
		{ID: "name", Key: "name", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "email_key", Type: "unique", Attributes: []string{"email"}},
	}, []databases.Permission{
		{Type: "create", Role: "users"},
	}, true))

	start := make(chan struct{})
	errCh := make(chan error, 2)
	victim := func() {
		<-start
		_, err := docDB.UpsertDocument(ctx, projectID, "app", "users", databases.Document{
			ID:   "victim-doc",
			Data: map[string]any{"email": "race@x.com", "name": "original"},
		}, []string{"email"}, nil, databases.SystemPrincipal)
		errCh <- err
	}
	attacker := func() {
		<-start
		_, err := docDB.UpsertDocument(ctx, projectID, "app", "users", databases.Document{
			ID:   "attacker-doc",
			Data: map[string]any{"email": "race@x.com", "name": "hacked"},
		}, []string{"email"}, nil, databases.Principal{Roles: []string{"users"}})
		errCh <- err
	}
	go victim()
	go attacker()
	close(start)
	victimErr := <-errCh
	attackerErr := <-errCh

	require.NoError(t, victimErr)
	if attackerErr != nil {
		// 阶段③包 C 语义（与上方顺序版用例一致）：attacker 预查经 SELECT
		// policy 过滤看不到 victim 行 ⇒ 走纯插入 ⇒ 撞唯一索引 DuplicateKey；
		// 预查可见时为 UPDATE 权限拒绝 ErrPermissionDenied。两种交错下写入
		// 均被拒，最终数据保持 victim 的值。
		require.True(t, errors.Is(attackerErr, ErrPermissionDenied) || errors.Is(attackerErr, ErrDuplicateKey),
			"attacker 并发 upsert 必须被拒（PermissionDenied 或经唯一索引 DuplicateKey），got: %v", attackerErr)
	}

	// 唯一键保证集合内仅一行，且数据必须保持 victim 的值。
	list, err := docDB.ListDocuments(ctx, projectID, "app", "users", databases.Query{
		AST: &query.Query{Filter: query.Eq("email", "race@x.com")},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	require.Equal(t, "original", list.Documents[0].Data["name"])
}

func TestPostgresDocumentDatabase_Permissions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, testutil.SeedSystemDocumentCollections(ctx, db, docDB, projectID))

	created, err := docDB.CreateDocument(ctx, projectID, databases.SystemDatabaseID, "users", databases.Document{
		Data: map[string]any{
			"email": "perm@torchwood.local",
			"name":  "Permission Test",
		},
	}, []databases.Permission{
		{Type: "read", Role: "user:alice"},
	}, databases.SystemPrincipal)
	require.NoError(t, err)

	// User without permission cannot read.
	list, err := docDB.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "users", databases.Query{
		AST: &query.Query{Filter: query.Eq("$id", created.ID)},
	}, databases.Principal{Roles: []string{"user:bob"}})
	require.NoError(t, err)
	require.Len(t, list.Documents, 0)

	// User with permission can read.
	list, err = docDB.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "users", databases.Query{
		AST: &query.Query{Filter: query.Eq("$id", created.ID)},
	}, databases.Principal{Roles: []string{"user:alice"}})
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)

	// System roles bypass permissions.
	list, err = docDB.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "users", databases.Query{}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(list.Documents), 1)

	// Get without permission is denied.
	_, err = docDB.GetDocument(ctx, projectID, databases.SystemDatabaseID, "users", created.ID, databases.Principal{Roles: []string{"user:bob"}})
	require.ErrorIs(t, err, ErrPermissionDenied)

	// Get with permission succeeds.
	got, err := docDB.GetDocument(ctx, projectID, databases.SystemDatabaseID, "users", created.ID, databases.Principal{Roles: []string{"user:alice"}})
	require.NoError(t, err)
	require.Equal(t, "perm@torchwood.local", got.Data["email"])
}

func TestCatalog_NoSentinelSystemCollections_MultipleProjects(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	docDB := NewPostgresDocumentDB(db, nil)

	projectA, _, cleanupA := testutil.CreateTestProject(ctx, db)
	defer cleanupA()
	projectB, _, cleanupB := testutil.CreateTestProject(ctx, db)
	defer cleanupB()

	collA, err := docDB.GetCollection(ctx, projectA, databases.SystemDatabaseID, "users")
	require.NoError(t, err)
	require.Nil(t, collA)

	collB, err := docDB.GetCollection(ctx, projectB, databases.SystemDatabaseID, "users")
	require.NoError(t, err)
	require.Nil(t, collB)
}

// TestErrDuplicateKey_DomainAlias (#8): infra alias must be the same instance
// as the domain error so errors.Is comparisons keep working.
func TestErrDuplicateKey_DomainAlias(t *testing.T) {
	require.Equal(t, databases.ErrDuplicateKey, ErrDuplicateKey)
	require.True(t, errors.Is(ErrDuplicateKey, databases.ErrDuplicateKey))
	require.True(t, errors.Is(ErrDuplicateKey, databases.ErrDuplicateKey))
}

// TestDeleteCollection_CleansPerms (#3): deleting a collection must remove its
// document-level permissions so that recreating the same collection cannot leak
// old permissions onto new documents. 阶段③包 A：_acl 内嵌行内，DROP TABLE 即
// 随行消亡——本测试退化为行为级防回归（重建后旧 ACE 不得复活）+ 物理表消亡
// 的结构性断言。
func TestDeleteCollection_CleansPerms(t *testing.T) {
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

	createColl := func() {
		t.Helper()
		require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "notes", "Notes", []databases.Attribute{
			{ID: "title", Key: "title", Type: "string", Size: 256},
		}, nil, nil, true))
	}

	createColl()
	_, err := docDB.CreateDocument(ctx, projectID, "app", "notes", databases.Document{
		ID:   "doc-1",
		Data: map[string]any{"title": "secret"},
	}, []databases.Permission{{Type: "read", Role: "user:alice"}}, databases.SystemPrincipal)
	require.NoError(t, err)

	// After deletion the physical table (and its embedded _acl) must be gone.
	oldPhysical := testPhysicalName(t, ctx, db, projectID, "app", "notes")
	require.NoError(t, docDB.DeleteCollection(ctx, projectID, "app", "notes"))
	var exists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = ? AND table_name = ?)`,
		testSchema(t, projectID, "app"), oldPhysical).Scan(&exists))
	require.False(t, exists, "删除集合后物理表不得残留")

	// Recreate the same collection and a document with the same ID but no
	// document-level permissions: alice must NOT see it (no old-ACL leak).
	createColl()
	_, err = docDB.CreateDocument(ctx, projectID, "app", "notes", databases.Document{
		ID:   "doc-1",
		Data: map[string]any{"title": "fresh"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	alice := databases.Principal{Roles: []string{"user:alice"}}
	list, err := docDB.ListDocuments(ctx, projectID, "app", "notes", databases.Query{AST: &query.Query{Filter: query.Eq("$id", "doc-1")}}, alice)
	require.NoError(t, err)
	require.Len(t, list.Documents, 0)

	// System can still see the recreated document.
	sys, err := docDB.ListDocuments(ctx, projectID, "app", "notes", databases.Query{AST: &query.Query{Filter: query.Eq("$id", "doc-1")}}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, sys.Documents, 1)
}

// TestListDocuments_MultiValueEqualNotEqual (#4): multi-value equal/notEqual
// must work on non-text columns (BIGINT) and keep single/multi value behavior
// on string columns.
func TestListDocuments_MultiValueEqualNotEqual(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
	}, nil, nil, true))

	ids := make([]string, 0, 5)
	for i, title := range []string{"a", "b", "c", "d", "e"} {
		created, err := docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
			Data: map[string]any{"title": title, "views": i + 1},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
		ids = append(ids, created.ID)
	}
	_ = ids

	// Multi-value equal on an integer column.
	list, err := docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("views", "1", "2", "3")},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 3)
	got := map[float64]bool{}
	for _, d := range list.Documents {
		got[d.Data["views"].(float64)] = true
	}
	require.True(t, got[1] && got[2] && got[3])

	// Multi-value notEqual on an integer column.
	list, err = docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Ne("views", "1", "2")},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 3)
	got = map[float64]bool{}
	for _, d := range list.Documents {
		got[d.Data["views"].(float64)] = true
	}
	require.True(t, got[3] && got[4] && got[5])

	// Single value equal on a string column (regression).
	list, err = docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("title", "a")},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	require.Equal(t, "a", list.Documents[0].Data["title"])

	// Multi-value equal on a string column (regression).
	list, err = docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("title", "a", "b")},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 2)

	// Single value notEqual on a string column (regression).
	list, err = docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Ne("title", "a")},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 4)
}

// TestListDocuments_AstOr compiles proto/AST or into SQL OR (two matching rows).
func TestListDocuments_AstOr(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	for _, title := range []string{"alpha", "beta", "gamma"} {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
			Data: map[string]any{"title": title},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
	}

	ast := &query.Query{
		Filter: &query.Filter{Op: query.OpOr, Children: []*query.Filter{
			{Op: query.OpEqual, Attribute: "title", Values: []string{"alpha"}},
			{Op: query.OpEqual, Attribute: "title", Values: []string{"gamma"}},
		}},
	}
	list, err := docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{AST: ast}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(2), list.TotalCount)
	got := map[string]bool{}
	for _, d := range list.Documents {
		got[d.Data["title"].(string)] = true
	}
	require.True(t, got["alpha"] && got["gamma"])
	require.False(t, got["beta"])

	count, err := docDB.CountDocuments(ctx, projectID, "app", "posts", databases.Query{AST: ast}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}

// TestListDocuments_SelectProjection (#6a): select() must filter Data to the
// chosen keys while system fields always remain on the Document struct, with
// $id/$createdAt/$updatedAt aliases honored.
func TestListDocuments_SelectProjection(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "profiles", "Profiles", []databases.Attribute{
		{ID: "name", Key: "name", Type: "string", Size: 256},
		{ID: "age", Key: "age", Type: "integer"},
		{ID: "email", Key: "email", Type: "string", Size: 256},
	}, nil, nil, true))

	created, err := docDB.CreateDocument(ctx, projectID, "app", "profiles", databases.Document{
		Data: map[string]any{"name": "alice", "age": 30, "email": "a@b.c"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	// Projection to ["name","age"]: Data only holds those keys.
	list, err := docDB.ListDocuments(ctx, projectID, "app", "profiles", databases.Query{
		AST: &query.Query{Selects: []string{"name", "age"}, PageSize: 10},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	doc := list.Documents[0]
	require.Equal(t, map[string]any{"name": "alice", "age": float64(30)}, doc.Data)
	require.NotEmpty(t, doc.ID)
	require.False(t, doc.CreatedAt.IsZero())

	// Projection to ["$id"]: alias maps to the system _id field, so Data is
	// empty while the system fields remain.
	list, err = docDB.ListDocuments(ctx, projectID, "app", "profiles", databases.Query{
		AST: &query.Query{Selects: []string{"$id"}, PageSize: 10},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	doc = list.Documents[0]
	require.Empty(t, doc.Data)
	require.Equal(t, created.ID, doc.ID)
	require.False(t, doc.CreatedAt.IsZero())
}

// TestListDocuments_CursorPagination (#6b): cursorAfter/cursorBefore keyset
// pagination with default DESC ordering, explicit orderAsc, reverse cursor,
// and error cases (missing cursor doc / invalid order field).
func TestListDocuments_CursorPagination(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "seqdocs", "SeqDocs", []databases.Attribute{
		{ID: "seq", Key: "seq", Type: "integer"},
		{ID: "age", Key: "age", Type: "integer"},
	}, nil, nil, true))

	// Create d1..d4 with strictly increasing _created_at (d4 latest).
	for i := 1; i <= 4; i++ {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "seqdocs", databases.Document{
			ID:   "d" + string(rune('0'+i)),
			Data: map[string]any{"seq": i, "age": 50 - i*10},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
	}
	orderDESC := []string{"d4", "d3", "d2", "d1"}

	// Default ordering (DESC) cursorAfter pagination: page 1 = [d4, d3],
	// page 2 with cursor on the last id of page 1 = [d2, d1] (no overlap,
	// list exhausted).
	page1, err := docDB.ListDocuments(ctx, projectID, "app", "seqdocs", databases.Query{
		AST: &query.Query{PageSize: 2},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, page1.Documents, 2)
	require.Equal(t, orderDESC[:2], []string{page1.Documents[0].ID, page1.Documents[1].ID})

	page2, err := docDB.ListDocuments(ctx, projectID, "app", "seqdocs", databases.Query{
		AST: &query.Query{PageSize: 2, CursorAfter: page1.Documents[1].ID},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, page2.Documents, 2)
	require.Equal(t, orderDESC[2:], []string{page2.Documents[0].ID, page2.Documents[1].ID})
	require.NotEqual(t, page1.Documents[0].ID, page2.Documents[0].ID)
	require.NotEqual(t, page1.Documents[1].ID, page2.Documents[0].ID)

	// cursorAfter + orderAsc("age"): ages 10,20,30,40 → page1 [d4,d3],
	// page2 [d2,d1].
	asc1, err := docDB.ListDocuments(ctx, projectID, "app", "seqdocs", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "age"}}, PageSize: 2},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, asc1.Documents, 2)
	require.Equal(t, "d4", asc1.Documents[0].ID)
	require.Equal(t, "d3", asc1.Documents[1].ID)

	asc2, err := docDB.ListDocuments(ctx, projectID, "app", "seqdocs", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "age"}}, PageSize: 2, CursorAfter: asc1.Documents[1].ID},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, asc2.Documents, 2)
	require.Equal(t, "d2", asc2.Documents[0].ID)
	require.Equal(t, "d1", asc2.Documents[1].ID)

	// cursorBefore reverse pagination (default DESC): the predicate
	// (created_at, _id) > cursor selects the rows before the cursor in the
	// DESC result order, i.e. the previous page [d4, d3] before d2.
	rev, err := docDB.ListDocuments(ctx, projectID, "app", "seqdocs", databases.Query{
		AST: &query.Query{PageSize: 2, CursorBefore: "d2"},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, rev.Documents, 2)
	require.Equal(t, "d4", rev.Documents[0].ID)
	require.Equal(t, "d3", rev.Documents[1].ID)

	// Cursor document does not exist → InvalidArgument.
	_, err = docDB.ListDocuments(ctx, projectID, "app", "seqdocs", databases.Query{
		AST: &query.Query{PageSize: 2, CursorAfter: "nope-not-exists"},
	}, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// Invalid order field in cursor mode → InvalidArgument (no silent skip).
	_, err = docDB.ListDocuments(ctx, projectID, "app", "seqdocs", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "bad field"}}, PageSize: 2, CursorAfter: "d1"},
	}, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestListDocuments_PaginationGuards (A1/F3-2): page_size 负数回退默认页大小；
// AST page_size 优先于请求级 page_size；AST offset 显式拒绝（keyset-only）；
// 非 keyset token 拒绝。
func TestListDocuments_PaginationGuards(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "n", Key: "n", Type: "integer"},
	}, nil, nil, true))

	for i := 0; i < 7; i++ {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
			Data: map[string]any{"n": i},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
	}

	// page_size=5 且 DSL 未显式指定 limit → 返回 5 条，total=7，NextPageToken 非空。
	list, err := docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		PageSize: 5,
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 5)
	require.Equal(t, int64(7), list.TotalCount)
	require.NotEmpty(t, list.NextPageToken)

	// 第二页：剩余 2 条，无 NextPageToken。
	list2, err := docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		PageSize:  5,
		PageToken: list.NextPageToken,
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list2.Documents, 2)
	// R5-J3-2：offset 续页跳过精确 COUNT，total=0（proto 语义 total<=0=unknown）。
	require.Zero(t, list2.TotalCount)
	require.Empty(t, list2.NextPageToken)

	// page_size=-1 → 回退默认页大小 50，7 条全部返回。
	list, err = docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		PageSize: -1,
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 7)

	// page_size=0 → 同默认 50，7 条全部返回。
	list, err = docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 7)

	// AST page_size 显式指定时优先于请求级 page_size（C7：等价旧 DSL limit）。
	list, err = docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		PageSize: 5,
		AST:      &query.Query{PageSize: 3},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 3)

	// AST offset（keyset-only，C2）：List 一律拒绝——正负值同理（负值是
	// 客户端语法糖层的解析错误，服务端按显式拒绝兜底）。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		AST: &query.Query{Offset: -1},
	}, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "cursor pagination")

	_, err = docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		AST: &query.Query{Offset: 10001, PageSize: 2},
	}, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "cursor pagination")

	_, err = docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		AST: &query.Query{Offset: 0, PageSize: 2},
	}, databases.SystemPrincipal)
	require.NoError(t, err, "offset=0 与缺省不可区分，等价无操作")

	_, err = docDB.CountDocuments(ctx, projectID, "app", "docs", databases.Query{AST: &query.Query{Offset: 1}}, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 非 keyset token（旧 offset 族 / 任意垃圾）→ InvalidArgument。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		PageToken: "not-a-valid-token", // #nosec G101 -- 测试固定值
	}, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "keyset token required")
}

// countQueryHook 统计经 bun 执行的 COUNT(*) 查询条数（R5-J3-2 行为验证：
// offset 续页不得再产生 COUNT 查询）。
type countQueryHook struct {
	count int
}

func (h *countQueryHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *countQueryHook) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	if strings.Contains(event.Query, "COUNT(*)") {
		h.count++
	}
}

// TestListDocuments_KeysetContinuationSkipsCount（C2 阶段①收敛后的续页行为；
// 承接 R5-J3-2 的成本纪律）：仅首页（无 cursor）执行精确 COUNT；token 续页
// （keyset-only：首页满页发 ka: token）跳过 COUNT（total=0=unknown），满页
// 判定 has-more（满页 → next token，不满页无 next）；cursorAfter DSL 行为不变。
func TestListDocuments_KeysetContinuationSkipsCount(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "n", Key: "n", Type: "integer"},
	}, nil, nil, true))

	for i := 0; i < 7; i++ {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
			Data: map[string]any{"n": i},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
	}

	hook := &countQueryHook{}
	db.AddQueryHook(hook)

	// 首页：保持精确 COUNT，total=7；满页发 keyset token（keyset-only：
	// 不再发 offset 族 token）。
	page1, err := docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		PageSize: 3,
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, page1.Documents, 3)
	require.Equal(t, int64(7), page1.TotalCount)
	require.NotEmpty(t, page1.NextPageToken)
	require.Contains(t, page1.NextPageToken, "ka:")
	require.GreaterOrEqual(t, hook.count, 1, "首页应执行精确 COUNT")

	// keyset 续页：跳过 COUNT（total=0）；满页 → 续页 token。
	hook.count = 0
	page2, err := docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		PageSize:  3,
		PageToken: page1.NextPageToken,
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, page2.Documents, 3)
	require.Zero(t, page2.TotalCount, "续页不再精确 COUNT（total=0=unknown）")
	require.NotEmpty(t, page2.NextPageToken, "满页应有续页 token")
	require.Zero(t, hook.count, "续页不得产生 COUNT 查询")

	// 末页：不满页（1<3）→ 无 next，仍无 COUNT。
	hook.count = 0
	page3, err := docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		PageSize:  3,
		PageToken: page2.NextPageToken,
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, page3.Documents, 1)
	require.Zero(t, page3.TotalCount)
	require.Empty(t, page3.NextPageToken, "不满页无续页 token")
	require.Zero(t, hook.count)

	// keyset 模式（cursor 非空）行为完全不变：无 COUNT、total=0（W-D 语义）。
	hook.count = 0
	cursor := page1.Documents[len(page1.Documents)-1].ID
	kPage, err := docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		AST: &query.Query{PageSize: 3, CursorAfter: cursor},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Zero(t, kPage.TotalCount)
	require.Zero(t, hook.count, "keyset 模式无 COUNT（W-D，行为不变）")
	require.NotEmpty(t, kPage.NextPageToken)
}

// TestCreateCollection_DefaultTimeIndex (R5-J3-1，D-P1-3)：新建用户集合默认
// 获得 (_tenant,_created_at,_id) 时间索引（默认排序与 keyset 谓词的支撑）；
// 系统集合跳过（与 _version 列处理一致）；存量集合（DROP INDEX 模拟旧版本
// 建表）在下次 DDL touch 时经 reconcile 路径幂等补建，重复 touch 不报错。
func TestCreateCollection_DefaultTimeIndex(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "idxdocs", "Idx Docs", []databases.Attribute{
		{ID: "n", Key: "n", Type: "integer"},
	}, nil, nil, true))

	schema := testSchema(t, projectID, "app")
	defaultIndexExists := func(coll string) bool {
		phys := testPhysicalName(t, ctx, db, projectID, "app", coll)
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = ? AND tablename = ? AND indexname = ?`,
			schema, phys, fmt.Sprintf("idx_%s_tenant_created", phys)).Scan(&n))
		return n == 1
	}
	dropDefaultIndex := func(coll string) {
		phys := testPhysicalName(t, ctx, db, projectID, "app", coll)
		_, err := db.ExecContext(ctx, fmt.Sprintf(`DROP INDEX %s`,
			quoteIdent(schema)+"."+quoteIdent(fmt.Sprintf("idx_%s_tenant_created", phys))))
		require.NoError(t, err)
	}

	// 新建用户集合：pg_indexes 中存在默认时间索引。
	require.True(t, defaultIndexExists("idxdocs"), "新建用户集合应默认建 (_tenant,_created_at,_id) 索引")

	// 存量补建（B3 语义收窄）：CreateIndex 是在线通道（纯 catalog DML +
	// 事务外 CIC），不再搭车默认索引自愈——锁型 DDL（非并发 CREATE INDEX
	// 取 SHARE 锁）会阻塞并发读写，与在线通道目的相悖；自愈移交 schema
	// 漂移对账（CONCURRENTLY 通道补齐）。
	dropDefaultIndex("idxdocs")
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "idxdocs", databases.Index{
		ID: "n_key", Type: "key", Attributes: []string{"n"},
	}))
	require.False(t, defaultIndexExists("idxdocs"),
		"在线通道 CreateIndex 不再触发默认索引自愈（B3 语义收窄）")
	_, err := ReconcileSchemaDrift(ctx, db, SchemaReconcileOptions{})
	require.NoError(t, err)
	require.True(t, defaultIndexExists("idxdocs"), "schema 漂移对账经 CONCURRENTLY 补建默认索引")
	// CreateAttribute touch 保持既有自愈（AccessExclusive 通道，无并发读者
	// 承诺，补建无害）。
	dropDefaultIndex("idxdocs")
	require.NoError(t, docDB.CreateAttribute(ctx, projectID, "app", "idxdocs", databases.Attribute{ID: "m", Key: "m", Type: "integer"}))
	require.True(t, defaultIndexExists("idxdocs"), "存量集合在 DDL touch（CreateAttribute）时幂等补建")
	require.NoError(t, docDB.CreateAttribute(ctx, projectID, "app", "idxdocs", databases.Attribute{ID: "k", Key: "k", Type: "integer"}))
	require.True(t, defaultIndexExists("idxdocs"), "重复 DDL touch 幂等，索引仍在")

	// 系统集合（sentinel 名单）跳过默认时间索引：与 _version 列处理一致。
	// sentinel 的 catalog 无 database_id='_' 行（06-databases），CreateCollection
	// 元数据插入必撞 FK，故直接走 DDL 内部路径（与 CreateCollection 的 DDL 部分
	// 同构，即"测试重建旧文档表"路径）验证 isSystem 分支。
	projSchema := testProjectSchema(t, projectID)
	impl := docDB.(*postgresDocumentDB)
	internalID, err := impl.resolveInternalID(ctx, projectID)
	require.NoError(t, err)
	require.NoError(t, impl.createCollectionTable(ctx, projSchema, "users", internalID, nil, true))
	require.NoError(t, impl.reconcileVersionColumn(ctx, projSchema, "users", true))
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = ? AND tablename = 'users' AND indexname = 'idx_users_tenant_created'`,
		projSchema).Scan(&n))
	require.Zero(t, n, "系统集合跳过默认时间索引（与 _version 列处理一致）")
}

// TestListDocuments_InputLimits (A2): AST 叶数上限（原 queries 条数上限的
// 单 AST 等价物）与 equal 多值个数超上限均报 InvalidArgument；正常调用不受
// 影响。DSL 串条数/长度上限随双栈退役移除（服务端不再收字符串）。
func TestListDocuments_InputLimits(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	// 101 个过滤叶（and 嵌套）→ InvalidArgument（MaxQueries=100）。
	children := make([]*query.Filter, query.MaxQueries+1)
	for i := range children {
		children[i] = query.Eq("title", "x")
	}
	over := &query.Query{Filter: query.And(children...)}
	_, err := docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{AST: over}, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = docDB.CountDocuments(ctx, projectID, "app", "posts", databases.Query{AST: over}, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 1001 个 equal 值 → InvalidArgument（maxFilterValues）。
	values := make([]string, maxFilterValues+1)
	for i := range values {
		values[i] = "a"
	}
	_, err = docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("title", values...)},
	}, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 正常查询不受影响。
	list, err := docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("title", "hello"), PageSize: 10},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 0)
}

// TestCreateCollectionMetadata_IdempotentSystemRow (A3): 系统集合元数据行已存在时
// 重复 insertCollectionMetadata 幂等成功（DO NOTHING + 行数判断，复用既有物理名）。
func TestCreateCollectionMetadata_IdempotentSystemRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, testutil.SeedSystemDocumentCollections(ctx, db, docDB, projectID))

	p := &postgresDocumentDB{db: db}
	_, idempotent, err := p.insertCollectionMetadata(ctx, projectID, databases.SystemDatabaseID, "users", "users", nil, nil, nil, true)
	require.NoError(t, err)
	require.True(t, idempotent)
}

// TestCreateCollectionMetadata_DuplicateUserCollection (A3): 用户集合建同名同 ID
// 集合，第二次经 DO NOTHING 行数判断返回 ErrDuplicateKey，MapDocumentDBError 后为
// AlreadyExists（既有映射，不依赖 A6 新增）。
func TestCreateCollectionMetadata_DuplicateUserCollection(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "notes", "Notes", nil, nil, nil, true))

	err := docDB.CreateCollection(ctx, projectID, "app", "notes", "Notes", nil, nil, nil, true)
	require.ErrorIs(t, err, ErrDuplicateKey)
	require.Equal(t, codes.AlreadyExists, status.Code(shared.MapDocumentDBError(err)))
}

// TestBulkUpdateDocuments_RollbackOnFailure (A4): Bulk 事务化——第 2 条为不存在的
// 文档 ID（UpdateDocument 尾随 GetDocument 返回 nil → error）时整体回滚，
// 第 1 条的更新不得生效。
func TestBulkUpdateDocuments_RollbackOnFailure(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	doc1, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{"title": "original"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	n, err := docDB.BulkUpdateDocuments(ctx, projectID, "app", "docs",
		[]string{doc1.ID, "missing-id"},
		map[string]any{"title": "changed"}, nil, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, int64(0), n)

	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", doc1.ID, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, "original", got.Data["title"])
}

// TestBulkDeleteDocuments_RollbackOnFailure (R5-P2-6)：BulkDelete 混入不存在
// 文档 → ErrDocumentNotFound 整体回滚，存在文档保留（all-or-nothing 与
// BulkUpdate 对称）。
func TestBulkDeleteDocuments_RollbackOnFailure(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	doc1, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{"title": "original"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	n, err := docDB.BulkDeleteDocuments(ctx, projectID, "app", "docs",
		[]string{doc1.ID, "missing-id"}, databases.SystemPrincipal)
	require.ErrorIs(t, err, databases.ErrDocumentNotFound)
	require.Equal(t, int64(0), n)

	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", doc1.ID, databases.SystemPrincipal)
	require.NoError(t, err)
	require.NotNil(t, got, "整体回滚：存在文档不得被删除")
}

// TestBulkDocuments_PermissionDeniedRollback (R5-P2-6)：批量化把权限校验前置
// ——混入无 update/delete 权限的文档 → 整体 ErrPermissionDenied，任何文档
// 不得被修改/删除（判定函数仍是 AllowsDocumentAccess，非 SQL 谓词）。
func TestBulkDocuments_PermissionDeniedRollback(t *testing.T) {
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
	// 集合无 update/delete 授权：文档级写权限完全由 _perms 决定（B1）。
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "any"},
	}, true))

	// d1 持有 update/delete:user:u1（文档级），d2 仅 read（不可写）。
	d1, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{"title": "one"},
	}, []databases.Permission{
		{Type: "read", Role: "user:u1"},
		{Type: "update", Role: "user:u1"},
		{Type: "delete", Role: "user:u1"},
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	d2, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{"title": "two"},
	}, []databases.Permission{{Type: "read", Role: "user:u1"}}, databases.SystemPrincipal)
	require.NoError(t, err)

	u1 := databases.Principal{Roles: []string{"users", "user:u1"}}
	_, err = docDB.BulkUpdateDocuments(ctx, projectID, "app", "docs", []string{d1.ID, d2.ID},
		map[string]any{"title": "changed"}, nil, u1)
	require.ErrorIs(t, err, ErrPermissionDenied)

	for id, want := range map[string]string{d1.ID: "one", d2.ID: "two"} {
		got, err := docDB.GetDocument(ctx, projectID, "app", "docs", id, databases.SystemPrincipal)
		require.NoError(t, err)
		require.Equal(t, want, got.Data["title"], "%s 不得被修改（整体回滚）", id)
	}

	_, err = docDB.BulkDeleteDocuments(ctx, projectID, "app", "docs", []string{d1.ID, d2.ID}, u1)
	require.ErrorIs(t, err, ErrPermissionDenied)
	for _, id := range []string{d1.ID, d2.ID} {
		got, err := docDB.GetDocument(ctx, projectID, "app", "docs", id, databases.SystemPrincipal)
		require.NoError(t, err)
		require.NotNil(t, got, "整体回滚：文档不得被删除")
	}
}

// TestBulkDocuments_DuplicateIDsSingleEffect (R5-P2-6)：批量化按唯一文档集合
// 执行——重复 _id 只生效一次（affected=1、_version 恰好 +1），不再具有
// 逐条循环的重复执行语义。
func TestBulkDocuments_DuplicateIDsSingleEffect(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	doc1, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{"title": "original"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	n, err := docDB.BulkUpdateDocuments(ctx, projectID, "app", "docs",
		[]string{doc1.ID, doc1.ID},
		map[string]any{"title": "changed"}, nil, databases.SystemPrincipal)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "重复 _id 按唯一文档计数")

	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", doc1.ID, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, "changed", got.Data["title"])
	require.Equal(t, int64(2), got.Version, "重复 _id 恰好一次 _version +1")
}

// bulkStatementHook 统计 bun 执行的全部语句条数（R5-P2-6 行为验证：批量化后
// bulk 语句数为 O(N)——每文档 1 条 outbox + 常数条批量语句）。
type bulkStatementHook struct {
	count int
}

func (h *bulkStatementHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *bulkStatementHook) AfterQuery(_ context.Context, _ *bun.QueryEvent) {
	h.count++
}

// TestBulkDocuments_StatementCount (R5-P2-6)：100 文档 BulkUpdate / BulkDelete
// 的总语句数必须 < 2N——旧逐条路径每文档 ~8 条语句（权限点查、prePerms 点查、
// UPDATE、_perms 清/写、尾随回读、事件内 GetCollection），~8N 长事务持锁；
// 批量化后为 N 条 outbox + 常数条批量语句。下界 >= N 锁定 per-doc outbox
// 不得被过度合并。
func TestBulkDocuments_StatementCount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, events.NewEventOutbox(db))
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "any"},
	}, true))

	const n = 100
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		created, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
			ID:   fmt.Sprintf("stmt-%03d", i),
			Data: map[string]any{"title": "t"},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
		ids = append(ids, created.ID)
	}

	hook := &bulkStatementHook{}
	db.AddQueryHook(hook)

	affected, err := docDB.BulkUpdateDocuments(ctx, projectID, "app", "docs", ids,
		map[string]any{"title": "bulk"}, nil, databases.SystemPrincipal)
	require.NoError(t, err)
	require.EqualValues(t, n, affected)
	updateStmts := hook.count
	t.Logf("bulk update %d docs -> %d statements (旧逐条路径 ~%d)", n, updateStmts, 8*n)
	require.Less(t, updateStmts, 2*n, "100 文档 bulk update 语句数应 < 2N（R5-P2-6；旧逐条路径 ~8N）")
	require.GreaterOrEqual(t, updateStmts, n, "每文档至少 1 条 outbox INSERT（per-doc 事件不得被合并）")

	hook.count = 0
	affected, err = docDB.BulkDeleteDocuments(ctx, projectID, "app", "docs", ids, databases.SystemPrincipal)
	require.NoError(t, err)
	require.EqualValues(t, n, affected)
	t.Logf("bulk delete %d docs -> %d statements (旧逐条路径 ~%d)", n, hook.count, 8*n)
	require.Less(t, hook.count, 2*n, "100 文档 bulk delete 语句数应 < 2N（R5-P2-6；旧逐条路径 ~8N）")
	require.GreaterOrEqual(t, hook.count, n, "每文档至少 1 条 outbox INSERT（per-doc 事件不得被合并）")
}

// TestListDocuments_SystemPathRawPGError (A6/A7 → J4-6): SystemPrincipal（信任路径，跳过
// 白名单）查询未声明列 → adapter 已在 infra 层按 SQLSTATE 翻译为 status（J4-6 下沉），
// 调用方看到的是 gRPC InvalidArgument 而非裸 pgdriver.Error。
func TestListDocuments_SystemPathRawPGError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, testutil.SeedSystemDocumentCollections(ctx, db, docDB, projectID))

	_, err := docDB.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "users", databases.Query{
		AST: &query.Query{Filter: query.Eq("nonexistent_col", "x")},
	}, databases.SystemPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	// 域码体系（redesign §4.1）：消息携带稳定域码前缀，ErrorInfo.reason 同源。
	require.Contains(t, status.Convert(err).Message(), databases.ErrCodeInvalidArgument)
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			require.Equal(t, databases.ErrCodeInvalidArgument, info.Reason)
			require.Equal(t, "42703", info.Metadata["sqlstate"])
			require.NotEmpty(t, info.Metadata["error_id"])
		}
	}
	// 裸 pgdriver.Error 不应上抛至调用方（J4-6）
	var pgErr pgdriver.Error
	require.False(t, errors.As(err, &pgErr), "pgdriver.Error should be translated before returning: %v", err)
}

// TestListDocuments_QueryFieldWhitelist (A7): 非 System 路径查询字段白名单
// （未声明列/非法 order → InvalidArgument）；search 需 fulltext 索引列
// （无索引集合 → InvalidArgument，files.name → 可用）。
func TestListDocuments_QueryFieldWhitelist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, testutil.SeedSystemDocumentCollections(ctx, db, docDB, projectID))
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	bob := databases.Principal{Roles: []string{"user:bob"}}

	// 非 System 查询未声明列 → InvalidArgument。
	_, err := docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("nonexistent", "x")},
	}, bob)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 非 System 查询未声明列（Count）→ InvalidArgument。
	_, err = docDB.CountDocuments(ctx, projectID, "app", "posts", databases.Query{AST: &query.Query{Filter: query.Eq("nonexistent", "x")}}, bob)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// order 非法字段 → InvalidArgument（不再静默跳过）。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "bad field", Desc: true}}},
	}, bob)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// search 对无 fulltext 索引的集合 → InvalidArgument。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Search("title", "hello")},
	}, bob)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// search 命中 fulltext 索引列（files.name_fulltext）→ 可用。
	keysPrincipal := databases.Principal{Roles: []string{"keys"}}
	_, err = docDB.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "files", databases.Query{
		AST: &query.Query{Filter: query.Search("name", "hello")},
	}, keysPrincipal)
	require.NoError(t, err)

	// 合法字段（声明 attr + 系统列）不受影响。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("title", "x"), Orders: []query.Order{{Attribute: "$id"}}, PageSize: 5},
	}, bob)
	require.NoError(t, err)
}

// TestListDocuments_SensitiveFieldBlacklist (A7): default 库系统集合的凭据/令牌类列
// （users.password_hash 等）禁止作为过滤条件 → InvalidArgument；自定义库同名集合不受影响。
func TestListDocuments_SensitiveFieldBlacklist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, testutil.SeedSystemDocumentCollections(ctx, db, docDB, projectID))

	keysPrincipal := databases.Principal{Roles: []string{"keys"}}

	// users（default 库系统集合）：password_hash 禁止过滤。
	_, err := docDB.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "users", databases.Query{
		AST: &query.Query{Filter: query.Eq("password_hash", "x")},
	}, keysPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// sessions.secret_hash / identities.provider_data 同样禁止。
	_, err = docDB.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "sessions", databases.Query{
		AST: &query.Query{Filter: query.Eq("secret_hash", "x")},
	}, keysPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = docDB.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "identities", databases.Query{
		AST: &query.Query{Filter: query.Eq("provider_data", "x")},
	}, keysPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 非敏感声明列（email）可查。
	_, err = docDB.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "users", databases.Query{
		AST: &query.Query{Filter: query.Eq("email", "x@y.z")},
	}, keysPrincipal)
	require.NoError(t, err)

	// 自定义库同名集合不受黑名单影响（可建同名集合，白名单外字段仅按 attrs 校验）。
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "users", "Users", []databases.Attribute{
		{ID: "password_hash", Key: "password_hash", Type: "string", Size: 512},
	}, nil, nil, true))
	_, err = docDB.ListDocuments(ctx, projectID, "app", "users", databases.Query{
		AST: &query.Query{Filter: query.Eq("password_hash", "x")},
	}, keysPrincipal)
	require.NoError(t, err)
}

// TestCreateDocument_AuditColumns (#12): _created_by/_updated_by are filled
// from the principal's first user:<id> role; keys-only principals leave them
// empty; user data cannot spoof _-prefixed audit fields.
func TestCreateDocument_AuditColumns(t *testing.T) {
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
	perms := []databases.Permission{
		{Type: "create", Role: "user:abc"}, {Type: "read", Role: "user:abc"}, {Type: "update", Role: "user:abc"},
		{Type: "create", Role: "keys"}, {Type: "read", Role: "keys"}, {Type: "update", Role: "keys"},
	}
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "audit", "Audit", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, perms, false))

	// user:abc principal: spoofed _created_at/_created_by/_updated_by in data
	// are ignored; audit columns carry "abc".
	created, err := docDB.CreateDocument(ctx, projectID, "app", "audit", databases.Document{
		Data: map[string]any{
			"title":       "t1",
			"_created_at": "2000-01-01T00:00:00Z",
			"_created_by": "spoof",
			"_updated_by": "spoof",
			"not_a_col!":  "ignored",
		},
	}, nil, databases.Principal{Roles: []string{"user:abc"}})
	require.NoError(t, err)
	require.Equal(t, "abc", created.CreatedBy)
	require.Equal(t, "abc", created.UpdatedBy)
	require.False(t, created.CreatedAt.IsZero())
	require.WithinDuration(t, time.Now(), created.CreatedAt, time.Hour)
	require.NotContains(t, created.Data, "_created_at")
	require.NotContains(t, created.Data, "_created_by")
	require.NotContains(t, created.Data, "_updated_by")
	require.Equal(t, "t1", created.Data["title"])

	// Update as user:abc → UpdatedBy == "abc".
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "audit", databases.DocumentUpdate{
		Document: databases.Document{
			ID:   created.ID,
			Data: map[string]any{"title": "t2"},
		},
		ExpectedVersion: created.Version,
	}, databases.Principal{Roles: []string{"user:abc"}})
	require.NoError(t, err)
	require.Equal(t, "abc", updated.UpdatedBy)
	require.Equal(t, "t2", updated.Data["title"])

	// keys-only principal (no user:<id> role) → audit columns empty.
	keysDoc, err := docDB.CreateDocument(ctx, projectID, "app", "audit", databases.Document{
		Data: map[string]any{"title": "k1"},
	}, nil, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.Empty(t, keysDoc.CreatedBy)
	require.Empty(t, keysDoc.UpdatedBy)
}

// TestDeleteIndex_RecreateSameIndex (R02-P1-1)：DeleteIndex 事务化——删除后
// catalog 与物理索引一致，同名索引可重建（不撞 catalog 合一行内重复）。
func TestDeleteIndex_RecreateSameIndex(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "title_key", Type: "key", Attributes: []string{"title"}},
	}, nil, true))

	coll, err := docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Len(t, coll.Indexes, 1)

	require.NoError(t, docDB.DeleteIndex(ctx, projectID, "app", "posts", "title_key"))

	coll, err = docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Empty(t, coll.Indexes)

	// 同名重建必须成功（物理索引 DROP 与元数据删除均已完成）。
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "posts", databases.Index{
		ID:         "title_key",
		Type:       "key",
		Attributes: []string{"title"},
	}))
	coll, err = docDB.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Len(t, coll.Indexes, 1)
	require.Equal(t, "title_key", coll.Indexes[0].ID)
}

// TestCreateDatabase_RollbackOnMetadataFailure (R02-P1-2)：CreateDatabase 整体
// 事务化——元数据 INSERT 失败（预插同 PK 行）时，事务内已建的 schema 必须回滚，
// 不允许出现"schema 存在而元数据缺失"。
func TestCreateDatabase_RollbackOnMetadataFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)

	// 预插同 (project_id, database_id) 元数据行，令 INSERT 撞复合主键。
	_, err := db.ExecContext(ctx,
		`INSERT INTO public.catalog_databases (project_id, database_id, name, created_at, updated_at) VALUES (?, 'app', 'preexisting', NOW(), NOW())`,
		projectID)
	require.NoError(t, err)

	err = docDB.CreateDatabase(ctx, projectID, "app", "Application DB")
	require.Error(t, err)
	require.True(t, errors.Is(err, databases.ErrDuplicateKey) || isUniqueViolation(err), "expected duplicate key (ErrDuplicateKey or unique violation), got: %v", err)

	// 事务回滚后 schema 必须不存在（to_regnamespace 返回 NULL）。
	var reg any
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT to_regnamespace(?)`, testSchema(t, projectID, "app")).Scan(&reg))
	require.Nil(t, reg)

	// 元数据未被篡改。
	_, err = docDB.GetDatabase(ctx, projectID, "app")
	require.NoError(t, err)
}

// TestUpdateDocument_PermissionsOnlyRefreshesAuditColumns (R02-P1-4)：仅变更
// permissions 时同样刷新 _updated_at/_updated_by，数据字段保持不变。
func TestUpdateDocument_PermissionsOnlyRefreshesAuditColumns(t *testing.T) {
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
	perms := []databases.Permission{
		{Type: "create", Role: "user:abc"}, {Type: "read", Role: "user:abc"}, {Type: "update", Role: "user:abc"},
		{Type: "create", Role: "keys"}, {Type: "read", Role: "keys"}, {Type: "update", Role: "keys"},
	}
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "audit", "Audit", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, perms, false))

	created, err := docDB.CreateDocument(ctx, projectID, "app", "audit", databases.Document{
		Data: map[string]any{"title": "t1"},
	}, []databases.Permission{
		{Type: "read", Role: "user:abc"}, {Type: "update", Role: "user:abc"},
	}, databases.Principal{Roles: []string{"user:abc"}})
	require.NoError(t, err)
	require.NotEmpty(t, created.CreatedBy)

	// 仅更新 permissions（无数据字段）：审计列必须刷新。
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "audit", databases.DocumentUpdate{
		Document: databases.Document{
			ID: created.ID,
		},
		Permissions: []databases.Permission{
			{Type: "read", Role: "user:abc"},
		},
		ExpectedVersion: created.Version,
	}, databases.Principal{Roles: []string{"user:abc"}})
	require.NoError(t, err)
	require.Equal(t, "abc", updated.UpdatedBy)
	require.True(t, updated.UpdatedAt.After(created.UpdatedAt), "updated_at must be refreshed on permissions-only update")
	require.Equal(t, "t1", updated.Data["title"])
	require.Len(t, updated.Permissions, 1)
	require.Equal(t, databases.Permission{Type: "read", Role: "user:abc"}, updated.Permissions[0])
}

// TestConflictLockKey_NoAmbiguity (R02-P2-2)：conflictLockKey 序列化无歧义——
// 含 \x00 / 空串 / 类型不同的值组合不得生成相同锁键。
func TestConflictLockKey_NoAmbiguity(t *testing.T) {
	// 旧分隔符拼接实现下这两组值键相同（值内含 \x00 的经典碰撞）。
	require.NotEqual(t,
		conflictLockKey([]any{"a\x00b", "c"}),
		conflictLockKey([]any{"a", "b\x00c"}))

	// 空串与分隔符组合。
	require.NotEqual(t,
		conflictLockKey([]any{"ab", ""}),
		conflictLockKey([]any{"a", "b"}))

	// 顺序敏感。
	require.NotEqual(t,
		conflictLockKey([]any{"a", "b"}),
		conflictLockKey([]any{"b", "a"}))

	// 类型区分（int64 vs 字符串同数值）。
	require.NotEqual(t,
		conflictLockKey([]any{int64(1)}),
		conflictLockKey([]any{"1"}))

	// 确定性：相同输入产生相同键。
	require.Equal(t,
		conflictLockKey([]any{"x", int64(42), true}),
		conflictLockKey([]any{"x", int64(42), true}))
}

// TestListDocuments_SameCreatedAtPaginationStable (R08-P2-4)：同 _created_at 的
// 多行在默认排序下按 _id DESC 稳定分页，翻页无重复、无遗漏。
func TestListDocuments_SameCreatedAtPaginationStable(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "n", Key: "n", Type: "integer"},
	}, nil, nil, true))

	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
			ID:   id,
			Data: map[string]any{"n": i},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
	}
	// 拉平 _created_at：全部改为同一时间戳，使默认排序只能依赖 _id tiebreaker。
	ts := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s SET _created_at = ?`, tableName(testSchema(t, projectID, "app"), testPhysicalName(t, ctx, db, projectID, "app", "docs"))), ts)
	require.NoError(t, err)

	var got []string
	token := ""
	for {
		list, err := docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
			PageSize:  2,
			PageToken: token,
		}, databases.SystemPrincipal)
		require.NoError(t, err)
		for _, d := range list.Documents {
			got = append(got, d.ID)
		}
		token = list.NextPageToken
		if token == "" {
			break
		}
	}
	require.Equal(t, []string{"e", "d", "c", "b", "a"}, got)
}

// TestListDocuments_PageSizeClamp (R08-P3-7)：page_size 超过 maxQueryLimit 时
// clamp 到上限，返回条数不超过 maxQueryLimit 且仍有续页 token。
func TestListDocuments_PageSizeClamp(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", nil, nil, nil, true))

	for i := 0; i < maxQueryLimit+1; i++ {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
			Data: map[string]any{},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
	}

	list, err := docDB.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		PageSize: maxQueryLimit + 50,
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, maxQueryLimit)
	require.Equal(t, int64(maxQueryLimit+1), list.TotalCount)
	require.NotEmpty(t, list.NextPageToken)
}

// TestCatalogReads_DoNotApplyProjectSchema：未 Apply 的项目读 catalog 不得
// projectschema.Apply / CREATE SCHEMA；EnsureCatalog 之后静态表存在。
func TestCatalogReads_DoNotApplyProjectSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	project := &model.Project{
		ID:        fmt.Sprintf("e6%x", time.Now().UnixNano()),
		Name:      "e6-no-ensure",
		Status:    "active",
		Settings:  map[string]any{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	_, err := db.NewInsert().Model(project).Exec(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		schema := testProjectSchema(t, project.ID)
		_, _ = db.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, quoteIdent(schema)))
		_, _ = db.NewDelete().Model((*model.Project)(nil)).Where("id = ?", project.ID).Exec(ctx)
	})

	docDB := NewPostgresDocumentDB(db, nil)
	schema := testProjectSchema(t, project.ID)

	coll, err := docDB.GetCollection(ctx, project.ID, databases.SystemDatabaseID, "users")
	require.NoError(t, err)
	require.Nil(t, coll)

	gotDB, err := docDB.GetDatabase(ctx, project.ID, "default")
	require.NoError(t, err)
	require.Nil(t, gotDB)

	listDB, err := docDB.ListDatabases(ctx, project.ID)
	require.NoError(t, err)
	require.Empty(t, listDB)

	listColl, _, err := docDB.ListCollections(ctx, project.ID, "default", databases.ListQuery{})
	require.NoError(t, err)
	require.Empty(t, listColl)

	var reg any
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regnamespace(?)`, schema).Scan(&reg))
	require.Nil(t, reg, "catalog 读路径不得 Apply/CREATE SCHEMA")

	require.NoError(t, docDB.EnsureCatalog(ctx, project.ID))
	coll, err = docDB.GetCollection(ctx, project.ID, databases.SystemDatabaseID, "users")
	require.NoError(t, err)
	require.Nil(t, coll, "cut 后 catalog 无 sentinel users")

	sentinel, err := docDB.GetDatabase(ctx, project.ID, databases.SystemDatabaseID)
	require.NoError(t, err)
	require.Nil(t, sentinel, "cut 后 catalog 无 database_id='_'")

	var staticUsers any
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass(?)`, schema+".users").Scan(&staticUsers))
	require.NotNil(t, staticUsers)
	var hasID, hasDocID int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = ? AND table_name = 'users' AND column_name = 'id'`, schema).Scan(&hasID))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = ? AND table_name = 'users' AND column_name = '_id'`, schema).Scan(&hasDocID))
	require.Equal(t, 1, hasID)
	require.Zero(t, hasDocID)
}

// TestCreateIndex_FulltextAlignment (W-E)：fulltext 索引表达式与查询编译
// 逐字对齐（单列 ::text）；orders 对 GIN 无意义不再产生语法错误 DDL；
// 多列 fulltext 在 CreateIndex 与 CreateCollection 两个入口均被拒绝
// （拼接表达式与单字段查询永不匹配，索引形同虚设）。
func TestCreateIndex_FulltextAlignment(t *testing.T) {
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
	attrs := []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "body", Key: "body", Type: "string", Size: 4096},
	}
	// infra 直连无 app 层默认权限，显式授予 keys create/read。
	collPerms := []databases.Permission{
		{Type: "create", Role: "keys"},
		{Type: "read", Role: "keys"},
	}
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", attrs, nil, collPerms, true))

	// orders=["desc"]：GIN 忽略 order，此前拼接 DESC 会产生语法错误 DDL。
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "posts", databases.Index{
		ID: "body_ft", Type: "fulltext", Attributes: []string{"body"}, Orders: []string{"desc"},
	}))

	keys := databases.Principal{Roles: []string{"keys"}}
	for i, body := range []string{"hello world", "goodbye moon", "hello again"} {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
			ID:   fmt.Sprintf("d%d", i),
			Data: map[string]any{"title": "t", "body": body},
		}, nil, keys)
		require.NoError(t, err)
	}

	got, err := docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Search("body", "hello")},
	}, keys)
	require.NoError(t, err, "对齐后的 fulltext 索引上 search 必须可用")
	require.Len(t, got.Documents, 2)

	// 物理索引表达式必须与查询编译逐字对齐（to_tsvector('simple', body::text)），
	// 否则 GIN 不命中、search 退化为全表逐行 to_tsvector。
	postsPhys := testPhysicalName(t, ctx, db, projectID, "app", "posts")
	var indexdef string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = ?`,
		fmt.Sprintf("idx_%s_body_ft", postsPhys)).Scan(&indexdef))
	require.Contains(t, indexdef, "to_tsvector")
	require.Contains(t, indexdef, "(body)::text",
		"fulltext 索引表达式须与 compilePredicate 的查询表达式一致（::text 对齐）")

	// 多列 fulltext：CreateIndex 入口拒绝。
	err = docDB.CreateIndex(ctx, projectID, "app", "posts", databases.Index{
		ID: "multi_ft", Type: "fulltext", Attributes: []string{"body", "title"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "exactly one attribute")

	// 多列 fulltext：CreateCollection 入口拒绝。
	err = docDB.CreateCollection(ctx, projectID, "app", "bads", "Bad", attrs, []databases.Index{
		{ID: "m", Type: "fulltext", Attributes: []string{"body", "title"}},
	}, nil, true)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestListDocuments_KeysetTokenContinuation (W-D)：cursor 模式的
// NextPageToken 是 keyset token（ka:/kb:），PageToken 续页保持 keyset 语义
// （不再切回 offset）；keyset 模式跳过精确 COUNT（TotalCount=0）；整页
// permissions 经批量查询回填。
func TestListDocuments_KeysetTokenContinuation(t *testing.T) {
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
	// documentSecurity=false：不做 ACL 过滤，专注 perms 回填与分页语义。
	// documentSecurity=false + read:any：集合级放行 guest，不做文档 ACL 过滤，
	// 专注 perms 回填与 keyset 分页语义。
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "kdocs", "KDocs", []databases.Attribute{
		{ID: "n", Key: "n", Type: "integer"},
	}, nil, []databases.Permission{{Type: "read", Role: "any"}}, false))

	for i := 0; i < 12; i++ {
		perms := []databases.Permission(nil)
		if i%3 == 0 { // 部分文档带 ACE，验证批量回填正确性
			perms = []databases.Permission{{Type: "read", Role: "keys"}, {Type: "read", Role: "user:alice"}}
		}
		_, err := docDB.CreateDocument(ctx, projectID, "app", "kdocs", databases.Document{
			ID:   fmt.Sprintf("doc-%04d", i),
			Data: map[string]any{"n": i},
		}, perms, databases.SystemPrincipal)
		require.NoError(t, err)
	}

	guest := databases.Principal{}

	// 首页：cursorAfter + orderAsc，满页（limit=5）→ keyset token。
	page1, err := docDB.ListDocuments(ctx, projectID, "app", "kdocs", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "$id"}}, PageSize: 5, CursorAfter: "doc-0004"},
	}, guest)
	require.NoError(t, err)
	require.Len(t, page1.Documents, 5)
	require.Equal(t, "doc-0005", page1.Documents[0].ID)
	require.Equal(t, "doc-0009", page1.Documents[4].ID)
	require.Equal(t, "ka:doc-0009", page1.NextPageToken, "keyset 模式回传 ka: token 而非 offset token")
	require.Zero(t, page1.TotalCount, "keyset 模式不做精确 COUNT")
	// 批量回填：i%3==0 的文档（页内 0006、0009）带 2 条 ACE，其余为空——
	// 同页混合命中/未命中验证批量查询按文档正确分组。
	for _, d := range page1.Documents {
		if d.ID == "doc-0006" || d.ID == "doc-0009" {
			require.Len(t, d.Permissions, 2, d.ID)
		} else {
			require.Empty(t, d.Permissions, d.ID)
		}
	}

	// 续页：PageToken（不带 cursorAfter 查询）→ 保持 keyset 语义。
	page2, err := docDB.ListDocuments(ctx, projectID, "app", "kdocs", databases.Query{
		AST:       &query.Query{Orders: []query.Order{{Attribute: "$id"}}, PageSize: 5},
		PageToken: page1.NextPageToken,
	}, guest)
	require.NoError(t, err)
	require.Len(t, page2.Documents, 2)
	require.Equal(t, "doc-0010", page2.Documents[0].ID)
	require.Equal(t, "doc-0011", page2.Documents[1].ID)
	require.Empty(t, page2.NextPageToken, "不满页（2<5）无续页 token")
	// offset 模式确认批量回填非空路径（doc-0000 有 2 条 ACE）与精确 COUNT 不受影响。
	off, err := docDB.ListDocuments(ctx, projectID, "app", "kdocs", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "$id"}}, PageSize: 1},
	}, guest)
	require.NoError(t, err)
	require.Len(t, off.Documents[0].Permissions, 2, "批量回填必须返回完整 ACE 列表")

	// before 方向（默认 DESC 展示序，与既有 rev 用例同模式）：
	// cursorBefore(doc-0006) → 比它新的 5 行 [0011..0007]，满页 → kb:0011；
	// 续页 before 0011 → 无更靠前（更新）的行 → 空页收尾。
	b1, err := docDB.ListDocuments(ctx, projectID, "app", "kdocs", databases.Query{
		AST: &query.Query{PageSize: 5, CursorBefore: "doc-0006"},
	}, guest)
	require.NoError(t, err)
	require.Len(t, b1.Documents, 5)
	require.Equal(t, "doc-0011", b1.Documents[0].ID)
	require.Equal(t, "doc-0007", b1.Documents[4].ID)
	require.Equal(t, "kb:doc-0011", b1.NextPageToken)

	b2, err := docDB.ListDocuments(ctx, projectID, "app", "kdocs", databases.Query{
		AST:       &query.Query{PageSize: 5},
		PageToken: b1.NextPageToken,
	}, guest)
	require.NoError(t, err)
	require.Empty(t, b2.Documents)
	require.Empty(t, b2.NextPageToken)
}
