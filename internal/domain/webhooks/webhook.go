// Package webhooks 定义 Webhooks 一等公民子系统的领域模型与端口
// （docs/design/functions-v4.md §6，D5）：订阅 outbox 事件脊柱，按
// CloudEvents 1.0 structured mode（§4.2 信封，复用 functions 域协议类型）
// 投递到用户配置的 HTTPS endpoint；非 2xx 按重试梯度重投，终态失败行进
// DLQ。本包只承载实体/端口/纯领域逻辑（重试梯度、轮换窗口、订阅匹配），
// HTTP 投递与持久化在 infra。
package webhooks

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// 状态词表（webhook_subscriptions.status CHECK 约束同源）。
const (
	// StatusEnabled 是投递中（缺省）。
	StatusEnabled = "enabled"
	// StatusPaused 是暂停（快照扫描跳过；不投递不删行）。
	StatusPaused = "paused"
)

// 订阅串上限（与函数事件触发器同源：functions.MaxEventPatterns ×
// MaxEventPatternBytes；此处独立常量避免 webhooks 语义漂移时误联动）。
const (
	MaxEvents     = 64
	MaxEventBytes = 200
)

// SecretPrefix 是签名密钥明文前缀（whsec_，业界惯例；接收方一眼可辨用途）。
const SecretPrefix = "whsec_"

// FingerprintPrefix 是指纹前缀（区别于明文前缀，管理面唯一可见形态）。
const FingerprintPrefix = "whsec_"

// MaxURLBytes 是订阅 URL 的字节上限（protovalidate max_len 同源）。
const MaxURLBytes = 2048

// Webhook 是一个订阅实体（webhook_subscriptions 行的领域投影）。
type Webhook struct {
	ID        string
	ProjectID string
	// URL 是投递终点（https 强制；出站 SSRF 守卫在拨号点把关）。
	URL string
	// Events 是订阅串集（eventmatch 语法，原样存储；匹配按解析后索引进行）。
	Events []string
	// Status 是 enabled | paused。
	Status string
	// SecretEnc 是当前签名密钥的 secretbox 密文（enc:v1: 前缀；明文仅在
	// 创建/轮换响应出现一次，此后不落任何日志/响应面）。
	SecretEnc string
	// PreviousSecretEnc 是轮换窗口内的旧钥密文（空 = 无轮换或窗口已过）。
	PreviousSecretEnc string
	// SecretRotatedAt 是最近一次轮换时刻（读取侧据此判定旧钥宽限窗）。
	SecretRotatedAt time.Time
	// SecretFingerprint 是密钥指纹（管理面唯一可见形态）。
	SecretFingerprint string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Enabled 报告订阅是否投递中。
func (w *Webhook) Enabled() bool { return w.Status == StatusEnabled }

// Fingerprint 由明文密钥派生指纹（whsec_ + SHA-256 前 8 hex）：管理面比对
// 用，不可逆推明文。
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return FingerprintPrefix + hex.EncodeToString(sum[:])[:8]
}

// SecretRotationWindow 是轮换宽限窗缺省（v2 签名窗口 24h；config
// functions.webhooks.secret_grace_period 可覆盖）。
const SecretRotationWindow = 24 * time.Hour

// EffectivePreviousSecret 报告旧钥是否仍在宽限窗内（rotatedAt 为零值 =
// 从未轮换，不在窗内；window <=0 用缺省窗）。
func EffectivePreviousSecret(now time.Time, rotatedAt time.Time, window time.Duration) bool {
	if rotatedAt.IsZero() {
		return false
	}
	if window <= 0 {
		window = SecretRotationWindow
	}
	return now.Sub(rotatedAt) < window
}

// ValidateURL 校验投递终点（创建/更新期应用层校验；protovalidate 同规则
// 双保险）：https 强制 + 长度上限。
func ValidateURL(u string) error {
	if u == "" {
		return fmt.Errorf("url is required")
	}
	if len(u) > MaxURLBytes {
		return fmt.Errorf("url exceeds maximum of %d bytes", MaxURLBytes)
	}
	const scheme = "https://"
	if len(u) < len(scheme) || u[:len(scheme)] != scheme {
		return fmt.Errorf("url must use https scheme")
	}
	return nil
}
