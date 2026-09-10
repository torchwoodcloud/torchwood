package idgen_test

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/idgen"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	infraidgen "github.com/torchwoodcloud/torchwood/internal/infra/idgen"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

type stubProjectRepo struct {
	settings map[string]any
}

func (s stubProjectRepo) GetProject(context.Context, string) (*projects.Project, error) {
	return &projects.Project{ID: "proj-1", Settings: s.settings}, nil
}
func (stubProjectRepo) CreateProject(context.Context, *projects.Project) error { return nil }
func (stubProjectRepo) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (stubProjectRepo) ListProjects(context.Context) ([]projects.Project, error) { return nil, nil }
func (stubProjectRepo) UpdateProject(context.Context, *projects.Project) error   { return nil }
func (stubProjectRepo) DeleteProject(context.Context, string) error              { return nil }

func TestService_NewID_Sequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	cfg := &config.AppConfig{
		Idgen: &config.IdGen{
			DefaultStrategy: "uuid",
			Resources: &config.IdGen_Resources{
				Users: "sequence",
			},
		},
	}
	svc, err := infraidgen.NewService(cfg, rdb, stubProjectRepo{})
	require.NoError(t, err)

	id1, err := svc.NewID(ctx, "proj-1", idgen.ResourceUsers)
	require.NoError(t, err)
	require.Equal(t, "1", id1)
	id2, err := svc.NewID(ctx, "proj-1", idgen.ResourceUsers)
	require.NoError(t, err)
	require.Equal(t, "2", id2)
}

func TestService_NewID_ULID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cfg := &config.AppConfig{
		Idgen: &config.IdGen{
			Resources: &config.IdGen_Resources{Users: "ulid"},
		},
	}
	svc, err := infraidgen.NewService(cfg, nil, stubProjectRepo{})
	require.NoError(t, err)

	id, err := svc.NewID(ctx, "proj-1", idgen.ResourceUsers)
	require.NoError(t, err)
	require.Len(t, id, 26)
}

func TestService_NewID_Snowflake(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cfg := &config.AppConfig{
		Idgen: &config.IdGen{
			DefaultStrategy: "snowflake",
			Snowflake:       &config.IdGen_Snowflake{NodeId: 2},
		},
	}
	svc, err := infraidgen.NewService(cfg, nil, stubProjectRepo{settings: map[string]any{"idgen.users": "snowflake"}})
	require.NoError(t, err)

	sfID, err := svc.NewID(ctx, "proj-1", idgen.ResourceUsers)
	require.NoError(t, err)
	require.NotEmpty(t, sfID)
}
