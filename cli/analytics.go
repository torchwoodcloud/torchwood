package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/lynx-go/commands"
)

const (
	methodAnalyticsIngest      = "/torchwood.server.v1.AnalyticsService/IngestEvents"
	methodAnalyticsOverview    = "/torchwood.server.v1.AnalyticsService/GetOverview"
	methodAnalyticsDefinitions = "/torchwood.server.v1.AnalyticsService/ListEventDefinitions"
	methodAnalyticsTimeseries  = "/torchwood.server.v1.AnalyticsService/QueryTimeseries"
	methodAnalyticsBreakdown   = "/torchwood.server.v1.AnalyticsService/QueryBreakdown"
	methodAnalyticsRetention   = "/torchwood.server.v1.AnalyticsService/QueryRetention"
	methodAnalyticsUserEvents  = "/torchwood.server.v1.AnalyticsService/ListUserEvents"
)

// newAnalyticsCmd 覆盖 AnalyticsService 全部 7 个方法：服务端权威事件摄入
// （ingest，analytics.write）与固定形状查询（overview/events/timeseries/
// breakdown/retention/user-events，analytics.read）。窗口护栏（HOUR ≤7 天 /
// DAY ≤366 天 / breakdown ≤30 天 / retention cohort ≤92 天 / user-events
// ≤92 天）由服务端执行。
func newAnalyticsCmd(g *GlobalFlags) *group {
	return newGroup(g, "analytics", "event analytics: server-side ingest (analytics.write) and fixed-shape queries (analytics.read)", func(sub *commands.App) {
		sub.Register(
			newAnalyticsIngestCmd(g),
			newAnalyticsOverviewCmd(g),
			newAnalyticsEventsCmd(g),
			newAnalyticsTimeseriesCmd(g),
			newAnalyticsBreakdownCmd(g),
			newAnalyticsRetentionCmd(g),
			newAnalyticsUserEventsCmd(g),
		)
	})
}

func newAnalyticsIngestCmd(g *GlobalFlags) *verb {
	var file string
	return newVerb(g, "ingest", "ingest a batch of server-authoritative events (analytics.write; 1-100 per batch, bad events are skipped not rejected)", "analytics ingest --file <path | ->",
		func(fs *flag.FlagSet) {
			fs.StringVar(&file, "file", "", `JSON payload: a bare event array or a {"events": [...]} object ('-' reads stdin); event fields (camelCase): name (required), occurredAt (RFC3339), props, sessionId, userId`)
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			if file == "" {
				return fmt.Errorf("--file is required (path or '-' for stdin)")
			}
			req, err := buildIngestReq(file)
			if err != nil {
				return err
			}
			return call(g, env, methodAnalyticsIngest, req)
		})
}

// buildIngestReq 读入 --file 载荷并构造 IngestServerEventsRequest。
func buildIngestReq(path string) (map[string]any, error) {
	var content []byte
	var err error
	if path == "-" {
		content, err = io.ReadAll(os.Stdin)
	} else {
		content, err = os.ReadFile(path) // #nosec G304 -- path from --file flag
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read --file: %v", err)
	}
	return ingestEventsPayload(content)
}

// ingestEventsPayload 解析载荷：裸事件数组包一层 events 键；对象原样透传
// （须含 events）。形状与批量上限校验归服务端 protovalidate。JSONL（一行一
// 事件）会在这里被静默截断成首个事件，必须显式拒绝。
func ingestEventsPayload(content []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("failed to parse ingest payload: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf(`ingest payload must be a single JSON array or object (one event per line is not supported; wrap the events in an array)`)
	}
	switch t := v.(type) {
	case []any:
		return map[string]any{"events": t}, nil
	case map[string]any:
		return t, nil
	default:
		return nil, fmt.Errorf(`ingest payload must be a JSON array of events or an {"events": [...]} object`)
	}
}

func newAnalyticsOverviewCmd(g *GlobalFlags) *verb {
	var from, to string
	return newVerb(g, "overview", "window KPIs + top events + today's real-time counts (source: rollup|raw)", "analytics overview --from <ts> --to <ts>",
		func(fs *flag.FlagSet) {
			registerWindowFlags(fs, &from, &to)
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			if err := requireWindow(from, to); err != nil {
				return err
			}
			return call(g, env, methodAnalyticsOverview, map[string]any{"periodStart": tsJSON(from), "periodEnd": tsJSON(to)})
		})
}

// newAnalyticsEventsCmd: analytics events list —— 事件字典（自由上报 +
// 事后发现）。
func newAnalyticsEventsCmd(g *GlobalFlags) *group {
	return newGroup(g, "events", "event dictionary (definitions discovered from ingestion)", func(sub *commands.App) {
		sub.Register(newAnalyticsEventsListCmd(g))
	})
}

func newAnalyticsEventsListCmd(g *GlobalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "list event definitions (name, first/last seen, 30d total)", "analytics events list [--page-size <n>] [--page-token <t>]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (0-500, server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next page token from the previous response")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			return call(g, env, methodAnalyticsDefinitions, listJSON(pageSize, pageToken))
		})
}

func newAnalyticsTimeseriesCmd(g *GlobalFlags) *verb {
	var names, from, to, granularity string
	return newVerb(g, "timeseries", "event counts over time (hour → raw, day → rollup)", "analytics timeseries [--names '<json array>'] --from <ts> --to <ts> --granularity <hour|day>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&names, "names", "", `event names as a JSON array (up to 10); omit for all events aggregated`)
			registerWindowFlags(fs, &from, &to)
			fs.StringVar(&granularity, "granularity", "", "bucket size: hour | day (required)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			if err := requireWindow(from, to); err != nil {
				return err
			}
			gran, err := granularityJSON(granularity)
			if err != nil {
				return err
			}
			req := map[string]any{"periodStart": tsJSON(from), "periodEnd": tsJSON(to), "granularity": gran}
			if names != "" {
				list, err := jsonStringList(names, "--names")
				if err != nil {
					return err
				}
				req["names"] = list
			}
			return call(g, env, methodAnalyticsTimeseries, req)
		})
}

// granularityJSON 把 hour|day 映射为 protojson 接受的枚举名（全名原样透传）。
func granularityJSON(s string) (string, error) {
	switch s {
	case "hour":
		return "ANALYTICS_GRANULARITY_HOUR", nil
	case "day":
		return "ANALYTICS_GRANULARITY_DAY", nil
	case "ANALYTICS_GRANULARITY_HOUR", "ANALYTICS_GRANULARITY_DAY":
		return s, nil
	default:
		return "", fmt.Errorf("invalid --granularity %q: use hour or day", s)
	}
}

func newAnalyticsBreakdownCmd(g *GlobalFlags) *verb {
	var name, propKey, from, to string
	var topN int
	return newVerb(g, "breakdown", "break an event down by a property key (raw, top-N + __other__, window ≤30 days)", "analytics breakdown --name <event> --prop-key <key> --from <ts> --to <ts> [--top-n <n>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "event name (required)")
			fs.StringVar(&propKey, "prop-key", "", "property key to group by (validated server-side against an allowlist to prevent injection)")
			registerWindowFlags(fs, &from, &to)
			fs.IntVar(&topN, "top-n", 0, "top-N buckets (0-50, remaining merged into __other__; default 20)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			if name == "" || propKey == "" {
				return fmt.Errorf("--name and --prop-key are required")
			}
			if err := requireWindow(from, to); err != nil {
				return err
			}
			req := map[string]any{"name": name, "propKey": propKey, "periodStart": tsJSON(from), "periodEnd": tsJSON(to)}
			if topN > 0 {
				req["topN"] = topN
			}
			return call(g, env, methodAnalyticsBreakdown, req)
		})
}

func newAnalyticsRetentionCmd(g *GlobalFlags) *verb {
	var from, to string
	return newVerb(g, "retention", "retention matrix (cohort × D0-D14, day granularity UTC; cohort window ≤92 days)", "analytics retention --from <ts> --to <ts>",
		func(fs *flag.FlagSet) {
			registerWindowFlags(fs, &from, &to)
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			if err := requireWindow(from, to); err != nil {
				return err
			}
			return call(g, env, methodAnalyticsRetention, map[string]any{"cohortStart": tsJSON(from), "cohortEnd": tsJSON(to)})
		})
}

func newAnalyticsUserEventsCmd(g *GlobalFlags) *verb {
	var from, to, pageToken string
	var pageSize int
	return newVerb(g, "user-events", "drill into one user's event trail (raw, newest first, keyset pagination, window ≤92 days)", "analytics user-events <user-id> [--page-size <n>] [--page-token <t>] [--from <ts>] [--to <ts>]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (0-100, server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "opaque cursor from the previous page")
			fs.StringVar(&from, "from", "", "optional window start (RFC3339 or YYYY-MM-DD)")
			fs.StringVar(&to, "to", "", "optional window end")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req := map[string]any{"userId": args[0]}
			if pageSize > 0 {
				req["pageSize"] = pageSize
			}
			if pageToken != "" {
				req["pageToken"] = pageToken
			}
			if from != "" {
				req["periodStart"] = tsJSON(from)
			}
			if to != "" {
				req["periodEnd"] = tsJSON(to)
			}
			return call(g, env, methodAnalyticsUserEvents, req)
		})
}

// registerWindowFlags 挂 --from/--to（分析查询的公共窗口旗标）。
func registerWindowFlags(fs *flag.FlagSet, from, to *string) {
	fs.StringVar(from, "from", "", "window start (RFC3339, or YYYY-MM-DD = UTC midnight)")
	fs.StringVar(to, "to", "", "window end (same formats)")
}

// requireWindow 校验必填时间窗。
func requireWindow(from, to string) error {
	if from == "" || to == "" {
		return fmt.Errorf("--from and --to are required (RFC3339, or YYYY-MM-DD = UTC midnight)")
	}
	return nil
}

// tsJSON 归一时间旗标：纯日期（YYYY-MM-DD）补为 UTC 零点 RFC3339，其余原样
// 透传（格式合法性由 protojson/服务端校验）。
func tsJSON(s string) string {
	if len(s) == 10 && s[4] == '-' && s[7] == '-' {
		return s + "T00:00:00Z"
	}
	return s
}
