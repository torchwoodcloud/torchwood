package server_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// auditFakeRepo 记录收到的 ListFilter。
type auditFakeRepo struct {
	filter  audit.ListFilter
	entries []audit.Entry
}

func (r *auditFakeRepo) Insert(_ context.Context, _ *audit.Entry) error { return nil }
func (r *auditFakeRepo) ListByActor(context.Context, string, string, int) ([]audit.Entry, error) {
	return nil, nil
}
func (r *auditFakeRepo) List(_ context.Context, filter audit.ListFilter) ([]audit.Entry, int, error) {
	r.filter = filter
	return r.entries, len(r.entries), nil
}

func auditCtx(p *shared.Principal) context.Context {
	return contexts.WithPrincipal(context.Background(), p)
}

// TestAuditLogs_ListScoping：项目作用域与平台级视图的门槛语义。
func TestAuditLogs_ListScoping(t *testing.T) {
	repo := &auditFakeRepo{}
	uc := server.NewAuditLogs(repo)

	projectAdmin := &shared.Principal{
		ActorID:         "admin-1",
		ActorKind:       shared.ActorKindAdmin,
		ProjectID:       "proj-1",
		IsPlatformAdmin: false,
	}
	platformAdmin := &shared.Principal{
		ActorID:         "admin-2",
		ActorKind:       shared.ActorKindAdmin,
		ProjectID:       "proj-1",
		IsPlatformAdmin: true,
	}

	t.Run("项目作用域：filter 带凭证项目", func(t *testing.T) {
		_, _, err := uc.List(auditCtx(projectAdmin), server.AuditLogsQuery{PageSize: 50})
		require.NoError(t, err)
		require.Equal(t, "proj-1", repo.filter.ProjectID)
		require.False(t, repo.filter.IncludePlatform)
		require.False(t, repo.filter.AllProjects)
	})

	t.Run("无项目上下文：FailedPrecondition（对齐 outbox 决策 v8）", func(t *testing.T) {
		_, _, err := uc.List(auditCtx(&shared.Principal{ActorKind: shared.ActorKindAdmin}), server.AuditLogsQuery{})
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("include_platform 非平台 admin 拒绝", func(t *testing.T) {
		_, _, err := uc.List(auditCtx(projectAdmin), server.AuditLogsQuery{IncludePlatform: true})
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})

	t.Run("all_projects 非平台 admin 拒绝", func(t *testing.T) {
		_, _, err := uc.List(auditCtx(projectAdmin), server.AuditLogsQuery{AllProjects: true})
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})

	t.Run("平台 admin include_platform：项目 ∪ 平台行", func(t *testing.T) {
		_, _, err := uc.List(auditCtx(platformAdmin), server.AuditLogsQuery{IncludePlatform: true})
		require.NoError(t, err)
		require.Equal(t, "proj-1", repo.filter.ProjectID)
		require.True(t, repo.filter.IncludePlatform)
		require.False(t, repo.filter.AllProjects)
	})

	t.Run("平台 admin all_projects：豁免项目上下文", func(t *testing.T) {
		noProject := &shared.Principal{ActorKind: shared.ActorKindAdmin, IsPlatformAdmin: true}
		_, _, err := uc.List(auditCtx(noProject), server.AuditLogsQuery{AllProjects: true})
		require.NoError(t, err)
		require.True(t, repo.filter.AllProjects)
		require.Empty(t, repo.filter.ProjectID)
	})

	t.Run("无凭证拒绝", func(t *testing.T) {
		_, _, err := uc.List(context.Background(), server.AuditLogsQuery{})
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})
}
