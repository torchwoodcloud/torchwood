package bunrepo

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// SQL 形状断言（bun 更新写规范护栏之二，2026-09-08 dev 事故）：不依赖真实
// 数据库，直接断言 repo 更新方法渲染出的 UPDATE 语句 SET 列集合精确等于
// 白名单。bun v1.2.18 无 DryRun API，此处用 QueryHook 技法：DSN 指向不可达
// 端口，Exec 建连必失败，但 bun 在建连前先回调 BeforeQuery 并携带已渲染
// 语句。bun 升级改变渲染行为（尤其 skipupdate / DefaultPlaceholder 语义）
// 时，本组测试立即红灯。

type sqlShapeHook struct {
	mu   sync.Mutex
	last string
}

func (h *sqlShapeHook) BeforeQuery(_ context.Context, event *bun.QueryEvent) context.Context {
	h.mu.Lock()
	h.last = string(event.Query)
	h.mu.Unlock()
	return context.Background()
}

func (h *sqlShapeHook) AfterQuery(context.Context, *bun.QueryEvent) {}

func (h *sqlShapeHook) capturedSQL() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

func newRenderOnlyDB(t *testing.T) (*bun.DB, *sqlShapeHook) {
	t.Helper()
	hook := &sqlShapeHook{}
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN("postgres://render-only@127.0.0.1:1/none?sslmode=disable"),
		pgdriver.WithDialTimeout(500*time.Millisecond),
		pgdriver.WithReadTimeout(500*time.Millisecond),
	))
	bunDB := bun.NewDB(sqldb, pgdialect.New())
	bunDB.AddQueryHook(hook)
	t.Cleanup(func() { _ = bunDB.Close() })
	return bunDB, hook
}

var setColumnRe = regexp.MustCompile(`"([A-Za-z0-9_]+)"`)

// setColumnsOf 提取 UPDATE 语句 SET 子句中的列名。
func setColumnsOf(t *testing.T, query string) []string {
	t.Helper()
	start := strings.Index(query, " SET ")
	require.NotEqual(t, -1, start, "语句缺少 SET 子句: %s", query)
	end := strings.Index(query[start:], " WHERE ")
	require.NotEqual(t, -1, end, "语句缺少 WHERE 子句: %s", query)
	section := query[start+len(" SET ") : start+end]
	matches := setColumnRe.FindAllStringSubmatch(section, -1)
	cols := make([]string, 0, len(matches))
	for _, m := range matches {
		cols = append(cols, m[1])
	}
	return cols
}

// TestUpdateProject_SQLShapeWhitelist：UpdateProject 的 SET 列集合必须精确
// 等于登记白名单，且恒不触碰 identity 列 internal_id（事故原样路径：改
// registration_policy 曾把它渲染成 DEFAULT = nextval，每次更新烧号并改写
// 数据面租户号）。
func TestUpdateProject_SQLShapeWhitelist(t *testing.T) {
	bunDB, hook := newRenderOnlyDB(t)
	repo := NewProjectRepository(&clients.Database{DB: bunDB})

	p := &projects.Project{
		ID:                 "p1",
		InternalID:         4242,
		Name:               "demo",
		Description:        "d",
		Status:             "active",
		Settings:           map[string]any{"k": "v"},
		RegistrationPolicy: "invite_only",
		CreatedAt:          time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC),
		UpdatedAt:          time.Date(2026, 9, 8, 1, 0, 0, 0, time.UTC),
	}
	_ = repo.UpdateProject(context.Background(), p) // 建连失败无妨，SQL 已捕获

	query := hook.capturedSQL()
	require.ElementsMatch(t,
		[]string{"name", "description", "registration_policy", "updated_at"},
		setColumnsOf(t, query),
		"SET 列集合必须精确等于登记白名单（新可变列须显式登记）")
	require.NotContains(t, query, "internal_id", "identity 列对 UPDATE 只读（硬不变量）")
	require.NotContains(t, query, "created_at")
	require.NotContains(t, query, "status")
	require.NotContains(t, query, "settings")
	require.NotContains(t, query, "DEFAULT")
}

// TestProjectModel_SkipUpdateExcludesInternalID：model.Project.InternalID 的
// skipupdate 标签是模型侧防线（bun 对 autoincrement 隐式置 NullZero，
// DefaultPlaceholder 会把零值渲染成 DEFAULT）。本测试锁死该标签在 bun UPDATE
// 渲染路径上的语义——bun 升级若丢失 skipupdate 支持立即红灯。
func TestProjectModel_SkipUpdateExcludesInternalID(t *testing.T) {
	bunDB, hook := newRenderOnlyDB(t)

	m := &model.Project{
		ID:                 "p1",
		InternalID:         42,
		Name:               "demo",
		RegistrationPolicy: "open",
		Settings:           map[string]any{},
		CreatedAt:          time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC),
		UpdatedAt:          time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC),
	}
	_, _ = bunDB.NewUpdate().Model(m).WherePK().Exec(context.Background())

	require.NotContains(t, hook.capturedSQL(), "internal_id",
		"model.Project.InternalID 的 skipupdate 失效：bun 升级行为变化，identity 列将随全模型 UPDATE 被 DEFAULT 重写")
}
