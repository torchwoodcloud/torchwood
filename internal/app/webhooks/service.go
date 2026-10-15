// Package webhooks 是 Webhooks 一等公民子系统的 use-case 聚合
// （functions-v4.md §6，D5）：订阅 CRUD + 密钥轮换（双钥宽限窗）+ 投递
// 调度引擎（ClaimDue CAS → HTTPNotifier → 梯度推进/终态 DLQ）+ outbox
// 事件入口（订阅匹配 → 建投递行）。计量只进 metrics 不进 billing（D5）。
package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"strings"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	domainwebhooks "github.com/torchwoodcloud/torchwood/internal/domain/webhooks"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/secretbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service 是 Webhooks 服务的 use-case 聚合（实现 domainwebhooks.
// DeliveryScheduler——worker 到期扫描 loop 经端口消费）。
type Service struct {
	repo       domainwebhooks.WebhookRepo
	deliveries domainwebhooks.DeliveryRepo
	notifier   domainwebhooks.Notifier
	// projects 是项目目录（可选：nil 时引擎/快照/清理为空操作——最小测试
	// 装配只消费 CRUD 面）。
	projects projects.Repository
	// policy 是重投策略（重试梯度 + 终态阈值；配置覆盖缺省）。
	policy domainwebhooks.DeliveryPolicy
	// secretGrace 是轮换宽限窗（旧钥 v2 签名窗口；配置覆盖缺省）。
	secretGrace time.Duration
	// timeout 是单次投递尝试的 HTTP 预算（秒）。
	timeoutSeconds int64
	// concurrency 是单轮到期扫描的投递并发上限。
	concurrency int
	// retention 是终态投递行保留窗。
	retention time.Duration
	// maxPerProject 是 per-project 订阅数配额（D5）。
	maxPerProject int
	// encKey 是 secretbox 密钥材料（security.encryption_key，回退
	// jwt.secret——与 OAuth client secret / TOTP secret 同一派生入口）。
	encKey string
	// scanCursor 是到期扫描的项目轮转游标（串行；镜像 Functions 聚合同款
	// ——webhook_deliveries 在项目 schema 内）。
	scanCursor appshared.ProjectRotation
	log        *slog.Logger
	now        func() time.Time
}

// NewService 构造聚合（Wire 装配入口；server/worker 同构——server 只消费
// CRUD/浏览面，worker 另经 DeliveryScheduler 端口消费引擎面）。测试侧同包
// 直改 now/log 字段注入时钟与日志（wire 不支持 variadic 装配，无 Option 形态）。
func NewService(repo domainwebhooks.WebhookRepo, deliveries domainwebhooks.DeliveryRepo, notifier domainwebhooks.Notifier, projectRepo projects.Repository, cfg *config.AppConfig) *Service {
	s := &Service{
		repo:       repo,
		deliveries: deliveries,
		notifier:   notifier,
		projects:   projectRepo,
		log:        slog.Default(),
		now:        time.Now,
	}
	if cfg != nil {
		wc := cfg.GetFunctions().GetWebhooks()
		s.policy = domainwebhooks.NewDeliveryPolicy(
			parseBackoff(wc.GetRetryBackoff()),
			int(wc.GetMaxAttempts()),
			parseDuration(wc.GetRetryWindow(), 0),
		)
		s.secretGrace = parseDuration(wc.GetSecretGracePeriod(), domainwebhooks.SecretRotationWindow)
		s.timeoutSeconds = int64(parseDuration(wc.GetDeliveryTimeout(), 10*time.Second) / time.Second)
		s.concurrency = int(wc.GetDeliveryConcurrency())
		s.retention = parseDuration(wc.GetRetention(), 7*24*time.Hour)
		s.maxPerProject = int(wc.GetMaxPerProject())
	}
	if s.secretGrace <= 0 {
		s.secretGrace = domainwebhooks.SecretRotationWindow
	}
	if s.timeoutSeconds <= 0 {
		s.timeoutSeconds = 10
	}
	if s.concurrency <= 0 {
		s.concurrency = 4
	}
	if s.retention <= 0 {
		s.retention = 7 * 24 * time.Hour
	}
	if s.maxPerProject <= 0 {
		s.maxPerProject = 32
	}
	if cfg != nil {
		key, fallback := config.EncryptionSecret(cfg)
		s.encKey = key
		if fallback {
			s.log.Warn("webhooks: security.encryption_key not set; falling back to jwt.secret for secret encryption")
		}
	}
	return s
}

// ——配置解析（空/非法值回落缺省；语义见 config.proto functions.webhooks）——

func parseDuration(s string, fallback time.Duration) time.Duration {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return fallback
	}
	return d
}

// parseBackoff 解析逗号分隔的梯度序列（"10s,1m,5m,30m,2h,8h"）；空/非法
// （含任何非正项）返回 nil = 用领域缺省。
func parseBackoff(s string) []time.Duration {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]time.Duration, 0, len(parts))
	for _, p := range parts {
		d, err := time.ParseDuration(strings.TrimSpace(p))
		if err != nil || d <= 0 {
			return nil
		}
		out = append(out, d)
	}
	return out
}

// ——CRUD——

// CreateWebhookCommand 创建订阅命令。
type CreateWebhookCommand struct {
	// ID 可选（空 = 服务端生成 "wh-<uuid>"）。
	ID        string
	ProjectID string
	URL       string
	Events    []string
	// Status 可选（空 = enabled；仅接受 enabled/paused）。
	Status string
}

// CreateWebhook 创建订阅：https 强制（protovalidate + 应用层双保险）、
// 订阅串逐条 eventmatch 解析校验（fail-closed，拼错立刻 400）、配额守门、
// 密钥服务端生成。返回订阅视图与**一次性明文密钥**（调用方立即持久化；
// 之后任何面只出指纹）。
func (s *Service) CreateWebhook(ctx context.Context, cmd CreateWebhookCommand) (*domainwebhooks.Webhook, string, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, "", err
	}
	if err := domainwebhooks.ValidateURL(cmd.URL); err != nil {
		return nil, "", status.Errorf(codes.InvalidArgument, "url: %v", err)
	}
	events, err := normalizeEvents(cmd.Events)
	if err != nil {
		return nil, "", err
	}
	st := domainwebhooks.StatusEnabled
	if cmd.Status != "" {
		if cmd.Status != domainwebhooks.StatusEnabled && cmd.Status != domainwebhooks.StatusPaused {
			return nil, "", status.Errorf(codes.InvalidArgument, "status must be enabled or paused")
		}
		st = cmd.Status
	}
	if n, err := s.repo.CountWebhooks(ctx, cmd.ProjectID); err == nil && n >= s.maxPerProject {
		return nil, "", status.Errorf(codes.ResourceExhausted,
			"webhook quota exceeded (max %d per project)", s.maxPerProject)
	}
	secret, err := generateSecret()
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "generate webhook secret: %v", err)
	}
	enc, err := secretbox.Encrypt(secret, s.encKey)
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "encrypt webhook secret: %v", err)
	}
	now := s.now()
	w := &domainwebhooks.Webhook{
		ID:                cmd.ID,
		ProjectID:         cmd.ProjectID,
		URL:               cmd.URL,
		Events:            events,
		Status:            st,
		SecretEnc:         enc,
		SecretFingerprint: domainwebhooks.Fingerprint(secret),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if w.ID == "" {
		w.ID = "wh-" + idgen.UUID().String()
	}
	if err := s.repo.CreateWebhook(ctx, w); err != nil {
		return nil, "", err
	}
	return w, secret, nil
}

// GetWebhook 读取订阅（密钥不回显；不存在归一 NotFound）。
func (s *Service) GetWebhook(ctx context.Context, projectID, webhookID string) (*domainwebhooks.Webhook, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	w, err := s.repo.GetWebhook(ctx, projectID, webhookID)
	if err != nil {
		return nil, s.mapRepoErr(err)
	}
	return w, nil
}

// ListWebhooks 项目内全量（分页由 handler 经 crud 投影）。
func (s *Service) ListWebhooks(ctx context.Context, projectID string) ([]domainwebhooks.Webhook, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	return s.repo.ListWebhooks(ctx, projectID)
}

// UpdateWebhookCommand 部分更新命令（nil/未置位 = 不修改；events 以
// EventsSet 表达「设置了（含空集拒绝）」——repeated 无 presence，与函数
// 事件触发器「列表字段整体替换」同语义）。
type UpdateWebhookCommand struct {
	ProjectID string
	WebhookID string
	URL       *string
	Events    []string
	EventsSet bool
	Status    *string
}

// UpdateWebhook 部分更新：url/status/events 可变；密钥走 RotateWebhookSecret。
func (s *Service) UpdateWebhook(ctx context.Context, cmd UpdateWebhookCommand) (*domainwebhooks.Webhook, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	w, err := s.repo.GetWebhook(ctx, cmd.ProjectID, cmd.WebhookID)
	if err != nil {
		return nil, s.mapRepoErr(err)
	}
	if cmd.URL != nil {
		if err := domainwebhooks.ValidateURL(*cmd.URL); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "url: %v", err)
		}
		w.URL = *cmd.URL
	}
	if cmd.EventsSet {
		events, err := normalizeEvents(cmd.Events)
		if err != nil {
			return nil, err
		}
		w.Events = events
	}
	if cmd.Status != nil {
		if *cmd.Status != domainwebhooks.StatusEnabled && *cmd.Status != domainwebhooks.StatusPaused {
			return nil, status.Errorf(codes.InvalidArgument, "status must be enabled or paused")
		}
		w.Status = *cmd.Status
	}
	w.UpdatedAt = s.now()
	if err := s.repo.UpdateWebhook(ctx, w); err != nil {
		return nil, err
	}
	return w, nil
}

// DeleteWebhook 删除订阅（投递行经外键级联清理）。
func (s *Service) DeleteWebhook(ctx context.Context, projectID, webhookID string) error {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if _, err := s.repo.GetWebhook(ctx, projectID, webhookID); err != nil {
		return s.mapRepoErr(err)
	}
	return s.repo.DeleteWebhook(ctx, projectID, webhookID)
}

// RotateWebhookSecret 轮换签名密钥（双钥宽限窗，v4 §4.1 v2 语义同款）：
// 新钥即刻生效（v1 签名），旧钥移入 previous 槽、宽限窗内投递侧同时携带
// v2 旧钥签名——持旧钥的接收方过渡期不断流。返回**一次性新明文**。
func (s *Service) RotateWebhookSecret(ctx context.Context, projectID, webhookID string) (*domainwebhooks.Webhook, string, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, "", err
	}
	w, err := s.repo.GetWebhook(ctx, projectID, webhookID)
	if err != nil {
		return nil, "", s.mapRepoErr(err)
	}
	secret, err := generateSecret()
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "generate webhook secret: %v", err)
	}
	enc, err := secretbox.Encrypt(secret, s.encKey)
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "encrypt webhook secret: %v", err)
	}
	now := s.now()
	w.PreviousSecretEnc = w.SecretEnc
	w.SecretRotatedAt = now
	w.SecretEnc = enc
	w.SecretFingerprint = domainwebhooks.Fingerprint(secret)
	w.UpdatedAt = now
	if err := s.repo.UpdateWebhook(ctx, w); err != nil {
		return nil, "", err
	}
	return w, secret, nil
}

// ——领域辅助——

// normalizeEvents 校验订阅串集：非空、≤ 上限、逐条 eventmatch 解析
// （与函数事件触发器共用同一解析器——存储侧不再有第二套宽松口径）。
func normalizeEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		return nil, status.Error(codes.InvalidArgument, "events is required (at least one subscription pattern)")
	}
	if len(events) > domainwebhooks.MaxEvents {
		return nil, status.Errorf(codes.InvalidArgument, "events exceeds maximum of %d patterns", domainwebhooks.MaxEvents)
	}
	out := make([]string, 0, len(events))
	seen := make(map[string]struct{}, len(events))
	for _, e := range events {
		if len(e) > domainwebhooks.MaxEventBytes {
			return nil, status.Errorf(codes.InvalidArgument, "invalid event %q: exceeds maximum of %d bytes", e, domainwebhooks.MaxEventBytes)
		}
		if _, err := domainfunctions.ParseEventPattern(e); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid event %q: %v", e, err)
		}
		if _, dup := seen[e]; dup {
			continue // 按序去重（与函数事件触发器 normalize 同口径）
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out, nil
}

// generateSecret 生成签名密钥明文（whsec_ + 32 字节 base64url，43 字符
// 不可猜）。
func generateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return domainwebhooks.SecretPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// effectiveSecrets 解密生效密钥对（当前钥 + 宽限窗内的旧钥；窗外的
// previous 不再携带——投递侧 v2 签名通道收敛）。解密失败原样上抛（调用方
// 记 attempt 错误、行留 pending 下轮重试——密钥材料配置错误的可恢复语义）。
func (s *Service) effectiveSecrets(ctx context.Context, w *domainwebhooks.Webhook) (current, previous string, err error) {
	current, err = secretbox.Decrypt(w.SecretEnc, s.encKey)
	if err != nil {
		return "", "", status.Errorf(codes.Internal, "decrypt webhook secret %s: %v", w.ID, err)
	}
	if w.PreviousSecretEnc != "" && domainwebhooks.EffectivePreviousSecret(s.now(), w.SecretRotatedAt, s.secretGrace) {
		previous, err = secretbox.Decrypt(w.PreviousSecretEnc, s.encKey)
		if err != nil {
			// 旧钥解密失败不阻断投递（新钥仍有效）：降级为单钥签名。
			s.log.Warn("webhooks: decrypt previous secret failed; delivering with current only",
				"webhook_id", w.ID, "error", err)
			previous = ""
		}
	}
	return current, previous, nil
}

// mapRepoErr 归一仓储错误（ErrNotFound → NotFound；不泄露跨项目存在性）。
func (s *Service) mapRepoErr(err error) error {
	if err == nil {
		return nil
	}
	if err == domainwebhooks.ErrNotFound {
		return status.Error(codes.NotFound, "webhook not found")
	}
	return err
}
