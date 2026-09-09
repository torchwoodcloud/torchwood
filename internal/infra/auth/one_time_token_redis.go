package auth

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RedisOneTimeTokenStore stores one-time consumption markers via NonceStore.
type RedisOneTimeTokenStore struct {
	nonces domainauth.NonceStore
}

func NewRedisOneTimeTokenStore(rdb *redis.Client) *RedisOneTimeTokenStore {
	return newOneTimeTokenStore(NewRedisNonceStore(rdb))
}

func newOneTimeTokenStore(nonces domainauth.NonceStore) *RedisOneTimeTokenStore {
	return &RedisOneTimeTokenStore{nonces: nonces}
}

// Register uses SETNX so a jti can only be registered once (collision guard),
// and the record expires with the token's TTL.
func (s *RedisOneTimeTokenStore) Register(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	if s.nonces == nil || key == "" {
		return true, nil
	}
	ok, err := s.nonces.PutNX(ctx, key, value, ttl)
	if err != nil {
		return false, status.Error(codes.Internal, "one-time token store failed")
	}
	return ok, nil
}

// Consume atomically fetches and deletes the record (GETDEL); an empty value
// with nil error means the token was never issued, already consumed or expired.
func (s *RedisOneTimeTokenStore) Consume(ctx context.Context, key string) (string, error) {
	if s.nonces == nil || key == "" {
		return "", nil
	}
	value, err := s.nonces.Consume(ctx, key)
	if err != nil {
		return "", status.Error(codes.Internal, "one-time token store failed")
	}
	return value, nil
}

var _ domainauth.OneTimeTokenStore = (*RedisOneTimeTokenStore)(nil)
