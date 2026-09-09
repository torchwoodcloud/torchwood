package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// RedisNonceStore 是一次性挑战的 Redis KV 适配器。
type RedisNonceStore struct {
	rdb *redis.Client
}

func NewRedisNonceStore(rdb *redis.Client) *RedisNonceStore {
	return &RedisNonceStore{rdb: rdb}
}

func (s *RedisNonceStore) Put(ctx context.Context, key, value string, ttl time.Duration) error {
	if s.rdb == nil || key == "" {
		return nil
	}
	if err := s.rdb.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("nonce put: %w", err)
	}
	return nil
}

func (s *RedisNonceStore) PutNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	if s.rdb == nil || key == "" {
		return true, nil
	}
	ok, err := s.rdb.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("nonce putnx: %w", err)
	}
	return ok, nil
}

func (s *RedisNonceStore) Get(ctx context.Context, key string) (string, error) {
	if s.rdb == nil || key == "" {
		return "", nil
	}
	value, err := s.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("nonce get: %w", err)
	}
	return value, nil
}

func (s *RedisNonceStore) Consume(ctx context.Context, key string) (string, error) {
	if s.rdb == nil || key == "" {
		return "", nil
	}
	value, err := s.rdb.GetDel(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("nonce consume: %w", err)
	}
	return value, nil
}

var _ domainauth.NonceStore = (*RedisNonceStore)(nil)
