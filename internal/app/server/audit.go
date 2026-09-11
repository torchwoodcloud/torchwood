package server

import (
	"context"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AuditLogs 是审计日志查询用例（roadmap §3.4「管理面写操作可查」）。
// 写入侧在 gRPC 审计拦截器全量落库，本用例只管读取的作用域语义。
type AuditLogs struct {
	repo audit.Repository
}

func NewAuditLogs(repo audit.Repository) *AuditLogs {
	return &AuditLogs{repo: repo}
}

// AuditLogsQuery 与 server.v1.ListAuditLogsRequest 同构；分页（Offset/
// PageSize）由传输层经 pkg/crud 解析后填入。
type AuditLogsQuery struct {
	ActorID         string
	ActorKind       string
	Action          string
	Status          string
	ResourceID      string
	CreatedAfter    *time.Time
	CreatedBefore   *time.Time
	IncludePlatform bool
	AllProjects     bool
	Offset          int
	PageSize        int
}

// List 返回当页审计行与过滤条件下总数。
//
// 项目作用域（对齐 OutboxService 决策 v8）：默认取凭证项目上下文，缺失即
// FailedPrecondition；仅 all_projects 的平台全量视图豁免。include_platform
// （并入 project_id IS NULL 的平台级行）与 all_projects 均要求平台 admin——
// 角色门（admin/owner）已在拦截器 admin_roles 声明，此处只叠加平台级视图
// 的平台 admin 门槛。
func (a *AuditLogs) List(ctx context.Context, query AuditLogsQuery) ([]audit.Entry, int, error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p == nil {
		return nil, 0, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if (query.IncludePlatform || query.AllProjects) && !p.IsPlatformAdmin {
		return nil, 0, status.Error(codes.PermissionDenied, "platform-level audit views require a platform admin")
	}
	filter := audit.ListFilter{
		ActorID:         query.ActorID,
		ActorKind:       query.ActorKind,
		Action:          query.Action,
		Status:          query.Status,
		ResourceID:      query.ResourceID,
		CreatedAfter:    query.CreatedAfter,
		CreatedBefore:   query.CreatedBefore,
		IncludePlatform: query.IncludePlatform,
		AllProjects:     query.AllProjects,
		Offset:          query.Offset,
		PageSize:        query.PageSize,
	}
	if !query.AllProjects {
		if p.ProjectID == "" {
			return nil, 0, status.Error(codes.FailedPrecondition, "project context required (X-Torchwood-Project header for admin sessions)")
		}
		filter.ProjectID = p.ProjectID
	}
	return a.repo.List(ctx, filter)
}
