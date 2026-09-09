package bunrepo

import (
	"context"
	"errors"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/infra/projectschema"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errEmptyProjectID = status.Error(codes.InvalidArgument, "project_id is required")

func mapIdentError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ident.ErrInvalidSchemaResourceID) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return err
}

// ProjectTable 返回项目数据面 schema 的 bun.Ident 与 ModelTableExpr 模板
// （"?." + table + " AS " + alias）。禁止未限定表名。
func ProjectTable(projectID, table, alias string) (bun.Ident, string, error) {
	schema, err := ident.ProjectSchemaName(projectID)
	if err != nil {
		return bun.Ident(""), "", mapIdentError(err)
	}
	expr := "?." + table
	if alias != "" {
		expr += " AS " + alias
	}
	return bun.Ident(schema), expr, nil
}

// ProjectQuoted 返回 quoteIdent 后的项目 schema 名，供 Raw SQL。
func ProjectQuoted(projectID string) (string, error) {
	schema, err := ident.ProjectSchemaName(projectID)
	if err != nil {
		return "", mapIdentError(err)
	}
	return `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`, nil
}

// Scoped 在项目 schema 上打开 bun 连接：Apply + ModelTableExpr 模板。
// projectID 为空时直接失败（禁止扫 public / 全项目）。
func Scoped(ctx context.Context, db *clients.Database, projectID, table, alias string) (bun.IDB, bun.Ident, string, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, bun.Ident(""), "", errEmptyProjectID
	}
	if err := projectschema.Apply(ctx, db, projectID); err != nil {
		return nil, bun.Ident(""), "", mapIdentError(err)
	}
	sch, expr, err := ProjectTable(projectID, table, alias)
	if err != nil {
		return nil, bun.Ident(""), "", err
	}
	return db.Conn(ctx), sch, expr, nil
}
