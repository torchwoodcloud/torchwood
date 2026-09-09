// Package principalcache 是端用户 principal 的进程内短 TTL 缓存（P0.5 热路径
// DB 清账，设计 §6 约束③）：客户端鉴权一次原本要 4 次 DB 往返（session 校验
// + users.GetByID + LoadUserRoles 内重复 users.GetByID + memberships），缓存
// 命中时以一次 Redis 失效标记检查替代。
//
// 键：(projectID, sessionID, iat)——iat 变化（重新登录/重签）自然产生新键，
// 旧键随 TTL 过期。TTL 30s：**最坏吊销延迟 = TTL 30s**（登出/封禁路径写
// Redis 失效标记主动失效，见 InvalidateUser/InvalidateSession；标记检查
// 失败按 miss 处理回退 DB 实时校验，fail-closed）。该取舍是热路径 SLA 的
// 显式代价，注释与 08-functions.md 同步明示。
//
// 独立小包的原因：infra/auth（validator）与 SessionService 都要引用，且
// 避免与 app 层失效写点构成 import 环——失效写点在 infra/auth 内部
// （SessionService.DeleteSessionsByUser 是全部登出/封禁会话删除的单一咽喉）。
package principalcache

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
)

const (
	// TTL 是缓存条目存活时长（最坏吊销延迟的上界之一）。
	TTL = 30 * time.Second
	// MaxEntries 是 LRU 容量上限（防进程内无界增长）。
	MaxEntries = 10000
	// markerSessionPrefix / markerUserPrefix 是 Redis 失效标记键前缀：
	//   torchwood:principal-inval:s:<sessionID>
	//   torchwood:principal-inval:u:<projectID>:<userID>
	markerSessionPrefix = "torchwood:principal-inval:s:"
	markerUserPrefix    = "torchwood:principal-inval:u:"
	// markerTTL 是失效标记存活时长：只需覆盖可能仍引用被删会话的缓存条目
	// （条目本身 ≤ TTL 30s），取 5min 容忍时钟偏移与跨实例写延迟。
	markerTTL = 5 * time.Minute
)

// Key 是缓存键投影。
type Key struct {
	ProjectID string
	SessionID string
	IAT       int64
}

func (k Key) String() string {
	return fmt.Sprintf("%s|%s|%d", k.ProjectID, k.SessionID, k.IAT)
}

type entry struct {
	key       string
	principal *shared.Principal
	expiresAt time.Time
}

// Cache 是 principal 进程内 LRU + Redis 失效标记。rdb 可为 nil（单进程部署
// /测试）：跳过标记检查，失效仍由 TTL 与进程内写路径（Invalidate* 同时删
// 本地条目）保证。
type Cache struct {
	ttl time.Duration
	now func() time.Time
	rdb *redis.Client

	mu    sync.Mutex
	order *list.List // LRU：front = 最近使用
	items map[string]*list.Element
}

// New 构造缓存（rdb 可 nil）。
func New(rdb *redis.Client) *Cache {
	return &Cache{
		ttl:   TTL,
		now:   time.Now,
		rdb:   rdb,
		order: list.New(),
		items: map[string]*list.Element{},
	}
}

// SetTTL / SetNow 供测试注入。
func (c *Cache) SetTTL(ttl time.Duration)    { c.ttl = ttl }
func (c *Cache) SetNow(now func() time.Time) { c.now = now }

// Get 返回缓存的 principal（深拷贝，防调用方篡改共享切片）；未命中/已过期/
// 失效标记命中返回 nil。
func (c *Cache) Get(ctx context.Context, key Key) *shared.Principal {
	skey := key.String()
	c.mu.Lock()
	el, ok := c.items[skey]
	if !ok {
		c.mu.Unlock()
		return nil
	}
	e := el.Value.(*entry)
	if c.now().After(e.expiresAt) {
		c.removeLocked(skey, el)
		c.mu.Unlock()
		return nil
	}
	// LRU 触碰。
	c.order.MoveToFront(el)
	c.mu.Unlock()

	if c.invalidated(ctx, key, e.principal) {
		c.mu.Lock()
		c.removeLocked(skey, el)
		c.mu.Unlock()
		return nil
	}
	return copyPrincipal(e.principal)
}

// Put 写入缓存条目。
func (c *Cache) Put(key Key, principal *shared.Principal) {
	if principal == nil {
		return
	}
	skey := key.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[skey]; ok {
		el.Value.(*entry).principal = copyPrincipal(principal)
		el.Value.(*entry).expiresAt = c.now().Add(c.ttl)
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&entry{key: skey, principal: copyPrincipal(principal), expiresAt: c.now().Add(c.ttl)})
	c.items[skey] = el
	for c.itemsLen() > MaxEntries {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.removeLocked(oldest.Value.(*entry).key, oldest)
	}
}

// InvalidateSession 使单会话的缓存条目失效（写 Redis 标记 + 删本地条目；
// 单会话登出路径用——当前全部删除走 DeleteSessionsByUser，本方法为单会话
// 撤销预留）。
func (c *Cache) InvalidateSession(ctx context.Context, projectID, sessionID string) error {
	c.invalidateLocal(func(p *shared.Principal) bool {
		return p != nil && p.ProjectID == projectID && p.SessionID == sessionID
	})
	if c.rdb == nil {
		return nil
	}
	return c.rdb.Set(ctx, markerSessionPrefix+sessionID, "1", markerTTL).Err()
}

// InvalidateUser 使该用户全部会话的缓存条目失效（DeleteSessionsByUser /
// 封禁路径的单一咽喉写点）。
func (c *Cache) InvalidateUser(ctx context.Context, projectID, userID string) error {
	c.invalidateLocal(func(p *shared.Principal) bool {
		return p != nil && p.ProjectID == projectID && p.UserID == userID
	})
	if c.rdb == nil {
		return nil
	}
	return c.rdb.Set(ctx, markerUserPrefix+projectID+":"+userID, "1", markerTTL).Err()
}

// invalidated 检查 Redis 失效标记：标记命中 → true；Redis 故障 → true
// （fail-closed：回退 DB 实时校验）。rdb 为 nil → false（无跨实例失效面，
// 进程内失效已由 TTL/写路径覆盖）。两次标记检查合并为一次 pipeline 往返。
func (c *Cache) invalidated(ctx context.Context, key Key, p *shared.Principal) bool {
	if c.rdb == nil {
		return false
	}
	cmds := make([]*redis.StringCmd, 0, 2)
	if key.SessionID != "" {
		cmds = append(cmds, c.rdb.Get(ctx, markerSessionPrefix+key.SessionID))
	}
	if p != nil && p.UserID != "" && key.ProjectID != "" {
		cmds = append(cmds, c.rdb.Get(ctx, markerUserPrefix+key.ProjectID+":"+p.UserID))
	}
	if len(cmds) == 0 {
		return false
	}
	for _, cmd := range cmds {
		_, err := cmd.Result()
		if err == nil {
			return true // 标记存在 = 已失效
		}
		if err != redis.Nil {
			return true // 基础设施故障：放弃缓存收益，回退 DB 实时校验
		}
	}
	return false
}

func (c *Cache) invalidateLocal(match func(*shared.Principal) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var victims []*list.Element
	for el := c.order.Front(); el != nil; el = el.Next() {
		if match(el.Value.(*entry).principal) {
			victims = append(victims, el)
		}
	}
	for _, el := range victims {
		c.removeLocked(el.Value.(*entry).key, el)
	}
}

// removeLocked 要求持 c.mu。
func (c *Cache) removeLocked(skey string, el *list.Element) {
	c.order.Remove(el)
	delete(c.items, skey)
}

func (c *Cache) itemsLen() int { return len(c.items) }

// copyPrincipal 深拷贝共享可变字段（切片），防调用方篡改缓存内容。
func copyPrincipal(p *shared.Principal) *shared.Principal {
	if p == nil {
		return nil
	}
	out := *p
	out.Roles = append([]string(nil), p.Roles...)
	out.Permissions = append([]string(nil), p.Permissions...)
	return &out
}
