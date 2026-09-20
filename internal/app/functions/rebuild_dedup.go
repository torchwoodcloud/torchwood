package functions

import (
	"context"
	"time"
)

// RebuildDedup 是镜像缺失自动重建的在途去重端口（跨进程；缺陷 B——server
// 与 worker 的执行错误路径都会触发重建，进程内 map 无法跨副本去重，并发
// 重建同一 deployment 存在终态竞态）。语义：
//   - TryAcquire 原子（SETNX 语义）：首个调用方抢到键，其余调用方得到
//     false 让路；
//   - ttl 兜底进程崩溃后的键残留：取构建超时预算 + 余量（调用方组装），
//     正常路径构建结束主动 Release，TTL 只是兜底；
//   - Release 幂等：重复释放/释放不存在的键都不得报错。
//
// 实现在 infra/functions（Redis SETNX + TTL）；nil = 回落进程内 map 去重
// （单副本语义，旧构造/测试保持兼容）。端口留在 app 层（消费方声明）：
// infra 实现经组合根 wire.Bind 接入，infra 不反向 import app。
type RebuildDedup interface {
	TryAcquire(ctx context.Context, key string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, key string) error
}
