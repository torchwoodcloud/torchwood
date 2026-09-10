package cmd

import (
	"flag"

	"github.com/lynx-go/commands"
)

const (
	methodOutboxListDead = "/torchwood.server.v1.OutboxService/ListDeadLetters"
	methodOutboxReplay   = "/torchwood.server.v1.OutboxService/ReplayDeadLetter"
)

// newOutboxCmd 提供 outbox 死信管理（admin）。
func newOutboxCmd(g *globalFlags) *group {
	return newGroup(g, "outbox", "outbox dead-letter management (list-dead/replay)", func(sub *commands.App) {
		sub.Register(
			newOutboxListDeadCmd(g),
			newOutboxReplayCmd(g),
		)
	})
}

// 项目上下文来自凭证（决策 v8 项目寻址不变量）：API key 取密钥行绑定，
// 请求体不再携带 project_id。
func newOutboxListDeadCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list-dead", "list dead letters", "admin outbox list-dead",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token of the previous page")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			payload := map[string]any{}
			if pageSize != 0 {
				payload["page_size"] = pageSize
			}
			if pageToken != "" {
				payload["page_token"] = pageToken
			}
			return call(g, env, methodOutboxListDead, payload)
		})
}

func newOutboxReplayCmd(g *globalFlags) *verb {
	return newVerb(g, "replay", "replay a single dead letter", "admin outbox replay <event-id>",
		func(fs *flag.FlagSet) {},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodOutboxReplay, map[string]any{"event_id": args[0]})
		})
}
