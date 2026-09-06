package consolegrpc

import (
	"context"

	consolev1 "github.com/torchwooddev/torchwood/genproto/console/v1"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
	"github.com/torchwooddev/torchwood/internal/app/console"
	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type AdminsService struct {
	consolev1.UnimplementedAdminsServiceServer
	admins *console.Admins
}

func NewAdminsService(admins *console.Admins) *AdminsService {
	return &AdminsService{admins: admins}
}

func (s *AdminsService) GetCurrentAdmin(ctx context.Context, _ *consolev1.GetCurrentAdminRequest) (*consolev1.Admin, error) {
	p, ok := principalFrom(ctx)
	if !ok || p.ActorKind != shared.ActorKindAdmin || !p.IsAuthenticated() {
		return nil, status.Error(codes.Unauthenticated, "admin context missing")
	}
	admin, err := s.admins.Get(ctx, callerID(ctx))
	if err != nil {
		return nil, err
	}
	return mapAdmin(admin), nil
}

func (s *AdminsService) ListAdmins(ctx context.Context, req *consolev1.ListAdminsRequest) (*consolev1.ListAdminsResponse, error) {
	if err := appshared.RequireConsolePrincipal(ctx); err != nil {
		return nil, err
	}
	// P3-9：ListAdmins 分页（走 crud，内存分页，total_count 精确）
	params, err := crud.ParseListParams(req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	admins, err := s.admins.List(ctx)
	if err != nil {
		return nil, err
	}
	start := params.Offset
	if start > len(admins) {
		start = len(admins)
	}
	end := start + int(params.PageSize)
	if end > len(admins) {
		end = len(admins)
	}
	page := admins[start:end]
	hasMore := end < len(admins)
	info := crud.BuildPaginationInfo(params, len(admins), hasMore)
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
	out := make([]*consolev1.Admin, len(page))
	for i := range page {
		out[i] = mapAdmin(&page[i])
	}
	return &consolev1.ListAdminsResponse{
		Admins: out,
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      info.PageSize,
			TotalCount:    int32(info.TotalCount),
			NextPageToken: nextToken,
			PrevPageToken: prevToken,
		},
	}, nil
}

func (s *AdminsService) CreateAdmin(ctx context.Context, req *consolev1.CreateAdminRequest) (*consolev1.Admin, error) {
	if err := appshared.RequireConsolePrincipal(ctx); err != nil {
		return nil, err
	}
	admin, err := s.admins.Create(ctx, console.CreateAdminCommand{
		Email:    req.GetEmail(),
		Password: req.GetPassword(),
		Role:     req.GetRole(),
	})
	if err != nil {
		return nil, err
	}
	return mapAdmin(admin), nil
}

func (s *AdminsService) UpdateAdmin(ctx context.Context, req *consolev1.UpdateAdminRequest) (*consolev1.Admin, error) {
	if err := appshared.RequireConsolePrincipal(ctx); err != nil {
		return nil, err
	}
	// role 为 optional（R10-P1-6）：未设置 = 不修改；设置（含空串）= 更新/清空。
	// use-case 对空串同样按"不修改"处理（UpdateAdminCommand.Role=="" 跳过）。
	cmd := console.UpdateAdminCommand{
		ID:       req.GetId(),
		CallerID: callerID(ctx),
		Password: req.GetPassword(),
	}
	if req.Role != nil {
		cmd.Role = req.GetRole()
	}
	admin, err := s.admins.Update(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapAdmin(admin), nil
}

func (s *AdminsService) DeleteAdmin(ctx context.Context, req *consolev1.DeleteAdminRequest) (*sharedv1.Empty, error) {
	if err := appshared.RequireConsolePrincipal(ctx); err != nil {
		return nil, err
	}
	if err := s.admins.Delete(ctx, req.GetId(), callerID(ctx)); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func principalFrom(ctx context.Context) (*shared.Principal, bool) {
	return contexts.Principal(ctx)
}

func callerID(ctx context.Context) string {
	p, ok := principalFrom(ctx)
	if !ok {
		return ""
	}
	return p.AdminLookupID()
}

func mapAdmin(a *projects.Admin) *consolev1.Admin {
	if a == nil {
		return nil
	}
	return &consolev1.Admin{
		Id:        a.ID,
		Email:     a.Email,
		Role:      a.Role,
		CreatedAt: timestamppb.New(a.CreatedAt),
		UpdatedAt: timestamppb.New(a.UpdatedAt),
	}
}
