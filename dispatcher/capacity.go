// 节点容量共享（四期 4c M4，设计 docs/design/functions-runtimes-and-sources.md
// §4）：多节点细胞模型下 MaxResidentInstances 只是每节点进程内计数（互不知），
// 集群总量经 Redis 容量键 torchwood:fncap:resident:<node_id>（SET+TTL）求和
// 约束。本文件是 PoolManager 侧的两件事：
//
//  1. tryReserveGlobal：trySpawn 在本节点上限（现状）之后查全局总量——
//     满 → 回退本节点预留、请求交给排队路径（与本地池满同款语义）；
//     Redis 不可用 → **fail-open**（退化为仅本节点上限 + 限频告警）。
//     fail-open 裁决：容量门是可用性门不是安全门——Redis 抖动时拒绝全部
//     冷启动（fail-closed）会把控制面故障放大成执行面全面不可用；而 M8
//     死节点收敛是数据安全面（fail-safe：快照不可用跳过收敛、绝不误删
//     健康记录），两者语义方向相反，不得混淆。
//  2. refreshCapacity：本节点容量键的维护——值 = 进程内权威计数
//     （residentTotal，SET 实际值而非增量运算，写丢失/重试不产生漂移），
//     刷新点与 residentTotal 同步：spawn 成功 +1 / terminate 回收 -1 /
//     reaper 回收 -1；reaper 每轮无条件再刷一次保 TTL（无流量实例不被
//     键过期从全局求和中漏计）。节点死后键在 capacityKeyTTL 内消失，
//     全局求和自动不再计入死节点。
package dispatcher

import (
	"context"
	"log/slog"
)

// tryReserveGlobal 报告本节点在全局配额下是否可再占一个常驻名额（M4）。
// 调用点在 trySpawn 已完成本节点预留之后（预留先行——全局门拒绝时负责回退）。
// 返回 false = 全局总量已满（调用方回退预留、交给排队路径）。
//
// 全局上限未配置（<=0）恒 true 且不产生任何 Redis 读——单机/未设全局上限
// 的部署冷启动热路径零额外往返。求和含本节点容量键（值可能滞后于刚完成的
// 本节点预留一个增量：刷新点在 spawn 成功而非预留时，任务口径）——门是软
// 的，精度在 ±1 常驻额内，与 fail-open 同属可接受近似。
func (p *PoolManager) tryReserveGlobal(ctx context.Context) bool {
	if p.cfg.MaxResidentInstancesGlobal <= 0 {
		return true
	}
	total, err := p.registry.SumNodeCapacity(ctx)
	if err != nil {
		// fail-open（见文件头裁决）：Redis 不可用退化为仅本节点上限。
		p.warnCapacityFailure(err)
		return true
	}
	return total < p.cfg.MaxResidentInstancesGlobal
}

// refreshCapacity 把本节点容量键刷新为当前进程内常驻总量（best-effort）：
// 独立短超时 context（不占用调用路径预算），失败限频告警。nodeID 为空
// （未装配节点身份的单测缺省形态）跳过——不产生无主容量键。
func (p *PoolManager) refreshCapacity() {
	if p.nodeID == "" {
		return
	}
	p.mu.Lock()
	total := p.residentTotal
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), capacityWriteTimeout)
	defer cancel()
	if err := p.registry.SaveNodeCapacity(ctx, p.nodeID, total, capacityKeyTTL); err != nil {
		p.warnCapacityFailure(err)
	}
}

// warnCapacityFailure 落一次容量通道故障告警（限频 spawnWarnInterval，与
// spawn 失败告警同窗口——trySpawn 按 PollInterval 重试，不拦即刷屏）。
func (p *PoolManager) warnCapacityFailure(err error) {
	p.mu.Lock()
	now := p.clock()
	if now.Sub(p.lastCapWarnAt) < spawnWarnInterval {
		p.mu.Unlock()
		return
	}
	p.lastCapWarnAt = now
	p.mu.Unlock()
	slog.Warn("dispatcher: node capacity channel degraded; global resident limit falls back to per-node limit",
		"node", p.nodeID, "error", err)
}
