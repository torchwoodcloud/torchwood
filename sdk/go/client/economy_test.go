package client

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

type fakeEconomy struct {
	clientv1.UnimplementedPaymentsServiceServer
	clientv1.UnimplementedAssetsServiceServer
	clientv1.UnimplementedSubscriptionsServiceServer
	rec *recorder
}

func (s *fakeEconomy) CreateOrder(ctx context.Context, req *clientv1.CreateOrderRequest) (*clientv1.CreateOrderResponse, error) {
	s.rec.record(ctx)
	return &clientv1.CreateOrderResponse{
		Order: &clientv1.PaymentOrder{
			Id: "ord-1", Amount: req.Amount, Currency: req.Currency, PurposeKind: req.PurposeKind, Status: "paying",
		},
	}, nil
}

func (s *fakeEconomy) GetMyOrder(ctx context.Context, req *clientv1.GetMyOrderRequest) (*clientv1.PaymentOrder, error) {
	s.rec.record(ctx)
	return &clientv1.PaymentOrder{Id: req.OrderId, Amount: 1999, Currency: "USD", Status: "paid"}, nil
}

func (s *fakeEconomy) ListMyOrders(ctx context.Context, _ *clientv1.ListMyOrdersRequest) (*clientv1.ListMyOrdersResponse, error) {
	s.rec.record(ctx)
	return &clientv1.ListMyOrdersResponse{Orders: []*clientv1.PaymentOrder{{Id: "ord-1", Amount: 1999}}}, nil
}

func (s *fakeEconomy) VerifyReceipt(ctx context.Context, req *clientv1.VerifyReceiptRequest) (*clientv1.VerifyReceiptResponse, error) {
	s.rec.record(ctx)
	return &clientv1.VerifyReceiptResponse{
		Order: &clientv1.PaymentOrder{Id: req.OrderId, Status: "paid", Amount: 1999}, TransactionId: "txn-1",
	}, nil
}

func (s *fakeEconomy) ListAssetDefs(ctx context.Context, _ *clientv1.ListAssetDefsRequest) (*clientv1.ListAssetDefsResponse, error) {
	s.rec.record(ctx)
	return &clientv1.ListAssetDefsResponse{Defs: []*clientv1.AssetDef{{Id: "d1", Code: "gold", Class: "currency"}}}, nil
}

func (s *fakeEconomy) ListMyAssets(ctx context.Context, _ *clientv1.ListMyAssetsRequest) (*clientv1.ListMyAssetsResponse, error) {
	s.rec.record(ctx)
	return &clientv1.ListMyAssetsResponse{Holdings: []*clientv1.AssetHolding{{Id: "h1", DefCode: "gold", Quantity: 100}}}, nil
}

func (s *fakeEconomy) ListMyAssetLedger(ctx context.Context, req *clientv1.ListMyAssetLedgerRequest) (*clientv1.ListMyAssetLedgerResponse, error) {
	s.rec.record(ctx)
	return &clientv1.ListMyAssetLedgerResponse{Entries: []*clientv1.AssetLedgerEntry{{Id: "e1", DefCode: req.DefCode, Delta: 100, QuantityAfter: 100}}}, nil
}

func (s *fakeEconomy) ListPlans(ctx context.Context, _ *clientv1.ListPlansRequest) (*clientv1.ListPlansResponse, error) {
	s.rec.record(ctx)
	return &clientv1.ListPlansResponse{Plans: []*clientv1.SubscriptionPlan{{Id: "p1", Code: "pro", Amount: 999}}}, nil
}

func (s *fakeEconomy) Subscribe(ctx context.Context, req *clientv1.SubscribeRequest) (*clientv1.SubscribeResponse, error) {
	s.rec.record(ctx)
	return &clientv1.SubscribeResponse{Subscription: &clientv1.Subscription{Id: "sub-1", PlanCode: req.PlanCode, Status: "active"}}, nil
}

func (s *fakeEconomy) GetMySubscription(ctx context.Context, _ *clientv1.GetMySubscriptionRequest) (*clientv1.Subscription, error) {
	s.rec.record(ctx)
	return &clientv1.Subscription{Id: "sub-1", Status: "active"}, nil
}

func (s *fakeEconomy) Cancel(ctx context.Context, req *clientv1.CancelRequest) (*clientv1.Subscription, error) {
	s.rec.record(ctx)
	return &clientv1.Subscription{Id: req.SubscriptionId, CancelAtPeriodEnd: true}, nil
}

func newEconomyClient(t *testing.T) (*Client, *recorder) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	rec := &recorder{}
	srv := grpc.NewServer()
	fake := &fakeEconomy{rec: rec}
	clientv1.RegisterPaymentsServiceServer(srv, fake)
	clientv1.RegisterAssetsServiceServer(srv, fake)
	clientv1.RegisterSubscriptionsServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	c, err := New("passthrough:///bufconn",
		WithInitialTokens(&clientv1.TokenBundle{AccessToken: "jwt-1"}),
		WithDialOptions(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		})),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, rec
}

func TestClientPayments_CreateGetList(t *testing.T) {
	c, rec := newEconomyClient(t)
	ctx := context.Background()

	created, err := c.Payments.CreateOrderValues(ctx, "idem-1", "stripe", 1999, "USD", "topup", map[string]any{"currency_code": "gold", "amount": "100"})
	require.NoError(t, err)
	require.Equal(t, "ord-1", created.Order.Id)
	require.Equal(t, int64(1999), created.Order.Amount)
	require.Equal(t, "Bearer jwt-1", rec.auth("authorization"))

	got, err := c.Payments.GetMyOrder(ctx, "ord-1")
	require.NoError(t, err)
	require.Equal(t, int64(1999), got.Amount)

	list, err := c.Payments.ListMyOrders(ctx, 20, "")
	require.NoError(t, err)
	require.Len(t, list.Orders, 1)

	vr, err := c.Payments.VerifyReceipt(ctx, "ord-1", "base64-receipt")
	require.NoError(t, err)
	require.Equal(t, "txn-1", vr.TransactionId)
}

func TestClientAssets_ReadOnly(t *testing.T) {
	c, _ := newEconomyClient(t)
	ctx := context.Background()

	defs, err := c.Assets.ListAssetDefs(ctx, 20, "")
	require.NoError(t, err)
	require.Equal(t, "gold", defs.Defs[0].Code)

	holdings, err := c.Assets.ListMyAssets(ctx, 20, "")
	require.NoError(t, err)
	require.Equal(t, int64(100), holdings.Holdings[0].Quantity)

	ledger, err := c.Assets.ListMyAssetLedger(ctx, "gold", 20, "")
	require.NoError(t, err)
	require.Equal(t, int64(100), ledger.Entries[0].Delta)
}

func TestClientSubscriptions_SubscribeAndCancel(t *testing.T) {
	c, _ := newEconomyClient(t)
	ctx := context.Background()

	plans, err := c.Subscriptions.ListPlans(ctx, 20, "")
	require.NoError(t, err)
	require.Equal(t, int64(999), plans.Plans[0].Amount)

	sub, err := c.Subscriptions.Subscribe(ctx, &clientv1.SubscribeRequest{PlanCode: "pro", Mode: "platform", IdempotencyKey: "idem-s"})
	require.NoError(t, err)
	require.Equal(t, "sub-1", sub.Subscription.Id)

	mine, err := c.Subscriptions.GetMySubscription(ctx, "")
	require.NoError(t, err)
	require.Equal(t, "active", mine.Status)

	canceled, err := c.Subscriptions.Cancel(ctx, "sub-1")
	require.NoError(t, err)
	require.True(t, canceled.CancelAtPeriodEnd)
}
