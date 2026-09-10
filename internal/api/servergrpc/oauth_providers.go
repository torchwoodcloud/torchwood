package servergrpc

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type OAuthProvidersService struct {
	serverv1.UnimplementedOAuthProvidersServiceServer
	oauthProviders *appserver.OAuthProviders
}

func NewOAuthProvidersService(oauthProviders *appserver.OAuthProviders) *OAuthProvidersService {
	return &OAuthProvidersService{oauthProviders: oauthProviders}
}

func (s *OAuthProvidersService) ListOAuthProviders(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListOAuthProvidersResponse, error) {
	params, err := crud.ParseListParams(req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	items, err := s.oauthProviders.List(ctx, projectID)
	if err != nil {
		return nil, err
	}
	start := params.Offset
	if start > len(items) {
		start = len(items)
	}
	end := start + int(params.PageSize)
	if end > len(items) {
		end = len(items)
	}
	page := items[start:end]
	hasMore := end < len(items)
	info := crud.BuildPaginationInfo(params, len(items), hasMore)
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
	out := make([]*serverv1.OAuthProvider, len(page))
	for i := range page {
		out[i] = mapOAuthProvider(&page[i])
	}
	return &serverv1.ListOAuthProvidersResponse{
		OauthProviders: out,
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      info.PageSize,
			TotalCount:    int32(info.TotalCount),
			NextPageToken: nextToken,
			PrevPageToken: prevToken,
		},
	}, nil
}

func (s *OAuthProvidersService) UpsertOAuthProvider(ctx context.Context, req *serverv1.UpsertOAuthProviderRequest) (*serverv1.OAuthProvider, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	stored, err := s.oauthProviders.Upsert(ctx, appserver.UpsertOAuthProviderCommand{
		ProjectID:    projectID,
		Provider:     req.GetProvider(),
		Enabled:      req.GetEnabled(),
		ClientID:     req.GetClientId(),
		ClientSecret: req.GetClientSecret(),
		Scopes:       req.GetScopes(),
	})
	if err != nil {
		return nil, err
	}
	return mapOAuthProvider(stored), nil
}

func (s *OAuthProvidersService) DeleteOAuthProvider(ctx context.Context, req *serverv1.DeleteOAuthProviderRequest) (*sharedv1.Empty, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if err := s.oauthProviders.Delete(ctx, projectID, req.GetProvider()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func mapOAuthProvider(cfg *projects.OAuthProvider) *serverv1.OAuthProvider {
	if cfg == nil {
		return nil
	}
	out := &serverv1.OAuthProvider{
		Provider:        cfg.Provider,
		Enabled:         cfg.Enabled,
		ClientId:        cfg.ClientID,
		HasClientSecret: cfg.ClientSecret != "",
		Scopes:          append([]string(nil), cfg.Scopes...),
	}
	if !cfg.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(cfg.CreatedAt)
	}
	if !cfg.UpdatedAt.IsZero() {
		out.UpdatedAt = timestamppb.New(cfg.UpdatedAt)
	}
	return out
}

func projectIDFromContext(ctx context.Context) string {
	p, ok := contexts.Principal(ctx)
	if !ok {
		return ""
	}
	return p.ProjectID
}
