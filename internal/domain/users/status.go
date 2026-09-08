package users

import "fmt"

const (
	StatusActive   = "active"
	StatusInactive = "inactive"
	StatusBlocked  = "blocked"
	// StatusDeleted 是 DeleteAccount 的匿名化软删终态（T-03）：仅删除路径
	// 写入，不得经任何外部入参设置；CanAuthenticate 恒 false。
	StatusDeleted = "deleted"
)

var validStatuses = map[string]struct{}{
	StatusActive:   {},
	StatusInactive: {},
	StatusBlocked:  {},
	StatusDeleted:  {},
}

// ValidateStatus reports whether s is an allowed user status value.
func ValidateStatus(s string) error {
	if _, ok := validStatuses[s]; !ok {
		return fmt.Errorf("invalid user status %q (allowed: active, inactive, blocked, deleted)", s)
	}
	return nil
}

// CanAuthenticate reports whether a user with the given status may sign in or use tokens.
// Empty status is treated as active (collection default).
func CanAuthenticate(s string) bool {
	if s == "" {
		return true
	}
	return s == StatusActive
}
