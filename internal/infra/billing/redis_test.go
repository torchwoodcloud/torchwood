package billing

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
)

func newTestCounter(t *testing.T) (*miniredis.Miniredis, *RedisCounter) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, NewRedisCounter(rdb)
}

func TestRedisCounterIncrTTLAndIdempotentRead(t *testing.T) {
	t.Parallel()
	mr, c := newTestCounter(t)
	ctx := context.Background()
	hour := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)

	require.NoError(t, c.IncrAt(ctx, "proj-1", domainbilling.MetricAPICalls, hour, 3))
	require.NoError(t, c.IncrAt(ctx, "proj-1", domainbilling.MetricAPICalls, hour, 2))
	got, err := c.Get(ctx, "proj-1", domainbilling.MetricAPICalls, hour)
	require.NoError(t, err)
	require.Equal(t, int64(5), got)

	key := usageKey("proj-1", domainbilling.MetricAPICalls, hour)
	ttl := mr.TTL(key)
	require.GreaterOrEqual(t, ttl, domainbilling.BucketTTL-time.Minute)
	require.LessOrEqual(t, ttl, domainbilling.BucketTTL)

	// 二次 INCR 不刷新 TTL（键仍接近 48h）。
	ttl2 := mr.TTL(key)
	require.GreaterOrEqual(t, ttl2, domainbilling.BucketTTL-time.Minute)
}

func TestRedisCounterListHourAndStorageSet(t *testing.T) {
	t.Parallel()
	_, c := newTestCounter(t)
	ctx := context.Background()
	hour := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)

	require.NoError(t, c.IncrAt(ctx, "p1", domainbilling.MetricAPICalls, hour, 10))
	require.NoError(t, c.IncrAt(ctx, "p1", domainbilling.MetricFunctionDurationMS, hour, 42))
	require.NoError(t, c.Set(ctx, "p1", domainbilling.MetricStorageBytes, hour, 1024))
	require.NoError(t, c.Set(ctx, "p1", domainbilling.MetricStorageBytes, hour, 2048)) // 快照覆盖

	buckets, err := c.ListHour(ctx, hour)
	require.NoError(t, err)
	got := map[string]int64{}
	for _, b := range buckets {
		require.Equal(t, "p1", b.ProjectID)
		require.Equal(t, hour, b.PeriodStart)
		got[b.Metric] = b.Value
	}
	require.Equal(t, int64(10), got[domainbilling.MetricAPICalls])
	require.Equal(t, int64(42), got[domainbilling.MetricFunctionDurationMS])
	require.Equal(t, int64(2048), got[domainbilling.MetricStorageBytes])
}

func TestRedisCounterSkipUnknownAndNonPositive(t *testing.T) {
	t.Parallel()
	_, c := newTestCounter(t)
	ctx := context.Background()
	hour := time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)
	require.NoError(t, c.IncrAt(ctx, "p1", "realtime_messages", hour, 9))
	require.NoError(t, c.IncrAt(ctx, "p1", domainbilling.MetricAPICalls, hour, 0))
	got, err := c.Get(ctx, "p1", domainbilling.MetricAPICalls, hour)
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
}
