package servergrpc

import (
	"context"
	"fmt"
	"time"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type UsersService struct {
	serverv1.UnimplementedUsersServiceServer
	users *appserver.Users
}

func NewUsersService(users *appserver.Users) *UsersService {
	return &UsersService{users: users}
}

func (s *UsersService) projectID(ctx context.Context) string {
	p, ok := contexts.Principal(ctx)
	if !ok {
		return ""
	}
	return p.ProjectID
}

func (s *UsersService) CreateUser(ctx context.Context, req *serverv1.CreateUserRequest) (*serverv1.User, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	doc, err := s.users.CreateUser(ctx, projectID, appserver.CreateUserCommand{
		Email:    req.GetEmail(),
		Password: req.GetPassword(),
		Name:     req.GetName(),
		Status:   req.GetStatus(),
		Labels:   structToStringSlice(req.GetLabels()),
		Prefs:    req.GetPrefs().AsMap(),
	})
	if err != nil {
		return nil, err
	}
	return mapUserDoc(doc), nil
}

func (s *UsersService) ListUsers(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListUsersResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	docs, total, next, err := s.users.ListUsers(ctx, projectID, databases.Query{
		Queries:   req.GetQueries(),
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
	}, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.User, len(docs))
	for i := range docs {
		out[i] = mapUserDoc(&docs[i])
	}
	return &serverv1.ListUsersResponse{
		Users: out,
		Meta:  &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), TotalCount: int32(total), NextPageToken: next},
	}, nil
}

func (s *UsersService) GetUser(ctx context.Context, req *serverv1.GetUserRequest) (*serverv1.User, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	doc, err := s.users.GetUser(ctx, projectID, req.GetId(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	return mapUserDoc(doc), nil
}

func (s *UsersService) UpdateUser(ctx context.Context, req *serverv1.UpdateUserRequest) (*serverv1.User, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	updates := map[string]any{}
	// D-1 presence 语义：optional 字段以非 nil 判断（本仓生成物无 HasXxx），
	// 「未设置=不修改、设置含空串=更新/清空」，范本 servergrpc/projects.go UpdateProject。
	if req.Status != nil {
		updates["status"] = req.GetStatus()
	}
	if req.GetLabels() != nil {
		updates["labels"] = structToStringSlice(req.GetLabels())
	}
	if req.GetPrefs() != nil {
		updates["prefs"] = req.GetPrefs().AsMap()
	}
	if req.Name != nil {
		updates["name"] = req.GetName()
	}
	if req.Email != nil {
		updates["email"] = req.GetEmail()
	}
	if req.EmailVerified != nil {
		updates["email_verified"] = req.GetEmailVerified()
	}
	doc, err := s.users.UpdateUser(ctx, projectID, req.GetId(), updates, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return mapUserDoc(doc), nil
}

func (s *UsersService) UpdateUserPassword(ctx context.Context, req *serverv1.UpdateUserPasswordRequest) (*serverv1.User, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	doc, err := s.users.UpdateUserPassword(ctx, projectID, req.GetId(), req.GetPassword())
	if err != nil {
		return nil, err
	}
	return mapUserDoc(doc), nil
}

func (s *UsersService) DeleteUser(ctx context.Context, req *serverv1.GetUserRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if err := s.users.DeleteUser(ctx, projectID, req.GetId(), dbPrincipal(ctx)); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *UsersService) ListUserSessions(ctx context.Context, req *serverv1.GetUserRequest) (*serverv1.ListUserSessionsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	docs, err := s.users.ListUserSessions(ctx, projectID, req.GetId())
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.Session, len(docs))
	for i := range docs {
		out[i] = mapSessionDoc(&docs[i])
	}
	return &serverv1.ListUserSessionsResponse{Sessions: out}, nil
}

func (s *UsersService) DeleteUserSession(ctx context.Context, req *serverv1.DeleteUserSessionRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if err := s.users.DeleteUserSession(ctx, projectID, req.GetId(), req.GetSessionId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *UsersService) CreateUserToken(ctx context.Context, req *serverv1.GetUserRequest) (*serverv1.CreateUserTokenResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	bundle, err := s.users.CreateUserToken(ctx, projectID, req.GetId())
	if err != nil {
		return nil, err
	}
	return &serverv1.CreateUserTokenResponse{
		Tokens: &serverv1.TokenBundle{
			AccessToken:  bundle.AccessToken,
			RefreshToken: bundle.RefreshToken,
			ExpiresAt:    timestamppb.New(time.Unix(bundle.ExpiresAt, 0)),
		},
	}, nil
}

func mapUserDoc(doc *databases.Document) *serverv1.User {
	if doc == nil {
		return nil
	}
	u := &serverv1.User{
		Id:        doc.ID,
		CreatedAt: timestamppb.New(doc.CreatedAt),
		UpdatedAt: timestamppb.New(doc.UpdatedAt),
	}
	if v, ok := doc.Data["email"].(string); ok {
		u.Email = v
	}
	if v, ok := doc.Data["name"].(string); ok {
		u.Name = v
	}
	if v, ok := doc.Data["status"].(string); ok {
		u.Status = v
	}
	if v, ok := doc.Data["email_verified"].(bool); ok {
		u.EmailVerified = v
	}
	if v, ok := doc.Data["phone"].(string); ok {
		u.Phone = v
	}
	if v, ok := doc.Data["labels"]; ok {
		if labels, err := structpb.NewValue(v); err == nil {
			u.Labels = labels.GetStructValue()
		}
	}
	if v, ok := doc.Data["prefs"]; ok {
		if prefs, err := structpb.NewValue(v); err == nil {
			u.Prefs = prefs.GetStructValue()
		}
	}
	return u
}

func mapSessionDoc(doc *databases.Document) *serverv1.Session {
	if doc == nil {
		return nil
	}
	s := &serverv1.Session{
		Id:        doc.ID,
		UserId:    stringValue(doc.Data["user_id"]),
		Provider:  stringValue(doc.Data["provider"]),
		UserAgent: stringValue(doc.Data["user_agent"]),
		Ip:        stringValue(doc.Data["ip"]),
		CreatedAt: timestamppb.New(doc.CreatedAt),
	}
	if expireAtRaw, ok := doc.Data["expire_at"]; ok {
		if expireAt, err := parseSessionTime(expireAtRaw); err == nil {
			s.ExpireAt = timestamppb.New(expireAt)
		}
	}
	return s
}

// parseSessionTime decodes session expire_at values from document storage
// （原位于 infra/auth.ParseSessionTime，现下沉至消费端以消除 api→infra 依赖，
// 逻辑保持一致：time.Time 直返，string 按 RFC3339Nano 解析）。
func parseSessionTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case string:
		return time.Parse(time.RFC3339Nano, t)
	}
	return time.Time{}, fmt.Errorf("unsupported time type")
}

// structToStringSlice 将 Struct（数组值）转为 []any 字符串数组；Appwrite 的
// labels 语义是字符串数组，宽容接受字符串与数字标量。
func structToStringSlice(v *structpb.Struct) []any {
	if v == nil {
		return nil
	}
	lv, ok := v.Fields["values"]
	if !ok {
		return nil
	}
	list := lv.GetListValue()
	if list == nil {
		return nil
	}
	out := make([]any, 0, len(list.Values))
	for _, item := range list.Values {
		switch val := item.AsInterface().(type) {
		case string:
			out = append(out, val)
		case float64:
			out = append(out, val)
		default:
			out = append(out, fmt.Sprint(val))
		}
	}
	return out
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}
