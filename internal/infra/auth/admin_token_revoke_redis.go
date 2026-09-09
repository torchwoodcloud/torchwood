package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RedisAdminTokenRevokeStore stores admin sign-out timestamps in Redis.
type RedisAdminTokenRevokeStore struct {
	rdb *redis.Client
}

func NewRedisAdminTokenRevokeStore(rdb *redis.Client) *RedisAdminTokenRevokeStore {
	return &RedisAdminTokenRevokeStore{rdb: rdb}
}

func (s *RedisAdminTokenRevokeStore) RevokeBefore(ctx context.Context, adminID string, revokedAt time.Time, ttl time.Duration) error {
	if adminID == "" {
		return nil
	}
	key := adminTokenRevokeKey(adminID)
	existing, err := s.RevokedBefore(ctx, adminID)
	if err != nil {
		return err
	}
	if !existing.IsZero() && existing.After(revokedAt) {
		revokedAt = existing
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	if err := s.rdb.Set(ctx, key, revokedAt.Unix(), ttl).Err(); err != nil {
		return status.Error(codes.Internal, "admin token revoke store failed")
	}
	return nil
}

func (s *RedisAdminTokenRevokeStore) RevokedBefore(ctx context.Context, adminID string) (time.Time, error) {
	if adminID == "" {
		return time.Time{}, nil
	}
	raw, err := s.rdb.Get(ctx, adminTokenRevokeKey(adminID)).Int64()
	if err == redis.Nil {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, status.Error(codes.Internal, "admin token revoke lookup failed")
	}
	return time.Unix(raw, 0), nil
}

func adminTokenRevokeKey(adminID string) string {
	return fmt.Sprintf("Torchwood:admin:revoked:%s", adminID)
}

var _ domainauth.AdminTokenRevokeStore = (*RedisAdminTokenRevokeStore)(nil)
