package webhooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainprojects "github.com/torchwoodcloud/torchwood/internal/domain/projects"
	domainwebhooks "github.com/torchwoodcloud/torchwood/internal/domain/webhooks"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/secretbox"
)

// ——测试替身：内存仓储 + 可编程 Notifier——

type fakeWebhookRepo struct {
	mu      sync.Mutex
	items   map[string]*domainwebhooks.Webhook // key: project|id
	overlap int                                // CountWebhooks 的固定返回（配额测试；-1 = 按实际计）
}

func newFakeWebhookRepo() *fakeWebhookRepo {
	return &fakeWebhookRepo{items: map[string]*domainwebhooks.Webhook{}, overlap: -1}
}

func (f *fakeWebhookRepo) key(projectID, id string) string { return projectID + "|" + id }

func (f *fakeWebhookRepo) CreateWebhook(_ context.Context, w *domainwebhooks.Webhook) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(w.ProjectID, w.ID)
	if _, dup := f.items[k]; dup {
		return fmt.Errorf("duplicate key %s", k)
	}
	cp := *w
	f.items[k] = &cp
	return nil
}

func (f *fakeWebhookRepo) GetWebhook(_ context.Context, projectID, webhookID string) (*domainwebhooks.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.items[f.key(projectID, webhookID)]
	if !ok {
		return nil, domainwebhooks.ErrNotFound
	}
	cp := *w
	return &cp, nil
}

func (f *fakeWebhookRepo) ListWebhooks(_ context.Context, projectID string) ([]domainwebhooks.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domainwebhooks.Webhook
	for k, w := range f.items {
		if strings.HasPrefix(k, projectID+"|") {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeWebhookRepo) UpdateWebhook(_ context.Context, w *domainwebhooks.Webhook) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(w.ProjectID, w.ID)
	if _, ok := f.items[k]; !ok {
		return domainwebhooks.ErrNotFound
	}
	cp := *w
	f.items[k] = &cp
	return nil
}

func (f *fakeWebhookRepo) DeleteWebhook(_ context.Context, projectID, webhookID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(projectID, webhookID)
	if _, ok := f.items[k]; !ok {
		return domainwebhooks.ErrNotFound
	}
	delete(f.items, k)
	return nil
}

func (f *fakeWebhookRepo) CountWebhooks(_ context.Context, projectID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.overlap >= 0 {
		return f.overlap, nil
	}
	n := 0
	for k := range f.items {
		if strings.HasPrefix(k, projectID+"|") {
			n++
		}
	}
	return n, nil
}

func (f *fakeWebhookRepo) ListEnabledWebhooks(ctx context.Context, projectID string) ([]domainwebhooks.Webhook, error) {
	all, err := f.ListWebhooks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]domainwebhooks.Webhook, 0, len(all))
	for _, w := range all {
		if w.Enabled() {
			out = append(out, w)
		}
	}
	return out, nil
}

type fakeDeliveryRepo struct {
	mu     sync.Mutex
	items  map[string]*domainwebhooks.Delivery // key: project|id
	seen   map[string]struct{}                 // webhook|event 幂等键
	pruned int64
}

func newFakeDeliveryRepo() *fakeDeliveryRepo {
	return &fakeDeliveryRepo{items: map[string]*domainwebhooks.Delivery{}, seen: map[string]struct{}{}}
}

func (f *fakeDeliveryRepo) key(projectID, id string) string { return projectID + "|" + id }

func (f *fakeDeliveryRepo) CreateDeliveries(_ context.Context, items []*domainwebhooks.Delivery) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inserted := 0
	for _, d := range items {
		idem := d.WebhookID + "|" + d.EventID
		if _, dup := f.seen[idem]; dup {
			continue
		}
		f.seen[idem] = struct{}{}
		cp := *d
		f.items[f.key(d.ProjectID, d.ID)] = &cp
		inserted++
	}
	return inserted, nil
}

func (f *fakeDeliveryRepo) GetDelivery(_ context.Context, projectID, webhookID, deliveryID string) (*domainwebhooks.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.items[f.key(projectID, deliveryID)]
	if !ok || d.WebhookID != webhookID {
		return nil, domainwebhooks.ErrNotFound
	}
	cp := *d
	return &cp, nil
}

func (f *fakeDeliveryRepo) ListDeliveries(_ context.Context, projectID, webhookID, status string, limit, offset int) (domainwebhooks.DeliveryListPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []domainwebhooks.Delivery
	for _, d := range f.items {
		if d.ProjectID != projectID || d.WebhookID != webhookID {
			continue
		}
		if status != "" && d.Status != status {
			continue
		}
		all = append(all, *d)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	total := len(all)
	if offset < len(all) {
		all = all[offset:]
	} else {
		all = nil
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return domainwebhooks.DeliveryListPage{Items: all, Total: total}, nil
}

// ClaimDue 简化版领取（单线程测试语义）：过滤到期行 → plan 终态判 → CAS
// 恒赢（推进 attempts/next_attempt_at）。
func (f *fakeDeliveryRepo) ClaimDue(_ context.Context, projectID string, now time.Time, limit int, plan func(d *domainwebhooks.Delivery) domainwebhooks.AttemptPlan) ([]domainwebhooks.DeliveryClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var due []*domainwebhooks.Delivery
	for _, d := range f.items {
		if d.ProjectID == projectID && d.Status == domainwebhooks.DeliveryStatusPending && !d.NextAttemptAt.After(now) {
			cp := *d
			due = append(due, &cp)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].NextAttemptAt.Before(due[j].NextAttemptAt) })
	if len(due) > limit {
		due = due[:limit]
	}
	var claims []domainwebhooks.DeliveryClaim
	for _, d := range due {
		p := plan(d)
		if p.Terminal {
			d.Status = domainwebhooks.DeliveryStatusFailed
			f.items[f.key(projectID, d.ID)] = d
			continue
		}
		d.Attempts++
		d.NextAttemptAt = p.Next
		f.items[f.key(projectID, d.ID)] = d
		claims = append(claims, domainwebhooks.DeliveryClaim{Delivery: *d})
	}
	return claims, nil
}

func (f *fakeDeliveryRepo) MarkDelivered(_ context.Context, projectID, deliveryID string, _ time.Time, code int) error {
	return f.outcome(projectID, deliveryID, domainwebhooks.DeliveryStatusDelivered, code, "")
}

func (f *fakeDeliveryRepo) MarkFailed(_ context.Context, projectID, deliveryID string, _ time.Time, code int, lastErr string) error {
	return f.outcome(projectID, deliveryID, domainwebhooks.DeliveryStatusFailed, code, lastErr)
}

func (f *fakeDeliveryRepo) outcome(projectID, deliveryID, st string, code int, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.items[f.key(projectID, deliveryID)]
	if !ok || d.Status != domainwebhooks.DeliveryStatusPending {
		return nil
	}
	d.Status = st
	d.LastResponseCode = code
	d.LastError = lastErr
	return nil
}

func (f *fakeDeliveryRepo) RecordAttemptError(_ context.Context, projectID, deliveryID string, _ time.Time, code int, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.items[f.key(projectID, deliveryID)]
	if !ok {
		return nil
	}
	d.LastResponseCode = code
	d.LastError = lastErr
	return nil
}

func (f *fakeDeliveryRepo) ResetForRedeliver(_ context.Context, projectID, webhookID, deliveryID string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.items[f.key(projectID, deliveryID)]
	if !ok || d.WebhookID != webhookID {
		return false, domainwebhooks.ErrNotFound
	}
	if d.Status != domainwebhooks.DeliveryStatusDelivered && d.Status != domainwebhooks.DeliveryStatusFailed {
		return false, nil
	}
	d.Status = domainwebhooks.DeliveryStatusPending
	d.Attempts = 0
	d.NextAttemptAt = now
	d.LastResponseCode = 0
	d.LastError = ""
	return true, nil
}

func (f *fakeDeliveryRepo) PruneTerminal(_ context.Context, projectID string, cutoff time.Time, _ int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, d := range f.items {
		if d.ProjectID != projectID {
			continue
		}
		if d.Status != domainwebhooks.DeliveryStatusDelivered && d.Status != domainwebhooks.DeliveryStatusFailed {
			continue
		}
		if d.CreatedAt.Before(cutoff) {
			delete(f.items, k)
			n++
		}
	}
	f.pruned += n
	return n, nil
}

// fakeNotifier 记录投递载荷并按脚本返回错误。
type fakeNotifier struct {
	mu    sync.Mutex
	calls []domainfunctions.Notification
	errs  []error // 逐次消费；耗尽后用 last
	last  error
}

func (f *fakeNotifier) Notify(_ context.Context, n domainfunctions.Notification) (domainfunctions.NotificationResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, n)
	idx := len(f.calls) - 1
	f.mu.Unlock()
	if idx < len(f.errs) {
		return domainfunctions.NotificationResult{}, f.errs[idx]
	}
	return domainfunctions.NotificationResult{}, f.last
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeNotifier) callAt(i int) domainfunctions.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// projectsStub 是项目目录替身（快照/引擎/清理扫描遍历面；固定 active 项目）。
type projectsStub struct{ ids []string }

func (p projectsStub) CreateProject(context.Context, *domainprojects.Project) error { return nil }

func (p projectsStub) GetProject(_ context.Context, id string) (*domainprojects.Project, error) {
	for _, id2 := range p.ids {
		if id2 == id {
			return &domainprojects.Project{ID: id, Status: "active"}, nil
		}
	}
	return nil, errors.New("not found")
}

func (p projectsStub) GetProjectByName(context.Context, string) (*domainprojects.Project, error) {
	return nil, nil
}

func (p projectsStub) ListProjects(context.Context) ([]domainprojects.Project, error) {
	out := make([]domainprojects.Project, len(p.ids))
	for i, id := range p.ids {
		out[i] = domainprojects.Project{ID: id, Status: "active"}
	}
	return out, nil
}

func (p projectsStub) UpdateProject(context.Context, *domainprojects.Project) error { return nil }
func (p projectsStub) DeleteProject(context.Context, string) error                  { return nil }
func (p projectsStub) DeleteProjectControlPlaneRows(context.Context, string) error  { return nil }

// ——构造辅助——

func testConfig() *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{Jwt: &config.Security_Jwt{Secret: "test-jwt-secret-0123456789"}},
		Functions: &config.Functions{
			Webhooks: &config.Functions_Webhooks{
				RetryBackoff:        "10ms,20ms",
				MaxAttempts:         3,
				RetryWindow:         "1h",
				DeliveryConcurrency: 2,
				DeliveryTimeout:     "5s",
				Retention:           "1h",
				MaxPerProject:       2,
			},
		},
	}
}

func newTestService(t *testing.T, cfg *config.AppConfig, repo *fakeWebhookRepo, deliveries *fakeDeliveryRepo, notifier *fakeNotifier, projs projectsStub) *Service {
	t.Helper()
	s := NewService(repo, deliveries, notifier, projs, cfg)
	// 测试时钟与 discards 日志（同包直改字段——wire 不装配 Option）。
	now := time.Now
	s.now = func() time.Time { return now() }
	s.log = slog.New(slog.DiscardHandler)
	return s
}

// seedWebhook 直接落一个已加密密钥的订阅（绕过 CRUD 的 principal 守卫）。
func seedWebhook(t *testing.T, repo *fakeWebhookRepo, id string) *domainwebhooks.Webhook {
	t.Helper()
	secret, err := generateSecret()
	require.NoError(t, err)
	enc, err := secretbox.Encrypt(secret, "test-jwt-secret-0123456789")
	require.NoError(t, err)
	w := &domainwebhooks.Webhook{
		ID:                id,
		ProjectID:         "p1",
		URL:               "https://example.com/hook",
		Events:            []string{"databases.app.collections.notes.documents.create"},
		Status:            domainwebhooks.StatusEnabled,
		SecretEnc:         enc,
		SecretFingerprint: domainwebhooks.Fingerprint(secret),
	}
	require.NoError(t, repo.CreateWebhook(context.Background(), w))
	return w
}
