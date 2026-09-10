package functionsdispatcher

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// newRegistryTestRedis 构造集成测试用 Redis 客户端：优先 TORCHWOOD_TEST_REDIS_ADDR
// 指向的真 redis-server（本地 task docker:up / CI redis service），否则退回
// miniredis（内嵌 Lua 解释器，claimIdleLua 真实求值——不是绕过脚本的桩）。
func newRegistryTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	if addr := os.Getenv("TORCHWOOD_TEST_REDIS_ADDR"); addr != "" {
		rdb := redis.NewClient(&redis.Options{Addr: addr})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := rdb.Ping(ctx).Err()
		cancel()
		if err == nil {
			t.Cleanup(func() { _ = rdb.Close() })
			return rdb
		}
		t.Logf("TORCHWOOD_TEST_REDIS_ADDR=%s unreachable (%v), falling back to miniredis", addr, err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// seedInstance 把记录以 Go 编码形态写入注册表（对齐 Save 路径）。
func seedInstance(t *testing.T, rdb *redis.Client, ref FunctionRef, rec InstanceRecord) {
	t.Helper()
	raw, err := encodeRecord(rec)
	require.NoError(t, err)
	require.NoError(t, rdb.HSet(context.Background(), registryKey(ref), rec.InstanceID, raw).Err())
}

// TestRedisRegistry_ClaimIdleRoundTrip 真 Lua claim 往返（P0 回归）：
// spawn → claim → 释放 → 二次 claim 全链路。修复前 claimIdleLua 把
// lease_until 改写为数字，claim 返回记录反序列化必败（Dispatch 500）且
// Redis 记录毒化（List/Update 静默吞掉，实例成账外幽灵）。
func TestRedisRegistry_ClaimIdleRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb := newRegistryTestRedis(t)
	reg := NewRedisRegistry(rdb)
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "redis-it", FunctionID: "fnit"}
	require.NoError(t, rdb.Del(ctx, registryKey(ref)).Err())

	now := time.Now()
	seedInstance(t, rdb, ref, InstanceRecord{
		InstanceID:     "inst-1",
		ContainerID:    "inst-1",
		IP:             "10.0.0.1",
		DeploymentID:   "dep-1",
		SpawnedAt:      now,
		IdleSince:      now,
		LeaseUntilMS:   now.Add(leaseTTL).UnixMilli(),
		IdleTTLSeconds: 300,
	})

	// ① claim 返回记录必须可反序列化（修复前此处 time.Time 收到数字即失败）。
	rec, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "inst-1", rec.InstanceID)
	require.True(t, rec.Busy)

	// ② Redis 中的记录：lease_until_ms 为数字；时间字段仍是 RFC3339 字符串
	// （不变量：cjson 往返不得改写时间字段形态）。
	raw, err := rdb.HGet(ctx, registryKey(ref), "inst-1").Result()
	require.NoError(t, err)
	var probe struct {
		Busy         bool                       `json:"busy"`
		LeaseUntilMS int64                      `json:"lease_until_ms"`
		LeaseUntil   json.RawMessage            `json:"lease_until"`
		SpawnedAt    string                     `json:"spawned_at"`
		IdleSince    string                     `json:"idle_since"`
		All          map[string]json.RawMessage `json:"-"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &probe))
	require.True(t, probe.Busy, "claim 必须置 busy")
	require.Equal(t, now.Add(leaseTTL).UnixMilli(), probe.LeaseUntilMS, "续租毫秒值必须落进 Redis")
	require.Empty(t, probe.LeaseUntil, "不得再出现 lease_until 字符串字段")
	require.NotEmpty(t, probe.SpawnedAt, "spawned_at 必须仍是字符串")
	_, err = time.Parse(time.RFC3339Nano, probe.SpawnedAt)
	require.NoError(t, err, "spawned_at 必须可反序列化为 time.Time")
	_, err = time.Parse(time.RFC3339Nano, probe.IdleSince)
	require.NoError(t, err, "idle_since 必须可反序列化为 time.Time")

	// ③ 释放后二次 claim 成功；List/Update 不再静默丢记录。
	updated, err := reg.Update(ctx, ref, "inst-1", func(r *InstanceRecord) {
		r.Busy = false
		r.IdleSince = time.Now()
		r.LeaseUntilMS = time.Now().Add(leaseTTL).UnixMilli()
	})
	require.NoError(t, err)
	require.True(t, updated, "claim 后记录必须可 Update（毒化记录会静默返回 false）")
	rec2, err := reg.ClaimIdle(ctx, ref, "dep-1", time.Now().Add(leaseTTL))
	require.NoError(t, err)
	require.NotNil(t, rec2, "释放后二次 claim 必须成功")
	require.Equal(t, "inst-1", rec2.InstanceID)

	records, err := reg.List(ctx, ref)
	require.NoError(t, err)
	require.Len(t, records, 1, "List 不得静默丢弃记录")
	require.False(t, records[0].SpawnedAt.IsZero())
}

// TestRedisRegistry_ClaimIdleMutex 并发认领互斥：两个空闲实例、并发 8 个
// claim，恰好 2 个成功且无重复实例（busy 置位原子性不回归）。
func TestRedisRegistry_ClaimIdleMutex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb := newRegistryTestRedis(t)
	reg := NewRedisRegistry(rdb)
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "redis-it", FunctionID: "fn-mutex"}
	require.NoError(t, rdb.Del(ctx, registryKey(ref)).Err())

	now := time.Now()
	for _, id := range []string{"inst-1", "inst-2"} {
		seedInstance(t, rdb, ref, InstanceRecord{
			InstanceID:   id,
			ContainerID:  id,
			IP:           "10.0.0." + id[len(id)-1:],
			DeploymentID: "dep-1",
			SpawnedAt:    now,
			IdleSince:    now,
			LeaseUntilMS: now.Add(leaseTTL).UnixMilli(),
		})
	}

	const n = 8
	var mu sync.Mutex
	claimed := map[string]int{}
	var successes atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec, err := reg.ClaimIdle(ctx, ref, "dep-1", time.Now().Add(leaseTTL))
			require.NoError(t, err)
			if rec != nil {
				successes.Add(1)
				mu.Lock()
				claimed[rec.InstanceID]++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, int32(2), successes.Load(), "恰好认领 2 个空闲实例")
	for id, count := range claimed {
		require.Equal(t, 1, count, "实例 %s 不得被重复认领", id)
	}
}

// TestRedisRegistry_LegacyPoisonedRecordRecoverable 存量毒化记录恢复可见：
// 修复前版本写入的 lease_until 是数字（Time.UnmarshalJSON 必败 → List/Update
// 静默吞掉）。字符串字段删除后旧字段成为未知字段被忽略，记录重新进入
// reaper 视野（可清理），且可被正常认领改写。
func TestRedisRegistry_LegacyPoisonedRecordRecoverable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb := newRegistryTestRedis(t)
	reg := NewRedisRegistry(rdb)
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "redis-it", FunctionID: "fn-poison"}
	key := registryKey(ref)
	require.NoError(t, rdb.Del(ctx, key).Err())

	// 手工注入一条现网形态的毒化记录（lease_until 为数字毫秒）。
	now := time.Now()
	poisoned := `{"instance_id":"inst-poison","container_id":"inst-poison","ip":"10.0.0.9",` +
		`"deployment_id":"dep-1","busy":false,"draining":false,"requests":1,` +
		`"spawned_at":"` + now.Add(-time.Hour).UTC().Format(time.RFC3339Nano) + `",` +
		`"idle_since":"` + now.Add(-30*time.Minute).UTC().Format(time.RFC3339Nano) + `",` +
		`"lease_until":` + "1797012345678" + `,` +
		`"min_instances":0,"idle_ttl_seconds":300,"max_requests":1000}`
	require.NoError(t, rdb.HSet(ctx, key, "inst-poison", poisoned).Err())

	records, err := reg.List(ctx, ref)
	require.NoError(t, err)
	require.Len(t, records, 1, "毒化记录必须重新可见（不再静默丢弃）")

	updated, err := reg.Update(ctx, ref, "inst-poison", func(r *InstanceRecord) {
		r.Busy = false
	})
	require.NoError(t, err)
	require.True(t, updated, "毒化记录必须可改写")

	rec, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
	require.NoError(t, err)
	require.NotNil(t, rec, "毒化记录可被认领")
	require.Equal(t, "inst-poison", rec.InstanceID)

	// 认领改写后记录恢复规范形态：lease_until 消失、lease_until_ms 落位。
	raw, err := rdb.HGet(ctx, key, "inst-poison").Result()
	require.NoError(t, err)
	require.NotContains(t, raw, `"lease_until":`, "改写后不得残留数字 lease_until 字段")
	require.Contains(t, raw, `"lease_until_ms":`)
}

// TestRedisRegistry_AcquireSpawnLock 真 Redis 上的 spawn 锁语义：互斥获取 +
// 持锁者比对删除（他人锁不可删）。
func TestRedisRegistry_AcquireSpawnLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb := newRegistryTestRedis(t)
	reg := NewRedisRegistry(rdb)
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "redis-it", FunctionID: "fn-lock"}

	acquired1, release1, err := reg.AcquireSpawnLock(ctx, ref, time.Minute)
	require.NoError(t, err)
	require.True(t, acquired1)

	acquired2, release2, err := reg.AcquireSpawnLock(ctx, ref, time.Minute)
	require.NoError(t, err)
	require.False(t, acquired2, "锁被占用时不得获取")
	release2()
	acquired3, release3, err := reg.AcquireSpawnLock(ctx, ref, time.Minute)
	require.NoError(t, err)
	require.False(t, acquired3, "无锁持有者的 release 必须是 no-op")
	_ = release3

	release1()
	acquired4, release4, err := reg.AcquireSpawnLock(ctx, ref, time.Minute)
	require.NoError(t, err)
	require.True(t, acquired4, "持锁者释放后必须可重获")
	release4()
}
