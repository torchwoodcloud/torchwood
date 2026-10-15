package webhooks

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound 是「不存在」的领域错误（app 层归一 NotFound；不泄露跨项目
// 存在性——所有查询按 project 收窄，未命中一律本错误）。
var ErrNotFound = errors.New("webhook: not found")

// WebhookRepo 持久化订阅实体（webhook_subscriptions，迁移 000027）。方法
// 均按 project 收窄——跨项目/跨库同名合法是本项目物理模型不变量。密钥
// 以密文形态过端口（SecretEnc/PreviousSecretEnc），明文只在 app 层短暂
// 存在于创建/轮换响应与投递载荷构造。
type WebhookRepo interface {
	// CreateWebhook 插入订阅（幂等冲突 = id 已存在，实现侧原样上抛约束错误）。
	CreateWebhook(ctx context.Context, w *Webhook) error
	// GetWebhook 按 (project, id) 读取；未命中返回 ErrNotFound。
	GetWebhook(ctx context.Context, projectID, webhookID string) (*Webhook, error)
	// ListWebhooks 项目内全量（管理面列表 + 快照扫描分页由调用方做；
	// 按 created_at DESC）。
	ListWebhooks(ctx context.Context, projectID string) ([]Webhook, error)
	// UpdateWebhook 全量更新可变列（url/events/status/updated_at + 密钥
	// 四列——轮换路径复用）；id/project_id/created_at 不可变。实现侧必须
	// 遵守 bun 更新写规范（显式列白名单，禁止裸全模型覆盖）。
	UpdateWebhook(ctx context.Context, w *Webhook) error
	// DeleteWebhook 按 (project, id) 删除（投递行经外键级联清理）。
	DeleteWebhook(ctx context.Context, projectID, webhookID string) error
	// CountWebhooks 项目内订阅数（per-project 配额，D5）。
	CountWebhooks(ctx context.Context, projectID string) (int, error)
	// ListEnabledWebhooks 项目内启用的订阅（worker 订阅匹配器周期快照
	// 扫描；走 webhook_subscriptions_enabled_scan partial 索引；paused 不
	// 投递——与 cron 领取 enabled=true 同语义）。
	ListEnabledWebhooks(ctx context.Context, projectID string) ([]Webhook, error)
}

// DeliveryListPage 是投递行分页结果（offset 分页 + 总数，对齐
// ListDeploymentsPaged 先例）。
type DeliveryListPage struct {
	Items []Delivery
	Total int
}

// DeliveryRepo 持久化投递行（webhook_deliveries，迁移 000027）。
type DeliveryRepo interface {
	// CreateDeliveries 批量插入投递行：同批行共享 (project, webhook 集)，
	// 逐行独立插入；(webhook_id, event_id) 唯一索引冲突静默跳过
	//（at-least-once 去重吸收）。返回实际插入条数。
	CreateDeliveries(ctx context.Context, items []*Delivery) (int, error)
	// GetDelivery 按 (project, webhook, id) 读取；未命中返回 ErrNotFound。
	GetDelivery(ctx context.Context, projectID, webhookID, deliveryID string) (*Delivery, error)
	// ListDeliveries 按 (project, webhook) 分页（created_at DESC + 总数；
	// status 非空时按状态过滤——DLQ 浏览面）。
	ListDeliveries(ctx context.Context, projectID, webhookID, status string, limit, offset int) (DeliveryListPage, error)
	// ClaimDue 单项目内原子领取到期投递：候选扫描（无锁，命中
	// webhook_deliveries_due partial 索引）后逐条 CAS 推进——
	// `UPDATE attempts/next_attempt_at WHERE id AND attempts=旧 AND
	// next_attempt_at=旧` 判 rows=1，多实例并发只有赢家（**先 CAS 后投递**
	// 红线：投递中崩溃的行已按梯度退避，不热循环；与 ClaimDueCron 同款）。
	// 终态判定（达 MaxAttempts / 越 RetryWindow）经 plan 回调由领域层给出，
	// CAS 直接置 failed 且不返回该行。
	ClaimDue(ctx context.Context, projectID string, now time.Time, limit int, plan func(d *Delivery) AttemptPlan) ([]DeliveryClaim, error)
	// MarkDelivered 投递成功收口：status=delivered + 响应码 + 清错误
	//（仅 pending 行可收口——与领取/重置的 CAS 竞争由 status 条件化解）。
	MarkDelivered(ctx context.Context, projectID, deliveryID string, now time.Time, responseCode int) error
	// MarkFailed 终态失败收口（梯度耗尽/越窗；仅 pending 行可收口）。
	MarkFailed(ctx context.Context, projectID, deliveryID string, now time.Time, responseCode int, lastErr string) error
	// RecordAttemptError 记录一次非终态失败的错误摘要（响应码 + 截断
	// 文本；next_attempt_at 已由领取推进，此处只补审计面）。
	RecordAttemptError(ctx context.Context, projectID, deliveryID string, now time.Time, responseCode int, lastErr string) error
	// ResetForRedeliver 手动重投重置：终态行（delivered/failed）→
	// pending、attempts=0、next_attempt_at=now、清错误。pending 行拒绝
	//（已在投递队列中）。返回是否重置成功。
	ResetForRedeliver(ctx context.Context, projectID, webhookID, deliveryID string, now time.Time) (bool, error)
	// PruneTerminal 清理终态行（delivered/failed，created_at 早于 cutoff；
	// pending 行由重试窗终态化后自然进入下一轮清理口径）。返回删除条数。
	PruneTerminal(ctx context.Context, projectID string, cutoff time.Time, limit int) (int64, error)
}
