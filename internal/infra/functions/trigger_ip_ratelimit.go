package functions

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// triggerIPWindowScript 原子执行 INCR + 首次 EXPIRE 并返回 [count, ttl]
// （与 infra/auth ratelimit_redis 同一固定窗口模式；拒绝时 ttl 即窗口剩余
// 秒数，供 Retry-After）。崩溃安全：不存在「计数已增但 TTL 未设」中间态。
const triggerIPWindowScript = `
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return {count, redis.call('TTL', KEYS[1])}
`

// TriggerIPRateLimiter 是 HTTP 触发器公开端点的 per-IP 固定窗口限频器
// （P1 触发器模块，设计 §3）：键 torchwood:ftrig:ip:{ip}，配额独立于通用
// 限流（默认 3000/min——微信回调出口 IP 段集中，沿用全局 300/min 会在
// 发奖风暴下 429 → 重试耗尽 → 事件丢失；可经
// functions.trigger.http_ip_per_minute 配置）。
type TriggerIPRateLimiter struct {
	rdb    *redis.Client
	limit  int
	window time.Duration
}

func NewTriggerIPRateLimiter(rdb *redis.Client, cfg *config.AppConfig) *TriggerIPRateLimiter {
	limit := int(cfg.GetFunctions().GetTrigger().GetHttpIpPerMinute())
	if limit <= 0 {
		limit = 3000
	}
	return &TriggerIPRateLimiter{rdb: rdb, limit: limit, window: time.Minute}
}

// AllowTriggerIP 报告该 IP 本窗口内是否仍可请求；拒绝时返回窗口剩余时长
// （Retry-After）。Redis 故障 fail-closed（返回 error，调用方以 5xx 让回调
// 方重试——函数链路整体依赖 Redis，Q1 拍板语义一致）。
func (l *TriggerIPRateLimiter) AllowTriggerIP(ctx context.Context, ip string) (bool, time.Duration, error) {
	if ip == "" {
		return true, 0, nil
	}
	res, err := l.rdb.Eval(ctx, triggerIPWindowScript, []string{"torchwood:ftrig:ip:" + ip}, int64(l.window.Seconds())).Slice()
	if err != nil {
		return false, 0, err
	}
	count, _ := res[0].(int64)
	ttl, _ := res[1].(int64)
	if count > int64(l.limit) {
		if ttl < 0 {
			ttl = 0
		}
		return false, time.Duration(ttl) * time.Second, nil
	}
	return true, 0, nil
}
