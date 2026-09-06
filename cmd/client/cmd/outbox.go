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
	return newGroup(g, "outbox", "outbox 死信管理（list-dead/replay）", func(sub *commands.App) {
		sub.Register(
			newOutboxListDeadCmd(g),
			newOutboxReplayCmd(g),
		)
	})
}

func newOutboxListDeadCmd(g *globalFlags) *verb {
	var projectID string
	var pageSize int
	var pageToken string
	return newVerb(g, "list-dead", "列出死信", "admin outbox list-dead [--project-id]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&projectID, "project-id", "", "项目 ID（必填）")
			fs.IntVar(&pageSize, "page-size", 0, "每页条数")
			fs.StringVar(&pageToken, "page-token", "", "上一页 next_page_token")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			payload := map[string]any{}
			if projectID != "" {
				payload["project_id"] = projectID
			}
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
	var projectID string
	return newVerb(g, "replay", "重放单条死信", "admin outbox replay <event-id> [--project-id]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&projectID, "project-id", "", "项目 ID（必填）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			payload := map[string]any{"event_id": args[0]}
			if projectID != "" {
				payload["project_id"] = projectID
			}
			return call(g, env, methodOutboxReplay, payload)
		})
}
