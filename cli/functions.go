package cli

import (
	"flag"
	"fmt"

	"github.com/lynx-go/commands"
)

const (
	methodFunctionsCreate           = "/torchwood.server.v1.FunctionsService/CreateFunction"
	methodFunctionsList             = "/torchwood.server.v1.FunctionsService/ListFunctions"
	methodFunctionsGet              = "/torchwood.server.v1.FunctionsService/GetFunction"
	methodFunctionsUpdate           = "/torchwood.server.v1.FunctionsService/UpdateFunction"
	methodFunctionsDelete           = "/torchwood.server.v1.FunctionsService/DeleteFunction"
	methodFunctionsSetScopes        = "/torchwood.server.v1.FunctionsService/SetFunctionScopes"
	methodFunctionsRotateSecret     = "/torchwood.server.v1.FunctionsService/RotateFunctionSecret"
	methodFunctionsInvoke           = "/torchwood.server.v1.FunctionsService/InvokeFunction"
	methodFunctionsListInvocations  = "/torchwood.server.v1.FunctionsService/ListInvocations"
	methodFunctionsGetInvocation    = "/torchwood.server.v1.FunctionsService/GetInvocation"
	methodFunctionsCreateTrigger    = "/torchwood.server.v1.FunctionsService/CreateFunctionTrigger"
	methodFunctionsListTriggers     = "/torchwood.server.v1.FunctionsService/ListFunctionTriggers"
	methodFunctionsDeleteTrigger    = "/torchwood.server.v1.FunctionsService/DeleteFunctionTrigger"
	methodFunctionsRotateTriggerTok = "/torchwood.server.v1.FunctionsService/RotateFunctionTriggerToken"
)

// newFunctionsCmd 覆盖 FunctionsService 的 v4 方法面（docs/design/
// functions-v4.md §2）：functions（create/list/get/update/delete）、
// scopes、rotate-secret、invoke、invocations（list/get）、triggers 四方法。
// 部署面（deployments/variables/runtimes/specifications）随执行面外包退役
// ——函数 = endpoint 常驻服务，部署走 fleetly CLI 或用户自有工具链。
func newFunctionsCmd(g *GlobalFlags) *group {
	return newGroup(g, "functions", "function management (FunctionsService v4: target + invocations)", func(sub *commands.App) {
		sub.Register(
			newFunctionsListCmd(g),
			newFunctionsCreateCmd(g),
			newFunctionsGetCmd(g),
			newFunctionsUpdateCmd(g),
			newFunctionsDeleteCmd(g),
			newFunctionsScopesCmd(g),
			newFunctionsRotateSecretCmd(g),
			newFunctionsInvokeCmd(g),
			newFunctionsInvocationsCmd(g),
			newFunctionsTriggersCmd(g),
		)
	})
}

func newFunctionsListCmd(g *GlobalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "list functions", "functions list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default 50, max 1000)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodFunctionsList, listJSON(pageSize, pageToken))
		})
}

func newFunctionsCreateCmd(g *GlobalFlags) *verb {
	var id, name, targetURL, targetSecret string
	var timeoutSeconds int
	var enabled bool
	var declaredScopes string
	var clientCallable bool
	var clientPerUserLimit int
	var clientLimitWindow string
	return newVerb(g, "create", "create a function (endpoint target + one-time secret)", "functions create --id <id> --name <name> --url <https://endpoint> [--target-secret <secret>] [--scopes a:write,b:read] [--client-callable] [--client-per-user-limit <n>] [--client-limit-window <minute|hour|day>]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "function ID (required)")
			fs.StringVar(&name, "name", "", "function name (required)")
			fs.StringVar(&targetURL, "url", "", "target endpoint URL, https required in production (required)")
			fs.StringVar(&targetSecret, "target-secret", "", "signing secret (omit = server-generated; echoed once in this response)")
			fs.IntVar(&timeoutSeconds, "timeout-seconds", 0, "timeout in seconds (1-300, server default when omitted)")
			fs.StringVar(&declaredScopes, "scopes", "", "comma-separated declared scopes, e.g. assets:write,economy:write")
			fs.BoolVar(&enabled, "enabled", false, "whether enabled (pass --enabled=true/false explicitly to take effect)")
			// 客户端调用面策略（P2，设计 §4；optional——显式传入才生效）。
			fs.BoolVar(&clientCallable, "client-callable", false, "allow client-face invocation (pass --client-callable=true/false explicitly to take effect)")
			fs.IntVar(&clientPerUserLimit, "client-per-user-limit", 0, "per-user invocation quota within the limit window (>=1 when --client-callable=true)")
			fs.StringVar(&clientLimitWindow, "client-limit-window", "", "quota window granularity: minute | hour | day (day is UTC-based, server default when omitted)")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			req, err := buildCreateFunctionReq(v, id, name, targetURL, targetSecret, timeoutSeconds, enabled,
				clientCallable, clientPerUserLimit, clientLimitWindow, declaredScopes)
			if err != nil {
				return err
			}
			return call(g, env, methodFunctionsCreate, req)
		})
}

func newFunctionsGetCmd(g *GlobalFlags) *verb {
	return newVerb(g, "get", "get a function by ID", "functions get <function-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodFunctionsGet, map[string]any{"functionId": args[0]})
		})
}

func newFunctionsUpdateCmd(g *GlobalFlags) *verb {
	var name, targetURL string
	var timeoutSeconds int
	var enabled bool
	var clientCallable bool
	var clientPerUserLimit int
	var clientLimitWindow string
	return newVerb(g, "update", "update a function (only explicitly passed fields)", "functions update [--name] [--url] [--timeout-seconds] [--enabled] [--client-callable] [--client-per-user-limit] [--client-limit-window] <function-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "function name")
			fs.StringVar(&targetURL, "url", "", "target endpoint URL (secret rotation is a separate command)")
			fs.IntVar(&timeoutSeconds, "timeout-seconds", 0, "timeout in seconds (1-300)")
			fs.BoolVar(&enabled, "enabled", false, "whether enabled (pass --enabled=true/false explicitly to take effect)")
			// 客户端调用面策略（P2，设计 §4；proto3 optional——显式传入才生效）。
			fs.BoolVar(&clientCallable, "client-callable", false, "allow client-face invocation (pass --client-callable=true/false explicitly to take effect)")
			fs.IntVar(&clientPerUserLimit, "client-per-user-limit", 0, "per-user invocation quota within the limit window (>=1 when --client-callable=true)")
			fs.StringVar(&clientLimitWindow, "client-limit-window", "", "quota window granularity: minute | hour | day (day is UTC-based)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildUpdateFunctionReq(v, args[0], name, targetURL, timeoutSeconds, enabled,
				clientCallable, clientPerUserLimit, clientLimitWindow)
			if err != nil {
				return err
			}
			return call(g, env, methodFunctionsUpdate, req)
		})
}

func newFunctionsDeleteCmd(g *GlobalFlags) *verb {
	return newVerb(g, "delete", "delete a function", "functions delete <function-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodFunctionsDelete, map[string]any{"functionId": args[0]})
		})
}

// newFunctionsScopesCmd: functions scopes set（SetFunctionScopes 全量替换）。
func newFunctionsScopesCmd(g *GlobalFlags) *verb {
	var scopes string
	return newVerb(g, "scopes", "replace declared scopes (comma-separated; empty revokes all)", "functions scopes --scopes <a:write,b:read> <function-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&scopes, "scopes", "", "comma-separated declared scopes (empty string revokes all platform access)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req := map[string]any{"functionId": args[0], "declaredScopes": splitList(scopes)}
			return call(g, env, methodFunctionsSetScopes, req)
		})
}

// newFunctionsRotateSecretCmd: functions rotate-secret（RotateFunctionSecret，
// 新明文一次性回显——立即部署到函数侧；旧钥保留一个宽限窗双签）。
func newFunctionsRotateSecretCmd(g *GlobalFlags) *verb {
	return newVerb(g, "rotate-secret", "rotate the endpoint target signing secret (new secret echoed once)", "functions rotate-secret <function-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodFunctionsRotateSecret, map[string]any{"functionId": args[0]})
		})
}

// newFunctionsInvokeCmd: functions invoke（InvokeFunction，同步默认、--async
// 立即返回 queued invocation）。
func newFunctionsInvokeCmd(g *GlobalFlags) *verb {
	var input string
	var async bool
	return newVerb(g, "invoke", "invoke a function", "functions invoke --input <json> <function-id>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&input, "input", "", "invocation input (required, must be a valid JSON string, ≤32KB)")
			fs.BoolVar(&async, "async", false, "run asynchronously (only takes effect when --async=true is passed explicitly)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildInvokeReq(v, args[0], input, async)
			if err != nil {
				return err
			}
			return call(g, env, methodFunctionsInvoke, req)
		})
}

// newFunctionsInvocationsCmd: functions invocations list/get。
func newFunctionsInvocationsCmd(g *GlobalFlags) *group {
	var invStatus, invPageToken = "", ""
	var invPageSize = 0
	return newGroup(g, "invocations", "function invocation records", func(sub *commands.App) {
		sub.Register(
			newVerb(g, "list", "list invocation records", "functions invocations list [--status <s>] [--page-size <n>] [--page-token <t>] <function-id>",
				func(fs *flag.FlagSet) {
					fs.StringVar(&invStatus, "status", "", "filter by invocation status (queued|running|completed|failed)")
					fs.IntVar(&invPageSize, "page-size", 0, "page size (server default when omitted)")
					fs.StringVar(&invPageToken, "page-token", "", "next page token from the previous response")
				},
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 1); err != nil {
						return err
					}
					req := listJSON(invPageSize, invPageToken)
					if invStatus != "" {
						req["status"] = invStatus
					}
					req["functionId"] = args[0]
					return call(g, env, methodFunctionsListInvocations, req)
				}),
			newVerb(g, "get", "get an invocation record by ID", "functions invocations get <function-id> <invocation-id>", nil,
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 2); err != nil {
						return err
					}
					return call(g, env, methodFunctionsGetInvocation, map[string]any{"functionId": args[0], "invocationId": args[1]})
				}),
		)
	})
}

// newFunctionsTriggersCmd: functions triggers（v4 触发器四方法）。
func newFunctionsTriggersCmd(g *GlobalFlags) *group {
	return newGroup(g, "triggers", "function trigger management (http / cron / event)", func(sub *commands.App) {
		sub.Register(
			newFunctionsTriggersCreateCmd(g),
			newFunctionsTriggersListCmd(g),
			newFunctionsTriggersDeleteCmd(g),
			newFunctionsTriggersRotateTokenCmd(g),
		)
	})
}

func newFunctionsTriggersCreateCmd(g *GlobalFlags) *verb {
	var functionID, typ, responseMode, ackBody, handshake, expr, misfire, events string
	var bodyLimit int
	return newVerb(g, "create", "create a trigger (type = http | cron | event)", "functions triggers create --function-id <id> --type http --response-mode sync [--ack-body <b>] [--handshake echo] [--body-limit <bytes>]\n  functions triggers create --function-id <id> --type cron --expr \"*/5 * * * *\" [--misfire catch_up_once]\n  functions triggers create --function-id <id> --type event --events databases.db.collections.col.documents.created",
		func(fs *flag.FlagSet) {
			fs.StringVar(&functionID, "function-id", "", "function ID (required)")
			fs.StringVar(&typ, "type", "", "trigger type: http | cron | event (required)")
			fs.StringVar(&responseMode, "response-mode", "", "http: sync | async_ack (required for http)")
			fs.StringVar(&ackBody, "ack-body", "", "http async_ack 200 response body (≤1KB)")
			fs.StringVar(&handshake, "handshake", "", "http GET handshake: echo")
			fs.IntVar(&bodyLimit, "body-limit", 0, "http request body limit in bytes (65536 default, 1MB max)")
			fs.StringVar(&expr, "expr", "", "cron: 5-field UTC cron expression (required for cron)")
			fs.StringVar(&misfire, "misfire", "", "cron misfire policy: skip | catch_up_once (default)")
			fs.StringVar(&events, "events", "", "event: comma-separated subscription patterns (required for event)")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			req, err := buildCreateTriggerReq(v, functionID, typ, responseMode, ackBody, handshake, bodyLimit, expr, misfire, events)
			if err != nil {
				return err
			}
			return call(g, env, methodFunctionsCreateTrigger, req)
		})
}

func newFunctionsTriggersListCmd(g *GlobalFlags) *verb {
	return newVerb(g, "list", "list function triggers", "functions triggers list <function-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodFunctionsListTriggers, map[string]any{"functionId": args[0]})
		})
}

func newFunctionsTriggersDeleteCmd(g *GlobalFlags) *verb {
	return newVerb(g, "delete", "delete a trigger", "functions triggers delete <function-id> <trigger-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodFunctionsDeleteTrigger, map[string]any{"functionId": args[0], "triggerId": args[1]})
		})
}

func newFunctionsTriggersRotateTokenCmd(g *GlobalFlags) *verb {
	return newVerb(g, "rotate-token", "rotate an http trigger token (old token invalid immediately)", "functions triggers rotate-token <function-id> <trigger-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodFunctionsRotateTriggerTok, map[string]any{"functionId": args[0], "triggerId": args[1]})
		})
}

// ——请求构造（显式传入才生效的 optional 字段经 setChanged 判定）——

// buildCreateFunctionReq 构造 CreateFunctionRequest（id/name/url 必填；
// 客户端调用面策略 optional——显式传入才生效）。
func buildCreateFunctionReq(v *verb, id, name, targetURL, targetSecret string, timeoutSeconds int, enabled bool,
	clientCallable bool, clientPerUserLimit int, clientLimitWindow, declaredScopes string) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("--id is required")
	}
	if name == "" {
		return nil, fmt.Errorf("--name is required")
	}
	if targetURL == "" {
		return nil, fmt.Errorf("--url is required (target endpoint URL)")
	}
	req := map[string]any{"id": id, "name": name, "targetUrl": targetURL}
	if targetSecret != "" {
		req["targetSecret"] = targetSecret
	}
	setChanged(v, "timeout-seconds", req, "timeoutSeconds", timeoutSeconds)
	setChanged(v, "enabled", req, "enabled", enabled)
	setChanged(v, "client-callable", req, "clientCallable", clientCallable)
	setChanged(v, "client-per-user-limit", req, "clientPerUserLimit", clientPerUserLimit)
	setChanged(v, "client-limit-window", req, "clientLimitWindow", clientLimitWindow)
	if scopes := splitList(declaredScopes); len(scopes) > 0 {
		req["declaredScopes"] = scopes
	}
	return req, nil
}

// buildUpdateFunctionReq 构造 UpdateFunctionRequest：仅设置显式传入的字段。
func buildUpdateFunctionReq(v *verb, functionID, name, targetURL string, timeoutSeconds int, enabled bool,
	clientCallable bool, clientPerUserLimit int, clientLimitWindow string) (map[string]any, error) {
	if functionID == "" {
		return nil, fmt.Errorf("missing function-id")
	}
	req := map[string]any{"functionId": functionID}
	setChanged(v, "name", req, "name", name)
	setChanged(v, "url", req, "targetUrl", targetURL)
	setChanged(v, "timeout-seconds", req, "timeoutSeconds", timeoutSeconds)
	setChanged(v, "enabled", req, "enabled", enabled)
	setChanged(v, "client-callable", req, "clientCallable", clientCallable)
	setChanged(v, "client-per-user-limit", req, "clientPerUserLimit", clientPerUserLimit)
	setChanged(v, "client-limit-window", req, "clientLimitWindow", clientLimitWindow)
	return req, nil
}

// buildInvokeReq 构造 InvokeFunctionRequest（--input 必填，与服务端校验一致）。
func buildInvokeReq(v *verb, functionID, input string, async bool) (map[string]any, error) {
	if functionID == "" {
		return nil, fmt.Errorf("missing function-id")
	}
	if input == "" {
		return nil, fmt.Errorf("--input is required (invocation input JSON string)")
	}
	req := map[string]any{"functionId": functionID, "data": input}
	setChanged(v, "async", req, "async", async)
	return req, nil
}

// buildCreateTriggerReq 构造 CreateFunctionTriggerRequest（type 分派配置段）。
func buildCreateTriggerReq(v *verb, functionID, typ, responseMode, ackBody, handshake string, bodyLimit int,
	expr, misfire, events string) (map[string]any, error) {
	if functionID == "" {
		return nil, fmt.Errorf("--function-id is required")
	}
	req := map[string]any{"functionId": functionID}
	switch typ {
	case "http":
		if responseMode == "" {
			return nil, fmt.Errorf("--response-mode is required for http triggers")
		}
		http := map[string]any{"responseMode": responseMode}
		if ackBody != "" {
			http["ackBody"] = ackBody
		}
		if handshake != "" {
			http["handshake"] = handshake
		}
		if bodyLimit > 1<<30 {
			return nil, fmt.Errorf("--body-limit exceeds maximum of %d", 1<<30)
		}
		setChanged(v, "body-limit", http, "bodyLimitBytes", int32(bodyLimit)) // #nosec G115 -- 已钳制 ≤1<<30
		req["type"] = "http"
		req["http"] = http
	case "cron":
		if expr == "" {
			return nil, fmt.Errorf("--expr is required for cron triggers")
		}
		cron := map[string]any{"expr": expr}
		if misfire != "" {
			cron["misfire"] = misfire
		}
		req["type"] = "cron"
		req["cron"] = cron
	case "event":
		patterns := splitList(events)
		if len(patterns) == 0 {
			return nil, fmt.Errorf("--events is required for event triggers")
		}
		req["type"] = "event"
		req["event"] = map[string]any{"events": patterns}
	default:
		return nil, fmt.Errorf("--type must be http, cron, or event")
	}
	return req, nil
}
