package cmd

import (
	"flag"

	"github.com/lynx-go/commands"
)

const (
	methodBillingUsage      = "/torchwood.server.v1.BillingService/GetUsage"
	methodBillingRollups    = "/torchwood.server.v1.BillingService/ListRollups"
	methodBillingStatements = "/torchwood.server.v1.BillingService/ListStatements"
)

// newBillingCmd 覆盖 BillingService 全部 3 个方法（当前用量 / 小时 rollup /
// 月账单文档），全部只读（billing.read）。一期不出票、不收款。
func newBillingCmd(g *globalFlags) *group {
	return newGroup(g, "billing", "platform usage queries: current usage, hourly rollups, monthly statements (read-only: billing.read)", func(sub *commands.App) {
		sub.Register(
			newBillingUsageCmd(g),
			newBillingRollupsCmd(g),
			newBillingStatementsCmd(g),
		)
	})
}

// newBillingUsageCmd: billing usage [--metric] [--from] [--to]——period 缺省 =
// 当前 UTC 月（服务端归一）。
func newBillingUsageCmd(g *globalFlags) *verb {
	var metric, from, to string
	return newVerb(g, "usage", "aggregated usage per metric within a window (defaults to the current UTC month)", "billing usage [--metric <m>] [--from <ts>] [--to <ts>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&metric, "metric", "", "metric (api_calls | storage_bytes | function_duration_ms; omit for all)")
			fs.StringVar(&from, "from", "", "window start (RFC3339, or YYYY-MM-DD = UTC midnight)")
			fs.StringVar(&to, "to", "", "window end")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			req := map[string]any{}
			if metric != "" {
				req["metric"] = metric
			}
			if from != "" {
				req["periodStart"] = tsJSON(from)
			}
			if to != "" {
				req["periodEnd"] = tsJSON(to)
			}
			return call(g, env, methodBillingUsage, req)
		})
}

func newBillingRollupsCmd(g *globalFlags) *verb {
	var metric, from, to, pageToken string
	var pageSize int
	return newVerb(g, "rollups", "list hourly usage rollups (period_start DESC)", "billing rollups [--metric <m>] [--from <ts>] [--to <ts>] [--page-size <n>] [--page-token <t>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&metric, "metric", "", "metric (api_calls | storage_bytes | function_duration_ms; omit for all)")
			fs.StringVar(&from, "from", "", "window start (RFC3339, or YYYY-MM-DD = UTC midnight)")
			fs.StringVar(&to, "to", "", "window end")
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next page token from the previous response")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			req := listJSON(pageSize, pageToken)
			if metric != "" {
				req["metric"] = metric
			}
			if from != "" {
				req["periodStart"] = tsJSON(from)
			}
			if to != "" {
				req["periodEnd"] = tsJSON(to)
			}
			return call(g, env, methodBillingRollups, req)
		})
}

func newBillingStatementsCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "statements", "list monthly billing statements (draft → final)", "billing statements [--page-size <n>] [--page-token <t>]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next page token from the previous response")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			return call(g, env, methodBillingStatements, listJSON(pageSize, pageToken))
		})
}
