package auth

import (
	"context"

	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
)

// CredentialVerifier 是对外凭证校验（token introspection，OAuth2 RFC 9660
// 模式）的 domain 端口。实现方是进程内凭证校验器（infra/auth.Validator），
// 因此 introspection 判定与请求侧认证完全同源——JWT 验签（purpose 派生
// 密钥）+ 会话表活查 + 用户状态检查 + 实时角色解析；API key 走 sha256
// 查表 + 启用/过期/项目绑定检查。用例层只做判定投影，禁止复刻校验逻辑
// （第二套校验实现必然漂移）。
type CredentialVerifier interface {
	// ValidateCredential 按指定凭证类型校验原文（与认证拦截器同一入口）。
	ValidateCredential(ctx context.Context, raw string, credentialType shared.CredentialType) (*shared.Principal, error)
	// ParseClaims 验签并解析 JWT claims（非破坏性：不消费一次性 token、
	// 不触达会话/用户表），供 introspection 读取 exp/token_type 等元数据并
	// 做非破坏性前置过滤（一次性/refresh/admin JWT 不得进入消费路径）。
	ParseClaims(raw string) (*jwtparser.Claims, bool)
}
