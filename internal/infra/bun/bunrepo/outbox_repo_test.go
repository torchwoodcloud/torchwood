package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

func TestOutboxRepo_ListDeadLetters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	repo := bunrepo.NewOutboxRepository(db)

	// 空项目应返回空
	letters, total, next, err := repo.ListDeadLetters(ctx, "proj-empty", 10, "")
	require.NoError(t, err)
	require.Equal(t, int64(0), total)
	require.Empty(t, letters)
	require.Empty(t, next)

	// 插入 3 条死信，2 条属于 p1，1 条属于 p2
	for i, pid := range []string{"p1", "p1", "p2"} {
		_, err := db.ExecContext(ctx, `INSERT INTO document_events_outbox_dead (event_id, project_id, topic, channel, payload, attempts, last_error, created_at, failed_at) VALUES (?, ?, 'topic', 'ch', '{}', 10, 'err', NOW(), NOW())`, "e"+string(rune('1'+i)), pid)
		require.NoError(t, err)
	}
	letters, total, next, err = repo.ListDeadLetters(ctx, "p1", 10, "")
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, letters, 2)
	require.Empty(t, next)

	// 分页：page_size=1
	letters, total, next, err = repo.ListDeadLetters(ctx, "p1", 1, "")
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, letters, 1)
	require.NotEmpty(t, next)
	letters2, _, next2, err := repo.ListDeadLetters(ctx, "p1", 1, next)
	require.NoError(t, err)
	require.Len(t, letters2, 1)
	require.Empty(t, next2)
	require.NotEqual(t, letters[0].EventID, letters2[0].EventID)
}

func TestOutboxRepo_ReplayDeadLetter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	repo := bunrepo.NewOutboxRepository(db)

	// 插入一条死信
	_, err := db.ExecContext(ctx, `INSERT INTO document_events_outbox_dead (event_id, project_id, topic, channel, payload, attempts, last_error, created_at, failed_at) VALUES ('e1', 'p1', 'topic', 'ch', '{"a":1}', 10, 'err', NOW(), NOW())`)
	require.NoError(t, err)

	// 重放成功
	require.NoError(t, repo.ReplayDeadLetter(ctx, "e1", "p1"))

	// 死信应被删除，outbox 应有行
	n, err := db.NewSelect().Model((*struct {
		ID string `bun:"event_id"`
	})(nil)).TableExpr("document_events_outbox_dead").Where("event_id = ?", "e1").Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	n, err = db.NewSelect().Model((*struct {
		ID string `bun:"event_id"`
	})(nil)).TableExpr("document_events_outbox").Where("event_id = ?", "e1").Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// 幂等：二次重放应成功且不产生重复
	require.NoError(t, repo.ReplayDeadLetter(ctx, "e1", "p1"))
	n, err = db.NewSelect().TableExpr("document_events_outbox").Where("event_id = ?", "e1").Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// project 错配应失败（不存在于 dead 且不在 outbox 的 project 错配）
	_, err = db.ExecContext(ctx, `INSERT INTO document_events_outbox_dead (event_id, project_id, topic, channel, payload, attempts, last_error, created_at, failed_at) VALUES ('e2', 'p1', 'topic', 'ch', '{}', 10, 'err', NOW(), NOW())`)
	require.NoError(t, err)
	err = repo.ReplayDeadLetter(ctx, "e2", "p2")
	require.Error(t, err)

	// 超时与隔离：available_at 应为 NOW 附近
	var available time.Time
	require.NoError(t, db.NewSelect().TableExpr("document_events_outbox").Column("available_at").Where("event_id = ?", "e1").Scan(ctx, &available))
	require.WithinDuration(t, time.Now(), available, 5*time.Second)
}
