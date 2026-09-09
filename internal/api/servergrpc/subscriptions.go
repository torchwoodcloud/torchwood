package servergrpc

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appsubs "github.com/torchwoodcloud/torchwood/internal/app/subscriptions"
	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SubscriptionsService 是订阅管理面 gRPC handler。
type SubscriptionsService struct {
	serverv1.UnimplementedSubscriptionsServiceServer
	subs *appsubs.Subscriptions
}

// NewSubscriptionsService constructs the server subscriptions service.
func NewSubscriptionsService(subs *appsubs.Subscriptions) *SubscriptionsService {
	return &SubscriptionsService{subs: subs}
}

func (s *SubscriptionsService) CreatePlan(ctx context.Context, req *serverv1.CreatePlanRequest) (*serverv1.SubscriptionPlan, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	plan, err := s.subs.CreatePlan(withAuditResource(ctx, req.GetCode()), appsubs.CreatePlanCommand{
		Code:              req.GetCode(),
		Name:              req.GetName(),
		Amount:            req.GetAmount(),
		Currency:          req.GetCurrency(),
		Interval:          domainsubs.Interval(req.GetInterval()),
		IntervalDays:      req.GetIntervalDays(),
		GraceDays:         req.GetGraceDays(),
		TrialDays:         req.GetTrialDays(),
		Benefits:          protoBenefits(req.GetBenefits()),
		ProviderOverrides: protoOverrides(req.GetProviderOverrides()),
	})
	if err != nil {
		return nil, err
	}
	return mapServerPlan(plan), nil
}

func (s *SubscriptionsService) ListPlans(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListPlansResponse, error) {
	before, err := decodeServerOrderCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	plans, err := s.subs.ListPlans(ctx, true, int(req.GetPageSize()), before)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.SubscriptionPlan, len(plans))
	for i := range plans {
		out[i] = mapServerPlan(&plans[i])
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(plans) > 0 {
		meta.NextPageToken = encodeServerOrderCursor(plans[len(plans)-1].CreatedAt)
	}
	return &serverv1.ListPlansResponse{Plans: out, Meta: meta}, nil
}

func (s *SubscriptionsService) GetPlan(ctx context.Context, req *serverv1.GetPlanRequest) (*serverv1.SubscriptionPlan, error) {
	// plan_id required 由 buf.validate 注解在 validate 拦截器承担（09-api-guide §2.3）。
	plan, err := s.subs.GetPlan(ctx, req.GetPlanId())
	if err != nil {
		return nil, err
	}
	return mapServerPlan(plan), nil
}

func (s *SubscriptionsService) UpdatePlan(ctx context.Context, req *serverv1.UpdatePlanRequest) (*serverv1.SubscriptionPlan, error) {
	// plan_id required 同上，由 buf.validate 注解承担。
	cmd := appsubs.UpdatePlanCommand{PlanID: req.GetPlanId()}
	if req.Name != nil {
		cmd.Name = req.Name
	}
	if req.Amount != nil {
		cmd.Amount = req.Amount
	}
	if req.Currency != nil {
		cmd.Currency = req.Currency
	}
	if req.Interval != nil {
		iv := domainsubs.Interval(req.GetInterval())
		cmd.Interval = &iv
	}
	if req.IntervalDays != nil {
		cmd.IntervalDays = req.IntervalDays
	}
	if req.GraceDays != nil {
		cmd.GraceDays = req.GraceDays
	}
	if req.TrialDays != nil {
		cmd.TrialDays = req.TrialDays
	}
	if req.GetBenefits() != nil {
		b := protoBenefits(req.GetBenefits())
		cmd.Benefits = &b
	}
	if req.GetProviderOverrides() != nil {
		o := protoOverrides(req.GetProviderOverrides())
		cmd.ProviderOverrides = &o
	}
	if req.Status != nil {
		st := domainsubs.PlanStatus(req.GetStatus())
		cmd.Status = &st
	}
	plan, err := s.subs.UpdatePlan(withAuditResource(ctx, req.GetPlanId()), cmd)
	if err != nil {
		return nil, err
	}
	return mapServerPlan(plan), nil
}

func (s *SubscriptionsService) DeletePlan(ctx context.Context, req *serverv1.DeletePlanRequest) (*sharedv1.Empty, error) {
	// plan_id required 同上，由 buf.validate 注解承担。
	if err := s.subs.DeletePlan(withAuditResource(ctx, req.GetPlanId()), req.GetPlanId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *SubscriptionsService) ListSubscriptions(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListSubscriptionsResponse, error) {
	before, err := decodeServerOrderCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	rows, err := s.subs.ListProjectSubscriptions(ctx, int(req.GetPageSize()), before)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.Subscription, len(rows))
	for i := range rows {
		out[i] = mapServerSub(&rows[i], nil)
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(rows) > 0 {
		meta.NextPageToken = encodeServerOrderCursor(rows[len(rows)-1].CreatedAt)
	}
	return &serverv1.ListSubscriptionsResponse{Subscriptions: out, Meta: meta}, nil
}

func (s *SubscriptionsService) GetSubscription(ctx context.Context, req *serverv1.GetSubscriptionRequest) (*serverv1.Subscription, error) {
	// subscription_id required 同上，由 buf.validate 注解承担。
	sub, plan, err := s.subs.GetSubscription(ctx, req.GetSubscriptionId())
	if err != nil {
		return nil, err
	}
	return mapServerSub(sub, plan), nil
}

func (s *SubscriptionsService) CancelSubscription(ctx context.Context, req *serverv1.CancelSubscriptionRequest) (*serverv1.Subscription, error) {
	// subscription_id required 同上，由 buf.validate 注解承担。
	sub, plan, err := s.subs.ForceCancel(withAuditResource(ctx, req.GetSubscriptionId()), req.GetSubscriptionId())
	if err != nil {
		return nil, err
	}
	return mapServerSub(sub, plan), nil
}

func (s *SubscriptionsService) ExpireSubscription(ctx context.Context, req *serverv1.ExpireSubscriptionRequest) (*serverv1.Subscription, error) {
	// subscription_id required 同上，由 buf.validate 注解承担。
	sub, plan, err := s.subs.ForceExpire(withAuditResource(ctx, req.GetSubscriptionId()), req.GetSubscriptionId())
	if err != nil {
		return nil, err
	}
	return mapServerSub(sub, plan), nil
}

func mapServerPlan(p *domainsubs.Plan) *serverv1.SubscriptionPlan {
	if p == nil {
		return nil
	}
	return &serverv1.SubscriptionPlan{
		Id:                p.ID,
		ProjectId:         p.ProjectID,
		Code:              p.Code,
		Name:              p.Name,
		Amount:            p.Amount,
		Currency:          p.Currency,
		Interval:          string(p.Interval),
		IntervalDays:      p.IntervalDays,
		GraceDays:         p.GraceDays,
		TrialDays:         p.TrialDays,
		Benefits:          mapServerBenefits(p.Benefits),
		ProviderOverrides: &serverv1.ProviderOverrides{StripePriceId: p.ProviderOverrides.StripePriceID},
		Status:            string(p.Status),
		CreatedAt:         timestamppb.New(p.CreatedAt),
		UpdatedAt:         timestamppb.New(p.UpdatedAt),
	}
}

func mapServerSub(sub *domainsubs.Subscription, plan *domainsubs.Plan) *serverv1.Subscription {
	if sub == nil {
		return nil
	}
	out := &serverv1.Subscription{
		Id:                 sub.ID,
		ProjectId:          sub.ProjectID,
		UserId:             sub.UserID,
		PlanId:             sub.PlanID,
		Mode:               string(sub.Mode),
		Provider:           sub.Provider,
		ProviderSubId:      sub.ProviderSubID,
		Status:             string(sub.Status),
		CurrentPeriodStart: timestamppb.New(sub.CurrentPeriodStart),
		CurrentPeriodEnd:   timestamppb.New(sub.CurrentPeriodEnd),
		CancelAtPeriodEnd:  sub.CancelAtPeriodEnd,
		BillingAssetCode:   sub.BillingAssetCode,
		Benefits:           mapServerBenefits(sub.Benefits),
		CreatedAt:          timestamppb.New(sub.CreatedAt),
		UpdatedAt:          timestamppb.New(sub.UpdatedAt),
	}
	if plan != nil {
		out.PlanCode = plan.Code
	}
	if sub.GraceUntil != nil {
		out.GraceUntil = timestamppb.New(*sub.GraceUntil)
	}
	return out
}

func mapServerBenefits(b domainsubs.Benefits) *serverv1.Benefits {
	out := &serverv1.Benefits{}
	for _, g := range b.Grants {
		item := &serverv1.BenefitGrant{AssetCode: g.AssetCode, Quantity: g.Quantity}
		if g.ExpiresIn != nil {
			item.ExpiresIn = g.ExpiresIn
		}
		out.Grants = append(out.Grants, item)
	}
	for _, e := range b.Entitlements {
		out.Entitlements = append(out.Entitlements, &serverv1.BenefitEntitlement{
			AssetCode: e.AssetCode,
			Tier:      e.Tier,
		})
	}
	return out
}

func protoBenefits(in *serverv1.Benefits) domainsubs.Benefits {
	if in == nil {
		return domainsubs.Benefits{}
	}
	out := domainsubs.Benefits{
		Grants:       make([]domainsubs.BenefitGrant, 0, len(in.Grants)),
		Entitlements: make([]domainsubs.BenefitEntitlement, 0, len(in.Entitlements)),
	}
	for _, g := range in.Grants {
		item := domainsubs.BenefitGrant{AssetCode: g.GetAssetCode(), Quantity: g.GetQuantity()}
		if g.ExpiresIn != nil {
			v := g.GetExpiresIn()
			item.ExpiresIn = &v
		}
		out.Grants = append(out.Grants, item)
	}
	for _, e := range in.Entitlements {
		out.Entitlements = append(out.Entitlements, domainsubs.BenefitEntitlement{
			AssetCode: e.GetAssetCode(),
			Tier:      e.GetTier(),
		})
	}
	return out
}

func protoOverrides(in *serverv1.ProviderOverrides) domainsubs.ProviderOverrides {
	if in == nil {
		return domainsubs.ProviderOverrides{}
	}
	return domainsubs.ProviderOverrides{StripePriceID: in.GetStripePriceId()}
}
