package bunrepo_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainwebhooks "github.com/torchwoodcloud/torchwood/internal/domain/webhooks"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// seedWebhookRepo 落一个订阅（绕过 app 层；密文任意——仓储层不做解密）。
func seedWebhookRepo(t *testing.T, ctx context.Context, repo domainwebhooks.WebhookRepo, projectID, id string, status string) *domainwebhooks.Webhook {
	t.Helper()
	w := &domainwebhooks.Webhook{
		ID:                id,
		ProjectID:         projectID,
		URL:               "https://example.com/hook",
		Events:            []string{"databases.app.collections.notes.documents.create"},
		Status:            status,
		SecretEnc:         "enc:v1:test",
		SecretFingerprint: "whsec_deadbeef",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	require.NoError(t, repo.CreateWebhook(ctx, w))
	return w
}

func deliveryRow(t *testing.T, projectID, webhookID, eventID string, attempts int, due time.Time) *domainwebhooks.Delivery {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"specversion": "1.0", "id": "whd-x"})
	require.NoError(t, err)
	return &domainwebhooks.Delivery{
		ID: "whd-" + eventID, ProjectID: projectID, WebhookID: webhookID,
		EventID: eventID, EventType: "auth.users.created", Seq: 1,
		Payload: payload, Status: domainwebhooks.DeliveryStatusPending,
		Attempts: attempts, NextAttemptAt: due,
		CreatedAt: due, UpdatedAt: due,
	}
}

// TestWebhookDeliveryRepository_ClaimIdempotencyAndLifecycle 投递仓储核心
// 语义（迁移 000027）：幂等插入、到期领取（attempts/next 推进）、收口状态
// 机（仅 pending 可收口）、手动重置、保留窗清理、跨项目隔离。
func TestWebhookDeliveryRepository_ClaimIdempotencyAndLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	whRepo := bunrepo.NewWebhookRepository(db)
	dRepo := bunrepo.NewWebhookDeliveryRepository(db)
	wh := seedWebhookRepo(t, ctx, whRepo, projectID, "wh-1", domainwebhooks.StatusEnabled)

	now := time.Now()
	due := now.Add(-time.Minute)

	// ① 幂等插入：重复 (webhook_id, event_id) 被唯一索引吸收。
	n, err := dRepo.CreateDeliveries(ctx, []*domainwebhooks.Delivery{
		deliveryRow(t, projectID, wh.ID, "ev-1", 0, due),
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = dRepo.CreateDeliveries(ctx, []*domainwebhooks.Delivery{
		deliveryRow(t, projectID, wh.ID, "ev-1", 0, due),
	})
	require.NoError(t, err)
	require.Zero(t, n, "唯一索引吸收 at-least-once 重复")

	// ② 到期领取：CAS 推进 attempts/next_attempt_at（梯度 10s）。
	claims, err := dRepo.ClaimDue(ctx, projectID, now, 10,
		func(d *domainwebhooks.Delivery) domainwebhooks.AttemptPlan {
			return domainwebhooks.AttemptPlan{Attempt: true, Next: now.Add(10 * time.Second)}
		})
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.Equal(t, 1, claims[0].Attempts)
	require.WithinDuration(t, now.Add(10*time.Second), claims[0].NextAttemptAt, time.Second)

	// 未到期行不再领取。
	claims, err = dRepo.ClaimDue(ctx, projectID, now, 10,
		func(d *domainwebhooks.Delivery) domainwebhooks.AttemptPlan {
			return domainwebhooks.AttemptPlan{Attempt: true, Next: now}
		})
	require.NoError(t, err)
	require.Empty(t, claims)

	// ③ 收口状态机：delivered。
	require.NoError(t, dRepo.MarkDelivered(ctx, projectID, "whd-ev-1", now, 200))
	got, err := dRepo.GetDelivery(ctx, projectID, wh.ID, "whd-ev-1")
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.DeliveryStatusDelivered, got.Status)
	require.Equal(t, 200, got.LastResponseCode)

	// 终态行不再被领取扫描（partial 索引口径）。
	claims, err = dRepo.ClaimDue(ctx, projectID, now.Add(time.Hour), 10,
		func(d *domainwebhooks.Delivery) domainwebhooks.AttemptPlan {
			return domainwebhooks.AttemptPlan{Attempt: true, Next: now}
		})
	require.NoError(t, err)
	require.Empty(t, claims)

	// ④ 手动重置：终态 → pending/attempts=0/立即到期；pending 行拒绝。
	_, err = dRepo.CreateDeliveries(ctx, []*domainwebhooks.Delivery{
		deliveryRow(t, projectID, wh.ID, "ev-2", 3, due.Add(-time.Hour)),
	})
	require.NoError(t, err)
	require.NoError(t, dRepo.MarkFailed(ctx, projectID, "whd-ev-2", now, 503, "retry policy exhausted"))
	ok, err := dRepo.ResetForRedeliver(ctx, projectID, wh.ID, "whd-ev-2", now)
	require.NoError(t, err)
	require.True(t, ok)
	got, err = dRepo.GetDelivery(ctx, projectID, wh.ID, "whd-ev-2")
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.DeliveryStatusPending, got.Status)
	require.Zero(t, got.Attempts)

	ok, err = dRepo.ResetForRedeliver(ctx, projectID, wh.ID, "whd-ev-2", now)
	require.NoError(t, err)
	require.False(t, ok, "pending 行拒绝重投（已在投递队列）")

	// ⑤ 列表过滤 + 跨项目隔离（未命中一律 NotFound，不泄露存在性）。
	page, err := dRepo.ListDeliveries(ctx, projectID, wh.ID, domainwebhooks.DeliveryStatusPending, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, page.Total)
	otherProject, _, cleanupOther := testutil.CreateTestProject(ctx, db)
	defer cleanupOther()
	_, err = dRepo.GetDelivery(ctx, otherProject, wh.ID, "whd-ev-1")
	require.ErrorIs(t, err, domainwebhooks.ErrNotFound)

	// ⑥ 终态失败 + 保留窗清理（pending 不清理）。
	require.NoError(t, dRepo.MarkFailed(ctx, projectID, "whd-ev-1", now, 503, "retry policy exhausted"))
	pruned, err := dRepo.PruneTerminal(ctx, projectID, now.Add(time.Minute), 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), pruned)
	_, err = dRepo.GetDelivery(ctx, projectID, wh.ID, "whd-ev-1")
	require.ErrorIs(t, err, domainwebhooks.ErrNotFound)
	got, err = dRepo.GetDelivery(ctx, projectID, wh.ID, "whd-ev-2")
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.DeliveryStatusPending, got.Status, "pending 行不进清理口径")
}

// TestWebhookRepository_EnabledScanAndQuota 订阅仓储：paused 不进快照扫描、
// 计数、更新列白名单（bun 更新写规范——显式 .Column，不触碰 created_at）。
func TestWebhookRepository_EnabledScanAndQuota(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	repo := bunrepo.NewWebhookRepository(db)

	enabled := seedWebhookRepo(t, ctx, repo, projectID, "wh-on", domainwebhooks.StatusEnabled)
	seedWebhookRepo(t, ctx, repo, projectID, "wh-off", domainwebhooks.StatusPaused)

	scan, err := repo.ListEnabledWebhooks(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, scan, 1)
	require.Equal(t, "wh-on", scan[0].ID)

	n, err := repo.CountWebhooks(ctx, projectID)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	// 更新：可变列生效（url/events/status + 密钥四列轮换路径）；
	// created_at 不被 UPDATE 触碰。
	enabled.URL = "https://other.example.com/hook"
	enabled.Status = domainwebhooks.StatusPaused
	require.NoError(t, repo.UpdateWebhook(ctx, enabled))
	got, err := repo.GetWebhook(ctx, projectID, "wh-on")
	require.NoError(t, err)
	require.Equal(t, "https://other.example.com/hook", got.URL)
	require.Equal(t, domainwebhooks.StatusPaused, got.Status)
	require.WithinDuration(t, enabled.CreatedAt, got.CreatedAt, time.Second)

	// 不存在归一 ErrNotFound。
	_, err = repo.GetWebhook(ctx, projectID, "wh-missing")
	require.ErrorIs(t, err, domainwebhooks.ErrNotFound)
	require.Error(t, repo.DeleteWebhook(ctx, projectID, "wh-missing"))
}
