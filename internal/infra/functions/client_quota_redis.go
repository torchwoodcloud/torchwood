package functions

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
)

// clientQuotaWindowScript 原子执行 INCR + 首次 EXPIRE 并返回 [count, ttl]
// （与通用限流/触发器 per-IP 限频同一固定窗口模式）。崩溃安全：不存在
// 「计数已增但 TTL 未设」的中间态。
const clientQuotaWindowScript = `
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return {count, redis.call('TTL', KEYS[1])}
`

// ClientQuotaLimiter 是客户端调用面每用户限频的 Redis 固定窗口实现（P2，
// 设计 §5）：键 torchwood:fnq:{project}:{function}:{user}:{bucket} 由 app 层
// 组装（bucket 按窗口粒度：minute=YYYYMMDDHHMM、hour=YYYYMMDDHH、
// day=UTC YYYYMMDD）。先限频后执行（先 INCR 后预占），与通用限流同原子性。
type ClientQuotaLimiter struct {
	rdb *redis.Client
}

func NewClientQuotaLimiter(rdb *redis.Client) *ClientQuotaLimiter {
	return &ClientQuotaLimiter{rdb: rdb}
}

// Allow 取一次配额；窗口结束时刻由 app 层按 bucket 数学给出（EXPIRE 依据 +
// 拒绝时的 RetryInfo）。Redis 故障返回 error——故障降级（按 function_executions
// 计数 / DB 亦不可用 fail-closed）由 app 层裁决。
func (l *ClientQuotaLimiter) Allow(ctx context.Context, key string, limit int, windowEnd time.Time) (domainfunctions.QuotaResult, error) {
	res := domainfunctions.QuotaResult{WindowEnd: windowEnd}
	if key == "" {
		res.Allowed = true
		return res, nil
	}
	// TTL 至少 1s：分钟粒度 bucket 在窗口尾部的剩余秒数可能为 0。
	ttl := time.Until(windowEnd)
	if ttl < time.Second {
		ttl = time.Second
	}
	out, err := l.rdb.Eval(ctx, clientQuotaWindowScript, []string{key}, int64(ttl.Seconds())).Slice()
	if err != nil {
		return res, err
	}
	count, _ := out[0].(int64)
	res.Allowed = count <= int64(limit)
	return res, nil
}

var _ domainfunctions.ClientQuotaLimiter = (*ClientQuotaLimiter)(nil)
