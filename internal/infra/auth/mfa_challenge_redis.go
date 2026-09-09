package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	mfaChallengeTTL     = 5 * time.Minute
	mfaChallengeKeyPre  = "Torchwood:mfa:challenge:"
	mfaChallengeUserPre = "Torchwood:mfa:challenge:user:"
	mfaChallengeMissing = "invalid or expired challenge"
)

// RedisMFAChallengeStore stores one-time MFA login challenge tokens.
// Token KV 走 NonceStore；用户索引仍用 Redis SET（RevokeByUser）。
type RedisMFAChallengeStore struct {
	nonces domainauth.NonceStore
	rdb    *redis.Client
}

func NewRedisMFAChallengeStore(rdb *redis.Client) domainauth.MFAChallengeStore {
	return newMFAChallengeStore(NewRedisNonceStore(rdb), rdb)
}

func newMFAChallengeStore(nonces domainauth.NonceStore, rdb *redis.Client) *RedisMFAChallengeStore {
	return &RedisMFAChallengeStore{nonces: nonces, rdb: rdb}
}

func (s *RedisMFAChallengeStore) Create(ctx context.Context, projectID, userID string) (string, time.Time, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, status.Error(codes.Internal, "mfa challenge generation failed")
	}
	token := hex.EncodeToString(buf)
	key := mfaChallengeKeyPre + token
	value := projectID + ":" + userID
	if err := s.nonces.Put(ctx, key, value, mfaChallengeTTL); err != nil {
		return "", time.Time{}, status.Error(codes.Internal, "mfa challenge store failed")
	}
	// 用户索引：删除因子时作废该用户全部未消费挑战。
	idxKey := mfaChallengeUserKey(projectID, userID)
	if err := s.rdb.SAdd(ctx, idxKey, token).Err(); err != nil {
		return "", time.Time{}, status.Error(codes.Internal, "mfa challenge store failed")
	}
	if err := s.rdb.Expire(ctx, idxKey, mfaChallengeTTL).Err(); err != nil {
		return "", time.Time{}, status.Error(codes.Internal, "mfa challenge store failed")
	}
	return token, time.Now().Add(mfaChallengeTTL), nil
}

func (s *RedisMFAChallengeStore) Consume(ctx context.Context, token string) (string, string, error) {
	if token == "" {
		return "", "", status.Error(codes.Unauthenticated, mfaChallengeMissing)
	}
	value, err := s.nonces.Consume(ctx, mfaChallengeKeyPre+token)
	if err != nil {
		return "", "", status.Error(codes.Internal, "mfa challenge lookup failed")
	}
	if value == "" {
		// 不区分无效/过期/已用，防探测。
		return "", "", status.Error(codes.Unauthenticated, mfaChallengeMissing)
	}
	projectID, userID, ok := strings.Cut(value, ":")
	if !ok || projectID == "" || userID == "" {
		return "", "", status.Error(codes.Unauthenticated, mfaChallengeMissing)
	}
	_ = s.rdb.SRem(ctx, mfaChallengeUserKey(projectID, userID), token).Err()
	return projectID, userID, nil
}

// RevokeByUser 作废该用户全部未消费的挑战（删除 MFA 因子时调用）。
func (s *RedisMFAChallengeStore) RevokeByUser(ctx context.Context, projectID, userID string) error {
	idxKey := mfaChallengeUserKey(projectID, userID)
	tokens, err := s.rdb.SMembers(ctx, idxKey).Result()
	if err != nil {
		return status.Error(codes.Internal, "mfa challenge revocation failed")
	}
	for _, token := range tokens {
		if _, err := s.nonces.Consume(ctx, mfaChallengeKeyPre+token); err != nil {
			return status.Error(codes.Internal, "mfa challenge revocation failed")
		}
	}
	if err := s.rdb.Del(ctx, idxKey).Err(); err != nil {
		return status.Error(codes.Internal, "mfa challenge revocation failed")
	}
	return nil
}

func mfaChallengeUserKey(projectID, userID string) string {
	return mfaChallengeUserPre + projectID + ":" + userID
}

var _ domainauth.MFAChallengeStore = (*RedisMFAChallengeStore)(nil)
