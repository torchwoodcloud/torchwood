package auth

import (
	"context"
	"errors"
	"time"
)

// ErrNoChallenge 表示挑战不存在/已消费（NoopMFAChallengeStore.Consume 恒返回）。
var ErrNoChallenge = errors.New("mfa challenge not found")

// 以下 Noop 实现仅供测试装配使用：生产组合根恒注入 Redis 版本
// （infra/auth）。用例层不再对 nil 依赖做静默旁路（Round4 J5-5：
// 「nil 即关闭频控」属埋雷写法，缺失依赖应在构造期显式化）。

// NoopLoginThrottle 不做任何登录频控。仅供测试。
type NoopLoginThrottle struct{}

func (NoopLoginThrottle) Check(context.Context, string, string, string) error {
	return nil
}

func (NoopLoginThrottle) RecordFailure(context.Context, string, string, string, bool) error {
	return nil
}

func (NoopLoginThrottle) Reset(context.Context, string, string, string) error {
	return nil
}

// NoopMFAChallengeStore 是不落存储的 MFA 挑战桩：Create 返回固定 token，
// Consume 恒失败（无真实会话语义）。仅供测试（且仅覆盖「无需真实挑战」的路径）。
type NoopMFAChallengeStore struct{}

const noopChallengeToken = "noop-test-challenge"

func (NoopMFAChallengeStore) Create(context.Context, string, string) (string, time.Time, error) {
	return noopChallengeToken, time.Now().Add(5 * time.Minute), nil
}

func (NoopMFAChallengeStore) Consume(context.Context, string) (string, string, error) {
	return "", "", ErrNoChallenge
}

func (NoopMFAChallengeStore) RevokeByUser(context.Context, string, string) error {
	return nil
}
