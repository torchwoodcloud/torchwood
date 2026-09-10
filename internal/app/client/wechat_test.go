package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

func TestAccount_ResolveWeChatUser_CrossProviderLink(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	cfg := buildTestConfig()
	projectRepo := bunrepo.NewProjectRepository(db)
	account := NewTestAccount(cfg, projectRepo, db)

	unionID := "union-cross-link"
	webInfo := &domainauth.OAuthUserInfo{
		OpenID:      "web-openid",
		UnionID:     unionID,
		ProviderUID: domainauth.WeChatIdentityUID(unionID, "web-openid"),
		Name:        "WeChat Web",
	}

	user1, err := account.resolveWeChatUser(ctx, projectID, domainauth.ProviderWeChatWeb, webInfo)
	require.NoError(t, err)
	require.NotEmpty(t, user1.ID)

	mpInfo := &domainauth.OAuthUserInfo{
		OpenID:      "mp-openid",
		UnionID:     unionID,
		ProviderUID: domainauth.WeChatIdentityUID(unionID, "mp-openid"),
		Name:        "WeChat MP",
	}
	user2, err := account.resolveWeChatUser(ctx, projectID, domainauth.ProviderWeChatMiniProgram, mpInfo)
	require.NoError(t, err)
	require.Equal(t, user1.ID, user2.ID)
}

func TestAccount_CreateWeChatMiniProgramSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	_, err := db.NewInsert().Model(&model.ProjectOAuthProvider{
		ProjectID:    projectID,
		Provider:     domainauth.ProviderWeChatMiniProgram,
		Enabled:      true,
		ClientID:     "wx-test-appid",
		ClientSecret: "wx-test-secret",
	}).Exec(ctx)
	require.NoError(t, err)

	cfg := buildTestConfig()
	projectRepo := bunrepo.NewProjectRepository(db)
	oauthRepo := bunrepo.NewOAuthProviderRepository(db, cfg)
	account := NewTestAccountWithDeps(cfg, projectRepo, oauthRepo, db, nil, nil, nil)

	_, _, _, _, err = account.CreateWeChatMiniProgramSession(ctx, CreateWeChatMiniProgramSessionCommand{
		ProjectID: projectID,
		Code:      "invalid-code",
	})
	require.Error(t, err)
}
