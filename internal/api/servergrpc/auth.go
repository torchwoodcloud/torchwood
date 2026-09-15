package servergrpc

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AuthService 是对外凭证校验面（token introspection）：判定语义全部在
// app 用例（复用进程内校验器），handler 只做 proto ↔ 用例转换。
type AuthService struct {
	serverv1.UnimplementedAuthServiceServer
	auth *appserver.Auth
}

func NewAuthService(auth *appserver.Auth) *AuthService {
	return &AuthService{auth: auth}
}

func (s *AuthService) VerifyToken(ctx context.Context, req *serverv1.VerifyTokenRequest) (*serverv1.VerifyTokenResponse, error) {
	res, err := s.auth.VerifyToken(ctx, appserver.VerifyTokenCommand{
		Token: req.GetToken(),
		Type:  mapVerifyCredentialType(req.GetType()),
	})
	if err != nil {
		return nil, err
	}
	resp := &serverv1.VerifyTokenResponse{
		Valid:     res.Valid,
		UserId:    res.UserID,
		Username:  res.Username,
		ProjectId: res.ProjectID,
		SessionId: res.SessionID,
		Roles:     res.Roles,
		Labels:    res.Labels,
		ActorKind: string(res.ActorKind),
		ApiKeyId:  res.APIKeyID,
	}
	if !res.ExpiresAt.IsZero() {
		resp.ExpiresAt = timestamppb.New(res.ExpiresAt)
	}
	for _, g := range res.Groups {
		resp.Groups = append(resp.Groups, &serverv1.GroupRef{Id: g.ID, Role: g.Role})
	}
	return resp, nil
}

func mapVerifyCredentialType(t serverv1.VerifyCredentialType) appserver.CredentialDispatch {
	switch t {
	case serverv1.VerifyCredentialType_VERIFY_CREDENTIAL_TYPE_TOKEN:
		return appserver.CredentialJWT
	case serverv1.VerifyCredentialType_VERIFY_CREDENTIAL_TYPE_API_KEY:
		return appserver.CredentialAPIKey
	default:
		// UNSPECIFIED 与 AUTO 同义：按结构分派。
		return appserver.CredentialAuto
	}
}
