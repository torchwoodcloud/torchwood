package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
)

// OutboxService 封装 Server API 的 Outbox 死信管理。
type OutboxService struct {
	c   *Client
	api serverv1.OutboxServiceClient
}

// ListDeadLetters 列出死信。
func (s *OutboxService) ListDeadLetters(ctx context.Context, req *serverv1.ListDeadLettersRequest) (*serverv1.ListDeadLettersResponse, error) {
	return s.api.ListDeadLetters(ctx, req)
}

// ReplayDeadLetter 重放单条死信。
func (s *OutboxService) ReplayDeadLetter(ctx context.Context, req *serverv1.ReplayDeadLetterRequest) (*serverv1.ReplayDeadLetterResponse, error) {
	return s.api.ReplayDeadLetter(ctx, req)
}
