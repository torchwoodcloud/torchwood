package dispatcher

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖四期 4c M4 容量共享（设计
// docs/design/functions-runtimes-and-sources.md §4）：全局常驻上限
// （Redis 容量键求和）的拒绝/放行、Redis 不可用 fail-open、容量键刷新点
// （spawn 成功 +1 / terminate -1 / reaper 回收 -1 与每轮 TTL 保鲜）、
// 全局上限未配置时热路径零 Redis 读。

// newCapTestPool 组装带节点身份与短队首超时的被测池（全局上限用例）；
// 返回 daemon 供 spawn 次数断言。
func newCapTestPool(t *testing.T, reg *fakeRegistry, mutate func(*PoolConfig)) (*PoolManager, *fakeDaemon) {
	t.Helper()
	d := newFakeDaemon()
	runner := &fakeRunner{}
	runner.healthy = true
	pool := newTestPool(d, reg, runner, func(c *PoolConfig) {
		c.QueueHeadTimeout = 150 * time.Millisecond
		if mutate != nil {
			mutate(c)
		}
	})
	pool.SetNodeID("node-a")
	return pool, d
}

// TestPoolDispatch_GlobalLimitRejects 全局上限拒绝：他节点容量键已把集群
// 总量顶到上限时，本节点即使远未满（本地 8）也拒绝冷启动——请求走排队路径
// 以 ResourceExhausted 收场，spawn 一次都不发生。
func TestPoolDispatch_GlobalLimitRejects(t *testing.T) {
	reg := newFakeRegistry()
	reg.seedCapacity("node-b", 3) // 他节点已占 3
	pool, d := newCapTestPool(t, reg, func(c *PoolConfig) {
		c.MaxResidentInstancesGlobal = 3
	})
	ctx := context.Background()

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "no free instance within queue head timeout")
	require.Zero(t, d.spawnCount, "全局满必须一次 spawn 都不发生")
	require.Zero(t, pool.ResidentTotal(), "拒绝路径必须回退本节点预留")
	require.Zero(t, capSelfCount(reg, "node-a"), "全局满不得留下本节点容量写入")
	require.Greater(t, reg.capSumCalls, 0, "排队重试必须反复读全局总量")
}

// TestPoolDispatch_GlobalLimitAllowsThenStops 全局门按容量键求和放行到上限：
// 全局 2 → fn1 冷启动放行（求和 0<2，spawn 后本节点键=1）→ fn2 放行（求和
// 1<2，键=2）→ fn3 拒绝（求和 2>=2）。容量键刷新点（spawn 成功 +1）直接
// 喂给全局门。
func TestPoolDispatch_GlobalLimitAllowsThenStops(t *testing.T) {
	reg := newFakeRegistry()
	pool, _ := newCapTestPool(t, reg, func(c *PoolConfig) {
		c.MaxResidentInstancesGlobal = 2
	})
	ctx := context.Background()

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)
	require.Equal(t, 1, capSelfCount(reg, "node-a"))

	req2 := dispatchReq()
	req2.FunctionID = "fn2"
	_, err = pool.Dispatch(ctx, req2)
	require.NoError(t, err)
	require.Equal(t, 2, capSelfCount(reg, "node-a"))

	req3 := dispatchReq()
	req3.FunctionID = "fn3"
	_, err = pool.Dispatch(ctx, req3)
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "全局总量达上限后必须拒绝冷启动")
	require.Equal(t, 2, capSelfCount(reg, "node-a"), "拒绝路径必须回退本节点预留")
}

// TestPoolDispatch_GlobalCapacityRedisFailOpen Redis 不可用 fail-open：
// 全局上限=1 但求和失败 → 冷启动照常放行（退化为仅本节点上限），故障告警
// 限频（5s 窗口内连续 spawn 只落一条）。
func TestPoolDispatch_GlobalCapacityRedisFailOpen(t *testing.T) {
	reg := newFakeRegistry()
	reg.capErr = errors.New("redis down")
	pool, d := newCapTestPool(t, reg, func(c *PoolConfig) {
		c.MaxResidentInstancesGlobal = 1
	})
	records := captureSlog(t)

	ctx := context.Background()
	_, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err, "Redis 失败必须 fail-open 放行（容量门不是安全门）")
	require.Equal(t, 1, d.spawnCount)
	require.Equal(t, 1, capSelfCount(reg, "node-a"))

	req2 := dispatchReq()
	req2.FunctionID = "fn2"
	_, err = pool.Dispatch(ctx, req2)
	require.NoError(t, err, "第二次 spawn 同样 fail-open（本节点上限 8 未触顶）")

	warns := 0
	for _, m := range records.snapshot() {
		if strings.Contains(m, "capacity channel degraded") {
			warns++
		}
	}
	require.Equal(t, 1, warns, "容量通道告警必须限频（5s 窗口一条）: %v", records.snapshot())
}

// TestPoolDispatch_GlobalLimitUnsetNoRedisReads 全局上限未配置（缺省 0）：
// 冷启动热路径零全局求和调用（无额外 Redis 往返），容量键照常刷新。
func TestPoolDispatch_GlobalLimitUnsetNoRedisReads(t *testing.T) {
	reg := newFakeRegistry()
	pool, _ := newCapTestPool(t, reg, nil)
	ctx := context.Background()

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)
	require.Zero(t, reg.capSumCalls, "未配置全局上限不得读全局总量")
	require.Equal(t, 1, capSelfCount(reg, "node-a"), "spawn 成功必须刷新本节点容量键")
}

// TestPoolReaper_RefreshesCapacityKey 容量键刷新点（与 residentTotal 同点）：
// spawn 成功 +1 → reaper 空转轮 TTL 保鲜（再写一次，值不变）→ idle 回收
// -1 → 传输错误强杀 -1。
func TestPoolReaper_RefreshesCapacityKey(t *testing.T) {
	reg := newFakeRegistry()
	pool, _ := newCapTestPool(t, reg, nil)
	ctx := context.Background()

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)
	require.Equal(t, 1, capSelfCount(reg, "node-a"))

	// reaper 空转轮：无回收也刷新（TTL 保鲜，值不变）。
	writesBefore := reg.capWrites
	pool.Reaper(ctx)
	require.Greater(t, reg.capWrites, writesBefore, "reaper 每轮必须刷新容量键")
	require.Equal(t, 1, capSelfCount(reg, "node-a"))

	// idle 回收（killInstance → terminate）→ -1。
	markIdleOld(t, reg, 400*time.Second, 0)
	pool.Reaper(ctx)
	require.Zero(t, capSelfCount(reg, "node-a"), "idle 回收必须回退容量键")
	require.Equal(t, 0, pool.ResidentTotal())

	// 再次 spawn 后走传输错误强杀路径（killInstance → terminate）→ -1。
	runner := pool.runner.(*fakeRunner)
	runner.invokeFn = func(string, string) (*invokeResult, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	_, err = pool.Dispatch(ctx, dispatchReq())
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Zero(t, capSelfCount(reg, "node-a"), "强杀路径必须回退容量键")
}

// TestGlobalLimitDoesNotAffectInstanceClaim 全局门只约束扩容（spawn），
// 不影响既有实例的认领执行：全局已满（本节点键=1）后，同函数第二个请求
// 认领空闲实例正常执行，不再触全局门。
func TestGlobalLimitDoesNotAffectInstanceClaim(t *testing.T) {
	reg := newFakeRegistry()
	pool, _ := newCapTestPool(t, reg, func(c *PoolConfig) {
		c.MaxResidentInstancesGlobal = 1
	})
	ctx := context.Background()

	_, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)
	sumCallsBefore := reg.capSumCalls

	resp, err := pool.Dispatch(ctx, dispatchReq())
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	require.Equal(t, sumCallsBefore, reg.capSumCalls,
		"实例认领路径不得触发全局求和（无需扩容时零 Redis 读）")
}

// capSelfCount 读取 fake 容量键表（断言辅助）。
func capSelfCount(reg *fakeRegistry, nodeID string) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.caps[nodeID]
}

// captureSlog 把默认 slog logger 换成记录用 handler（测试期生效，结束恢复）。
func captureSlog(t *testing.T) *recordingHandler {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	records := &recordingHandler{}
	slog.SetDefault(slog.New(records))
	return records
}
