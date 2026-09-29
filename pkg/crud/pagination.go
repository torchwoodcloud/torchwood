package crud

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// PageTokenVersion is the current version of page token format
	PageTokenVersion = "v1"

	// DefaultTokenTTL is the default time-to-live for page tokens
	DefaultTokenTTL = 24 * time.Hour

	// TokenModeLegacy represents offset-based token mode.
	TokenModeLegacy = "legacy"

	// MaxQueryOffset 上限：拒绝客户端伪造超深分页 token 打爆数据库。
	// 仅服务静态表/控制面 offset token（文档面已 keyset-only，原 documentdb
	// 侧同名上限随 C2 阶段①收敛删除——R4-J2-4 的对齐关系就此解除）。
	MaxQueryOffset = 10000

	// pageTokenKeyPurpose 是密钥派生的 purpose 域，与 pkg/jwtparser
	// DeriveKey(master, purpose) 同构（HMAC-SHA256(master, purpose)），
	// 本地实现以避免 pkg 内反向依赖。
	pageTokenKeyPurpose = "torchwood-page-token-v1"
)

// pageTokenSecret 进程级签名密钥（HMAC-SHA256，空串=未启用）。由组合根在
// 启动期通过 InitPageTokenSigning 注入（server 与 worker 都必须调用）；未注入
// 时编码退化为未签名 token、带签名的 token 解码被拒——保证灰度发布期间旧
// token 兼容，同时新签发 token 在任何配置正确的进程中都可验证。
var pageTokenSecret atomic.Value // string

// pageTokenMarshal 是 token 编码链路的序列化入口（var 仅为测试注入必败
// marshaler——PageTokenData 的 json.Marshal 实际不可失败，B8 判据要求该
// 错误路径可测）。
var pageTokenMarshal = json.Marshal

// InitPageTokenSigning 启用页 token 的 HMAC 签名与验签。master 为部署主密钥
// （security.jwt.secret）；实际签名密钥经 HMAC-SHA256(master, purpose) 派生，
// 与 JWT/OAuth 等域隔离。
func InitPageTokenSigning(master string) error {
	if strings.TrimSpace(master) == "" {
		return fmt.Errorf("page token signing requires a non-empty master secret")
	}
	mac := hmac.New(sha256.New, []byte(master))
	_, _ = mac.Write([]byte(pageTokenKeyPurpose))
	pageTokenSecret.Store(hex.EncodeToString(mac.Sum(nil)))
	return nil
}

// PageTokenSigningEnabled 报告当前进程是否已启用签名（测试与启动断言用）。
func PageTokenSigningEnabled() bool {
	return pageTokenKey() != ""
}

func pageTokenKey() string {
	if v, ok := pageTokenSecret.Load().(string); ok {
		return v
	}
	return ""
}

// seal 为 token 附加 HMAC 签名（若已启用签名）。失败仅可能因内部状态异常，
// 此时退化为未签名并在解码侧被拒（fail-closed），不影响可用性。
func seal(data *PageTokenData) {
	key := pageTokenKey()
	if key == "" {
		return
	}
	if sig, err := SignPageToken(data, key); err == nil {
		data.Sig = sig
	}
}

// PageTokenData represents the internal structure of a page token
// Following AIP-158: https://google.aip.dev/158
type PageTokenData struct {
	Version      string    `json:"v"`                       // Token format version
	Mode         string    `json:"mode,omitempty"`          // legacy
	Offset       int       `json:"offset"`                  // Current offset
	Created      time.Time `json:"created"`                 // Token creation time
	PageSize     int32     `json:"page_size"`               // Original page size
	OrderBy      string    `json:"order_by,omitempty"`      // OrderBy of original request
	FilterDigest string    `json:"filter_digest,omitempty"` // Canonical filter digest
	TTL          int64     `json:"ttl,omitempty"`           // Token TTL in seconds
	Sig          string    `json:"sig,omitempty"`           // HMAC-SHA256 signature
}

// EncodePageToken creates a page token from offset
// This is a simplified version following AIP-158.
// 已启用签名时（InitPageTokenSigning）token 附带 HMAC，客户端无法伪造偏移。
// marshal 失败返回 error（B8：历史上的 "v1:offset" 兜底产物自签名全进程启用
// 后已不可解码，兜底即语义自不一致；PageTokenData 的 marshal 实际不可失败，
// 错误路径仅供注入式单测与防御）。
func EncodePageToken(offset int) (string, error) {
	data := PageTokenData{
		Version: PageTokenVersion,
		Mode:    TokenModeLegacy,
		Offset:  offset,
		Created: time.Now().UTC(),
	}
	seal(&data)

	return encodeTokenData(data)
}

// SignPageToken computes HMAC-SHA256 signature for token payload.
func SignPageToken(data *PageTokenData, secret string) (string, error) {
	if data == nil {
		return "", fmt.Errorf("page token data is nil")
	}
	if strings.TrimSpace(secret) == "" {
		return "", fmt.Errorf("page token secret is required")
	}
	canonical := *data
	canonical.Sig = ""
	raw, err := pageTokenMarshal(canonical)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// VerifyPageTokenSignature validates page token signature.
func VerifyPageTokenSignature(data *PageTokenData, secret string) error {
	if data == nil {
		return fmt.Errorf("page token data is nil")
	}
	if data.Sig == "" {
		return fmt.Errorf("page token signature missing")
	}
	expected, err := SignPageToken(data, secret)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(expected), []byte(data.Sig)) {
		return fmt.Errorf("invalid page token signature")
	}
	return nil
}

// encodeTokenData encodes token data to base64 string
func encodeTokenData(data PageTokenData) (string, error) {
	jsonBytes, err := pageTokenMarshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal page token data: %w", err)
	}

	// Base64 encode the JSON
	return base64.URLEncoding.EncodeToString(jsonBytes), nil
}

// DecodePageToken decodes a page token and returns the offset
// Following AIP-158 standard.
// 仅接受 base64 JSON 格式（历史 "v1:offset" 简单格式已随签名全进程启用退役，
// 不再解析）；带 Sig 的 token 必须能被当前进程密钥验证（篡改/跨环境重放即
// 拒绝）；启用签名的进程拒绝无签名 token（fail-closed）。
func DecodePageToken(token string) (int, error) {
	if token == "" {
		return 0, nil
	}

	data, err := decodeTokenData(token)
	if err != nil {
		return 0, fmt.Errorf("invalid page token format")
	}
	if err := verifyTokenAuthenticity(&data); err != nil {
		return 0, err
	}
	// Validate token version
	if data.Version != PageTokenVersion {
		return 0, fmt.Errorf("invalid page token version: %s", data.Version)
	}

	// Check token expiration
	ttl := DefaultTokenTTL
	if data.TTL > 0 {
		ttl = time.Duration(data.TTL) * time.Second
	}
	if time.Since(data.Created) > ttl {
		return 0, fmt.Errorf("page token has expired")
	}

	return data.Offset, nil
}

// verifyTokenAuthenticity 校验 token 来源可信：
//   - 未启用签名（key 为空）：跳过校验，保持未启用进程（如部分测试）的
//     历史语义；
//   - 已启用签名：token 必须携带且通过当前进程密钥的 HMAC 验证，
//     无签名 / 篡改 / 跨环境 token 一律拒绝（fail-closed）。
func verifyTokenAuthenticity(data *PageTokenData) error {
	key := pageTokenKey()
	if key == "" {
		return nil
	}
	if data.Sig == "" {
		return fmt.Errorf("page token signature missing: token signing is enforced on this deployment")
	}
	return VerifyPageTokenSignature(data, key)
}

// decodeTokenData decodes token data from base64 string
func decodeTokenData(token string) (PageTokenData, error) {
	var data PageTokenData

	// Decode base64
	jsonBytes, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return data, fmt.Errorf("failed to decode page token: %w", err)
	}

	// Unmarshal JSON
	err = json.Unmarshal(jsonBytes, &data)
	if err != nil {
		return data, fmt.Errorf("failed to parse page token data: %w", err)
	}

	return data, nil
}

// DecodePageTokenFull decodes a page token and returns full metadata
func DecodePageTokenFull(token string) (*PageTokenData, error) {
	if token == "" {
		return nil, fmt.Errorf("empty page token")
	}

	data, err := decodeTokenData(token)
	if err != nil {
		return nil, err
	}

	if err := verifyTokenAuthenticity(&data); err != nil {
		return nil, err
	}

	// Validate token version
	if data.Version != PageTokenVersion {
		return nil, fmt.Errorf("invalid page token version: %s", data.Version)
	}

	// Check token expiration
	ttl := DefaultTokenTTL
	if data.TTL > 0 {
		ttl = time.Duration(data.TTL) * time.Second
	}
	if time.Since(data.Created) > ttl {
		return nil, fmt.Errorf("page token has expired")
	}
	if data.Mode == "" {
		data.Mode = TokenModeLegacy
	}

	return &data, nil
}
