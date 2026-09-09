package functions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
)

// 执行 token（P0 执行身份，设计 §1；owner 拍板：Redis 不透明 token，主动 DEL
// 吊销）：
//   - 格式：ExecutionTokenPrefix（"twx_"）+ 32 字节 crypto/rand base64url；
//   - 键：torchwood:exec-token:sha256hex(token)——服务端只存哈希，不存原值
//     （与 API key secret_hash 同一收敛原则）；
//   - 值：ExecutionTokenInfo 的 JSON 投影；
//   - TTL：函数超时 + 60s 宽限，仅作崩溃兜底；正常路径由调用方在执行结束
//     （成功/失败/panic）后 Revoke 主动吊销。
//
// fail-closed：Mint/Validate/Revoke 的 Redis 故障原样上抛为 error，由调用方
// 区分（Validate 基础设施故障 ≠ token 不存在，前者 Internal 拒绝而非 401）。
const executionTokenKeyPrefix = "torchwood:exec-token:"

// executionTokenRandomBytes 是 token 随机段长度：32 字节 = 256 bit 熵。
const executionTokenRandomBytes = 32

// RedisExecutionTokenService 是 domainfunctions.ExecutionTokenService 的
// Redis 实现（与 ratelimit/one-time token 同层：internal/infra/auth）。
type RedisExecutionTokenService struct {
	rdb *redis.Client
}

func NewRedisExecutionTokenService(rdb *redis.Client) *RedisExecutionTokenService {
	return &RedisExecutionTokenService{rdb: rdb}
}

var _ domainfunctions.ExecutionTokenService = (*RedisExecutionTokenService)(nil)

// executionTokenKey 由 token 原值派生存储键（sha256hex）。
func executionTokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return executionTokenKeyPrefix + hex.EncodeToString(sum[:])
}

// Mint 铸造新执行 token 并以 JSON 投影落 Redis（SET + TTL）。
func (s *RedisExecutionTokenService) Mint(ctx context.Context, info domainfunctions.ExecutionTokenInfo, ttl time.Duration) (string, error) {
	if s.rdb == nil {
		return "", fmt.Errorf("execution token store unavailable")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("execution token ttl must be positive")
	}
	if info.ProjectID == "" || info.FunctionID == "" || info.ExecutionID == "" {
		return "", fmt.Errorf("execution token info requires project/function/execution id")
	}
	raw := make([]byte, executionTokenRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate execution token: %w", err)
	}
	token := shared.ExecutionTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	payload, err := json.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("marshal execution token info: %w", err)
	}
	if err := s.rdb.Set(ctx, executionTokenKey(token), payload, ttl).Err(); err != nil {
		return "", fmt.Errorf("store execution token: %w", err)
	}
	return token, nil
}

// Validate 校验 token：不存在/已吊销/已过期返回 (nil, nil)；Redis 故障返回
// error（fail-closed，调用方必须区分——不得把基础设施故障降级为匿名）。
func (s *RedisExecutionTokenService) Validate(ctx context.Context, token string) (*domainfunctions.ExecutionTokenInfo, error) {
	if s.rdb == nil {
		return nil, fmt.Errorf("execution token store unavailable")
	}
	if token == "" {
		return nil, nil
	}
	payload, err := s.rdb.Get(ctx, executionTokenKey(token)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load execution token: %w", err)
	}
	var info domainfunctions.ExecutionTokenInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		// 值损坏视同 token 无效（不信任半写状态）。
		return nil, nil
	}
	return &info, nil
}

// Revoke 主动吊销 token（DEL，幂等；token 不存在时静默成功）。
func (s *RedisExecutionTokenService) Revoke(ctx context.Context, token string) error {
	if s.rdb == nil {
		return fmt.Errorf("execution token store unavailable")
	}
	if token == "" {
		return nil
	}
	if err := s.rdb.Del(ctx, executionTokenKey(token)).Err(); err != nil {
		return fmt.Errorf("revoke execution token: %w", err)
	}
	return nil
}
