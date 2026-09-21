package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// User 是项目 schema 内的用户行；表名由 ModelTableExpr 限定。
type User struct {
	bun.BaseModel `bun:"alias:u"`

	ID            string          `bun:"id,pk"`
	Email         string          `bun:"email,notnull"`
	PasswordHash  string          `bun:"password_hash,notnull,default:''"`
	Name          string          `bun:"name,notnull,default:''"`
	Avatar        string          `bun:"avatar,notnull,default:''"`
	Status        string          `bun:"status,notnull,default:'active'"`
	EmailVerified bool            `bun:"email_verified,notnull,default:false"`
	PendingEmail  string          `bun:"pending_email,notnull,default:''"`
	Phone         string          `bun:"phone,notnull,default:''"`
	PhoneVerified bool            `bun:"phone_verified,notnull,default:false"`
	Labels        json.RawMessage `bun:"labels,type:jsonb,notnull"`
	Prefs         json.RawMessage `bun:"prefs,type:jsonb,notnull"`
	Factors       json.RawMessage `bun:"factors,type:jsonb,notnull"`
	CreatedAt     time.Time       `bun:"created_at,notnull"`
	UpdatedAt     time.Time       `bun:"updated_at,notnull"`
}
