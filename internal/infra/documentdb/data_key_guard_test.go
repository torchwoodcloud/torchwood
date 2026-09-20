package documentdb

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestValidateDataKey（S6 缺陷 1）：文档数据键 fail-closed 规则——`_` 前缀
// 系统列保留、标识符语法 safeNameRe、≤63 字节（PG NAMEDATALEN-1）。历史上
// 非法键在 build*Parts 被静默丢弃（200 成功但字段未落库），现改为 InvalidArgument。
func TestValidateDataKey(t *testing.T) {
	valid := []string{"name", "email_verified", "user_id", "a", strings.Repeat("x", 63)}
	for _, k := range valid {
		require.NoError(t, validateDataKey(k), "key %q should be valid", k)
	}

	invalid := []struct {
		key    string
		reason string
	}{
		{"my key", "invalid attribute key"},        // 空格：标识符语法
		{"a.b", "invalid attribute key"},           // 点：标识符语法
		{"", "invalid attribute key"},              // 空：标识符语法
		{"9start", "invalid attribute key"},        // 数字开头：标识符语法
		{"café", "invalid attribute key"},          // 非 ASCII：标识符语法
		{"_foo", "reserved for system columns"},    // `_` 前缀：系统列保留
		{"_id", "reserved for system columns"},     // 系统列本身
		{"_acl", "reserved for system columns"},    // ACL 系统列
		{strings.Repeat("x", 64), "identifier limit"}, // 超长：PG 静默截断
	}
	for _, tc := range invalid {
		err := validateDataKey(tc.key)
		require.Error(t, err, "key %q should be rejected", tc.key)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "key %q", tc.key)
		require.Contains(t, err.Error(), tc.key, "错误信息必须携带键名定位")
		require.Contains(t, err.Error(), tc.reason, "key %q", tc.key)
	}
}

// TestBuildParts_RejectIllegalKeys（S6 缺陷 1）：三个 builder 对非法数据键
// 一律 InvalidArgument（不再静默 continue），错误信息携带键名；合法键不受影响。
func TestBuildParts_RejectIllegalKeys(t *testing.T) {
	// 非法键样本（覆盖三类原因）× 三条 builder 通道。
	illegal := []string{"my key", "a.b", "_foo", strings.Repeat("x", 64)}
	for _, k := range illegal {
		t.Run("insert/"+k, func(t *testing.T) {
			_, _, _, err := buildInsertParts(databases.Document{Data: map[string]any{k: 1}}, nil)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Contains(t, err.Error(), k)
		})
		t.Run("update/"+k, func(t *testing.T) {
			_, _, err := buildUpdateParts(databases.Document{Data: map[string]any{k: 1}}, "", nil)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Contains(t, err.Error(), k)
		})
		t.Run("increment/"+k, func(t *testing.T) {
			// 键校验先于 delta == 0 语义跳过：delta=0 的非法键同样拒绝。
			_, _, err := buildIncrementParts(map[string]int64{k: 1})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Contains(t, err.Error(), k)
			_, _, err = buildIncrementParts(map[string]int64{k: 0})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Contains(t, err.Error(), k)
		})
	}

	// 合法键不受影响（形态回归）。
	cols, phs, args, err := buildInsertParts(databases.Document{Data: map[string]any{"name": "x"}}, nil)
	require.NoError(t, err)
	require.Equal(t, `"name"`, cols)
	require.Equal(t, "?", phs)
	require.Equal(t, []any{"x"}, args)

	setParts, _, err := buildUpdateParts(databases.Document{Data: map[string]any{"name": "x"}}, "u", nil)
	require.NoError(t, err)
	require.Equal(t, []string{`"name" = ?`, "_updated_at = ?", `"_updated_by" = ?`}, setParts[:3])

	incParts, incArgs, err := buildIncrementParts(map[string]int64{"views": 3})
	require.NoError(t, err)
	require.Equal(t, []string{`"views" = COALESCE("views", 0) + ?`}, incParts)
	require.Equal(t, []any{int64(3)}, incArgs)

	// delta == 0 合法键：语义 no-op 跳过（既有行为保持）。
	incParts, incArgs, err = buildIncrementParts(map[string]int64{"views": 0})
	require.NoError(t, err)
	require.Empty(t, incParts)
	require.Empty(t, incArgs)
}

// TestPostgresDocumentDatabase_IllegalDataKeys（S6 缺陷 1 端到端）：六条写路径
// （create/update/upsert 插入与更新支/increment/bulk/execute-tx op）对非法
// 数据键一律 InvalidArgument 且错误信息携带键名；合法键照常写入。修复前这些
// 路径全部 200 成功但字段被静默丢弃。
func TestPostgresDocumentDatabase_IllegalDataKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 128},
		{ID: "email", Key: "email", Type: "string", Size: 128},
		{ID: "views", Key: "views", Type: "integer"},
	}, []databases.Index{
		{ID: "uq_email", Type: "unique", Attributes: []string{"email"}},
	}, []databases.Permission{
		{Type: "create", Role: "keys"},
		{Type: "read", Role: "keys"},
		{Type: "update", Role: "keys"},
		{Type: "delete", Role: "keys"},
	}, true))
	principal := databases.Principal{Roles: []string{"keys"}, KeyID: "kguard"}

	t.Run("create rejects illegal keys", func(t *testing.T) {
		for _, k := range []string{"my key", "a.b", "_foo", strings.Repeat("x", 64)} {
			_, err := docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
				Data: map[string]any{"title": "t", k: 1},
			}, nil, principal)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "key %q", k)
			require.Contains(t, err.Error(), k, "key %q", k)
		}
	})

	// 合法基线： create/update 照常落库（守卫不误伤）。
	created, err := docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
		Data: map[string]any{"title": "ok", "email": "a@b.c", "views": 1},
	}, nil, principal)
	require.NoError(t, err)

	t.Run("update rejects illegal keys", func(t *testing.T) {
		for _, k := range []string{"my key", "a.b", "_foo", strings.Repeat("x", 64)} {
			_, err := docDB.UpdateDocument(ctx, projectID, "app", "posts", databases.DocumentUpdate{
				Document:        databases.Document{ID: created.ID, Data: map[string]any{k: 1}},
				ExpectedVersion: created.Version,
			}, principal)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "key %q", k)
			require.Contains(t, err.Error(), k, "key %q", k)
		}
	})

	t.Run("increment rejects illegal keys", func(t *testing.T) {
		for _, k := range []string{"my key", "a.b", "_foo", strings.Repeat("x", 64)} {
			_, err := docDB.UpdateDocument(ctx, projectID, "app", "posts", databases.DocumentUpdate{
				Document:        databases.Document{ID: created.ID},
				Increment:       map[string]int64{k: 1},
				ExpectedVersion: created.Version,
			}, principal)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "key %q", k)
			require.Contains(t, err.Error(), k, "key %q", k)
		}
	})

	t.Run("upsert insert branch rejects illegal keys", func(t *testing.T) {
		for i, k := range []string{"my key", "a.b", "_foo", strings.Repeat("x", 64)} {
			_, err := docDB.UpsertDocument(ctx, projectID, "app", "posts", databases.Document{
				ID:   fmt.Sprintf("u1-%d", i),
				Data: map[string]any{"email": fmt.Sprintf("u1-%d@b.c", i), k: 1},
			}, []string{"email"}, nil, principal)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "key %q", k)
			require.Contains(t, err.Error(), k, "key %q", k)
		}
	})

	t.Run("upsert update branch rejects illegal keys", func(t *testing.T) {
		// 先以合法键插入命中行，再以非法键 upsert 走更新支。
		_, err := docDB.UpsertDocument(ctx, projectID, "app", "posts", databases.Document{
			ID:   "u2",
			Data: map[string]any{"email": "hit@b.c", "title": "v1"},
		}, []string{"email"}, nil, principal)
		require.NoError(t, err)
		for _, k := range []string{"my key", "a.b", "_foo", strings.Repeat("x", 64)} {
			_, err := docDB.UpsertDocument(ctx, projectID, "app", "posts", databases.Document{
				ID:   "u2",
				Data: map[string]any{"email": "hit@b.c", k: 1},
			}, []string{"email"}, nil, principal)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "key %q", k)
			require.Contains(t, err.Error(), k, "key %q", k)
		}
	})

	t.Run("bulk update rejects illegal keys", func(t *testing.T) {
		for _, k := range []string{"my key", "_foo"} {
			_, err := docDB.BulkUpdateDocuments(ctx, projectID, "app", "posts", []string{created.ID},
				map[string]any{k: 1}, nil, principal)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "key %q", k)
			require.Contains(t, err.Error(), k, "key %q", k)
		}
	})

	t.Run("execute-tx ops reject illegal keys", func(t *testing.T) {
		long := strings.Repeat("x", 64)
		cases := []struct {
			op  databases.TransactionOp
			key string
		}{
			{databases.TransactionOp{Type: databases.TransactionOpCreate, CollectionID: "posts", DocumentID: "tx-c1", Data: map[string]any{"my key": 1}}, "my key"},
			{databases.TransactionOp{Type: databases.TransactionOpCreate, CollectionID: "posts", DocumentID: "tx-c2", Data: map[string]any{"a.b": 1}}, "a.b"},
			{databases.TransactionOp{Type: databases.TransactionOpCreate, CollectionID: "posts", DocumentID: "tx-c3", Data: map[string]any{"_foo": 1}}, "_foo"},
			{databases.TransactionOp{Type: databases.TransactionOpUpdate, CollectionID: "posts", DocumentID: created.ID, Data: map[string]any{long: 1}, ExpectedVersion: &created.Version}, long},
		}
		for i, tc := range cases {
			_, err := docDB.ExecuteTransactions(ctx, projectID, "app", []databases.TransactionOp{tc.op},
				databases.TransactionModeAtomic, principal)
			require.Error(t, err, "op %d", i)
			oe := databases.AsOpError(err)
			require.NotNil(t, oe, "op %d", i)
			require.Equal(t, codes.InvalidArgument, status.Code(oe.Err), "op %d", i)
			require.Contains(t, oe.Err.Error(), tc.key, "op %d", i)
		}
	})

	t.Run("legal keys still write through", func(t *testing.T) {
		updated, err := docDB.UpdateDocument(ctx, projectID, "app", "posts", databases.DocumentUpdate{
			Document:        databases.Document{ID: created.ID, Data: map[string]any{"views": 7}},
			Increment:       map[string]int64{"views": 0},
			ExpectedVersion: created.Version,
		}, principal)
		require.NoError(t, err)
		require.Equal(t, float64(7), updated.Data["views"])
	})
}
