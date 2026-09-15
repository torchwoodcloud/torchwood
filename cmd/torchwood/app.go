package main

import (
	"github.com/lynx-go/commands"

	"github.com/torchwoodcloud/torchwood/cli"
)

// NewApp 装配根命令表——本包只做 CLI 装配（命令登记 + 帮助面/退出码钩子），
// 命令实现细节全部在 cli 包；version 由 main 包 ldflags 注入。
func NewApp(version string) *commands.App {
	g := cli.NewGlobalFlags(version)
	app := commands.New()
	app.HelpHeader = `Torchwood CLI calls the Server API over gRPC (not the HTTP gateway);
authentication always uses the x-api-key metadata (scopes follow the API
key's scopes). Defaults to 127.0.0.1:9060 (the server gRPC listens on
loopback only; for remote use go through an SSH tunnel, change
server.grpc.addr, or terminate TLS on a reverse proxy that forwards h2c
to the backend and pass --tls).
Global flags (--endpoint/--api-key/--timeout/--output/--tls/--profile) go
after the subcommand path and before positional arguments. Precedence per
flag: explicit flag > TORCHWOOD_CLI_* env var > config file profile
(~/.torchwood/config.yaml, one profile per project — see ` + "`torchwood config`" + `)
> built-in default.`
	app.HelpFooter = "torchwood " + version
	app.ExitCode = cli.RPCExitCode
	app.Register(
		cli.NewHealthCmd(g),
		cli.NewVersionCmd(g, version),
		cli.NewUUIDCmd(g),
		cli.NewProjectsCmd(g),
		cli.NewUsersCmd(g),
		cli.NewDatabasesCmd(g),
		cli.NewGroupsCmd(g),
		cli.NewStorageCmd(g),
		cli.NewFunctionsCmd(g),
		cli.NewOAuthProvidersCmd(g),
		cli.NewLeaderboardsCmd(g),
		cli.NewAnalyticsCmd(g),
		cli.NewPaymentsCmd(g),
		cli.NewAssetsCmd(g),
		cli.NewSubscriptionsCmd(g),
		cli.NewBillingCmd(g),
		cli.NewAdminCmd(g),
		cli.NewAuditLogsCmd(g),
		cli.NewConfigCmd(),
		cli.NewRPCCmd(g),
		cli.NewRunbookCmd(g),
	)
	return app
}
