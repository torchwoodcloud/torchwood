package cmd

import (
	"flag"

	"github.com/lynx-go/commands"
)

const methodAuditLogsList = "/torchwood.server.v1.AuditLogsService/ListAuditLogs"

// newAuditLogsCmd 覆盖 AuditLogsService：list（结构化过滤 + 分页，JSON 输出）。
// 项目上下文来自凭证（API key 绑定项目）；include-platform/all-projects
// 仅平台 admin 凭证可用（项目 key 调用会得 PermissionDenied）。
func newAuditLogsCmd(g *globalFlags) *group {
	return newGroup(g, "audit-logs", "audit log queries (AuditLogsService)", func(sub *commands.App) {
		sub.Register(newAuditLogsListCmd(g))
	})
}

func newAuditLogsListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken, actorID, actorKind, action, status, resourceID, createdAfter, createdBefore string
	var includePlatform, allProjects bool
	return newVerb(g, "list", "list audit logs (filters are exact match; timestamps RFC3339)",
		"audit-logs list [--action] [--status] [--actor-id] [--actor-kind] [--resource-id] [--created-after] [--created-before] [--include-platform] [--all-projects] [--page-size] [--page-token]",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default 50, max 1000)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
			fs.StringVar(&actorID, "actor-id", "", "filter by actor id (end_user id / admin id / api key id)")
			fs.StringVar(&actorKind, "actor-kind", "", "filter by actor kind (end_user | admin | service | execution | system)")
			fs.StringVar(&action, "action", "", "filter by gRPC full method, e.g. /torchwood.server.v1.FunctionsService/UpdateFunction")
			fs.StringVar(&status, "status", "", "filter by status (success | denied | throttled | grpc code)")
			fs.StringVar(&resourceID, "resource-id", "", "filter by resource instance id")
			fs.StringVar(&createdAfter, "created-after", "", "created_at >= (RFC3339, e.g. 2026-09-01T00:00:00Z)")
			fs.StringVar(&createdBefore, "created-before", "", "created_at <= (RFC3339)")
			fs.BoolVar(&includePlatform, "include-platform", false, "also include platform-level rows (project_id IS NULL; platform admin only)")
			fs.BoolVar(&allProjects, "all-projects", false, "cross-project view including platform rows (platform admin only)")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			req := listJSON(pageSize, pageToken)
			if actorID != "" {
				req["actorId"] = actorID
			}
			if actorKind != "" {
				req["actorKind"] = actorKind
			}
			if action != "" {
				req["action"] = action
			}
			if status != "" {
				req["status"] = status
			}
			if resourceID != "" {
				req["resourceId"] = resourceID
			}
			if createdAfter != "" {
				req["createdAfter"] = createdAfter
			}
			if createdBefore != "" {
				req["createdBefore"] = createdBefore
			}
			if includePlatform {
				req["includePlatform"] = true
			}
			if allProjects {
				req["allProjects"] = true
			}
			return call(g, env, methodAuditLogsList, req)
		})
}
