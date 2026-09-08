package auth

import "context"

// Login failure throttling namespaces.
const (
	LoginNamespaceAdmin   = "admin"
	LoginNamespaceEndUser = "end_user"
)

// LoginThrottle rate-limits password sign-in attempts per email and per client IP.
type LoginThrottle interface {
	// Check returns an error when the email or IP has exceeded the failure budget.
	Check(ctx context.Context, namespace, email, ip string) error
	// RecordFailure registers a failed sign-in attempt. recordEmail=false 时
	// 仅累计 IP 维度（T-01：未注册邮箱的失败不得写入任何邮箱键——既不锁死
	// 他人邮箱（R05-P1-5），也让 IP 维度计数与账号存在性无关，429 不构成
	// 存在性 oracle）；真实账号密码错误恒双维计数。
	RecordFailure(ctx context.Context, namespace, email, ip string, recordEmail bool) error
	// Reset clears recorded failures after a successful sign-in.
	Reset(ctx context.Context, namespace, email, ip string) error
}
