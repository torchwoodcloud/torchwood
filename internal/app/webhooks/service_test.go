package webhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	domainwebhooks "github.com/torchwoodcloud/torchwood/internal/domain/webhooks"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

// serverPrincipalCtx 返回携带平台 admin principal 的上下文（CRUD 写方法
// use-case 层守卫 RequireServerPrincipal——镜像 functions 侧 authz_test
// 同款注入）。
func serverPrincipalCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &domainshared.Principal{
		ActorID:         "admin-1",
		ActorKind:       domainshared.ActorKindAdmin,
		IsPlatformAdmin: true,
		UserID:          "admin-1",
	})
}

// CRUD 面：创建校验（https 强制/订阅串 eventmatch 解析/配额）、密钥一次性
// 明文、更新 presence 语义、轮换双钥宽限窗。

func TestCreateWebhook_GeneratesSecretAndValidates(t *testing.T) {
	repo := newFakeWebhookRepo()
	svc := newTestService(t, testConfig(), repo, newFakeDeliveryRepo(), &fakeNotifier{}, projectsStub{ids: []string{"p1"}})
	ctx := serverPrincipalCtx()

	w, secret, err := svc.CreateWebhook(ctx, CreateWebhookCommand{
		ProjectID: "p1",
		URL:       "https://example.com/hook",
		Events:    []string{"databases.app.collections.notes.documents.create", "auth.*"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, w.ID)
	require.True(t, len(w.ID) > 3 && w.ID[:3] == "wh-")
	// 密钥：明文一次性返回、指纹可复算、密文不含明文。
	require.Equal(t, "whsec_", secret[:6])
	require.Equal(t, domainwebhooks.Fingerprint(secret), w.SecretFingerprint)
	require.NotContains(t, w.SecretEnc, secret)
	stored, err := repo.GetWebhook(context.Background(), "p1", w.ID)
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.StatusEnabled, stored.Status)

	// http:// 拒绝。
	_, _, err = svc.CreateWebhook(ctx, CreateWebhookCommand{
		ProjectID: "p1", URL: "http://example.com/hook",
		Events: []string{"auth.*"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 订阅串格式错误拒绝（eventmatch fail-closed）。
	_, _, err = svc.CreateWebhook(ctx, CreateWebhookCommand{
		ProjectID: "p1", URL: "https://example.com/hook",
		Events: []string{"databases.*.collections.notes.documents.create"},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 空订阅串集拒绝。
	_, _, err = svc.CreateWebhook(ctx, CreateWebhookCommand{
		ProjectID: "p1", URL: "https://example.com/hook",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestCreateWebhook_QuotaEnforced(t *testing.T) {
	repo := newFakeWebhookRepo()
	svc := newTestService(t, testConfig(), repo, newFakeDeliveryRepo(), &fakeNotifier{}, projectsStub{ids: []string{"p1"}})
	ctx := serverPrincipalCtx()
	for i := 0; i < 2; i++ {
		_, _, err := svc.CreateWebhook(ctx, CreateWebhookCommand{
			ProjectID: "p1", URL: "https://example.com/hook", Events: []string{"auth.*"},
		})
		require.NoError(t, err)
	}
	_, _, err := svc.CreateWebhook(ctx, CreateWebhookCommand{
		ProjectID: "p1", URL: "https://example.com/hook", Events: []string{"auth.*"},
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "配额 = testConfig MaxPerProject 2")
}

func TestUpdateWebhook_PartialSemantics(t *testing.T) {
	repo := newFakeWebhookRepo()
	svc := newTestService(t, testConfig(), repo, newFakeDeliveryRepo(), &fakeNotifier{}, projectsStub{ids: []string{"p1"}})
	ctx := serverPrincipalCtx()
	w, _, err := svc.CreateWebhook(ctx, CreateWebhookCommand{
		ProjectID: "p1", URL: "https://example.com/hook", Events: []string{"auth.*"},
	})
	require.NoError(t, err)

	// 只改 url：events 不动、密钥列不被触碰。
	newURL := "https://other.example.com/hook"
	got, err := svc.UpdateWebhook(ctx, UpdateWebhookCommand{
		ProjectID: "p1", WebhookID: w.ID, URL: &newURL,
	})
	require.NoError(t, err)
	require.Equal(t, newURL, got.URL)
	require.Equal(t, []string{"auth.*"}, got.Events)
	stored, err := repo.GetWebhook(context.Background(), "p1", w.ID)
	require.NoError(t, err)
	require.Equal(t, w.SecretEnc, stored.SecretEnc)

	// events 全量替换（EventsSet 表达 presence）。
	got, err = svc.UpdateWebhook(ctx, UpdateWebhookCommand{
		ProjectID: "p1", WebhookID: w.ID, Events: []string{"payments.orders.paid"}, EventsSet: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"payments.orders.paid"}, got.Events)

	// 非法 status 拒绝。
	bad := "disabled"
	_, err = svc.UpdateWebhook(ctx, UpdateWebhookCommand{
		ProjectID: "p1", WebhookID: w.ID, Status: &bad,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 不存在归一 NotFound（跨项目存在性不泄露）。
	_, err = svc.GetWebhook(ctx, "p2", w.ID)
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestRotateWebhookSecret_DoubleKeyWindow(t *testing.T) {
	repo := newFakeWebhookRepo()
	svc := newTestService(t, testConfig(), repo, newFakeDeliveryRepo(), &fakeNotifier{}, projectsStub{ids: []string{"p1"}})
	ctx := serverPrincipalCtx()
	w, oldSecret, err := svc.CreateWebhook(ctx, CreateWebhookCommand{
		ProjectID: "p1", URL: "https://example.com/hook", Events: []string{"auth.*"},
	})
	require.NoError(t, err)

	// 窗口内轮换：新钥即刻生效、旧钥进 previous 槽、两钥皆可解。
	got, newSecret, err := svc.RotateWebhookSecret(ctx, "p1", w.ID)
	require.NoError(t, err)
	require.NotEqual(t, oldSecret, newSecret)
	require.Equal(t, domainwebhooks.Fingerprint(newSecret), got.SecretFingerprint)

	stored, err := repo.GetWebhook(context.Background(), "p1", w.ID)
	require.NoError(t, err)
	cur, prev, err := svc.effectiveSecrets(context.Background(), stored)
	require.NoError(t, err)
	require.Equal(t, newSecret, cur, "当前钥 = 新钥")
	require.Equal(t, oldSecret, prev, "宽限窗内旧钥可解（v2 签名通道）")

	// 双签语义：新旧两钥对同一 body 的签名都能被 VerifySignatureHeader 验过
	//（复用 functions 协议面——webhook 接收方视角）。
	body := []byte(`{"specversion":"1.0"}`)
	ts := time.Now().Unix()
	header := domainfunctions.BuildSignatureHeader(ts, [][2]string{
		{"v1", domainfunctions.ComputeSignature(cur, ts, body)},
		{"v2", domainfunctions.ComputeSignature(prev, ts, body)},
	})
	require.NoError(t, domainfunctions.VerifySignatureHeader(header, body, []string{newSecret, oldSecret}, time.Now()))

	// 宽限窗外（rotated_at 回拨 25h）：prev 收敛为空——旧钥签名通道关闭。
	stored.SecretRotatedAt = time.Now().Add(-25 * time.Hour)
	cur2, prev2, err := svc.effectiveSecrets(context.Background(), stored)
	require.NoError(t, err)
	require.Equal(t, newSecret, cur2)
	require.Empty(t, prev2, "窗外旧钥不再携带")
}

// ——投递引擎面——

// dueDelivery 直落一条到期投递行（eventID 区分多行；绕过 CreateDeliveries
// 的信封构造以精确控制 attempts/created_at）。
func dueDelivery(t *testing.T, drepo *fakeDeliveryRepo, webhookID, eventID string, attempts int, createdAt time.Time) *domainwebhooks.Delivery {
	t.Helper()
	payload, err := json.Marshal(domainfunctions.CloudEvent{
		Specversion: "1.0", ID: "whd-x", Source: "projects/p1",
		Type: "auth.users.created", DataContentType: "application/json",
		Data: json.RawMessage(`{"id":"u1"}`), TwProject: "p1", TwSeq: 7,
	})
	require.NoError(t, err)
	d := &domainwebhooks.Delivery{
		ID: "whd-" + webhookID + "-" + eventID, ProjectID: "p1", WebhookID: webhookID,
		EventID: eventID, EventType: "auth.users.created", Seq: 7,
		Payload: payload, Status: domainwebhooks.DeliveryStatusPending,
		Attempts: attempts, NextAttemptAt: createdAt, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	n, err := drepo.CreateDeliveries(context.Background(), []*domainwebhooks.Delivery{d})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	return d
}

func TestRunDueDeliveries_Success(t *testing.T) {
	repo := newFakeWebhookRepo()
	drepo := newFakeDeliveryRepo()
	notifier := &fakeNotifier{}
	svc := newTestService(t, testConfig(), repo, drepo, notifier, projectsStub{ids: []string{"p1"}})
	w := seedWebhook(t, repo, "wh-1")
	dueDelivery(t, drepo, w.ID, "ev-1", 0, time.Now().Add(-time.Minute))

	n, err := svc.RunDueDeliveries(context.Background(), time.Now(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, notifier.count())
	call := notifier.callAt(0)
	require.Equal(t, w.URL, call.TargetURL)
	// 双签通道：创建路径未轮换 → prev 为空。
	require.NotEmpty(t, call.TargetSecret)
	require.Empty(t, call.TargetPreviousSecret)
	// 信封完整：structured mode 属性齐备。
	require.Equal(t, "1.0", call.Event.Specversion)
	require.Equal(t, "auth.users.created", call.Event.Type)
	require.Equal(t, "projects/p1", call.Event.Source)
	require.Equal(t, int64(7), call.Event.TwSeq)

	got, err := drepo.GetDelivery(context.Background(), "p1", w.ID, "whd-wh-1-ev-1")
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.DeliveryStatusDelivered, got.Status)
	require.Equal(t, 200, got.LastResponseCode)
	require.Empty(t, got.LastError)
	require.Equal(t, 1, got.Attempts)
}

func TestRunDueDeliveries_RetryLadderAdvances(t *testing.T) {
	repo := newFakeWebhookRepo()
	drepo := newFakeDeliveryRepo()
	// 恒失败（非 2xx 形态：HTTPNotifier 的错误文本携带 "status N"）。
	notifier := &fakeNotifier{last: status.Errorf(codes.Unavailable, "notify: endpoint status 500")}
	svc := newTestService(t, testConfig(), repo, drepo, notifier, projectsStub{ids: []string{"p1"}})
	w := seedWebhook(t, repo, "wh-1")
	dueDelivery(t, drepo, w.ID, "ev-1", 0, time.Now().Add(-time.Minute))

	now := time.Now()
	// 第 1 次尝试失败 → next = now + backoff[0]（10ms 档）。
	_, err := svc.RunDueDeliveries(context.Background(), now, 10)
	require.NoError(t, err)
	got, err := drepo.GetDelivery(context.Background(), "p1", w.ID, "whd-wh-1-ev-1")
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.DeliveryStatusPending, got.Status, "非终态失败保持 pending")
	require.Equal(t, 1, got.Attempts)
	require.Equal(t, 500, got.LastResponseCode)
	require.NotEmpty(t, got.LastError)
	require.WithinDuration(t, now.Add(10*time.Millisecond), got.NextAttemptAt, 2*time.Millisecond)

	// 梯度第 2 档（20ms）。
	now2 := got.NextAttemptAt.Add(time.Millisecond)
	_, err = svc.RunDueDeliveries(context.Background(), now2, 10)
	require.NoError(t, err)
	got, err = drepo.GetDelivery(context.Background(), "p1", w.ID, "whd-wh-1-ev-1")
	require.NoError(t, err)
	require.Equal(t, 2, got.Attempts)
	require.WithinDuration(t, now2.Add(20*time.Millisecond), got.NextAttemptAt, 2*time.Millisecond)
	require.Equal(t, 2, notifier.count())
}

func TestRunDueDeliveries_TerminalAfterMaxAttempts_DLQ(t *testing.T) {
	repo := newFakeWebhookRepo()
	drepo := newFakeDeliveryRepo()
	notifier := &fakeNotifier{last: status.Errorf(codes.Unavailable, "notify: endpoint status 503")}
	svc := newTestService(t, testConfig(), repo, drepo, notifier, projectsStub{ids: []string{"p1"}})
	w := seedWebhook(t, repo, "wh-1")
	// 已试 2 次（max_attempts=3）：下一次尝试是第 3 次（最后一次机会）。
	dueDelivery(t, drepo, w.ID, "ev-1", 2, time.Now().Add(-time.Minute))

	now := time.Now()
	n, err := svc.RunDueDeliveries(context.Background(), now, 10)
	require.NoError(t, err)
	require.Equal(t, 1, n, "第 3 次尝试仍执行")
	require.Equal(t, 1, notifier.count())

	// 第 3 次失败后：梯度耗尽，领取期终态判定直接 DLQ、不经 HTTP。
	n, err = svc.RunDueDeliveries(context.Background(), now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, 0, n, "终态行不再领取")
	require.Equal(t, 1, notifier.count())

	got, err := drepo.GetDelivery(context.Background(), "p1", w.ID, "whd-wh-1-ev-1")
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.DeliveryStatusFailed, got.Status, "failed 即 DLQ")
}

func TestRunDueDeliveries_RetryWindowCaps(t *testing.T) {
	repo := newFakeWebhookRepo()
	drepo := newFakeDeliveryRepo()
	notifier := &fakeNotifier{last: status.Errorf(codes.Unavailable, "notify: dial refused")}
	svc := newTestService(t, testConfig(), repo, drepo, notifier, projectsStub{ids: []string{"p1"}})
	w := seedWebhook(t, repo, "wh-1")
	// created_at 回拨 2h（retry_window=1h）：即使 attempts 未耗尽也终态。
	dueDelivery(t, drepo, w.ID, "ev-1", 0, time.Now().Add(-2*time.Hour))

	n, err := svc.RunDueDeliveries(context.Background(), time.Now(), 10)
	require.NoError(t, err)
	require.Equal(t, 0, n, "越窗行领取期直接终态化，不经 HTTP")
	require.Equal(t, 0, notifier.count())
	got, err := drepo.GetDelivery(context.Background(), "p1", w.ID, "whd-wh-1-ev-1")
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.DeliveryStatusFailed, got.Status)
}

func TestRedeliverDelivery_ResetsTerminalRow(t *testing.T) {
	repo := newFakeWebhookRepo()
	drepo := newFakeDeliveryRepo()
	notifier := &fakeNotifier{last: status.Errorf(codes.Unavailable, "notify: endpoint status 500")}
	svc := newTestService(t, testConfig(), repo, drepo, notifier, projectsStub{ids: []string{"p1"}})
	ctx := serverPrincipalCtx()
	w := seedWebhook(t, repo, "wh-1")
	dueDelivery(t, drepo, w.ID, "ev-1", 0, time.Now().Add(-time.Minute))
	_, err := svc.RunDueDeliveries(context.Background(), time.Now(), 10)
	require.NoError(t, err)

	// pending 行拒绝重投（已在投递队列中）。
	_, err = svc.RedeliverDelivery(ctx, "p1", w.ID, "whd-wh-1-ev-1")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	// 终态化（下一次领取判定梯度耗尽——attempts 2→3 后 4 > 3）。
	for i := 0; i < 2; i++ {
		_, err = svc.RunDueDeliveries(context.Background(), time.Now().Add(time.Duration(i+1)*time.Hour), 10)
		require.NoError(t, err)
	}
	got, err := drepo.GetDelivery(context.Background(), "p1", w.ID, "whd-wh-1-ev-1")
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.DeliveryStatusFailed, got.Status)

	// 重投：pending + attempts=0 + 立即到期、错误面清零。
	got, err = svc.RedeliverDelivery(ctx, "p1", w.ID, "whd-wh-1-ev-1")
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.DeliveryStatusPending, got.Status)
	require.Equal(t, 0, got.Attempts)
	require.LessOrEqual(t, got.NextAttemptAt.Unix(), time.Now().Unix(), "立即到期")
	require.Equal(t, 0, got.LastResponseCode)
	require.Empty(t, got.LastError)

	// 下轮扫描重新投递（全新一轮梯度）。
	notifier.last = nil
	n, err := svc.RunDueDeliveries(context.Background(), time.Now(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, err = drepo.GetDelivery(context.Background(), "p1", w.ID, "whd-wh-1-ev-1")
	require.NoError(t, err)
	require.Equal(t, domainwebhooks.DeliveryStatusDelivered, got.Status)
}

func TestCreateDeliveries_IdempotentAndEnvelopeShape(t *testing.T) {
	repo := newFakeWebhookRepo()
	drepo := newFakeDeliveryRepo()
	svc := newTestService(t, testConfig(), repo, drepo, &fakeNotifier{}, projectsStub{ids: []string{"p1"}})
	w1 := seedWebhook(t, repo, "wh-1")
	seedWebhook(t, repo, "wh-2")

	ev := &domainevents.Envelope{
		EventID: "ev-1", Event: domainevents.EventDocumentsCreate, ProjectID: "p1",
		DatabaseID: "app", CollectionID: "notes", DocumentID: "doc_x",
		Version: 3, Seq: 42, CreatedAt: time.Now(),
	}
	n, err := svc.CreateDeliveries(context.Background(), CreateDeliveriesCommand{
		ProjectID: "p1", WebhookIDs: []string{w1.ID, "wh-2"}, Envelope: ev, Data: `{"event":"databases.documents.create"}`,
	})
	require.NoError(t, err)
	require.Equal(t, 2, n)

	// 同事件重复投递（at-least-once/停机补投重叠）：幂等吸收。
	n, err = svc.CreateDeliveries(context.Background(), CreateDeliveriesCommand{
		ProjectID: "p1", WebhookIDs: []string{w1.ID, "wh-2"}, Envelope: ev, Data: `{"event":"databases.documents.create"}`,
	})
	require.NoError(t, err)
	require.Equal(t, 0, n, "(webhook_id, event_id) 唯一索引去重")

	// 信封形状（§4.2）：id = 投递行 id、twproject/twseq 扩展属性齐备。
	require.Len(t, drepo.items, 2)
	for _, d := range drepo.items {
		var ce domainfunctions.CloudEvent
		require.NoError(t, json.Unmarshal(d.Payload, &ce))
		require.Equal(t, "1.0", ce.Specversion)
		require.Equal(t, d.ID, ce.ID, "CloudEvent id = 投递行 id")
		require.Equal(t, "projects/p1", ce.Source)
		require.Equal(t, ev.Event, ce.Type)
		require.Equal(t, "p1", ce.TwProject)
		require.Equal(t, int64(42), ce.TwSeq)
		require.NotEmpty(t, ce.Data)
		require.NotEmpty(t, ce.Time)
	}
}

func TestRefreshSubscriptionIndex_Snapshot(t *testing.T) {
	repo := newFakeWebhookRepo()
	svc := newTestService(t, testConfig(), repo, newFakeDeliveryRepo(), &fakeNotifier{}, projectsStub{ids: []string{"p1", "p2"}})
	ctx := serverPrincipalCtx()
	w1, _, err := svc.CreateWebhook(ctx, CreateWebhookCommand{
		ProjectID: "p1", URL: "https://example.com/1",
		Events: []string{"databases.app.collections.notes.documents.create"},
	})
	require.NoError(t, err)
	w2, _, err := svc.CreateWebhook(ctx, CreateWebhookCommand{
		ProjectID: "p2", URL: "https://example.com/2", Events: []string{"payments.*"},
	})
	require.NoError(t, err)

	idx, err := svc.RefreshSubscriptionIndex(context.Background(), 100)
	require.NoError(t, err)
	require.False(t, idx.Empty())
	require.Equal(t, []string{w1.ID}, idx.Match("p1", "app", "notes", "create"))
	require.Nil(t, idx.Match("p1", "app", "notes", "delete"), "op 不命中")
	require.Equal(t, []string{w2.ID}, idx.MatchSystem("p2", "payments.orders.paid"))
	require.Nil(t, idx.MatchSystem("p2", "auth.users.created"), "跨域不命中")
}

func TestPruneOldDeliveries_Retention(t *testing.T) {
	repo := newFakeWebhookRepo()
	drepo := newFakeDeliveryRepo()
	svc := newTestService(t, testConfig(), repo, drepo, &fakeNotifier{}, projectsStub{ids: []string{"p1"}})
	w := seedWebhook(t, repo, "wh-1")
	old := time.Now().Add(-2 * time.Hour) // retention = 1h

	// 旧终态行：应清理。
	oldRow := dueDelivery(t, drepo, w.ID, "ev-old", 0, old)
	oldRow.Status = domainwebhooks.DeliveryStatusDelivered
	drepo.items["p1|"+oldRow.ID] = oldRow
	// 旧 pending 行：不清理（由重试窗终态化后进入下一轮清理口径）。
	pendingRow := dueDelivery(t, drepo, w.ID, "ev-pending", 0, old)
	// 新终态行：不清理。
	newRow := dueDelivery(t, drepo, w.ID, "ev-new", 0, time.Now())
	newRow.Status = domainwebhooks.DeliveryStatusFailed
	drepo.items["p1|"+newRow.ID] = newRow

	n, err := svc.PruneOldDeliveries(context.Background(), 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	_, err = drepo.GetDelivery(context.Background(), "p1", w.ID, oldRow.ID)
	require.Error(t, err, "超保留窗终态行已清理")
	_, err = drepo.GetDelivery(context.Background(), "p1", w.ID, pendingRow.ID)
	require.NoError(t, err, "pending 行不清理")
	_, err = drepo.GetDelivery(context.Background(), "p1", w.ID, newRow.ID)
	require.NoError(t, err, "窗内终态行不清理")
}

func TestDeliveryResponseCode_ParsesNotifierErrors(t *testing.T) {
	// HTTPNotifier 非 2xx 的错误文本（notify.go："notify: endpoint status %d"）。
	require.Equal(t, 500, deliveryResponseCode(status.Errorf(codes.Unavailable, "notify: endpoint status 500")))
	require.Equal(t, 429, deliveryResponseCode(status.Errorf(codes.Unavailable, "notify: endpoint status 429")))
	require.Equal(t, 0, deliveryResponseCode(status.Errorf(codes.DeadlineExceeded, "notify: context deadline exceeded")))
	require.Equal(t, 0, deliveryResponseCode(status.Errorf(codes.Unavailable, "notify: dial refused")))
	require.Equal(t, 0, deliveryResponseCode(nil))
}
