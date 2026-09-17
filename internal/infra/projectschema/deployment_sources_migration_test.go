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

// TestApply_FunctionDeploymentSources 验证 000023 function_deployments 源
// 快照五列（Functions 部署源多元化二期阶段 1/4，
// docs/design/functions-runtimes-and-sources.md §0/§2）：
//  1. up：五列存在，source_type 带 CHECK 词表 zip|git|image 与 DEFAULT 'zip'；
//  2. 存量行回填：不带 source 列的 INSERT（迁移前形态）落库即 'zip' + 空串；
//  3. CHECK 兜底：词表外 source_type 拒绝；
//  4. down：五列成对 DROP；up 重放可恢复（运维回滚路径的对称性）。
func TestApply_FunctionDeploymentSources(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	// CreateTestProject 已全量 Apply（含 000023）；显式再 Apply 一次确认幂等。
	require.NoError(t, projectschema.Apply(ctx, db, projectID))

	quoted := testutil.CatalogQuoted(projectID)

	// ① 五列形态：类型/可空/默认值与迁移声明一致。
	type colInfo struct {
		dataType   string
		isNullable string
		columnDflt *string
	}
	colOf := func(t *testing.T, col string) colInfo {
		t.Helper()
		var ci colInfo
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT data_type, is_nullable, column_default
			FROM information_schema.columns
			WHERE table_schema = ? AND table_name = 'function_deployments' AND column_name = ?`,
			strings.Trim(quoted, `"`), col).Scan(&ci.dataType, &ci.isNullable, &ci.columnDflt), col)
		return ci
	}
	for _, col := range []string{"source_type", "source_url", "source_ref", "source_dir", "context_sha256"} {
		ci := colOf(t, col)
		require.Equal(t, "text", ci.dataType, col)
		require.Equal(t, "NO", ci.isNullable, col)
		require.NotNil(t, ci.columnDflt, "%s 必须带 DEFAULT（存量行回填依据）", col)
	}
	require.Contains(t, *colOf(t, "source_type").columnDflt, "zip")

	// ② 存量行回填：INSERT 不带 source 列（000023 之前的行形态），读回
	// source_type='zip'、其余空串——ALTER DEFAULT 即回填，无需数据迁移。
	seedFn := `INSERT INTO ` + quoted + `.functions (id, project_id, name, runtime)
		VALUES ('fn_legacy', ?, 'legacy', 'node-18.0')`
	_, err := db.ExecContext(ctx, seedFn, strings.Trim(quoted, `"`))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO `+quoted+`.function_deployments
		(id, function_id, project_id) VALUES ('dep_legacy', 'fn_legacy', ?)`,
		strings.Trim(quoted, `"`))
	require.NoError(t, err)

	var sourceType, sourceURL, sourceRef, sourceDir, contextSHA string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT source_type, source_url, source_ref, source_dir, context_sha256
		FROM `+quoted+`.function_deployments WHERE id = 'dep_legacy'`).
		Scan(&sourceType, &sourceURL, &sourceRef, &sourceDir, &contextSHA))
	require.Equal(t, "zip", sourceType, "存量行 DEFAULT 回填 zip")
	require.Empty(t, sourceURL)
	require.Empty(t, sourceRef)
	require.Empty(t, sourceDir)
	require.Empty(t, contextSHA)

	// ③ CHECK 兜底：词表外 source_type 拒绝。
	_, err = db.ExecContext(ctx, `INSERT INTO `+quoted+`.function_deployments
		(id, function_id, project_id, source_type) VALUES ('dep_bogus', 'fn_legacy', ?, 'tar')`,
		strings.Trim(quoted, `"`))
	require.Error(t, err, "source_type CHECK 词表外必须拒绝")
	_, err = db.ExecContext(ctx, `INSERT INTO `+quoted+`.function_deployments
		(id, function_id, project_id, source_type) VALUES ('dep_git', 'fn_legacy', ?, 'git')`,
		strings.Trim(quoted, `"`))
	require.NoError(t, err, "git 在词表内（二期启用）")
	_, err = db.ExecContext(ctx, `INSERT INTO `+quoted+`.function_deployments
		(id, function_id, project_id, source_type) VALUES ('dep_image', 'fn_legacy', ?, 'image')`,
		strings.Trim(quoted, `"`))
	require.NoError(t, err, "image 在词表内（三期启用预留）")

	// ④ down → 五列成对 DROP；up 重放 → 恢复（down 文件不经 embed，按
	// 磁盘相对路径读取，{{schema}} 占位符替换与 Apply 同规则）。
	downSQL, err := os.ReadFile("migrations/000023_function_deployment_sources.down.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, strings.ReplaceAll(string(downSQL), "{{schema}}", quoted))
	require.NoError(t, err)
	for _, col := range []string{"source_type", "source_url", "source_ref", "source_dir", "context_sha256"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_schema = ? AND table_name = 'function_deployments' AND column_name = ?`,
			strings.Trim(quoted, `"`), col).Scan(&n), col)
		require.Zero(t, n, "down 后 %s 应不存在", col)
	}

	upSQL, err := os.ReadFile("migrations/000023_function_deployment_sources.up.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, strings.ReplaceAll(string(upSQL), "{{schema}}", quoted))
	require.NoError(t, err)
	ci := colOf(t, "source_type")
	require.Equal(t, "text", ci.dataType)
	// PG 会在 column_default 上补类型后缀（'zip'::text），用包含断言。
	require.Contains(t, *ci.columnDflt, "'zip'", "up 重放后 DEFAULT 'zip' 恢复")
}
