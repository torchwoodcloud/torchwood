// Package assets 是 v3 统一资产系统的 use-case 聚合（设计 §2）：
// Def CRUD、只读查询、对账、worker 到期轮转；五动词鉴权后委托领域 Service。
package assets

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/uow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultListLimit = 25
	maxListLimit     = 100
	expireBatch      = 500
)

var (
	assetOpsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_asset_ops_total",
		Help: "Total asset operations by kind, class and result.",
	}, []string{"kind", "class", "result"})
	assetNegativeBlockedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torchwood_asset_negative_quantity_blocked_total",
		Help: "Asset operations rejected to prevent negative quantity.",
	})
	assetLedgerDriftTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torchwood_asset_ledger_drift_total",
		Help: "Holdings vs ledger replay drifts found by reconcile.",
	})
)

func init() {
	prometheus.MustRegister(assetOpsTotal, assetNegativeBlockedTotal, assetLedgerDriftTotal)
}

// Assets 是资产子域 use-case 聚合。
type Assets struct {
	svc        *domainassets.Service
	defs       domainassets.DefRepo
	holdings   domainassets.HoldingRepo
	ledger     domainassets.LedgerRepo
	projects   projects.Repository
	logger     *slog.Logger
	now        func() time.Time
	scanCursor appshared.ProjectRotation // ExpireDue 轮转游标（tick 串行）
}

// NewAssets 构造 use-case 聚合（db 注入 uow.Runner 端口）。
func NewAssets(
	db uow.Runner,
	defs domainassets.DefRepo,
	holdings domainassets.HoldingRepo,
	ledger domainassets.LedgerRepo,
	events shared.EventPublisher,
	logger *slog.Logger,
	projectRepo projects.Repository,
) *Assets {
	return newAssets(db, defs, holdings, ledger, events, logger, projectRepo)
}

func newAssets(
	db uow.Runner,
	defs domainassets.DefRepo,
	holdings domainassets.HoldingRepo,
	ledger domainassets.LedgerRepo,
	events shared.EventPublisher,
	logger *slog.Logger,
	projectRepo projects.Repository,
) *Assets {
	if logger == nil {
		logger = slog.Default()
	}
	a := &Assets{
		defs:     defs,
		holdings: holdings,
		ledger:   ledger,
		projects: projectRepo,
		logger:   logger,
		now:      func() time.Time { return time.Now().UTC() },
	}
	a.svc = domainassets.NewService(db, defs, holdings, ledger, events, a.ts, newID)
	return a
}

func (a *Assets) ts() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now().UTC()
}

func newID() string { return idgen.ULID().String() }

func normalizeList(limit int, before time.Time) (int, time.Time) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	if before.IsZero() {
		before = time.Now().UTC().Add(time.Hour)
	}
	return limit, before
}

func validateCode(code string) (string, error) {
	c, err := domainassets.ValidateCode(code)
	if err != nil {
		return "", mapWriteError(err)
	}
	return c, nil
}

func mapWriteError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, domainassets.ErrInsufficient) {
		assetNegativeBlockedTotal.Inc()
	}
	switch {
	case errors.Is(err, domainassets.ErrProjectRequired):
		return status.Error(codes.Unauthenticated, "missing project context")
	case errors.Is(err, domainassets.ErrSameOwner):
		return status.Error(codes.InvalidArgument, "cannot transfer to the same owner")
	case errors.Is(err, domainassets.ErrTransferOwnersRequired):
		return status.Error(codes.InvalidArgument, "from_owner_id and to_owner_id are required")
	case errors.Is(err, domainassets.ErrHoldingIDRequired):
		return status.Error(codes.InvalidArgument, "holding_id is required")
	case errors.Is(err, domainassets.ErrOwnerRequired):
		return status.Error(codes.InvalidArgument, "owner_id is required")
	case errors.Is(err, domainassets.ErrInvalidCode):
		return status.Error(codes.InvalidArgument, "def code must match ^[a-z][a-z0-9_]{0,63}$")
	case errors.Is(err, domainassets.ErrIdempotencyTooLong):
		return status.Errorf(codes.InvalidArgument, "idempotency_key exceeds %d characters", domainassets.MaxIdempotencyKey)
	case errors.Is(err, domainassets.ErrMatrix),
		errors.Is(err, domainassets.ErrInvalidQuantity),
		errors.Is(err, domainassets.ErrExpiresAtRequired),
		errors.Is(err, domainassets.ErrIdempotencyRequired),
		errors.Is(err, domainassets.ErrInvalidOwnerType):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, domainassets.ErrInsufficient),
		errors.Is(err, domainassets.ErrMaxQuantity),
		errors.Is(err, domainassets.ErrNotTradable),
		errors.Is(err, domainassets.ErrUniquePerOwner),
		errors.Is(err, domainassets.ErrDefArchived),
		errors.Is(err, domainassets.ErrDuplicateCode):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, domainassets.ErrDefNotFound),
		errors.Is(err, domainassets.ErrHoldingNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, domainassets.ErrConcurrent):
		return status.Error(codes.Aborted, err.Error())
	default:
		return err
	}
}

func operatorFrom(ctx context.Context) json.RawMessage {
	p, ok := contexts.Principal(ctx)
	if !ok || p == nil {
		return domainassets.MarshalOperator(domainassets.OperatorSnapshot{IsSystem: true})
	}
	// 执行主体（P0 执行身份）：账本 operator 溯源到 function + 触发用户
	// （对标 PlayFab currentPlayerId 审计语义）。actor_id = function_id，
	// user_id = InvokingUserID（Server 面触发为空）；缺的字段如实留空。
	if p.ActorKind == shared.ActorKindExecution {
		return domainassets.MarshalOperator(domainassets.OperatorSnapshot{
			ActorKind:      string(p.ActorKind),
			ActorID:        p.FunctionID,
			UserID:         p.InvokingUserID,
			CredentialType: string(p.CredentialType),
		})
	}
	return domainassets.MarshalOperator(domainassets.OperatorSnapshot{
		ActorKind:      string(p.ActorKind),
		ActorID:        string(p.ActorID),
		UserID:         p.UserID,
		APIKeyID:       p.APIKeyID,
		CredentialType: string(p.CredentialType),
		IsSystem:       p.IsSystem(),
	})
}

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
