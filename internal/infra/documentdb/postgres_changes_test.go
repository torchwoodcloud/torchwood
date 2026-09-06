package documentdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	domainevents "github.com/torchwooddev/torchwood/internal/domain/events"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/infra/events"
	"github.com/torchwooddev/torchwood/internal/testutil"
	"github.com/torchwooddev/torchwood/pkg/idgen"
)

// changesEnv 组装 ChangeFeed 测试环境（真实 PG；事件直接经 outbox publisher
// 落库——ChangeFeed 只读 outbox 表，无需建业务集合）。
func changesEnv(t *testing.T) (databases.DocumentDB, *clients.Database) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgresDocumentDB(db, events.NewEventOutbox(db)), db
}

// publishChange 落一条文档可见性为 user:<userID> 的文档事件，返回落库行。
func publishChange(t *testing.T, db *clients.Database, docID, event, userID string) model.DocumentEventsOutbox {
	t.Helper()
	ev := domainevents.Envelope{
		// EventID 预置：Publish 只在缺省时生成（改内部副本），回查行需要
		// 外侧可用的稳定 ID。
		EventID:      idgen.ULID().String(),
		Event:        event,
		ProjectID:    "default",
		DatabaseID:   "app",
		CollectionID: "docs",
		DocumentID:   docID,
		Version:      1,
		CreatedAt:    time.Now(),
		ACL: domainevents.ACLSnapshot{
			DocumentSecurity:    true,
			DocumentPermissions: []databases.Permission{{Type: "read", Role: "user:" + userID}},
			DocHasPerms:         true,
		},
	}
	if event != domainevents.EventDocumentsDelete {
		ev.Data = &databases.Document{ID: docID, Data: map[string]any{"t": "v"}, Version: 1}
	}
	require.NoError(t, events.NewEventOutbox(db).Publish(context.Background(), ev))
	var row model.DocumentEventsOutbox
	require.NoError(t, db.Conn(context.Background()).NewSelect().Model(&row).
		Where("event_id = ?", ev.EventID).Scan(context.Background()))
	return row
}

func u1Principal() databases.Principal {
	return databases.Principal{Roles: []string{"users", "user:u1"}}
}

// TestListChanges_OrderSinceAndTombstone：seq 升序、since 过滤、delete 为
// tombstone（无 data）。
func TestListChanges_OrderSinceAndTombstone(t *testing.T) {
	docDB, db := changesEnv(t)
	ctx := context.Background()

	r1 := publishChange(t, db, "d1", domainevents.EventDocumentsCreate, "u1")
	r2 := publishChange(t, db, "d2", domainevents.EventDocumentsCreate, "u1")
	r3 := publishChange(t, db, "d1", domainevents.EventDocumentsDelete, "u1")

	// since=0：全部三事件升序。
	changes, hasMore, next, err := docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{}, u1Principal())
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Zero(t, next, "自然耗尽游标为 0")
	require.Len(t, changes, 3)
	require.Equal(t, r1.Seq, changes[0].Seq)
	require.Equal(t, r2.Seq, changes[1].Seq)
	require.Equal(t, r3.Seq, changes[2].Seq)
	require.NotNil(t, changes[0].Data, "create 事件带写后全文档")
	require.Nil(t, changes[2].Data, "delete 事件无 data（tombstone）")
	require.Equal(t, "d1", changes[2].DocumentID)

	// since=r1：仅 r2、r3。
	changes, hasMore, next, err = docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{SinceSeq: r1.Seq}, u1Principal())
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Zero(t, next)
	require.Len(t, changes, 2)
	require.Equal(t, r2.Seq, changes[0].Seq)
}

// TestListChanges_VisibilityFilter：按请求者过滤（快照 ACL + 当前
// principal——与 hub 扇出同语义）。
func TestListChanges_VisibilityFilter(t *testing.T) {
	docDB, db := changesEnv(t)
	ctx := context.Background()

	publishChange(t, db, "d1", domainevents.EventDocumentsCreate, "u1")
	publishChange(t, db, "d2", domainevents.EventDocumentsCreate, "u2")

	changes, _, _, err := docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{}, u1Principal())
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Equal(t, "d1", changes[0].DocumentID)

	// 特权主体旁路（BypassesDocumentACL）。
	admin := databases.Principal{Roles: []string{"__system__"}}
	changes, _, _, err = docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{}, admin)
	require.NoError(t, err)
	require.Len(t, changes, 2)
}

// TestListChanges_LimitAndHasMore：limit 截断 + has_more + 末条 seq 续传链。
func TestListChanges_LimitAndHasMore(t *testing.T) {
	docDB, db := changesEnv(t)
	ctx := context.Background()

	var lastSeq int64
	for i := 0; i < 5; i++ {
		lastSeq = publishChange(t, db, "d", domainevents.EventDocumentsCreate, "u1").Seq
	}

	changes, hasMore, next, err := docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{Limit: 2}, u1Principal())
	require.NoError(t, err)
	require.Len(t, changes, 2)
	require.True(t, hasMore)
	require.Equal(t, changes[1].Seq, next, "满页退出游标 = 末条返回 seq")

	// 续传链：以末条 seq 为 since 直至 has_more=false。
	since := changes[1].Seq
	got := len(changes)
	for {
		nextChanges, more, _, err := docDB.ListChanges(ctx, "default", "app", "docs",
			databases.ListChangesOptions{SinceSeq: since, Limit: 2}, u1Principal())
		require.NoError(t, err)
		got += len(nextChanges)
		if !more {
			break
		}
		since = nextChanges[len(nextChanges)-1].Seq
	}
	require.Equal(t, 5, got, "续传链必须无重无漏覆盖全部 5 条")

	// since=最末：空集、无更多。
	changes, hasMore, next, err = docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{SinceSeq: lastSeq}, u1Principal())
	require.NoError(t, err)
	require.Empty(t, changes)
	require.False(t, hasMore)
	require.Zero(t, next)
}

// TestListChanges_CursorExits（R15 核心：(a)/(b) 两退出的游标精确断言）：
//   - (a) 满页退出：游标 = 末条*返回*事件 seq，续传首条恰为第 limit+1 条；
//   - (b) 扫描上限退出：游标 > 最后可见 seq（越过不可见块）。
func TestListChanges_CursorExits(t *testing.T) {
	docDB, db := changesEnv(t)
	ctx := context.Background()

	// ---- (a) 满页退出 ----
	rows := make([]model.DocumentEventsOutbox, 0, 5)
	for i := 0; i < 5; i++ {
		rows = append(rows, publishChange(t, db, "d", domainevents.EventDocumentsCreate, "u1"))
	}
	limit := 2
	page, hasMore, next, err := docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{Limit: limit}, u1Principal())
	require.NoError(t, err)
	require.True(t, hasMore)
	require.Len(t, page, limit)
	require.Equal(t, rows[limit-1].Seq, next, "(a) 游标 = 末条返回 seq")
	// 续传首条恰为第 limit+1 条（无重无漏）。
	nextChanges, _, _, err := docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{SinceSeq: next, Limit: limit}, u1Principal())
	require.NoError(t, err)
	require.Len(t, nextChanges, 2)
	require.Equal(t, rows[limit].Seq, nextChanges[0].Seq, "(a) 续传首条 = 第 limit+1 条")

	// ---- (b) 扫描上限退出 ----
	// 扫描页 = 500 行/页：head + 600 不可见 + tail，首页（500 行）全是
	// head + 不可见 → scanned=500 ≥ 缩小的上限 200 → 触顶。
	docDB2, db2 := changesEnv(t)
	head := publishChange(t, db2, "h1", domainevents.EventDocumentsCreate, "u1")
	for i := 0; i < 600; i++ { // 连续不可见块（他人私有）
		publishChange(t, db2, "x", domainevents.EventDocumentsCreate, "u2")
	}
	tail := publishChange(t, db2, "t1", domainevents.EventDocumentsCreate, "u1")

	// 缩扫描上限（var 覆写）：扫描页 500 行 >> 上限 200——首页即触顶。
	saved := maxChangesScanRows
	maxChangesScanRows = 200
	t.Cleanup(func() { maxChangesScanRows = saved })

	got, hasMore2, next2, err := docDB2.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{}, u1Principal())
	require.NoError(t, err)
	require.True(t, hasMore2, "(b) 上限退出 has_more=true（修前的静默 false 即漏失根源）")
	require.Equal(t, 1, len(got), "触顶前仅 head 可见（tail 在 500 行之外）")
	require.Equal(t, head.Seq, got[0].Seq)
	require.Greater(t, next2, head.Seq, "(b) 游标 = 扫描位置，越过不可见块")
	require.Less(t, next2, tail.Seq, "扫描游标不超过实际扫描到的行（tail 在首页之外）")
}

// TestListChanges_InvisibleBlockPagination（R15 核心验收）：可见 → 不可见
// 块（触扫描上限）→ 可见，分页跟随 next_since_seq 跨页全部取回两端可见
// 事件、无重无漏、不循环；块后无可见事件时正常终止。
func TestListChanges_InvisibleBlockPagination(t *testing.T) {
	docDB, db := changesEnv(t)
	ctx := context.Background()

	var wantSeqs []int64
	for i := 0; i < 2; i++ { // 头部可见（≤ limit=2，首查不触满页退出）
		wantSeqs = append(wantSeqs, publishChange(t, db, "h", domainevents.EventDocumentsCreate, "u1").Seq)
	}
	for i := 0; i < 600; i++ { // 连续不可见块（> 扫描页 500 行，触上限）
		publishChange(t, db, "x", domainevents.EventDocumentsCreate, "u2")
	}
	for i := 0; i < 4; i++ { // 尾部可见
		wantSeqs = append(wantSeqs, publishChange(t, db, "t", domainevents.EventDocumentsCreate, "u1").Seq)
	}

	saved := maxChangesScanRows
	maxChangesScanRows = 200
	t.Cleanup(func() { maxChangesScanRows = saved })

	// 分页循环：跟随 next_since_seq（0 时回退末条 seq——自然耗尽才为 0）。
	var gotSeqs []int64
	since := int64(0)
	pages := 0
	for {
		page, hasMore, next, err := docDB.ListChanges(ctx, "default", "app", "docs",
			databases.ListChangesOptions{SinceSeq: since, Limit: 2}, u1Principal())
		require.NoError(t, err)
		for _, c := range page {
			gotSeqs = append(gotSeqs, c.Seq)
		}
		pages++
		if !hasMore {
			break
		}
		cursor := next
		if cursor == 0 {
			require.NotEmpty(t, page, "has_more=true 且游标 0 时必须有末条可回退")
			cursor = page[len(page)-1].Seq
		}
		require.Greater(t, cursor, since, "游标必须严格前进（否则原地循环）")
		since = cursor
		require.Less(t, pages, 50, "分页必须收敛，不得原地循环")
	}
	require.Equal(t, wantSeqs, gotSeqs, "跨不可见块分页必须无重无漏取回两端全部可见事件")
}

// TestListChanges_InvisibleBlockToEnd：块后无可见事件——上限退出后一次
// 空查询即自然终止。
func TestListChanges_InvisibleBlockToEnd(t *testing.T) {
	docDB, db := changesEnv(t)
	ctx := context.Background()

	publishChange(t, db, "h", domainevents.EventDocumentsCreate, "u1")
	for i := 0; i < 550; i++ { // 不可见块 > 扫描页 500 行 → 首页即触顶
		publishChange(t, db, "x", domainevents.EventDocumentsCreate, "u2")
	}

	saved := maxChangesScanRows
	maxChangesScanRows = 200
	t.Cleanup(func() { maxChangesScanRows = saved })

	changes, hasMore, next, err := docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{}, u1Principal())
	require.NoError(t, err)
	require.True(t, hasMore, "上限退出 has_more=true")
	require.Len(t, changes, 1)
	require.Positive(t, next)

	// 块后无可见事件：游标续传自然耗尽。
	changes, hasMore, next, err = docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{SinceSeq: next}, u1Principal())
	require.NoError(t, err)
	require.Empty(t, changes)
	require.False(t, hasMore)
	require.Zero(t, next, "自然耗尽游标为 0")
}

// TestListChanges_DocumentFilter：documentID 过滤（WS 文档频道重放）。
func TestListChanges_DocumentFilter(t *testing.T) {
	docDB, db := changesEnv(t)
	ctx := context.Background()

	publishChange(t, db, "d1", domainevents.EventDocumentsCreate, "u1")
	publishChange(t, db, "d2", domainevents.EventDocumentsCreate, "u1")
	publishChange(t, db, "d1", domainevents.EventDocumentsUpdate, "u1")

	changes, _, _, err := docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{DocumentID: "d1"}, u1Principal())
	require.NoError(t, err)
	require.Len(t, changes, 2)
	for _, c := range changes {
		require.Equal(t, "d1", c.DocumentID)
	}
}

// TestListChanges_ResumeExpired：since_seq 早于集合最老可用事件（模拟
// 保留清理删旧行）→ ErrResumeExpired；since=0 不判过期。
func TestListChanges_ResumeExpired(t *testing.T) {
	docDB, db := changesEnv(t)
	ctx := context.Background()

	r1 := publishChange(t, db, "d1", domainevents.EventDocumentsCreate, "u1")
	r2 := publishChange(t, db, "d2", domainevents.EventDocumentsCreate, "u1")

	// 模拟清理：删掉最老行。
	_, err := db.Conn(ctx).NewDelete().Model((*model.DocumentEventsOutbox)(nil)).
		Where("seq = ?", r1.Seq).Exec(ctx)
	require.NoError(t, err)

	// since=r1（早于现存最老 r2）→ 过期。
	_, _, _, err = docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{SinceSeq: r1.Seq}, u1Principal())
	require.ErrorIs(t, err, databases.ErrResumeExpired)

	// since=0：从最老可用事件起，不判过期。
	changes, _, _, err := docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{}, u1Principal())
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Equal(t, r2.Seq, changes[0].Seq)

	// since=r2：正常续传（空集）。
	_, _, _, err = docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{SinceSeq: r2.Seq}, u1Principal())
	require.NoError(t, err)
}

// TestListChanges_TransactionIDPassthrough：批标识随事件透出。
func TestListChanges_TransactionIDPassthrough(t *testing.T) {
	docDB, db := changesEnv(t)
	ctx := context.Background()

	row := publishChange(t, db, "d1", domainevents.EventDocumentsCreate, "u1")
	// publishChange 走单文档路径（无 ctx 批标识）——transaction_id 空。
	changes, _, _, err := docDB.ListChanges(ctx, "default", "app", "docs",
		databases.ListChangesOptions{}, u1Principal())
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Empty(t, changes[0].TransactionID)
	require.Equal(t, row.Seq, changes[0].Seq)
	require.NotEmpty(t, changes[0].EventID)
}
