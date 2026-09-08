package servergrpc

import (
	"context"

	serverv1 "github.com/torchwooddev/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
	appserver "github.com/torchwooddev/torchwood/internal/app/server"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type ProjectsService struct {
	serverv1.UnimplementedProjectsServiceServer
	projects *appserver.Projects
	invites  *appserver.InviteCodes
}

func NewProjectsService(projects *appserver.Projects, invites *appserver.InviteCodes) *ProjectsService {
	return &ProjectsService{projects: projects, invites: invites}
}

func (s *ProjectsService) CreateProject(ctx context.Context, req *serverv1.CreateProjectRequest) (*serverv1.Project, error) {
	p, err := s.projects.CreateProject(ctx, appserver.CreateProjectCommand{
		ID:          req.GetId(),
		Name:        req.GetName(),
		Description: req.GetDescription(),
	})
	if err != nil {
		return nil, err
	}
	return mapProject(p), nil
}

func (s *ProjectsService) ListProjects(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListProjectsResponse, error) {
	list, info, err := s.projects.ListProjects(ctx, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	var nextToken, prevToken string
	if info.HasNext {
		if nextToken, err = crud.EncodePageToken(info.NextOffset); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if info.HasPrevious {
		if prevToken, err = crud.EncodePageToken(info.PreviousOffset); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	resp := &serverv1.ListProjectsResponse{
		Projects: make([]*serverv1.Project, len(list)),
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      info.PageSize,
			NextPageToken: nextToken,
			PrevPageToken: prevToken,
			TotalCount:    int32(info.TotalCount),
		},
	}
	for i, p := range list {
		resp.Projects[i] = mapProject(&p)
	}
	return resp, nil
}

func (s *ProjectsService) GetProject(ctx context.Context, req *serverv1.GetProjectRequest) (*serverv1.Project, error) {
	p, err := s.projects.GetProject(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if p == nil {
		// 对齐 GetUser 模式：use-case 返回 nil,nil 时显式转 NotFound，
		// 避免 gRPC OK + 空响应的错误分类。
		return nil, status.Error(codes.NotFound, "project not found")
	}
	return mapProject(p), nil
}

func (s *ProjectsService) UpdateProject(ctx context.Context, req *serverv1.UpdateProjectRequest) (*serverv1.Project, error) {
	ctx = contexts.WithAuditResource(ctx, req.GetId())
	cmd := appserver.UpdateProjectCommand{ProjectID: req.GetId()}
	if req.Name != nil {
		cmd.Name = req.Name
	}
	if req.Description != nil {
		cmd.Description = req.Description
	}
	if req.RegistrationPolicy != nil {
		cmd.RegistrationPolicy = req.RegistrationPolicy
	}
	p, err := s.projects.UpdateProject(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapProject(p), nil
}

func (s *ProjectsService) DeleteProject(ctx context.Context, req *serverv1.GetProjectRequest) (*sharedv1.Empty, error) {
	ctx = contexts.WithAuditResource(ctx, req.GetId())
	if err := s.projects.DeleteProject(ctx, req.GetId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

// UpdateOAuthRedirectAllowlist 更新项目重定向白名单（整表替换；空列表 =
// 清空回落默认白名单）。
func (s *ProjectsService) UpdateOAuthRedirectAllowlist(ctx context.Context, req *serverv1.UpdateOAuthRedirectAllowlistRequest) (*serverv1.Project, error) {
	ctx = contexts.WithAuditResource(ctx, req.GetProjectId()+"/oauth-redirect-allowlist")
	p, err := s.projects.UpdateOAuthRedirectAllowlist(ctx, appserver.UpdateOAuthRedirectAllowlistCommand{
		ProjectID: req.GetProjectId(),
		URLs:      req.GetUrls(),
	})
	if err != nil {
		return nil, err
	}
	return mapProject(p), nil
}

func mapProject(p *projects.Project) *serverv1.Project {
	if p == nil {
		return nil
	}
	return &serverv1.Project{
		Id:                       p.ID,
		Name:                     p.Name,
		Description:              p.Description,
		Status:                   p.Status,
		RegistrationPolicy:       p.RegistrationPolicy,
		OauthAllowedRedirectUrls: projects.OAuthAllowedRedirectURLs(p.Settings),
		CreatedAt:                timestamppb.New(p.CreatedAt),
		UpdatedAt:                timestamppb.New(p.UpdatedAt),
	}
}

// ---- 邀请码（T-03）----

func (s *ProjectsService) CreateInviteCode(ctx context.Context, req *serverv1.CreateInviteCodeRequest) (*serverv1.InviteCode, error) {
	ctx = contexts.WithAuditResource(ctx, req.GetProjectId()+"/invite-codes")
	cmd := appserver.CreateInviteCodeCommand{ProjectID: req.GetProjectId()}
	if req.MaxUses != nil {
		cmd.MaxUses = req.MaxUses
	}
	if ts := req.GetExpireAt(); ts != nil {
		t := ts.AsTime()
		cmd.ExpireAt = &t
	}
	code, err := s.invites.Create(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapInviteCode(code), nil
}

func (s *ProjectsService) ListInviteCodes(ctx context.Context, req *serverv1.ListInviteCodesRequest) (*serverv1.ListInviteCodesResponse, error) {
	codes, err := s.invites.List(ctx, req.GetProjectId())
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.InviteCode, len(codes))
	for i := range codes {
		out[i] = mapInviteCode(&codes[i])
	}
	return &serverv1.ListInviteCodesResponse{
		InviteCodes: out,
		Meta:        &sharedv1.ListResponseMeta{PageSize: int32(len(out))},
	}, nil
}

func (s *ProjectsService) DeleteInviteCode(ctx context.Context, req *serverv1.DeleteInviteCodeRequest) (*sharedv1.Empty, error) {
	ctx = contexts.WithAuditResource(ctx, req.GetProjectId()+"/invite-codes/"+req.GetId())
	if err := s.invites.Delete(ctx, req.GetProjectId(), req.GetId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func mapInviteCode(c *projects.InviteCode) *serverv1.InviteCode {
	if c == nil {
		return nil
	}
	out := &serverv1.InviteCode{
		Id:        c.ID,
		ProjectId: c.ProjectID,
		Code:      c.Code,
		MaxUses:   int32(c.MaxUses),
		UsedCount: int32(c.UsedCount),
		Revoked:   c.Revoked(),
		CreatedBy: c.CreatedBy,
		CreatedAt: timestamppb.New(c.CreatedAt),
	}
	if c.ExpireAt != nil {
		out.ExpireAt = timestamppb.New(*c.ExpireAt)
	}
	return out
}
