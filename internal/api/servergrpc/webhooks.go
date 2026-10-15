// WebhooksService 的 Server 面 gRPC handler（functions-v4.md §6，D5）：
// 订阅 CRUD + 密钥轮换 + 投递行浏览/重投。形状校验全在 protovalidate
// （链尾 ValidateInterceptor）；密钥明文只在创建/轮换响应一次性出现，
// 本文件任何映射函数不得把密文/明文带入其他响应面。
package servergrpc

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	apishared "github.com/torchwoodcloud/torchwood/internal/api/shared"
	appwebhooks "github.com/torchwoodcloud/torchwood/internal/app/webhooks"
	domainwebhooks "github.com/torchwoodcloud/torchwood/internal/domain/webhooks"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// WebhooksService 实现 serverv1.WebhooksServiceServer。
type WebhooksService struct {
	serverv1.UnimplementedWebhooksServiceServer
	webhooks *appwebhooks.Service
}

// NewWebhooksService 构造 handler（Wire）。
func NewWebhooksService(webhooks *appwebhooks.Service) *WebhooksService {
	return &WebhooksService{webhooks: webhooks}
}

func (s *WebhooksService) projectID(ctx context.Context) (string, error) {
	p, ok := contexts.Principal(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing project context")
	}
	if p.ProjectID == "" {
		return "", status.Error(codes.Unauthenticated, "missing project context")
	}
	return p.ProjectID, nil
}

func (s *WebhooksService) CreateWebhook(ctx context.Context, req *serverv1.CreateWebhookRequest) (*serverv1.Webhook, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	w, _, err := s.webhooks.CreateWebhook(ctx, appwebhooks.CreateWebhookCommand{
		ID:        req.GetId(),
		ProjectID: projectID,
		URL:       req.GetUrl(),
		Events:    req.GetEvents(),
		Status:    req.GetStatus(),
	})
	if err != nil {
		return nil, err
	}
	// 明文密钥只在 CreateWebhookSecret/RotateWebhookSecret 的专用响应出现；
	// 本响应按 Webhook 视图投影（仅指纹）。
	return mapWebhook(w), nil
}

func (s *WebhooksService) GetWebhook(ctx context.Context, req *serverv1.GetWebhookRequest) (*serverv1.Webhook, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	w, err := s.webhooks.GetWebhook(ctx, projectID, req.GetWebhookId())
	if err != nil {
		return nil, err
	}
	return mapWebhook(w), nil
}

func (s *WebhooksService) ListWebhooks(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListWebhooksResponse, error) {
	params, err := crud.ParseListParams(req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ws, err := s.webhooks.ListWebhooks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	page, info, err := crud.SliceOffsetPage(ws, params)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*serverv1.Webhook, len(page))
	for i := range page {
		out[i] = mapWebhook(&page[i])
	}
	return &serverv1.ListWebhooksResponse{
		Webhooks: out,
		Meta:     apishared.OffsetPageMeta(info),
	}, nil
}

func (s *WebhooksService) UpdateWebhook(ctx context.Context, req *serverv1.UpdateWebhookRequest) (*serverv1.Webhook, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	cmd := appwebhooks.UpdateWebhookCommand{
		ProjectID: projectID,
		WebhookID: req.GetWebhookId(),
		URL:       req.Url,
		Status:    req.Status,
		Events:    req.GetEvents(),
		EventsSet: req.Events != nil,
	}
	w, err := s.webhooks.UpdateWebhook(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapWebhook(w), nil
}

func (s *WebhooksService) DeleteWebhook(ctx context.Context, req *serverv1.GetWebhookRequest) (*sharedv1.Empty, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.webhooks.DeleteWebhook(ctx, projectID, req.GetWebhookId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *WebhooksService) RotateWebhookSecret(ctx context.Context, req *serverv1.RotateWebhookSecretRequest) (*serverv1.WebhookSecret, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	w, secret, err := s.webhooks.RotateWebhookSecret(ctx, projectID, req.GetWebhookId())
	if err != nil {
		return nil, err
	}
	return &serverv1.WebhookSecret{
		WebhookId:         w.ID,
		Secret:            secret,
		SecretFingerprint: w.SecretFingerprint,
	}, nil
}

func (s *WebhooksService) ListDeliveries(ctx context.Context, req *serverv1.ListDeliveriesRequest) (*serverv1.ListDeliveriesResponse, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	params, err := crud.ParseListParams(req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	pageRes, err := s.webhooks.ListDeliveries(ctx, projectID, req.GetWebhookId(), req.GetStatus(), int(params.PageSize), params.Offset)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.WebhookDelivery, len(pageRes.Items))
	for i := range pageRes.Items {
		out[i] = mapDelivery(&pageRes.Items[i])
	}
	return &serverv1.ListDeliveriesResponse{
		Deliveries: out,
		Meta: &sharedv1.ListResponseMeta{
			PageSize:   params.PageSize,
			TotalCount: int32(pageRes.Total),
		},
	}, nil
}

func (s *WebhooksService) GetDelivery(ctx context.Context, req *serverv1.GetDeliveryRequest) (*serverv1.WebhookDelivery, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.webhooks.GetDelivery(ctx, projectID, req.GetWebhookId(), req.GetDeliveryId())
	if err != nil {
		return nil, err
	}
	return mapDelivery(d), nil
}

func (s *WebhooksService) RedeliverDelivery(ctx context.Context, req *serverv1.RedeliverDeliveryRequest) (*serverv1.WebhookDelivery, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.webhooks.RedeliverDelivery(ctx, projectID, req.GetWebhookId(), req.GetDeliveryId())
	if err != nil {
		return nil, err
	}
	return mapDelivery(d), nil
}

// ——投影——

// mapWebhook 投影订阅视图（密钥任何形态不出现在本视图）。
func mapWebhook(w *domainwebhooks.Webhook) *serverv1.Webhook {
	return &serverv1.Webhook{
		Id:                w.ID,
		ProjectId:         w.ProjectID,
		Url:               w.URL,
		Events:            w.Events,
		Status:            w.Status,
		SecretFingerprint: w.SecretFingerprint,
		CreatedAt:         timestamppb.New(w.CreatedAt),
		UpdatedAt:         timestamppb.New(w.UpdatedAt),
	}
}

// mapDelivery 投影投递行视图（payload 重投事实源不外发）。
func mapDelivery(d *domainwebhooks.Delivery) *serverv1.WebhookDelivery {
	return &serverv1.WebhookDelivery{
		Id:               d.ID,
		WebhookId:        d.WebhookID,
		EventId:          d.EventID,
		EventType:        d.EventType,
		Seq:              d.Seq,
		Attempts:         int32(d.Attempts),
		NextAttemptAt:    timestamppb.New(d.NextAttemptAt),
		Status:           d.Status,
		LastResponseCode: int32(d.LastResponseCode),
		LastError:        d.LastError,
		CreatedAt:        timestamppb.New(d.CreatedAt),
		UpdatedAt:        timestamppb.New(d.UpdatedAt),
	}
}
