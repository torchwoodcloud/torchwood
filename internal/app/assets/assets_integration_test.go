package assets

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

func TestIntegration_PaidTopupGrantsInSameTx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	uc := NewAssets(
		db,
		bunrepo.NewAssetDefRepository(db),
		bunrepo.NewAssetHoldingRepository(db),
		bunrepo.NewAssetLedgerRepository(db),
		infraevents.NewEventOutbox(db),
		nil,
		bunrepo.NewProjectRepository(db),
	)
	admin := contexts.WithPrincipal(ctx, &domainshared.Principal{
		ActorKind:      domainshared.ActorKindService,
		CredentialType: domainshared.CredentialTypeAPIKey,
		ProjectID:      projectID,
		APIKeyID:       "k1",
	})
	_, err := uc.CreateDef(admin, CreateDefCommand{
		Code: "gold", Name: "Gold", Class: domainassets.ClassCurrency,
	})
	require.NoError(t, err)

	f := NewOrderFulfiller(uc)
	order := &domainpayments.Order{
		ID: "ord-it-1", ProjectID: projectID, UserID: "u1", Amount: 100,
		PurposeKind: domainpayments.PurposeTopup,
		Purpose:     json.RawMessage(`{"currency_code":"gold","amount":100}`),
	}
	err = db.RunInTx(admin, func(txCtx context.Context) error {
		_, err := f.Fulfill(txCtx, order)
		if err != nil {
			return err
		}
		return errors.New("boom") // 模拟后续失败，整单回滚
	})
	require.Error(t, err)

	holdings, err := bunrepo.NewAssetHoldingRepository(db).ListByOwner(ctx, projectID, domainassets.OwnerTypeUser, "u1", 10, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, holdings, "履约失败必须与外层事务一起回滚")

	_, err = f.Fulfill(admin, order)
	require.NoError(t, err)
	holdings, err = bunrepo.NewAssetHoldingRepository(db).ListByOwner(ctx, projectID, domainassets.OwnerTypeUser, "u1", 10, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, holdings, 1)
	require.Equal(t, int64(100), holdings[0].Quantity)

	report, err := uc.Reconcile(admin)
	require.NoError(t, err)
	require.True(t, report.ZeroDrift)
}
