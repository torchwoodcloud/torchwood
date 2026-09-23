package subscriptions

import (
	"context"
	"time"
)

// PlanRepo 持久化 subscription_plans。方法加入调用方的 uow.Run；实现可从 ctx 读取连接。
type PlanRepo interface {
	Insert(ctx context.Context, plan *Plan) error
	GetByID(ctx context.Context, projectID, planID string) (*Plan, error)
	GetByCode(ctx context.Context, projectID, code string) (*Plan, error)
	// GetByCodeForShare 在事务内 SELECT ... FOR SHARE，防止 Subscribe 途中归档。
	GetByCodeForShare(ctx context.Context, projectID, code string) (*Plan, error)
	GetByIDForShare(ctx context.Context, projectID, planID string) (*Plan, error)
	// List 按时间列分页（ascending=false 倒序 = 历史默认，游标谓词随方向取 < / >）。
	List(ctx context.Context, projectID string, includeArchived bool, limit int, before time.Time, ascending bool) ([]Plan, error)
	Update(ctx context.Context, plan *Plan) error
}

// SubscriptionRepo 持久化 subscriptions。写路径必须在调用方 uow.Run 内
// （与资产 Grant/Mutate / outbox 同一工作单元，总则 10）；实现可从 ctx 读取连接。
// SubscriptionListFilter 是项目订阅列表的结构化过滤（exact 匹配 + created_at
// 闭区间；零值字段 = 不过滤）。Ascending 是时间列排序方向（false = DESC，
// 历史默认），游标谓词随方向取 < / >。
type SubscriptionListFilter struct {
	UserID        string
	Status        Status
	CreatedAfter  time.Time
	CreatedBefore time.Time
	Ascending     bool
}

type SubscriptionRepo interface {
	// Insert 落库；(project_id, idempotency_key) 冲突时返回已存在行与 false。
	Insert(ctx context.Context, sub *Subscription) (existing *Subscription, inserted bool, err error)
	GetByID(ctx context.Context, projectID, id string) (*Subscription, error)
	GetByIDForUpdate(ctx context.Context, projectID, id string) (*Subscription, error)
	GetByIdempotencyKey(ctx context.Context, projectID, key string) (*Subscription, error)
	GetByProviderSubID(ctx context.Context, projectID, provider, providerSubID string) (*Subscription, error)
	GetByProviderSubIDForUpdate(ctx context.Context, projectID, provider, providerSubID string) (*Subscription, error)
	// GetCurrentByUser 返回本人当前非终态订阅；planID 非空时限定计划。
	// 若无非终态，返回最近一条（含终态）便于 GetMySubscription。
	GetCurrentByUser(ctx context.Context, projectID, userID, planID string) (*Subscription, error)
	// ListNonTerminalByUserPlan 列出 (user, plan) 下非终态行（订阅互斥检查）。
	ListNonTerminalByUserPlan(ctx context.Context, projectID, userID, planID string) ([]Subscription, error)
	ListByUser(ctx context.Context, projectID, userID string, limit int, before time.Time) ([]Subscription, error)
	// ListByProject 返回项目订阅（created_at 分页 + 结构化过滤与方向，Server/Console 面）。
	ListByProject(ctx context.Context, projectID string, limit int, before time.Time, f SubscriptionListFilter) ([]Subscription, error)
	Update(ctx context.Context, sub *Subscription, expectStatus Status) error
	// ListDueForBillingInProject 扫描 platform 模式待处理行（FOR UPDATE SKIP LOCKED）：
	// 周期到期、past_due 宽限、cancel_at_period_end。
	ListDueForBillingInProject(ctx context.Context, projectID string, now time.Time, limit int) ([]Subscription, error)
}
