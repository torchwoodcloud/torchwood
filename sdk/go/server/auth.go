package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
)

// AuthService 封装 Server API 的对外凭证校验（token introspection）。
type AuthService struct {
	c   *Client
	api serverv1.AuthServiceClient
}

// VerifyToken 校验 Torchwood 凭证原文（端用户 access JWT 或 API key secret）。
// 任何校验失败（过期/撤销/封禁/不存在）返回 Valid=false 而非错误。
func (a *AuthService) VerifyToken(ctx context.Context, token string, credentialType serverv1.VerifyCredentialType) (*serverv1.VerifyTokenResponse, error) {
	return a.api.VerifyToken(ctx, &serverv1.VerifyTokenRequest{
		Token: token,
		Type:  credentialType,
	})
}
