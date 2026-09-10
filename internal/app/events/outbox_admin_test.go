package events_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	appevents "github.com/torchwoodcloud/torchwood/internal/app/events"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	domainprojects "github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stubOutboxRepo struct {
	listFn   func(ctx context.Context, projectID string, pageSize int32, pageToken string) ([]domainevents.DeadLetter, int64, string, error)
	replayFn func(ctx context.Context, eventID, projectID string) error
}

func (s *stubOutboxRepo) ListDeadLetters(ctx context.Context, projectID string, pageSize int32, pageToken string) ([]domainevents.DeadLetter, int64, string, error) {
	if s.listFn != nil {
		return s.listFn(ctx, projectID, pageSize, pageToken)
	}
	return nil, 0, "", nil
}
func (s *stubOutboxRepo) ReplayDeadLetter(ctx context.Context, eventID, projectID string) error {
	if s.replayFn != nil {
		return s.replayFn(ctx, eventID, projectID)
	}
	return nil
}

type stubProjectRepo struct {
	projects map[string]*domainprojects.Project
}

func (s *stubProjectRepo) CreateProject(ctx context.Context, p *domainprojects.Project) error {
	return nil
}
func (s *stubProjectRepo) GetProject(ctx context.Context, id string) (*domainprojects.Project, error) {
	if s == nil || s.projects == nil {
		return &domainprojects.Project{ID: id, Status: "active"}, nil
	}
	if p, ok := s.projects[id]; ok {
		return p, nil
	}
	return nil, nil
}
func (s *stubProjectRepo) GetProjectByName(ctx context.Context, name string) (*domainprojects.Project, error) {
	return nil, nil
}
func (s *stubProjectRepo) ListProjects(ctx context.Context) ([]domainprojects.Project, error) {
	return nil, nil
}
func (s *stubProjectRepo) UpdateProject(ctx context.Context, p *domainprojects.Project) error {
	return nil
}
func (s *stubProjectRepo) DeleteProject(ctx context.Context, id string) error { return nil }
func (s *stubProjectRepo) DeleteProjectControlPlaneRows(ctx context.Context, projectID string) error {
	return nil
}

func adminCtx(projectID string) context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindAdmin,
		AdminID:   "admin-1",
		Roles:     []string{"owner"},
		ProjectID: projectID,
	})
}

func TestOutboxAdmin_ListDeadLetters_ProjectMismatch(t *testing.T) {
	repo := &stubOutboxRepo{}
	uc := appevents.NewOutboxAdmin(repo, nil)
	// 决策 v8：越权统一 NotFound 防枚举（错误码总则）。
	ctx := adminCtx("p1")
	_, _, _, err := uc.ListDeadLetters(ctx, "p2", 10, "")
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestOutboxAdmin_ReplayDeadLetter_ProjectMismatch(t *testing.T) {
	repo := &stubOutboxRepo{}
	uc := appevents.NewOutboxAdmin(repo, nil)
	ctx := adminCtx("p1")
	err := uc.ReplayDeadLetter(ctx, "e1", "p2")
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestOutboxAdmin_ReplayDeadLetter_RequiresProjectID(t *testing.T) {
	repo := &stubOutboxRepo{}
	uc := appevents.NewOutboxAdmin(repo, nil)
	ctx := adminCtx("p1")
	err := uc.ReplayDeadLetter(ctx, "e1", "")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestOutboxAdmin_ListDeadLetters_Suspended(t *testing.T) {
	repo := &stubOutboxRepo{}
	projRepo := &stubProjectRepo{projects: map[string]*domainprojects.Project{"p1": {ID: "p1", Status: "suspended"}}}
	uc := appevents.NewOutboxAdmin(repo, projRepo)
	ctx := adminCtx("p1")
	_, _, _, err := uc.ListDeadLetters(ctx, "p1", 10, "")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestOutboxAdmin_ListDeadLetters_ActivePasses(t *testing.T) {
	called := false
	repo := &stubOutboxRepo{listFn: func(ctx context.Context, projectID string, pageSize int32, pageToken string) ([]domainevents.DeadLetter, int64, string, error) {
		called = true
		return nil, 0, "", nil
	}}
	projRepo := &stubProjectRepo{projects: map[string]*domainprojects.Project{"p1": {ID: "p1", Status: "active"}}}
	uc := appevents.NewOutboxAdmin(repo, projRepo)
	ctx := adminCtx("p1")
	_, _, _, err := uc.ListDeadLetters(ctx, "p1", 10, "")
	require.NoError(t, err)
	require.True(t, called)
}

func TestOutboxAdmin_ReplayDeadLetter_Suspended(t *testing.T) {
	repo := &stubOutboxRepo{}
	projRepo := &stubProjectRepo{projects: map[string]*domainprojects.Project{"p1": {ID: "p1", Status: "suspended"}}}
	uc := appevents.NewOutboxAdmin(repo, projRepo)
	ctx := adminCtx("p1")
	err := uc.ReplayDeadLetter(ctx, "e1", "p1")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestOutboxAdmin_ReplayDeadLetter_ActivePasses(t *testing.T) {
	called := false
	repo := &stubOutboxRepo{replayFn: func(ctx context.Context, eventID, projectID string) error {
		called = true
		return nil
	}}
	projRepo := &stubProjectRepo{projects: map[string]*domainprojects.Project{"p1": {ID: "p1", Status: "active"}}}
	uc := appevents.NewOutboxAdmin(repo, projRepo)
	ctx := adminCtx("p1")
	err := uc.ReplayDeadLetter(ctx, "e1", "p1")
	require.NoError(t, err)
	require.True(t, called)
}
