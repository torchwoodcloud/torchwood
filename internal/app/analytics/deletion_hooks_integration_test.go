// 外部测试包（analytics_test）：注销双钩子端到端（PR5 验收，D10）——
//   - server 面 DeleteUser：既有删除事务内 tombstone INSERT（事务性）；
//   - client 面 DeleteAccount：软删后 tombstone INSERT；
//   - 端到端：DeleteUser → tombstone → 清洗后 raw/user_days/first_seen 无
//     该用户行、daily 计数不回退；重删幂等。
package analytics_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
	appclient "github.com/torchwoodcloud/torchwood/internal/app/client"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

// hooksEnv 装配注销双钩子端到端的生产同构用例（真 bun 仓储 + 真会话服务 +
// analytics tombstone 仓储）。
type hooksEnv struct {
	*rollupEnv
	users    *appserver.Users
	account  *appclient.Account
	maint    *appanalytics.Maintenance
	sessions *auth.SessionService
}

func setupHooksEnv(t *testing.T) *hooksEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	e := setupRollupEnv(t)

	cfg := &config.AppConfig{Security: &config.Security{Jwt: &config.Security_Jwt{Secret: "analytics-hooks-test-secret"}}} // #nosec G101 -- 测试固定密钥
	usersRepo := bunrepo.NewUserRepository(e.db)
	sessionRepo := bunrepo.NewSessionRepository(e.db)
	roles := appclient.NewUserRoles(usersRepo, bunrepo.NewMembershipRepository(e.db))
	sessions := auth.NewSessionService(cfg, sessionRepo, roles, nil)

	usersUC := appserver.NewUsers(e.projectsRepo, sessions, e.db, usersRepo, sessionRepo,
		bunrepo.NewGroupRepository(e.db), bunrepo.NewMembershipRepository(e.db), e.workerRepo)
	account := appclient.NewAccount(cfg, e.projectsRepo, nil, nil, sessions,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, roles, nil, nil, nil, nil,
		usersRepo, nil, sessionRepo, nil, nil, nil, nil, e.workerRepo, nil)

	maint := appanalytics.NewMaintenance(e.workerRepo, e.workerRepo, e.projectsRepo, 90, nil)
	return &hooksEnv{rollupEnv: e, users: usersUC, account: account, maint: maint, sessions: sessions}
}

// createUser 注册真实 users 行（匿名用户形态——归因锚点）。
func (e *hooksEnv) createUser(t *testing.T) string {
	t.Helper()
	userID := idgen.ULID().String()
	registered, err := users.Register(users.RegisterInput{
		ID:        userID,
		Email:     users.AnonymousEmail(userID),
		Name:      "HookUser",
		Anonymous: true,
	})
	require.NoError(t, err)
	require.NoError(t, bunrepo.NewUserRepository(e.db).Insert(e.ctx, e.projectID, registered))
	return userID
}

func (e *hooksEnv) serverCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "key-1", ActorKind: shared.ActorKindService,
		Roles: []string{"keys"}, Permissions: []string{"users.write"},
	})
}

func (e *hooksEnv) endUserCtx(userID string) context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: idgen.ID(userID), ActorKind: shared.ActorKindEndUser, UserID: userID, ProjectID: e.projectID,
	})
}

func (e *hooksEnv) tombstoneCount(t *testing.T, userID string, done bool) int {
	t.Helper()
	donePred := "IS NULL"
	if done {
		donePred = "IS NOT NULL"
	}
	return e.count(fmt.Sprintf(
		`SELECT count(*) FROM %s.analytics_user_deletions WHERE user_id = ? AND done_at %s`, e.schema, donePred), userID)
}

// TestHooksIntegration_ServerDeleteUserTombstone（PR5 验收：注销端到端）——
// DeleteUser → tombstone → 清洗后三表无该用户行、daily 不回退、重删幂等。
func TestHooksIntegration_ServerDeleteUserTombstone(t *testing.T) {
	e := setupHooksEnv(t)

	dayD := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	clock := dayD.Add(12 * time.Hour)
	userID := e.createUser(t)

	// 用户两日活跃 → rollup 产出三表。
	e.seedEvent(t, "level_complete", userID, dayD.AddDate(0, 0, -1).Add(9*time.Hour))
	e.seedEvent(t, "level_complete", userID, dayD.Add(10*time.Hour))
	e.runRollup(t, clock)
	require.Equal(t, 2, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_events WHERE user_id = ?`, e.schema), userID))
	require.Equal(t, 2, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_days WHERE user_id = ?`, e.schema), userID))
	require.Equal(t, 1, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_first_seen WHERE user_id = ?`, e.schema), userID))
	dailyBefore := e.count(fmt.Sprintf(`SELECT COALESCE(SUM(total), 0) FROM %s.analytics_daily`, e.schema))

	// server 面 DeleteUser（既有删除事务内 tombstone INSERT）。
	require.NoError(t, e.users.DeleteUser(e.serverCtx(), e.projectID, userID, databases.Principal{}))
	require.Equal(t, 1, e.tombstoneCount(t, userID, false), "删除事务内写入 tombstone（done_at NULL）")
	require.Equal(t, 0, e.tombstoneCount(t, userID, true))

	// worker 清洗。
	require.NoError(t, e.maint.RunCleanupOnce(e.ctx, clock))
	require.Equal(t, 0, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_events WHERE user_id = ?`, e.schema), userID))
	require.Equal(t, 0, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_days WHERE user_id = ?`, e.schema), userID))
	require.Equal(t, 0, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_first_seen WHERE user_id = ?`, e.schema), userID))
	require.Equal(t, dailyBefore, e.count(fmt.Sprintf(`SELECT COALESCE(SUM(total), 0) FROM %s.analytics_daily`, e.schema)),
		"daily 聚合计数不回退（D10 已声明近似口径）")
	require.Equal(t, 1, e.tombstoneCount(t, userID, true))

	// 重删幂等：重放 tombstone + 重跑清洗，无副作用。
	require.NoError(t, e.workerRepo.EnqueueUserDeletion(e.ctx, e.projectID, userID, clock.Add(time.Hour)))
	require.NoError(t, e.maint.RunCleanupOnce(e.ctx, clock.Add(time.Hour)))
	require.Equal(t, 1, e.tombstoneCount(t, userID, true))
	require.Equal(t, 0, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_events WHERE user_id = ?`, e.schema), userID))
}

// TestHooksIntegration_ClientDeleteAccountTombstone（PR5，D10 双钩子）——
// client 面 DeleteAccount 同样触发 tombstone（软删提交后写入）。
func TestHooksIntegration_ClientDeleteAccountTombstone(t *testing.T) {
	e := setupHooksEnv(t)

	dayD := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	clock := dayD.Add(12 * time.Hour)
	userID := e.createUser(t)
	e.seedEvent(t, "level_complete", userID, dayD.Add(10*time.Hour))
	e.runRollup(t, clock)

	require.NoError(t, e.account.DeleteAccount(e.endUserCtx(userID)))
	require.Equal(t, 1, e.tombstoneCount(t, userID, false), "client 面注销同样写入 tombstone")

	// 软删语义保持：status=deleted。
	found, err := bunrepo.NewUserRepository(e.db).GetByID(e.ctx, e.projectID, userID)
	require.NoError(t, err)
	require.NotNil(t, found)
	require.Equal(t, users.StatusDeleted, found.Status)

	// 清洗收敛。
	require.NoError(t, e.maint.RunCleanupOnce(e.ctx, clock))
	require.Equal(t, 0, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_events WHERE user_id = ?`, e.schema), userID))
	require.Equal(t, 1, e.tombstoneCount(t, userID, true))
}
