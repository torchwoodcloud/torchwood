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

// TestApply_FunctionDeploymentBuildNode 验证 000024 function_deployments
// build_node 列（四期 4a-1，Functions 多机执行面构建亲和，
// docs/design/functions-runtimes-and-sources.md §4 M5）：
//  1. up：列存在，TEXT NOT NULL DEFAULT ”（存量行回填空串 = 无亲和）；
//  2. 存量行回填：不带 build_node 的 INSERT（迁移前形态）落库即空串；
//  3. down：成对 DROP；up 重放可恢复（运维回滚路径的对称性）。
func TestApply_FunctionDeploymentBuildNode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	// CreateTestProject 已全量 Apply（含 000024）；显式再 Apply 一次确认幂等。
	require.NoError(t, projectschema.Apply(ctx, db, projectID))

	quoted := testutil.CatalogQuoted(projectID)
	schema := strings.Trim(quoted, `"`)

	// ① 列形态：text / NOT NULL / DEFAULT ''。
	type colInfo struct {
		dataType   string
		isNullable string
		columnDflt *string
	}
	var ci colInfo
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = ? AND table_name = 'function_deployments' AND column_name = 'build_node'`,
		schema).Scan(&ci.dataType, &ci.isNullable, &ci.columnDflt))
	require.Equal(t, "text", ci.dataType)
	require.Equal(t, "NO", ci.isNullable)
	require.NotNil(t, ci.columnDflt, "build_node 必须带 DEFAULT ''（存量行回填依据）")

	// ② 存量行回填：INSERT 不带 build_node（000024 之前的行形态）→ 空串。
	seedFn := `INSERT INTO ` + quoted + `.functions (id, project_id, name, runtime)
		VALUES ('fn_legacy', ?, 'legacy', 'node-18.0')`
	_, err := db.ExecContext(ctx, seedFn, schema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO `+quoted+`.function_deployments
		(id, function_id, project_id) VALUES ('dep_legacy', 'fn_legacy', ?)`,
		schema)
	require.NoError(t, err)

	var buildNode string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT build_node
		FROM `+quoted+`.function_deployments WHERE id = 'dep_legacy'`).Scan(&buildNode))
	require.Empty(t, buildNode, "存量行 DEFAULT 回填空串 = 无亲和")

	// ③ down → 成对 DROP；up 重放 → 恢复（down 文件不经 embed，按磁盘相对
	// 路径读取，{{schema}} 占位符替换与 Apply 同规则）。
	downSQL, err := os.ReadFile("migrations/000024_function_deployment_build_node.down.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, strings.ReplaceAll(string(downSQL), "{{schema}}", quoted))
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = ? AND table_name = 'function_deployments' AND column_name = 'build_node'`,
		schema).Scan(&n))
	require.Zero(t, n, "down 后 build_node 应不存在")

	upSQL, err := os.ReadFile("migrations/000024_function_deployment_build_node.up.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, strings.ReplaceAll(string(upSQL), "{{schema}}", quoted))
	require.NoError(t, err)
	var ci2 colInfo
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = ? AND table_name = 'function_deployments' AND column_name = 'build_node'`,
		schema).Scan(&ci2.dataType, &ci2.isNullable, &ci2.columnDflt))
	require.Equal(t, "text", ci2.dataType)
	require.Contains(t, *ci2.columnDflt, "''", "up 重放后 DEFAULT '' 恢复")
}
