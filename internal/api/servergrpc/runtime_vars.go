package servergrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// RuntimeVarsService 是 server 面运行时变量管理的 gRPC handler：透传 app
// 用例（校验/编排/错误映射全在用例层），本层只做 proto ↔ domain 映射 +
// 项目上下文提取 + actor 前缀 + 审计资源标注（D8：审计按 FullMethod 自动
// 落库，handler 只需 WithAuditResource）。
type RuntimeVarsService struct {
	serverv1.UnimplementedRuntimeVarsServiceServer
	runtimeVars *appserver.RuntimeVars
}

func NewRuntimeVarsService(runtimeVars *appserver.RuntimeVars) *RuntimeVarsService {
	return &RuntimeVarsService{runtimeVars: runtimeVars}
}

// ---- 集合 ----

func (s *RuntimeVarsService) CreateVarSet(ctx context.Context, req *serverv1.CreateVarSetRequest) (*serverv1.VarSet, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "runtime_var_sets/"+req.GetVarSetId())
	view, err := s.runtimeVars.CreateVarSet(ctx, appserver.CreateVarSetCommand{
		ProjectID:   projectID,
		VarSetID:    req.GetVarSetId(),
		Visibility:  visibilityFromProto(req.GetVisibility()),
		Description: req.GetDescription(),
	})
	if err != nil {
		return nil, err
	}
	return mapVarSetView(view), nil
}

func (s *RuntimeVarsService) ListVarSets(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListVarSetsResponse, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	views, err := s.runtimeVars.ListVarSets(ctx, projectID)
	if err != nil {
		return nil, err
	}
	params, err := crud.ParseListParams(req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	start := params.Offset
	if start > len(views) {
		start = len(views)
	}
	end := start + int(params.PageSize)
	if end > len(views) {
		end = len(views)
	}
	page := views[start:end]
	hasMore := end < len(views)
	info := crud.BuildPaginationInfo(params, len(views), hasMore)
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
	out := make([]*serverv1.VarSet, len(page))
	for i := range page {
		out[i] = mapVarSetView(&page[i])
	}
	return &serverv1.ListVarSetsResponse{
		VarSets: out,
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      info.PageSize,
			TotalCount:    int32(info.TotalCount),
			NextPageToken: nextToken,
			PrevPageToken: prevToken,
		},
	}, nil
}

func (s *RuntimeVarsService) GetVarSet(ctx context.Context, req *serverv1.GetVarSetRequest) (*serverv1.VarSet, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "runtime_var_sets/"+req.GetVarSetId())
	view, err := s.runtimeVars.GetVarSet(ctx, projectID, req.GetVarSetId())
	if err != nil {
		return nil, err
	}
	return mapVarSetView(view), nil
}

func (s *RuntimeVarsService) UpdateVarSet(ctx context.Context, req *serverv1.UpdateVarSetRequest) (*serverv1.VarSet, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "runtime_var_sets/"+req.GetVarSetId())
	cmd := appserver.UpdateVarSetCommand{
		ProjectID: projectID,
		VarSetID:  req.GetVarSetId(),
	}
	if req.Visibility != nil {
		v := visibilityFromProto(req.GetVisibility())
		cmd.Visibility = &v
	}
	if req.Description != nil {
		cmd.Description = req.Description
	}
	view, err := s.runtimeVars.UpdateVarSet(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapVarSetView(view), nil
}

func (s *RuntimeVarsService) DeleteVarSet(ctx context.Context, req *serverv1.DeleteVarSetRequest) (*sharedv1.Empty, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "runtime_var_sets/"+req.GetVarSetId())
	if err := s.runtimeVars.DeleteVarSet(ctx, projectID, req.GetVarSetId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

// ---- 变量 ----

func (s *RuntimeVarsService) CreateRuntimeVar(ctx context.Context, req *serverv1.CreateRuntimeVarRequest) (*serverv1.RuntimeVar, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, runtimeVarAuditResource(req.GetVarSetId(), req.GetKey()))
	value, err := runtimeVarValueInput(req.GetValue())
	if err != nil {
		return nil, err
	}
	created, err := s.runtimeVars.CreateRuntimeVar(ctx, appserver.CreateRuntimeVarCommand{
		ProjectID:   projectID,
		VarSetID:    req.GetVarSetId(),
		Key:         req.GetKey(),
		Value:       value,
		Description: req.GetDescription(),
		Actor:       runtimeVarActorFromContext(ctx),
	})
	if err != nil {
		return nil, err
	}
	return mapRuntimeVar(created), nil
}

func (s *RuntimeVarsService) ListRuntimeVars(ctx context.Context, req *serverv1.ListRuntimeVarsRequest) (*serverv1.ListRuntimeVarsResponse, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "runtime_var_sets/"+req.GetVarSetId()+"/vars")
	vars, err := s.runtimeVars.ListRuntimeVars(ctx, projectID, req.GetVarSetId())
	if err != nil {
		return nil, err
	}
	params, err := crud.ParseListParams(req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	start := params.Offset
	if start > len(vars) {
		start = len(vars)
	}
	end := start + int(params.PageSize)
	if end > len(vars) {
		end = len(vars)
	}
	page := vars[start:end]
	hasMore := end < len(vars)
	info := crud.BuildPaginationInfo(params, len(vars), hasMore)
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
	out := make([]*serverv1.RuntimeVar, len(page))
	for i := range page {
		out[i] = mapRuntimeVar(&page[i])
	}
	return &serverv1.ListRuntimeVarsResponse{
		Vars: out,
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      info.PageSize,
			TotalCount:    int32(info.TotalCount),
			NextPageToken: nextToken,
			PrevPageToken: prevToken,
		},
	}, nil
}

func (s *RuntimeVarsService) GetRuntimeVar(ctx context.Context, req *serverv1.GetRuntimeVarRequest) (*serverv1.RuntimeVar, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, runtimeVarAuditResource(req.GetVarSetId(), req.GetKey()))
	v, err := s.runtimeVars.GetRuntimeVar(ctx, projectID, req.GetVarSetId(), req.GetKey())
	if err != nil {
		return nil, err
	}
	return mapRuntimeVar(v), nil
}

func (s *RuntimeVarsService) UpdateRuntimeVar(ctx context.Context, req *serverv1.UpdateRuntimeVarRequest) (*serverv1.RuntimeVar, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, runtimeVarAuditResource(req.GetVarSetId(), req.GetKey()))
	value, err := runtimeVarValueInput(req.GetValue())
	if err != nil {
		return nil, err
	}
	updated, err := s.runtimeVars.UpdateRuntimeVar(ctx, appserver.UpdateRuntimeVarCommand{
		ProjectID:   projectID,
		VarSetID:    req.GetVarSetId(),
		Key:         req.GetKey(),
		Value:       value,
		Description: req.Description,
		Actor:       runtimeVarActorFromContext(ctx),
	})
	if err != nil {
		return nil, err
	}
	return mapRuntimeVar(updated), nil
}

func (s *RuntimeVarsService) DeleteRuntimeVar(ctx context.Context, req *serverv1.DeleteRuntimeVarRequest) (*sharedv1.Empty, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, runtimeVarAuditResource(req.GetVarSetId(), req.GetKey()))
	if err := s.runtimeVars.DeleteRuntimeVar(ctx, projectID, req.GetVarSetId(), req.GetKey(), runtimeVarActorFromContext(ctx)); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

// ---- 版本链 ----

func (s *RuntimeVarsService) ListRuntimeVarVersions(ctx context.Context, req *serverv1.ListRuntimeVarVersionsRequest) (*serverv1.ListRuntimeVarVersionsResponse, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "runtime_var_sets/"+req.GetVarSetId()+"/versions")
	versions, err := s.runtimeVars.ListRuntimeVarVersions(ctx, projectID, req.GetVarSetId())
	if err != nil {
		return nil, err
	}
	params, err := crud.ParseListParams(req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	start := params.Offset
	if start > len(versions) {
		start = len(versions)
	}
	end := start + int(params.PageSize)
	if end > len(versions) {
		end = len(versions)
	}
	page := versions[start:end]
	hasMore := end < len(versions)
	info := crud.BuildPaginationInfo(params, len(versions), hasMore)
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
	out := make([]*serverv1.RuntimeVarVersion, len(page))
	for i := range page {
		out[i] = mapRuntimeVarVersion(&page[i])
	}
	return &serverv1.ListRuntimeVarVersionsResponse{
		Versions: out,
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      info.PageSize,
			TotalCount:    int32(info.TotalCount),
			NextPageToken: nextToken,
			PrevPageToken: prevToken,
		},
	}, nil
}

func (s *RuntimeVarsService) GetRuntimeVarVersion(ctx context.Context, req *serverv1.GetRuntimeVarVersionRequest) (*serverv1.GetRuntimeVarVersionResponse, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "runtime_var_sets/"+req.GetVarSetId()+"/versions/"+strconv.FormatInt(req.GetRevision(), 10))
	detail, err := s.runtimeVars.GetRuntimeVarVersion(ctx, projectID, req.GetVarSetId(), req.GetRevision())
	if err != nil {
		return nil, err
	}
	entries := make([]*serverv1.RuntimeVarVersionEntry, len(detail.Vars))
	for i := range detail.Vars {
		v := detail.Vars[i]
		value, err := runtimeVarValueFromStored(v.ValueType, v.Value)
		if err != nil {
			return nil, err
		}
		entry := &serverv1.RuntimeVarVersionEntry{
			Key:         v.Key,
			Value:       value,
			Description: v.Description,
		}
		if !v.CreatedAt.IsZero() {
			entry.CreatedAt = timestamppb.New(v.CreatedAt)
		}
		entries[i] = entry
	}
	return &serverv1.GetRuntimeVarVersionResponse{
		Version: mapRuntimeVarVersion(&detail.Version),
		Entries: entries,
	}, nil
}

func (s *RuntimeVarsService) RollbackRuntimeVar(ctx context.Context, req *serverv1.RollbackRuntimeVarRequest) (*serverv1.RuntimeVarVersion, error) {
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "runtime_var_sets/"+req.GetVarSetId())
	rolled, err := s.runtimeVars.Rollback(ctx, projectID, req.GetVarSetId(), req.GetTargetRevision(), runtimeVarActorFromContext(ctx))
	if err != nil {
		return nil, err
	}
	return mapRuntimeVarVersion(rolled), nil
}

// ---- 映射 ----

func mapVarSetView(v *appserver.VarSetView) *serverv1.VarSet {
	if v == nil {
		return nil
	}
	out := &serverv1.VarSet{
		VarSetId:    v.VarSet.VarSetID,
		Visibility:  visibilityToProto(v.VarSet.Visibility),
		Description: v.VarSet.Description,
		Etag:        v.ETag,
		Revision:    v.Revision,
		VarCount:    int32(v.VarCount),
		TotalBytes:  v.TotalBytes,
	}
	if !v.VarSet.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(v.VarSet.CreatedAt)
	}
	if !v.VarSet.UpdatedAt.IsZero() {
		out.UpdatedAt = timestamppb.New(v.VarSet.UpdatedAt)
	}
	return out
}

func mapRuntimeVar(v *projects.RuntimeVar) *serverv1.RuntimeVar {
	if v == nil {
		return nil
	}
	value, err := runtimeVarValueFromStored(v.ValueType, v.Value)
	if err != nil {
		// 存储侧值必为写路径构造的合法 JSON；此处失败 = 数据损坏。
		value = &serverv1.RuntimeVarValue{}
	}
	out := &serverv1.RuntimeVar{
		VarSetId:    v.VarSetID,
		Key:         v.Key,
		Value:       value,
		Description: v.Description,
	}
	if !v.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(v.CreatedAt)
	}
	if !v.UpdatedAt.IsZero() {
		out.UpdatedAt = timestamppb.New(v.UpdatedAt)
	}
	return out
}

func mapRuntimeVarVersion(v *projects.RuntimeVarVersion) *serverv1.RuntimeVarVersion {
	if v == nil {
		return nil
	}
	out := &serverv1.RuntimeVarVersion{
		Revision: v.Revision,
		Action:   versionActionToProto(v.Action),
		Summary:  v.Summary,
		Actor:    v.Actor,
	}
	if !v.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(v.CreatedAt)
	}
	return out
}

// visibilityFromProto 映射 proto 可见性 → domain 串；UNSPECIFIED → 空
// （app 层归一为 public 缺省）。
func visibilityFromProto(v serverv1.VarSetVisibility) string {
	switch v {
	case serverv1.VarSetVisibility_VAR_SET_VISIBILITY_PRIVATE:
		return projects.VarSetVisibilityPrivate
	default:
		return ""
	}
}

func visibilityToProto(v string) serverv1.VarSetVisibility {
	switch v {
	case projects.VarSetVisibilityPrivate:
		return serverv1.VarSetVisibility_VAR_SET_VISIBILITY_PRIVATE
	default:
		return serverv1.VarSetVisibility_VAR_SET_VISIBILITY_PUBLIC
	}
}

func versionActionToProto(a string) serverv1.RuntimeVarVersionAction {
	switch a {
	case projects.RuntimeVarActionCreate:
		return serverv1.RuntimeVarVersionAction_RUNTIME_VAR_VERSION_ACTION_CREATE
	case projects.RuntimeVarActionUpdate:
		return serverv1.RuntimeVarVersionAction_RUNTIME_VAR_VERSION_ACTION_UPDATE
	case projects.RuntimeVarActionDelete:
		return serverv1.RuntimeVarVersionAction_RUNTIME_VAR_VERSION_ACTION_DELETE
	case projects.RuntimeVarActionRollback:
		return serverv1.RuntimeVarVersionAction_RUNTIME_VAR_VERSION_ACTION_ROLLBACK
	default:
		return serverv1.RuntimeVarVersionAction_RUNTIME_VAR_VERSION_ACTION_UNSPECIFIED
	}
}

// runtimeVarValueInput 映射 proto oneof → app 值输入（oneof 未设置 →
// InvalidArgument；类型构造/校验在 app 构造器内）。
func runtimeVarValueInput(v *serverv1.RuntimeVarValue) (appserver.RuntimeVarValueInput, error) {
	if v == nil {
		return appserver.RuntimeVarValueInput{}, status.Error(codes.InvalidArgument, "value is required")
	}
	switch kind := v.GetKind().(type) {
	case *serverv1.RuntimeVarValue_StringValue:
		return appserver.StringVarValue(kind.StringValue), nil
	case *serverv1.RuntimeVarValue_IntegerValue:
		return appserver.IntegerVarValue(kind.IntegerValue), nil
	case *serverv1.RuntimeVarValue_FloatValue:
		return appserver.FloatVarValue(kind.FloatValue)
	case *serverv1.RuntimeVarValue_BoolValue:
		return appserver.BoolVarValue(kind.BoolValue), nil
	case *serverv1.RuntimeVarValue_JsonValue:
		return appserver.JSONVarValue(kind.JsonValue)
	default:
		return appserver.RuntimeVarValueInput{}, status.Error(codes.InvalidArgument, "value kind is required")
	}
}

// runtimeVarValueFromStored 把存储侧（value_type, JSON 文本）还原为 oneof。
func runtimeVarValueFromStored(valueType, valueJSON string) (*serverv1.RuntimeVarValue, error) {
	switch valueType {
	case projects.RuntimeVarTypeString:
		var s string
		if err := json.Unmarshal([]byte(valueJSON), &s); err != nil {
			return nil, status.Errorf(codes.Internal, "corrupt runtime var value: %v", err)
		}
		return &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_StringValue{StringValue: s}}, nil
	case projects.RuntimeVarTypeInteger:
		var n int64
		if err := json.Unmarshal([]byte(valueJSON), &n); err != nil {
			return nil, status.Errorf(codes.Internal, "corrupt runtime var value: %v", err)
		}
		return &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_IntegerValue{IntegerValue: n}}, nil
	case projects.RuntimeVarTypeFloat:
		var f float64
		if err := json.Unmarshal([]byte(valueJSON), &f); err != nil {
			return nil, status.Errorf(codes.Internal, "corrupt runtime var value: %v", err)
		}
		return &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_FloatValue{FloatValue: f}}, nil
	case projects.RuntimeVarTypeBoolean:
		var b bool
		if err := json.Unmarshal([]byte(valueJSON), &b); err != nil {
			return nil, status.Errorf(codes.Internal, "corrupt runtime var value: %v", err)
		}
		return &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_BoolValue{BoolValue: b}}, nil
	case projects.RuntimeVarTypeJSON:
		return &serverv1.RuntimeVarValue{Kind: &serverv1.RuntimeVarValue_JsonValue{JsonValue: valueJSON}}, nil
	default:
		return nil, status.Errorf(codes.Internal, "unknown runtime var value type %q", valueType)
	}
}

// runtimeVarAuditResource 构造 var 级审计资源标注。
func runtimeVarAuditResource(varSetID, key string) string {
	return fmt.Sprintf("runtime_var_sets/%s/vars/%s", varSetID, key)
}

// runtimeVarActorFromContext 从 Principal 派生 actor 前缀（§2.5：admin:<id> /
// apikey:<id> / function:<id>）；server 面写动词的凭证族只有前两类。
func runtimeVarActorFromContext(ctx context.Context) string {
	p, ok := contexts.Principal(ctx)
	if !ok || p == nil {
		return ""
	}
	switch p.ActorKind {
	case shared.ActorKindAdmin:
		return "admin:" + p.AdminID
	case shared.ActorKindService:
		return "apikey:" + p.APIKeyID
	case shared.ActorKindExecution:
		return "function:" + p.FunctionID
	default:
		return ""
	}
}
