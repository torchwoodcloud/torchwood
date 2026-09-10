// Package subscriptions 是 v3 订阅子域的 use-case 聚合（设计 §3）：
// 计划 CRUD、Subscribe/Cancel、hosted webhook 镜像、platform 周期扣款与 benefits 履约。
package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/uow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultListLimit = 25
	maxListLimit     = 100
	maxIdempotency   = 128
	billingBatch     = 100
)

var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var currencyPattern = regexp.MustCompile(`^[A-Za-z]{3}$`)

var (
	subscriptionBillingCycleTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_subscription_billing_cycle_total",
		Help: "Subscription billing cycle outcomes.",
	}, []string{"result"})
)

func init() {
	prometheus.MustRegister(subscriptionBillingCycleTotal)
}

// assetOps 是订阅履约所需的资产动词（*assets.Assets 满足）。
type assetOps interface {
	Grant(ctx context.Context, cmd appassets.GrantCommand) (*appassets.OpResult, error)
	Consume(ctx context.Context, cmd appassets.ConsumeCommand) (*appassets.OpResult, error)
	Mutate(ctx context.Context, cmd appassets.MutateCommand) (*appassets.OpResult, error)
	LiveHoldingForUpdate(ctx context.Context, ownerID, defCode string) (*domainassets.Holding, error)
}

// Subscriptions 是订阅子域 use-case 聚合。
type Subscriptions struct {
	cfg        *config.AppConfig
	db         uow.Isolator
	plans      domainsubs.PlanRepo
	subs       domainsubs.SubscriptionRepo
	assets     assetOps
	orders     domainpayments.OrderRepo
	providers  domainpayments.ProviderRegistry
	hosted     domainsubs.HostedBilling
	events     shared.EventPublisher
	projects   projects.Repository
	index      domainpayments.ProviderIndexRepo
	logger     *slog.Logger
	now        func() time.Time
	scanCursor appshared.ProjectRotation // RunBillingCycle 轮转游标（tick 串行）
}

// NewSubscriptions 构造 use-case 聚合（Wire）。
func NewSubscriptions(
	cfg *config.AppConfig,
	db uow.Isolator,
	plans domainsubs.PlanRepo,
	subs domainsubs.SubscriptionRepo,
	assets *appassets.Assets,
	orders domainpayments.OrderRepo,
	providers domainpayments.ProviderRegistry,
	hosted domainsubs.HostedBilling,
	events shared.EventPublisher,
	logger *slog.Logger,
	projectRepo projects.Repository,
	index domainpayments.ProviderIndexRepo,
) *Subscriptions {
	return newSubscriptions(cfg, db, plans, subs, assets, orders, providers, hosted, events, logger, projectRepo, index)
}

func newSubscriptions(
	cfg *config.AppConfig,
	db uow.Isolator,
	plans domainsubs.PlanRepo,
	subs domainsubs.SubscriptionRepo,
	assets assetOps,
	orders domainpayments.OrderRepo,
	providers domainpayments.ProviderRegistry,
	hosted domainsubs.HostedBilling,
	events shared.EventPublisher,
	logger *slog.Logger,
	projectRepo projects.Repository,
	index domainpayments.ProviderIndexRepo,
) *Subscriptions {
	if logger == nil {
		logger = slog.Default()
	}
	return &Subscriptions{
		cfg:       cfg,
		db:        db,
		plans:     plans,
		subs:      subs,
		assets:    assets,
		orders:    orders,
		providers: providers,
		hosted:    hosted,
		events:    events,
		projects:  projectRepo,
		index:     index,
		logger:    logger,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

func (s *Subscriptions) upsertIndex(ctx context.Context, provider, kind, ref, projectID string) error {
	if s.index == nil || ref == "" {
		return nil
	}
	return s.index.Upsert(ctx, provider, kind, ref, projectID)
}

func (s *Subscriptions) ts() time.Time {
	if s.now != nil {
		return s.now()
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
	c := strings.ToLower(strings.TrimSpace(code))
	if !codePattern.MatchString(c) {
		return "", status.Error(codes.InvalidArgument, "plan code must match ^[a-z][a-z0-9_]{0,63}$")
	}
	return c, nil
}

func validateIdempotency(key string) (string, error) {
	k := strings.TrimSpace(key)
	if k == "" {
		return "", status.Error(codes.InvalidArgument, domainsubs.ErrIdempotencyRequired.Error())
	}
	if len(k) > maxIdempotency {
		return "", status.Errorf(codes.InvalidArgument, "idempotency_key exceeds %d characters", maxIdempotency)
	}
	return k, nil
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

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, domainsubs.ErrPlanNotFound), errors.Is(err, domainsubs.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, domainsubs.ErrPlanArchived),
		errors.Is(err, domainsubs.ErrAlreadySubscribed),
		errors.Is(err, domainsubs.ErrInvalidTransition),
		errors.Is(err, domainsubs.ErrReplayTerminalSubscription):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, domainsubs.ErrDuplicateCode):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, domainsubs.ErrInvalidMode),
		errors.Is(err, domainsubs.ErrIdempotencyRequired):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, domainsubs.ErrNotConfigured):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, domainsubs.ErrConcurrent):
		return status.Error(codes.Aborted, err.Error())
	default:
		return err
	}
}

func (s *Subscriptions) publish(ctx context.Context, sub *domainsubs.Subscription, event string, now time.Time) error {
	if s.events == nil {
		return nil
	}
	return s.events.Publish(ctx, domainevents.Envelope{
		EventID:   newID(),
		Event:     event,
		ProjectID: sub.ProjectID,
		Domain:    domainsubs.EventDomain,
		Channel:   domainsubs.AccountsChannel(sub.UserID),
		CreatedAt: now,
		// version 用订阅 updated_at 纳秒（判序语义同订单事件，P1-14）。
		Version: sub.UpdatedAt.UnixNano(),
		Attrs: map[string]any{
			"subscription_id": sub.ID,
			"user_id":         sub.UserID,
			"plan_id":         sub.PlanID,
			"mode":            string(sub.Mode),
			"status":          string(sub.Status),
		},
	})
}

func purposeJSON(subID, planCode, cycle string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"subscription_id": subID,
		"plan_code":       planCode,
		"cycle":           cycle,
	})
	return raw
}

func parseSubscriptionPurpose(raw json.RawMessage) (subID, planCode, cycle string, err error) {
	var p struct {
		SubscriptionID string `json:"subscription_id"`
		PlanCode       string `json:"plan_code"`
		Cycle          string `json:"cycle"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", "", status.Error(codes.InvalidArgument, "invalid subscription purpose")
	}
	if p.SubscriptionID == "" {
		return "", "", "", status.Error(codes.InvalidArgument, "purpose.subscription_id is required")
	}
	return p.SubscriptionID, p.PlanCode, p.Cycle, nil
}
