package cmd

import (
	"github.com/lynx-go/commands"
)

// newAdminCmd 提供平台运维管理命令（W-J）。outbox 走 Server RPC（需 API
// key）；export/import、schema、sync-roles-sig 直连数据库（不经 InvokeJSON/
// API 面）：导出需要 tw_system 旁路身份与 catalog/outbox 直读，POC 运维工
// 具属性允许直连，DSN 走 --dsn/环境变量，因此不挂全局旗标、不做 api-key 校验。
func newAdminCmd(g *globalFlags) *group {
	return newGroup(g, "admin", "平台运维管理（outbox 死信、项目 export/import 等）", func(sub *commands.App) {
		sub.Register(
			newOutboxCmd(g),
			newAdminExportCmd(),
			newAdminImportCmd(),
			newAdminSchemaCmd(),
			newAdminSyncRolesSigCmd(),
		)
	})
}
