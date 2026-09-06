package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/lynx-go/commands"

	"github.com/torchwooddev/torchwood/internal/infra/documentdb"
)

// newAdminSchemaCmd 提供 schema 漂移对账命令（转出 POC 门禁 B3，redesign
// §4.4）：`torchwood admin schema repair [--dry-run]`——扫描三类漂移
// （缺列 / INVALID·failed 索引 / 幽灵表）并修复；--dry-run 只报告 diff 不落
// DDL。逻辑与 server 启动钩子的后台 reconcile 同源（documentdb.
// ReconcileSchemaDrift），CLI 形态对齐 admin export/import（直连 DB）。
func newAdminSchemaCmd() *group {
	return newGroup(nil, "schema", "schema 漂移对账（缺列 / INVALID 索引 / 幽灵表，B3）", func(sub *commands.App) {
		sub.Register(newAdminSchemaRepairCmd())
	})
}

func newAdminSchemaRepairCmd() *verb {
	var dsn string
	var dryRun bool
	return newVerb(nil, "repair", "扫描并修复 schema 漂移（--dry-run 只报告不修复）", "admin schema repair [--dry-run]",
		func(fs *flag.FlagSet) {
			fs.BoolVar(&dryRun, "dry-run", false, "只报告漂移 diff，不执行修复 DDL")
			fs.StringVar(&dsn, "dsn", os.Getenv(adminDBFlagDsn), "Postgres DSN（缺省读 "+adminDBFlagDsn+"）")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			db, closeDB, err := openAdminProjectDB(dsn)
			if err != nil {
				return err
			}
			defer closeDB()
			report, err := documentdb.ReconcileSchemaDrift(context.Background(), db, documentdb.SchemaReconcileOptions{
				DryRun: dryRun,
			})
			if err != nil {
				return err
			}
			out, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintf(env.Stderr, "漂移扫描（dry-run，未修复）：%d 集合 / %d 项检出 / %d 失败\n",
					report.Scanned, len(report.Items), report.Failed)
			} else {
				fmt.Fprintf(env.Stderr, "漂移修复完成：%d 集合 / %d 项修复 / %d 失败\n",
					report.Scanned, report.Fixed, report.Failed)
			}
			return printJSON(env.Stdout, out)
		})
}
