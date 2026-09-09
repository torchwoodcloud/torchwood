package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// OAuthProvidersService 封装 Server API 的 OAuth 提供商管理服务。
type OAuthProvidersService struct {
	c   *Client
	api serverv1.OAuthProvidersServiceClient
}

// ListOAuthProviders 列出 OAuth 提供商。
func (s *OAuthProvidersService) ListOAuthProviders(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListOAuthProvidersResponse, error) {
	return s.api.ListOAuthProviders(ctx, req)
}

// UpsertOAuthProvider 创建或更新 OAuth 提供商。
func (s *OAuthProvidersService) UpsertOAuthProvider(ctx context.Context, req *serverv1.UpsertOAuthProviderRequest) (*serverv1.OAuthProvider, error) {
	return s.api.UpsertOAuthProvider(ctx, req)
}

// DeleteOAuthProvider 删除 OAuth 提供商。
func (s *OAuthProvidersService) DeleteOAuthProvider(ctx context.Context, req *serverv1.DeleteOAuthProviderRequest) error {
	_, err := s.api.DeleteOAuthProvider(ctx, req)
	return err
}
