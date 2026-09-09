package clients

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
)

// NewRedisClient 构造仅依赖 Redis 的客户端（functions-dispatcher 独立进程
// 用：零 daemon/DB 依赖是方案③的装配意义所在，不能为拿 Redis 被迫配置
// Postgres DSN）。语义与 NewDataClients 的 Redis 支路一致：addr 必填 +
// 5s Ping 兜底。
func NewRedisClient(cfg *config.AppConfig) (*redis.Client, func(), error) {
	rdb, err := newRedis(cfg.GetData().GetRedis())
	if err != nil {
		return nil, nil, err
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = rdb.Ping(pingCtx).Err()
	cancel()
	if err != nil {
		_ = rdb.Close()
		return nil, nil, fmt.Errorf("redis ping failed: %w", err)
	}
	return rdb, func() { _ = rdb.Close() }, nil
}
