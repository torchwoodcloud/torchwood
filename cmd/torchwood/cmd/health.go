package cmd

import (
	"github.com/lynx-go/commands"
)

const (
	methodHealthCheck  = "/torchwood.server.v1.HealthService/Check"
	methodHealthGetVer = "/torchwood.server.v1.HealthService/GetVersion"
)

// newHealthCmd 提供 HealthService 两个公开方法（ACCESS_PUBLIC，无需 API key）。
func newHealthCmd(g *globalFlags) *group {
	return newGroup(g, "health", "health checks (public, no API key required)", func(sub *commands.App) {
		sub.Register(
			newPublicVerb(g, "get", "query service health status", "health get", nil,
				func(v *verb, env *commands.Environment, _ []string) error {
					return call(g, env, methodHealthCheck, nil)
				}),
			newPublicVerb(g, "version", "query the server build version", "health version", nil,
				func(v *verb, env *commands.Environment, _ []string) error {
					return call(g, env, methodHealthGetVer, nil)
				}),
		)
	})
}
