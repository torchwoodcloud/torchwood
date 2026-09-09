package auth

import (
	"context"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// NewOAuthAuthenticatorFactory 提供 domain OAuthAuthenticatorFactory 的默认实现。
func NewOAuthAuthenticatorFactory() domainauth.OAuthAuthenticatorFactory {
	return oauthAuthenticatorFactory{}
}

type oauthAuthenticatorFactory struct{}

func (oauthAuthenticatorFactory) NewAuthenticator(provider, clientID, clientSecret, redirectURL string, scopes []string) (domainauth.OAuthAuthenticator, error) {
	return NewOAuthAuthenticator(provider, clientID, clientSecret, redirectURL, scopes)
}

// NewWeChatMiniProgramExchanger 提供 WeChatMiniProgramExchanger 的默认实现。
func NewWeChatMiniProgramExchanger() domainauth.WeChatMiniProgramExchanger {
	return weChatMiniProgramExchanger{}
}

type weChatMiniProgramExchanger struct{}

func (weChatMiniProgramExchanger) ExchangeWeChatMiniProgramCode(ctx context.Context, appID, appSecret, code string) (*domainauth.OAuthUserInfo, error) {
	return ExchangeWeChatMiniProgramCode(ctx, appID, appSecret, code)
}

// NewOTPGenerator 提供 OTPGenerator 的默认实现。
func NewOTPGenerator() domainauth.OTPGenerator {
	return otpGenerator{}
}

type otpGenerator struct{}

func (otpGenerator) GenerateOTP(digits int) (string, error) {
	return GenerateOTP(digits)
}
