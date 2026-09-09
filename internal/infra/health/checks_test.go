package health

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
)

func TestDetails_AllOK(t *testing.T) {
	c := &Checkers{deps: []*DependencyChecker{
		{Name: "a", Check: func(ctx context.Context) error { return nil }},
		{Name: "b", Check: func(ctx context.Context) error { return nil }},
	}}
	st := c.Details(context.Background())
	require.Len(t, st, 2)
	for _, d := range st {
		require.Equal(t, "ok", d.GetStatus())
		require.Empty(t, d.GetError())
	}
}

func TestDetails_FailurePropagation(t *testing.T) {
	boom := errors.New("boom")
	c := &Checkers{deps: []*DependencyChecker{
		{Name: "a", Check: func(ctx context.Context) error { return nil }},
		{Name: "b", Check: func(ctx context.Context) error { return boom }},
	}}
	st := c.Details(context.Background())
	require.Equal(t, "ok", st[0].GetStatus())
	require.Empty(t, st[0].GetError())
	require.Equal(t, "unavailable", st[1].GetStatus())
	require.Equal(t, "boom", st[1].GetError())
}

func TestDetails_Timeout(t *testing.T) {
	c := &Checkers{deps: []*DependencyChecker{
		{Name: "slow", Timeout: 50 * time.Millisecond, Check: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
	}}
	start := time.Now()
	st := c.Details(context.Background())
	require.Less(t, time.Since(start), 500*time.Millisecond)
	require.Equal(t, "unavailable", st[0].GetStatus())
	require.Equal(t, context.DeadlineExceeded.Error(), st[0].GetError())
}

func TestDetails_PanicRecover(t *testing.T) {
	c := &Checkers{deps: []*DependencyChecker{
		{Name: "panicky", Check: func(ctx context.Context) error { panic("kaboom") }},
	}}
	st := c.Details(context.Background())
	require.Equal(t, "unavailable", st[0].GetStatus())
	require.Contains(t, st[0].GetError(), "panic")
	require.Contains(t, st[0].GetError(), "kaboom")
}

func TestDependencyChecker_CheckHealthSelfTimeout(t *testing.T) {
	c := &DependencyChecker{Name: "x", Timeout: 50 * time.Millisecond, Check: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	start := time.Now()
	err := c.CheckHealth()
	require.Error(t, err)
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestDependencyChecker_CheckHealthOK(t *testing.T) {
	c := &DependencyChecker{Name: "x", Check: func(ctx context.Context) error { return nil }}
	require.NoError(t, c.CheckHealth())
}

func TestDependencyChecker_DefaultTimeout(t *testing.T) {
	c := &DependencyChecker{Name: "x", Check: func(ctx context.Context) error { return nil }}
	require.Equal(t, DefaultTimeout, c.timeout())
	require.Equal(t, DefaultTimeout, (&DependencyChecker{Name: "y", Timeout: -1}).timeout())
}

func TestCheckers_Deps(t *testing.T) {
	c := &Checkers{deps: []*DependencyChecker{
		{Name: "a", Check: func(ctx context.Context) error { return nil }},
	}}
	deps := c.Deps()
	require.Len(t, deps, 1)
	require.NoError(t, deps[0].CheckHealth())
}

func TestDetails_SingleflightOnCacheMiss(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	c := &Checkers{
		deps: []*DependencyChecker{
			{Name: "a", Check: func(ctx context.Context) error {
				calls.Add(1)
				<-release
				return nil
			}},
		},
	}

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := c.Details(context.Background())
			if len(st) != 1 || st[0].GetStatus() != "ok" {
				errs <- errors.New("unexpected details result")
			}
		}()
	}
	// 等待所有调用者到达并至少有一个开始探测。
	waitForCalls(t, &calls, 1)
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), calls.Load(), "concurrent cache-miss callers must share one refresh")
}

func TestDetails_CacheHitServesWithoutProbe(t *testing.T) {
	var calls atomic.Int64
	c := &Checkers{deps: []*DependencyChecker{
		{Name: "a", Check: func(ctx context.Context) error {
			calls.Add(1)
			return nil
		}},
	}}
	require.Len(t, c.Details(context.Background()), 1)
	require.Equal(t, int64(1), calls.Load())
	require.Len(t, c.Details(context.Background()), 1)
	require.Equal(t, int64(1), calls.Load(), "TTL 内不应重新探测")
}

func TestDetails_CacheTTLExpiryRefreshes(t *testing.T) {
	var calls atomic.Int64
	c := &Checkers{
		CacheTTL: 30 * time.Millisecond,
		deps: []*DependencyChecker{
			{Name: "a", Check: func(ctx context.Context) error {
				calls.Add(1)
				return nil
			}},
		},
	}
	require.Len(t, c.Details(context.Background()), 1)
	// J6-4：轮询替代固定 sleep——TTL(30ms) 过期后的首次调用必然触发重新探测。
	testutil.Eventually(t, 5*time.Second, func() bool {
		_ = c.Details(context.Background())
		return calls.Load() >= 2
	})
	require.Len(t, c.Details(context.Background()), 1)
	require.Equal(t, int64(2), calls.Load(), "TTL 过期后应重新探测")
}

func TestDetails_DefaultCacheTTL(t *testing.T) {
	c := &Checkers{}
	require.Equal(t, resultCacheTTL, c.cacheTTL())
	c.CacheTTL = 5 * time.Second
	require.Equal(t, 5*time.Second, c.cacheTTL())
}

func waitForCalls(t *testing.T, calls *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.GreaterOrEqual(t, calls.Load(), want, "probe did not start in time")
}
