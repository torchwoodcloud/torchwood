package client

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type CreateVerificationCommand struct {
	ProjectID string
	URL       string
}

type VerificationChallenge struct {
	UserID   string
	ExpireAt int64
}

type UpdateVerificationCommand struct {
	ProjectID string
	UserID    string
	Secret    string
}

func (a *Account) CreateVerification(ctx context.Context, cmd CreateVerificationCommand) (*VerificationChallenge, error) {
	if a.tokens == nil {
		return nil, status.Error(codes.Unimplemented, "account verification is not configured")
	}
	if a.mailer == nil {
		return nil, status.Error(codes.Unimplemented, "email delivery is not configured")
	}
	p, err := a.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	projectID := strings.TrimSpace(cmd.ProjectID)
	if projectID == "" {
		projectID = p.ProjectID
	}
	if projectID != p.ProjectID || p.UserID == "" {
		return nil, status.Error(codes.PermissionDenied, "cannot create verification for another user")
	}
	if err := validateRedirectURL(cmd.URL); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid url: %v", err)
	}
	if err := a.validateProjectOAuthRedirectURLs(ctx, projectID, cmd.URL, cmd.URL); err != nil {
		return nil, err
	}
	if err := a.requireProject(ctx, projectID); err != nil {
		return nil, err
	}

	user, err := a.requireAccountUser(ctx, projectID, p.UserID)
	if err != nil {
		return nil, err
	}
	email := normalizeEmail(user.Email)
	if email == "" || strings.HasSuffix(email, "@torchwood.local") {
		return nil, status.Error(codes.FailedPrecondition, "user email cannot be verified")
	}
	if user.EmailVerified {
		return nil, status.Error(codes.AlreadyExists, "email is already verified")
	}

	clientInfo := contexts.ClientInfoFrom(ctx)
	if err := a.tokens.CheckSendRateLimit(ctx, projectID, email, clientInfo.IP); err != nil {
		return nil, err
	}

	secret, expireAt, err := a.tokens.CreateVerificationToken(ctx, projectID, p.UserID, email)
	if err != nil {
		return nil, err
	}
	link := buildAccountActionURL(cmd.URL, p.UserID, secret)
	subject := "Verify your Torchwood email"
	body := fmt.Sprintf("Click the link below to verify your email address:\n\n%s\n\nThis link expires in 24 hours.", link)
	if err := a.mailer.Send(ctx, email, subject, body); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to send verification email: %v", err)
	}
	return &VerificationChallenge{UserID: p.UserID, ExpireAt: expireAt.Unix()}, nil
}

func (a *Account) UpdateVerification(ctx context.Context, cmd UpdateVerificationCommand) (*User, error) {
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
	if err := a.tokens.VerifyVerificationToken(ctx, projectID, userID, secret); err != nil {
		return nil, err
	}

	if _, err := a.requireAccountUser(ctx, projectID, userID); err != nil {
		return nil, err
	}
	if err := a.usersRepo.Update(ctx, projectID, userID, map[string]any{"email_verified": true}); err != nil {
		return nil, fmt.Errorf("verify email: %w", err)
	}
	return a.requireAccountUser(ctx, projectID, userID)
}

func buildAccountActionURL(rawURL, userID, secret string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set("userId", userID)
	q.Set("secret", secret)
	u.RawQuery = q.Encode()
	return u.String()
}
