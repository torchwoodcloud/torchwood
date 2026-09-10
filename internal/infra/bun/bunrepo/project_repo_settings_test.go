package bunrepo_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
)

// TestSetProjectSetting 验证 settings 单键原子写端口：写入/删除/其余键保留，
// 且 name/registration_policy 等其他列不受触碰（settings 不在 UpdateProject
// 白名单，本方法是 settings 的唯一更新入口）。
func TestSetProjectSetting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewProjectRepository(db)
	writer := bunrepo.NewProjectSettingsWriter(db)

	// 写入新键（settings 原为空对象）。
	require.NoError(t, writer.SetProjectSetting(ctx, projectID,
		projects.SettingsKeyOAuthAllowedRedirectURLs,
		[]string{"https://app.example.com", "http://localhost:5173"}))

	got, err := repo.GetProject(ctx, projectID)
	require.NoError(t, err)
	require.Equal(t, []string{"https://app.example.com", "http://localhost:5173"},
		projects.OAuthAllowedRedirectURLs(got.Settings))
	require.NotEmpty(t, got.Settings, "settings 应为含白名单键的对象")

	// 其余键保留：先种一个伴生键，再改白名单键。
	require.NoError(t, writer.SetProjectSetting(ctx, projectID, "auth.other_key", map[string]any{"v": 1}))
	require.NoError(t, writer.SetProjectSetting(ctx, projectID,
		projects.SettingsKeyOAuthAllowedRedirectURLs, []string{"https://app.example.com"}))
	got, err = repo.GetProject(ctx, projectID)
	require.NoError(t, err)
	require.Equal(t, []string{"https://app.example.com"},
		projects.OAuthAllowedRedirectURLs(got.Settings))
	require.Equal(t, float64(1), got.Settings["auth.other_key"].(map[string]any)["v"],
		"伴生 settings 键不得被白名单键写入覆盖")

	// 其他列不触碰。
	require.NoError(t, writer.SetProjectSetting(ctx, projectID,
		projects.SettingsKeyOAuthAllowedRedirectURLs, nil))
	got, err = repo.GetProject(ctx, projectID)
	require.NoError(t, err)
	require.Nil(t, projects.OAuthAllowedRedirectURLs(got.Settings), "nil = 删除键")
	require.NotContains(t, got.Settings, "auth.oauth_allowed_redirect_urls")
	require.Contains(t, got.Settings, "auth.other_key", "其他键保留")
	require.Equal(t, "open", got.RegistrationPolicy, "registration_policy 不得被 settings 写触碰")
	require.NotEmpty(t, got.Name)
	require.NotZero(t, got.InternalID, "internal_id 对 settings 写只读")
}
