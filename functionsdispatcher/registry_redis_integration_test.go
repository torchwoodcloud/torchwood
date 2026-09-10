package functionsdispatcher

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
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
// miniredis（内嵌 Lua 解释器，claimIdleLua/releaseInstanceLua 真实求值——
// 不是绕过脚本的桩）。
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

// TestRedisRegistry_ClaimReleaseRoundTrip 真 Lua claim/release 往返（v3 回归）：
// spawn → claim → release → 二次 claim 全链路。claim 置 inflight=1 + 续租；
// release 归零 inflight + requests+1 + 落 idle_since_ms（数值毫秒，Lua cjson
// 往返不改写任何字段形态——时间字段毫秒化后排雷，v3 §1.3）。
func TestRedisRegistry_ClaimReleaseRoundTrip(t *testing.T) {
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
		Concurrency:    1,
		SpawnedAtMS:    now.UnixMilli(),
		IdleSinceMS:    now.UnixMilli(),
		LeaseUntilMS:   now.Add(leaseTTL).UnixMilli(),
		IdleTTLSeconds: 300,
	})

	// ① claim 返回记录必须可反序列化且 inflight=1。
	rec, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "inst-1", rec.InstanceID)
	require.Equal(t, 1, rec.Inflight, "claim 必须置 inflight=1")

	// ② Redis 中的记录：全部时间字段保持数值毫秒形态（cjson 往返不改写，
	// v3 §1.3 排雷）；inflight 落位；不得再出现 busy 布尔。
	raw, err := rdb.HGet(ctx, registryKey(ref), "inst-1").Result()
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &fields))
	require.NotContains(t, fields, "busy", "不得再出现 busy 旧字段")
	require.NotContains(t, fields, "spawned_at", "时间字段必须保持数值毫秒形态")
	require.NotContains(t, fields, "idle_since", "时间字段必须保持数值毫秒形态")
	var inflight int
	require.NoError(t, json.Unmarshal(fields["inflight"], &inflight))
	require.Equal(t, 1, inflight, "claim 必须置 inflight=1")
	require.JSONEq(t, strconv.FormatInt(now.UnixMilli(), 10), string(fields["spawned_at_ms"]),
		"spawned_at_ms 必须保持数值毫秒")

	// ③ release：inflight 归零 + requests+1 + idle_since_ms 落位；二次 claim 成功。
	leaseUntil := now.Add(2 * leaseTTL)
	out, err := reg.Release(ctx, ref, "inst-1", now, leaseUntil, false)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Zero(t, out.Inflight)
	require.Equal(t, int64(1), out.Requests)
	require.Equal(t, now.UnixMilli(), out.IdleSinceMS)
	require.Equal(t, leaseUntil.UnixMilli(), out.LeaseUntilMS, "release 必须续租")

	rec2, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
	require.NoError(t, err)
	require.NotNil(t, rec2, "release 后二次 claim 必须成功")
	require.Equal(t, "inst-1", rec2.InstanceID)

	records, err := reg.List(ctx, ref)
	require.NoError(t, err)
	require.Len(t, records, 1, "List 不得静默丢弃记录")
	require.NotZero(t, records[0].SpawnedAtMS)
}

// TestRedisRegistry_InflightConcurrency 真 Lua 并发 claim/release（v3 §1.1/
// §1.3）：concurrency=4 的单实例、并发 8 个 claim 恰好 4 个成功（上限收敛）；
// 全部 release 后 inflight==0、requests==4、可再次认领（计数无漂移）。
func TestRedisRegistry_InflightConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb := newRegistryTestRedis(t)
	reg := NewRedisRegistry(rdb)
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "redis-it", FunctionID: "fn-inflight"}
	require.NoError(t, rdb.Del(ctx, registryKey(ref)).Err())

	now := time.Now()
	seedInstance(t, rdb, ref, InstanceRecord{
		InstanceID:   "inst-1",
		ContainerID:  "inst-1",
		IP:           "10.0.0.1",
		DeploymentID: "dep-1",
		Concurrency:  4,
		SpawnedAtMS:  now.UnixMilli(),
		IdleSinceMS:  now.UnixMilli(),
		LeaseUntilMS: now.Add(leaseTTL).UnixMilli(),
	})

	const n = 8
	var successes atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
			require.NoError(t, err)
			if rec != nil {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.Equal(t, int32(4), successes.Load(), "并发认领必须恰好收敛到 concurrency=4 上限")

	// N claim + N release：计数收敛（release 走真 Lua）。
	for i := 0; i < 4; i++ {
		out, err := reg.Release(ctx, ref, "inst-1", now, now.Add(leaseTTL), false)
		require.NoError(t, err)
		require.NotNil(t, out)
	}
	records, err := reg.List(ctx, ref)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Zero(t, records[0].Inflight, "4 claim + 4 release 后 inflight 必须归零")
	require.Equal(t, int64(4), records[0].Requests)

	rec, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
	require.NoError(t, err)
	require.NotNil(t, rec, "计数收敛后必须可再次认领（无永久「满载」漂移）")
}

// TestRedisRegistry_ClaimIdleMutex 并发认领互斥：两个空闲实例（concurrency=1）、
// 并发 8 个 claim，恰好 2 个成功且无重复实例（concurrency=1 退化为 v2 串行
// 互斥，不回归）。
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
			SpawnedAtMS:  now.UnixMilli(),
			IdleSinceMS:  now.UnixMilli(),
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

// TestRedisRegistry_LegacyRecordCompat 存量旧记录兼容（v3 §1.3「兼容陷阱」，
// 必须处理——清 Redis 键升级会泄漏容器）：busy 布尔 + RFC3339 时间字符串 +
// 数字 lease_until 遗留字段的记录可被 List/decode（busy 推导 inflight）、可被
// 认领改写；Lua 重写后 legacy 字段原样保留（字符串不毒化），自然老化。
func TestRedisRegistry_LegacyRecordCompat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb := newRegistryTestRedis(t)
	reg := NewRedisRegistry(rdb)
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "redis-it", FunctionID: "fn-legacy"}
	key := registryKey(ref)
	require.NoError(t, rdb.Del(ctx, key).Err())

	// 手工注入一条 v2 形态的旧记录（busy 布尔 + RFC3339 时间 + 数字 lease_until）。
	now := time.Now()
	legacy := `{"instance_id":"inst-legacy","container_id":"inst-legacy","ip":"10.0.0.9",` +
		`"deployment_id":"dep-1","busy":false,"draining":false,"requests":1,` +
		`"spawned_at":"` + now.Add(-time.Hour).UTC().Format(time.RFC3339Nano) + `",` +
		`"idle_since":"` + now.Add(-30*time.Minute).UTC().Format(time.RFC3339Nano) + `",` +
		`"lease_until":` + "1797012345678" + `,` +
		`"min_instances":0,"idle_ttl_seconds":300,"max_requests":1000}`
	require.NoError(t, rdb.HSet(ctx, key, "inst-legacy", legacy).Err())

	records, err := reg.List(ctx, ref)
	require.NoError(t, err)
	require.Len(t, records, 1, "旧记录必须可见（双读兼容，不得静默丢弃）")
	require.Zero(t, records[0].Inflight, "busy=false 推导 inflight=0")
	require.NotZero(t, records[0].SpawnedAtMS, "RFC3339 spawned_at 必须解析为毫秒")
	require.NotZero(t, records[0].IdleSinceMS, "RFC3339 idle_since 必须解析为毫秒")

	rec, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
	require.NoError(t, err)
	require.NotNil(t, rec, "旧记录可被认领（Lua inflight/busy 兜底）")
	require.Equal(t, "inst-legacy", rec.InstanceID)
	require.Equal(t, 1, rec.Inflight)

	// 认领改写（Lua 重写）后：inflight/lease_until_ms 落位；legacy 字符串字段
	// 原样保留（cjson 不改写字符串形态），记录仍可 decode——无升级 runbook。
	raw, err := rdb.HGet(ctx, key, "inst-legacy").Result()
	require.NoError(t, err)
	require.Contains(t, raw, `"inflight":1`)
	require.Contains(t, raw, `"lease_until_ms":`)
	require.Contains(t, raw, `"spawned_at":"`, "legacy 字符串字段经 Lua 往返原样保留（不毒化）")

	// 旧记录 release：busy 缺省推导通道 + 收账照常。
	out, err := reg.Release(ctx, ref, "inst-legacy", now, now.Add(leaseTTL), false)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Zero(t, out.Inflight)
	require.Equal(t, int64(2), out.Requests)
}

// TestRedisRegistry_ReleaseTimeoutFuse release 的 timeout 标志：timeouts 累加
// （熔断计数）、正常释放不累加（v3 §1.4）。
func TestRedisRegistry_ReleaseTimeoutFuse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb := newRegistryTestRedis(t)
	reg := NewRedisRegistry(rdb)
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "redis-it", FunctionID: "fn-fuse"}
	require.NoError(t, rdb.Del(ctx, registryKey(ref)).Err())

	now := time.Now()
	seedInstance(t, rdb, ref, InstanceRecord{
		InstanceID:   "inst-1",
		ContainerID:  "inst-1",
		IP:           "10.0.0.1",
		DeploymentID: "dep-1",
		SpawnedAtMS:  now.UnixMilli(),
		IdleSinceMS:  now.UnixMilli(),
		LeaseUntilMS: now.Add(leaseTTL).UnixMilli(),
	})

	_, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
	require.NoError(t, err)
	_, err = reg.Release(ctx, ref, "inst-1", now, now.Add(leaseTTL), false)
	require.NoError(t, err)
	records, _ := reg.List(ctx, ref)
	require.Zero(t, records[0].Timeouts, "正常释放不得累加 timeouts")

	// 超时释放路径 ×3：timeouts 累加到 3。
	for i := 0; i < 3; i++ {
		_, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
		require.NoError(t, err)
		out, err := reg.Release(ctx, ref, "inst-1", now, now.Add(leaseTTL), true)
		require.NoError(t, err)
		require.Equal(t, i+1, out.Timeouts)
	}
	records, _ = reg.List(ctx, ref)
	require.Equal(t, 3, records[0].Timeouts, "超时释放路径必须累加 timeouts")
	require.Zero(t, records[0].Inflight)
}

// TestRedisRegistry_ReleaseMaxRequestsDrains release 后达记录固化 max_requests
// 判 draining（v3 §1.3 对抗审查修正：用记录固化值，非请求携带值）。
func TestRedisRegistry_ReleaseMaxRequestsDrains(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rdb := newRegistryTestRedis(t)
	reg := NewRedisRegistry(rdb)
	ctx := context.Background()
	ref := FunctionRef{ProjectID: "redis-it", FunctionID: "fn-drain"}
	require.NoError(t, rdb.Del(ctx, registryKey(ref)).Err())

	now := time.Now()
	seedInstance(t, rdb, ref, InstanceRecord{
		InstanceID:   "inst-1",
		ContainerID:  "inst-1",
		IP:           "10.0.0.1",
		DeploymentID: "dep-1",
		MaxRequests:  2,
		SpawnedAtMS:  now.UnixMilli(),
		IdleSinceMS:  now.UnixMilli(),
		LeaseUntilMS: now.Add(leaseTTL).UnixMilli(),
	})

	// 第 1 次：requests=1 < 2，不 draining。
	_, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
	require.NoError(t, err)
	out, err := reg.Release(ctx, ref, "inst-1", now, now.Add(leaseTTL), false)
	require.NoError(t, err)
	require.False(t, out.Draining)

	// 第 2 次：requests=2 >= max_requests → draining（实例不再可认领）。
	_, err = reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
	require.NoError(t, err)
	out, err = reg.Release(ctx, ref, "inst-1", now, now.Add(leaseTTL), false)
	require.NoError(t, err)
	require.True(t, out.Draining, "达记录固化 max_requests 后必须判 draining")

	rec, err := reg.ClaimIdle(ctx, ref, "dep-1", now.Add(leaseTTL))
	require.NoError(t, err)
	require.Nil(t, rec, "draining 实例不得被认领")
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
