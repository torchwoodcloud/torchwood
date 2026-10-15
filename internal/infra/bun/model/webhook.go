package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// WebhookSubscription 是 Webhooks 订阅实体（迁移 000027，functions-v4.md
// §6）：密钥以 secretbox 密文落库（明文仅在创建/轮换响应一次性返回）。
type WebhookSubscription struct {
	bun.BaseModel `bun:"table:webhook_subscriptions,alias:ws"`

	ID        string   `bun:"id,pk"`
	ProjectID string   `bun:"project_id,notnull"`
	URL       string   `bun:"url,notnull"`
	Events    []string `bun:"events,array,notnull"`
	// Status ∈ {enabled, paused}（CHECK 约束同源）。
	Status string `bun:"status,notnull,default:'enabled'"`
	// SecretEnc 是当前签名密钥密文（enc:v1: 前缀）。
	SecretEnc string `bun:"secret_enc,notnull"`
	// PreviousSecretEnc 是轮换窗口内的旧钥密文（NULL = 无轮换/窗外）。
	PreviousSecretEnc string    `bun:"previous_secret_enc,nullzero"`
	SecretRotatedAt   time.Time `bun:"secret_rotated_at,nullzero"`
	SecretFingerprint string    `bun:"secret_fingerprint,notnull"`
	CreatedAt         time.Time `bun:"created_at,notnull"`
	UpdatedAt         time.Time `bun:"updated_at,notnull"`
}

// WebhookDelivery 是投递行（重投事实源 + DLQ）：payload 为 CloudEvents 1.0
// structured mode 信封整体，重投期间不变。
type WebhookDelivery struct {
	bun.BaseModel `bun:"table:webhook_deliveries,alias:wd"`

	ID        string          `bun:"id,pk"`
	ProjectID string          `bun:"project_id,notnull"`
	WebhookID string          `bun:"webhook_id,notnull"`
	EventID   string          `bun:"event_id,notnull"`
	EventType string          `bun:"event_type,notnull"`
	Seq       int64           `bun:"seq,notnull,default:0"`
	Payload   json.RawMessage `bun:"payload,type:jsonb,notnull"`
	// Status ∈ {pending, delivered, failed}（CHECK 约束同源；failed = DLQ）。
	Status string `bun:"status,notnull,default:'pending'"`
	// Attempts 是已执行尝试次数（含首投）。
	Attempts      int       `bun:"attempts,notnull,default:0"`
	NextAttemptAt time.Time `bun:"next_attempt_at,notnull"`
	// LastResponseCode 是最近尝试的响应码（NULL = 未达 HTTP 层）。
	LastResponseCode int       `bun:"last_response_code,nullzero"`
	LastError        string    `bun:"last_error,notnull,default:''"`
	CreatedAt        time.Time `bun:"created_at,notnull"`
	UpdatedAt        time.Time `bun:"updated_at,notnull"`
}
