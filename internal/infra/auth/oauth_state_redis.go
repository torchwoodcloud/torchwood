package auth

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const oauthStateTTL = 10 * time.Minute

type RedisOAuthStateStore struct {
	nonces domainauth.NonceStore
}

func NewRedisOAuthStateStore(rdb *redis.Client) *RedisOAuthStateStore {
	return newOAuthStateStore(NewRedisNonceStore(rdb))
}

func newOAuthStateStore(nonces domainauth.NonceStore) *RedisOAuthStateStore {
	return &RedisOAuthStateStore{nonces: nonces}
}

func (s *RedisOAuthStateStore) Save(ctx context.Context, state domainauth.OAuthState, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = oauthStateTTL
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return status.Error(codes.Internal, "oauth state encode failed")
	}
	if err := s.nonces.Put(ctx, oauthStateKey(state.StateID), string(payload), ttl); err != nil {
		return status.Error(codes.Internal, "oauth state store failed")
	}
	return nil
}

// Consume atomically fetches and deletes the state via GETDEL so a state can
// only be redeemed once, closing the concurrent-callback replay window.
func (s *RedisOAuthStateStore) Consume(ctx context.Context, stateID string) (*domainauth.OAuthState, error) {
	raw, err := s.nonces.Consume(ctx, oauthStateKey(stateID))
	if err != nil {
		return nil, status.Error(codes.Internal, "oauth state lookup failed")
	}
	if raw == "" {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired oauth state")
	}
	var state domainauth.OAuthState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, status.Error(codes.Internal, "oauth state decode failed")
	}
	return &state, nil
}

func oauthStateKey(stateID string) string {
	return "Torchwood:oauth:state:" + stateID
}

var _ domainauth.OAuthStateStore = (*RedisOAuthStateStore)(nil)
