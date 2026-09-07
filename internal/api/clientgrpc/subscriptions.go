package clientgrpc

import (
	"context"
	"encoding/base64"
	"time"

	clientv1 "github.com/torchwooddev/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
	appsubs "github.com/torchwooddev/torchwood/internal/app/subscriptions"
	domainsubs "github.com/torchwooddev/torchwood/internal/domain/subscriptions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SubscriptionsService 是终端用户订阅面 gRPC handler（薄：DTO 映射）。
type SubscriptionsService struct {
	clientv1.UnimplementedSubscriptionsServiceServer
	subs *appsubs.Subscriptions
}

// NewSubscriptionsService constructs the client subscriptions service.
func NewSubscriptionsService(subs *appsubs.Subscriptions) *SubscriptionsService {
	return &SubscriptionsService{subs: subs}
}

func (s *SubscriptionsService) ListPlans(ctx context.Context, req *clientv1.ListPlansRequest) (*clientv1.ListPlansResponse, error) {
	before, err := decodeSubCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	plans, err := s.subs.ListClientPlans(ctx, int(req.GetPageSize()), before)
	if err != nil {
		return nil, err
	}
	out := make([]*clientv1.SubscriptionPlan, len(plans))
	for i := range plans {
		out[i] = mapClientPlan(&plans[i])
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(plans) > 0 {
		meta.NextPageToken = encodeSubCursor(plans[len(plans)-1].CreatedAt)
	}
	return &clientv1.ListPlansResponse{Plans: out, Meta: meta}, nil
}

func (s *SubscriptionsService) Subscribe(ctx context.Context, req *clientv1.SubscribeRequest) (*clientv1.SubscribeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	result, err := s.subs.Subscribe(ctx, appsubs.SubscribeCommand{
		PlanCode:         req.GetPlanCode(),
		Mode:             domainsubs.Mode(req.GetMode()),
		IdempotencyKey:   req.GetIdempotencyKey(),
		BillingAssetCode: req.GetBillingAssetCode(),
	})
	if err != nil {
		return nil, err
	}
	mapped := mapClientSub(result.Subscription, result.Plan)
	mapped.PaymentUrl = result.PaymentURL
	return &clientv1.SubscribeResponse{
		Subscription:     mapped,
		IdempotentReplay: result.IdempotentReplay,
		PaymentUrl:       result.PaymentURL,
		OrderId:          result.OrderID,
	}, nil
}

func (s *SubscriptionsService) GetMySubscription(ctx context.Context, req *clientv1.GetMySubscriptionRequest) (*clientv1.Subscription, error) {
	var planCode string
	if req != nil {
		planCode = req.GetPlanCode()
	}
	sub, plan, err := s.subs.GetMySubscription(ctx, planCode)
	if err != nil {
		return nil, err
	}
	return mapClientSub(sub, plan), nil
}

func (s *SubscriptionsService) Cancel(ctx context.Context, req *clientv1.CancelRequest) (*clientv1.Subscription, error) {
	// subscription_id required 由 CancelRequest 的 buf.validate 注解在
	// validate 拦截器承担（09-api-guide §2.3）。
	sub, plan, err := s.subs.CancelAtPeriodEnd(ctx, req.GetSubscriptionId())
	if err != nil {
		return nil, err
	}
	return mapClientSub(sub, plan), nil
}

func encodeSubCursor(t time.Time) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano)))
}

func decodeSubCursor(token string) (time.Time, error) {
	if token == "" {
		return time.Time{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, string(raw))
}

func mapClientPlan(p *domainsubs.Plan) *clientv1.SubscriptionPlan {
	if p == nil {
		return nil
	}
	return &clientv1.SubscriptionPlan{
		Id:           p.ID,
		Code:         p.Code,
		Name:         p.Name,
		Amount:       p.Amount,
		Currency:     p.Currency,
		Interval:     string(p.Interval),
		IntervalDays: p.IntervalDays,
		GraceDays:    p.GraceDays,
		TrialDays:    p.TrialDays,
		Benefits:     mapClientBenefits(p.Benefits),
	}
}

func mapClientSub(sub *domainsubs.Subscription, plan *domainsubs.Plan) *clientv1.Subscription {
	if sub == nil {
		return nil
	}
	out := &clientv1.Subscription{
		Id:                 sub.ID,
		PlanId:             sub.PlanID,
		Mode:               string(sub.Mode),
		Status:             string(sub.Status),
		CurrentPeriodStart: timestamppb.New(sub.CurrentPeriodStart),
		CurrentPeriodEnd:   timestamppb.New(sub.CurrentPeriodEnd),
		CancelAtPeriodEnd:  sub.CancelAtPeriodEnd,
		Benefits:           mapClientBenefits(sub.Benefits),
		CreatedAt:          timestamppb.New(sub.CreatedAt),
	}
	if plan != nil {
		out.PlanCode = plan.Code
	}
	if sub.GraceUntil != nil {
		out.GraceUntil = timestamppb.New(*sub.GraceUntil)
	}
	return out
}

func mapClientBenefits(b domainsubs.Benefits) *clientv1.Benefits {
	out := &clientv1.Benefits{}
	for _, g := range b.Grants {
		item := &clientv1.BenefitGrant{AssetCode: g.AssetCode, Quantity: g.Quantity}
		if g.ExpiresIn != nil {
			item.ExpiresIn = g.ExpiresIn
		}
		out.Grants = append(out.Grants, item)
	}
	for _, e := range b.Entitlements {
		out.Entitlements = append(out.Entitlements, &clientv1.BenefitEntitlement{
			AssetCode: e.AssetCode,
			Tier:      e.Tier,
		})
	}
	return out
}
