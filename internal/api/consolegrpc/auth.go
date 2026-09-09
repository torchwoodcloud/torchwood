package consolegrpc

import (
	"context"
	"time"

	consolev1 "github.com/torchwoodcloud/torchwood/genproto/console/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/internal/app/console"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type AuthService struct {
	consolev1.UnimplementedConsoleAuthServiceServer
	auth  *console.Auth
	setup *console.Setup
}

func NewAuthService(auth *console.Auth, setup *console.Setup) *AuthService {
	return &AuthService{auth: auth, setup: setup}
}

func (s *AuthService) SignIn(ctx context.Context, req *consolev1.SignInRequest) (*consolev1.SignInResponse, error) {
	tokens, err := s.auth.SignIn(ctx, console.SignInCommand{
		Email:    req.GetEmail(),
		Password: req.GetPassword(),
	})
	if err != nil {
		return nil, err
	}
	setSessionCookies(ctx, s.auth, tokens)
	return mapSignInResponse(tokens), nil
}

func (s *AuthService) RefreshToken(ctx context.Context, req *consolev1.RefreshTokenRequest) (*consolev1.SignInResponse, error) {
	refreshToken := req.GetRefreshToken()
	if refreshToken == "" {
		// Cookie-only 浏览器流：refresh token 由 HttpOnly cookie 携带。
		refreshToken = refreshTokenFromCookie(ctx)
	}
	tokens, err := s.auth.RefreshToken(ctx, console.RefreshTokenCommand{
		RefreshToken: refreshToken,
	})
	if err != nil {
		return nil, err
	}
	setSessionCookies(ctx, s.auth, tokens)
	return mapSignInResponse(tokens), nil
}

func (s *AuthService) SignOut(ctx context.Context, _ *consolev1.SignOutRequest) (*sharedv1.Empty, error) {
	if err := s.auth.SignOut(ctx); err != nil {
		return nil, err
	}
	clearSessionCookies(ctx, s.auth)
	return &sharedv1.Empty{}, nil
}

func (s *AuthService) GetSetupStatus(ctx context.Context, _ *consolev1.GetSetupStatusRequest) (*consolev1.GetSetupStatusResponse, error) {
	needsSetup, err := s.setup.GetSetupStatus(ctx)
	if err != nil {
		return nil, err
	}
	return &consolev1.GetSetupStatusResponse{
		NeedsSetup:         needsSetup,
		SetupTokenRequired: s.setup.SetupTokenConfigured(),
	}, nil
}

func (s *AuthService) SignUp(ctx context.Context, req *consolev1.SignUpRequest) (*consolev1.SignUpResponse, error) {
	result, err := s.setup.SignUp(ctx, console.SignUpCommand{
		Email:      req.GetEmail(),
		Password:   req.GetPassword(),
		SetupToken: req.GetSetupToken(),
		ProjectID:  req.GetProjectId(),
		DatabaseID: req.GetDatabaseId(),
	})
	if err != nil {
		return nil, err
	}
	// 与 SignIn 一致：注册成功后下发会话 cookie，浏览器端免再次登录。
	setSessionCookies(ctx, s.auth, result.Tokens)
	return &consolev1.SignUpResponse{
		Admin:        mapAdmin(result.Admin),
		AccessToken:  result.Tokens.AccessToken,
		RefreshToken: result.Tokens.RefreshToken,
	}, nil
}

func mapSignInResponse(tokens *console.TokenPair) *consolev1.SignInResponse {
	if tokens == nil {
		return &consolev1.SignInResponse{}
	}
	return &consolev1.SignInResponse{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    timestamppb.New(time.Unix(tokens.ExpiresAt, 0)),
	}
}
