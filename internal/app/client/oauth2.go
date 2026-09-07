package client

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/url"
	"strings"
	"time"

	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type CreateOAuth2LinkSessionCommand struct {
	ProjectID string
	Provider  string
	Success   string
	Failure   string
}

type CreateOAuth2LinkTokenSessionCommand struct {
	ProjectID string
	Provider  string
	Code      string
	State     string
}

type CreateOAuth2SessionCommand struct {
	ProjectID string
	Provider  string
	Success   string
	Failure   string
}

type CreateOAuth2TokenSessionCommand struct {
	ProjectID string
	Provider  string
	Success   string
	Failure   string
	Code      string
	State     string
}

type OAuth2CallbackResult struct {
	ProjectID     string
	SuccessURL    string
	FailureURL    string
	User          *User
	Tokens        *TokenBundle
	SessionCookie string
	RedirectURL   string
	MFA           *MFASignInChallenge
}

func (a *Account) CreateOAuth2Session(ctx context.Context, cmd CreateOAuth2SessionCommand) (string, string, error) {
	if a.oauthState == nil {
		return "", "", status.Error(codes.Unimplemented, "oauth2 is not configured")
	}
	projectID := strings.TrimSpace(cmd.ProjectID)
	provider := normalizeOAuthProvider(cmd.Provider)
	if projectID == "" {
		return "", "", status.Error(codes.InvalidArgument, "project_id is required")
	}
	if provider == "" {
		return "", "", status.Error(codes.InvalidArgument, "provider is required")
	}
	return a.createOAuth2Session(ctx, createOAuth2SessionParams{
		projectID: projectID,
		provider:  provider,
		success:   cmd.Success,
		failure:   cmd.Failure,
	})
}

func (a *Account) CreateOAuth2LinkSession(ctx context.Context, cmd CreateOAuth2LinkSessionCommand) (string, string, error) {
	p, err := a.requireUser(ctx)
	if err != nil {
		return "", "", err
	}
	projectID := strings.TrimSpace(cmd.ProjectID)
	if projectID == "" {
		projectID = p.ProjectID
	}
	if projectID != p.ProjectID {
		return "", "", status.Error(codes.PermissionDenied, "cannot link oauth provider for another project")
	}
	return a.createOAuth2Session(ctx, createOAuth2SessionParams{
		projectID:  projectID,
		provider:   cmd.Provider,
		success:    cmd.Success,
		failure:    cmd.Failure,
		linkUserID: p.UserID,
	})
}

// CreateOAuth2LinkTokenSession 以 code+state 完成 OAuth 身份 link（token 面）。
// M5 C5（评审补偿控制）：state 归属必须与调用者一致——requireUser 之外，
// 消费的 state.LinkUserID 必须等于 caller.UserID，否则 PermissionDenied
//（堵"他人 state 冒名消费"与"login state 被当 link state 消费"）。
func (a *Account) CreateOAuth2LinkTokenSession(ctx context.Context, cmd CreateOAuth2LinkTokenSessionCommand) (*User, error) {
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	result, err := a.completeOAuth2Code(ctx, completeOAuth2CodeCommand{
		ProjectID:    cmd.ProjectID,
		Provider:     cmd.Provider,
		Code:         cmd.Code,
		State:        cmd.State,
		CallerUserID: p.UserID,
	})
	if err != nil {
		return nil, err
	}
	if result.User == nil {
		return nil, status.Error(codes.Internal, "oauth link did not return user")
	}
	return result.User, nil
}

type createOAuth2SessionParams struct {
	projectID  string
	provider   string
	success    string
	failure    string
	linkUserID string
}

// createOAuth2Session 生成 state 并返回 (authorize URL, nonce, error)。
// nonce 由传输面种入 TORCHWOOD_oauth_nonce_<project> cookie（M5 C5 login
// CSRF 绑定）；state 记录同值落库供回调配对。
func (a *Account) createOAuth2Session(ctx context.Context, params createOAuth2SessionParams) (string, string, error) {
	if a.oauthState == nil {
		return "", "", status.Error(codes.Unimplemented, "oauth2 is not configured")
	}
	provider := normalizeOAuthProvider(params.provider)
	if provider == "" {
		return "", "", status.Error(codes.InvalidArgument, "provider is required")
	}
	if provider == domainauth.ProviderWeChatMiniProgram {
		return "", "", status.Error(codes.InvalidArgument, "use CreateWeChatMiniProgramSession for wechat_miniprogram")
	}
	if err := validateRedirectURL(params.success); err != nil {
		return "", "", status.Errorf(codes.InvalidArgument, "invalid success url: %v", err)
	}
	if err := validateRedirectURL(params.failure); err != nil {
		return "", "", status.Errorf(codes.InvalidArgument, "invalid failure url: %v", err)
	}
	if err := a.validateProjectOAuthRedirectURLs(ctx, params.projectID, params.success, params.failure); err != nil {
		return "", "", err
	}
	if err := a.requireProject(ctx, params.projectID); err != nil {
		return "", "", err
	}
	oauthCfg, err := a.loadOAuthProvider(ctx, params.projectID, provider)
	if err != nil {
		return "", "", err
	}
	stateID := idgen.UUID().String()
	nonce := idgen.UUID().String()
	verifier := ""
	challenge := ""
	if usesWeChatPKCE(provider) {
		verifier = oauth2.GenerateVerifier()
		challenge = oauth2.S256ChallengeFromVerifier(verifier)
	}
	if err := a.oauthState.Save(ctx, domainauth.OAuthState{
		StateID:      stateID,
		ProjectID:    params.projectID,
		Provider:     provider,
		SuccessURL:   params.success,
		FailureURL:   params.failure,
		PKCEVerifier: verifier,
		LinkUserID:   params.linkUserID,
		Nonce:        nonce,
	}, 0); err != nil {
		return "", "", err
	}
	if a.oauthFactory == nil {
		return "", "", status.Error(codes.Unimplemented, "oauth factory is not configured")
	}
	authClient, err := a.oauthFactory.NewAuthenticator(provider, oauthCfg.ClientID, oauthCfg.ClientSecret, a.oauthCallbackURL(provider), oauthCfg.Scopes)
	if err != nil {
		return "", "", status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return authClient.AuthorizeURL(stateID, challenge), nonce, nil
}

func (a *Account) CreateOAuth2TokenSession(ctx context.Context, cmd CreateOAuth2TokenSessionCommand) (*User, *TokenBundle, string, *MFASignInChallenge, error) {
	result, err := a.completeOAuth2Code(ctx, completeOAuth2CodeCommand{
		ProjectID: cmd.ProjectID,
		Provider:  cmd.Provider,
		Code:      cmd.Code,
		State:     cmd.State,
	})
	if err != nil {
		return nil, nil, "", nil, err
	}
	return result.User, result.Tokens, result.SessionCookie, result.MFA, nil
}

// HandleOAuth2Callback 处理浏览器 GET 回调（公开端点）。M5 C5：link 流要求
// 携带 state 对应项目的端用户会话 cookie 且会话本人 == LinkUserID；login 流
// 要求 TORCHWOOD_oauth_nonce_<project> cookie 与 state 配对（防 CSRF/注入）。
// sessionCookies/oauthNonces 为 project → cookie value（transport 从请求
// cookie 中按前缀抽取）；两者恒非 nil（空 map 也强制校验，fail-closed）。
func (a *Account) HandleOAuth2Callback(ctx context.Context, provider, code, state string, sessionCookies, oauthNonces map[string]string) (*OAuth2CallbackResult, error) {
	result, err := a.completeOAuth2Code(ctx, completeOAuth2CodeCommand{
		Provider:       provider,
		Code:           code,
		State:          state,
		SessionCookies: sessionCookies,
		OAuthNonces:    oauthNonces,
	})
	if err != nil {
		// completeOAuth2Code 在多数失败分支返回 nil result，这里必须兜底。
		failureURL := "/"
		var successURL string
		if result != nil {
			if result.FailureURL != "" {
				failureURL = result.FailureURL
			}
			successURL = result.SuccessURL
		}
		return &OAuth2CallbackResult{
			SuccessURL:  successURL,
			FailureURL:  failureURL,
			RedirectURL: appendQuery(failureURL, "error", "oauth_failed"),
		}, err
	}
	redirect := appendOAuthSPAFragment(result.SuccessURL, result.User.ID, result.Tokens)
	if result.MFA != nil {
		redirect = appendOAuthMFAFragment(result.SuccessURL, result.User.ID, result.MFA.Token, result.MFA.Factors)
	}
	return &OAuth2CallbackResult{
		ProjectID:     result.ProjectID,
		SuccessURL:    result.SuccessURL,
		FailureURL:    result.FailureURL,
		User:          result.User,
		Tokens:        result.Tokens,
		SessionCookie: result.SessionCookie,
		MFA:           result.MFA,
		RedirectURL:   redirect,
	}, nil
}

type completeOAuth2CodeCommand struct {
	ProjectID string
	Provider  string
	Code      string
	State     string
	// CallerUserID 是 token 面 link 消费者的登录用户（M5 C5）：非空时要求
	// state.LinkUserID 与之相等，否则 PermissionDenied。
	CallerUserID string
	// SessionCookies 是 HTTP 回调面携带的端用户会话 cookie（project → cookie
	// value，M5 C5）：非 nil 时 link 流要求其中含 state 对应项目且会话本人
	// == LinkUserID。token 面传 nil。
	SessionCookies map[string]string
	// OAuthNonces 是 HTTP 回调面携带的 OAuth nonce cookie（project → value，
	// M5 C5 login CSRF）：非 nil 时强制与 state.Nonce 配对，不匹配即拒。
	OAuthNonces map[string]string
}

type completeOAuth2CodeResult struct {
	ProjectID     string
	SuccessURL    string
	FailureURL    string
	User          *User
	Tokens        *TokenBundle
	SessionCookie string
	MFA           *MFASignInChallenge
}

func (a *Account) completeOAuth2Code(ctx context.Context, cmd completeOAuth2CodeCommand) (*completeOAuth2CodeResult, error) {
	if a.oauthState == nil {
		return nil, status.Error(codes.Unimplemented, "oauth2 is not configured")
	}
	provider := normalizeOAuthProvider(cmd.Provider)
	code := strings.TrimSpace(cmd.Code)
	stateID := strings.TrimSpace(cmd.State)
	if provider == "" {
		return nil, status.Error(codes.InvalidArgument, "provider is required")
	}
	if code == "" {
		return nil, status.Error(codes.InvalidArgument, "code is required")
	}
	if stateID == "" {
		return nil, status.Error(codes.InvalidArgument, "state is required")
	}

	oauthState, err := a.oauthState.Consume(ctx, stateID)
	if err != nil {
		return nil, err
	}

	if oauthState.Provider != provider {
		return nil, status.Error(codes.Unauthenticated, "oauth provider mismatch")
	}
	projectID := oauthState.ProjectID
	if cmd.ProjectID != "" && cmd.ProjectID != projectID {
		return nil, status.Error(codes.Unauthenticated, "oauth project mismatch")
	}

	// M5 C5：login CSRF——HTTP 回调面必须回带与 state 配对的 nonce cookie
	//（发起时种入、state 记录同值）。缺失/不匹配一律拒绝（fail-closed）。
	if cmd.OAuthNonces != nil {
		nonce := cmd.OAuthNonces[projectID]
		if oauthState.Nonce == "" || nonce == "" ||
			subtle.ConstantTimeCompare([]byte(nonce), []byte(oauthState.Nonce)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "oauth state nonce mismatch")
		}
	}

	project, err := a.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, status.Error(codes.NotFound, "project not found")
	}

	oauthCfg, err := a.loadOAuthProvider(ctx, projectID, provider)
	if err != nil {
		return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL}, err
	}

	if a.oauthFactory == nil {
		return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL}, status.Error(codes.Unimplemented, "oauth factory is not configured")
	}
	authClient, err := a.oauthFactory.NewAuthenticator(provider, oauthCfg.ClientID, oauthCfg.ClientSecret, a.oauthCallbackURL(provider), oauthCfg.Scopes)
	if err != nil {
		return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL}, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	profile, err := authClient.Exchange(ctx, code, oauthState.PKCEVerifier)
	if err != nil {
		return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL}, status.Errorf(codes.Unauthenticated, "oauth code exchange failed: %v", err)
	}

	if oauthState.LinkUserID != "" {
		// M5 C5：link 流消费归属校验（fail-closed）——
		//   token 面：CallerUserID 必须等于 LinkUserID；
		//   回调面：必须携带 state 对应项目的端用户会话 cookie 且会话本人
		//   == LinkUserID（堵"攻击者 code 注入受害者回调完成冒名 link"）。
		// 两条通道皆无（含 login-token 方法消费 link state）一律拒绝。
		callerMatched := cmd.CallerUserID != "" &&
			subtle.ConstantTimeCompare([]byte(cmd.CallerUserID), []byte(oauthState.LinkUserID)) == 1
		if !callerMatched {
			if cmd.CallerUserID != "" {
				return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL},
					status.Error(codes.PermissionDenied, "oauth link caller mismatch")
			}
			if cmd.SessionCookies == nil {
				return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL},
					status.Error(codes.PermissionDenied, "oauth link caller mismatch")
			}
			if err := a.verifyLinkSessionCookie(ctx, projectID, oauthState.LinkUserID, cmd.SessionCookies); err != nil {
				return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL}, err
			}
		}
		if err := a.linkOAuthIdentity(ctx, projectID, oauthState.LinkUserID, provider, profile); err != nil {
			return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL}, err
		}
		linked, err := a.requireAccountUser(ctx, projectID, oauthState.LinkUserID)
		if err != nil {
			return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL}, err
		}
		return &completeOAuth2CodeResult{
			ProjectID:  projectID,
			SuccessURL: oauthState.SuccessURL,
			FailureURL: oauthState.FailureURL,
			User:       linked,
		}, nil
	}

	user, err := a.resolveOAuthUser(ctx, projectID, provider, profile)
	if err != nil {
		return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL}, err
	}
	if !users.CanAuthenticate(user.Status) {
		return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL}, status.Error(codes.Unauthenticated, "user account is not active")
	}

	user, tokens, cookie, mfa, err := a.finishSignInWithProvider(ctx, projectID, user, provider)
	if err != nil {
		return &completeOAuth2CodeResult{SuccessURL: oauthState.SuccessURL, FailureURL: oauthState.FailureURL}, err
	}
	return &completeOAuth2CodeResult{
		ProjectID:     projectID,
		SuccessURL:    oauthState.SuccessURL,
		FailureURL:    oauthState.FailureURL,
		User:          user,
		Tokens:        tokens,
		SessionCookie: cookie,
		MFA:           mfa,
	}, nil
}

func (a *Account) loadOAuthProvider(ctx context.Context, projectID, provider string) (*domainOAuthProvider, error) {
	if a.oauthProviders == nil {
		return nil, status.Error(codes.FailedPrecondition, "oauth provider repository is not configured")
	}
	cfg, err := a.oauthProviders.GetOAuthProvider(ctx, projectID, provider)
	if err != nil {
		return nil, err
	}
	if cfg == nil || !cfg.Enabled {
		return nil, status.Error(codes.FailedPrecondition, "oauth provider is not enabled for this project")
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, status.Error(codes.FailedPrecondition, "oauth provider credentials are missing")
	}
	return &domainOAuthProvider{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Scopes:       cfg.Scopes,
	}, nil
}

type domainOAuthProvider struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
}

func (a *Account) oauthCallbackURL(provider string) string {
	base := strings.TrimRight(a.publicBaseURL(), "/")
	return fmt.Sprintf("%s/v1/account/oauth2/%s/callback", base, provider)
}

func (a *Account) publicBaseURL() string {
	if u := strings.TrimSpace(a.cfg.GetServer().GetHttp().GetPublicUrl()); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:9099"
}

func normalizeOAuthProvider(provider string) string {
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
	case domainauth.ProviderWeChatApp:
		return domainauth.ProviderWeChatApp
	default:
		return ""
	}
}

func validateRedirectURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("url host is required")
	}
	return nil
}

func appendQuery(rawURL, key, value string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

func appendOAuthSPAFragment(rawURL, userID string, tokens *TokenBundle) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	vals := url.Values{}
	if userID != "" {
		vals.Set("userId", userID)
	}
	if tokens != nil {
		vals.Set("access_token", tokens.AccessToken)
	}
	u.Fragment = vals.Encode()
	return u.String()
}

// appendOAuthMFAFragment 构造 MFA 挑战重定向：无会话，只有 userId 与挑战信息。
func appendOAuthMFAFragment(rawURL, userID, challengeToken string, factors []domainauth.Factor) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	vals := url.Values{}
	if userID != "" {
		vals.Set("userId", userID)
	}
	if challengeToken != "" {
		vals.Set("mfaRequired", "true")
		vals.Set("challengeToken", challengeToken)
	}
	vals.Set("mfaFactorTypes", mfaFactorTypes(factors))
	u.Fragment = vals.Encode()
	return u.String()
}

func mfaFactorTypes(factors []domainauth.Factor) string {
	seen := map[string]struct{}{}
	parts := make([]string, 0, len(factors))
	for _, f := range factors {
		if f.Type == "" {
			continue
		}
		if _, ok := seen[f.Type]; ok {
			continue
		}
		seen[f.Type] = struct{}{}
		parts = append(parts, f.Type)
	}
	return strings.Join(parts, ",")
}

// verifyLinkSessionCookie 校验 link 流回调携带的端用户会话 cookie（M5 C5）：
// cookie 必须为 state 对应项目、HMAC 验签通过，且解析出的会话存在、未过期、
// 归属 link 目标用户。任一不满足即 PermissionDenied（不泄露具体原因，防探测）。
func (a *Account) verifyLinkSessionCookie(ctx context.Context, projectID, linkUserID string, cookies map[string]string) error {
	if a.sessionCookies == nil {
		return status.Error(codes.PermissionDenied, "oauth link identity cannot be verified")
	}
	raw := cookies[projectID]
	if raw == "" {
		return status.Error(codes.PermissionDenied, "oauth link requires session cookie")
	}
	cookieProject, sessionID, err := a.sessionCookies.Verify(raw)
	if err != nil || cookieProject != projectID || sessionID == "" {
		return status.Error(codes.PermissionDenied, "oauth link requires session cookie")
	}
	if a.sessionRepo == nil {
		return status.Error(codes.PermissionDenied, "oauth link identity cannot be verified")
	}
	sess, err := a.sessionRepo.GetByID(ctx, projectID, sessionID)
	if err != nil || sess == nil {
		return status.Error(codes.PermissionDenied, "oauth link requires session cookie")
	}
	if sess.ExpireAt.IsZero() || sess.ExpireAt.Before(time.Now()) || sess.UserID != linkUserID {
		return status.Error(codes.PermissionDenied, "oauth link caller mismatch")
	}
	return nil
}

func (a *Account) requireProject(ctx context.Context, projectID string) error {
	project, err := a.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if project == nil {
		return status.Error(codes.NotFound, "project not found")
	}
	return nil
}

func (a *Account) validateProjectOAuthRedirectURLs(ctx context.Context, projectID, successURL, failureURL string) error {
	project, err := a.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if project == nil {
		return status.Error(codes.NotFound, "project not found")
	}
	allowed := projects.OAuthAllowedRedirectURLs(project.Settings)
	if len(allowed) == 0 {
		allowed = projects.DefaultOAuthRedirectAllowlist(a.publicBaseURL())
	}
	if !projects.MatchRedirectURL(successURL, allowed) {
		return status.Error(codes.InvalidArgument, "success url is not allowed for this project")
	}
	if !projects.MatchRedirectURL(failureURL, allowed) {
		return status.Error(codes.InvalidArgument, "failure url is not allowed for this project")
	}
	return nil
}
