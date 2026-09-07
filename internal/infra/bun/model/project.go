package model

import (
	"time"

	"github.com/uptrace/bun"
)

type Project struct {
	bun.BaseModel `bun:"table:projects,alias:p"`

	ID          string         `bun:"id,pk"`
	InternalID  int64          `bun:"internal_id,autoincrement"`
	Name        string         `bun:"name,notnull,unique"`
	Description string         `bun:"description"`
	Status      string         `bun:"status,notnull,default:'active'"`
	Settings    map[string]any `bun:"settings,type:jsonb"`
	CreatedAt   time.Time      `bun:"created_at,notnull"`
	UpdatedAt   time.Time      `bun:"updated_at,notnull"`
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

	ID           string     `bun:"id,pk"`
	Email        string     `bun:"email,notnull,unique"`
	PasswordHash string     `bun:"password_hash,notnull"`
	Role         string     `bun:"role,notnull,default:'owner'"`
	// RevokedAt 凭证撤销时间戳（nullzero：零值写 NULL=未撤销），随迁移 000006。
	RevokedAt time.Time `bun:"revoked_at,nullzero"`
	CreatedAt time.Time `bun:"created_at,notnull"`
	UpdatedAt time.Time `bun:"updated_at,notnull"`
}
