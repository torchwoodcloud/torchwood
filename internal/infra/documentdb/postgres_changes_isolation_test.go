package documentdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/events"
	"github.com/torchwooddev/torchwood/internal/testutil"
)

// isolationEnv 建项目 A / B 双项目环境：各建 default 库 + 同名集合 foo
// （catalog 权限默认集：create:keys + read:any，documentSecurity=false——
// ACL 层对任何主体可见，泄漏只能归因于 project 维度缺失）。
func isolationEnv(t *testing.T, ctx context.Context) (databases.DocumentDB, string, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	docDB := NewPostgresDocumentDB(db, events.NewEventOutbox(db))

	projA, _, cleanupA := testutil.CreateTestProjectT(ctx, t, db)
	t.Cleanup(cleanupA)
	projB, _, cleanupB := testutil.CreateTestProjectT(ctx, t, db)
	t.Cleanup(cleanupB)

	for _, pid := range []string{projA, projB} {
		require.NoError(t, docDB.CreateDatabase(ctx, pid, "default", "Default DB"))
		require.NoError(t, docDB.CreateCollection(ctx, pid, "default", "foo", "Foo", []databases.Attribute{
			{ID: "title", Key: "title", Type: "string", Size: 256},
		}, nil, []databases.Permission{
			{Type: "create", Role: "keys"},
			{Type: "read", Role: "any"},
		}, false))
	}
	return docDB, projA, projB
}

// keyPrincipal 构造 API key（ActorKind=Service）的文档主体投影。
func keyPrincipal(keyID string) databases.Principal {
	return databases.Principal{Roles: []string{"keys"}, KeyID: keyID}
}

// TestListChanges_CrossProjectSameNameIsolated（B-1 回归）：topic/频道 =
// databases.<db>.collections.<coll> 不含 project 维度——跨项目同名集合
// （default/foo）产生相同 topic。事件面必须按 outbox.project_id 隔离：
// B 写文档后，A 的 API key 主体调 :changes 不得看到 B 的事件（含文档全文）。
func TestListChanges_CrossProjectSameNameIsolated(t *testing.T) {
	docDB, projA, projB := isolationEnv(t, context.Background())
	ctx := context.Background()

	// B 写文档（真实 Create 路径产生 outbox 事件）。
	_, err := docDB.CreateDocument(ctx, projB, "default", "foo", databases.Document{
		ID:   "secret-b",
		Data: map[string]any{"title": "B-secret"},
	}, nil, keyPrincipal("kb"))
	require.NoError(t, err)

	// A 的 key 主体读自己项目的 :changes——必须零返回（续传游标 0）。
	changes, hasMore, next, err := docDB.ListChanges(ctx, projA, "default", "foo",
		databases.ListChangesOptions{}, keyPrincipal("ka"))
	require.NoError(t, err)
	require.Empty(t, changes, "B-1：A 不得看到 B 项目同名集合的事件（含文档全文）")
	require.False(t, hasMore)
	require.Zero(t, next)

	// 反向：B 读也不见 A 的事件（当前 B 视角恰为空集，写入 A 侧后双向验证）。
	_, err = docDB.CreateDocument(ctx, projA, "default", "foo", databases.Document{
		ID:   "own-a",
		Data: map[string]any{"title": "A-own"},
	}, nil, keyPrincipal("ka"))
	require.NoError(t, err)

	changesB, _, _, err := docDB.ListChanges(ctx, projB, "default", "foo",
		databases.ListChangesOptions{}, keyPrincipal("kb"))
	require.NoError(t, err)
	require.Len(t, changesB, 1, "B 只能看到自己的事件")
	require.Equal(t, "secret-b", changesB[0].DocumentID)
	require.Equal(t, "B-secret", changesB[0].Data.Data["title"])

	// 正向功能不回归：A 自己写自己读全通（只含自己的 1 条）。
	changesA, _, _, err := docDB.ListChanges(ctx, projA, "default", "foo",
		databases.ListChangesOptions{}, keyPrincipal("ka"))
	require.NoError(t, err)
	require.Len(t, changesA, 1)
	require.Equal(t, "own-a", changesA[0].DocumentID)
}

// TestListChanges_CrossProjectCursorChain（B-1 + R15 组合）：跨项目隔离下
// 分页/续传游标链仍无重无漏——A 连续写 5 条、B 交错写 3 条，A 的 :changes
// 分页遍历恰好取回自己的 5 条（seq 全升序、无 B 的 seq）。
func TestListChanges_CrossProjectCursorChain(t *testing.T) {
	docDB, projA, projB := isolationEnv(t, context.Background())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := docDB.CreateDocument(ctx, projA, "default", "foo", databases.Document{
			ID:   "a-" + string(rune('0'+i)),
			Data: map[string]any{"title": "A"},
		}, nil, keyPrincipal("ka"))
		require.NoError(t, err)
		if i%2 == 0 {
			_, err := docDB.CreateDocument(ctx, projB, "default", "foo", databases.Document{
				ID:   "b-" + string(rune('0'+i)),
				Data: map[string]any{"title": "B"},
			}, nil, keyPrincipal("kb"))
			require.NoError(t, err)
		}
	}

	// 分页遍历 A 的 :changes。
	var got []databases.DocumentChange
	since := int64(0)
	for {
		page, hasMore, next, err := docDB.ListChanges(ctx, projA, "default", "foo",
			databases.ListChangesOptions{SinceSeq: since, Limit: 2}, keyPrincipal("ka"))
		require.NoError(t, err)
		got = append(got, page...)
		if !hasMore {
			break
		}
		cursor := next
		if cursor == 0 {
			require.NotEmpty(t, page)
			cursor = page[len(page)-1].Seq
		}
		require.Greater(t, cursor, since, "游标必须严格前进")
		since = cursor
	}
	require.Len(t, got, 5, "A 恰好取回自己的 5 条（B 的 3 条不可见）")
	for i := 1; i < len(got); i++ {
		require.Less(t, got[i-1].Seq, got[i].Seq, "seq 升序无重无漏")
	}
	for _, c := range got {
		require.Contains(t, c.DocumentID, "a-", "不得混入 B 项目文档")
	}
}
