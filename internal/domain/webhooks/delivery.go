package webhooks

import (
	"context"
	"time"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
)

// 投递状态词表（webhook_deliveries.status CHECK 约束同源；failed 即 DLQ）。
const (
	// DeliveryStatusPending 是待投/重投中。
	DeliveryStatusPending = "pending"
	// DeliveryStatusDelivered 是送达（2xx）。
	DeliveryStatusDelivered = "delivered"
	// DeliveryStatusFailed 是终态失败（DLQ；RedeliverDelivery 可重置）。
	DeliveryStatusFailed = "failed"
)

// 重试梯度缺省（D5 拍板：即时首投 → 10s → 1m → 5m → 30m → 2h → 8h ×…
// 约 15 次封顶 24h；末值循环，config functions.webhooks.retry_backoff
// 可覆盖整条序列）。
var DefaultRetryBackoff = []time.Duration{
	10 * time.Second,
	time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	8 * time.Hour,
}

// 缺省终态阈值（config functions.webhooks.max_attempts / retry_window 覆盖）。
const (
	// DefaultMaxAttempts 是最大投递尝试次数（含首投）。
	DefaultMaxAttempts = 15
	// DefaultRetryWindow 是首投起算的终态封顶窗。
	DefaultRetryWindow = 24 * time.Hour
	// MaxAttemptsCeiling 是 max_attempts 的配置硬上限（防配出无界重试）。
	MaxAttemptsCeiling = 100
)

// DeliveryPolicy 是重投调度策略（重试梯度 + 终态阈值；app 层由配置构造，
// 领域层只消费——纯值对象，零 I/O）。
type DeliveryPolicy struct {
	// Backoff 是失败后的重试梯度序列（末值循环；空 = 用缺省）。
	Backoff []time.Duration
	// MaxAttempts 是最大尝试次数（含首投；<=0 用缺省）。
	MaxAttempts int
	// RetryWindow 是首投起算的终态封顶窗（<=0 用缺省）。
	RetryWindow time.Duration
}

// NewDeliveryPolicy 以缺省值收敛非法/未配置字段。
func NewDeliveryPolicy(backoff []time.Duration, maxAttempts int, retryWindow time.Duration) DeliveryPolicy {
	if len(backoff) == 0 {
		backoff = DefaultRetryBackoff
	}
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	if maxAttempts > MaxAttemptsCeiling {
		maxAttempts = MaxAttemptsCeiling
	}
	if retryWindow <= 0 {
		retryWindow = DefaultRetryWindow
	}
	return DeliveryPolicy{Backoff: backoff, MaxAttempts: maxAttempts, RetryWindow: retryWindow}
}

// backoffFor 返回第 attempts 次失败后的等待（下标越界取末值循环——
// "10s→1m→5m→30m→2h→8h→8h…"）。
func (p DeliveryPolicy) backoffFor(attempts int) time.Duration {
	if len(p.Backoff) == 0 {
		p.Backoff = DefaultRetryBackoff
	}
	if attempts < 1 {
		attempts = 1
	}
	idx := attempts - 1
	if idx >= len(p.Backoff) {
		idx = len(p.Backoff) - 1
	}
	return p.Backoff[idx]
}

// AttemptPlan 是一次领取的推进计划（repo 的 CAS 推进依据；与 cron
// CronNextPlan 同构的领域决策输出）。
type AttemptPlan struct {
	// Attempt 报告本次领取是否执行投递（false = 已终态化，仅推进状态）。
	Attempt bool
	// Terminal 报告领取后是否终态（达 MaxAttempts 或越 RetryWindow）——
	// repo 在 CAS 中直接置 failed，不再返回该行。
	Terminal bool
	// Next 是 CAS 推进目标（next_attempt_at）；Terminal 时无意义。
	Next time.Time
}

// PlanAttempt 由领域层给出领取推进决策（bun 适配器在领取循环中回调，
// 与 ClaimDueCron 的 CronNextFunc 同款）：
//   - attempts+1 > MaxAttempts：终态（不再尝试）；
//   - 下一次尝试时刻越出 RetryWindow（created+window）：终态；
//   - 其余：推进 next_attempt_at = now + backoff(attempts+1)（乐观推进——
//     投递中崩溃的行按梯度退避后自愈，不热循环），Attempt=true。
func (p DeliveryPolicy) PlanAttempt(d *Delivery, now time.Time) AttemptPlan {
	next := p.backoffFor(d.Attempts + 1)
	if d.Attempts+1 > p.MaxAttempts || now.Add(next).After(d.CreatedAt.Add(p.RetryWindow)) {
		return AttemptPlan{Attempt: false, Terminal: true}
	}
	return AttemptPlan{Attempt: true, Next: now.Add(next)}
}

// Delivery 是一条投递行（webhook_deliveries 的领域投影；重投事实源 + DLQ）。
type Delivery struct {
	ID        string
	ProjectID string
	WebhookID string
	// EventID 是事件幂等键（outbox event_id；(webhook_id, event_id) 唯一
	// 索引吸收 at-least-once 重复）。
	EventID string
	// EventType 是 CloudEvents type（事件目录名 / databases.documents.{op}）。
	EventType string
	// Seq 是 outbox 全局序（事件产生序；0 = 未知序）。
	Seq int64
	// Payload 是 CloudEvents 1.0 structured mode 信封 JSON（重投期间不变）。
	Payload []byte
	Status  string
	// Attempts 是已执行尝试次数（含首投）。
	Attempts int
	// NextAttemptAt 是下一次尝试时刻（pending 可读；终态 = 最后尝试时刻）。
	NextAttemptAt time.Time
	// LastResponseCode 是最近尝试的响应码（2xx = 送达；0 = 未达 HTTP 层）。
	LastResponseCode int
	// LastError 是最近失败的错误摘要（截断留档；成功后清空）。
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DeliveryClaim 是一次原子领取成功的到期投递（CAS 赢家才出现在返回值中；
// 多实例并发扫描同一到期行只有一条领取拿到它）。Attempts 已含本次（即
// 第 Attempts 次尝试）。
type DeliveryClaim struct {
	Delivery
}

// Notifier 是单次投递尝试端口。**直接复用 functions 域的事件轴投递契约**
// （v4 §4.2/§6：cron / 事件触发 / webhook 共用同一信封同一管道——判定准则
// 「无调用方在等应用层结果」，2xx = 送达；err = 送达失败，计一次 attempt）。
// 生产实现 = infra/functions.HTTPNotifier（守卫 client + per-webhook 双签），
// 不另立第二套 HTTP 适配。
type Notifier = domainfunctions.Notifier

// DeliveryScheduler 是到期投递的调度端口（worker 到期扫描 loop 每分钟
// 驱动）：领取到期投递行（CAS）并执行投递尝试，按梯度推进/终态化；保留
// 窗清理同属投递生命周期维护。实现在 app 层（app/webhooks.Service）。
type DeliveryScheduler interface {
	// RunDueDeliveries 单轮到期扫描：返回本次执行的投递尝试条数。
	RunDueDeliveries(ctx context.Context, now time.Time, budget int) (int, error)
	// PruneOldDeliveries 清理超保留窗的终态投递行：返回删除条数。
	PruneOldDeliveries(ctx context.Context, budget int) (int64, error)
}
