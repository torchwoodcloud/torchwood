package model

import (
	"time"

	"github.com/uptrace/bun"
)

type Project struct {
	bun.BaseModel `bun:"table:projects,alias:p"`

	ID string `bun:"id,pk"`
	// InternalID 是 identity 列（迁移 000001），数据面 _tenant 租户号与
	// roles_sig Tenant 的唯一来源。skipupdate 是 UPDATE 侧防线：bun 对
	// autoincrement 隐式置 NullZero，pgdialect 的 DefaultPlaceholder 会把
	// 零值渲染成 SET internal_id = DEFAULT（= nextval，烧号并改写租户号，
	// 2026-09-08 dev 事故）。对 UPDATE 只读；INSERT 侧排除 + RETURNING 行为
	// 不变。仓库更新必须列白名单（bunrepo update_guard_test 兜底）。
	InternalID  int64          `bun:"internal_id,autoincrement,skipupdate"`
	Name        string         `bun:"name,notnull,unique"`
	Description string         `bun:"description"`
	Status      string         `bun:"status,notnull,default:'active'"`
	Settings    map[string]any `bun:"settings,type:jsonb"`
	// RegistrationPolicy 注册策略（T-03）：open/invite_only/closed，默认 open。
	RegistrationPolicy string    `bun:"registration_policy,notnull,default:'open'"`
	CreatedAt          time.Time `bun:"created_at,notnull"`
	UpdatedAt          time.Time `bun:"updated_at,notnull"`
}

// InviteCode 邀请码（T-03，invite_only 注册策略；迁移 000007）。明文 code
// 入库：邀请码是分发给被邀请者的凭证（Console 需回显复制重发），泄露风险
// 由一次性/次数上限/过期/可吊销控制，与 API key 的哈希存储模型不同。
type InviteCode struct {
	bun.BaseModel `bun:"table:invite_codes,alias:ic"`

	ID        string     `bun:"id,pk"`
	ProjectID string     `bun:"project_id,notnull"`
	Code      string     `bun:"code,notnull"`
	MaxUses   int        `bun:"max_uses,notnull,default:1"`
	UsedCount int        `bun:"used_count,notnull,default:0"`
	ExpireAt  *time.Time `bun:"expire_at"`
	RevokedAt *time.Time `bun:"revoked_at"`
	CreatedBy string     `bun:"created_by,notnull,default:''"`
	CreatedAt time.Time  `bun:"created_at,notnull"`
}

type APIKey struct {
	bun.BaseModel `bun:"table:api_keys,alias:ak"`

	ID         string     `bun:"id,pk"`
	ProjectID  string     `bun:"project_id,notnull"`
	Name       string     `bun:"name,notnull"`
	SecretHash string     `bun:"secret_hash,notnull"`
	Scopes     []string   `bun:"scopes,array"`
	ExpireAt   *time.Time `bun:"expire_at"`
	Enabled    bool       `bun:"enabled,notnull,default:true"`
	CreatedAt  time.Time  `bun:"created_at,notnull"`
	UpdatedAt  time.Time  `bun:"updated_at,notnull"`
}

type Admin struct {
	bun.BaseModel `bun:"table:admins,alias:ca"`

	ID           string `bun:"id,pk"`
	Email        string `bun:"email,notnull,unique"`
	PasswordHash string `bun:"password_hash,notnull"`
	Role         string `bun:"role,notnull,default:'owner'"`
	// RevokedAt 凭证撤销时间戳（nullzero：零值写 NULL=未撤销），随迁移 000006。
	RevokedAt time.Time `bun:"revoked_at,nullzero"`
	CreatedAt time.Time `bun:"created_at,notnull"`
	UpdatedAt time.Time `bun:"updated_at,notnull"`
}
