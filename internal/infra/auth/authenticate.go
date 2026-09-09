package auth

import (
	"context"
	"net/http"

	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Authenticate 是 gRPC / HTTP / Realtime 共用的认证入口：解析传输面凭证并校验。
// Grant 差异（Realtime 禁 API key、HTTP upload 禁 end-user）由调用方在成功后施加。
func (v *Validator) Authenticate(ctx context.Context, req shared.AuthnRequest) (*shared.Principal, error) {
	ct, raw, err := shared.ParseAuthnRequest(req)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return v.ValidateCredential(ctx, raw, ct)
}

// AuthnRequestFromHTTP 从 HTTP 请求组装共用凭证面。
func AuthnRequestFromHTTP(r *http.Request) shared.AuthnRequest {
	if r == nil {
		return shared.AuthnRequest{}
	}
	return shared.AuthnRequest{
		Authorization: r.Header.Values("Authorization"),
		APIKey:        r.Header.Values("X-Api-Key"),
		CookieHeaders: r.Header.Values("Cookie"),
	}
}
