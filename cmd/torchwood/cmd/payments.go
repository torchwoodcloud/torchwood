package cmd

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

const (
	methodPaymentsOrdersList = "/torchwood.server.v1.PaymentsService/ListOrders"
	methodPaymentsOrderGet   = "/torchwood.server.v1.PaymentsService/GetOrder"
	methodPaymentsRefund     = "/torchwood.server.v1.PaymentsService/Refund"
	methodPaymentsManualFill = "/torchwood.server.v1.PaymentsService/ManualFulfill"
)

// newPaymentsCmd 覆盖 PaymentsService 全部 4 个方法（订单查询/退款/人工履约
// 兜底）。读 payments.read；Refund/ManualFulfill 是金额敏感写（payments.write
// + admin 角色，自动进审计日志）。amount 一律最小货币单位 int64。
func newPaymentsCmd(g *globalFlags) *group {
	return newGroup(g, "payments", "payment orders: query, refund, manual fulfill (reads: payments.read; refund/fulfill: payments.write)", func(sub *commands.App) {
		sub.Register(
			newPaymentsOrdersListCmd(g),
			newPaymentsOrderGetCmd(g),
			newPaymentsRefundCmd(g),
			newPaymentsFulfillCmd(g),
		)
	})
}

func newPaymentsOrdersListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "list payment orders (newest first)", "payments list [--page-size <n>] [--page-token <t>]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next page token from the previous response")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			return call(g, env, methodPaymentsOrdersList, listJSON(pageSize, pageToken))
		})
}

func newPaymentsOrderGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "get a payment order by ID", "payments get <order-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodPaymentsOrderGet, map[string]any{"orderId": args[0]})
		})
}

func newPaymentsRefundCmd(g *globalFlags) *verb {
	var amount int64
	var reason string
	return newVerb(g, "refund", "refund a paid order (payments.write; audit-logged; does not revoke granted assets)", "payments refund <order-id> [--amount <n>] [--reason <text>]",
		func(fs *flag.FlagSet) {
			fs.Int64Var(&amount, "amount", 0, "refund amount in minimal currency units (omitted/0 = full refund)")
			fs.StringVar(&reason, "reason", "", "reason (for audit)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildRefundReq(args[0], amount, reason)
			if err != nil {
				return err
			}
			return call(g, env, methodPaymentsRefund, req)
		})
}

// buildRefundReq 构造 RefundRequest；proto 语义 0 = 全额退款，无需 presence。
func buildRefundReq(orderID string, amount int64, reason string) (map[string]any, error) {
	if orderID == "" {
		return nil, fmt.Errorf("missing order-id positional argument")
	}
	req := map[string]any{"orderId": orderID}
	if amount != 0 {
		req["amount"] = amount
	}
	if reason != "" {
		req["reason"] = reason
	}
	return req, nil
}

func newPaymentsFulfillCmd(g *globalFlags) *verb {
	var reason string
	return newVerb(g, "fulfill", "manually mark a paid order fulfilled (fallback when fulfillment failed; payments.write; audit-logged)", "payments fulfill <order-id> [--reason <text>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&reason, "reason", "", "reason (for audit)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			if args[0] == "" {
				return fmt.Errorf("missing order-id positional argument")
			}
			req := map[string]any{"orderId": args[0]}
			if reason != "" {
				req["reason"] = reason
			}
			return call(g, env, methodPaymentsManualFill, req)
		})
}
