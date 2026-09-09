package functions

import (
	"context"
	"sync"
	"time"

	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
)

// ——函数/变量短 TTL 进程内缓存（P0.5 热路径 DB 清账，设计 §6 约束③）——
//
// 键 (projectID, functionID)，TTL 30s。失效语义：同进程的
// SetVariables/UpdateFunction/SetFunctionScopes/CreateDeployment/
// DeleteDeployment/DeleteFunction 即时失效；跨实例（server/worker 各自
// 进程）最多 30s 收敛——池策略与变量在窗口内的陈旧是可接受取舍（执行
// 语义按各自快照推进，不会跨快照拼接）。

const cacheTTL = 30 * time.Second

type fnCacheKey struct {
	projectID  string
	functionID string
}

type fnCacheEntry struct {
	fn        *domainfunctions.Function
	vars      map[string]string
	haveVars  bool
	expiresAt time.Time
}

type fnCache struct {
	mu      sync.Mutex
	entries map[fnCacheKey]*fnCacheEntry
	// trigCache 缓存「函数是否存在 http/cron 触发器」（P2 egress 分类热路径
	// 输入；同 TTL 30s）。值 = 存在任意触发器行（含禁用——禁用触发器可随时
	// 重新启用，按行存在性分类更保守且避免 enable 边界一致性问）。
	trigCache map[fnCacheKey]trigCacheEntry
	// now 可注入（测试）；nil = time.Now。
	// ttl 可注入（测试；<=0 = 禁用缓存直通 repo）。
	now func() time.Time
	ttl time.Duration
}

type trigCacheEntry struct {
	exists    bool
	expiresAt time.Time
}

func newFnCache() *fnCache {
	return &fnCache{entries: map[fnCacheKey]*fnCacheEntry{}, trigCache: map[fnCacheKey]trigCacheEntry{}, ttl: cacheTTL}
}

func (c *fnCache) nowFn() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// getFn 返回缓存的函数记录（含池策略/latest 指针投影）；未命中返回 nil。
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

// getVars 返回缓存的变量视图；haveVars=false = 未命中。
func (c *fnCache) getVars(projectID, functionID string) (map[string]string, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[fnCacheKey{projectID, functionID}]
	if !ok || c.nowFn().After(e.expiresAt) {
		return nil, false
	}
	if !e.haveVars {
		return nil, false
	}
	out := make(map[string]string, len(e.vars))
	for k, v := range e.vars {
		out[k] = v
	}
	return out, true
}

// store 以单条目同时缓存 fn 与 vars（执行路径一次 DB 往返各取其一，缓存
// 整体共享失效粒度 = 函数）。
func (c *fnCache) store(projectID, functionID string, fn *domainfunctions.Function, vars map[string]string, haveVars bool) {
	if c.ttl <= 0 || fn == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[fnCacheKey{projectID, functionID}] = &fnCacheEntry{
		fn:        fn,
		vars:      vars,
		haveVars:  haveVars,
		expiresAt: c.nowFn().Add(c.ttl),
	}
}

// invalidate 即时失效（同进程写路径调用；跨实例靠 TTL 收敛）。
func (c *fnCache) invalidate(projectID, functionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, fnCacheKey{projectID, functionID})
	c.invalidateTriggers(projectID, functionID)
}

// getTriggers 返回缓存的触发器存在性（have=false = 未命中）。
func (c *fnCache) getTriggers(projectID, functionID string) (exists, have bool) {
	if c.ttl <= 0 {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.trigCache[fnCacheKey{projectID, functionID}]
	if !ok || c.nowFn().After(e.expiresAt) {
		return false, false
	}
	return e.exists, true
}

// storeTriggers 缓存触发器存在性（CreateFunctionTrigger/DeleteTrigger/
// DeleteFunction 写路径即时失效——invalidate 已覆盖）。
func (c *fnCache) storeTriggers(projectID, functionID string, exists bool) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.trigCache[fnCacheKey{projectID, functionID}] = trigCacheEntry{
		exists:    exists,
		expiresAt: c.nowFn().Add(c.ttl),
	}
}

// invalidateTriggers 失效触发器存在性缓存。
func (c *fnCache) invalidateTriggers(projectID, functionID string) {
	delete(c.trigCache, fnCacheKey{projectID, functionID})
}

// hasTriggersCached 报告函数是否存在 http/cron 触发器行（egress 分类的
// 触发器维度输入；30s 缓存摊薄热路径 DB 往返）。triggers 未装配时按
// false 处理——单函数属性误判只影响网络选择保守性，不影响正确性。
func (f *Functions) hasTriggersCached(ctx context.Context, projectID, functionID string) bool {
	if f.triggers == nil {
		return false
	}
	if exists, ok := f.cache.getTriggers(projectID, functionID); ok {
		return exists
	}
	trgs, err := f.triggers.ListTriggers(ctx, projectID, functionID)
	if err != nil {
		// 查询失败不阻断执行：按 false 处理（可信网络——分类错误退化为
		// 更宽网络，fail-open 仅此一处且由 30s 重试收敛）。
		return false
	}
	exists := len(trgs) > 0
	f.cache.storeTriggers(projectID, functionID, exists)
	return exists
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
	vars, haveVars := f.cache.getVars(projectID, functionID)
	f.cache.store(projectID, functionID, fn, vars, haveVars)
	return fn, nil
}

// getCachedVariables 带缓存的 GetVariables（执行热路径用）。
func (f *Functions) getCachedVariables(ctx context.Context, projectID, functionID string) (map[string]string, error) {
	if vars, ok := f.cache.getVars(projectID, functionID); ok {
		return vars, nil
	}
	vars, err := f.repo.GetVariables(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	fn := f.cache.getFn(projectID, functionID)
	if fn == nil {
		// 函数未入缓存时不为变量单独建条目（保持单条目粒度）。
		return vars, nil
	}
	f.cache.store(projectID, functionID, fn, vars, true)
	return vars, nil
}
