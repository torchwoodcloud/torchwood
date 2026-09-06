package events

import (
	"context"
	"time"

	"github.com/torchwooddev/torchwood/internal/app/shared"
	"github.com/torchwooddev/torchwood/internal/domain/events"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type OutboxAdmin struct {
	repo     events.OutboxRepository
	projects projects.Repository
}

func NewOutboxAdmin(repo events.OutboxRepository, projects projects.Repository) *OutboxAdmin {
	return &OutboxAdmin{repo: repo, projects: projects}
}

func (o *OutboxAdmin) ensureProjectActive(ctx context.Context, projectID string) error {
	if o.projects == nil {
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	proj, err := o.projects.GetProject(ctx2, projectID)
	if err != nil {
		return status.Errorf(codes.Internal, "project lookup failed: %v", err)
	}
	if proj == nil || proj.Status != "active" {
		return status.Error(codes.FailedPrecondition, "project is not active")
	}
	return nil
}

func (o *OutboxAdmin) ListDeadLetters(ctx context.Context, projectID string, pageSize int32, pageToken string) ([]events.DeadLetter, int64, string, error) {
	// 二道防线（决策 v8）：死信 payload 含文档数据，读面与重放同为
	// server 写主体门（角色/scope 细粒度由拦截器策略表把关）。
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return nil, 0, "", err
	}
	if projectID == "" {
		return nil, 0, "", status.Error(codes.FailedPrecondition, "project context required")
	}
	// 项目绑定核对（fail-closed：空 ProjectID 不再放行——修越权面）；
	// 越权统一 NotFound 防枚举（错误码总则）。
	if p, _ := contexts.Principal(ctx); p != nil && p.ProjectID != projectID {
		return nil, 0, "", status.Error(codes.NotFound, "dead letters not found")
	}
	if err := o.ensureProjectActive(ctx, projectID); err != nil {
		return nil, 0, "", err
	}
	return o.repo.ListDeadLetters(ctx, projectID, pageSize, pageToken)
}

func (o *OutboxAdmin) ReplayDeadLetter(ctx context.Context, eventID, projectID string) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if eventID == "" {
		return status.Error(codes.InvalidArgument, "event_id is required")
	}
	if projectID == "" {
		return status.Error(codes.FailedPrecondition, "project context required")
	}
	if p, _ := contexts.Principal(ctx); p != nil && p.ProjectID != projectID {
		return status.Error(codes.NotFound, "dead letter not found")
	}
	if err := o.ensureProjectActive(ctx, projectID); err != nil {
		return err
	}
	return o.repo.ReplayDeadLetter(ctx, eventID, projectID)
}
