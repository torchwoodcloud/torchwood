package client

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	"github.com/torchwooddev/torchwood/internal/domain/audit"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	domainidgen "github.com/torchwooddev/torchwood/internal/domain/idgen"
	"github.com/torchwooddev/torchwood/internal/domain/messaging"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"github.com/torchwooddev/torchwood/pkg/jwtparser"
	"github.com/torchwooddev/torchwood/pkg/password"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Account struct {
	cfg             *config.AppConfig
	projectRepo     projects.Repository
	oauthProviders  projects.OAuthProviderRepository
	usersRepo       users.Repository
	identities      domainauth.IdentityRepository
	sessionRepo     domainauth.SessionRepository
	sessions        domainauth.SessionService
	otp             domainauth.OTPChallengeStore
	oauthState      domainauth.OAuthStateStore
	tokens          domainauth.AccountTokenStore
	loginThrottle   domainauth.LoginThrottle
	rotation        domainauth.RefreshRotationStore
	idGen           domainidgen.Generator
	mailer          messaging.Mailer
	sms             messaging.SMSSender
	rateLimiter     domainauth.RateLimiter
	roles           domainauth.UserRoleResolver
	mfa             domainauth.MFAService
	mfaChallenges   domainauth.MFAChallengeStore
	oneTimeTokens   domainauth.OneTimeTokenStore
	auditRepo       audit.Repository
	oauthFactory    domainauth.OAuthAuthenticatorFactory
	weChatExchanger domainauth.WeChatMiniProgramExchanger
	otpGenerator    domainauth.OTPGenerator
	// sessionCookies 校验并解出端用户会话 cookie（M5 C5 OAuth link 回调面）。
	sessionCookies domainauth.SessionCookieVerifier
}

func NewAccount(
	cfg *config.AppConfig,
	projectRepo projects.Repository,
	oauthProviders projects.OAuthProviderRepository,
	sessions domainauth.SessionService,
	otp domainauth.OTPChallengeStore,
	oauthState domainauth.OAuthStateStore,
	tokens domainauth.AccountTokenStore,
	loginThrottle domainauth.LoginThrottle,
	rotation domainauth.RefreshRotationStore,
	idGen domainidgen.Generator,
	mailer messaging.Mailer,
	sms messaging.SMSSender,
	rateLimiter domainauth.RateLimiter,
	roles domainauth.UserRoleResolver,
	mfa domainauth.MFAService,
	mfaChallenges domainauth.MFAChallengeStore,
	oneTimeTokens domainauth.OneTimeTokenStore,
	auditRepo audit.Repository,
	usersRepo users.Repository,
	identities domainauth.IdentityRepository,
	sessionRepo domainauth.SessionRepository,
	oauthFactory domainauth.OAuthAuthenticatorFactory,
	weChatExchanger domainauth.WeChatMiniProgramExchanger,
	otpGenerator domainauth.OTPGenerator,
	sessionCookies domainauth.SessionCookieVerifier,
) *Account {
	return &Account{
		cfg:             cfg,
		projectRepo:     projectRepo,
		oauthProviders:  oauthProviders,
		usersRepo:       usersRepo,
		identities:      identities,
		sessionRepo:     sessionRepo,
		sessions:        sessions,
		otp:             otp,
		oauthState:      oauthState,
		tokens:          tokens,
		loginThrottle:   normalizeLoginThrottle(loginThrottle),
		rotation:        rotation,
		oauthFactory:    oauthFactory,
		weChatExchanger: weChatExchanger,
		otpGenerator:    otpGenerator,
		idGen:           idGen,
		mailer:          mailer,
		sms:             sms,
		rateLimiter:     rateLimiter,
		roles:           roles,
		mfa:             mfa,
		mfaChallenges:   normalizeMFAChallengeStore(mfaChallenges),
		oneTimeTokens:   oneTimeTokens,
		auditRepo:       auditRepo,
		sessionCookies:  sessionCookies,
	}
}

// normalizeLoginThrottle 把未注入的 loginThrottle 显式落为 Noop（仅供测试
// 场景会出现）：用例层不再对 nil 分支静默旁路（Round4 J5-5），生产组合根
// 恒注入 infra/auth 的 Redis 版本。
func normalizeLoginThrottle(t domainauth.LoginThrottle) domainauth.LoginThrottle {
	if t == nil {
		return domainauth.NoopLoginThrottle{}
	}
	return t
}

// normalizeMFAChallengeStore 与 normalizeLoginThrottle 同理（Round4 J5-5）。
// 注意：Noop 仅消除 nil 风险，不具备真实挑战语义；MFA 能力开关仍以 a.mfa
// 是否注入为准。
func normalizeMFAChallengeStore(s domainauth.MFAChallengeStore) domainauth.MFAChallengeStore {
	if s == nil {
		return domainauth.NoopMFAChallengeStore{}
	}
	return s
}

// generateOTP 通过注入的 OTPGenerator 生成验证码；未注入时回退到
// 纯随机生成（与 infra/auth.GenerateOTP 同逻辑，避免 app 层直接依赖 infra）。
func (a *Account) generateOTP(digits int) (string, error) {
	if a.otpGenerator != nil {
		return a.otpGenerator.GenerateOTP(digits)
	}
	if digits <= 0 || digits > 10 {
		return "", fmt.Errorf("invalid otp digits: %d", digits)
	}
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	format := fmt.Sprintf("%%0%dd", digits)
	return fmt.Sprintf(format, n), nil
}

type SignUpCommand struct {
	ProjectID string
	Email     string
	Password  string
	Name      string
}

type SignInCommand struct {
	ProjectID string
	Email     string
	Password  string
}

type RefreshTokenCommand struct {
	ProjectID    string
	RefreshToken string
}

type User struct {
	ID            string
	Email         string
	Name          string
	Status        string
	EmailVerified bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type TokenBundle = domainauth.TokenBundle

type Session struct {
	ID        string
	UserID    string
	Provider  string
	UserAgent string
	IP        string
	ExpireAt  time.Time
	CreatedAt time.Time
	Current   bool
}

type UpdateAccountCommand struct {
	// Name/Email 为指针（D-1 presence 语义）：nil=不修改；非 nil（含空串）=更新/清空。
	Name        *string
	Email       *string
	URL         string // 改邮箱时必填：新邮箱验证链接模板（语义同 CreateVerificationRequest.url）
	Password    string
	OldPassword string
}

type ConfirmEmailChangeCommand struct {
	ProjectID string
	UserID    string
	Secret    string
}

// SignUp 频控：每 IP 每小时最多 10 次。
const (
	signUpIPWindow = time.Hour
	signUpIPLimit  = 10
)

// dummyPasswordHash 是固定哑哈希，用户不存在时也执行一次 Verify，
// 保持 SignIn 两条失败路径的耗时一致。
var dummyPasswordHash = sync.OnceValue(func() string {
	h, err := password.Hash("torchwood-dummy-signin-password")
	if err != nil {
		return ""
	}
	return h
})

// init 预热 dummyPasswordHash，消除首次 SignIn 用户不存在路径的时序差异
// （R05-P2-9：包初始化时即完成 bcrypt 预热）。
func init() { dummyPasswordHash() }

func (a *Account) checkSignUpRateLimit(ctx context.Context, projectID, ip string) error {
	// nil 容忍：未装配限流器或拿不到客户端 IP 时不做限制。
	if a.rateLimiter == nil || ip == "" {
		return nil
	}
	return a.rateLimiter.Allow(ctx, "signup:ip:"+projectID+":"+ip, signUpIPLimit, signUpIPWindow)
}

func (a *Account) SignUp(ctx context.Context, cmd SignUpCommand) (*User, *TokenBundle, string, *MFASignInChallenge, error) {
	if cmd.ProjectID == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	email := normalizeEmail(cmd.Email)
	if email == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "email is required")
	}
	if err := validateEmail(email); err != nil {
		return nil, nil, "", nil, err
	}
	if err := validatePasswordStrength(cmd.Password); err != nil {
		return nil, nil, "", nil, err
	}
	clientInfo := contexts.ClientInfoFrom(ctx)
	project, err := a.projectRepo.GetProject(ctx, cmd.ProjectID)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if project == nil {
		return nil, nil, "", nil, status.Error(codes.NotFound, "project not found")
	}
	// 频控放在 project 校验之后并按 project 维度计数：无效 project 不污染
	// 频控键，不同 project 各自独立计数（R05-P3-11）。
	if err := a.checkSignUpRateLimit(ctx, project.ID, clientInfo.IP); err != nil {
		return nil, nil, "", nil, err
	}

	existing, err := a.usersRepo.GetByEmail(ctx, project.ID, email)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if err := users.RequireUniqueEmail(existing); err != nil {
		return nil, nil, "", nil, appshared.MapUserError(err)
	}

	userID, err := a.generateUserID(ctx, project.ID)
	if err != nil {
		return nil, nil, "", nil, err
	}
	registered, err := users.Register(users.RegisterInput{
		ID:       userID,
		Email:    email,
		Password: cmd.Password,
		Name:     cmd.Name,
	})
	if err != nil {
		return nil, nil, "", nil, appshared.MapUserError(err)
	}
	if err := a.usersRepo.Insert(ctx, project.ID, registered); err != nil {
		if errors.Is(err, users.ErrEmailAlreadyRegistered) {
			return nil, nil, "", nil, appshared.MapUserError(err)
		}
		return nil, nil, "", nil, fmt.Errorf("insert user: %w", err)
	}

	return a.finishSignIn(ctx, project.ID, accountUser(registered))
}

func (a *Account) generateUserID(ctx context.Context, projectID string) (string, error) {
	if a.idGen != nil {
		return a.idGen.NewID(ctx, projectID, domainidgen.ResourceUsers)
	}
	return idgen.UUID().String(), nil
}

func (a *Account) finishSignIn(ctx context.Context, projectID string, user *User) (*User, *TokenBundle, string, *MFASignInChallenge, error) {
	return a.finishSignInWithProvider(ctx, projectID, user, domainauth.ProviderEmail)
}

func (a *Account) SignIn(ctx context.Context, cmd SignInCommand) (*User, *TokenBundle, string, *MFASignInChallenge, error) {
	if cmd.ProjectID == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	email := normalizeEmail(cmd.Email)
	if email == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "email is required")
	}
	if cmd.Password == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "password is required")
	}
	clientInfo := contexts.ClientInfoFrom(ctx)
	if err := a.checkLoginThrottle(ctx, email, clientInfo.IP); err != nil {
		return nil, nil, "", nil, err
	}
	invalidCredentials := func() (*User, *TokenBundle, string, *MFASignInChallenge, error) {
		return nil, nil, "", nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	project, err := a.projectRepo.GetProject(ctx, cmd.ProjectID)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if project == nil {
		return nil, nil, "", nil, status.Error(codes.NotFound, "project not found")
	}

	found, err := a.usersRepo.GetByEmail(ctx, project.ID, email)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if found == nil {
		// 用户不存在时对固定哑哈希执行一次 Verify，抹平"不存在"与"密码错误"
		// 两条路径的响应时序差异（防枚举）。
		_, _ = password.Verify(cmd.Password, dummyPasswordHash())
		// 未注册邮箱不记录失败计数：既不影响真实用户邮箱键，也不污染 IP 键
		// （R05-P1-5：未注册邮箱连续失败不得触发锁定）。
		return invalidCredentials()
	}
	if ok, _ := password.Verify(cmd.Password, found.PasswordHash); !ok {
		a.recordLoginFailure(ctx, email, clientInfo.IP)
		return invalidCredentials()
	}

	if !found.CanAuthenticate() {
		return nil, nil, "", nil, status.Error(codes.Unauthenticated, "user account is not active")
	}
	a.resetLoginThrottle(ctx, email, clientInfo.IP)
	return a.finishSignIn(ctx, project.ID, accountUser(found))
}

// checkLoginThrottle / recordLoginFailure / resetLoginThrottle 不再判 nil
// （Round4 J5-5）：构造期已把缺失依赖显式落为 NoopLoginThrottle（仅供测试），
// 生产路径恒为 Redis 实现，频控不会因漏注入被静默关闭。
func (a *Account) checkLoginThrottle(ctx context.Context, email, ip string) error {
	return a.loginThrottle.Check(ctx, domainauth.LoginNamespaceEndUser, email, ip)
}

func (a *Account) recordLoginFailure(ctx context.Context, email, ip string) {
	// intentionally ignored: throttle is best-effort, failure must not block login
	_ = a.loginThrottle.RecordFailure(ctx, domainauth.LoginNamespaceEndUser, email, ip)
}

func (a *Account) resetLoginThrottle(ctx context.Context, email, ip string) {
	// intentionally ignored: throttle reset is best-effort
	_ = a.loginThrottle.Reset(ctx, domainauth.LoginNamespaceEndUser, email, ip)
}

func (a *Account) Me(ctx context.Context) (*User, error) {
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	return a.requireAccountUser(ctx, p.ProjectID, p.UserID)
}

func (a *Account) SignOut(ctx context.Context) error {
	p, ok := contexts.Principal(ctx)
	if !ok || p.SessionID == "" {
		return nil
	}
	if a.sessionRepo == nil {
		return nil
	}
	return a.sessionRepo.Delete(ctx, p.ProjectID, p.SessionID)
}

func (a *Account) RefreshToken(ctx context.Context, cmd RefreshTokenCommand) (*TokenBundle, string, error) {
	if cmd.RefreshToken == "" {
		return nil, "", status.Error(codes.InvalidArgument, "refresh_token is required")
	}
	claims, ok := jwtparser.Parse(jwtparser.DeriveKey(a.cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeEndUserJWT), cmd.RefreshToken)
	if !ok {
		return nil, "", status.Error(codes.Unauthenticated, "invalid refresh token")
	}
	if claims.TokenType != jwtparser.TokenTypeRefresh || claims.ActorKind != "end_user" {
		return nil, "", status.Error(codes.Unauthenticated, "invalid refresh token")
	}
	projectID := claims.ProjectID
	if cmd.ProjectID != "" && cmd.ProjectID != projectID {
		return nil, "", status.Error(codes.Unauthenticated, "invalid refresh token")
	}
	if claims.SessionID == "" || claims.UserID == "" {
		return nil, "", status.Error(codes.Unauthenticated, "invalid refresh token")
	}
	if err := a.sessions.EnsureActiveSession(ctx, projectID, claims.SessionID, claims.UserID); err != nil {
		return nil, "", err
	}
	if err := a.ensureUserCanAuthenticate(ctx, projectID, claims.UserID); err != nil {
		return nil, "", err
	}
	if a.rotation == nil {
		return a.sessions.IssueTokens(ctx, projectID, claims.UserID, claims.Username, claims.SessionID)
	}

	refreshTTL := 7 * 24 * time.Hour
	if d, err := time.ParseDuration(a.cfg.GetSecurity().GetJwt().GetRefreshTtl()); err == nil {
		refreshTTL = d
	}
	rotationKey := domainauth.RefreshRotationKey(projectID, claims.SessionID)
	newRefreshTokenID := idgen.UUID().String()
	result, err := a.rotation.Rotate(ctx, rotationKey, claims.TokenID, newRefreshTokenID, refreshTTL)
	if err != nil {
		return nil, "", err
	}
	switch result {
	case domainauth.RotateOK:
		return a.sessions.IssueTokensWithRefreshID(ctx, projectID, claims.UserID, claims.Username, claims.SessionID, newRefreshTokenID)
	case domainauth.RotateMismatch:
		if a.sessionRepo != nil {
			_ = a.sessionRepo.Delete(ctx, projectID, claims.SessionID)
		}
		return nil, "", status.Error(codes.Unauthenticated, "refresh token reuse detected")
	default: // RotateMissing
		return nil, "", status.Error(codes.Unauthenticated, "session expired")
	}
}

func (a *Account) UpdateAccount(ctx context.Context, cmd UpdateAccountCommand) (*User, error) {
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	found, err := a.usersRepo.GetByID(ctx, p.ProjectID, p.UserID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}

	updates := map[string]any{}
	if cmd.Name != nil {
		updates["name"] = *cmd.Name
	}
	hash := found.PasswordHash
	oldEmail := normalizeEmail(found.Email)
	emailChanging := false
	clearingEmail := false
	stagedEmail := ""
	if cmd.Email != nil {
		newEmail := normalizeEmail(*cmd.Email)
		switch {
		case newEmail == "":
			// D-1：设置空串=清空。email 是登录凭据，清空属敏感变更（下方与改密
			// 同门槛要求旧密码）；同时丢弃未确认的 pending_email，避免悬置 staging。
			clearingEmail = true
			updates["email"] = ""
			if found.PendingEmail != "" {
				updates["pending_email"] = ""
			}
		case newEmail != oldEmail:
			if err := validateEmail(newEmail); err != nil {
				return nil, err
			}
			// 邮箱变更走 staging（R05-P1-2，A 档）：新邮箱验证通过前 email 保持
			// 旧值（旧邮箱仍可登录/找回），仅写入 pending_email + 签发 email_change
			// token + 向新邮箱发验证邮件；验证通过（ConfirmEmailChange）才切换。
			if cmd.URL == "" {
				return nil, status.Error(codes.InvalidArgument, "url is required when changing email")
			}
			if err := validateRedirectURL(cmd.URL); err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "invalid url: %v", err)
			}
			if err := a.validateProjectOAuthRedirectURLs(ctx, p.ProjectID, cmd.URL, cmd.URL); err != nil {
				return nil, err
			}
			taken, err := a.usersRepo.GetByEmail(ctx, p.ProjectID, newEmail)
			if err != nil {
				return nil, err
			}
			if taken != nil && taken.ID != p.UserID {
				return nil, status.Error(codes.AlreadyExists, users.ErrEmailAlreadyRegistered.Error())
			}
			updates["pending_email"] = newEmail
			emailChanging = true
			stagedEmail = newEmail
		default: // 设置了与当前相同的 email：幂等无操作。
		}
	}
	if cmd.Password != "" {
		if err := validatePasswordStrength(cmd.Password); err != nil {
			return nil, err
		}
		newHash, err := password.Hash(cmd.Password)
		if err != nil {
			return nil, err
		}
		updates["password_hash"] = newHash
	}
	// 敏感变更（改邮箱/清空邮箱/改密码）二次验证：非匿名用户（已有密码）必须提供旧密码；
	// 匿名用户（password_hash 为空）升级为实名/设置密码时跳过。
	if (emailChanging || clearingEmail || updates["password_hash"] != nil) && hash != "" {
		if cmd.OldPassword == "" {
			return nil, status.Error(codes.InvalidArgument, "old_password is required")
		}
		if ok, _ := password.Verify(cmd.OldPassword, hash); !ok {
			return nil, status.Error(codes.Unauthenticated, "invalid old password")
		}
	}
	if len(updates) == 0 {
		return accountUser(found), nil
	}

	// 先撤会话、后提交：撤会话失败即返回，无"密码已改但旧会话仍存活"窗口。
	// 邮箱变更走 staging：pending 阶段不撤会话（旧邮箱仍可登录），撤会话
	// 时机推迟到 ConfirmEmailChange 成功时（G3-3 语义在确认路径保持）。
	if _, passwordChanged := updates["password_hash"]; passwordChanged {
		if err := a.sessions.DeleteSessionsByUser(ctx, p.ProjectID, p.UserID); err != nil {
			return nil, fmt.Errorf("delete sessions after account change: %w", err)
		}
	}

	// R05-P1-2（A 档 staging）：先签发 email_change token 并向**新邮箱**发送
	// 验证邮件（失败则变更不落库，无副作用），提交后才向**旧邮箱**发安全通知
	//（B 档成果保留），让被劫持者第一时间察觉并止损。
	if emailChanging {
		if a.tokens == nil || a.mailer == nil {
			return nil, status.Error(codes.Unimplemented, "email delivery is not configured")
		}
		// Round3 H6-3：与 verification/recovery/magic 对齐，签发前走发送频控
		//（60s cooldown + IP 窗口），防改邮箱邮件轰炸。
		clientInfo := contexts.ClientInfoFrom(ctx)
		if err := a.tokens.CheckSendRateLimit(ctx, p.ProjectID, stagedEmail, clientInfo.IP); err != nil {
			return nil, err
		}
		secret, expireAt, err := a.tokens.CreateEmailChangeToken(ctx, p.ProjectID, p.UserID, stagedEmail)
		if err != nil {
			return nil, err
		}
		link := buildAccountActionURL(cmd.URL, p.UserID, secret)
		subject := "Confirm your Torchwood email change"
		body := fmt.Sprintf("Click the link below to confirm your new email address:\n\n%s\n\nThis link expires at %s.", link, expireAt.Format("2006-01-02 15:04 MST"))
		if err := a.mailer.Send(ctx, stagedEmail, subject, body); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to send email change confirmation: %v", err)
		}
	}

	if err := a.usersRepo.Update(ctx, p.ProjectID, p.UserID, updates); err != nil {
		if mapped := appshared.MapUserError(err); mapped != err {
			return nil, mapped
		}
		return nil, fmt.Errorf("update account: %w", err)
	}
	updated, err := a.requireAccountUser(ctx, p.ProjectID, p.UserID)
	if err != nil {
		return nil, err
	}
	if emailChanging && oldEmail != "" && a.mailer != nil {
		subject := "Your Torchwood email address is being changed"
		body := fmt.Sprintf("Your Torchwood account email is pending change to %s. The change takes effect only after confirmation from the new address.\n\nIf you did not make this change, sign in to your account and update your email or contact support immediately.", stagedEmail)
		if err := a.mailer.Send(ctx, oldEmail, subject, body); err != nil {
			slog.Warn("email change notification failed", "user_id", p.UserID, "error", err)
		}
	}
	return updated, nil
}

// ConfirmEmailChange 消费 email_change 一次性 token（GETDEL 原子）并校验新
// 邮箱未被他人占用，通过后切换 email、清除 pending_email、置 email_verified，
// 并先撤全部会话再提交（G3-3 语义）。免登录（ACCESS_PUBLIC）：点邮件链接即完成，
// 与 recovery 同一安全模型（随机 secret + TTL + 一次性消费）。
func (a *Account) ConfirmEmailChange(ctx context.Context, cmd ConfirmEmailChangeCommand) (*User, error) {
	if a.tokens == nil {
		return nil, status.Error(codes.Unimplemented, "account verification is not configured")
	}
	projectID := strings.TrimSpace(cmd.ProjectID)
	userID := strings.TrimSpace(cmd.UserID)
	secret := strings.TrimSpace(cmd.Secret)
	if projectID == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	if userID == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	if secret == "" {
		return nil, status.Error(codes.InvalidArgument, "secret is required")
	}
	if err := a.requireProject(ctx, projectID); err != nil {
		return nil, err
	}
	newEmail, err := a.tokens.VerifyEmailChangeToken(ctx, projectID, userID, secret)
	if err != nil {
		return nil, err
	}
	// 新邮箱在 token 有效期内可能已被他人注册（并发创建）：GetByEmail
	// 查重，被占用则 AlreadyExists（token 已被原子消费，不可重试）。
	taken, err := a.usersRepo.GetByEmail(ctx, projectID, newEmail)
	if err != nil {
		return nil, err
	}
	if taken != nil && taken.ID != userID {
		return nil, status.Error(codes.AlreadyExists, users.ErrEmailAlreadyRegistered.Error())
	}
	if _, err := a.requireAccountUser(ctx, projectID, userID); err != nil {
		return nil, err
	}
	if err := a.sessions.DeleteSessionsByUser(ctx, projectID, userID); err != nil {
		return nil, fmt.Errorf("delete sessions after email change: %w", err)
	}
	if err := a.usersRepo.Update(ctx, projectID, userID, map[string]any{
		"email":          newEmail,
		"pending_email":  "",
		"email_verified": true,
	}); err != nil {
		if mapped := appshared.MapUserError(err); mapped != err {
			return nil, mapped
		}
		return nil, fmt.Errorf("confirm email change: %w", err)
	}
	return a.requireAccountUser(ctx, projectID, userID)
}

// ListSessions 循环分页拉取全部会话（PageSize=1000，直至 NextPageToken 空），
// 避免 ListDocuments 默认 50 条截断。
func (a *Account) ListSessions(ctx context.Context) ([]Session, error) {
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	list, err := a.sessionRepo.ListByUser(ctx, p.ProjectID, p.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(list))
	for i := range list {
		s := mapDomainSession(&list[i])
		s.Current = s.ID == p.SessionID
		out = append(out, s)
	}
	return out, nil
}

func (a *Account) DeleteSession(ctx context.Context, sessionID string) error {
	p, err := a.requireUser(ctx)
	if err != nil {
		return err
	}
	if sessionID == "" {
		return status.Error(codes.InvalidArgument, "session_id is required")
	}
	if err := a.deleteUserSession(ctx, p, sessionID); err != nil {
		return err
	}
	return nil
}

func (a *Account) DeleteSessions(ctx context.Context, keepCurrent bool) error {
	p, err := a.requireUser(ctx)
	if err != nil {
		return err
	}
	sessions, err := a.ListSessions(ctx)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if keepCurrent && s.ID == p.SessionID {
			continue
		}
		if err := a.deleteUserSession(ctx, p, s.ID); err != nil {
			return err
		}
	}
	return nil
}

func (a *Account) GetPrefs(ctx context.Context) (map[string]any, error) {
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	found, err := a.usersRepo.GetByID(ctx, p.ProjectID, p.UserID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	if found.Prefs == nil {
		return map[string]any{}, nil
	}
	return found.Prefs, nil
}

func (a *Account) UpdatePrefs(ctx context.Context, prefs map[string]any) (map[string]any, error) {
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if prefs == nil {
		return nil, status.Error(codes.InvalidArgument, "prefs is required")
	}
	if err := validatePrefs(prefs); err != nil {
		return nil, err
	}
	if err := a.usersRepo.Update(ctx, p.ProjectID, p.UserID, map[string]any{"prefs": prefs}); err != nil {
		return nil, fmt.Errorf("update prefs: %w", err)
	}
	found, err := a.usersRepo.GetByID(ctx, p.ProjectID, p.UserID)
	if err != nil {
		return nil, err
	}
	if found == nil || found.Prefs == nil {
		return map[string]any{}, nil
	}
	return found.Prefs, nil
}

// prefs 大小与嵌套深度上限。
const (
	maxPrefsBytes = 64 * 1024
	maxPrefsDepth = 20
)

func validatePrefs(prefs map[string]any) error {
	raw, err := json.Marshal(prefs)
	if err != nil {
		return status.Error(codes.InvalidArgument, "invalid prefs")
	}
	if len(raw) > maxPrefsBytes {
		return status.Error(codes.InvalidArgument, "prefs exceed size limit")
	}
	if prefsDepth(prefs, 0) > maxPrefsDepth {
		return status.Error(codes.InvalidArgument, "prefs nesting is too deep")
	}
	return nil
}

func prefsDepth(v any, depth int) int {
	switch t := v.(type) {
	case map[string]any:
		depth++
		for _, child := range t {
			if d := prefsDepth(child, depth); d > depth {
				depth = d
			}
		}
	case []any:
		depth++
		for _, child := range t {
			if d := prefsDepth(child, depth); d > depth {
				depth = d
			}
		}
	}
	return depth
}

func (a *Account) deleteUserSession(ctx context.Context, p *shared.Principal, sessionID string) error {
	sess, err := a.sessionRepo.GetByID(ctx, p.ProjectID, sessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return status.Error(codes.NotFound, "session not found")
	}
	if sess.UserID != p.UserID {
		return status.Error(codes.PermissionDenied, "cannot delete another user's session")
	}
	return a.sessionRepo.Delete(ctx, p.ProjectID, sessionID)
}

func (a *Account) requireAccountUser(ctx context.Context, projectID, userID string) (*User, error) {
	found, err := a.usersRepo.GetByID(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	return accountUser(found), nil
}

func (a *Account) requireUser(ctx context.Context) (*shared.Principal, error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p == nil || p.ActorKind != shared.ActorKindEndUser || p.UserID == "" {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	return p, nil
}

func (a *Account) ensureUserCanAuthenticate(ctx context.Context, projectID, userID string) error {
	found, err := a.usersRepo.GetByID(ctx, projectID, userID)
	if err != nil {
		return status.Error(codes.Unauthenticated, "user lookup failed")
	}
	if found == nil {
		return status.Error(codes.Unauthenticated, "user not found")
	}
	if !found.CanAuthenticate() {
		return status.Error(codes.Unauthenticated, "user account is not active")
	}
	return nil
}

func accountUser(u *users.User) *User {
	if u == nil {
		return nil
	}
	return &User{
		ID:            u.ID,
		Email:         u.Email,
		Name:          u.Name,
		Status:        u.Status,
		EmailVerified: u.EmailVerified,
		CreatedAt:     u.CreatedAt,
		UpdatedAt:     u.UpdatedAt,
	}
}

func mapDomainSession(s *domainauth.Session) Session {
	if s == nil {
		return Session{}
	}
	return Session{
		ID:        s.ID,
		UserID:    s.UserID,
		Provider:  s.Provider,
		UserAgent: s.UserAgent,
		IP:        s.IP,
		ExpireAt:  s.ExpireAt,
		CreatedAt: s.CreatedAt,
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

//nolint:unused
func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

// SecureCookies 报告端用户会话 cookie 是否需 Secure 标志（与 console 保持一致：public_url 为 https 时才标记 Secure）。
func (a *Account) SecureCookies() bool {
	if a == nil || a.cfg == nil || a.cfg.GetServer() == nil || a.cfg.GetServer().GetHttp() == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(a.cfg.GetServer().GetHttp().GetPublicUrl()), "https://")
}
