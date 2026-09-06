package cmd

import "github.com/spf13/cobra"

// NewAdminCmd 提供平台运维管理命令（W-J）。
func NewAdminCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "平台运维管理（outbox 死信、项目 export/import 等）",
	}
	cmd.AddCommand(NewOutboxCmd(g))
	// B5 export/import 直连数据库（不经 InvokeJSON/API 面）：导出需要
	// tw_system 旁路身份与 catalog/outbox 直读，POC 运维工具属性允许直连；
	// 不接 globalFlags（无 server 地址依赖），DSN 走 --dsn/环境变量。
	cmd.AddCommand(newAdminExportCmd(), newAdminImportCmd())
	// B3 schema 漂移对账（缺列/INVALID 索引/幽灵表修复，redesign §4.4）：
	// 与启动钩子后台 reconcile 同源逻辑，支持 --dry-run。
	cmd.AddCommand(newAdminSchemaCmd())
	// B15 roles 签名密钥落库作业（部署期 owner 一次性作业）：运行态启动钩子
	// 已退役（000004 收口 authenticator 对 tw_secrets 零权限），换钥后重跑。
	cmd.AddCommand(newAdminSyncRolesSigCmd())
	return cmd
}
