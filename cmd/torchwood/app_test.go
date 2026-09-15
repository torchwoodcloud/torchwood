package main

import (
	"testing"
)

// rootCommandNames 是根命令表全量名单（与 app.go 的 Register 顺序一致）。
var rootCommandNames = []string{
	"health",
	"version",
	"uuid",
	"projects",
	"users",
	"databases",
	"groups",
	"storage",
	"functions",
	"oauth-providers",
	"leaderboards",
	"analytics",
	"payments",
	"assets",
	"subscriptions",
	"billing",
	"admin",
	"audit-logs",
	"config",
	"rpc",
	"runbook",
}

// TestNewAppRegistersRootCommands 守卫 CLI 装配：根命令表逐一可解析，
// 帮助面版本串随 ldflags 注入值拼接。
func TestNewAppRegistersRootCommands(t *testing.T) {
	app := NewApp("vtest")
	for _, name := range rootCommandNames {
		if _, ok := app.Lookup(name); !ok {
			t.Errorf("root command %q not registered", name)
		}
	}
	if _, ok := app.Lookup("no-such-verb"); ok {
		t.Error("unknown verb resolved from root command table")
	}
}
