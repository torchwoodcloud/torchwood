package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lynx-go/commands"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
)

// adminDBFlagDsn 是直连 DSN 的环境变量缺省来源（与运行态一致；POC 工具
// 属性允许 admin 子命令直连 DB，不经 API 面——export/import 需要平台旁路
// 身份与 catalog 直读，走 Server RPC 需要新增 proto + 授权面，不划算）。
const adminDBFlagDsn = "TORCHWOOD_DATA_DATABASE_SOURCE"

// openAdminProjectDB 以 DSN 直开元数据库连接（B5 export/import 专用）。
// 参数形态与运行态 newDatabase 对齐（2MiB 写缓冲 + 60s 读超时，容纳导出
// 长查询/导入批量写），调用方负责 Close。
func openAdminProjectDB(dsn string) (*clients.Database, func(), error) {
	source := strings.TrimSpace(dsn)
	if source == "" {
		return nil, nil, fmt.Errorf("database source is empty: pass --dsn or set %s", adminDBFlagDsn)
	}
	u, err := url.Parse(source)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid database source: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, nil, fmt.Errorf("invalid database scheme %q: expected postgres", u.Scheme)
	}
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(source),
		pgdriver.WithBufferSize(2<<20),
		pgdriver.WithDialTimeout(5*time.Second),
		pgdriver.WithReadTimeout(60*time.Second),
		pgdriver.WithWriteTimeout(10*time.Second),
		pgdriver.WithApplicationName("torchwood-admin"),
	))
	db := &clients.Database{DB: bun.NewDB(sqldb, pgdialect.New())}
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("database ping failed: %w", err)
	}
	return db, func() { _ = db.Close() }, nil
}

func newAdminExportCmd() *verb {
	var projectID, outDir, dsn string
	return newVerb(nil, "export", "导出项目文档面（catalog 快照 + 集合 NDJSON + snapshot_seq，转出 POC B5）", "admin export --project <id> --out <dir>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&projectID, "project", "", "项目 ID（必填）")
			fs.StringVar(&outDir, "out", "", "导出目录（必填，写入 manifest.json 与 data/*.ndjson）")
			fs.StringVar(&dsn, "dsn", os.Getenv(adminDBFlagDsn), "Postgres DSN（缺省读 "+adminDBFlagDsn+"）")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			if projectID == "" || outDir == "" {
				return fmt.Errorf("--project 与 --out 必填")
			}
			db, closeDB, err := openAdminProjectDB(dsn)
			if err != nil {
				return err
			}
			defer closeDB()
			manifest, err := documentdb.ExportProject(context.Background(), db, projectID, outDir)
			if err != nil {
				return err
			}
			out, err := json.MarshalIndent(manifest, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintf(env.Stderr, "导出完成：%d 库 / %d 集合 → %s\n", len(manifest.Databases), len(manifest.Collections), outDir)
			fmt.Fprintf(env.Stderr, "snapshot_seq=%d；增量续接：:changes?since_seq=%d\n", manifest.SnapshotSeq, manifest.SnapshotSeq)
			return printJSON(env.Stdout, out)
		})
}

func newAdminImportCmd() *verb {
	var projectID, inDir, dsn string
	return newVerb(nil, "import", "导入项目文档面（catalog 重建 + 行保真导入，转出 POC B5）", "admin import --project <id> --in <dir>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&projectID, "project", "", "项目 ID（必填，须与导出时一致）")
			fs.StringVar(&inDir, "in", "", "导入目录（必填，须含 manifest.json）")
			fs.StringVar(&dsn, "dsn", os.Getenv(adminDBFlagDsn), "Postgres DSN（缺省读 "+adminDBFlagDsn+"）")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			if projectID == "" || inDir == "" {
				return fmt.Errorf("--project 与 --in 必填")
			}
			db, closeDB, err := openAdminProjectDB(dsn)
			if err != nil {
				return err
			}
			defer closeDB()
			report, err := documentdb.ImportProject(context.Background(), db, projectID, inDir)
			if err != nil {
				return err
			}
			out, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintf(env.Stderr, "导入完成：%d 库 / %d 集合 / %d 行\n",
				len(report.DatabasesRestored), len(report.CollectionsRestored), report.RowsImported)
			fmt.Fprintln(env.Stderr, report.ResumeHint)
			return printJSON(env.Stdout, out)
		})
}
