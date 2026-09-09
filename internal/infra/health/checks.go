// Package health 提供依赖健康检查器：单依赖探测（lynx.Checker）与
// 并行探测集合（Details），供 gRPC/HTTP 健康端点与 lynx readiness 使用。
package health

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/redis/go-redis/v9"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

// DefaultTimeout 是未显式指定超时时的探测超时。
const DefaultTimeout = 2 * time.Second

// resultCacheTTL 是 Details 探测结果的快照缓存时长：gRPC health 端点被
// 外部轮询时避免每次请求都打穿到数据库/Redis/MinIO。
const resultCacheTTL = 10 * time.Second

// DependencyChecker 单依赖探测；实现 lynx.Checker（CheckHealth() error，
// 无 ctx，超时在内部自控）。
type DependencyChecker struct {
	Name    string
	Timeout time.Duration // 默认 2s
	Check   func(ctx context.Context) error
}

// CheckHealth 以独立超时执行探测，实现 lynx.Checker。
func (c *DependencyChecker) CheckHealth() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout())
	defer cancel()
	return c.Check(ctx)
}

func (c *DependencyChecker) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

var _ lynx.Checker = (*DependencyChecker)(nil)

// Checkers 是依赖集合（只读，并发安全）。
type Checkers struct {
	deps []*DependencyChecker

	// CacheTTL 覆盖 Details 结果快照的缓存时长；0 使用默认 resultCacheTTL。
	CacheTTL time.Duration

	mu       sync.Mutex
	cached   []*serverv1.DependencyStatus
	cachedAt time.Time
	// inFlight 非 nil 表示有刷新在途，后续调用者等待该 channel 关闭后
	// 直接读取刷新结果，避免缓存失效时并发打穿下游依赖（singleflight）。
	inFlight chan struct{}
}

func (c *Checkers) cacheTTL() time.Duration {
	if c.CacheTTL > 0 {
		return c.CacheTTL
	}
	return resultCacheTTL
}

// snapshot 返回当前缓存的一份拷贝；调用方须持有 c.mu。
func (c *Checkers) snapshot() []*serverv1.DependencyStatus {
	out := make([]*serverv1.DependencyStatus, len(c.cached))
	copy(out, c.cached)
	return out
}

// NewCheckers 构建依赖探测集合：Postgres（PingContext）、Redis（Ping）、
// MinIO（BucketExists）。
func NewCheckers(db *clients.Database, rdb *redis.Client, obj storage.ObjectStore) *Checkers {
	return &Checkers{
		deps: []*DependencyChecker{
			{Name: "postgres", Check: db.PingContext},
			{Name: "redis", Check: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
			{Name: "minio", Check: obj.Ping},
		},
	}
}

// NewCheckersFromDeps 构建显式依赖集合（测试与扩展场景使用）。
func NewCheckersFromDeps(deps ...*DependencyChecker) *Checkers {
	return &Checkers{deps: deps}
}

// Deps 返回全部依赖（供 lynx.HealthCheckersFunc 使用）。
func (c *Checkers) Deps() []lynx.Checker {
	deps := make([]lynx.Checker, 0, len(c.deps))
	for _, d := range c.deps {
		deps = append(deps, d)
	}
	return deps
}

// Details 并行探测各依赖（各自超时 + recover 兜底），返回逐依赖状态；
// 探测失败不影响其他依赖。结果带快照缓存（默认 10s，可用 CacheTTL 覆盖）：
// TTL 内重复调用直接返回上次结果；缓存失效时并发调用共享同一次刷新
// （singleflight），不会各自打穿下游依赖（readiness 语义仍由 lynx 聚合
// 实时决定）。
func (c *Checkers) Details(ctx context.Context) []*serverv1.DependencyStatus {
	c.mu.Lock()
	if c.cached != nil && time.Since(c.cachedAt) < c.cacheTTL() {
		out := c.snapshot()
		c.mu.Unlock()
		return out
	}
	if c.inFlight != nil {
		done := c.inFlight
		c.mu.Unlock()
		select {
		case <-done:
			// 刷新者已写入新快照，直接复用。
		case <-ctx.Done():
			// 等待期间调用方取消：返回当前（可能已过期）快照而非空结果。
		}
		c.mu.Lock()
		out := c.snapshot()
		c.mu.Unlock()
		return out
	}
	done := make(chan struct{})
	c.inFlight = done
	c.mu.Unlock()

	results := make([]*serverv1.DependencyStatus, len(c.deps))
	var wg sync.WaitGroup
	for i, dep := range c.deps {
		wg.Go(func() {
			results[i] = checkOne(dep, ctx)
		})
	}
	wg.Wait()

	c.mu.Lock()
	c.cached = results
	c.cachedAt = time.Now()
	c.inFlight = nil
	c.mu.Unlock()
	close(done)
	return results
}

// checkOne 探测单个依赖并返回状态；panic 兜底为 unavailable。
func checkOne(dep *DependencyChecker, ctx context.Context) (st *serverv1.DependencyStatus) {
	st = &serverv1.DependencyStatus{Name: dep.Name, Status: "ok"}
	defer func() {
		if r := recover(); r != nil {
			st.Status = "unavailable"
			st.Error = fmt.Sprintf("panic: %v", r)
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, dep.timeout())
	defer cancel()
	if err := dep.Check(probeCtx); err != nil {
		st.Status = "unavailable"
		st.Error = err.Error()
	}
	return st
}
