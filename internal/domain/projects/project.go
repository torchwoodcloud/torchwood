package projects

import "time"

type Project struct {
	ID          string
	Name        string
	Description string
	Status      string
	Settings    map[string]any
	InternalID  int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
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
