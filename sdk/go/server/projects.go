package server

import (
	"context"

	serverv1 "github.com/torchwooddev/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
)

// ProjectsService 封装 Server API 的 Projects 服务。
type ProjectsService struct {
	c   *Client
	api serverv1.ProjectsServiceClient
}

// CreateProject 创建项目（限平台 admin）。
func (s *ProjectsService) CreateProject(ctx context.Context, req *serverv1.CreateProjectRequest) (*serverv1.Project, error) {
	return s.api.CreateProject(ctx, req)
}

// ListProjects 列出项目。
func (s *ProjectsService) ListProjects(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListProjectsResponse, error) {
	return s.api.ListProjects(ctx, req)
}

// GetProject 按 ID 获取项目。
func (s *ProjectsService) GetProject(ctx context.Context, req *serverv1.GetProjectRequest) (*serverv1.Project, error) {
	return s.api.GetProject(ctx, req)
}

// UpdateProject 更新项目字段（空值不修改）。
func (s *ProjectsService) UpdateProject(ctx context.Context, req *serverv1.UpdateProjectRequest) (*serverv1.Project, error) {
	return s.api.UpdateProject(ctx, req)
}

// DeleteProject 删除项目（限平台 admin；级联清理项目 schema 与 public 行）。
func (s *ProjectsService) DeleteProject(ctx context.Context, req *serverv1.GetProjectRequest) (*sharedv1.Empty, error) {
	return s.api.DeleteProject(ctx, req)
}

// CreateInviteCode 生成邀请码（T-03；限平台 owner/admin，key 凭证禁入）。
func (s *ProjectsService) CreateInviteCode(ctx context.Context, req *serverv1.CreateInviteCodeRequest) (*serverv1.InviteCode, error) {
	return s.api.CreateInviteCode(ctx, req)
}

// ListInviteCodes 列出项目邀请码（T-03）。
func (s *ProjectsService) ListInviteCodes(ctx context.Context, req *serverv1.ListInviteCodesRequest) (*serverv1.ListInviteCodesResponse, error) {
	return s.api.ListInviteCodes(ctx, req)
}

// DeleteInviteCode 吊销邀请码（T-03，软删）。
func (s *ProjectsService) DeleteInviteCode(ctx context.Context, req *serverv1.DeleteInviteCodeRequest) (*sharedv1.Empty, error) {
	return s.api.DeleteInviteCode(ctx, req)
}

// UpdateOAuthRedirectAllowlist 更新项目 OAuth 重定向白名单（整表替换；
// 空列表 = 清空回落默认白名单；限平台 owner/admin，key 凭证禁入）。
func (s *ProjectsService) UpdateOAuthRedirectAllowlist(ctx context.Context, req *serverv1.UpdateOAuthRedirectAllowlistRequest) (*serverv1.Project, error) {
	return s.api.UpdateOAuthRedirectAllowlist(ctx, req)
}
