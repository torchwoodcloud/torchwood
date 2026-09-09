package client

// 测试装配辅助（Round4 J4-2 自 test_helpers.go 改名隔离）：全部消费方均为
// *_test.go，不再进入本包生产编译图。外部包的集成测试请自行以真实依赖
// 装配（见 internal/api/serverhttp、tests/acceptance 的就地构造）。

import (
	"context"

	"github.com/redis/go-redis/v9"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/messaging"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	infraauth "github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	inframessaging "github.com/torchwoodcloud/torchwood/internal/infra/messaging"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

func NewTestAccount(cfg *config.AppConfig, projectRepo projects.Repository, db *clients.Database) *Account {
	return NewTestAccountWithRedis(cfg, projectRepo, db, nil)
}

func NewTestAccountWithRedis(cfg *config.AppConfig, projectRepo projects.Repository, db *clients.Database, rdb *redis.Client) *Account {
	return NewTestAccountWithDeps(cfg, projectRepo, nil, db, rdb, nil, nil)
}

func NewTestAccountWithMailer(cfg *config.AppConfig, projectRepo projects.Repository, db *clients.Database, rdb *redis.Client, mailer messaging.Mailer) *Account {
	return NewTestAccountWithDeps(cfg, projectRepo, nil, db, rdb, mailer, nil)
}

func NewTestAccountWithDeps(
	cfg *config.AppConfig,
	projectRepo projects.Repository,
	oauthProviders projects.OAuthProviderRepository,
	db *clients.Database,
	rdb *redis.Client,
	mailer messaging.Mailer,
	sms messaging.SMSSender,
) *Account {
	var usersRepo = bunrepo.NewUserRepository(db)
	var sessionRepo = bunrepo.NewSessionRepository(db)
	var identities = bunrepo.NewIdentityRepository(db)
	var memberships = bunrepo.NewMembershipRepository(db)
	roles := NewUserRoles(usersRepo, memberships)
	var rotation domainauth.RefreshRotationStore
	if rdb != nil {
		rotation = infraauth.NewRedisRefreshRotationStore(rdb)
	}
	sessions := infraauth.NewSessionService(cfg, sessionRepo, roles, rotation)
	var otp domainauth.OTPChallengeStore
	var oauthState domainauth.OAuthStateStore
	var tokens domainauth.AccountTokenStore
	var loginThrottle domainauth.LoginThrottle
	var rateLimiter domainauth.RateLimiter
	var mfa domainauth.MFAService
	var mfaChallenges domainauth.MFAChallengeStore
	var oneTimeTokens domainauth.OneTimeTokenStore
	if rdb != nil {
		otp = infraauth.NewRedisOTPChallengeStore(rdb, cfg)
		oauthState = infraauth.NewRedisOAuthStateStore(rdb)
		tokens = infraauth.NewRedisAccountTokenStore(rdb)
		loginThrottle = infraauth.NewRedisLoginThrottle(rdb)
		rateLimiter = infraauth.NewRedisRateLimiter(rdb)
		mfa = infraauth.NewTOTPService(cfg, rdb)
		mfaChallenges = infraauth.NewRedisMFAChallengeStore(rdb)
		oneTimeTokens = infraauth.NewRedisOneTimeTokenStore(rdb)
	}
	if mailer == nil {
		mailer = inframessaging.NewMailer(cfg)
	}
	if sms == nil {
		sms = inframessaging.NewSMSService(cfg)
	}
	return NewAccount(cfg, projectRepo, nil, oauthProviders, sessions, otp, oauthState, tokens, loginThrottle, rotation, nil, mailer, sms, rateLimiter, roles, mfa, mfaChallenges, oneTimeTokens, nil, usersRepo, identities, sessionRepo, infraauth.NewOAuthAuthenticatorFactory(), infraauth.NewWeChatMiniProgramExchanger(), infraauth.NewOTPGenerator(), nil)
}

// CaptureMailer records sent messages for tests.
type CaptureMailer struct {
	To       []string
	Subjects []string
	Bodies   []string
}

func (m *CaptureMailer) Send(_ context.Context, to, subject, body string) error {
	m.To = append(m.To, to)
	m.Subjects = append(m.Subjects, subject)
	m.Bodies = append(m.Bodies, body)
	return nil
}

// CaptureSMSSender records sent SMS for tests.
type CaptureSMSSender struct {
	To   []string
	Body []string
}

func (s *CaptureSMSSender) Send(_ context.Context, to, body string) error {
	s.To = append(s.To, to)
	s.Body = append(s.Body, body)
	return nil
}
