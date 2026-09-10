package client

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var phoneDigitsOnly = regexp.MustCompile(`[^\d+]`)

type CreatePhoneOTPCommand struct {
	ProjectID string
	Phone     string
}

type CreatePhoneOTPSessionCommand struct {
	ProjectID   string
	Phone       string
	ChallengeID string
	OTP         string
}

func (a *Account) CreatePhoneOTP(ctx context.Context, cmd CreatePhoneOTPCommand) (*Challenge, error) {
	if a.otp == nil {
		return nil, status.Error(codes.Unimplemented, "phone otp is not configured")
	}
	if a.sms == nil {
		return nil, status.Error(codes.Unimplemented, "sms delivery is not configured")
	}
	projectID := strings.TrimSpace(cmd.ProjectID)
	phone, err := normalizePhone(cmd.Phone)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if projectID == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}

	project, err := a.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, status.Error(codes.NotFound, "project not found")
	}

	clientInfo := contexts.ClientInfoFrom(ctx)
	if err := a.otp.CheckSendRateLimit(ctx, projectID, phone, clientInfo.IP); err != nil {
		return nil, err
	}

	code, err := a.generateOTP(6)
	if err != nil {
		return nil, status.Error(codes.Internal, "otp generation failed")
	}
	challengeID, expireAt, err := a.otp.CreatePhoneChallenge(ctx, projectID, phone, code)
	if err != nil {
		return nil, err
	}

	body := fmt.Sprintf("Your Torchwood sign-in code is: %s. It expires in 5 minutes.", code)
	if err := a.sms.Send(ctx, phone, body); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to send otp sms: %v", err)
	}

	return &Challenge{ChallengeID: challengeID, ExpireAt: expireAt}, nil
}

func (a *Account) CreatePhoneOTPSession(ctx context.Context, cmd CreatePhoneOTPSessionCommand) (*User, *TokenBundle, string, *MFASignInChallenge, error) {
	if a.otp == nil {
		return nil, nil, "", nil, status.Error(codes.Unimplemented, "phone otp is not configured")
	}
	projectID := strings.TrimSpace(cmd.ProjectID)
	phone, err := normalizePhone(cmd.Phone)
	if err != nil {
		return nil, nil, "", nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	challengeID := strings.TrimSpace(cmd.ChallengeID)
	otp := strings.TrimSpace(cmd.OTP)
	if projectID == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "project_id is required")
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

	if err := a.otp.VerifyPhoneChallenge(ctx, projectID, challengeID, phone, otp); err != nil {
		return nil, nil, "", nil, err
	}

	user, err := a.findOrCreateUserByPhone(ctx, projectID, phone)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if !users.CanAuthenticate(user.Status) {
		return nil, nil, "", nil, status.Error(codes.Unauthenticated, "user account is not active")
	}
	return a.finishSignInWithProvider(ctx, projectID, user, domainauth.ProviderPhoneOTP)
}

func (a *Account) findOrCreateUserByPhone(ctx context.Context, projectID, phone string) (*User, error) {
	existing, err := a.usersRepo.GetByPhone(ctx, projectID, phone)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		user := accountUser(existing)
		if !existing.PhoneVerified {
			if err := a.usersRepo.Update(ctx, projectID, user.ID, map[string]any{"phone_verified": true}); err == nil {
				user.EmailVerified = existing.EmailVerified
			}
		}
		return user, nil
	}

	userID, err := a.generateUserID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	registered, err := users.Register(users.RegisterInput{
		ID:            userID,
		Email:         phonePlaceholderEmail(phone),
		Name:          phone,
		Phone:         phone,
		PhoneVerified: true,
	})
	if err != nil {
		return nil, appshared.MapUserError(err)
	}
	if err := a.usersRepo.Insert(ctx, projectID, registered); err != nil {
		if errors.Is(err, users.ErrEmailAlreadyRegistered) {
			existing, listErr := a.usersRepo.GetByPhone(ctx, projectID, phone)
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

func normalizePhone(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("phone is required")
	}
	cleaned := phoneDigitsOnly.ReplaceAllString(raw, "")
	if !strings.HasPrefix(cleaned, "+") {
		cleaned = "+" + strings.TrimPrefix(cleaned, "+")
	}
	digits := strings.TrimPrefix(cleaned, "+")
	if len(digits) < 8 || len(digits) > 15 {
		return "", fmt.Errorf("phone number must contain 8 to 15 digits")
	}
	for _, ch := range digits {
		if ch < '0' || ch > '9' {
			return "", fmt.Errorf("phone number contains invalid characters")
		}
	}
	return "+" + digits, nil
}

func phonePlaceholderEmail(phone string) string {
	safe := strings.TrimPrefix(phone, "+")
	return fmt.Sprintf("phone_%s@torchwood.local", safe)
}
