package client

import (
	"context"
	"strings"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 匿名会话按客户端 IP 频控：默认每 IP 每小时 20 次，防止无限刷用户文档与会话。
const (
	anonymousSessionIPWindow = time.Hour
	anonymousSessionIPLimit  = 20
)

type CreateAnonymousSessionCommand struct {
	ProjectID string
}

func (a *Account) CreateAnonymousSession(ctx context.Context, cmd CreateAnonymousSessionCommand) (*User, *TokenBundle, string, *MFASignInChallenge, error) {
	projectID := strings.TrimSpace(cmd.ProjectID)
	if projectID == "" {
		return nil, nil, "", nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	clientInfo := contexts.ClientInfoFrom(ctx)
	if err := a.checkAnonymousSessionRateLimit(ctx, clientInfo.IP); err != nil {
		return nil, nil, "", nil, err
	}
	if err := a.requireProject(ctx, projectID); err != nil {
		return nil, nil, "", nil, err
	}

	userID, err := a.generateUserID(ctx, projectID)
	if err != nil {
		return nil, nil, "", nil, err
	}
	registered, err := users.Register(users.RegisterInput{
		ID:        userID,
		Email:     users.AnonymousEmail(userID),
		Name:      "Anonymous",
		Anonymous: true,
	})
	if err != nil {
		return nil, nil, "", nil, appshared.MapUserError(err)
	}
	if err := a.usersRepo.Insert(ctx, projectID, registered); err != nil {
		return nil, nil, "", nil, err
	}
	return a.finishSignInWithProvider(ctx, projectID, accountUser(registered), domainauth.ProviderAnonymous)
}

func (a *Account) checkAnonymousSessionRateLimit(ctx context.Context, ip string) error {
	// M5 C8（fail-closed）：匿名会话是无限刷用户文档/会话的入口，限流是
	// 唯一闸门——限流器未装配或拿不到客户端 IP 时不再静默放行，直接拒绝
	//（生产组合根恒注入 infra/auth 的 Redis 限流器，传输层恒注 ClientInfo）。
	if a.rateLimiter == nil {
		return status.Error(codes.FailedPrecondition, "anonymous session rate limiter is not configured")
	}
	if ip == "" {
		return status.Error(codes.FailedPrecondition, "client ip is required for anonymous sessions")
	}
	return a.rateLimiter.Allow(ctx, "anonymous:ip:"+ip, anonymousSessionIPLimit, anonymousSessionIPWindow)
}
