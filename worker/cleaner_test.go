package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeCleaner 记录 CleanupOrphanChunks 调用次数（可配置失败）。
type fakeCleaner struct {
	calls atomic.Int32
	fail  atomic.Bool
}

func (f *fakeCleaner) CleanupOrphanChunks(ctx context.Context) (int, error) {
	f.calls.Add(1)
	if f.fail.Load() {
		return 0, errors.New("boom")
	}
	return 1, nil
}

// TestChunkCleaner_TickerTriggersCleanup 短 interval 注入：Start 后周期触发
// CleanupOrphanChunks，ctx 取消后优雅退出。
func TestChunkCleaner_TickerTriggersCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &fakeCleaner{}
	c := NewChunkCleaner(fake, slog.New(slog.DiscardHandler))
	c.initialDelay = 0
	c.interval = 20 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	// 等待至少 3 次调用（delay 0 立即 + ticker 周期）。
	deadline := time.Now().Add(2 * time.Second)
	for fake.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.GreaterOrEqual(t, fake.calls.Load(), int32(3), "ticker 应周期触发清理")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Start 未在 ctx 取消时退出")
	}
}

// TestChunkCleaner_FailureIsLoggedNotFatal 清理失败仅记日志，ticker 继续。
func TestChunkCleaner_FailureIsLoggedNotFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &fakeCleaner{}
	fake.fail.Store(true)
	c := NewChunkCleaner(fake, slog.New(slog.DiscardHandler))
	c.initialDelay = 0
	c.interval = 10 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	deadline := time.Now().Add(1 * time.Second)
	for fake.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.GreaterOrEqual(t, fake.calls.Load(), int32(3), "失败后 ticker 应继续触发")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Start 未在 ctx 取消时退出")
	}
}
