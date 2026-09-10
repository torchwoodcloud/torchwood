package servergrpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
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

// stubSettingsWriter 是最小 projects.SettingsWriter 桩（记录键值供断言）。
type stubSettingsWriter struct {
	saved map[string]map[string]any // projectID → key → value
}

func newStubSettingsWriter() *stubSettingsWriter {
	return &stubSettingsWriter{saved: map[string]map[string]any{}}
}

func (w *stubSettingsWriter) SetProjectSetting(_ context.Context, projectID, key string, value any) error {
	m, ok := w.saved[projectID]
	if !ok {
		m = map[string]any{}
		w.saved[projectID] = m
	}
	if value == nil {
		delete(m, key)
		return nil
	}
	m[key] = value
	return nil
}

// newTestProjectsService 组装 handler（UpdateProject 只依赖 projectRepo，
// docDB/db 传 nil）。
func newTestProjectsService(repo *stubProjectRepo) *ProjectsService {
	return newTestProjectsServiceWithSettings(repo, nil)
}

func newTestProjectsServiceWithSettings(repo *stubProjectRepo, settings projects.SettingsWriter) *ProjectsService {
	uc := appserver.NewProjects(repo, nil, nil, nil, nil, settings)
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

// ---- OAuth 重定向白名单 ----

func TestProjectsService_UpdateOAuthRedirectAllowlist_HappyPath(t *testing.T) {
	repo := &stubProjectRepo{project: &projects.Project{
		ID: "p1", Name: "P1", Status: "active",
		Settings: map[string]any{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	settings := newStubSettingsWriter()
	s := newTestProjectsServiceWithSettings(repo, settings)

	_, err := s.UpdateOAuthRedirectAllowlist(projectPrincipalCtx("", true),
		&serverv1.UpdateOAuthRedirectAllowlistRequest{
			ProjectId: "p1",
			Urls:      []string{"https://app.example.com", " http://localhost:5173 "},
		})
	require.NoError(t, err)
	// 写入通道：trim 后经 SettingsWriter 落库。
	require.Equal(t, []string{"https://app.example.com", "http://localhost:5173"},
		settings.saved["p1"][projects.SettingsKeyOAuthAllowedRedirectURLs])

	// 投影通道：回读行带 settings 时 Project 响应含白名单（真实实现回读 DB）。
	repo.project.Settings = map[string]any{
		projects.SettingsKeyOAuthAllowedRedirectURLs: []any{"https://app.example.com", "http://localhost:5173"},
	}
	p, err := s.GetProject(projectPrincipalCtx("", true), &serverv1.GetProjectRequest{Id: "p1"})
	require.NoError(t, err)
	require.Equal(t, []string{"https://app.example.com", "http://localhost:5173"},
		p.OauthAllowedRedirectUrls)
}

func TestProjectsService_UpdateOAuthRedirectAllowlist_RequiresPlatformAdmin(t *testing.T) {
	s := newTestProjectsServiceWithSettings(&stubProjectRepo{project: &projects.Project{
		ID: "p1", Name: "P1", Status: "active",
	}}, newStubSettingsWriter())
	_, err := s.UpdateOAuthRedirectAllowlist(projectPrincipalCtx("p1", false),
		&serverv1.UpdateOAuthRedirectAllowlistRequest{ProjectId: "p1", Urls: []string{"https://app.example.com"}})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestProjectsService_UpdateOAuthRedirectAllowlist_Missing(t *testing.T) {
	s := newTestProjectsServiceWithSettings(&stubProjectRepo{}, newStubSettingsWriter())
	_, err := s.UpdateOAuthRedirectAllowlist(projectPrincipalCtx("", true),
		&serverv1.UpdateOAuthRedirectAllowlistRequest{ProjectId: "missing", Urls: []string{"https://app.example.com"}})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestProjectsService_UpdateOAuthRedirectAllowlist_InvalidURL(t *testing.T) {
	s := newTestProjectsServiceWithSettings(&stubProjectRepo{project: &projects.Project{
		ID: "p1", Name: "P1", Status: "active",
	}}, newStubSettingsWriter())
	_, err := s.UpdateOAuthRedirectAllowlist(projectPrincipalCtx("", true),
		&serverv1.UpdateOAuthRedirectAllowlistRequest{ProjectId: "p1", Urls: []string{"ftp://evil.example.com"}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
