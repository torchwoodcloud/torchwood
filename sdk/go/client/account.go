package client

import (
	"context"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AccountService 封装 Client API 的 Account 服务。
type AccountService struct{ c *Client }

// SignUp 使用邮箱/密码注册新账户；成功（非 MFA 分支）后自动保存 token。
func (a *AccountService) SignUp(ctx context.Context, email, password, name string) (*clientv1.SignUpResponse, error) {
	resp, err := a.c.account.SignUp(ctx, &clientv1.SignUpRequest{
		Email:     email,
		Password:  password,
		Name:      name,
		ProjectId: a.c.cfg.ProjectID,
	})
	if err != nil {
		return nil, err
	}
	if !resp.MfaRequired && resp.Tokens != nil && resp.Tokens.AccessToken != "" {
		if err := a.c.saveTokens(resp.Tokens); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// SignIn 使用邮箱/密码登录；成功（非 MFA 分支）后自动保存 token。
func (a *AccountService) SignIn(ctx context.Context, email, password string) (*clientv1.SignInResponse, error) {
	resp, err := a.c.account.SignIn(ctx, &clientv1.SignInRequest{
		Email:     email,
		Password:  password,
		ProjectId: a.c.cfg.ProjectID,
	})
	if err != nil {
		return nil, err
	}
	if !resp.MfaRequired && resp.Tokens != nil && resp.Tokens.AccessToken != "" {
		if err := a.c.saveTokens(resp.Tokens); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// RefreshToken 用刷新令牌换取新令牌并保存。
func (a *AccountService) RefreshToken(ctx context.Context, refreshToken string) (*clientv1.RefreshTokenResponse, error) {
	resp, err := a.c.account.RefreshToken(ctx, &clientv1.RefreshTokenRequest{
		ProjectId:    a.c.cfg.ProjectID,
		RefreshToken: refreshToken,
	})
	if err != nil {
		return nil, err
	}
	if resp.Tokens != nil && resp.Tokens.AccessToken != "" {
		if err := a.c.saveTokens(resp.Tokens); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// Me 返回当前登录账户信息。
func (a *AccountService) Me(ctx context.Context) (*clientv1.Account, error) {
	return a.c.account.Me(ctx, &clientv1.MeRequest{ProjectId: a.c.cfg.ProjectID})
}

// SignOut 注销当前会话；成功或 token 已失效（Unauthenticated）都清空本地 token。
func (a *AccountService) SignOut(ctx context.Context) error {
	_, err := a.c.account.SignOut(ctx, &clientv1.SignOutRequest{ProjectId: a.c.cfg.ProjectID})
	if err == nil || status.Code(err) == codes.Unauthenticated {
		a.c.clearTokens()
	}
	return err
}

// UpdateAccount 更新账户资料；name/email 传 nil 表示不修改，传指针（含空串）表示更新/清空。
// 修改 email 时 url 必填（新邮箱验证链接模板，staging：验证通过前 email 保持旧值）。
func (a *AccountService) UpdateAccount(ctx context.Context, name, email *string, password, oldPassword, url string) (*clientv1.Account, error) {
	return a.c.account.UpdateAccount(ctx, &clientv1.UpdateAccountRequest{
		Name:        name,
		Email:       email,
		Password:    password,
		OldPassword: oldPassword,
		Url:         url,
	})
}

// ConfirmEmailChange 消费邮件链接中的一次性 secret 完成邮箱变更（公开方法，
// 凭 user_id + secret，无需登录——与 recovery 同一安全模型：随机 secret +
// TTL + 一次性消费；Round3 H4-2 起不再依赖登录态）。
func (a *AccountService) ConfirmEmailChange(ctx context.Context, userID, secret string) (*clientv1.Account, error) {
	return a.c.account.ConfirmEmailChange(ctx, &clientv1.ConfirmEmailChangeRequest{
		ProjectId: a.c.cfg.ProjectID,
		UserId:    userID,
		Secret:    secret,
	})
}

// ListSessions 列出当前账户的全部会话（Session.current 标记当前会话）。
func (a *AccountService) ListSessions(ctx context.Context) (*clientv1.ListSessionsResponse, error) {
	return a.c.account.ListSessions(ctx, &clientv1.ListSessionsRequest{})
}

// DeleteSession 删除指定会话（删除当前会话即登出）。
func (a *AccountService) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := a.c.account.DeleteSession(ctx, &clientv1.DeleteSessionRequest{SessionId: sessionID})
	return err
}

// DeleteSessions 删除全部会话；keepCurrent 为 true 时保留当前会话。
func (a *AccountService) DeleteSessions(ctx context.Context, keepCurrent bool) error {
	_, err := a.c.account.DeleteSessions(ctx, &clientv1.DeleteSessionsRequest{KeepCurrent: keepCurrent})
	return err
}

// GetPrefs 读取用户偏好（JSON）。
func (a *AccountService) GetPrefs(ctx context.Context) (*clientv1.GetPrefsResponse, error) {
	return a.c.account.GetPrefs(ctx, &clientv1.GetPrefsRequest{})
}

// UpdatePrefs 全量替换用户偏好（JSON）。
func (a *AccountService) UpdatePrefs(ctx context.Context, prefs map[string]any) (*clientv1.GetPrefsResponse, error) {
	st, err := toStruct(prefs)
	if err != nil {
		return nil, err
	}
	return a.c.account.UpdatePrefs(ctx, &clientv1.UpdatePrefsRequest{Prefs: st})
}

// CreateEmailOTP 发送邮箱验证码登录请求，返回 challenge_id。
func (a *AccountService) CreateEmailOTP(ctx context.Context, email string) (*clientv1.ChallengeResponse, error) {
	return a.c.account.CreateEmailOTP(ctx, &clientv1.CreateEmailOTPRequest{
		ProjectId: a.c.cfg.ProjectID,
		Email:     email,
	})
}

// CreateEmailOTPSession 用邮箱验证码完成登录；成功后自动保存 token。
func (a *AccountService) CreateEmailOTPSession(ctx context.Context, email, challengeID, otp string) (*clientv1.SignInResponse, error) {
	resp, err := a.c.account.CreateEmailOTPSession(ctx, &clientv1.CreateEmailOTPSessionRequest{
		ProjectId:   a.c.cfg.ProjectID,
		Email:       email,
		ChallengeId: challengeID,
		Otp:         otp,
	})
	if err != nil {
		return nil, err
	}
	if err := a.saveSignInTokens(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// CreateOAuth2Session 获取 OAuth2 授权跳转地址（匿名）。
func (a *AccountService) CreateOAuth2Session(ctx context.Context, provider, success, failure string) (*clientv1.CreateOAuth2SessionResponse, error) {
	return a.c.account.CreateOAuth2Session(ctx, &clientv1.CreateOAuth2SessionRequest{
		Provider:  provider,
		ProjectId: a.c.cfg.ProjectID,
		Success:   success,
		Failure:   failure,
	})
}

// CreateOAuth2TokenSession 用 OAuth2 回调 code 换取登录态；成功后自动保存 token。
func (a *AccountService) CreateOAuth2TokenSession(ctx context.Context, provider, success, failure, code, state string) (*clientv1.SignInResponse, error) {
	resp, err := a.c.account.CreateOAuth2TokenSession(ctx, &clientv1.CreateOAuth2TokenSessionRequest{
		Provider:  provider,
		ProjectId: a.c.cfg.ProjectID,
		Success:   success,
		Failure:   failure,
		Code:      code,
		State:     state,
	})
	if err != nil {
		return nil, err
	}
	if err := a.saveSignInTokens(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// CreatePhoneOTP 发送短信验证码登录请求，返回 challenge_id。
func (a *AccountService) CreatePhoneOTP(ctx context.Context, phone string) (*clientv1.ChallengeResponse, error) {
	return a.c.account.CreatePhoneOTP(ctx, &clientv1.CreatePhoneOTPRequest{
		ProjectId: a.c.cfg.ProjectID,
		Phone:     phone,
	})
}

// CreatePhoneOTPSession 用短信验证码完成登录；成功后自动保存 token。
func (a *AccountService) CreatePhoneOTPSession(ctx context.Context, phone, challengeID, otp string) (*clientv1.SignInResponse, error) {
	resp, err := a.c.account.CreatePhoneOTPSession(ctx, &clientv1.CreatePhoneOTPSessionRequest{
		ProjectId:   a.c.cfg.ProjectID,
		Phone:       phone,
		ChallengeId: challengeID,
		Otp:         otp,
	})
	if err != nil {
		return nil, err
	}
	if err := a.saveSignInTokens(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// CreateWeChatMiniProgramSession 用微信小程序 code 完成登录；成功后自动保存 token。
func (a *AccountService) CreateWeChatMiniProgramSession(ctx context.Context, code string) (*clientv1.SignInResponse, error) {
	resp, err := a.c.account.CreateWeChatMiniProgramSession(ctx, &clientv1.CreateWeChatMiniProgramSessionRequest{
		ProjectId: a.c.cfg.ProjectID,
		Code:      code,
	})
	if err != nil {
		return nil, err
	}
	if err := a.saveSignInTokens(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// CreateAnonymousSession 创建匿名会话；成功后自动保存 token。
func (a *AccountService) CreateAnonymousSession(ctx context.Context) (*clientv1.SignInResponse, error) {
	resp, err := a.c.account.CreateAnonymousSession(ctx, &clientv1.CreateAnonymousSessionRequest{
		ProjectId: a.c.cfg.ProjectID,
	})
	if err != nil {
		return nil, err
	}
	if err := a.saveSignInTokens(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// CreateOAuth2LinkSession 获取第三方账号绑定授权跳转地址（需已登录）。
func (a *AccountService) CreateOAuth2LinkSession(ctx context.Context, provider, success, failure string) (*clientv1.CreateOAuth2SessionResponse, error) {
	return a.c.account.CreateOAuth2LinkSession(ctx, &clientv1.CreateOAuth2LinkSessionRequest{
		Provider:  provider,
		ProjectId: a.c.cfg.ProjectID,
		Success:   success,
		Failure:   failure,
	})
}

// CreateOAuth2LinkTokenSession 用 OAuth2 回调 code 绑定第三方账号（需已登录）。
func (a *AccountService) CreateOAuth2LinkTokenSession(ctx context.Context, provider, code, state string) (*clientv1.Account, error) {
	return a.c.account.CreateOAuth2LinkTokenSession(ctx, &clientv1.CreateOAuth2LinkTokenSessionRequest{
		Provider:  provider,
		ProjectId: a.c.cfg.ProjectID,
		Code:      code,
		State:     state,
	})
}

// CreateVerification 发送邮箱验证邮件（url 为带 {{code}} 占位的确认链接模板）。
func (a *AccountService) CreateVerification(ctx context.Context, url string) (*clientv1.CreateVerificationResponse, error) {
	return a.c.account.CreateVerification(ctx, &clientv1.CreateVerificationRequest{
		ProjectId: a.c.cfg.ProjectID,
		Url:       url,
	})
}

// UpdateVerification 用邮件中的 secret 确认邮箱（公开方法，无需登录）。
func (a *AccountService) UpdateVerification(ctx context.Context, userID, secret string) (*clientv1.Account, error) {
	return a.c.account.UpdateVerification(ctx, &clientv1.UpdateVerificationRequest{
		ProjectId: a.c.cfg.ProjectID,
		UserId:    userID,
		Secret:    secret,
	})
}

// CreateRecovery 发送密码找回邮件（公开方法）。
func (a *AccountService) CreateRecovery(ctx context.Context, email, url string) error {
	_, err := a.c.account.CreateRecovery(ctx, &clientv1.CreateRecoveryRequest{
		ProjectId: a.c.cfg.ProjectID,
		Email:     email,
		Url:       url,
	})
	return err
}

// UpdateRecovery 用邮件中的 secret 重置密码（公开方法）。
func (a *AccountService) UpdateRecovery(ctx context.Context, userID, secret, password string) error {
	_, err := a.c.account.UpdateRecovery(ctx, &clientv1.UpdateRecoveryRequest{
		ProjectId: a.c.cfg.ProjectID,
		UserId:    userID,
		Secret:    secret,
		Password:  password,
	})
	return err
}

// ListFactors 列出 MFA 因子。
func (a *AccountService) ListFactors(ctx context.Context) (*clientv1.ListFactorsResponse, error) {
	return a.c.account.ListFactors(ctx, &clientv1.ListFactorsRequest{})
}

// CreateTOTPFactor 创建 TOTP 因子；secret/otpauth_url 仅本次响应返回明文。
func (a *AccountService) CreateTOTPFactor(ctx context.Context) (*clientv1.TOTPFactor, error) {
	return a.c.account.CreateTOTPFactor(ctx, &clientv1.CreateTOTPFactorRequest{})
}

// VerifyTOTPFactor 校验 code 并激活 TOTP 因子。
func (a *AccountService) VerifyTOTPFactor(ctx context.Context, factorID, code string) (*clientv1.Factor, error) {
	return a.c.account.VerifyTOTPFactor(ctx, &clientv1.VerifyTOTPFactorRequest{
		FactorId: factorID,
		Code:     code,
	})
}

// DeleteFactor 删除 MFA 因子；verified 因子需携带 TOTP 二次验证 code，
// pending 因子可传空串（R05-P1-4）。
func (a *AccountService) DeleteFactor(ctx context.Context, factorID, code string) error {
	_, err := a.c.account.DeleteFactor(ctx, &clientv1.DeleteFactorRequest{
		FactorId: factorID,
		Code:     code,
	})
	return err
}

// CreateMFASession 登录二次验证：携带 signIn/signUp 返回的 challenge_token 完成挑战；
// 成功后自动保存 token。
func (a *AccountService) CreateMFASession(ctx context.Context, challengeToken, factorID, code string) (*clientv1.SignInResponse, error) {
	resp, err := a.c.account.CreateMFASession(ctx, &clientv1.CreateMFASessionRequest{
		ProjectId:      a.c.cfg.ProjectID,
		ChallengeToken: challengeToken,
		FactorId:       factorID,
		Code:           code,
	})
	if err != nil {
		return nil, err
	}
	if err := a.saveSignInTokens(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// CreateJWT 用当前会话换取一次性 JWT（用于服务端安全回调/Webhook）。
func (a *AccountService) CreateJWT(ctx context.Context) (*clientv1.CreateJWTResponse, error) {
	return a.c.account.CreateJWT(ctx, &clientv1.CreateJWTRequest{})
}

// CreateMagicURLSession 发送 Magic URL 登录邮件（公开方法）。
func (a *AccountService) CreateMagicURLSession(ctx context.Context, email, url string) (*clientv1.ChallengeResponse, error) {
	return a.c.account.CreateMagicURLSession(ctx, &clientv1.CreateMagicURLSessionRequest{
		ProjectId: a.c.cfg.ProjectID,
		Email:     email,
		Url:       url,
	})
}

// UpdateMagicURLSession 用邮件中的 secret 完成 Magic URL 登录；成功后自动保存 token。
func (a *AccountService) UpdateMagicURLSession(ctx context.Context, userID, secret string) (*clientv1.SignInResponse, error) {
	resp, err := a.c.account.UpdateMagicURLSession(ctx, &clientv1.UpdateMagicURLSessionRequest{
		ProjectId: a.c.cfg.ProjectID,
		UserId:    userID,
		Secret:    secret,
	})
	if err != nil {
		return nil, err
	}
	if err := a.saveSignInTokens(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// ListLogs 返回最近 limit 条账号操作日志。
func (a *AccountService) ListLogs(ctx context.Context, limit int32) (*clientv1.ListLogsResponse, error) {
	return a.c.account.ListLogs(ctx, &clientv1.ListLogsRequest{Limit: limit})
}

// saveSignInTokens 非 MFA 且返回 token 时自动保存（SignIn 系列响应统一处理）。
func (a *AccountService) saveSignInTokens(resp *clientv1.SignInResponse) error {
	if resp == nil || resp.MfaRequired || resp.Tokens == nil || resp.Tokens.AccessToken == "" {
		return nil
	}
	return a.c.saveTokens(resp.Tokens)
}
