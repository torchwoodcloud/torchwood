package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	verificationTokenTTL = 24 * time.Hour
	recoveryTokenTTL     = time.Hour
	magicURLTokenTTL     = time.Hour
	// AccountTokenMaxAttempts 是错误 secret 尝试次数上限（与 OTP 对齐，5 次）；
	// 超过后锁定记录至 TTL 过期（P3-2：不再 DEL，便于观测与防重放）。
	AccountTokenMaxAttempts = 5

	// accountTokenVerifyIPWindow / MaxPerWindow 是公开消费口 IP 维度频控
	// （P3-2）：防止知道 project_id+user_id 的攻击者烧受害者待点击链接。
	accountTokenVerifyIPWindow       = 15 * time.Minute
	accountTokenVerifyIPMaxPerWindow = 30
)

type accountTokenRecord struct {
	ProjectID  string `json:"project_id"`
	UserID     string `json:"user_id"`
	Email      string `json:"email,omitempty"`
	Purpose    string `json:"purpose"`
	SecretHash string `json:"secret_hash"`
}

// RedisAccountTokenStore stores account verification and recovery tokens in Redis.
type RedisAccountTokenStore struct {
	nonces domainauth.NonceStore
	rdb    *redis.Client
}

func NewRedisAccountTokenStore(rdb *redis.Client) *RedisAccountTokenStore {
	return newAccountTokenStore(NewRedisNonceStore(rdb), rdb)
}

func newAccountTokenStore(nonces domainauth.NonceStore, rdb *redis.Client) *RedisAccountTokenStore {
	return &RedisAccountTokenStore{nonces: nonces, rdb: rdb}
}

func (s *RedisAccountTokenStore) CheckSendRateLimit(ctx context.Context, projectID, target, ip string) error {
	store := &RedisOTPChallengeStore{rdb: s.rdb, nonces: s.nonces}
	return store.CheckSendRateLimit(ctx, projectID, target, ip)
}

func (s *RedisAccountTokenStore) CreateVerificationToken(ctx context.Context, projectID, userID, email string) (string, time.Time, error) {
	return s.createToken(ctx, projectID, userID, email, domainauth.AccountTokenPurposeVerification, verificationTokenTTL)
}

func (s *RedisAccountTokenStore) VerifyVerificationToken(ctx context.Context, projectID, userID, secret string) error {
	return s.verifyToken(ctx, projectID, userID, secret, domainauth.AccountTokenPurposeVerification)
}

func (s *RedisAccountTokenStore) CreateRecoveryToken(ctx context.Context, projectID, userID, email string) (string, time.Time, error) {
	return s.createToken(ctx, projectID, userID, email, domainauth.AccountTokenPurposeRecovery, recoveryTokenTTL)
}

func (s *RedisAccountTokenStore) VerifyRecoveryToken(ctx context.Context, projectID, userID, secret string) error {
	return s.verifyToken(ctx, projectID, userID, secret, domainauth.AccountTokenPurposeRecovery)
}

func (s *RedisAccountTokenStore) CreateMagicURLToken(ctx context.Context, projectID, userID, email string) (string, string, time.Time, error) {
	secret, expireAt, err := s.createToken(ctx, projectID, userID, email, domainauth.AccountTokenPurposeMagicURL, magicURLTokenTTL)
	if err != nil {
		return "", "", time.Time{}, err
	}
	// 不透明 challengeID 与 secret 无关，仅用于 API 响应回传；secret 只在邮件链接里。
	challengeID, err := generateAccountTokenSecret()
	if err != nil {
		return "", "", time.Time{}, status.Error(codes.Internal, "account token generation failed")
	}
	return challengeID, secret, expireAt, nil
}

func (s *RedisAccountTokenStore) VerifyMagicURLToken(ctx context.Context, projectID, userID, secret string) error {
	return s.verifyToken(ctx, projectID, userID, secret, domainauth.AccountTokenPurposeMagicURL)
}

func (s *RedisAccountTokenStore) CreateEmailChangeToken(ctx context.Context, projectID, userID, email string) (string, time.Time, error) {
	return s.createToken(ctx, projectID, userID, email, domainauth.AccountTokenPurposeEmailChange, verificationTokenTTL)
}

// VerifyEmailChangeToken 原子消费邮箱变更 token 并返回 record 中的新邮箱。
func (s *RedisAccountTokenStore) VerifyEmailChangeToken(ctx context.Context, projectID, userID, secret string) (string, error) {
	return s.verifyTokenWithEmail(ctx, projectID, userID, secret, domainauth.AccountTokenPurposeEmailChange)
}

func (s *RedisAccountTokenStore) createToken(ctx context.Context, projectID, userID, email, purpose string, ttl time.Duration) (string, time.Time, error) {
	secret, err := generateAccountTokenSecret()
	if err != nil {
		return "", time.Time{}, status.Error(codes.Internal, "account token generation failed")
	}
	expireAt := time.Now().Add(ttl)
	record := accountTokenRecord{
		ProjectID:  projectID,
		UserID:     userID,
		Email:      email,
		Purpose:    purpose,
		SecretHash: HashOTP(secret),
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return "", time.Time{}, status.Error(codes.Internal, "account token encode failed")
	}
	key := accountTokenKey(purpose, projectID, userID)
	if err := s.nonces.Put(ctx, key, string(payload), ttl); err != nil {
		return "", time.Time{}, status.Error(codes.Internal, "account token store failed")
	}
	return secret, expireAt, nil
}

// accountTokenVerifyScript 原子完成「读取记录 -> 校验归属 -> 比对 secret ->
// 成功删除 / 错 secret 计数（超限锁定保留至 TTL，不 DEL）」：GET-改-SET 竞态会导致并发
// 双消费（recovery 双重置 / magic URL 双会话）与错 secret 作废链接（Round3
// H6-3：先 GETDEL 再比 hash 会在错 secret 时烧掉一次性 token）。
// P3-2：locked 保留至 TTL 供观测与防重放，不再 DEL。
// 返回值：ok:<email> / badsecret / locked / notfound。
const accountTokenVerifyScript = `
local raw = redis.call('GET', KEYS[1])
if not raw then
  return 'notfound'
end
local record = cjson.decode(raw)
if record.project_id ~= ARGV[3] or record.user_id ~= ARGV[4] or record.purpose ~= ARGV[5] then
  return 'mismatch'
end
if record.secret_hash ~= ARGV[1] then
  local attempts = (record.attempts or 0) + 1
  if attempts >= tonumber(ARGV[2]) then
    record.attempts = attempts
    local ttl = redis.call('PTTL', KEYS[1])
    if ttl > 0 then
      redis.call('SET', KEYS[1], cjson.encode(record), 'PX', ttl)
    else
      redis.call('SET', KEYS[1], cjson.encode(record))
    end
    return 'locked'
  end
  record.attempts = attempts
  local ttl = redis.call('PTTL', KEYS[1])
  if ttl > 0 then
    redis.call('SET', KEYS[1], cjson.encode(record), 'PX', ttl)
  else
    redis.call('SET', KEYS[1], cjson.encode(record))
  end
  return 'badsecret'
end
-- P3-2 把锁定改为保留记录后，成功路径必须显式检查锁定状态：否则超限后
-- 正确 secret 仍会匹配 hash 并 DEL+成功，锁定形同虚设（Round3 H6-3 契约：
-- 超限后正确 secret 也拒绝）。
if (record.attempts or 0) >= tonumber(ARGV[2]) then
  return 'locked'
end
redis.call('DEL', KEYS[1])
return 'ok:' .. tostring(record.email)
`

// verifyToken 通过 Lua 原子消费：校验与删除一体，杜绝并发双消费
// （recovery 双重置 / magic URL 双会话）。
func (s *RedisAccountTokenStore) verifyToken(ctx context.Context, projectID, userID, secret, purpose string) error {
	_, err := s.verifyTokenWithEmail(ctx, projectID, userID, secret, purpose)
	return err
}

// verifyTokenWithEmail 同 verifyToken，额外返回 record 中携带的 email
// （email_change 消费后需要新邮箱地址）。错误 secret 只计数不删除记录，
// 超限（AccountTokenMaxAttempts）锁定保留至 TTL；全部失败路径统一返回
// Unauthenticated（防枚举，Round3 H6-3）。公开消费口按 IP 额外限流
// （P3-2）。
func (s *RedisAccountTokenStore) verifyTokenWithEmail(ctx context.Context, projectID, userID, secret, purpose string) (string, error) {
	if ip := contexts.ClientInfoFrom(ctx).IP; ip != "" {
		if err := s.checkVerifyIPLimit(ctx, ip); err != nil {
			return "", err
		}
	}
	key := accountTokenKey(purpose, projectID, userID)
	result, err := s.rdb.Eval(ctx, accountTokenVerifyScript, []string{key},
		HashOTP(secret), AccountTokenMaxAttempts, projectID, userID, purpose).Text()
	if err != nil {
		return "", status.Error(codes.Internal, "account token verify failed")
	}
	switch {
	case strings.HasPrefix(result, "ok:"):
		return strings.TrimPrefix(result, "ok:"), nil
	default: // notfound / mismatch / badsecret / locked
		return "", status.Error(codes.Unauthenticated, "invalid or expired account token")
	}
}

func accountTokenKey(purpose, projectID, userID string) string {
	return fmt.Sprintf("Torchwood:account:token:%s:%s:%s", purpose, projectID, userID)
}

// checkVerifyIPLimit 对公开消费口（Verify*）按 IP 做固定窗口限流，超过
// accountTokenVerifyIPMaxPerWindow/15min 返回 ResourceExhausted（P3-2）。
func (s *RedisAccountTokenStore) checkVerifyIPLimit(ctx context.Context, ip string) error {
	return checkTokenVerifyIPLimit(ctx, s.rdb, ip)
}

// checkTokenVerifyIPLimit 是公开消费口（account token verify / OTP verify，
// M5 C8）共用的 IP 维度固定窗口频控：30 次/15min，共用同一计数桶——
// 防攻击者跨入口叠加尝试预算。
func checkTokenVerifyIPLimit(ctx context.Context, rdb *redis.Client, ip string) error {
	key := fmt.Sprintf("Torchwood:account:token:verify:ip:%s", ip)
	count, err := incrWithTTL(ctx, rdb, key, accountTokenVerifyIPWindow)
	if err != nil {
		return status.Error(codes.Internal, "account token verify rate limit check failed")
	}
	if count > accountTokenVerifyIPMaxPerWindow {
		return status.Error(codes.ResourceExhausted, "account token verify rate limit exceeded")
	}
	return nil
}

func generateAccountTokenSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

var _ domainauth.AccountTokenStore = (*RedisAccountTokenStore)(nil)
