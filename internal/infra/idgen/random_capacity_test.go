package idgen

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	domainidgen "github.com/torchwoodcloud/torchwood/internal/domain/idgen"
	pkgidgen "github.com/torchwoodcloud/torchwood/pkg/idgen"
)

const capTestSetKey = "Torchwood:id:random:proj-1:users"

func newCapTestService(t *testing.T, mr *miniredis.Miniredis) *Service {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &Service{
		rdb: rdb,
		randomCfg: pkgidgen.RandomConfig{
			Length:     8,
			Charset:    pkgidgen.RandomCharsetNumeric,
			MaxRetries: 10,
		}.WithDefaults(),
		randomPrefix: "Torchwood:id:random",
	}
}

func seedSet(t *testing.T, mr *miniredis.Miniredis, n int) {
	t.Helper()
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	for i := 0; i < n; i++ {
		require.NoError(t, rdb.SAdd(ctx, capTestSetKey, fmt.Sprintf("seed-%d", i)).Err())
	}
}

func TestNextRandom_SetFullRejected(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	seedSet(t, mr, 3)

	old := maxRandomSetSize
	maxRandomSetSize = 3
	t.Cleanup(func() { maxRandomSetSize = old })

	_, err = newCapTestService(t, mr).nextRandom(context.Background(), "proj-1", domainidgen.ResourceUsers)
	require.ErrorIs(t, err, ErrRandomSetFull)

	// 拒绝生成不得写入新成员。
	n, err := mr.SCard(capTestSetKey)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}

func TestNextRandom_AtLimitRejected(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	seedSet(t, mr, 2)

	old := maxRandomSetSize
	maxRandomSetSize = 2
	t.Cleanup(func() { maxRandomSetSize = old })

	_, err = newCapTestService(t, mr).nextRandom(context.Background(), "proj-1", domainidgen.ResourceUsers)
	require.ErrorIs(t, err, ErrRandomSetFull)
}

func TestNextRandom_UnderLimitSucceeds(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	seedSet(t, mr, 2)

	old := maxRandomSetSize
	maxRandomSetSize = 3
	t.Cleanup(func() { maxRandomSetSize = old })

	svc := newCapTestService(t, mr)
	id, err := svc.nextRandom(context.Background(), "proj-1", domainidgen.ResourceUsers)
	require.NoError(t, err)
	require.Len(t, id, 8)

	n, err := mr.SCard(capTestSetKey)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}
