package cli

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

const (
	methodWebhooksCreate       = "/torchwood.server.v1.WebhooksService/CreateWebhook"
	methodWebhooksList         = "/torchwood.server.v1.WebhooksService/ListWebhooks"
	methodWebhooksGet          = "/torchwood.server.v1.WebhooksService/GetWebhook"
	methodWebhooksUpdate       = "/torchwood.server.v1.WebhooksService/UpdateWebhook"
	methodWebhooksDelete       = "/torchwood.server.v1.WebhooksService/DeleteWebhook"
	methodWebhooksRotateSecret = "/torchwood.server.v1.WebhooksService/RotateWebhookSecret"
	methodWebhooksListDeliver  = "/torchwood.server.v1.WebhooksService/ListDeliveries"
	methodWebhooksGetDelivery  = "/torchwood.server.v1.WebhooksService/GetDelivery"
	methodWebhooksRedeliver    = "/torchwood.server.v1.WebhooksService/RedeliverDelivery"
)

// NewWebhooksCmd 覆盖 WebhooksService 全部九方法（functions-v4.md §6，D5）：
// webhooks（create/list/get/update/delete）、rotate-secret、deliveries
// （list/get/redeliver）。创建/轮换响应携带一次性明文密钥（立即配置到
// 接收方，之后任何接口只出指纹）。
func NewWebhooksCmd(g *GlobalFlags) *group {
	return newGroup(g, "webhooks", "webhook subscription management (WebhooksService: events → your HTTPS endpoint)", func(sub *commands.App) {
		sub.Register(
			newWebhooksCreateCmd(g),
			newWebhooksListCmd(g),
			newWebhooksGetCmd(g),
			newWebhooksUpdateCmd(g),
			newWebhooksDeleteCmd(g),
			newWebhooksRotateSecretCmd(g),
			newWebhooksDeliveriesCmd(g),
		)
	})
}

func newWebhooksCreateCmd(g *GlobalFlags) *verb {
	var id, url, status, events string
	return newVerb(g, "create", "create a subscription (one-time secret echoed in response)", "webhooks create --url <https://endpoint> --events <e1,e2> [--id <id>] [--status enabled|paused]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&url, "url", "", "delivery endpoint URL, https required (required)")
			fs.StringVar(&events, "events", "", "comma-separated subscription patterns, eventmatch syntax (required, 1-64), e.g. auth.users.created,databases.db.collections.col.documents.created")
			fs.StringVar(&id, "id", "", "subscription ID (omit = server-generated wh-<uuid>)")
			fs.StringVar(&status, "status", "", "initial status: enabled (default) | paused")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			req, err := buildCreateWebhookReq(v, url, events, id, status)
			if err != nil {
				return err
			}
			return call(g, env, methodWebhooksCreate, req)
		})
}

func newWebhooksListCmd(g *GlobalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "list webhook subscriptions", "webhooks list [--page-size <n>] [--page-token <t>]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodWebhooksList, listJSON(pageSize, pageToken))
		})
}

func newWebhooksGetCmd(g *GlobalFlags) *verb {
	return newVerb(g, "get", "get a webhook subscription by ID", "webhooks get <webhook-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodWebhooksGet, map[string]any{"webhookId": args[0]})
		})
}

func newWebhooksUpdateCmd(g *GlobalFlags) *verb {
	var url, status, events string
	return newVerb(g, "update", "update a subscription (only explicitly passed fields; --events replaces the whole set)", "webhooks update [--url <https://...>] [--events <e1,e2>] [--status enabled|paused] <webhook-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&url, "url", "", "delivery endpoint URL, https required")
			fs.StringVar(&events, "events", "", "comma-separated subscription patterns (full replacement)")
			fs.StringVar(&status, "status", "", "enabled | paused")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildUpdateWebhookReq(v, args[0], url, events, status)
			if err != nil {
				return err
			}
			return call(g, env, methodWebhooksUpdate, req)
		})
}

func newWebhooksDeleteCmd(g *GlobalFlags) *verb {
	return newVerb(g, "delete", "delete a subscription (delivery rows cascade)", "webhooks delete <webhook-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodWebhooksDelete, map[string]any{"webhookId": args[0]})
		})
}

// newWebhooksRotateSecretCmd: webhooks rotate-secret（新明文一次性回显，
// 旧钥保留一个宽限窗双签 v2——接收方不断流）。
func newWebhooksRotateSecretCmd(g *GlobalFlags) *verb {
	return newVerb(g, "rotate-secret", "rotate the signing secret (new secret echoed once; old key valid through the grace window)", "webhooks rotate-secret <webhook-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodWebhooksRotateSecret, map[string]any{"webhookId": args[0]})
		})
}

// newWebhooksDeliveriesCmd: webhooks deliveries（投递行 = DLQ 浏览/重投
// 管理面；failed 即死信）。
func newWebhooksDeliveriesCmd(g *GlobalFlags) *group {
	return newGroup(g, "deliveries", "delivery records (DLQ browsing and redelivery)", func(sub *commands.App) {
		sub.Register(
			newWebhooksDeliveriesListCmd(g),
			newWebhooksDeliveriesGetCmd(g),
			newWebhooksDeliveriesRedeliverCmd(g),
		)
	})
}

func newWebhooksDeliveriesListCmd(g *GlobalFlags) *verb {
	var status string
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "list delivery records (newest first; failed rows are the DLQ)", "webhooks deliveries list [--status pending|delivered|failed] [--page-size <n>] [--page-token <t>] <webhook-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&status, "status", "", "filter by delivery status: pending | delivered | failed (empty = all)")
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default when omitted)")
			fs.StringVar(&pageToken, "page-token", "", "next page token from the previous response")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req := listJSON(pageSize, pageToken)
			if status != "" {
				req["status"] = status
			}
			req["webhookId"] = args[0]
			return call(g, env, methodWebhooksListDeliver, req)
		})
}

func newWebhooksDeliveriesGetCmd(g *GlobalFlags) *verb {
	return newVerb(g, "get", "get a delivery record by ID", "webhooks deliveries get <webhook-id> <delivery-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodWebhooksGetDelivery, map[string]any{"webhookId": args[0], "deliveryId": args[1]})
		})
}

// newWebhooksDeliveriesRedeliverCmd: webhooks deliveries redeliver（终态行
// 重置为立即到期新一轮投递；行内 attempts 归零）。
func newWebhooksDeliveriesRedeliverCmd(g *GlobalFlags) *verb {
	return newVerb(g, "redeliver", "redeliver a terminal delivery (starts a fresh round; attempts reset)", "webhooks deliveries redeliver <webhook-id> <delivery-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodWebhooksRedeliver, map[string]any{"webhookId": args[0], "deliveryId": args[1]})
		})
}

// ——请求构造（显式传入才生效的 optional 字段经 setChanged 判定）——

// buildCreateWebhookReq 构造 CreateWebhookRequest（url/events 必填；
// id/status optional——显式传入才生效）。
func buildCreateWebhookReq(v *verb, url, events, id, status string) (map[string]any, error) {
	if url == "" {
		return nil, fmt.Errorf("--url is required (delivery endpoint URL, https)")
	}
	patterns := splitList(events)
	if len(patterns) == 0 {
		return nil, fmt.Errorf("--events is required (comma-separated subscription patterns)")
	}
	req := map[string]any{"url": url, "events": patterns}
	if id != "" {
		req["id"] = id
	}
	setChanged(v, "status", req, "status", status)
	return req, nil
}

// buildUpdateWebhookReq 构造 UpdateWebhookRequest：仅设置显式传入的字段
// （proto optional/presence 语义；--events 传入即全量替换订阅串集）。
func buildUpdateWebhookReq(v *verb, webhookID, url, events, status string) (map[string]any, error) {
	if webhookID == "" {
		return nil, fmt.Errorf("missing webhook-id")
	}
	req := map[string]any{"webhookId": webhookID}
	setChanged(v, "url", req, "url", url)
	setChanged(v, "status", req, "status", status)
	if v.changed("events") {
		req["events"] = splitList(events)
	}
	return req, nil
}
