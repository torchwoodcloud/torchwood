package functions

import (
	"context"
	"sync"
	"time"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
)

// ——函数短 TTL 进程内缓存（P0.5 热路径 DB 清账，设计 §6 约束③）——
//
// 键 (projectID, functionID)，TTL 30s。失效语义：同进程的
// UpdateFunction/SetFunctionScopes/RotateFunctionSecret/DeleteFunction 即时
// 失效；跨实例（server/worker 各自进程）最多 30s 收敛——target 配置在窗口
// 内的陈旧是可接受取舍（v4：变量面随执行面外包退役，D9——缓存不再承载
// variables 视图；调用双轴分流按 trigger_source 列值，无查询面）。

const cacheTTL = 30 * time.Second

type fnCacheKey struct {
	projectID  string
	functionID string
}

type fnCacheEntry struct {
	fn        *domainfunctions.Function
	expiresAt time.Time
}

type fnCache struct {
	mu      sync.Mutex
	entries map[fnCacheKey]*fnCacheEntry
	// now 可注入（测试）；nil = time.Now。
	// ttl 可注入（测试；<=0 = 禁用缓存直通 repo）。
	now func() time.Time
	ttl time.Duration
}

func newFnCache() *fnCache {
	return &fnCache{entries: map[fnCacheKey]*fnCacheEntry{}, ttl: cacheTTL}
}

func (c *fnCache) nowFn() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// getFn 返回缓存的函数记录（含 target 投影）；未命中返回 nil。
func (c *fnCache) getFn(projectID, functionID string) *domainfunctions.Function {
	if c.ttl <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[fnCacheKey{projectID, functionID}]
	if !ok || c.nowFn().After(e.expiresAt) {
		return nil
	}
	fn := *e.fn
	return &fn
}

// store 缓存函数记录（执行路径一次 DB 往返，缓存粒度 = 函数）。
func (c *fnCache) store(projectID, functionID string, fn *domainfunctions.Function) {
	if c.ttl <= 0 || fn == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[fnCacheKey{projectID, functionID}] = &fnCacheEntry{
		fn:        fn,
		expiresAt: c.nowFn().Add(c.ttl),
	}
}

// invalidate 即时失效（同进程写路径调用；跨实例靠 TTL 收敛）。
func (c *fnCache) invalidate(projectID, functionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, fnCacheKey{projectID, functionID})
}

// getCachedFunction 带缓存的 GetFunction（执行热路径用；写路径直连 repo）。
func (f *Functions) getCachedFunction(ctx context.Context, projectID, functionID string) (*domainfunctions.Function, error) {
	if fn := f.cache.getFn(projectID, functionID); fn != nil {
		return fn, nil
	}
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil || fn == nil {
		return fn, err
	}
	f.cache.store(projectID, functionID, fn)
	return fn, nil
}
