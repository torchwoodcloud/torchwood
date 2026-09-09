package principalcache_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/infra/auth/principalcache"
)

func testPrincipal(projectID, userID, sessionID string) *shared.Principal {
	return &shared.Principal{
		ActorKind:   shared.ActorKindEndUser,
		ProjectID:   projectID,
		UserID:      userID,
		SessionID:   sessionID,
		Roles:       []string{"users", "user:" + userID},
		Permissions: []string{"databases.read"},
	}
}

// TestCache_HitWithinTTL TTL 内命中返回等值 principal；返回的是深拷贝
// （调用方篡改不污染缓存）。
func TestCache_HitWithinTTL(t *testing.T) {
	c := principalcache.New(nil)
	ctx := context.Background()
	key := principalcache.Key{ProjectID: "p1", SessionID: "s1", IAT: 100}
	p := testPrincipal("p1", "u1", "s1")

	c.Put(key, p)
	got := c.Get(ctx, key)
	require.NotNil(t, got)
	require.Equal(t, "u1", got.UserID)
	require.Equal(t, []string{"users", "user:u1"}, got.Roles)

	// 深拷贝断言：修改返回值的切片不影响缓存内容。
	got.Roles[0] = "tampered"
	got2 := c.Get(ctx, key)
	require.Equal(t, "users", got2.Roles[0], "缓存条目必须与返回值隔离（深拷贝）")
}

// TestCache_CrossTTLMiss 跨 TTL 过期失效。
func TestCache_CrossTTLMiss(t *testing.T) {
	c := principalcache.New(nil)
	ctx := context.Background()
	now := time.Now()
	clock := now
	c.SetNow(func() time.Time { return clock })
	c.SetTTL(30 * time.Second)

	key := principalcache.Key{ProjectID: "p1", SessionID: "s1", IAT: 100}
	c.Put(key, testPrincipal("p1", "u1", "s1"))
	require.NotNil(t, c.Get(ctx, key))

	clock = now.Add(31 * time.Second)
	require.Nil(t, c.Get(ctx, key), "跨 TTL 必须失效")
}

// TestCache_InvalidateUserLocal 写路径失效：InvalidateUser 删除该用户全部
// 本地条目（nil rdb = 无跨实例标记面，进程内失效即全集）。
func TestCache_InvalidateUserLocal(t *testing.T) {
	c := principalcache.New(nil)
	ctx := context.Background()
	k1 := principalcache.Key{ProjectID: "p1", SessionID: "s1", IAT: 1}
	k2 := principalcache.Key{ProjectID: "p1", SessionID: "s2", IAT: 2}
	k3 := principalcache.Key{ProjectID: "p1", SessionID: "s3", IAT: 3}
	c.Put(k1, testPrincipal("p1", "u1", "s1"))
	c.Put(k2, testPrincipal("p1", "u1", "s2"))
	c.Put(k3, testPrincipal("p1", "u2", "s3"))

	require.NoError(t, c.InvalidateUser(ctx, "p1", "u1"))
	require.Nil(t, c.Get(ctx, k1), "u1 会话 1 必须失效")
	require.Nil(t, c.Get(ctx, k2), "u1 会话 2 必须失效")
	require.NotNil(t, c.Get(ctx, k3), "u2 条目不受影响")
}

// TestCache_InvalidateSession 单会话失效。
func TestCache_InvalidateSession(t *testing.T) {
	c := principalcache.New(nil)
	ctx := context.Background()
	key := principalcache.Key{ProjectID: "p1", SessionID: "s1", IAT: 1}
	c.Put(key, testPrincipal("p1", "u1", "s1"))
	require.NoError(t, c.InvalidateSession(ctx, "p1", "s1"))
	require.Nil(t, c.Get(ctx, key))
}

// TestCache_RedisFailureFailsClosed Redis 故障时标记检查失败按 miss 处理
// （fail-closed：回退 DB 实时校验，不放大吊销延迟）。
func TestCache_RedisFailureFailsClosed(t *testing.T) {
	// 不可达地址的 redis client（进程内无监听端口）。
	bad := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	defer func() { _ = bad.Close() }()
	c := principalcache.New(bad)
	c.SetTTL(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	key := principalcache.Key{ProjectID: "p1", SessionID: "s1", IAT: 1}
	c.Put(key, testPrincipal("p1", "u1", "s1"))
	require.Nil(t, c.Get(ctx, key), "标记检查不可用必须按 miss 处理（fail-closed）")
}

// TestCache_IATSeparatesKeys iat 变化（重签）产生独立键——新键未命中会
// 重新走 DB 实时校验，旧键随 TTL 消亡。
func TestCache_IATSeparatesKeys(t *testing.T) {
	c := principalcache.New(nil)
	ctx := context.Background()
	c.Put(principalcache.Key{ProjectID: "p1", SessionID: "s1", IAT: 1}, testPrincipal("p1", "u1", "s1"))
	require.NotNil(t, c.Get(ctx, principalcache.Key{ProjectID: "p1", SessionID: "s1", IAT: 1}))
	require.Nil(t, c.Get(ctx, principalcache.Key{ProjectID: "p1", SessionID: "s1", IAT: 2}), "iat 不同即独立键")
}
