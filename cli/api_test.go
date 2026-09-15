package cli

import (
	"github.com/lynx-go/commands"
)

// newTestApp 复刻 cmd/torchwood/app.go 的根命令表装配，供包内端到端分发
// 测试使用；全量走导出构造器（api.go），顺带回归守卫装配面本身——新增
// 根命令时两处（cmd/torchwood 与此处）需同步登记，漏登记会被
// cmd/torchwood/app_test.go 的命令名清单断言拦下。
func newTestApp(version string) *commands.App {
	g := NewGlobalFlags(version)
	app := commands.New()
	app.HelpFooter = "torchwood " + version
	app.ExitCode = RPCExitCode
	app.Register(
		NewHealthCmd(g),
		NewVersionCmd(g, version),
		NewUUIDCmd(g),
		NewProjectsCmd(g),
		NewUsersCmd(g),
		NewDatabasesCmd(g),
		NewGroupsCmd(g),
		NewStorageCmd(g),
		NewFunctionsCmd(g),
		NewOAuthProvidersCmd(g),
		NewLeaderboardsCmd(g),
		NewAnalyticsCmd(g),
		NewPaymentsCmd(g),
		NewAssetsCmd(g),
		NewSubscriptionsCmd(g),
		NewBillingCmd(g),
		NewAdminCmd(g),
		NewAuditLogsCmd(g),
		NewConfigCmd(),
		NewRPCCmd(g),
		NewRunbookCmd(g),
	)
	return app
}
