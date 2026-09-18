package cli

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

const (
	methodLeaderboardsSubmit         = "/torchwood.server.v1.LeaderboardsService/SubmitLeaderboardScore"
	methodLeaderboardsGetEntry       = "/torchwood.server.v1.LeaderboardsService/GetLeaderboardEntry"
	methodLeaderboardsTop            = "/torchwood.server.v1.LeaderboardsService/ListLeaderboardTop"
	methodLeaderboardsSettlementGet  = "/torchwood.server.v1.LeaderboardsService/GetLeaderboardSettlement"
	methodLeaderboardsSettlementList = "/torchwood.server.v1.LeaderboardsService/ListLeaderboardSettlements"
	methodLeaderboardsBoardCreate    = "/torchwood.server.v1.LeaderboardsService/CreateLeaderboardBoard"
	methodLeaderboardsBoardGet       = "/torchwood.server.v1.LeaderboardsService/GetLeaderboardBoard"
	methodLeaderboardsBoardsList     = "/torchwood.server.v1.LeaderboardsService/ListLeaderboardBoards"
	methodLeaderboardsBoardPeriods   = "/torchwood.server.v1.LeaderboardsService/ListLeaderboardBoardPeriods"
	methodLeaderboardsBoardUpdate    = "/torchwood.server.v1.LeaderboardsService/UpdateLeaderboardBoard"
)

// newLeaderboardsCmd 覆盖 LeaderboardsService 全部 10 个方法：提交与快照/榜读
// （submit/get-entry/top）、结算两读（settlements get/list）、board 配置管控
// （boards create/get/list/periods/update）。scope 三档：读 leaderboards.read、
// 提交 leaderboards.write、board 写 leaderboards.admin——能提交分值的密钥
// 不得改榜配置（Delete 不在 server 面，留给 console owner）。
func newLeaderboardsCmd(g *GlobalFlags) *group {
	return newGroup(g, "leaderboards", "leaderboard score submit/read, settlements, board config (reads: leaderboards.read; submit: leaderboards.write; board create/update: leaderboards.admin)", func(sub *commands.App) {
		sub.Register(
			newLeaderboardsSubmitCmd(g),
			newLeaderboardsGetEntryCmd(g),
			newLeaderboardsTopCmd(g),
			newLeaderboardsSettlementsCmd(g),
			newLeaderboardsBoardsCmd(g),
		)
	})
}

func newLeaderboardsSubmitCmd(g *GlobalFlags) *verb {
	var value, tiebreakValue int64
	var period, requestID string
	return newVerb(g, "submit", "submit a score on behalf of a subject (leaderboards.write)", "leaderboards submit --value <n> [--tiebreak-value <n>] [--period <p>] [--request-id <id>] <board-id> <subject-id>",
		func(fs *flag.FlagSet) {
			fs.Int64Var(&value, "value", 0, "score value (required; pass explicitly even for 0)")
			fs.Int64Var(&tiebreakValue, "tiebreak-value", 0, "tiebreak column value (direction must match the board's tiebreak declaration)")
			fs.StringVar(&period, "period", "", "target period (defaults to the current period; the previous period is accepted for offline catch-up)")
			fs.StringVar(&requestID, "request-id", "", "idempotency key (deduped per project+actor+request_id for 24h; retries don't burn the per-subject rate limit)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req, err := buildSubmitLeaderboardReq(v, args[0], args[1], value, tiebreakValue, period, requestID)
			if err != nil {
				return err
			}
			return call(g, env, methodLeaderboardsSubmit, req)
		})
}

// buildSubmitLeaderboardReq 构造 SubmitLeaderboardScoreRequest；value 为 0 也
// 是合法分值（参与即得分），故必填用旗标 presence 判断；tiebreak_value 是
// proto3 optional，同样用 presence 表达。
func buildSubmitLeaderboardReq(v *verb, boardID, subjectID string, value, tiebreakValue int64, period, requestID string) (map[string]any, error) {
	if boardID == "" || subjectID == "" {
		return nil, fmt.Errorf("missing board-id/subject-id positional arguments")
	}
	if !v.changed("value") {
		return nil, fmt.Errorf("--value is required")
	}
	req := map[string]any{"boardId": boardID, "subjectId": subjectID, "value": value}
	setChanged(v, "tiebreak-value", req, "tiebreakValue", tiebreakValue)
	if period != "" {
		req["period"] = period
	}
	if requestID != "" {
		req["requestId"] = requestID
	}
	return req, nil
}

func newLeaderboardsGetEntryCmd(g *GlobalFlags) *verb {
	var period string
	return newVerb(g, "get-entry", "get a subject's score snapshot in a board", "leaderboards get-entry <board-id> <subject-id> [--period <p>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&period, "period", "", "period (defaults to the current period)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			req := map[string]any{"boardId": args[0], "subjectId": args[1]}
			if period != "" {
				req["period"] = period
			}
			return call(g, env, methodLeaderboardsGetEntry, req)
		})
}

func newLeaderboardsTopCmd(g *GlobalFlags) *verb {
	var period, pageToken string
	var pageSize int
	return newVerb(g, "top", "list a board's top entries", "leaderboards top <board-id> [--period <p>] [--page-size <n>] [--page-token <t>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&period, "period", "", "period (defaults to the current period)")
			fs.IntVar(&pageSize, "page-size", 0, "page size (0-100, server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req := map[string]any{"boardId": args[0]}
			if period != "" {
				req["period"] = period
			}
			if pageSize > 0 {
				req["pageSize"] = pageSize
			}
			if pageToken != "" {
				req["pageToken"] = pageToken
			}
			return call(g, env, methodLeaderboardsTop, req)
		})
}

// newLeaderboardsSettlementsCmd: leaderboards settlements get <board> <period> /
// list <board>。发奖明细为只读投影（写操作只有 worker 与 console）。
func newLeaderboardsSettlementsCmd(g *GlobalFlags) *group {
	return newGroup(g, "settlements", "settlement reads (per-period grant details)", func(sub *commands.App) {
		sub.Register(
			newVerb(g, "get", "get one period's settlement and its grants", "leaderboards settlements get <board-id> <period>", nil,
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 2); err != nil {
						return err
					}
					return call(g, env, methodLeaderboardsSettlementGet, map[string]any{"boardId": args[0], "period": args[1]})
				}),
			newLeaderboardsSettlementListCmd(g),
		)
	})
}

func newLeaderboardsSettlementListCmd(g *GlobalFlags) *verb {
	var limit int
	return newVerb(g, "list", "list a board's settled periods", "leaderboards settlements list <board-id> [--limit <n>]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&limit, "limit", 0, "max settlements to return (0-200, server default when omitted)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req := map[string]any{"boardId": args[0]}
			if limit > 0 {
				req["limit"] = limit
			}
			return call(g, env, methodLeaderboardsSettlementList, req)
		})
}

// newLeaderboardsBoardsCmd: board 配置管控。create 幂等（配置逐字段相等 →
// 200 + 现状；不等 → 409 附字段 diff）；update 为 proto3 optional 语义。
func newLeaderboardsBoardsCmd(g *GlobalFlags) *group {
	return newGroup(g, "boards", "board config control (create/update need leaderboards.admin; rewards are console-owner only)", func(sub *commands.App) {
		sub.Register(
			newLeaderboardsBoardCreateCmd(g),
			newLeaderboardsBoardGetCmd(g),
			newLeaderboardsBoardsListCmd(g),
			newLeaderboardsBoardPeriodsCmd(g),
			newLeaderboardsBoardUpdateCmd(g),
		)
	})
}

// boardConfig 聚合 board 配置旗标（create/update 共用形状）。
type boardConfig struct {
	sort, tiebreakOrder, tieBreak string
	periodKind, periodTZ          string
	policy, subjectKind           string
	valueMin, valueMax            int64
	clientSubmit                  bool
	perSubjectSubmitLimit         int
	retentionPeriods              int
}

func registerBoardConfigFlags(fs *flag.FlagSet, c *boardConfig) {
	fs.StringVar(&c.sort, "sort", "", "score sort: asc | desc (default desc)")
	fs.StringVar(&c.tiebreakOrder, "tiebreak-order", "", "tiebreak direction: asc | desc (declares the single-column tiebreak)")
	fs.StringVar(&c.tieBreak, "tie-break", "", "tie-break policy: parallel | earliest | latest (default parallel)")
	fs.StringVar(&c.periodKind, "period-kind", "", "reset period: none | daily | weekly | monthly (default none)")
	fs.StringVar(&c.periodTZ, "period-tz", "", "IANA timezone for period boundaries")
	fs.StringVar(&c.policy, "policy", "", "aggregation over multiple submissions: best | latest | sum (default best)")
	fs.Int64Var(&c.valueMin, "value-min", 0, "reject scores below this value")
	fs.Int64Var(&c.valueMax, "value-max", 0, "reject scores above this value")
	fs.BoolVar(&c.clientSubmit, "client-submit", false, "allow client-side (client SDK) submissions")
	fs.IntVar(&c.perSubjectSubmitLimit, "per-subject-submit-limit", 0, "submissions per subject per period (1-10000, default 100)")
	fs.IntVar(&c.retentionPeriods, "retention-periods", 0, "settled periods to retain")
	fs.StringVar(&c.subjectKind, "subject-kind", "", "subject kind annotation (e.g. user | team)")
}

func newLeaderboardsBoardCreateCmd(g *GlobalFlags) *verb {
	var c boardConfig
	return newVerb(g, "create", "idempotently create a board (leaderboards.admin): existing with identical config → 200 + current state; differs → 409 with field diff", "leaderboards boards create <board-id> [--sort asc|desc] [--policy best|latest|sum] [--period-kind none|daily|weekly|monthly] [...]",
		func(fs *flag.FlagSet) {
			registerBoardConfigFlags(fs, &c)
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildCreateBoardReq(args[0], c)
			if err != nil {
				return err
			}
			return call(g, env, methodLeaderboardsBoardCreate, req)
		})
}

// buildCreateBoardReq 构造 CreateLeaderboardBoardRequest：仅放非零配置（零值
// = 服务端缺省归一，重放比较语义等价；value_min 允许负数下界）。
func buildCreateBoardReq(id string, c boardConfig) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("missing board-id positional argument")
	}
	req := map[string]any{"id": id}
	for key, val := range map[string]string{
		"sort": c.sort, "tiebreakOrder": c.tiebreakOrder, "tieBreak": c.tieBreak,
		"periodKind": c.periodKind, "periodTz": c.periodTZ,
		"policy": c.policy, "subjectKind": c.subjectKind,
	} {
		if val != "" {
			req[key] = val
		}
	}
	if c.valueMin != 0 {
		req["valueMin"] = c.valueMin
	}
	if c.valueMax != 0 {
		req["valueMax"] = c.valueMax
	}
	if c.clientSubmit {
		req["clientSubmit"] = true
	}
	if c.perSubjectSubmitLimit != 0 {
		req["perSubjectSubmitLimit"] = c.perSubjectSubmitLimit
	}
	if c.retentionPeriods != 0 {
		req["retentionPeriods"] = c.retentionPeriods
	}
	return req, nil
}

func newLeaderboardsBoardGetCmd(g *GlobalFlags) *verb {
	return newVerb(g, "get", "get a board's config (publish-gate primary read)", "leaderboards boards get <board-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodLeaderboardsBoardGet, map[string]any{"boardId": args[0]})
		})
}

func newLeaderboardsBoardsListCmd(g *GlobalFlags) *verb {
	return newVerb(g, "list", "list all boards (no pagination, ≤100 per project)", "leaderboards boards list", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			return call(g, env, methodLeaderboardsBoardsList, map[string]any{})
		})
}

func newLeaderboardsBoardPeriodsCmd(g *GlobalFlags) *verb {
	var limit int
	return newVerb(g, "periods", "list a board's period keys (newest first)", "leaderboards boards periods <board-id> [--limit <n>]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&limit, "limit", 0, "max period keys to return (0-200, server default when omitted)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req := map[string]any{"boardId": args[0]}
			if limit > 0 {
				req["limit"] = limit
			}
			return call(g, env, methodLeaderboardsBoardPeriods, req)
		})
}

func newLeaderboardsBoardUpdateCmd(g *GlobalFlags) *verb {
	var c boardConfig
	var clearTiebreak, clearValueBounds bool
	return newVerb(g, "update", "update a board (leaderboards.admin; only explicitly passed fields — pass --client-submit=true/false explicitly to take effect)", "leaderboards boards update <board-id> [--sort] [--tiebreak-order] [--tie-break] [--period-kind] [--period-tz] [--policy] [--value-min] [--value-max] [--client-submit] [--per-subject-submit-limit] [--retention-periods] [--subject-kind] [--clear-tiebreak] [--clear-value-bounds]",
		func(fs *flag.FlagSet) {
			registerBoardConfigFlags(fs, &c)
			fs.BoolVar(&clearTiebreak, "clear-tiebreak", false, "remove the tiebreak declaration")
			fs.BoolVar(&clearValueBounds, "clear-value-bounds", false, "remove the value_min/value_max bounds")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildUpdateBoardReq(v, args[0], c, clearTiebreak, clearValueBounds)
			if err != nil {
				return err
			}
			return call(g, env, methodLeaderboardsBoardUpdate, req)
		})
}

// buildUpdateBoardReq 构造 UpdateLeaderboardBoardRequest（proto3 optional：
// 未显式传入 = 不修改，presence 用旗标显式设置表达）。period/sort/tiebreak
// 声明/policy/tie_break 在榜内已有条目时服务端拒绝改（FailedPrecondition）。
func buildUpdateBoardReq(v *verb, id string, c boardConfig, clearTiebreak, clearValueBounds bool) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("missing board-id positional argument")
	}
	req := map[string]any{"boardId": id}
	setChanged(v, "sort", req, "sort", c.sort)
	setChanged(v, "tiebreak-order", req, "tiebreakOrder", c.tiebreakOrder)
	if clearTiebreak {
		req["clearTiebreak"] = true
	}
	setChanged(v, "tie-break", req, "tieBreak", c.tieBreak)
	setChanged(v, "period-kind", req, "periodKind", c.periodKind)
	setChanged(v, "period-tz", req, "periodTz", c.periodTZ)
	setChanged(v, "policy", req, "policy", c.policy)
	setChanged(v, "value-min", req, "valueMin", c.valueMin)
	setChanged(v, "value-max", req, "valueMax", c.valueMax)
	if clearValueBounds {
		req["clearValueBounds"] = true
	}
	setChanged(v, "client-submit", req, "clientSubmit", c.clientSubmit)
	setChanged(v, "per-subject-submit-limit", req, "perSubjectSubmitLimit", c.perSubjectSubmitLimit)
	setChanged(v, "retention-periods", req, "retentionPeriods", c.retentionPeriods)
	setChanged(v, "subject-kind", req, "subjectKind", c.subjectKind)
	return req, nil
}
