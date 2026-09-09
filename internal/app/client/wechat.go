package client

import (
	"context"
	"fmt"
	"strings"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type CreateWeChatMiniProgramSessionCommand struct {
	ProjectID string
	Code      string
}

func (a *Account) CreateWeChatMiniProgramSession(ctx context.Context, cmd CreateWeChatMiniProgramSessionCommand) (*User, *TokenBundle, string, *MFASignInChallenge, error) {
	projectID := strings.TrimSpace(cmd.ProjectID)
	code := strings.TrimSpace(cmd.Code)
	provider := domainauth.ProviderWeChatMiniProgram
	if projectID == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	if code == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "code is required")
	}

	project, err := a.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if project == nil {
		return nil, nil, "", nil, status.Error(codes.NotFound, "project not found")
	}

	oauthCfg, err := a.loadOAuthProvider(ctx, projectID, provider)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if a.weChatExchanger == nil {
		return nil, nil, "", nil, status.Error(codes.Unimplemented, "wechat exchanger is not configured")
	}
	profile, err := a.weChatExchanger.ExchangeWeChatMiniProgramCode(ctx, oauthCfg.ClientID, oauthCfg.ClientSecret, code)
	if err != nil {
		return nil, nil, "", nil, status.Errorf(codes.Unauthenticated, "wechat code exchange failed: %v", err)
	}

	user, err := a.resolveWeChatUser(ctx, projectID, provider, profile)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if !users.CanAuthenticate(user.Status) {
		return nil, nil, "", nil, status.Error(codes.Unauthenticated, "user account is not active")
	}
	return a.finishSignInWithProvider(ctx, projectID, user, provider)
}

func (a *Account) resolveWeChatUser(ctx context.Context, projectID, provider string, info *domainauth.OAuthUserInfo) (*User, error) {
	if info == nil || info.ProviderUID == "" {
		return nil, fmt.Errorf("wechat profile missing identity uid")
	}

	identity, err := a.findWeChatIdentity(ctx, projectID, info.ProviderUID)
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
		user := accountUser(found)
		if identity.Provider != provider {
			if err := a.createIdentity(ctx, projectID, user.ID, info, provider); err != nil {
				return nil, err
			}
		}
		return user, nil
	}

	email := strings.TrimSpace(info.Email)
	if email == "" {
		email = wechatPlaceholderEmail(provider, info.ProviderUID)
	}
	name := strings.TrimSpace(info.Name)
	if name == "" {
		name = "WeChat User"
	}

	user, err := a.findOrCreateUserByEmail(ctx, projectID, email, false)
	if err != nil {
		return nil, err
	}
	if info.Name != "" && user.Name != info.Name {
		if err := a.usersRepo.Update(ctx, projectID, user.ID, map[string]any{"name": info.Name}); err == nil {
			user.Name = info.Name
		}
	} else if user.Name == "" || user.Name == emailLocalPart(email) {
		if err := a.usersRepo.Update(ctx, projectID, user.ID, map[string]any{"name": name}); err == nil {
			user.Name = name
		}
	}
	if err := a.createIdentity(ctx, projectID, user.ID, info, provider); err != nil {
		return nil, err
	}
	return user, nil
}

func (a *Account) findWeChatIdentity(ctx context.Context, projectID, uid string) (*domainauth.Identity, error) {
	for _, provider := range domainauth.WeChatProviders() {
		identity, err := a.findIdentity(ctx, projectID, provider, uid)
		if err != nil {
			return nil, err
		}
		if identity != nil {
			return identity, nil
		}
	}
	return nil, nil
}

func wechatPlaceholderEmail(provider, uid string) string {
	safe := strings.NewReplacer("+", "", "@", "", ":", "_").Replace(uid)
	return fmt.Sprintf("wechat_%s_%s@torchwood.local", provider, safe)
}

func usesWeChatPKCE(provider string) bool {
	return !domainauth.IsWeChatProvider(provider)
}
