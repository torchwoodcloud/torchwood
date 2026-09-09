package client

import (
	"context"
	"fmt"
	"strings"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (a *Account) findIdentity(ctx context.Context, projectID, provider, providerUID string) (*domainauth.Identity, error) {
	if a.identities == nil {
		return nil, status.Error(codes.Internal, "identity repository is not configured")
	}
	return a.identities.GetByProviderUID(ctx, projectID, provider, providerUID)
}

func (a *Account) createIdentity(ctx context.Context, projectID, userID string, info *domainauth.OAuthUserInfo, provider string) error {
	if a.identities == nil {
		return status.Error(codes.Internal, "identity repository is not configured")
	}
	identityID := idgen.UUID().String()
	providerData := map[string]any{
		"name":       info.Name,
		"avatar_url": info.AvatarURL,
		"raw":        info.Raw,
	}
	if info.OpenID != "" {
		providerData["openid"] = info.OpenID
	}
	if info.UnionID != "" {
		providerData["unionid"] = info.UnionID
	}
	return a.identities.Insert(ctx, projectID, &domainauth.Identity{
		ID:            identityID,
		UserID:        userID,
		Provider:      provider,
		ProviderUID:   info.ProviderUID,
		ProviderEmail: info.Email,
		ProviderData:  providerData,
	})
}

func (a *Account) resolveOAuthUser(ctx context.Context, projectID, provider string, info *domainauth.OAuthUserInfo) (*User, error) {
	if domainauth.IsWeChatProvider(provider) {
		return a.resolveWeChatUser(ctx, projectID, provider, info)
	}
	if info == nil || info.ProviderUID == "" {
		return nil, fmt.Errorf("oauth profile missing provider uid")
	}
	if strings.TrimSpace(info.Email) == "" {
		return nil, fmt.Errorf("oauth provider did not return an email address")
	}
	if !info.EmailVerified {
		return nil, status.Error(codes.FailedPrecondition, "oauth email is not verified")
	}
	info.Email = normalizeEmail(info.Email)
	identity, err := a.findIdentity(ctx, projectID, provider, info.ProviderUID)
	if err != nil {
		return nil, err
	}
	if identity != nil {
		found, err := a.usersRepo.GetByID(ctx, projectID, identity.UserID)
		if err != nil {
			return nil, err
		}
		if found == nil {
			return nil, fmt.Errorf("identity references missing user")
		}
		return accountUser(found), nil
	}

	if info.Email != "" {
		taken, err := a.usersRepo.GetByEmail(ctx, projectID, info.Email)
		if err != nil {
			return nil, err
		}
		if taken != nil {
			return nil, status.Error(codes.FailedPrecondition, "an account with this email already exists; sign in and link the oauth provider")
		}
	}

	name := info.Name
	if name == "" && info.Email != "" {
		name = emailLocalPart(info.Email)
	}
	user, err := a.findOrCreateUserByEmail(ctx, projectID, info.Email, info.Email != "")
	if err != nil {
		return nil, err
	}
	if name != "" && user.Name == emailLocalPart(info.Email) {
		if err := a.usersRepo.Update(ctx, projectID, user.ID, map[string]any{"name": name}); err == nil {
			user.Name = name
		}
	}
	if err := a.createIdentity(ctx, projectID, user.ID, info, provider); err != nil {
		return nil, err
	}
	return user, nil
}

func (a *Account) linkOAuthIdentity(ctx context.Context, projectID, userID, provider string, info *domainauth.OAuthUserInfo) error {
	if info == nil || info.ProviderUID == "" {
		return status.Error(codes.InvalidArgument, "oauth profile missing provider uid")
	}
	info.Email = normalizeEmail(info.Email)
	existing, err := a.findIdentity(ctx, projectID, provider, info.ProviderUID)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.UserID != userID {
			return status.Error(codes.AlreadyExists, "oauth identity already linked to another account")
		}
		return nil
	}
	if strings.TrimSpace(info.Email) != "" {
		taken, err := a.usersRepo.GetByEmail(ctx, projectID, info.Email)
		if err != nil {
			return err
		}
		if taken != nil && taken.ID != userID {
			return status.Error(codes.AlreadyExists, "oauth email belongs to another account")
		}
	}
	return a.createIdentity(ctx, projectID, userID, info, provider)
}
