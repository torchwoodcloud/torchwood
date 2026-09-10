package client

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MFASignInChallenge 是登录钩子返回的二次验证挑战；nil 表示无需挑战。
type MFASignInChallenge struct {
	Token   string
	Factors []domainauth.Factor
}

// mfaFactorActivateWindow 是 pending 因子必须激活的时限。
const mfaFactorActivateWindow = 10 * time.Minute

// CompleteMFASession 频控：按账号与 IP 双维度限流，缓解 TOTP 爆破。
const (
	mfaCompleteUserWindow = 15 * time.Minute
	mfaCompleteUserLimit  = 10
	mfaCompleteIPWindow   = 15 * time.Minute
	mfaCompleteIPLimit    = 30
)

// mfaSignInChallenge 读取用户 verified 因子；存在时创建一次性挑战并返回，
// 不存在返回 nil，调用方按原逻辑签发会话。mfaChallenges 不再判 nil
// （Round4 J5-5）：构造期已显式落 Noop（仅供测试），旁路风险消除；此处
// 保留的 a.mfa 判定是 MFA 能力开关（未装配 = 无需挑战，语义同
// CompleteMFASession 的 Unimplemented）。
func (a *Account) mfaSignInChallenge(ctx context.Context, projectID string, user *User) (*MFASignInChallenge, error) {
	if a.mfa == nil || user == nil {
		return nil, nil
	}
	found, err := a.usersRepo.GetByID(ctx, projectID, user.ID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, status.Error(codes.Unauthenticated, "user not found")
	}
	factors := parseFactorsRaw(found.Factors)
	verified := make([]domainauth.Factor, 0, 1)
	for _, f := range factors {
		if f.Status == domainauth.FactorStatusVerified && f.Type == domainauth.FactorTypeTOTP {
			verified = append(verified, f)
		}
	}
	if len(verified) == 0 {
		return nil, nil
	}
	token, _, err := a.mfaChallenges.Create(ctx, projectID, user.ID)
	if err != nil {
		return nil, err
	}
	return &MFASignInChallenge{Token: token, Factors: verified}, nil
}

func (a *Account) ListFactors(ctx context.Context) ([]domainauth.Factor, error) {
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	factors, err := a.loadFactors(ctx, p)
	if err != nil {
		return nil, err
	}
	// 读路径不回显明文 secret（ListFactors 仅返回元数据）。
	for i := range factors {
		factors[i].Secret = ""
	}
	return factors, nil
}

func (a *Account) CreateTOTPFactor(ctx context.Context, projectID, userID, email string) (*domainauth.Factor, string, string, error) {
	if a.mfa == nil {
		return nil, "", "", status.Error(codes.Unimplemented, "mfa is not configured")
	}
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, "", "", err
	}
	if projectID == "" {
		projectID = p.ProjectID
	}
	if projectID != p.ProjectID || userID != p.UserID {
		return nil, "", "", status.Error(codes.PermissionDenied, "cannot create mfa factor for another user")
	}

	found, err := a.usersRepo.GetByID(ctx, p.ProjectID, p.UserID)
	if err != nil {
		return nil, "", "", err
	}
	if found == nil {
		return nil, "", "", status.Error(codes.NotFound, "user not found")
	}
	if email == "" {
		email = normalizeEmail(found.Email)
	}
	if email == "" {
		return nil, "", "", status.Error(codes.FailedPrecondition, "user email cannot be used for mfa")
	}

	project, err := a.projectRepo.GetProject(ctx, p.ProjectID)
	if err != nil {
		return nil, "", "", err
	}
	if project == nil {
		return nil, "", "", status.Error(codes.NotFound, "project not found")
	}
	issuer := project.Name
	if issuer == "" {
		issuer = p.ProjectID
	}

	factor, plainSecret, otpauthURL, err := a.mfa.CreateTOTPFactor(ctx, issuer, p.UserID, email)
	if err != nil {
		return nil, "", "", err
	}
	err = a.mutateFactors(ctx, p, func(factors []domainauth.Factor) ([]domainauth.Factor, error) {
		now := time.Now()
		kept := make([]domainauth.Factor, 0, len(factors)+1)
		for _, f := range factors {
			if f.Type == domainauth.FactorTypeTOTP && f.Status == domainauth.FactorStatusPending &&
				!f.CreatedAt.IsZero() && now.Sub(f.CreatedAt) > mfaFactorActivateWindow {
				continue
			}
			kept = append(kept, f)
		}
		return append(kept, *factor), nil
	})
	if err != nil {
		return nil, "", "", err
	}
	return factor, plainSecret, otpauthURL, nil
}

func (a *Account) VerifyTOTPFactor(ctx context.Context, projectID, userID, factorID, code string) (*domainauth.Factor, error) {
	if a.mfa == nil {
		return nil, status.Error(codes.Unimplemented, "mfa is not configured")
	}
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if projectID == "" {
		projectID = p.ProjectID
	}
	if projectID != p.ProjectID || userID != p.UserID {
		return nil, status.Error(codes.PermissionDenied, "cannot verify mfa factor for another user")
	}
	if factorID == "" {
		return nil, status.Error(codes.InvalidArgument, "factor_id is required")
	}
	if code == "" {
		return nil, status.Error(codes.InvalidArgument, "code is required")
	}

	expired := false
	var verified *domainauth.Factor
	err = a.mutateFactors(ctx, p, func(factors []domainauth.Factor) ([]domainauth.Factor, error) {
		idx := -1
		for i := range factors {
			if factors[i].ID == factorID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, status.Error(codes.NotFound, "mfa factor not found")
		}
		factor := factors[idx]
		if factor.Status != domainauth.FactorStatusPending {
			return nil, status.Error(codes.InvalidArgument, "mfa factor is already verified")
		}
		if !factor.CreatedAt.IsZero() && time.Since(factor.CreatedAt) > mfaFactorActivateWindow {
			expired = true
			return append(factors[:idx], factors[idx+1:]...), nil
		}
		if err := a.mfa.VerifyTOTPFactor(ctx, &factor, code); err != nil {
			return nil, err
		}
		factor.Status = domainauth.FactorStatusVerified
		factors[idx] = factor
		cp := factor
		cp.Secret = ""
		verified = &cp
		return factors, nil
	})
	if err != nil {
		return nil, err
	}
	if expired {
		return nil, status.Error(codes.Unauthenticated, "mfa factor activation expired")
	}
	return verified, nil
}

// DeleteFactor 删除 MFA 因子：pending 因子直接删除；verified 因子必须提供
// 有效 TOTP code 二次验证（防会话劫持者静默移除 MFA）；删除时作废该用户
// 全部未消费的登录挑战。
func (a *Account) DeleteFactor(ctx context.Context, projectID, userID, factorID, code string) error {
	p, err := a.requireUser(ctx)
	if err != nil {
		return err
	}
	if a.mfa == nil {
		return status.Error(codes.Unimplemented, "mfa is not configured")
	}
	if projectID == "" {
		projectID = p.ProjectID
	}
	if projectID != p.ProjectID || userID != p.UserID {
		return status.Error(codes.PermissionDenied, "cannot delete mfa factor for another user")
	}
	if factorID == "" {
		return status.Error(codes.InvalidArgument, "factor_id is required")
	}

	err = a.mutateFactors(ctx, p, func(factors []domainauth.Factor) ([]domainauth.Factor, error) {
		idx := -1
		for i := range factors {
			if factors[i].ID == factorID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, status.Error(codes.NotFound, "mfa factor not found")
		}
		if factors[idx].Status == domainauth.FactorStatusVerified {
			if code == "" {
				return nil, status.Error(codes.InvalidArgument, "mfa code is required")
			}
			if err := a.mfa.ValidateTOTP(ctx, &factors[idx], code); err != nil {
				return nil, err
			}
		}
		return append(factors[:idx], factors[idx+1:]...), nil
	})
	if err != nil {
		return err
	}
	// 作废该用户未消费的登录挑战，防止删除因子后挑战仍可完成登录。
	if a.mfaChallenges != nil {
		if err := a.mfaChallenges.RevokeByUser(ctx, p.ProjectID, p.UserID); err != nil {
			return err
		}
	}
	return nil
}

// CompleteMFASession 消费一次性挑战 token、校验 TOTP code 并签发会话。
func (a *Account) CompleteMFASession(ctx context.Context, projectID, challengeToken, factorID, code string) (*User, *TokenBundle, string, error) {
	if a.mfaChallenges == nil || a.mfa == nil {
		return nil, nil, "", status.Error(codes.Unimplemented, "mfa is not configured")
	}
	challengeProjectID, userID, err := a.mfaChallenges.Consume(ctx, challengeToken)
	if err != nil {
		return nil, nil, "", err
	}
	clientInfo := contexts.ClientInfoFrom(ctx)
	if err := a.checkMFACompleteRateLimit(ctx, userID, clientInfo.IP); err != nil {
		return nil, nil, "", err
	}
	if projectID == "" {
		projectID = challengeProjectID
	}
	if projectID != challengeProjectID {
		return nil, nil, "", status.Error(codes.Unauthenticated, "invalid or expired challenge")
	}
	if factorID == "" {
		return nil, nil, "", status.Error(codes.InvalidArgument, "factor_id is required")
	}
	if code == "" {
		return nil, nil, "", status.Error(codes.InvalidArgument, "code is required")
	}

	found, err := a.usersRepo.GetByID(ctx, projectID, userID)
	if err != nil {
		return nil, nil, "", err
	}
	if found == nil {
		return nil, nil, "", status.Error(codes.Unauthenticated, "user not found")
	}
	factors := parseFactorsRaw(found.Factors)
	var factor *domainauth.Factor
	for i := range factors {
		if factors[i].ID == factorID && factors[i].Status == domainauth.FactorStatusVerified {
			factor = &factors[i]
			break
		}
	}
	if factor == nil {
		return nil, nil, "", status.Error(codes.Unauthenticated, "mfa factor is not verified")
	}
	if err := a.mfa.ValidateTOTP(ctx, factor, code); err != nil {
		return nil, nil, "", err
	}

	user := accountUser(found)
	if !found.CanAuthenticate() {
		return nil, nil, "", status.Error(codes.Unauthenticated, "user account is not active")
	}
	tokens, cookie, err := a.sessions.CreateSessionAndTokens(ctx, projectID, userID, user.Email, domainauth.ProviderMFA)
	if err != nil {
		return nil, nil, "", err
	}
	return user, tokens, cookie, nil
}

// loadFactors 读取 users.factors JSON 数组。
func (a *Account) loadFactors(ctx context.Context, p *shared.Principal) ([]domainauth.Factor, error) {
	found, err := a.usersRepo.GetByID(ctx, p.ProjectID, p.UserID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	return parseFactorsRaw(found.Factors), nil
}

// mutateFactors 在 UpdateFactors 的 FOR UPDATE 快照上改 factors，禁止写回锁前副本。
func (a *Account) mutateFactors(ctx context.Context, p *shared.Principal, fn func([]domainauth.Factor) ([]domainauth.Factor, error)) error {
	err := a.usersRepo.UpdateFactors(ctx, p.ProjectID, p.UserID, func(current json.RawMessage) (json.RawMessage, error) {
		next, err := fn(parseFactorsRaw(current))
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(factorDocs(next))
		if err != nil {
			return nil, fmt.Errorf("update mfa factors: %w", err)
		}
		return raw, nil
	})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
			return err
		}
		return fmt.Errorf("update mfa factors: %w", err)
	}
	return nil
}

func parseFactorsRaw(raw json.RawMessage) []domainauth.Factor {
	if len(raw) == 0 {
		return nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err == nil {
		out := make([]domainauth.Factor, 0, len(items))
		for _, m := range items {
			if f, ok := factorFromMap(m); ok {
				out = append(out, f)
			}
		}
		return out
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return parseFactors(decoded)
}

func factorFromMap(m map[string]any) (domainauth.Factor, bool) {
	f := domainauth.Factor{
		ID:     stringValue(m["id"]),
		Type:   stringValue(m["type"]),
		Secret: stringValue(m["secret"]),
		Status: stringValue(m["status"]),
	}
	if createdAt, ok := m["created_at"].(string); ok {
		if t, err := parseSessionTime(createdAt); err == nil {
			f.CreatedAt = t
		}
	} else if createdAtTime, ok := m["created_at"].(time.Time); ok {
		f.CreatedAt = createdAtTime
	}
	if f.ID == "" || f.Type == "" {
		return domainauth.Factor{}, false
	}
	return f, true
}

// parseSessionTime 本地拷贝自 infra/auth.ParseSessionTime，避免 app 层
// 直接依赖 infra 适配器（Round4 J4-3）。
func parseSessionTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case string:
		return time.Parse(time.RFC3339Nano, t)
	}
	return time.Time{}, fmt.Errorf("unsupported time type")
}

func parseFactors(raw any) []domainauth.Factor {
	var out []domainauth.Factor
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return out
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if f, ok := factorFromMap(m); ok {
			out = append(out, f)
		}
	}
	return out
}

func factorDocs(factors []domainauth.Factor) []any {
	out := make([]any, 0, len(factors))
	for _, f := range factors {
		out = append(out, map[string]any{
			"id":         f.ID,
			"type":       f.Type,
			"secret":     f.Secret,
			"status":     f.Status,
			"created_at": f.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return out
}

func (a *Account) checkMFACompleteRateLimit(ctx context.Context, userID, ip string) error {
	// nil 容忍：未装配限流器或拿不到维度值时不做限制。
	if a.rateLimiter == nil {
		return nil
	}
	if userID != "" {
		if err := a.rateLimiter.Allow(ctx, "mfa:complete:user:"+userID, mfaCompleteUserLimit, mfaCompleteUserWindow); err != nil {
			return err
		}
	}
	if ip != "" {
		if err := a.rateLimiter.Allow(ctx, "mfa:complete:ip:"+ip, mfaCompleteIPLimit, mfaCompleteIPWindow); err != nil {
			return err
		}
	}
	return nil
}
