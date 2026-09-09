package serverhttp

import (
	"context"
	"net/http"
	"strings"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AuthValidator 是 HTTP 鉴权所需的最小凭证校验面（infra/auth.Validator 满足；
// 接口化便于单测且消除 api→infra 直依赖，仿 realtime/handler.go CredentialValidator 模式）。
type AuthValidator interface {
	Authenticate(ctx context.Context, req shared.AuthnRequest) (*shared.Principal, error)
	ValidateAdminProjectAccess(ctx context.Context, principal *shared.Principal) error
}

// httpAuth 是 serverhttp 各 handler 共享的 HTTP 认证/鉴权原语，与 gRPC /
// Realtime 共用 Validator.Authenticate。scope 匹配读 PolicySet（与拦截器
// 同源，禁止手写策略值）。
type httpAuth struct {
	validator AuthValidator
	policies  *domainauth.PolicySet
}

func newHTTPAuth(validator AuthValidator, policies *domainauth.PolicySet) *httpAuth {
	return &httpAuth{validator: validator, policies: policies}
}

// authenticate 解析并校验请求凭证，返回认证主体；多凭证并存、同一凭证头
// 多值或凭证缺失返回 401。
func (a *httpAuth) authenticate(r *http.Request) (*shared.Principal, error) {
	return a.validator.Authenticate(r.Context(), authnRequestFromHTTP(r))
}

// authorize 对已认证主体做方法级授权：
//   - API key 与函数执行 principal（P0 执行身份）必须持有 apiKeyScope(r)
//     返回的 scope（按方法区分读写权限）；targets 携带请求寻址的目标实例
//     （T-02 资源级 scope 强制；不可寻址的方法传零值——实例限定 scope 对其
//     恒不匹配）。execution 的 Permissions 已是 API key 同款权限串，走同一
//     AllowsAPIKeyTargets 求值——防执行 token 绕过 HTTP 面的 scope 门；
//   - admin 主体可经 X-Torchwood-Project 指定项目，并校验项目访问权。
func (a *httpAuth) authorize(r *http.Request, apiKeyScope func(*http.Request) string, targets domainauth.ScopeTargets) (*shared.Principal, error) {
	ctx := r.Context()
	principal, err := a.authenticate(r)
	if err != nil {
		return nil, err
	}
	if principal.CredentialType == shared.CredentialTypeAPIKey || principal.CredentialType == shared.CredentialTypeExecution {
		if !a.policies.AllowsAPIKeyTargets(apiKeyScope(r), principal.Permissions, targets) {
			return nil, status.Error(codes.PermissionDenied, "api key missing required scope")
		}
	}
	if principal.ActorKind == shared.ActorKindAdmin {
		// P3-4：X-Torchwood-Project 多值拒绝（fail-closed）。
		if values := r.Header.Values("X-Torchwood-Project"); len(values) > 1 {
			return nil, status.Error(codes.InvalidArgument, "multiple X-Torchwood-Project headers not allowed")
		} else if len(values) == 1 {
			if projectID := strings.TrimSpace(values[0]); projectID != "" {
				principal.ProjectID = projectID
			}
		}
		if err := a.validator.ValidateAdminProjectAccess(ctx, principal); err != nil {
			return nil, err
		}
	}
	return principal, nil
}

// projectID 解析请求上下文中的项目 ID（admin 会话可经 X-Torchwood-Project 覆盖）。
// P3-4：多值时调用方已在 authorize 阶段被拒绝，此处仅取首值保持一致。
func (a *httpAuth) projectID(r *http.Request, p *shared.Principal) string {
	if p == nil {
		return ""
	}
	switch p.CredentialType {
	case shared.CredentialTypeAPIKey:
		return p.ProjectID
	case shared.CredentialTypeToken, shared.CredentialTypeSession:
		if p.ActorKind == shared.ActorKindAdmin {
			if values := r.Header.Values("X-Torchwood-Project"); len(values) == 1 {
				if pid := strings.TrimSpace(values[0]); pid != "" {
					return pid
				}
			}
		}
		return p.ProjectID
	default:
		return p.ProjectID
	}
}

// authnRequestFromHTTP 从 HTTP 请求组装共用凭证面（避免 import infra/auth，
// 逻辑与 infra/auth.AuthnRequestFromHTTP 一致）。
func authnRequestFromHTTP(r *http.Request) shared.AuthnRequest {
	if r == nil {
		return shared.AuthnRequest{}
	}
	return shared.AuthnRequest{
		Authorization: r.Header.Values("Authorization"),
		APIKey:        r.Header.Values("X-Api-Key"),
		CookieHeaders: r.Header.Values("Cookie"),
	}
}
