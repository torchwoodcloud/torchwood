package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// WebhooksService 封装 Server API 的 Webhooks 服务（functions-v4.md §6，D5：
// 订阅 outbox 事件脊柱，按 CloudEvents 1.0 structured mode 投递；计量一期
// 仅进 metrics）。
type WebhooksService struct {
	c   *Client
	api serverv1.WebhooksServiceClient
}

// CreateWebhook 创建订阅（https 强制；签名密钥服务端生成、明文一次性回显）。
func (s *WebhooksService) CreateWebhook(ctx context.Context, req *serverv1.CreateWebhookRequest) (*serverv1.Webhook, error) {
	return s.api.CreateWebhook(ctx, req)
}

// GetWebhook 按 ID 获取订阅。
func (s *WebhooksService) GetWebhook(ctx context.Context, req *serverv1.GetWebhookRequest) (*serverv1.Webhook, error) {
	return s.api.GetWebhook(ctx, req)
}

// ListWebhooks 列出订阅。
func (s *WebhooksService) ListWebhooks(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListWebhooksResponse, error) {
	return s.api.ListWebhooks(ctx, req)
}

// UpdateWebhook 部分更新订阅（optional 未设置 = 不修改；密钥走 Rotate）。
func (s *WebhooksService) UpdateWebhook(ctx context.Context, req *serverv1.UpdateWebhookRequest) (*serverv1.Webhook, error) {
	return s.api.UpdateWebhook(ctx, req)
}

// DeleteWebhook 删除订阅（投递行级联清理）。
func (s *WebhooksService) DeleteWebhook(ctx context.Context, req *serverv1.GetWebhookRequest) error {
	_, err := s.api.DeleteWebhook(ctx, req)
	return err
}

// RotateWebhookSecret 轮换签名密钥（新明文一次性回显；旧钥宽限窗双签 v2）。
func (s *WebhooksService) RotateWebhookSecret(ctx context.Context, req *serverv1.RotateWebhookSecretRequest) (*serverv1.WebhookSecret, error) {
	return s.api.RotateWebhookSecret(ctx, req)
}

// ListDeliveries 列出投递行（DLQ 浏览面；status 过滤 optional）。
func (s *WebhooksService) ListDeliveries(ctx context.Context, req *serverv1.ListDeliveriesRequest) (*serverv1.ListDeliveriesResponse, error) {
	return s.api.ListDeliveries(ctx, req)
}

// GetDelivery 按 ID 获取投递行。
func (s *WebhooksService) GetDelivery(ctx context.Context, req *serverv1.GetDeliveryRequest) (*serverv1.WebhookDelivery, error) {
	return s.api.GetDelivery(ctx, req)
}

// RedeliverDelivery 手动重投（终态失败行重置为 pending 即时到期）。
func (s *WebhooksService) RedeliverDelivery(ctx context.Context, req *serverv1.RedeliverDeliveryRequest) (*serverv1.WebhookDelivery, error) {
	return s.api.RedeliverDelivery(ctx, req)
}
