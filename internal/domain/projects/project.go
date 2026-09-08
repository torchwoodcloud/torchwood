package projects

import (
	"fmt"
	"time"
)

// 项目注册策略（T-03）。
const (
	RegistrationOpen       = "open"
	RegistrationInviteOnly = "invite_only"
	RegistrationClosed     = "closed"
)

// ValidateRegistrationPolicy 校验注册策略值。
func ValidateRegistrationPolicy(p string) error {
	switch p {
	case RegistrationOpen, RegistrationInviteOnly, RegistrationClosed:
		return nil
	}
	return fmt.Errorf("invalid registration_policy %q (allowed: open, invite_only, closed)", p)
}

type Project struct {
	ID          string
	Name        string
	Description string
	Status      string
	Settings    map[string]any
	// RegistrationPolicy 注册策略（T-03）：默认 open 保持存量行为。
	RegistrationPolicy string
	InternalID         int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type APIKey struct {
	ID         string
	ProjectID  string
	Name       string
	SecretHash string
	Scopes     []string
	ExpireAt   *time.Time
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type Admin struct {
	ID           string
	Email        string
	PasswordHash string
	Role         string
	// RevokedAt 是凭证撤销时间戳（零值=未撤销）：撤销时刻之前签发的全部
	// token 在验证时失效（iat <= RevokedAt 即拒）。改密/删除等管理动作
	// 由 use-case 层在同事务内写入。
	RevokedAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// InviteCode 邀请码（T-03，invite_only 注册策略）。
type InviteCode struct {
	ID        string
	ProjectID string
	Code      string
	MaxUses   int
	UsedCount int
	ExpireAt  *time.Time
	RevokedAt *time.Time
	CreatedBy string
	CreatedAt time.Time
}

// Revoked 报告邀请码是否已吊销。
func (c *InviteCode) Revoked() bool { return c.RevokedAt != nil }
