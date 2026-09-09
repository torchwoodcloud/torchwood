package client

import (
	"context"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
)

// SubscriptionsService 封装 Client API 的订阅服务。
type SubscriptionsService struct{ c *Client }

// ListPlans 列出可订阅计划。
func (s *SubscriptionsService) ListPlans(ctx context.Context, pageSize int32, pageToken string) (*clientv1.ListPlansResponse, error) {
	return s.c.subscriptions.ListPlans(ctx, &clientv1.ListPlansRequest{
		PageSize:  pageSize,
		PageToken: pageToken,
	})
}

// Subscribe 订阅计划（hosted / platform）。
func (s *SubscriptionsService) Subscribe(ctx context.Context, req *clientv1.SubscribeRequest) (*clientv1.SubscribeResponse, error) {
	return s.c.subscriptions.Subscribe(ctx, req)
}

// GetMySubscription 获取本人当前订阅；planCode 空则返回非终态或最近一条。
func (s *SubscriptionsService) GetMySubscription(ctx context.Context, planCode string) (*clientv1.Subscription, error) {
	return s.c.subscriptions.GetMySubscription(ctx, &clientv1.GetMySubscriptionRequest{PlanCode: planCode})
}

// Cancel 期末取消订阅（cancel_at_period_end）。
func (s *SubscriptionsService) Cancel(ctx context.Context, subscriptionID string) (*clientv1.Subscription, error) {
	return s.c.subscriptions.Cancel(ctx, &clientv1.CancelRequest{SubscriptionId: subscriptionID})
}
