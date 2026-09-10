package bunrepo_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"github.com/uptrace/bun"
)

// TryClaim/Complete/Release 语义（redesign §4.1/§10.1）：认领-完成-重放-冲突。
func TestIdempotencyStore_ClaimCompleteReplayConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	s := bunrepo.NewIdempotencyStore(db)
	key := databases.IdempotencyKey{ProjectID: "p1", ActorID: "a1", RequestID: "r1"}

	c1, err := s.TryClaim(ctx, key, "fp1")
	require.NoError(t, err)
	require.Equal(t, databases.IdempotencyClaimAcquired, c1.State)
	require.NotEmpty(t, c1.Token)

	// 认领后同指纹重试 → InFlight。
	c1b, err := s.TryClaim(ctx, key, "fp1")
	require.NoError(t, err)
	require.Equal(t, databases.IdempotencyClaimInFlight, c1b.State)

	// 完成后同指纹 → Done + payload 重放。
	require.NoError(t, s.Complete(ctx, key, c1.Token, []byte(`{"ok":1}`)))
	c2, err := s.TryClaim(ctx, key, "fp1")
	require.NoError(t, err)
	require.Equal(t, databases.IdempotencyClaimDone, c2.State)
	require.JSONEq(t, `{"ok":1}`, string(c2.Payload))

	// 同 key 不同指纹 → KEY_CONFLICT。
	_, err = s.TryClaim(ctx, key, "fp2")
	require.ErrorIs(t, err, databases.ErrIdempotencyKeyConflict)

	// Release 只作用于 in_flight 行（done 行不受影响）。
	c3, err := s.TryClaim(ctx, databases.IdempotencyKey{ProjectID: "p1", ActorID: "a1", RequestID: "r2"}, "fp1")
	require.NoError(t, err)
	require.NoError(t, s.Release(ctx, databases.IdempotencyKey{ProjectID: "p1", ActorID: "a1", RequestID: "r2"}, c3.Token))
	c4, err := s.TryClaim(ctx, databases.IdempotencyKey{ProjectID: "p1", ActorID: "a1", RequestID: "r2"}, "fp1")
	require.NoError(t, err)
	require.Equal(t, databases.IdempotencyClaimAcquired, c4.State, "Release 后同键可重新认领")
}

// releaseRaceHook 确定性模拟 R8 竞态：TryClaim 的 INSERT 撞上冲突行后、SELECT
// 读回前，对端恰好 Release（行被删）——在 AfterQuery 钩子里于冲突 INSERT 完成
// 后立即删除该行，制造 "INSERT 0 rows + SELECT ErrNoRows" 窗口。
type releaseRaceHook struct {
	db      *clients.Database
	key     databases.IdempotencyKey
	inserts atomic.Int32 // ON CONFLICT INSERT 执行次数
	fired   atomic.Bool  // 只在首次冲突后触发一次删除
}

func (h *releaseRaceHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *releaseRaceHook) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	if !strings.Contains(event.Query, "ON CONFLICT") {
		return
	}
	h.inserts.Add(1)
	if h.fired.CompareAndSwap(false, true) {
		_, _ = h.db.Conn(context.Background()).ExecContext(context.Background(),
			`DELETE FROM idempotency_keys WHERE project_id = ? AND actor_id = ? AND request_id = ?`,
			h.key.ProjectID, h.key.ActorID, h.key.RequestID)
	}
}

// TestIdempotencyStore_TryClaimRetriesOnVanishingConflict（返工 R8）：
// INSERT 冲突 + SELECT ErrNoRows 的竞态经有界重试自愈（第二次 INSERT 直接
// 认领成功），不再把瞬态当故障上抛。
func TestIdempotencyStore_TryClaimRetriesOnVanishingConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	s := bunrepo.NewIdempotencyStore(db)
	key := databases.IdempotencyKey{ProjectID: "p1", ActorID: "a1", RequestID: "race-1"}

	// 预置对端 in_flight 占位行（对端正在执行）。
	peer, err := s.TryClaim(ctx, key, "fp1")
	require.NoError(t, err)
	require.Equal(t, databases.IdempotencyClaimAcquired, peer.State)

	hook := &releaseRaceHook{db: db, key: key}
	db.AddQueryHook(hook)

	claim, err := s.TryClaim(ctx, key, "fp1")
	require.NoError(t, err, "竞态（冲突行消失）应经重试自愈，不上抛")
	require.Equal(t, databases.IdempotencyClaimAcquired, claim.State)
	require.NotEmpty(t, claim.Token)
	require.GreaterOrEqual(t, hook.inserts.Load(), int32(2), "必须实际发生重试（第二次 INSERT 认领成功）")

	// 重试后拿到的新认领可正常完成（token 归属正确）。
	require.NoError(t, s.Complete(ctx, key, claim.Token, []byte(`{"healed":true}`)))
	replayed, err := s.TryClaim(ctx, key, "fp1")
	require.NoError(t, err)
	require.Equal(t, databases.IdempotencyClaimDone, replayed.State)
	require.JSONEq(t, `{"healed":true}`, string(replayed.Payload))
}
