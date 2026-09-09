package users

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/pkg/password"
)

const (
	CollectionID   = "users"
	LabelAnonymous = "anonymous"
)

var (
	ErrEmailAlreadyRegistered = errors.New("email already registered")
	ErrEmailRequired          = errors.New("email is required")
	ErrUserIDRequired         = errors.New("user id is required")
)

// User 是项目内终端用户聚合：邮箱唯一、状态、密码哈希与匿名标签。
type User struct {
	ID            string
	Email         string
	PasswordHash  string
	Name          string
	Status        string
	EmailVerified bool
	Phone         string
	PhoneVerified bool
	Labels        []string
	Prefs         map[string]any
	Factors       json.RawMessage
	PendingEmail  string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (u *User) CanAuthenticate() bool {
	if u == nil {
		return false
	}
	return CanAuthenticate(u.Status)
}

func (u *User) IsAnonymous() bool {
	if u == nil {
		return false
	}
	for _, l := range u.Labels {
		if l == LabelAnonymous {
			return true
		}
	}
	return false
}

func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// RequireUniqueEmail 在 GetByEmail 之后断言邮箱未被占用。
func RequireUniqueEmail(existing *User) error {
	if existing != nil {
		return ErrEmailAlreadyRegistered
	}
	return nil
}

func AnonymousEmail(userID string) string {
	shortID := userID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return fmt.Sprintf("anon_%s@torchwood.local", shortID)
}

// RegisterInput 是 User.Register 的入参；Password 为空表示无密码（OTP/OAuth/匿名）。
type RegisterInput struct {
	ID            string
	Email         string
	Password      string
	Name          string
	Status        string
	EmailVerified bool
	Phone         string
	PhoneVerified bool
	Anonymous     bool
	Labels        []string
	Prefs         map[string]any
}

// Register 构造新用户：规范化邮箱、校验状态/密码强度并哈希密码。
// 唯一性由调用方 GetByEmail + RequireUniqueEmail（及仓储 Insert 冲突）保证。
func Register(in RegisterInput) (*User, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, ErrUserIDRequired
	}
	email := NormalizeEmail(in.Email)
	if email == "" {
		return nil, ErrEmailRequired
	}
	status := in.Status
	if status == "" {
		status = StatusActive
	}
	if err := ValidateStatus(status); err != nil {
		return nil, err
	}
	var hash string
	if in.Password != "" {
		if err := ValidatePasswordStrength(in.Password); err != nil {
			return nil, err
		}
		var err error
		hash, err = password.Hash(in.Password)
		if err != nil {
			return nil, err
		}
	}
	labels := append([]string(nil), in.Labels...)
	name := in.Name
	if in.Anonymous {
		if !containsLabel(labels, LabelAnonymous) {
			labels = append(labels, LabelAnonymous)
		}
		if name == "" {
			name = "Anonymous"
		}
	}
	prefs := in.Prefs
	if prefs == nil {
		prefs = map[string]any{}
	}
	return &User{
		ID:            id,
		Email:         email,
		PasswordHash:  hash,
		Name:          name,
		Status:        status,
		EmailVerified: in.EmailVerified,
		Phone:         strings.TrimSpace(in.Phone),
		PhoneVerified: in.PhoneVerified,
		Labels:        labels,
		Prefs:         prefs,
	}, nil
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// DocumentData 是 Users API 投影到 databases.Document.Data 的字段快照。
// password_hash 永不进入投影（响应面零哈希泄露；密码校验走 usersRepo 的
// User.PasswordHash 结构体字段，与本投影无关）。
func (u *User) DocumentData() map[string]any {
	if u == nil {
		return map[string]any{}
	}
	labels := make([]any, len(u.Labels))
	for i, l := range u.Labels {
		labels[i] = l
	}
	prefs := u.Prefs
	if prefs == nil {
		prefs = map[string]any{}
	}
	data := map[string]any{
		"email":          u.Email,
		"name":           u.Name,
		"status":         u.Status,
		"email_verified": u.EmailVerified,
		"labels":         labels,
		"prefs":          prefs,
	}
	if u.Phone != "" {
		data["phone"] = u.Phone
		data["phone_verified"] = u.PhoneVerified
	}
	if u.PendingEmail != "" {
		data["pending_email"] = u.PendingEmail
	}
	if len(u.Factors) > 0 {
		var factors any
		if err := json.Unmarshal(u.Factors, &factors); err == nil {
			data["factors"] = factors
		}
	}
	return data
}

// LabelsFromAny 把 CreateUser 等入口的 []any labels 收成 []string。
// 与 gRPC structToStringSlice 一致：字符串原样保留，数字标量 stringify，不静默丢弃。
func LabelsFromAny(raw []any) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := labelToString(item); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func labelToString(item any) (string, bool) {
	switch v := item.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), true
	case int:
		return strconv.Itoa(v), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case json.Number:
		return v.String(), true
	default:
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" || s == "<nil>" {
			return "", false
		}
		return s, true
	}
}
