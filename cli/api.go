// Package cli 是 Torchwood CLI（cmd/torchwood）的实现包：全部命令动词/
// 分组/全局旗标解析在此实现，cmd/torchwood 只保留根命令表装配。
//
// 本文件是包对装配方的导出面：cmd/torchwood（CLI 入口）只从这里取根命令表
// 的构造器与全局钩子；verb/group 等实现细节保持包内私有。

package cli

import (
	"github.com/lynx-go/commands"
)

// NewGlobalFlags 构造全局旗标载体（内建默认值就位；version 由 main 包
// ldflags 注入，供自报 UA 与 version 动词使用）。
func NewGlobalFlags(version string) *GlobalFlags {
	return &GlobalFlags{endpoint: defaultEndpoint, timeout: defaultTimeout, output: defaultOutput, version: version}
}

// NewHealthCmd 构造 health 命令组（无需 API key）。
func NewHealthCmd(g *GlobalFlags) commands.Command { return newHealthCmd(g) }

// NewVersionCmd 构造 version 动词。
func NewVersionCmd(g *GlobalFlags, version string) commands.Command { return newVersionCmd(g, version) }

// NewUUIDCmd 构造 uuid 动词（本地生成，无需 API key）。
func NewUUIDCmd(g *GlobalFlags) commands.Command { return newUUIDCmd(g) }

// NewProjectsCmd 构造 projects 命令组。
func NewProjectsCmd(g *GlobalFlags) commands.Command { return newProjectsCmd(g) }

// NewUsersCmd 构造 users 命令组。
func NewUsersCmd(g *GlobalFlags) commands.Command { return newUsersCmd(g) }

// NewDatabasesCmd 构造 databases 命令组。
func NewDatabasesCmd(g *GlobalFlags) commands.Command { return newDatabasesCmd(g) }

// NewGroupsCmd 构造 groups 命令组。
func NewGroupsCmd(g *GlobalFlags) commands.Command { return newGroupsCmd(g) }

// NewStorageCmd 构造 storage 命令组。
func NewStorageCmd(g *GlobalFlags) commands.Command { return newStorageCmd(g) }

// NewFunctionsCmd 构造 functions 命令组。
func NewFunctionsCmd(g *GlobalFlags) commands.Command { return newFunctionsCmd(g) }

// NewOAuthProvidersCmd 构造 oauth providers 命令组。
func NewOAuthProvidersCmd(g *GlobalFlags) commands.Command { return newOAuthProvidersCmd(g) }

// NewLeaderboardsCmd 构造 leaderboards 命令组。
func NewLeaderboardsCmd(g *GlobalFlags) commands.Command { return newLeaderboardsCmd(g) }

// NewAnalyticsCmd 构造 analytics 命令组。
func NewAnalyticsCmd(g *GlobalFlags) commands.Command { return newAnalyticsCmd(g) }

// NewPaymentsCmd 构造 payments 命令组。
func NewPaymentsCmd(g *GlobalFlags) commands.Command { return newPaymentsCmd(g) }

// NewAssetsCmd 构造 assets 命令组。
func NewAssetsCmd(g *GlobalFlags) commands.Command { return newAssetsCmd(g) }

// NewSubscriptionsCmd 构造 subscriptions 命令组。
func NewSubscriptionsCmd(g *GlobalFlags) commands.Command { return newSubscriptionsCmd(g) }

// NewBillingCmd 构造 billing 命令组。
func NewBillingCmd(g *GlobalFlags) commands.Command { return newBillingCmd(g) }

// NewAdminCmd 构造 admin 命令组（含直连 DB 的运维子命令）。
func NewAdminCmd(g *GlobalFlags) commands.Command { return newAdminCmd(g) }

// NewAuditLogsCmd 构造 audit-logs 命令组。
func NewAuditLogsCmd(g *GlobalFlags) commands.Command { return newAuditLogsCmd(g) }

// NewConfigCmd 构造 config 命令组（管理 ~/.torchwood/config.yaml）。
func NewConfigCmd() commands.Command { return newConfigCmd() }

// NewRPCCmd 构造 rpc 逃生舱动词（按 protoregistry 动态调用任意 Server RPC）。
func NewRPCCmd(g *GlobalFlags) commands.Command { return newRPCCmd(g) }

// NewRunbookCmd 构造 runbook 命令组（版本化资源迁移）。
func NewRunbookCmd(g *GlobalFlags) commands.Command { return newRunbookCmd(g) }
