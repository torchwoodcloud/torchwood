package servergrpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwooddev/torchwood/genproto/server/v1"
	appserver "github.com/torchwooddev/torchwood/internal/app/server"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubProjectRepo 是最小 projects.Repository 桩（仅覆盖测试路径）。
type stubProjectRepo struct {
	project *projects.Project
}

func (r *stubProjectRepo) CreateProject(_ context.Context, p *projects.Project) error {
	r.project = p
	return nil
}

func (r *stubProjectRepo) GetProject(_ context.Context, id string) (*projects.Project, error) {
	if r.project == nil || r.project.ID != id {
		return nil, nil
	}
	return r.project, nil
}

func (r *stubProjectRepo) GetProjectByName(_ context.Context, name string) (*projects.Project, error) {
	if r.project != nil && r.project.Name == name {
		return r.project, nil
	}
	return nil, nil
}

func (r *stubProjectRepo) ListProjects(context.Context) ([]projects.Project, error) {
	return nil, nil
}

func (r *stubProjectRepo) UpdateProject(_ context.Context, p *projects.Project) error {
	r.project = p
	return nil
}

func (r *stubProjectRepo) DeleteProject(context.Context, string) error                 { return nil }
func (r *stubProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error { return nil }

// newTestProjectsService 组装 handler（UpdateProject 只依赖 projectRepo，
// docDB/db 传 nil）。
func newTestProjectsService(repo *stubProjectRepo) *ProjectsService {
	uc := appserver.NewProjects(repo, nil, nil, nil, nil)
	return NewProjectsService(uc, appserver.NewInviteCodes(nil))
}

func projectPrincipalCtx(projectID string, platformAdmin bool) context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:         "admin-1",
		ActorKind:       shared.ActorKindAdmin,
		IsPlatformAdmin: platformAdmin,
		ProjectID:       projectID,
	})
}

func TestProjectsService_UpdateProject_WithoutPrincipal(t *testing.T) {
	s := newTestProjectsService(&stubProjectRepo{})
	name := "Renamed"
	_, err := s.UpdateProject(context.Background(), &serverv1.UpdateProjectRequest{
		Id: "p1", Name: &name,
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// TestProjectsService_GetProject_Missing: use-case 返回 nil,nil 时 handler
// 必须转 NotFound，不得返回 gRPC OK + 空响应（F4-5）。
func TestProjectsService_GetProject_Missing(t *testing.T) {
	s := newTestProjectsService(&stubProjectRepo{})

	_, err := s.GetProject(projectPrincipalCtx("", true), &serverv1.GetProjectRequest{Id: "missing"})
	require.Error(t, err)
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestProjectsService_GetProject_Found(t *testing.T) {
	s := newTestProjectsService(&stubProjectRepo{project: &projects.Project{
		ID: "p1", Name: "Project 1", Status: "active",
		Settings: map[string]any{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}})

	p, err := s.GetProject(projectPrincipalCtx("", true), &serverv1.GetProjectRequest{Id: "p1"})
	require.NoError(t, err)
	require.NotNil(t, p)
	require.Equal(t, "p1", p.Id)
}

func TestProjectsService_UpdateProject_HappyPath(t *testing.T) {
	repo := &stubProjectRepo{project: &projects.Project{
		ID: "p1", Name: "Old Name", Description: "old desc", Status: "active",
		Settings: map[string]any{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	s := newTestProjectsService(repo)

	name := "New Name"
	desc := "new desc"
	p, err := s.UpdateProject(projectPrincipalCtx("", true), &serverv1.UpdateProjectRequest{
		Id: "p1", Name: &name, Description: &desc,
	})
	require.NoError(t, err)
	require.NotNil(t, p)
	require.Equal(t, "p1", p.Id)
	require.Equal(t, "New Name", p.Name)
	require.Equal(t, "new desc", p.Description)
	require.Equal(t, "New Name", repo.project.Name)
}

func TestProjectsService_UpdateProject_OwnProjectForRestrictedAdmin(t *testing.T) {
	repo := &stubProjectRepo{project: &projects.Project{
		ID: "p1", Name: "Old Name", Status: "active",
		Settings: map[string]any{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	s := newTestProjectsService(repo)

	desc := "updated by restricted admin"
	p, err := s.UpdateProject(projectPrincipalCtx("p1", false), &serverv1.UpdateProjectRequest{
		Id: "p1", Description: &desc,
	})
	require.NoError(t, err)
	require.Equal(t, "updated by restricted admin", p.Description)
}

func TestProjectsService_DeleteProject_WithoutPrincipal(t *testing.T) {
	s := newTestProjectsService(&stubProjectRepo{})
	_, err := s.DeleteProject(context.Background(), &serverv1.GetProjectRequest{Id: "p1"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestProjectsService_DeleteProject_RequiresPlatformAdmin(t *testing.T) {
	s := newTestProjectsService(&stubProjectRepo{project: &projects.Project{
		ID: "p1", Name: "Project 1", Status: "active",
	}})
	_, err := s.DeleteProject(projectPrincipalCtx("p1", false), &serverv1.GetProjectRequest{Id: "p1"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestProjectsService_DeleteProject_Missing(t *testing.T) {
	s := newTestProjectsService(&stubProjectRepo{})
	_, err := s.DeleteProject(projectPrincipalCtx("", true), &serverv1.GetProjectRequest{Id: "missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
}
