package idgen

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	domainidgen "github.com/torchwoodcloud/torchwood/internal/domain/idgen"
	pkgidgen "github.com/torchwoodcloud/torchwood/pkg/idgen"
)

// randomSetTTL 是 random 策略保留集合的 TTL：集合只增不减，长期运行会
// 无界增长；每次成功保留 ID 时刷新 TTL，空闲后集合自动过期（释放后 ID
// 理论上可被重新发放，与随机空间大小相比可忽略）。
const randomSetTTL = 30 * 24 * time.Hour

// maxRandomSetSize 是 random 策略保留集合的容量上限：超过后拒绝生成并
// Warn，兜底防御「短随机长度 + 长期高频生成」导致集合无界膨胀耗尽
// Redis 内存。该值只是护栏，不做精确配额（并发下集合可能瞬时略超）。
var maxRandomSetSize int64 = 1_000_000

// ErrRandomSetFull 表示 random 策略的保留集合已达容量上限，拒绝生成。
var ErrRandomSetFull = errors.New("random id set reached capacity limit")

func (s *Service) nextRandom(ctx context.Context, projectID string, resource domainidgen.Resource) (string, error) {
	if s.rdb == nil {
		return "", pkgidgen.ErrRandomRedisRequired
	}
	cfg := s.randomCfg
	setKey := fmt.Sprintf("%s:%s:%s", s.randomPrefix, projectID, resource)

	for range cfg.MaxRetries {
		candidate, err := pkgidgen.RandomString(cfg)
		if err != nil {
			return "", err
		}
		size, err := s.rdb.SCard(ctx, setKey).Result()
		if err != nil {
			return "", fmt.Errorf("random id reservation failed: %w", err)
		}
		if size >= maxRandomSetSize {
			slog.Warn("random id set reached capacity, refusing to generate",
				slog.String("set_key", setKey), slog.Int64("size", size))
			return "", ErrRandomSetFull
		}
		pipe := s.rdb.TxPipeline()
		sadd := pipe.SAdd(ctx, setKey, candidate)
		pipe.Expire(ctx, setKey, randomSetTTL)
		if _, err := pipe.Exec(ctx); err != nil {
			return "", fmt.Errorf("random id reservation failed: %w", err)
		}
		added, err := sadd.Result()
		if err != nil {
			return "", fmt.Errorf("random id reservation failed: %w", err)
		}
		if added == 1 {
			return candidate, nil
		}
	}
	return "", pkgidgen.ErrRandomReservationFailed
}
