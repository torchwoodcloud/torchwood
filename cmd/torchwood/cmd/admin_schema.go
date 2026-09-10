package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/lynx-go/commands"

	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
)

// newAdminSchemaCmd 提供 schema 漂移对账命令（转出 POC 门禁 B3，redesign
// §4.4）：`torchwood admin schema repair [--dry-run]`——扫描三类漂移
// （缺列 / INVALID·failed 索引 / 幽灵表）并修复；--dry-run 只报告 diff 不落
// DDL。逻辑与 server 启动钩子的后台 reconcile 同源（documentdb.
// ReconcileSchemaDrift），CLI 形态对齐 admin export/import（直连 DB）。
func newAdminSchemaCmd() *group {
	return newGroup(nil, "schema", "schema drift reconciliation (missing columns / INVALID indexes / ghost tables, B3)", func(sub *commands.App) {
		sub.Register(newAdminSchemaRepairCmd())
	})
}

func newAdminSchemaRepairCmd() *verb {
	var dsn string
	var dryRun bool
	return newVerb(nil, "repair", "scan and repair schema drift (--dry-run reports without repairing)", "admin schema repair [--dry-run]",
		func(fs *flag.FlagSet) {
			fs.BoolVar(&dryRun, "dry-run", false, "report the drift diff only, without executing repair DDL")
			fs.StringVar(&dsn, "dsn", os.Getenv(adminDBFlagDsn), "Postgres DSN (defaults to "+adminDBFlagDsn+")")
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
				fmt.Fprintf(env.Stderr, "drift scan (dry-run, not repaired): %d collections / %d items detected / %d failed\n",
					report.Scanned, len(report.Items), report.Failed)
			} else {
				fmt.Fprintf(env.Stderr, "drift repair finished: %d collections / %d items fixed / %d failed\n",
					report.Scanned, report.Fixed, report.Failed)
			}
			return printJSON(env.Stdout, out)
		})
}
