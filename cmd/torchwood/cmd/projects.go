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
	return newGroup(g, "projects", "project management (list/get; create/update/delete are platform-admin only, not exposed via CLI)", func(sub *commands.App) {
		sub.Register(
			newProjectsListCmd(g),
			newProjectsGetCmd(g),
		)
	})
}

func newProjectsListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "list projects", "projects list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default 50, max 1000)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodProjectsList, listJSON(pageSize, pageToken))
		})
}

func newProjectsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "get a project by ID", "projects get <id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodProjectsGet, map[string]any{"id": args[0]})
		})
}
