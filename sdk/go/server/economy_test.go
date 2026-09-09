package server

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

type fakeEconomy struct {
	serverv1.UnimplementedPaymentsServiceServer
	serverv1.UnimplementedAssetsServiceServer
	serverv1.UnimplementedSubscriptionsServiceServer
	serverv1.UnimplementedBillingServiceServer
}

func (f *fakeEconomy) ListOrders(_ context.Context, _ *sharedv1.ListRequest) (*serverv1.ListOrdersResponse, error) {
	return &serverv1.ListOrdersResponse{Orders: []*serverv1.PaymentOrder{{Id: "o1", Amount: 1999, UserId: "u1"}}}, nil
}

func (f *fakeEconomy) GetOrder(_ context.Context, req *serverv1.GetOrderRequest) (*serverv1.PaymentOrder, error) {
	return &serverv1.PaymentOrder{Id: req.OrderId, Amount: 1999, Status: "paid"}, nil
}

func (f *fakeEconomy) Refund(_ context.Context, req *serverv1.RefundRequest) (*serverv1.PaymentOrder, error) {
	return &serverv1.PaymentOrder{Id: req.OrderId, Status: "refunded", Amount: 1999}, nil
}

func (f *fakeEconomy) ManualFulfill(_ context.Context, req *serverv1.ManualFulfillRequest) (*serverv1.ManualFulfillResponse, error) {
	return &serverv1.ManualFulfillResponse{
		Order:       &serverv1.PaymentOrder{Id: req.OrderId, Status: "paid"},
		Fulfillment: &serverv1.Fulfillment{Id: "f1", OrderId: req.OrderId, Status: "done"},
	}, nil
}

func (f *fakeEconomy) CreateAssetDef(_ context.Context, req *serverv1.CreateAssetDefRequest) (*serverv1.AssetDef, error) {
	return &serverv1.AssetDef{Id: "d1", Code: req.Code, Class: req.Class, Name: req.Name}, nil
}

func (f *fakeEconomy) ListAssetDefs(_ context.Context, _ *sharedv1.ListRequest) (*serverv1.ListAssetDefsResponse, error) {
	return &serverv1.ListAssetDefsResponse{Defs: []*serverv1.AssetDef{{Id: "d1", Code: "gold"}}}, nil
}

func (f *fakeEconomy) GetAssetDef(_ context.Context, req *serverv1.GetAssetDefRequest) (*serverv1.AssetDef, error) {
	return &serverv1.AssetDef{Id: req.DefId, Code: "gold"}, nil
}

func (f *fakeEconomy) UpdateAssetDef(_ context.Context, req *serverv1.UpdateAssetDefRequest) (*serverv1.AssetDef, error) {
	return &serverv1.AssetDef{Id: req.DefId, Name: req.GetName()}, nil
}

func (f *fakeEconomy) DeleteAssetDef(_ context.Context, _ *serverv1.DeleteAssetDefRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (f *fakeEconomy) Grant(_ context.Context, req *serverv1.GrantRequest) (*serverv1.AssetOpResponse, error) {
	return &serverv1.AssetOpResponse{Entries: []*serverv1.AssetLedgerEntry{{Id: "e1", OwnerId: req.OwnerId, Delta: req.Quantity, QuantityAfter: req.Quantity}}}, nil
}

func (f *fakeEconomy) Consume(_ context.Context, req *serverv1.ConsumeRequest) (*serverv1.AssetOpResponse, error) {
	return &serverv1.AssetOpResponse{Entries: []*serverv1.AssetLedgerEntry{{Delta: -req.Quantity}}}, nil
}

func (f *fakeEconomy) Transfer(_ context.Context, _ *serverv1.TransferRequest) (*serverv1.AssetOpResponse, error) {
	return &serverv1.AssetOpResponse{Entries: []*serverv1.AssetLedgerEntry{{Kind: "transfer_out"}, {Kind: "transfer_in"}}}, nil
}

func (f *fakeEconomy) Mutate(_ context.Context, _ *serverv1.MutateRequest) (*serverv1.AssetOpResponse, error) {
	return &serverv1.AssetOpResponse{Entries: []*serverv1.AssetLedgerEntry{{Kind: "mutate"}}}, nil
}

func (f *fakeEconomy) Expire(_ context.Context, _ *serverv1.ExpireRequest) (*serverv1.AssetOpResponse, error) {
	return &serverv1.AssetOpResponse{Entries: []*serverv1.AssetLedgerEntry{{Kind: "expire"}}}, nil
}

func (f *fakeEconomy) Reconcile(_ context.Context, _ *serverv1.ReconcileRequest) (*serverv1.ReconcileResponse, error) {
	return &serverv1.ReconcileResponse{ZeroDrift: true}, nil
}

func (f *fakeEconomy) ListUserAssets(_ context.Context, req *serverv1.ListUserAssetsRequest) (*serverv1.ListUserAssetsResponse, error) {
	return &serverv1.ListUserAssetsResponse{Holdings: []*serverv1.AssetHolding{{Id: "h1", OwnerId: req.OwnerId, Quantity: 50, DefCode: "gold"}}}, nil
}

func (f *fakeEconomy) ListUserLedger(_ context.Context, req *serverv1.ListUserLedgerRequest) (*serverv1.ListUserLedgerResponse, error) {
	return &serverv1.ListUserLedgerResponse{Entries: []*serverv1.AssetLedgerEntry{{Id: "e1", OwnerId: req.OwnerId, Delta: 50}}}, nil
}

func (f *fakeEconomy) CreatePlan(_ context.Context, req *serverv1.CreatePlanRequest) (*serverv1.SubscriptionPlan, error) {
	return &serverv1.SubscriptionPlan{Id: "p1", Code: req.Code, Amount: req.Amount}, nil
}

func (f *fakeEconomy) ListPlans(_ context.Context, _ *sharedv1.ListRequest) (*serverv1.ListPlansResponse, error) {
	return &serverv1.ListPlansResponse{Plans: []*serverv1.SubscriptionPlan{{Id: "p1", Amount: 999}}}, nil
}

func (f *fakeEconomy) GetPlan(_ context.Context, req *serverv1.GetPlanRequest) (*serverv1.SubscriptionPlan, error) {
	return &serverv1.SubscriptionPlan{Id: req.PlanId, Amount: 999}, nil
}

func (f *fakeEconomy) UpdatePlan(_ context.Context, req *serverv1.UpdatePlanRequest) (*serverv1.SubscriptionPlan, error) {
	return &serverv1.SubscriptionPlan{Id: req.PlanId, Name: req.GetName()}, nil
}

func (f *fakeEconomy) DeletePlan(_ context.Context, _ *serverv1.DeletePlanRequest) (*sharedv1.Empty, error) {
	return &sharedv1.Empty{}, nil
}

func (f *fakeEconomy) ListSubscriptions(_ context.Context, _ *sharedv1.ListRequest) (*serverv1.ListSubscriptionsResponse, error) {
	return &serverv1.ListSubscriptionsResponse{Subscriptions: []*serverv1.Subscription{{Id: "s1"}}}, nil
}

func (f *fakeEconomy) GetSubscription(_ context.Context, req *serverv1.GetSubscriptionRequest) (*serverv1.Subscription, error) {
	return &serverv1.Subscription{Id: req.SubscriptionId, Status: "active"}, nil
}

func (f *fakeEconomy) CancelSubscription(_ context.Context, req *serverv1.CancelSubscriptionRequest) (*serverv1.Subscription, error) {
	return &serverv1.Subscription{Id: req.SubscriptionId, CancelAtPeriodEnd: true}, nil
}

func (f *fakeEconomy) ExpireSubscription(_ context.Context, req *serverv1.ExpireSubscriptionRequest) (*serverv1.Subscription, error) {
	return &serverv1.Subscription{Id: req.SubscriptionId, Status: "expired"}, nil
}

func (f *fakeEconomy) GetUsage(_ context.Context, _ *serverv1.GetUsageRequest) (*serverv1.Usage, error) {
	return &serverv1.Usage{ProjectId: "p1", Metrics: []*serverv1.UsageMetric{{Metric: "api_calls", Value: 10}}}, nil
}

func (f *fakeEconomy) ListRollups(_ context.Context, _ *serverv1.ListRollupsRequest) (*serverv1.ListRollupsResponse, error) {
	return &serverv1.ListRollupsResponse{Rollups: []*serverv1.UsageRollup{{Id: "r1", Value: 10}}}, nil
}

func (f *fakeEconomy) ListStatements(_ context.Context, _ *sharedv1.ListRequest) (*serverv1.ListStatementsResponse, error) {
	return &serverv1.ListStatementsResponse{Statements: []*serverv1.BillingStatement{{Id: "s1", Status: "draft"}}}, nil
}

func newEconomyClient(t *testing.T) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fake := &fakeEconomy{}
	serverv1.RegisterPaymentsServiceServer(srv, fake)
	serverv1.RegisterAssetsServiceServer(srv, fake)
	serverv1.RegisterSubscriptionsServiceServer(srv, fake)
	serverv1.RegisterBillingServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	c, err := New("passthrough:///bufconn",
		WithAPIKey("key-1"),
		WithDialOptions(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		})),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestServerPayments(t *testing.T) {
	c := newEconomyClient(t)
	ctx := context.Background()

	list, err := c.Payments.ListOrders(ctx, &sharedv1.ListRequest{PageSize: 20})
	require.NoError(t, err)
	require.Equal(t, int64(1999), list.Orders[0].Amount)

	got, err := c.Payments.GetOrder(ctx, "o1")
	require.NoError(t, err)
	require.Equal(t, "paid", got.Status)

	refunded, err := c.Payments.Refund(ctx, &serverv1.RefundRequest{OrderId: "o1"})
	require.NoError(t, err)
	require.Equal(t, "refunded", refunded.Status)

	ff, err := c.Payments.ManualFulfill(ctx, &serverv1.ManualFulfillRequest{OrderId: "o1", Reason: "demo"})
	require.NoError(t, err)
	require.Equal(t, "done", ff.Fulfillment.Status)
}

func TestServerAssets(t *testing.T) {
	c := newEconomyClient(t)
	ctx := context.Background()

	def, err := c.Assets.CreateAssetDef(ctx, &serverv1.CreateAssetDefRequest{Code: "gold", Name: "Gold", Class: "currency"})
	require.NoError(t, err)
	require.Equal(t, "gold", def.Code)

	defs, err := c.Assets.ListAssetDefs(ctx, &sharedv1.ListRequest{})
	require.NoError(t, err)
	require.Len(t, defs.Defs, 1)

	got, err := c.Assets.GetAssetDef(ctx, "d1")
	require.NoError(t, err)
	require.Equal(t, "gold", got.Code)

	name := "Gold v2"
	updated, err := c.Assets.UpdateAssetDef(ctx, &serverv1.UpdateAssetDefRequest{DefId: "d1", Name: &name})
	require.NoError(t, err)
	require.Equal(t, "Gold v2", updated.Name)

	require.NoError(t, c.Assets.DeleteAssetDef(ctx, "d1"))

	grant, err := c.Assets.Grant(ctx, &serverv1.GrantRequest{OwnerId: "u1", DefCode: "gold", Quantity: 100, IdempotencyKey: "g1"})
	require.NoError(t, err)
	require.Equal(t, int64(100), grant.Entries[0].Delta)

	_, err = c.Assets.Consume(ctx, &serverv1.ConsumeRequest{OwnerId: "u1", DefCode: "gold", Quantity: 10, IdempotencyKey: "c1"})
	require.NoError(t, err)
	_, err = c.Assets.Transfer(ctx, &serverv1.TransferRequest{FromOwnerId: "u1", ToOwnerId: "u2", DefCode: "gold", Quantity: 1, IdempotencyKey: "t1"})
	require.NoError(t, err)
	_, err = c.Assets.Mutate(ctx, &serverv1.MutateRequest{HoldingId: "h1", IdempotencyKey: "m1"})
	require.NoError(t, err)
	_, err = c.Assets.Expire(ctx, &serverv1.ExpireRequest{HoldingId: "h1", IdempotencyKey: "x1"})
	require.NoError(t, err)
	rec, err := c.Assets.Reconcile(ctx)
	require.NoError(t, err)
	require.True(t, rec.ZeroDrift)

	holdings, err := c.Assets.ListUserAssets(ctx, &serverv1.ListUserAssetsRequest{OwnerId: "u1"})
	require.NoError(t, err)
	require.Equal(t, int64(50), holdings.Holdings[0].Quantity)

	ledger, err := c.Assets.ListUserLedger(ctx, &serverv1.ListUserLedgerRequest{OwnerId: "u1"})
	require.NoError(t, err)
	require.Equal(t, "u1", ledger.Entries[0].OwnerId)
}

func TestServerSubscriptions(t *testing.T) {
	c := newEconomyClient(t)
	ctx := context.Background()

	plan, err := c.Subscriptions.CreatePlan(ctx, &serverv1.CreatePlanRequest{Code: "pro", Name: "Pro", Amount: 999, Currency: "USD", Interval: "month"})
	require.NoError(t, err)
	require.Equal(t, int64(999), plan.Amount)

	plans, err := c.Subscriptions.ListPlans(ctx, &sharedv1.ListRequest{})
	require.NoError(t, err)
	require.Len(t, plans.Plans, 1)

	got, err := c.Subscriptions.GetPlan(ctx, "p1")
	require.NoError(t, err)
	require.Equal(t, "p1", got.Id)

	name := "Pro+"
	_, err = c.Subscriptions.UpdatePlan(ctx, &serverv1.UpdatePlanRequest{PlanId: "p1", Name: &name})
	require.NoError(t, err)
	require.NoError(t, c.Subscriptions.DeletePlan(ctx, "p1"))

	subs, err := c.Subscriptions.ListSubscriptions(ctx, &sharedv1.ListRequest{})
	require.NoError(t, err)
	require.Len(t, subs.Subscriptions, 1)

	s, err := c.Subscriptions.GetSubscription(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "active", s.Status)

	canceled, err := c.Subscriptions.CancelSubscription(ctx, &serverv1.CancelSubscriptionRequest{SubscriptionId: "s1"})
	require.NoError(t, err)
	require.True(t, canceled.CancelAtPeriodEnd)

	expired, err := c.Subscriptions.ExpireSubscription(ctx, &serverv1.ExpireSubscriptionRequest{SubscriptionId: "s1"})
	require.NoError(t, err)
	require.Equal(t, "expired", expired.Status)
}

func TestServerBilling(t *testing.T) {
	c := newEconomyClient(t)
	ctx := context.Background()

	usage, err := c.Billing.GetUsage(ctx, &serverv1.GetUsageRequest{})
	require.NoError(t, err)
	require.Equal(t, int64(10), usage.Metrics[0].Value)

	rolls, err := c.Billing.ListRollups(ctx, &serverv1.ListRollupsRequest{})
	require.NoError(t, err)
	require.Len(t, rolls.Rollups, 1)

	stmts, err := c.Billing.ListStatements(ctx, &sharedv1.ListRequest{})
	require.NoError(t, err)
	require.Equal(t, "draft", stmts.Statements[0].Status)
}
