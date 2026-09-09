package functions

import (
	"context"
	"time"
)

// ExecutionTokenInfo 是执行 token 承载的身份信息（P0 执行身份，设计 §1）。
// 服务端只存其投影（Redis 值 JSON），token 原值不落任何存储。
type ExecutionTokenInfo struct {
	ProjectID   string
	FunctionID  string
	ExecutionID string
	// Scopes 是函数 declared_scopes 的规范化投影（"<resource>:<op>" 形态）。
	Scopes []string
	// InvokingUserID 是触发执行的端用户（可选；P2 客户端调用面起填充，
	// Server 面触发为空）。
	InvokingUserID string
}

// ExecutionTokenService 是函数执行短期凭证端口（进程无关：server 同步路径
// 与 worker 异步路径共用；实现为 Redis 不透明 token，主动吊销语义）。
//
// 生命周期契约：执行开始前 Mint（TTL = 函数超时 + 60s 宽限，仅作崩溃兜底）；
// 执行结束（成功/失败/panic）由调用方 Revoke 主动吊销——「执行结束即失效」
// 是主动语义；容器经 env 注入 token 后回访 Server API 时 Validate 换取
// execution principal。
type ExecutionTokenService interface {
	// Mint 铸造新 token（twx_ 前缀不透明串）；TTL <= 0 返回错误。
	Mint(ctx context.Context, info ExecutionTokenInfo, ttl time.Duration) (string, error)
	// Validate 校验 token 并返回身份信息；token 不存在/已吊销/已过期返回
	// (nil, nil)；基础设施故障返回 error——调用方必须区分两者并 fail-closed。
	Validate(ctx context.Context, token string) (*ExecutionTokenInfo, error)
	// Revoke 主动吊销 token（幂等：token 不存在时静默成功）。
	Revoke(ctx context.Context, token string) error
}
