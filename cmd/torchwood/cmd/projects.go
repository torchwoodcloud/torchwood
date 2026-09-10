package cmd

import (
	"flag"

	"github.com/lynx-go/commands"
)

const (
	methodProjectsList = "/torchwood.server.v1.ProjectsService/ListProjects"
	methodProjectsGet  = "/torchwood.server.v1.ProjectsService/GetProject"
)

// newProjectsCmd 提供 ProjectsService 的 list/get。
// CreateProject/UpdateProject/DeleteProject 限平台 admin（console session），API Key 无法调用，CLI 不提供。
func newProjectsCmd(g *globalFlags) *group {
	return newGroup(g, "projects", "项目管理（list/get；create/update/delete 限平台 admin，CLI 不提供）", func(sub *commands.App) {
		sub.Register(
			newProjectsListCmd(g),
			newProjectsGetCmd(g),
		)
	})
}

func newProjectsListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出项目", "projects list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodProjectsList, listJSON(pageSize, pageToken))
		})
}

func newProjectsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取项目", "projects get <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodProjectsGet, map[string]any{"id": args[0]})
		})
}
