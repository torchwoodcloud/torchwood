package functions

import (
	"context"
	"time"
)

// QuotaResult 是一次客户端调用配额判定的结果。
type QuotaResult struct {
	// Allowed 报告本次是否放行。
	Allowed bool
	// WindowEnd 是当前固定窗口的结束时刻（超限时构造 RetryInfo 的依据）；
	// 放行时同样携带（幂等语义，调用方可忽略）。
	WindowEnd time.Time
}

// ClientQuotaLimiter 是客户端调用面每用户限频的端口（P2，设计 §5）：
// 生产实现为 Redis 固定窗口（torchwood:fnq:{project}:{function}:{user}:{bucket}）；
// 故障语义由实现返回 error，调用方（app 层）负责 DB 降级计数与 fail-closed。
type ClientQuotaLimiter interface {
	// Allow 取一次配额（固定窗口 INCR+EXPIRE 原子语义）：key 由 app 层按
	// 窗口 bucket 组装（含 project/function/user 维度），limit 是函数配置的
	// 每窗口配额，windowEnd 是窗口结束时刻（EXPIRE 依据与拒绝时的 RetryInfo）。
	Allow(ctx context.Context, key string, limit int, windowEnd time.Time) (QuotaResult, error)
}
