package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/sdk/go/internal/conn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// slowHealth 是可配置延迟/失败的 HealthService fake：Check 记录调用时观察
// 到的 ctx deadline，按 failTimes 先返回 Unavailable 再成功（验证默认重试）。
type slowHealth struct {
	serverv1.UnimplementedHealthServiceServer

	mu          sync.Mutex
	delay       time.Duration
	failTimes   int
	calls       int
	hasDeadline []bool
	deadlines   []time.Time // 与 hasDeadline 一一对应；无 deadline 时为零值
}

func (h *slowHealth) Check(ctx context.Context, _ *serverv1.HealthCheckRequest) (*serverv1.HealthCheckResponse, error) {
	h.mu.Lock()
	h.calls++
	if dl, ok := ctx.Deadline(); ok {
		h.hasDeadline = append(h.hasDeadline, true)
		h.deadlines = append(h.deadlines, dl)
	} else {
		h.hasDeadline = append(h.hasDeadline, false)
		h.deadlines = append(h.deadlines, time.Time{})
	}
	fail := h.failTimes > 0
	if fail {
		h.failTimes--
	}
	delay := h.delay
	h.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	if fail {
		return nil, status.Error(codes.Unavailable, "flaky")
	}
	return &serverv1.HealthCheckResponse{Status: "ok"}, nil
}

// snapshot 返回调用次数与最近一次观察到的 deadline 剩余预算（无 deadline 为 0）。
func (h *slowHealth) snapshot() (calls int, lastBudget time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.deadlines); n > 0 && h.hasDeadline[n-1] {
		return h.calls, time.Until(h.deadlines[n-1])
	}
	return h.calls, 0
}

// newSlowListener 注册 slowHealth 并启动 bufconn gRPC 服务，返回监听器。
func newSlowListener(t *testing.T, h *slowHealth) *bufconn.Listener {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	serverv1.RegisterHealthServiceServer(srv, h)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis
}

func TestDefaultTimeoutInjected(t *testing.T) {
	h := &slowHealth{}
	lis := newSlowListener(t, h)

	c := newTestClient(t, lis)
	_, err := c.Health.Check(context.Background())
	require.NoError(t, err)
	_, budget := h.snapshot()
	require.Greater(t, budget, time.Duration(0), "服务端应观察到注入的 deadline")
	require.LessOrEqual(t, budget, conn.DefaultTimeout, "默认兜底预算应等于 30s")
	require.Equal(t, 30*time.Second, conn.DefaultTimeout)
}

func TestWithTimeoutBoundsSlowCall(t *testing.T) {
	h := &slowHealth{delay: 2 * time.Second}
	lis := newSlowListener(t, h)

	c := newTestClient(t, lis, WithTimeout(50*time.Millisecond))
	start := time.Now()
	_, err := c.Health.Check(context.Background())
	require.Error(t, err)
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
	require.Less(t, time.Since(start), time.Second, "应在配置的兜底超时内返回而非等满服务端延迟")
}

func TestWithTimeoutRespectsCallerDeadline(t *testing.T) {
	h := &slowHealth{}
	lis := newSlowListener(t, h)

	// 调用方 deadline(1h) 远大于兜底(50ms)：尊重调用方 → 不得被压缩到 50ms。
	c := newTestClient(t, lis, WithTimeout(50*time.Millisecond))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancel()
	_, err := c.Health.Check(ctx)
	require.NoError(t, err)
	_, budget := h.snapshot()
	require.Greater(t, budget, 59*time.Minute, "已有 deadline 时不得被 WithTimeout 覆盖")
}

func TestUnavailableRetriedByDefault(t *testing.T) {
	h := &slowHealth{failTimes: 2}
	lis := newSlowListener(t, h)

	c := newTestClient(t, lis, WithTimeout(10*time.Second))
	resp, err := c.Health.Check(context.Background())
	require.NoError(t, err, "UNAVAILABLE 应被默认 retryPolicy 重试后成功")
	require.Equal(t, "ok", resp.Status)
	calls, _ := h.snapshot()
	require.Equal(t, 3, calls, "两次失败 + 一次成功 = 共 3 次尝试")
}

func TestUnavailableNotRetriedWhenDisabled(t *testing.T) {
	h := &slowHealth{failTimes: 2}
	lis := newSlowListener(t, h)

	c := newTestClient(t, lis, WithRetryDisabled())
	_, err := c.Health.Check(context.Background())
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
	calls, _ := h.snapshot()
	require.Equal(t, 1, calls, "关闭重试后只尝试一次")
}
