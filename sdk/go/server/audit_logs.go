package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
)

// AuditLogsService 封装 Server API 的审计日志查询。
type AuditLogsService struct {
	c   *Client
	api serverv1.AuditLogsServiceClient
}

// ListAuditLogs 查询审计日志：项目上下文来自凭证（API key 绑定项目；
// 结构化过滤 exact 匹配，时间范围闭区间）。include_platform/all_projects
// 仅平台 admin 可用。
func (s *AuditLogsService) ListAuditLogs(ctx context.Context, req *serverv1.ListAuditLogsRequest) (*serverv1.ListAuditLogsResponse, error) {
	return s.api.ListAuditLogs(ctx, req)
}
