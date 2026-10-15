package projectschema_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/projectschema"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// TestApply_Webhooks 验证 000027 webhook_subscriptions / webhook_deliveries
// （Webhooks 一等公民，docs/design/functions-v4.md §6/D5）：
//  1. up：两表形态（状态 CHECK 词表、密钥加密列、投递调度列）；
//  2. 行为面：CHECK 约束拒非法状态、(webhook_id, event_id) 唯一索引幂等、
//     ON DELETE CASCADE 级联清投递行；
//  3. down → 成对 DROP；up 重放可恢复（运维回滚路径的对称性）。
func TestApply_Webhooks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	// CreateTestProject 已全量 Apply（含 000027）；显式再 Apply 一次确认幂等。
	require.NoError(t, projectschema.Apply(ctx, db, projectID))

	quoted := testutil.CatalogQuoted(projectID)
	schema := strings.Trim(quoted, `"`)

	// ① 订阅表形态：status CHECK 词表 + 密钥列。
	var statusCheck, secretCol int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = ? AND table_name = 'webhook_subscriptions' AND column_name = 'secret_enc'`,
		schema).Scan(&secretCol))
	require.Equal(t, 1, secretCol, "secret_enc 加密列存在")
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
		WHERE con.contype = 'c' AND con.conname = 'webhook_subscriptions_status_check' AND nsp.nspname = ?`,
		schema).Scan(&statusCheck))
	require.Equal(t, 1, statusCheck, "status CHECK 约束存在")

	// ② 行为面：合法行写入 → CHECK 拒非法状态 → 幂等唯一索引 → 级联删除。
	_, err := db.ExecContext(ctx, `INSERT INTO `+quoted+`.webhook_subscriptions
		(id, project_id, url, events, status, secret_enc, secret_fingerprint)
		VALUES ('wh-1', ?, 'https://example.com/hook', ARRAY['auth.*'], 'enabled', 'enc:v1:x', 'whsec_deadbeef')`, schema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO `+quoted+`.webhook_subscriptions
		(id, project_id, url, events, status, secret_enc, secret_fingerprint)
		VALUES ('wh-bad', ?, 'https://example.com/hook', ARRAY['auth.*'], 'disabled', 'enc:v1:x', 'whsec_x')`, schema)
	require.Error(t, err, "status CHECK 拒绝词表外状态")

	now := time.Now().UTC()
	payload := `{"specversion":"1.0"}`
	for i := 0; i < 2; i++ {
		_, err = db.ExecContext(ctx, `INSERT INTO `+quoted+`.webhook_deliveries
			(id, project_id, webhook_id, event_id, event_type, seq, payload, status, attempts, next_attempt_at, created_at, updated_at)
			VALUES ('whd-1', ?, 'wh-1', 'ev-1', 'auth.users.created', 1, ?, 'pending', 0, ?, ?, ?)`,
			schema, payload, now, now, now)
		if i == 0 {
			require.NoError(t, err)
		} else {
			require.Error(t, err, "(webhook_id, event_id) 唯一索引吸收重复（at-least-once 幂等）")
		}
	}

	// 到期扫描 partial 索引存在（webhook_deliveries_due）。
	var dueIdx int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_indexes WHERE schemaname = ? AND indexname = 'webhook_deliveries_due'`,
		schema).Scan(&dueIdx))
	require.Equal(t, 1, dueIdx)

	// 级联：删订阅 → 投递行随删。
	_, err = db.ExecContext(ctx, `DELETE FROM `+quoted+`.webhook_subscriptions WHERE id = 'wh-1'`)
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM `+quoted+`.webhook_deliveries WHERE webhook_id = 'wh-1'`).Scan(&n))
	require.Zero(t, n, "ON DELETE CASCADE 清投递行")

	// ③ down → 成对 DROP；up 重放 → 恢复。
	downSQL, err := os.ReadFile("migrations/000027_webhooks.down.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, strings.ReplaceAll(string(downSQL), "{{schema}}", quoted))
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = ? AND table_name IN ('webhook_subscriptions', 'webhook_deliveries')`,
		schema).Scan(&n))
	require.Zero(t, n, "down 后两表不存在")

	upSQL, err := os.ReadFile("migrations/000027_webhooks.up.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, strings.ReplaceAll(string(upSQL), "{{schema}}", quoted))
	require.NoError(t, err, "up 重放（幂等恢复）")
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = ? AND table_name IN ('webhook_subscriptions', 'webhook_deliveries')`,
		schema).Scan(&n))
	require.Equal(t, 2, n, "up 重放后两表恢复")
}
