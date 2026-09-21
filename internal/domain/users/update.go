package users

import (
	"fmt"
	"strings"
)

// UserUpdateColumns 是仓储 Update 允许 SET 的列（分列写，禁止整行覆盖）。
var UserUpdateColumns = map[string]struct{}{
	"email":          {},
	"password_hash":  {},
	"name":           {},
	"avatar":         {},
	"status":         {},
	"email_verified": {},
	"pending_email":  {},
	"phone":          {},
	"phone_verified": {},
	"labels":         {},
	"prefs":          {},
	"factors":        {},
	"updated_at":     {},
}

// NormalizeUpdateColumns 规范化 email；未知列或空 map → ErrInvalidUpdate。
func NormalizeUpdateColumns(cols map[string]any) (map[string]any, error) {
	if len(cols) == 0 {
		return nil, fmt.Errorf("%w: no columns to update", ErrInvalidUpdate)
	}
	out := make(map[string]any, len(cols))
	for k, v := range cols {
		col := strings.TrimSpace(k)
		if _, ok := UserUpdateColumns[col]; !ok {
			return nil, fmt.Errorf("%w: unknown column %q", ErrInvalidUpdate, k)
		}
		if col == "email" {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%w: email must be a string", ErrInvalidUpdate)
			}
			s = NormalizeEmail(s)
			if s == "" {
				return nil, fmt.Errorf("%w: email must not be empty", ErrInvalidUpdate)
			}
			out[col] = s
			continue
		}
		out[col] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no columns to update", ErrInvalidUpdate)
	}
	return out, nil
}

// IsEmailUniqueViolation 识别邮箱唯一约束名与 DETAIL Key (email)=。
func IsEmailUniqueViolation(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "sys_users_email_unique") ||
		strings.Contains(lower, "users_email_unique") ||
		strings.Contains(lower, "key (email)=")
}
