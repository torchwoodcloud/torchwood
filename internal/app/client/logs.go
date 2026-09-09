package client

import (
	"context"

	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultLogsLimit = 50
	maxLogsLimit     = 100
)

// ListLogs 返回当前用户的最近操作日志（created_at DESC，limit ≤ 100）。
func (a *Account) ListLogs(ctx context.Context, limit int32) ([]audit.Entry, error) {
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if a.auditRepo == nil {
		return nil, status.Error(codes.Unimplemented, "account logs are not configured")
	}
	n := int(limit)
	if n <= 0 {
		n = defaultLogsLimit
	}
	if n > maxLogsLimit {
		n = maxLogsLimit
	}
	return a.auditRepo.ListByActor(ctx, p.ProjectID, p.UserID, n)
}
