package documentdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

func testSchema(t *testing.T, projectID, databaseID string) string {
	t.Helper()
	s, err := ident.SchemaName(projectID, databaseID)
	require.NoError(t, err)
	return s
}

func testProjectSchema(t *testing.T, projectID string) string {
	t.Helper()
	s, err := ident.ProjectSchemaName(projectID)
	require.NoError(t, err)
	return s
}

// testPhysicalName 返回集合服务端分配的物理表名（阶段②包 B：逻辑/物理名
// 解耦，测试直查/直改物理表时必须经 catalog 解析，不得再用逻辑名拼表名）。
func testPhysicalName(t *testing.T, ctx context.Context, db *clients.Database, projectID, databaseID, collectionID string) string {
	t.Helper()
	var physical string
	require.NoError(t, db.NewSelect().Model((*model.DocumentCollection)(nil)).
		Column("physical_name").
		Where("project_id = ? AND database_id = ? AND collection_id = ?", projectID, databaseID, collectionID).
		Scan(ctx, &physical))
	require.NotEmpty(t, physical)
	return physical
}
