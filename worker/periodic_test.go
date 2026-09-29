package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ——Periodic 骨架单测：循环/启跑/延迟/预算/失败日志语义——
// （captureHandler 复用 consume_test.go 的日志捕获桩）

// TestPeriodic_TickerTriggersRounds 短 interval 注入：周期触发 fn，ctx 取消
// 后优雅退出（其余作业测试同款时序，此处覆盖骨架本体）。
func TestPeriodic_TickerTriggersRounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	p := NewPeriodic("test-loop", 20*time.Millisecond, 10*time.Millisecond, func(context.Context) error {
		calls.Add(1)
		return nil
	}, slog.New(slog.DiscardHandler))

	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.GreaterOrEqual(t, calls.Load(), int32(3), "ticker 应周期触发 fn")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Start 未在 ctx 取消时退出")
	}
}

// TestPeriodic_RunAtStart 启动即跑一轮（不等待首个 interval）。
func TestPeriodic_RunAtStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	p := NewPeriodic("test-runatstart", time.Hour, time.Minute, func(context.Context) error {
		calls.Add(1)
		return nil
	}, slog.New(slog.DiscardHandler)).RunAtStart()

	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	deadline := time.Now().Add(time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	require.Equal(t, int32(1), calls.Load(), "启动即跑一轮，不等首个 interval")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Start 未在 ctx 取消时退出")
	}
}

// TestPeriodic_AfterInitialDelay 首轮延迟后执行；interval 从启动时刻起算
// （对齐原 ChunkCleaner timer+ticker 并行语义——延迟窗口内 ticker 已在走）。
func TestPeriodic_AfterInitialDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	p := NewPeriodic("test-delay", time.Hour, time.Minute, func(context.Context) error {
		calls.Add(1)
		return nil
	}, slog.New(slog.DiscardHandler)).After(30 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	time.Sleep(120 * time.Millisecond)
	require.Equal(t, int32(1), calls.Load(), "首轮在延迟点执行一次，1h interval 内不再触发")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Start 未在 ctx 取消时退出")
	}
}

// TestPeriodic_RoundBudgetContext 骨架派生的单轮 ctx：带预算 deadline、
// parent 取消不传播（WithoutCancel）、parent 已取消仍执行在途轮次。
// Err 在 fn 调用时点取样——runOnce 返回后派生 ctx 已被自己的 defer cancel() 关闭。
func TestPeriodic_RoundBudgetContext(t *testing.T) {
	var lastCtx context.Context
	var errAtCall error
	p := NewPeriodic("test-budget", time.Hour, 500*time.Millisecond, func(ctx context.Context) error {
		lastCtx = ctx
		errAtCall = ctx.Err()
		return nil
	}, slog.New(slog.DiscardHandler))

	before := time.Now()
	p.runOnce(context.Background())
	deadline, ok := lastCtx.Deadline()
	require.True(t, ok, "runOnce 必须把带预算的 ctx 传给 fn")
	require.LessOrEqual(t, deadline.Sub(before), 500*time.Millisecond+10*time.Second,
		"预算不超过 budget 上界（10s 余量为调度抖动）")

	// WithoutCancel：parent 已取消，在途轮次仍按预算执行。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	p.runOnce(canceled)
	require.NoError(t, errAtCall, "parent 取消不传播到轮次 ctx")
	dl, ok := lastCtx.Deadline()
	require.True(t, ok)
	require.False(t, dl.IsZero(), "取消传播被隔离后预算仍然生效")
}

// TestPeriodic_RoundFailureLogging fn 返回 error：parent 存活时骨架记一次
// Error；关停（parent 已取消）引发的失败不记日志。
func TestPeriodic_RoundFailureLogging(t *testing.T) {
	handler := &captureHandler{}
	p := NewPeriodic("test-fail", time.Hour, time.Minute, func(context.Context) error {
		return errors.New("boom")
	}, slog.New(handler))

	p.runOnce(context.Background())
	require.Len(t, handler.records, 1, "失败轮次记一条日志")
	require.Equal(t, slog.LevelError, handler.records[0].Level)
	require.Contains(t, handler.records[0].Message, "test-fail round failed")

	handler.records = nil
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	p.runOnce(canceled)
	require.Empty(t, handler.records, "关停引发的失败不记日志")
}

// TestPeriodic_ConstructorGuards interval/budget 非正数构造期 fail-fast。
func TestPeriodic_ConstructorGuards(t *testing.T) {
	fn := func(context.Context) error { return nil }
	logger := slog.New(slog.DiscardHandler)
	require.Panics(t, func() { NewPeriodic("bad", 0, time.Minute, fn, logger) })
	require.Panics(t, func() { NewPeriodic("bad", time.Minute, 0, fn, logger) })
}

// TestPeriodic_NilLoggerFallsBackToDefault nil logger 回退 slog.Default
// （全 worker 作业构造器同款约定）。
func TestPeriodic_NilLoggerFallsBackToDefault(t *testing.T) {
	p := NewPeriodic("nil-logger", time.Minute, time.Second, func(context.Context) error { return nil }, nil)
	require.NotNil(t, p.logger)
	require.True(t, strings.Contains(p.name, "nil-logger"))
}
