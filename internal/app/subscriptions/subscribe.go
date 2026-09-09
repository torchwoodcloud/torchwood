package subscriptions

import (
	"context"
	"strconv"
	"strings"
	"time"

	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
	apppayments "github.com/torchwoodcloud/torchwood/internal/app/payments"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SubscribeCommand 是终端用户订阅入参。
type SubscribeCommand struct {
	PlanCode         string
	Mode             domainsubs.Mode
	IdempotencyKey   string
	BillingAssetCode string
	SuccessURL       string
	CancelURL        string
}

// SubscribeResult 是 Subscribe 结果。
type SubscribeResult struct {
	Subscription     *domainsubs.Subscription
	Plan             *domainsubs.Plan
	PaymentURL       string
	OrderID          string
	IdempotentReplay bool
}

// Subscribe 创建订阅（Client 面）。
func (s *Subscriptions) Subscribe(ctx context.Context, cmd SubscribeCommand) (*SubscribeResult, error) {
	projectID, userID, err := endUser(ctx)
	if err != nil {
		return nil, err
	}
	if !cmd.Mode.IsValid() {
		return nil, status.Errorf(codes.InvalidArgument, "mode must be hosted or platform")
	}
	key, err := validateIdempotency(cmd.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	code, err := validateCode(cmd.PlanCode)
	if err != nil {
		return nil, err
	}

	plan, err := s.plans.GetByCode(ctx, projectID, code)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, status.Error(codes.NotFound, "plan not found")
	}
	if plan.Status != domainsubs.PlanStatusActive {
		return nil, status.Error(codes.FailedPrecondition, domainsubs.ErrPlanArchived.Error())
	}

	now := s.ts()
	periodStart := now
	periodEnd, err := domainsubs.NextPeriodEnd(periodStart, plan.Interval, plan.IntervalDays)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	initial := domainsubs.StatusActive
	if plan.TrialDays > 0 {
		initial = domainsubs.StatusTrialing
		periodEnd = periodStart.Add(time.Duration(plan.TrialDays) * 24 * time.Hour)
	}

	sub := &domainsubs.Subscription{
		ID:                 newID(),
		ProjectID:          projectID,
		UserID:             userID,
		PlanID:             plan.ID,
		Mode:               cmd.Mode,
		Status:             initial,
		CurrentPeriodStart: periodStart,
		CurrentPeriodEnd:   periodEnd,
		Benefits:           plan.Benefits,
		IdempotencyKey:     key,
		BillingAssetCode:   strings.TrimSpace(cmd.BillingAssetCode),
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if cmd.Mode == domainsubs.ModeHosted {
		sub.Provider = domainpayments.ProviderStripe
		if initial != domainsubs.StatusTrialing {
			sub.Status = domainsubs.StatusTrialing // 等 webhook 确认后转 active
		}
	}

	var result SubscribeResult
	result.Plan = plan
	err = s.db.Run(ctx, func(txCtx context.Context) error {
		existing, inserted, err := s.subs.Insert(txCtx, sub)
		if err != nil {
			return err
		}
		if !inserted {
			// 幂等重放命中终态订阅（canceled/expired，E-P2-5）：原合同已死，
			// 作为成功重放返回会让客户端拿到一张无效订阅；返回明确错误引导
			// 换新幂等键重新订阅（终态行不占 live unique 位，新键可正常订阅）。
			if existing.Status.IsTerminal() {
				return domainsubs.ErrReplayTerminalSubscription
			}
			result.Subscription = existing
			result.IdempotentReplay = true
			return nil
		}
		live, err := s.subs.ListNonTerminalByUserPlan(txCtx, projectID, userID, plan.ID)
		if err != nil {
			return err
		}
		for i := range live {
			if live[i].ID != sub.ID {
				return domainsubs.ErrAlreadySubscribed
			}
		}

		switch cmd.Mode {
		case domainsubs.ModeHosted:
			return s.startHostedWithURLs(txCtx, sub, plan, &result, cmd.SuccessURL, cmd.CancelURL)
		case domainsubs.ModePlatform:
			return s.startPlatform(txCtx, sub, plan, &result, now)
		}
		return domainsubs.ErrInvalidMode
	})
	if err != nil {
		return nil, mapError(err)
	}
	if result.Subscription == nil {
		result.Subscription = sub
	}
	return &result, nil
}

func (s *Subscriptions) startHostedWithURLs(ctx context.Context, sub *domainsubs.Subscription, plan *domainsubs.Plan, result *SubscribeResult, reqSuccess, reqCancel string) error {
	if s.hosted == nil {
		return domainsubs.ErrNotConfigured
	}
	if plan.ProviderOverrides.StripePriceID == "" {
		return status.Error(codes.FailedPrecondition, "plan is missing stripe_price_id for hosted mode")
	}
	success, cancel := s.resolveCheckoutURLs(reqSuccess, reqCancel)
	sess, err := s.hosted.CreateCheckout(ctx, domainsubs.HostedCheckoutInput{
		SubscriptionID: sub.ID,
		ProjectID:      sub.ProjectID,
		PriceID:        plan.ProviderOverrides.StripePriceID,
		SuccessURL:     success,
		CancelURL:      cancel,
		IdempotencyKey: "sub:" + sub.ID,
	})
	if err != nil {
		return err
	}
	result.PaymentURL = sess.PaymentURL
	return s.upsertIndex(ctx, sub.Provider, domainpayments.IndexKindSubscription, sub.ID, sub.ProjectID)
}

func (s *Subscriptions) startPlatform(ctx context.Context, sub *domainsubs.Subscription, plan *domainsubs.Plan, result *SubscribeResult, now time.Time) error {
	// 试用期：发放 benefits，不扣款。
	if sub.Status == domainsubs.StatusTrialing {
		if err := s.fulfillBenefits(ctx, sub, sub.CurrentPeriodEnd); err != nil {
			return err
		}
		return s.publish(ctx, sub, domainsubs.EventActivated, now)
	}
	if plan.Amount == 0 {
		if err := s.fulfillBenefits(ctx, sub, sub.CurrentPeriodEnd); err != nil {
			return err
		}
		return s.publish(ctx, sub, domainsubs.EventActivated, now)
	}
	if sub.BillingAssetCode != "" {
		ctx = withSystemPrincipal(ctx, sub.ProjectID)
		if _, err := s.assets.Consume(ctx, appassets.ConsumeCommand{
			OwnerType:      domainassets.OwnerTypeUser,
			OwnerID:        sub.UserID,
			DefCode:        sub.BillingAssetCode,
			Quantity:       plan.Amount,
			IdempotencyKey: "sub:" + sub.ID + ":activate",
			RefType:        "subscription",
			RefID:          sub.ID,
		}); err != nil {
			return err
		}
		if err := s.fulfillBenefits(ctx, sub, sub.CurrentPeriodEnd); err != nil {
			return err
		}
		return s.publish(ctx, sub, domainsubs.EventActivated, now)
	}
	// 生成支付订单：订阅保持 trialing，paid 履约后转 active。
	from := sub.Status
	sub.Status = domainsubs.StatusTrialing
	sub.UpdatedAt = now
	if from != domainsubs.StatusTrialing {
		if err := s.subs.Update(ctx, sub, from); err != nil {
			return err
		}
	}
	order, url, err := s.createBillingOrder(ctx, sub, plan, "activate")
	if err != nil {
		return err
	}
	result.OrderID = order.ID
	result.PaymentURL = url
	return nil
}

//nolint:unused
func (s *Subscriptions) checkoutURLs() (success, cancel string) {
	return s.resolveCheckoutURLs("", "")
}

func (s *Subscriptions) resolveCheckoutURLs(reqSuccess, reqCancel string) (string, string) {
	base := "https://localhost"
	if s.cfg != nil && s.cfg.GetServer().GetHttp().GetPublicUrl() != "" {
		base = strings.TrimRight(s.cfg.GetServer().GetHttp().GetPublicUrl(), "/")
	}
	success := strings.TrimSpace(reqSuccess)
	if success == "" {
		success = base + "/?checkout=success&session_id={CHECKOUT_SESSION_ID}"
	}
	cancel := strings.TrimSpace(reqCancel)
	if cancel == "" {
		cancel = base + "/?checkout=cancel&session_id={CHECKOUT_SESSION_ID}"
	}
	return success, cancel
}

// maxBillingOrderAttempts 是同 cycle 建单的总尝试次数：原键 + `#2`…`#5`
// 共 5 把键。全部命中终态单后放弃本轮下单（E-P2-2）。
const maxBillingOrderAttempts = 5

// errBillingOrderExhausted：同 cycle 连续换键重建订单均命中终态单
// （closed/failed/refunded）。billOrPastDue 据此哨兵转入 markPastDue，
// 消灭「tryCharge 假成功、订阅永不推进也永不 past_due」的空转卡死。
var errBillingOrderExhausted = status.Error(codes.FailedPrecondition,
	"subscriptions: billing order rebuild attempts exhausted for this cycle")

// createBillingOrder 生成订阅扣款订单（Stripe，两段式，对齐 payments.CreateOrder）：
//
//  1. 经 payments.InsertCreatedOrder 落单 + payment_session index，在**独立事务**
//     提交——本函数可能被 Subscribe / processDue 的外层订阅事务调用，渠道下单
//     （外部 HTTP）不得拖长外层事务；index 行必须在回调可到达之前持久可见
//     （设计 §9.2：在调 CreatePayment 之前 COMMIT）。
//  2. CreatePayment 在事务之外。
//  3. 回填渠道引用并翻 paying + cs_/pi_ index upsert，第二个独立事务。
//
// CreatePayment 失败时订单保持 created：外层按各自语义处理（Subscribe 回滚订阅、
// processDue 记日志下轮重试），订单由到期 worker 关单；幂等键 sub:<id>:<cycle>
// （及重建用的 `#N` 后缀键）保证重放不重复建单。
//
// 幂等命中检查状态（E-P2-2）：仍为 created 的原单继续走下单流程（对齐
// payments.CreateOrder）；命中终态单（closed/failed/refunded）说明上一轮订单
// 已死（如 CreatePayment 失败后被 CloseExpiredOrders 关单），同键重放只会永久
// 拿回死单——换 `原键#N`（N 从 2 递增）重建，新键同样可被并发的两轮扫描幂等
// 命中（N 递增探测直到 inserted）；连续 maxBillingOrderAttempts 次全为终态则
// 返回 errBillingOrderExhausted 交由 billOrPastDue 走 markPastDue。
func (s *Subscriptions) createBillingOrder(ctx context.Context, sub *domainsubs.Subscription, plan *domainsubs.Plan, cycle string) (*domainpayments.Order, string, error) {
	if s.orders == nil || s.providers == nil {
		return nil, "", status.Error(codes.FailedPrecondition, "payments are required to bill this subscription")
	}
	providerName := domainpayments.ProviderStripe
	provider, err := s.providers.Get(providerName)
	if err != nil {
		return nil, "", status.Errorf(codes.FailedPrecondition, "unsupported provider %q", providerName)
	}
	now := s.ts()
	baseKey := "sub:" + sub.ID + ":" + cycle
	var order *domainpayments.Order
	for attempt := 0; attempt < maxBillingOrderAttempts; attempt++ {
		key := baseKey
		if attempt > 0 {
			key = baseKey + "#" + strconv.Itoa(attempt+1)
		}
		candidate, err := apppayments.NewCreatedOrder(apppayments.CreatedOrderSpec{
			ProjectID:      sub.ProjectID,
			UserID:         sub.UserID,
			Provider:       provider.Name(),
			Amount:         plan.Amount,
			Currency:       plan.Currency,
			PurposeKind:    domainpayments.PurposeSubscription,
			Purpose:        purposeJSON(sub.ID, plan.Code, cycle),
			IdempotencyKey: key,
			Now:            now,
		})
		if err != nil {
			return nil, "", err
		}
		var existing *domainpayments.Order
		var inserted bool
		if err := s.db.RunInNewTx(ctx, func(txCtx context.Context) error {
			var err error
			existing, inserted, err = apppayments.InsertCreatedOrder(txCtx, s.orders, s.index, candidate)
			return err
		}); err != nil {
			return nil, "", err
		}
		if inserted {
			order = candidate
			break
		}
		if existing.Status == domainpayments.OrderStatusCreated {
			order = existing // created 原单：继续走下单流程（渠道侧以 order:<id> 幂等）。
			break
		}
		if existing.Status.IsTerminal() {
			continue // 终态死单：换 `#N` 键重建。
		}
		// paying/paid/refunding 等渠道侧未决或已成功的单：原样返回等待回调推进，
		// 绝不重建（防止同一周期重复扣款）。
		return existing, "", nil
	}
	if order == nil {
		return nil, "", errBillingOrderExhausted
	}
	successURL, cancelURL := s.resolveCheckoutURLs("", "")
	session, err := provider.CreatePayment(ctx, domainpayments.CreatePaymentInput{
		OrderID:        order.ID,
		ProjectID:      order.ProjectID,
		Amount:         order.Amount,
		Currency:       order.Currency,
		Description:    "subscription " + sub.ID,
		ExpiresAt:      order.ExpiresAt,
		SuccessURL:     successURL,
		CancelURL:      cancelURL,
		IdempotencyKey: "order:" + order.ID,
	})
	if err != nil {
		return nil, "", err
	}
	var locked *domainpayments.Order
	if err := s.db.RunInNewTx(ctx, func(txCtx context.Context) error {
		var err error
		locked, err = s.orders.GetByIDForUpdate(txCtx, order.ProjectID, order.ID)
		if err != nil {
			return err
		}
		if locked == nil {
			return status.Error(codes.NotFound, "payment order not found")
		}
		if locked.Status != domainpayments.OrderStatusCreated {
			return nil // 并发已推进（回调先到）：保持现状态。
		}
		locked.ProviderSessionID = session.SessionID
		if session.ProviderOrderID != "" {
			locked.ProviderOrderID = session.ProviderOrderID
		}
		if err := locked.Transition(domainpayments.OrderStatusPaying, now); err != nil {
			return err
		}
		if err := s.orders.Update(txCtx, locked, domainpayments.OrderStatusCreated); err != nil {
			return err
		}
		if err := s.upsertIndex(txCtx, locked.Provider, domainpayments.IndexKindPaymentSession, session.SessionID, locked.ProjectID); err != nil {
			return err
		}
		if session.ProviderOrderID != "" {
			return s.upsertIndex(txCtx, locked.Provider, domainpayments.IndexKindPaymentOrder, session.ProviderOrderID, locked.ProjectID)
		}
		return nil
	}); err != nil {
		return nil, "", err
	}
	if locked != nil {
		return locked, session.PaymentURL, nil
	}
	order.ProviderSessionID = session.SessionID
	if session.ProviderOrderID != "" {
		order.ProviderOrderID = session.ProviderOrderID
	}
	return order, session.PaymentURL, nil
}
