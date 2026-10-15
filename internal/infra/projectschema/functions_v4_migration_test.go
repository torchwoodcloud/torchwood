package projectschema_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/projectschema"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// TestApply_FunctionsV4Target 验证 000028（Functions v4 破坏波，docs/design/
// functions-v4.md §2/§3/§7/§10）：
//  1. up：function_executions → function_invocations 改名（函数索引/幂等唯一
//     索引/限频 partial 索引随迁）；deployment_id 列退役；function_deployments
//     与 function_variables 表 DROP；functions 池五列 + runtime 系列列 DROP、
//     target 列族 ADD；
//  2. 行为面：改名后的表可写可查、target 列落库；
//  3. down → 对称回滚（v3 形态复位），up 重放可恢复。
func TestApply_FunctionsV4Target(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	// CreateTestProject 已全量 Apply（含 000028）；显式再 Apply 一次确认幂等。
	require.NoError(t, projectschema.Apply(ctx, db, projectID))

	quoted := testutil.CatalogQuoted(projectID)
	schema := strings.Trim(quoted, `"`)

	// ① 表改名 + 退役表消失 + target 列族存在。
	var invocations int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = ? AND table_name = 'function_invocations'`, schema).Scan(&invocations))
	require.Equal(t, 1, invocations, "function_invocations 表存在（改名后）")

	for _, gone := range []string{"function_executions", "function_deployments", "function_variables"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.tables
			WHERE table_schema = ? AND table_name = ?`, schema, gone).Scan(&n))
		require.Equal(t, 0, n, "%s 应已退役", gone)
	}

	for _, col := range []string{"target_url", "target_secret_enc", "target_previous_secret_enc",
		"target_secret_rotated_at", "target_secret_fingerprint"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_schema = ? AND table_name = 'functions' AND column_name = ?`, schema, col).Scan(&n))
		require.Equal(t, 1, n, "functions.%s 列存在", col)
	}
	for _, gone := range []string{"runtime", "entrypoint", "spec", "min_instances", "max_instances",
		"idle_ttl_seconds", "max_requests_per_instance", "concurrency", "latest_ready_deployment_id"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_schema = ? AND table_name = 'functions' AND column_name = ?`, schema, gone).Scan(&n))
		require.Equal(t, 0, n, "functions.%s 应已退役", gone)
	}

	// deployment_id 列退役。
	var depCol int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = ? AND table_name = 'function_invocations' AND column_name = 'deployment_id'`, schema).Scan(&depCol))
	require.Equal(t, 0, depCol)

	// ② 索引随迁。
	for _, idx := range []string{"function_invocations_function_idx",
		"function_invocations_client_idem_key", "function_invocations_client_quota"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_indexes WHERE schemaname = ? AND indexname = ?`, schema, idx).Scan(&n))
		require.Equal(t, 1, idx2count(n), "索引 %s 随表改名", idx)
	}

	// ③ 行为面：写入 + 幂等唯一索引 + 限频 partial 索引可用。
	_, err := db.ExecContext(ctx, `INSERT INTO `+quoted+`.functions
		(id, project_id, name, target_url, target_secret_enc, target_secret_fingerprint, enabled)
		VALUES ('fn-1', ?, 'fn', 'https://fn.example.com', 'enc:v1:x', 'twfnsec_deadbeef', TRUE)`, schema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO `+quoted+`.function_invocations
		(id, function_id, project_id, status, trigger_source, invoking_user_id, client_idempotency_key, created_at, updated_at)
		VALUES ('inv-1', 'fn-1', ?, 'queued', 'client', 'user-1', 'idem-1', NOW(), NOW())`, schema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO `+quoted+`.function_invocations
		(id, function_id, project_id, status, trigger_source, invoking_user_id, client_idempotency_key, created_at, updated_at)
		VALUES ('inv-2', 'fn-1', ?, 'queued', 'client', 'user-1', 'idem-1', NOW(), NOW())`, schema)
	require.Error(t, err, "幂等唯一索引随表改名后仍生效（23505 去重）")

	// ④ down → 对称回滚（v3 形态复位）；up 重放可恢复（运维回滚路径对称性）。
	downSQL, err := os.ReadFile("migrations/000028_functions_v4_target.down.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, strings.ReplaceAll(string(downSQL), "{{schema}}", quoted))
	require.NoError(t, err)
	var reverted int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = ? AND table_name = 'function_executions'`, schema).Scan(&reverted))
	require.Equal(t, 1, reverted, "down 后 v3 表名复位")

	upSQL, err := os.ReadFile("migrations/000028_functions_v4_target.up.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, strings.ReplaceAll(string(upSQL), "{{schema}}", quoted))
	require.NoError(t, err, "up 重放（幂等恢复）")
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = ? AND table_name = 'function_invocations'`, schema).Scan(&reverted))
	require.Equal(t, 1, reverted)
}

func idx2count(n int) int { return n }
