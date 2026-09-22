package payments

import (
	"context"
	"time"
)

// OrderListFilter 是项目订单列表的结构化过滤（exact 匹配 + created_at 闭区间；
// 零值字段 = 不过滤）。
type OrderListFilter struct {
	UserID        string
	Status        OrderStatus
	CreatedAfter  time.Time
	CreatedBefore time.Time
}

// OrderRepo 持久化 payment_orders。写路径加入调用方的 uow.Run
// （回调处理与履约 / outbox 同一工作单元，设计 §1.3、总则 10）；
// 实现可从 ctx 读取连接。
type OrderRepo interface {
	// Insert 落库新订单；(project_id, idempotency_key) 冲突时返回已存在的
	// 原单与 false（幂等锚点一：建单幂等键重复返回原单，不新建）。
	Insert(ctx context.Context, order *Order) (existing *Order, inserted bool, err error)
	// GetByID 按 id 取单（project 隔离过滤）。
	GetByID(ctx context.Context, projectID, orderID string) (*Order, error)
	// GetByIDForUpdate 在事务内 SELECT ... FOR UPDATE 锁单（回调 / 退款路径）。
	GetByIDForUpdate(ctx context.Context, projectID, orderID string) (*Order, error)
	// GetByProviderRef 按渠道会话 / 渠道支付单定位订单（回调路径）。
	GetByProviderRef(ctx context.Context, projectID, provider, providerSessionID, providerOrderID string) (*Order, error)
	// Update 写回订单（状态翻转、渠道引用回填）；带状态前置条件防并发覆写。
	Update(ctx context.Context, order *Order, expectStatus OrderStatus) error
	// ListByUser 返回本人订单（created_at DESC 分页）。
	ListByUser(ctx context.Context, projectID, userID string, limit int, before time.Time) ([]Order, error)
	// ListByProject 返回项目订单（created_at DESC 分页 + 结构化过滤，Server/Console 面）。
	ListByProject(ctx context.Context, projectID string, limit int, before time.Time, f OrderListFilter) ([]Order, error)
	// CloseExpiredInProject 把指定项目 created/paying 且超时的订单翻 closed。
	CloseExpiredInProject(ctx context.Context, projectID string, now time.Time, limit int) (int64, error)
}

// CallbackEventRepo 登记渠道回调事件：幂等锚点二 (provider, provider_event_id)
// 唯一（设计 §1.3）。插入与订单翻转同一事务。
type CallbackEventRepo interface {
	// InsertIfAbsent 落一行回调事件；(provider, provider_event_id) 已存在时
	// 返回 false——调用方据此幂等短路（重放返回 200，不重入状态机）。
	// projectID / orderID 为定位结果（未命中订单时均可为空）。
	InsertIfAbsent(ctx context.Context, event *CallbackEvent, projectID, orderID string) (inserted bool, err error)
}

// FulfillmentStatus 是履约记录状态。
type FulfillmentStatus string

const (
	FulfillmentPending FulfillmentStatus = "pending"
	FulfillmentDone    FulfillmentStatus = "done"
	FulfillmentFailed  FulfillmentStatus = "failed"
	// FulfillmentReverseFailed 表示退款时 Fulfiller.Reverse 失败的补偿欠账：
	// 渠道已退款但资产未回收，行即人工排查/补回收清单（status 列为 TEXT，
	// 新值无需迁移）。
	FulfillmentReverseFailed FulfillmentStatus = "reverse_failed"
)

// Fulfillment 是订单 paid 同事务内落下的履约记录（设计 §1.5）。
type Fulfillment struct {
	ID          string
	OrderID     string
	ProjectID   string
	PurposeKind PurposeKind
	Ref         string // 幂等锚点三：履约 ref（"order:{order_id}"；PR2 起指向 ledger entry）
	Status      FulfillmentStatus
	Detail      map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// FulfillmentRepo 持久化 payment_fulfillments。
type FulfillmentRepo interface {
	// InsertPending 落 pending 履约行；(order_id, purpose_kind) 已存在时
	// 返回已存在的行与 false（一单一类履约恰好一次）。
	InsertPending(ctx context.Context, f *Fulfillment) (existing *Fulfillment, inserted bool, err error)
	// MarkDone 把履约行置 done 并回填 ref。
	MarkDone(ctx context.Context, projectID, fulfillmentID, ref string, detail map[string]any) error
	// MarkFailed 把履约行置 failed（事务整体回滚路径一般用不到，
	// 留给人工排查标记）。
	MarkFailed(ctx context.Context, projectID, fulfillmentID, reason string) error
	// MarkReverseFailed 把订单的履约行置 reverse_failed 并把原因合并进
	// detail（退款时 Fulfiller.Reverse 失败的可查询补偿记录，含 order_id
	// 与原因；订单无履约行时插入一条 reverse_failed 行）。与订单翻转同一
	// 工作单元执行；失败不阻塞退款翻单（调用方仅记日志 + 指标）。
	// 管理员手动补回收（reverse 重试）入口暂缺，本行即人工欠账清单。
	MarkReverseFailed(ctx context.Context, order *Order, reason string) error
	// GetByOrder 按订单取履约行。
	GetByOrder(ctx context.Context, projectID, orderID string) (*Fulfillment, error)
}

// Fulfiller 是订单 paid 后的实际发放端口（设计 §1.5，hook）：
// topup / item_purchase 在 PR2 接入资产系统 Grant（与订单翻转同一事务），
// subscription 在 PR3 接入。PR1 的默认实现不发放、只返回占位 ref。
type Fulfiller interface {
	// Fulfill 在订单已翻 paid、履约行已插入后调用；返回履约 ref。
	// 返回错误则整段事务回滚（订单保持 paying，渠道重推后再试）。
	Fulfill(ctx context.Context, order *Order) (ref string, err error)
	// Reverse 回收已发放资产：按 fulfill:{orderID} 找到 Grant，再 Consume 同数量
	//（幂等 reverse:{orderID}）；无持有则应记录错误但不阻塞退款翻转。
	Reverse(ctx context.Context, order *Order) error
}
