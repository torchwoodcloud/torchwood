package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// SubscriptionsService 封装 Server API 的订阅管理。
type SubscriptionsService struct {
	c   *Client
	api serverv1.SubscriptionsServiceClient
}

func (s *SubscriptionsService) CreatePlan(ctx context.Context, req *serverv1.CreatePlanRequest) (*serverv1.SubscriptionPlan, error) {
	return s.api.CreatePlan(ctx, req)
}

func (s *SubscriptionsService) ListPlans(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListPlansResponse, error) {
	return s.api.ListPlans(ctx, req)
}

func (s *SubscriptionsService) GetPlan(ctx context.Context, planID string) (*serverv1.SubscriptionPlan, error) {
	return s.api.GetPlan(ctx, &serverv1.GetPlanRequest{PlanId: planID})
}

func (s *SubscriptionsService) UpdatePlan(ctx context.Context, req *serverv1.UpdatePlanRequest) (*serverv1.SubscriptionPlan, error) {
	return s.api.UpdatePlan(ctx, req)
}

func (s *SubscriptionsService) DeletePlan(ctx context.Context, planID string) error {
	_, err := s.api.DeletePlan(ctx, &serverv1.DeletePlanRequest{PlanId: planID})
	return err
}

func (s *SubscriptionsService) ListSubscriptions(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListSubscriptionsResponse, error) {
	return s.api.ListSubscriptions(ctx, req)
}

func (s *SubscriptionsService) GetSubscription(ctx context.Context, subscriptionID string) (*serverv1.Subscription, error) {
	return s.api.GetSubscription(ctx, &serverv1.GetSubscriptionRequest{SubscriptionId: subscriptionID})
}

func (s *SubscriptionsService) CancelSubscription(ctx context.Context, req *serverv1.CancelSubscriptionRequest) (*serverv1.Subscription, error) {
	return s.api.CancelSubscription(ctx, req)
}

func (s *SubscriptionsService) ExpireSubscription(ctx context.Context, req *serverv1.ExpireSubscriptionRequest) (*serverv1.Subscription, error) {
	return s.api.ExpireSubscription(ctx, req)
}
