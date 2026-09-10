package idgen_test

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/idgen"
	infraidgen "github.com/torchwoodcloud/torchwood/internal/infra/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	pkgidgen "github.com/torchwoodcloud/torchwood/pkg/idgen"
)

func TestService_NewID_RandomRedisSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	cfg := &config.AppConfig{
		Idgen: &config.IdGen{
			Random: &config.IdGen_Random{
				Length:         8,
				Charset:        "numeric",
				RedisKeyPrefix: "Torchwood:id:random",
				MaxRetries:     10,
			},
		},
	}
	svc, err := infraidgen.NewService(cfg, rdb, stubProjectRepo{settings: map[string]any{"idgen.users": "random"}})
	require.NoError(t, err)

	id1, err := svc.NewID(ctx, "proj-1", idgen.ResourceUsers)
	require.NoError(t, err)
	require.Len(t, id1, 8)

	id2, err := svc.NewID(ctx, "proj-1", idgen.ResourceUsers)
	require.NoError(t, err)
	require.Len(t, id2, 8)
	require.NotEqual(t, id1, id2)

	setKey := "Torchwood:id:random:proj-1:users"
	require.True(t, mr.Exists(setKey))
	n, err := mr.SCard(setKey)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

func TestService_NewID_RandomRequiresRedis(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cfg := &config.AppConfig{
		Idgen: &config.IdGen{
			Random: &config.IdGen_Random{Length: 8},
		},
	}
	svc, err := infraidgen.NewService(cfg, nil, stubProjectRepo{settings: map[string]any{"idgen.users": "random"}})
	require.NoError(t, err)

	_, err = svc.NewID(ctx, "proj-1", idgen.ResourceUsers)
	require.ErrorIs(t, err, pkgidgen.ErrRandomRedisRequired)
}

func TestService_NewID_RandomRetriesOnCollision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	setKey := "Torchwood:id:random:proj-1:users"
	require.NoError(t, rdb.SAdd(ctx, setKey, "11111111").Err())

	cfg := &config.AppConfig{
		Idgen: &config.IdGen{
			Random: &config.IdGen_Random{
				Length:         8,
				Charset:        "numeric",
				RedisKeyPrefix: "Torchwood:id:random",
				MaxRetries:     20,
			},
		},
	}
	svc, err := infraidgen.NewService(cfg, rdb, stubProjectRepo{settings: map[string]any{"idgen.users": "random"}})
	require.NoError(t, err)

	id, err := svc.NewID(ctx, "proj-1", idgen.ResourceUsers)
	require.NoError(t, err)
	require.NotEqual(t, "11111111", id)
}

func (r stubProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error { return nil }
