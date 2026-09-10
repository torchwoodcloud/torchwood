package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	otpChallengeTTL   = 5 * time.Minute
	otpSendCooldown   = 60 * time.Second
	otpIPWindow       = time.Hour
	otpIPMaxPerWindow = 10
	otpMaxAttempts    = 5
)

// otpHMACKeyNamespace 已收敛至 jwtparser.PurposeOTP（P3-6），保留注释说明历史前缀。
// 旧值 "torchwood-otp:" 现由 DeriveKey(HMAC-SHA256) 统一派生。

// otpVerifyScript 原子完成"读取 challenge -> 校验归属 -> 校验尝试次数 -> 比对验证码 ->
// 成功删除 / 失败递增尝试次数"，避免 GET-改-SET 竞态导致正确验证码被并发重放签发多个会话。
// challenge 以 JSON 字符串落 NonceStore（与 account token 同形）。
// 返回值：ok / notfound / mismatch / locked / badcode。
const otpVerifyScript = `
local raw = redis.call('GET', KEYS[1])
if not raw then
  return 'notfound'
end
local rec = cjson.decode(raw)
if rec.project_id ~= ARGV[1] or rec.channel ~= ARGV[2] or rec.target ~= ARGV[3] then
  return 'mismatch'
end
local attempts = tonumber(rec.attempts) or 0
if attempts >= tonumber(ARGV[5]) then
  redis.call('DEL', KEYS[1])
  return 'locked'
end
if rec.code_hash ~= ARGV[4] then
  rec.attempts = attempts + 1
  local ttl = redis.call('PTTL', KEYS[1])
  if ttl <= 0 then
    redis.call('DEL', KEYS[1])
    return 'notfound'
  end
  redis.call('SET', KEYS[1], cjson.encode(rec), 'PX', ttl)
  return 'badcode'
end
redis.call('DEL', KEYS[1])
return 'ok'
`

type otpChallengeRecord struct {
	ProjectID string `json:"project_id"`
	Channel   string `json:"channel"`
	Target    string `json:"target"`
	CodeHash  string `json:"code_hash"`
	Attempts  int    `json:"attempts"`
}

// RedisOTPChallengeStore stores OTP challenges in Redis.
type RedisOTPChallengeStore struct {
	nonces domainauth.NonceStore
	rdb    *redis.Client
	// hmacKey 用于对 OTP 验证码做 HMAC-SHA256 存储，Redis 数据泄露时无法对 6 位验证码离线爆破。
	hmacKey []byte
}

func NewRedisOTPChallengeStore(rdb *redis.Client, cfg *config.AppConfig) *RedisOTPChallengeStore {
	return newOTPChallengeStore(NewRedisNonceStore(rdb), rdb, cfg)
}

func newOTPChallengeStore(nonces domainauth.NonceStore, rdb *redis.Client, cfg *config.AppConfig) *RedisOTPChallengeStore {
	return &RedisOTPChallengeStore{
		nonces:  nonces,
		rdb:     rdb,
		hmacKey: jwtparser.DeriveKey(cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeOTP),
	}
}

func (s *RedisOTPChallengeStore) hashCode(code string) string {
	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *RedisOTPChallengeStore) CheckSendRateLimit(ctx context.Context, projectID, target, ip string) error {
	sendKey := fmt.Sprintf("Torchwood:otp:send:%s:%s", projectID, target)
	ok, err := s.nonces.PutNX(ctx, sendKey, "1", otpSendCooldown)
	if err != nil {
		return status.Error(codes.Internal, "otp rate limit check failed")
	}
	if !ok {
		return status.Error(codes.ResourceExhausted, "otp send cooldown active")
	}

	if ip == "" {
		return nil
	}
	ipKey := fmt.Sprintf("Torchwood:otp:ip:%s:%s", projectID, ip)
	// Round3 H6-4：INCR + 首次 EXPIRE 原子化，崩溃不留下无 TTL 计数键。
	count, err := incrWithTTL(ctx, s.rdb, ipKey, otpIPWindow)
	if err != nil {
		return status.Error(codes.Internal, "otp ip rate limit check failed")
	}
	if count > otpIPMaxPerWindow {
		return status.Error(codes.ResourceExhausted, "otp ip rate limit exceeded")
	}
	return nil
}

func (s *RedisOTPChallengeStore) CreateEmailChallenge(ctx context.Context, projectID, email, code string) (string, time.Time, error) {
	return s.createChallenge(ctx, projectID, domainauth.OTPChannelEmail, email, code)
}

func (s *RedisOTPChallengeStore) VerifyEmailChallenge(ctx context.Context, projectID, challengeID, email, code string) error {
	return s.verifyChallenge(ctx, projectID, challengeID, domainauth.OTPChannelEmail, email, code)
}

func (s *RedisOTPChallengeStore) CreatePhoneChallenge(ctx context.Context, projectID, phone, code string) (string, time.Time, error) {
	return s.createChallenge(ctx, projectID, domainauth.OTPChannelPhone, phone, code)
}

func (s *RedisOTPChallengeStore) VerifyPhoneChallenge(ctx context.Context, projectID, challengeID, phone, code string) error {
	return s.verifyChallenge(ctx, projectID, challengeID, domainauth.OTPChannelPhone, phone, code)
}

func (s *RedisOTPChallengeStore) createChallenge(ctx context.Context, projectID, channel, target, code string) (string, time.Time, error) {
	challengeID := newChallengeID()
	expireAt := time.Now().Add(otpChallengeTTL)
	payload, err := json.Marshal(otpChallengeRecord{
		ProjectID: projectID,
		Channel:   channel,
		Target:    target,
		CodeHash:  s.hashCode(code),
		Attempts:  0,
	})
	if err != nil {
		return "", time.Time{}, status.Error(codes.Internal, "otp challenge store failed")
	}
	if err := s.nonces.Put(ctx, challengeKey(challengeID), string(payload), otpChallengeTTL); err != nil {
		return "", time.Time{}, status.Error(codes.Internal, "otp challenge store failed")
	}
	return challengeID, expireAt, nil
}

func (s *RedisOTPChallengeStore) verifyChallenge(ctx context.Context, projectID, challengeID, channel, target, code string) error {
	// M5 C8：OTP verify 是公开消费口，按 IP 频控（对齐 account token 消费口
	// 的 30 次/15min，共用同一计数桶防跨入口叠加预算）。
	if ip := contexts.ClientInfoFrom(ctx).IP; ip != "" {
		if err := checkTokenVerifyIPLimit(ctx, s.rdb, ip); err != nil {
			return err
		}
	}
	key := challengeKey(challengeID)
	result, err := s.rdb.Eval(ctx, otpVerifyScript, []string{key},
		projectID, channel, target, s.hashCode(code), otpMaxAttempts).Text()
	if err != nil {
		if strings.Contains(err.Error(), "WRONGTYPE") {
			// 旧版本 HASH 格式的在途 challenge 按无效处理（TTL 仅 5 分钟）。
			return status.Error(codes.Unauthenticated, "invalid or expired otp challenge")
		}
		return status.Error(codes.Internal, "otp challenge verify failed")
	}

	switch result {
	case "ok":
		return nil
	case "locked":
		return status.Error(codes.ResourceExhausted, "otp attempts exceeded")
	case "badcode":
		return status.Error(codes.Unauthenticated, "invalid otp code")
	default: // notfound / mismatch
		return status.Error(codes.Unauthenticated, "invalid or expired otp challenge")
	}
}

func challengeKey(challengeID string) string {
	return "Torchwood:otp:ch:" + challengeID
}

func newChallengeID() string {
	return idgen.UUID().String()
}

var _ domainauth.OTPChallengeStore = (*RedisOTPChallengeStore)(nil)
