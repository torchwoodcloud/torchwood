package client

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type CreateEmailOTPCommand struct {
	ProjectID string
	Email     string
}

type Challenge struct {
	ChallengeID string
	ExpireAt    time.Time
}

type CreateEmailOTPSessionCommand struct {
	ProjectID   string
	Email       string
	ChallengeID string
	OTP         string
}

func (a *Account) CreateEmailOTP(ctx context.Context, cmd CreateEmailOTPCommand) (*Challenge, error) {
	if a.otp == nil {
		return nil, status.Error(codes.Unimplemented, "email otp is not configured")
	}
	if a.mailer == nil {
		return nil, status.Error(codes.Unimplemented, "email delivery is not configured")
	}
	projectID := strings.TrimSpace(cmd.ProjectID)
	email := normalizeEmail(cmd.Email)
	if projectID == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	if email == "" {
		return nil, status.Error(codes.InvalidArgument, "email is required")
	}
	if err := validateEmail(email); err != nil {
		return nil, err
	}

	project, err := a.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, status.Error(codes.NotFound, "project not found")
	}

	clientInfo := contexts.ClientInfoFrom(ctx)
	if err := a.otp.CheckSendRateLimit(ctx, projectID, email, clientInfo.IP); err != nil {
		return nil, err
	}

	code, err := a.generateOTP(6)
	if err != nil {
		return nil, status.Error(codes.Internal, "otp generation failed")
	}
	challengeID, expireAt, err := a.otp.CreateEmailChallenge(ctx, projectID, email, code)
	if err != nil {
		return nil, err
	}

	subject := "Your Torchwood sign-in code"
	body := fmt.Sprintf("Your one-time sign-in code is: %s\n\nThis code expires in 5 minutes.", code)
	if err := a.mailer.Send(ctx, email, subject, body); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to send otp email: %v", err)
	}

	return &Challenge{ChallengeID: challengeID, ExpireAt: expireAt}, nil
}

func (a *Account) CreateEmailOTPSession(ctx context.Context, cmd CreateEmailOTPSessionCommand) (*User, *TokenBundle, string, *MFASignInChallenge, error) {
	if a.otp == nil {
		return nil, nil, "", nil, status.Error(codes.Unimplemented, "email otp is not configured")
	}
	projectID := strings.TrimSpace(cmd.ProjectID)
	email := normalizeEmail(cmd.Email)
	challengeID := strings.TrimSpace(cmd.ChallengeID)
	otp := strings.TrimSpace(cmd.OTP)
	if projectID == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	if email == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "email is required")
	}
	if challengeID == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "challenge_id is required")
	}
	if otp == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "otp is required")
	}

	project, err := a.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if project == nil {
		return nil, nil, "", nil, status.Error(codes.NotFound, "project not found")
	}

	if err := a.otp.VerifyEmailChallenge(ctx, projectID, challengeID, email, otp); err != nil {
		return nil, nil, "", nil, err
	}

	user, err := a.findOrCreateUserByEmail(ctx, projectID, email, true)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if !users.CanAuthenticate(user.Status) {
		return nil, nil, "", nil, status.Error(codes.Unauthenticated, "user account is not active")
	}
	return a.finishSignInWithProvider(ctx, projectID, user, domainauth.ProviderEmailOTP)
}

func (a *Account) findOrCreateUserByEmail(ctx context.Context, projectID, email string, markVerified bool) (*User, error) {
	existing, err := a.usersRepo.GetByEmail(ctx, projectID, email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return accountUser(existing), nil
	}

	userID, err := a.generateUserID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	registered, err := users.Register(users.RegisterInput{
		ID:            userID,
		Email:         email,
		Name:          emailLocalPart(email),
		EmailVerified: markVerified,
	})
	if err != nil {
		return nil, appshared.MapUserError(err)
	}
	if err := a.usersRepo.Insert(ctx, projectID, registered); err != nil {
		if errors.Is(err, users.ErrEmailAlreadyRegistered) {
			existing, listErr := a.usersRepo.GetByEmail(ctx, projectID, email)
			if listErr != nil {
				return nil, listErr
			}
			if existing != nil {
				return accountUser(existing), nil
			}
		}
		return nil, fmt.Errorf("insert user: %w", err)
	}
	return accountUser(registered), nil
}

func (a *Account) finishSignInWithProvider(ctx context.Context, projectID string, user *User, provider string) (*User, *TokenBundle, string, *MFASignInChallenge, error) {
	// MFA 登录钩子：用户存在 verified 因子时不直接签发会话，返回挑战信息。
	challenge, err := a.mfaSignInChallenge(ctx, projectID, user)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if challenge != nil {
		return user, nil, "", challenge, nil
	}
	tokens, cookie, err := a.sessions.CreateSessionAndTokens(ctx, projectID, user.ID, user.Email, provider)
	if err != nil {
		return nil, nil, "", nil, err
	}
	return user, tokens, cookie, nil, nil
}

func normalizeEmail(email string) string {
	return users.NormalizeEmail(email)
}

// validateEmail 校验邮箱格式与长度（net/mail.ParseAddress + ≤254）。
func validateEmail(email string) error {
	if len(email) > 254 {
		return status.Error(codes.InvalidArgument, "email is too long")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return status.Error(codes.InvalidArgument, "invalid email format")
	}
	return nil
}

func emailLocalPart(email string) string {
	if local, _, ok := strings.Cut(email, "@"); ok && local != "" {
		return local
	}
	return email
}
