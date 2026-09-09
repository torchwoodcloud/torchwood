// Package billing 提供用量计数 Redis 适配器（设计 §4.2：INCRBY + TTL ≥ 48h）。
package billing

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
)

const (
	keyPrefix = "Torchwood:usage:"
	scanCount = 256
)

// incrByTTLScript 原子 INCRBY + 仅在无 TTL 时 EXPIRE（不刷新已有 TTL，
// 避免热键被无限续命；首次写入保证 ≥ 48h）。
const incrByTTLScript = `
local n = redis.call('INCRBY', KEYS[1], ARGV[1])
if redis.call('TTL', KEYS[1]) < 0 then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
end
return n
`

// setTTLScript 原子 SET + EXPIRE（storage_bytes 快照）。
const setTTLScript = `
redis.call('SET', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[2])
return ARGV[1]
`

// RedisCounter 实现 domain/billing.UsageCounter。
type RedisCounter struct {
	rdb *redis.Client
}

// NewRedisCounter 构造 Redis 用量计数器。
func NewRedisCounter(rdb *redis.Client) *RedisCounter {
	return &RedisCounter{rdb: rdb}
}

func usageKey(projectID, metric string, hour time.Time) string {
	return fmt.Sprintf("%s%d:%s:%s", keyPrefix, domainbilling.HourBucket(hour).Unix(), metric, projectID)
}

func hourPrefix(hour time.Time) string {
	return fmt.Sprintf("%s%d:", keyPrefix, domainbilling.HourBucket(hour).Unix())
}

func (c *RedisCounter) Incr(ctx context.Context, projectID, metric string, delta int64) error {
	return c.IncrAt(ctx, projectID, metric, time.Now(), delta)
}

func (c *RedisCounter) IncrAt(ctx context.Context, projectID, metric string, hour time.Time, delta int64) error {
	if projectID == "" || !domainbilling.KnownMetric(metric) || delta <= 0 {
		return nil
	}
	ttlSec := int64(domainbilling.BucketTTL / time.Second)
	_, err := c.rdb.Eval(ctx, incrByTTLScript, []string{usageKey(projectID, metric, hour)}, delta, ttlSec).Result()
	return err
}

func (c *RedisCounter) Set(ctx context.Context, projectID, metric string, hour time.Time, value int64) error {
	if projectID == "" || !domainbilling.KnownMetric(metric) || value < 0 {
		return nil
	}
	ttlSec := int64(domainbilling.BucketTTL / time.Second)
	_, err := c.rdb.Eval(ctx, setTTLScript, []string{usageKey(projectID, metric, hour)}, value, ttlSec).Result()
	return err
}

func (c *RedisCounter) Get(ctx context.Context, projectID, metric string, hour time.Time) (int64, error) {
	s, err := c.rdb.Get(ctx, usageKey(projectID, metric, hour)).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (c *RedisCounter) ListHour(ctx context.Context, hour time.Time) ([]domainbilling.Bucket, error) {
	hour = domainbilling.HourBucket(hour)
	match := hourPrefix(hour) + "*"
	var out []domainbilling.Bucket
	var cursor uint64
	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, match, scanCount).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			projectID, metric, ok := parseUsageKey(key, hour.Unix())
			if !ok {
				continue
			}
			n, err := c.rdb.Get(ctx, key).Int64()
			if err == redis.Nil {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, domainbilling.Bucket{
				ProjectID:   projectID,
				Metric:      metric,
				PeriodStart: hour,
				Value:       n,
			})
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return out, nil
}

func parseUsageKey(key string, hourUnix int64) (projectID, metric string, ok bool) {
	prefix := fmt.Sprintf("%s%d:", keyPrefix, hourUnix)
	rest, found := strings.CutPrefix(key, prefix)
	if !found {
		return "", "", false
	}
	metric, projectID, found = strings.Cut(rest, ":")
	if !found || !domainbilling.KnownMetric(metric) || projectID == "" {
		return "", "", false
	}
	return projectID, metric, true
}
