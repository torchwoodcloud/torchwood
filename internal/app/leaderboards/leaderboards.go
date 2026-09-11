// Package leaderboards 是排行榜 use-case 聚合（dogfooding 提案 2026-09-11，
// Phase 1）：submit / me / getEntry / top 读路径 + board 配置 CRUD（console）+
// retention 清理。Phase 2（声明式结榜发奖）另期落地。
package leaderboards

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/uow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultTopPageSize = 50
	maxTopPageSize     = 100
	maxTopOffset       = 10000
)

// Leaderboards 是排行榜子域 use-case 聚合。
type Leaderboards struct {
	db       uow.Runner
	boards   domainleaderboards.BoardRepo
	entries  domainleaderboards.EntryRepo
	idem     databases.IdempotencyStore
	projects projects.Repository
	logger   *slog.Logger
	now      func() time.Time
}

// NewLeaderboards 构造 use-case 聚合（db 注入 uow.Runner 端口）。
func NewLeaderboards(
	db uow.Runner,
	boards domainleaderboards.BoardRepo,
	entries domainleaderboards.EntryRepo,
	idem databases.IdempotencyStore,
	logger *slog.Logger,
	projectRepo projects.Repository,
) *Leaderboards {
	if logger == nil {
		logger = slog.Default()
	}
	return &Leaderboards{
		db:       db,
		boards:   boards,
		entries:  entries,
		idem:     idem,
		projects: projectRepo,
		logger:   logger,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (a *Leaderboards) ts() time.Time { return a.now() }

func projectScope(ctx context.Context) (string, error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p.ProjectID == "" {
		return "", status.Error(codes.Unauthenticated, "missing project context")
	}
	return p.ProjectID, nil
}

func endUser(ctx context.Context) (projectID, userID string, err error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p.ProjectID == "" || p.UserID == "" {
		return "", "", status.Error(codes.Unauthenticated, "unauthenticated")
	}
	return p.ProjectID, p.UserID, nil
}

func mapLeaderboardError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, domainleaderboards.ErrBoardNotFound),
		errors.Is(err, domainleaderboards.ErrEntryNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, domainleaderboards.ErrBoardExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, domainleaderboards.ErrImmutableField):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, domainleaderboards.ErrSubmitLimitExceeded):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, domainleaderboards.ErrClientSubmitDisabled):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, domainleaderboards.ErrInvalidConfig),
		errors.Is(err, domainleaderboards.ErrSubjectRequired),
		errors.Is(err, domainleaderboards.ErrSubjectTooLong),
		errors.Is(err, domainleaderboards.ErrValueOutOfRange),
		errors.Is(err, domainleaderboards.ErrValueTooLarge),
		errors.Is(err, domainleaderboards.ErrPeriodInvalid),
		errors.Is(err, domainleaderboards.ErrPeriodNotSubmittable),
		errors.Is(err, domainleaderboards.ErrTiebreakMismatch),
		errors.Is(err, domainleaderboards.ErrPeriodRequired):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return err
	}
}

func (a *Leaderboards) loadBoard(ctx context.Context, projectID, boardID string) (*domainleaderboards.Board, error) {
	b, err := a.boards.Get(ctx, projectID, boardID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, domainleaderboards.ErrBoardNotFound
	}
	return b, nil
}
