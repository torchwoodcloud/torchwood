package testutil

import (
	"context"
	"sync"
	"time"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FakeRateLimiter 是 domainauth.RateLimiter 端口的内存假实现：按 key 固定
// 窗口计数，count 超过阈值时返回 ResourceExhausted；Err 非 nil 时模拟后端
// 故障（fail-closed）。阈值优先用 f.Limit（测试注入），未设置时沿用调用方
// 传入的 limit，语义与 RedisRateLimiter 一致。
type FakeRateLimiter struct {
	mu     sync.Mutex
	Limit  int
	Err    error
	counts map[string]int
}

var _ domainauth.RateLimiter = (*FakeRateLimiter)(nil)

func (f *FakeRateLimiter) Allow(_ context.Context, key string, limit int, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts == nil {
		f.counts = make(map[string]int)
	}
	f.counts[key]++
	if f.Err != nil {
		return f.Err
	}
	threshold := f.Limit
	if threshold <= 0 {
		threshold = limit
	}
	if threshold > 0 && f.counts[key] > threshold {
		return status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	return nil
}

// Counts 返回各 key 的累计计数快照。
func (f *FakeRateLimiter) Counts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.counts))
	for k, v := range f.counts {
		out[k] = v
	}
	return out
}
