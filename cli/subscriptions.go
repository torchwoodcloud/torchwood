package cli

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

const (
	methodSubsPlanCreate = "/torchwood.server.v1.SubscriptionsService/CreatePlan"
	methodSubsPlanList   = "/torchwood.server.v1.SubscriptionsService/ListPlans"
	methodSubsPlanGet    = "/torchwood.server.v1.SubscriptionsService/GetPlan"
	methodSubsPlanUpdate = "/torchwood.server.v1.SubscriptionsService/UpdatePlan"
	methodSubsPlanDelete = "/torchwood.server.v1.SubscriptionsService/DeletePlan"
	methodSubsList       = "/torchwood.server.v1.SubscriptionsService/ListSubscriptions"
	methodSubsGet        = "/torchwood.server.v1.SubscriptionsService/GetSubscription"
	methodSubsCancel     = "/torchwood.server.v1.SubscriptionsService/CancelSubscription"
	methodSubsExpire     = "/torchwood.server.v1.SubscriptionsService/ExpireSubscription"
)

// newSubscriptionsCmd 覆盖 SubscriptionsService 全部 9 个方法：plan 配置
// （plans create/list/get/update/delete）+ 订阅生命周期（list/get/cancel/
// expire）。读 subscriptions.read；写 subscriptions.write——Cancel/Expire
// 服务端强制 admin 角色（普通 member 密钥被 plan 写放行、生命周期写拒绝）。
func newSubscriptionsCmd(g *GlobalFlags) *group {
	return newGroup(g, "subscriptions", "subscription plans and lifecycle (reads: subscriptions.read; writes: subscriptions.write, cancel/expire need admin)", func(sub *commands.App) {
		sub.Register(
			newSubsPlansCmd(g),
			newSubsListCmd(g),
			newSubsGetCmd(g),
			newSubsCancelCmd(g),
			newSubsExpireCmd(g),
		)
	})
}

// newSubsPlansCmd: plan 配置管控。amount 一律最小货币单位 int64；benefits
// 形状见 --benefits 旗标帮助。
func newSubsPlansCmd(g *GlobalFlags) *group {
	return newGroup(g, "plans", "subscription plan config (amounts in minimal currency units)", func(sub *commands.App) {
		sub.Register(
			newSubsPlanCreateCmd(g),
			newSubsPlanListCmd(g),
			newSubsPlanGetCmd(g),
			newSubsPlanUpdateCmd(g),
			newSubsPlanDeleteCmd(g),
		)
	})
}

func newSubsPlanCreateCmd(g *GlobalFlags) *verb {
	var code, name, currency, interval, stripePriceID string
	var amount, intervalDays int64
	var graceDays, trialDays int
	var benefits string
	return newVerb(g, "create", "create a subscription plan (subscriptions.write)", "subscriptions plans create --code <c> --name <n> --amount <n> --currency <cur> --interval <monthly|...> [...]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&code, "code", "", "plan code (required)")
			fs.StringVar(&name, "name", "", "display name (required)")
			fs.Int64Var(&amount, "amount", 0, "recurring amount in minimal currency units, e.g. 1999 = ¥19.99 (required)")
			fs.StringVar(&currency, "currency", "", "currency code, e.g. cny / usd (required)")
			fs.StringVar(&interval, "interval", "", "billing interval, e.g. month | week | year (required; custom spans via --interval-days)")
			fs.Int64Var(&intervalDays, "interval-days", 0, "explicit interval length in days (alternative to --interval)")
			fs.IntVar(&graceDays, "grace-days", 0, "grace days after period end before expiry")
			fs.IntVar(&trialDays, "trial-days", 0, "trial days before first charge")
			fs.StringVar(&benefits, "benefits", "", `JSON: {"grants":[{"assetCode":"gems","quantity":100,"expiresIn":30}],"entitlements":[{"assetCode":"vip","tier":1}]}`)
			fs.StringVar(&stripePriceID, "stripe-price-id", "", "Stripe price override (providerOverrides.stripePriceId)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			req, err := buildCreatePlanReq(code, name, amount, currency, interval, intervalDays, graceDays, trialDays, benefits, stripePriceID)
			if err != nil {
				return err
			}
			return call(g, env, methodSubsPlanCreate, req)
		})
}

// buildCreatePlanReq 构造 CreatePlanRequest：必填 code/name/amount/currency/
// interval（interval 与 interval_days 二选一即可，服务端做跨字段校验）。
func buildCreatePlanReq(code, name string, amount int64, currency, interval string, intervalDays int64, graceDays, trialDays int, benefits, stripePriceID string) (map[string]any, error) {
	if code == "" || name == "" || currency == "" {
		return nil, fmt.Errorf("--code, --name and --currency are required")
	}
	if amount == 0 {
		return nil, fmt.Errorf("--amount is required (minimal currency units, e.g. 1999)")
	}
	if interval == "" && intervalDays == 0 {
		return nil, fmt.Errorf("--interval or --interval-days is required")
	}
	req := map[string]any{"code": code, "name": name, "amount": amount, "currency": currency}
	if interval != "" {
		req["interval"] = interval
	}
	if intervalDays != 0 {
		req["intervalDays"] = intervalDays
	}
	if graceDays != 0 {
		req["graceDays"] = graceDays
	}
	if trialDays != 0 {
		req["trialDays"] = trialDays
	}
	if benefits != "" {
		m, err := jsonObject(benefits, "--benefits")
		if err != nil {
			return nil, err
		}
		req["benefits"] = m
	}
	if stripePriceID != "" {
		req["providerOverrides"] = map[string]any{"stripePriceId": stripePriceID}
	}
	return req, nil
}

func newSubsPlanListCmd(g *GlobalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "list subscription plans", "subscriptions plans list [--page-size <n>] [--page-token <t>]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next page token from the previous response")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			return call(g, env, methodSubsPlanList, listJSON(pageSize, pageToken))
		})
}

func newSubsPlanGetCmd(g *GlobalFlags) *verb {
	return newVerb(g, "get", "get a plan by ID", "subscriptions plans get <plan-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodSubsPlanGet, map[string]any{"planId": args[0]})
		})
}

func newSubsPlanUpdateCmd(g *GlobalFlags) *verb {
	var name, currency, interval, status, benefits, stripePriceID string
	var amount, intervalDays int64
	var graceDays, trialDays int
	return newVerb(g, "update", "update a plan (subscriptions.write; only explicitly passed fields)", "subscriptions plans update [--name] [--amount] [--currency] [--interval] [--interval-days] [--grace-days] [--trial-days] [--status active|archived] [--benefits '<json>'] [--stripe-price-id] <plan-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "display name")
			fs.Int64Var(&amount, "amount", 0, "recurring amount in minimal currency units")
			fs.StringVar(&currency, "currency", "", "currency code")
			fs.StringVar(&interval, "interval", "", "billing interval")
			fs.Int64Var(&intervalDays, "interval-days", 0, "explicit interval length in days")
			fs.IntVar(&graceDays, "grace-days", 0, "grace days after period end")
			fs.IntVar(&trialDays, "trial-days", 0, "trial days before first charge")
			fs.StringVar(&status, "status", "", "status: active | archived")
			fs.StringVar(&benefits, "benefits", "", `benefits JSON (only applied when passed)`)
			fs.StringVar(&stripePriceID, "stripe-price-id", "", "Stripe price override (only applied when passed)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildUpdatePlanReq(v, args[0], subPlanPatch{
				name: name, amount: amount, currency: currency, interval: interval,
				intervalDays: intervalDays, graceDays: graceDays, trialDays: trialDays,
				status: status, benefits: benefits, stripePriceID: stripePriceID,
			})
			if err != nil {
				return err
			}
			return call(g, env, methodSubsPlanUpdate, req)
		})
}

// subPlanPatch 聚合 UpdatePlanRequest 的可修改字段。
type subPlanPatch struct {
	name, currency, interval, status string
	amount, intervalDays             int64
	graceDays, trialDays             int
	benefits, stripePriceID          string
}

// buildUpdatePlanReq 构造 UpdatePlanRequest（proto3 optional：未显式传入 =
// 不修改）。benefits/providerOverrides 是普通消息字段（nil 语义由服务端定），
// 仅显式传入才携带，避免误清。
func buildUpdatePlanReq(v *verb, planID string, p subPlanPatch) (map[string]any, error) {
	if planID == "" {
		return nil, fmt.Errorf("missing plan-id positional argument")
	}
	req := map[string]any{"planId": planID}
	setChanged(v, "name", req, "name", p.name)
	setChanged(v, "amount", req, "amount", p.amount)
	setChanged(v, "currency", req, "currency", p.currency)
	setChanged(v, "interval", req, "interval", p.interval)
	setChanged(v, "interval-days", req, "intervalDays", p.intervalDays)
	setChanged(v, "grace-days", req, "graceDays", p.graceDays)
	setChanged(v, "trial-days", req, "trialDays", p.trialDays)
	setChanged(v, "status", req, "status", p.status)
	if v.changed("benefits") {
		m, err := jsonObject(p.benefits, "--benefits")
		if err != nil {
			return nil, err
		}
		req["benefits"] = m
	}
	if v.changed("stripe-price-id") {
		req["providerOverrides"] = map[string]any{"stripePriceId": p.stripePriceID}
	}
	return req, nil
}

func newSubsPlanDeleteCmd(g *GlobalFlags) *verb {
	return newVerb(g, "delete", "delete a plan (subscriptions.write)", "subscriptions plans delete <plan-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodSubsPlanDelete, map[string]any{"planId": args[0]})
		})
}

func newSubsListCmd(g *GlobalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "list subscriptions", "subscriptions list [--page-size <n>] [--page-token <t>]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next page token from the previous response")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			return call(g, env, methodSubsList, listJSON(pageSize, pageToken))
		})
}

func newSubsGetCmd(g *GlobalFlags) *verb {
	return newVerb(g, "get", "get a subscription by ID", "subscriptions get <subscription-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodSubsGet, map[string]any{"subscriptionId": args[0]})
		})
}

// newSubsCancelCmd / newSubsExpireCmd: 生命周期终态动词（admin 强制）。
// 两者都是立即终态（forceTerminal），仅落点状态不同：Cancel → canceled
// （托管订阅同步向渠道发起取消）；Expire → expired。期末取消（cancel at
// period end）是 Client 面另一条路径，不经此动词。
func newSubsCancelCmd(g *GlobalFlags) *verb {
	var reason string
	return newVerb(g, "cancel", "force-cancel a subscription immediately (subscriptions.write, admin; status → canceled)", "subscriptions cancel <subscription-id> [--reason <text>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&reason, "reason", "", "reason (for audit)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req := map[string]any{"subscriptionId": args[0]}
			if reason != "" {
				req["reason"] = reason
			}
			return call(g, env, methodSubsCancel, req)
		})
}

func newSubsExpireCmd(g *GlobalFlags) *verb {
	var reason string
	return newVerb(g, "expire", "expire a subscription immediately (subscriptions.write, admin)", "subscriptions expire <subscription-id> [--reason <text>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&reason, "reason", "", "reason (for audit)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req := map[string]any{"subscriptionId": args[0]}
			if reason != "" {
				req["reason"] = reason
			}
			return call(g, env, methodSubsExpire, req)
		})
}
