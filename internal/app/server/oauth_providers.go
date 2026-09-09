package server

import (
	"context"
	"strings"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type OAuthProviders struct {
	repo projects.OAuthProviderRepository
}

func NewOAuthProviders(repo projects.OAuthProviderRepository) *OAuthProviders {
	return &OAuthProviders{repo: repo}
}

type UpsertOAuthProviderCommand struct {
	ProjectID    string
	Provider     string
	Enabled      bool
	ClientID     string
	ClientSecret string
	Scopes       []string
}

func (o *OAuthProviders) List(ctx context.Context, projectID string) ([]projects.OAuthProvider, error) {
	return o.repo.ListOAuthProviders(ctx, projectID)
}

func (o *OAuthProviders) Upsert(ctx context.Context, cmd UpsertOAuthProviderCommand) (*projects.OAuthProvider, error) {
	provider := normalizeServerOAuthProvider(cmd.Provider)
	if provider == "" {
		return nil, status.Error(codes.InvalidArgument, "unsupported oauth provider")
	}
	clientID := strings.TrimSpace(cmd.ClientID)
	if clientID == "" {
		return nil, status.Error(codes.InvalidArgument, "client_id is required")
	}

	clientSecret := strings.TrimSpace(cmd.ClientSecret)
	if clientSecret == "" {
		existing, err := o.repo.GetOAuthProvider(ctx, cmd.ProjectID, provider)
		if err != nil {
			return nil, err
		}
		if existing != nil && existing.ClientSecret != "" {
			clientSecret = existing.ClientSecret
		} else if cmd.Enabled {
			return nil, status.Error(codes.InvalidArgument, "client_secret is required when enabling the provider")
		}
	}

	cfg := &projects.OAuthProvider{
		ProjectID:    cmd.ProjectID,
		Provider:     provider,
		Enabled:      cmd.Enabled,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       append([]string(nil), cmd.Scopes...),
	}
	if err := o.repo.UpsertOAuthProvider(ctx, cfg); err != nil {
		return nil, err
	}
	stored, err := o.repo.GetOAuthProvider(ctx, cmd.ProjectID, provider)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, status.Error(codes.Internal, "oauth provider not found after upsert")
	}
	return stored, nil
}

func (o *OAuthProviders) Delete(ctx context.Context, projectID, provider string) error {
	provider = normalizeServerOAuthProvider(provider)
	if provider == "" {
		return status.Error(codes.InvalidArgument, "unsupported oauth provider")
	}
	existing, err := o.repo.GetOAuthProvider(ctx, projectID, provider)
	if err != nil {
		return err
	}
	if existing == nil {
		return status.Error(codes.NotFound, "oauth provider not found")
	}
	return o.repo.DeleteOAuthProvider(ctx, projectID, provider)
}

func normalizeServerOAuthProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case domainauth.ProviderGoogle:
		return domainauth.ProviderGoogle
	case domainauth.ProviderGitHub:
		return domainauth.ProviderGitHub
	case domainauth.ProviderWeChatWeb:
		return domainauth.ProviderWeChatWeb
	case domainauth.ProviderWeChatMP:
		return domainauth.ProviderWeChatMP
	case domainauth.ProviderWeChatMiniProgram:
		return domainauth.ProviderWeChatMiniProgram
	default:
		return ""
	}
}
