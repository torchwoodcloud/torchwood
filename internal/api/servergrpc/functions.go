package servergrpc

import (
	"context"
	"time"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	apishared "github.com/torchwoodcloud/torchwood/internal/api/shared"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// FunctionsService 是 Functions v4 的 gRPC 面（docs/design/functions-v4.md）：
// 函数 CRUD + endpoint target + 调用（InvokeFunction/invocations）+ 触发器。
// 部署/变量/运行时 RPC 随执行面外包退役（§10）。
type FunctionsService struct {
	serverv1.UnimplementedFunctionsServiceServer
	functions *appfunctions.Functions
}

func NewFunctionsService(functions *appfunctions.Functions) *FunctionsService {
	return &FunctionsService{functions: functions}
}

func (s *FunctionsService) projectID(ctx context.Context) (string, error) {
	p, ok := contexts.Principal(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing project context")
	}
	if p.ProjectID == "" {
		return "", status.Error(codes.Unauthenticated, "missing project context")
	}
	return p.ProjectID, nil
}

func (s *FunctionsService) CreateFunction(ctx context.Context, req *serverv1.CreateFunctionRequest) (*serverv1.CreateFunctionResponse, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetId())
	cmd := appfunctions.CreateFunctionCommand{
		ID:             req.GetId(),
		ProjectID:      projectID,
		Name:           req.GetName(),
		TargetURL:      req.GetTargetUrl(),
		TargetSecret:   req.GetTargetSecret(),
		Enabled:        req.Enabled,
		DeclaredScopes: req.GetDeclaredScopes(),
		// 客户端调用面策略（P2，设计 §4）。
		ClientCallable:         req.ClientCallable,
		ClientAnonymousAllowed: req.ClientAnonymousAllowed,
	}
	if req.TimeoutSeconds != nil {
		t := int(req.GetTimeoutSeconds())
		cmd.TimeoutSeconds = &t
	}
	if req.ClientPerUserLimit != nil {
		l := int(req.GetClientPerUserLimit())
		cmd.ClientPerUserLimit = &l
	}
	if req.ClientLimitWindow != nil {
		cmd.ClientLimitWindow = req.ClientLimitWindow
	}
	fn, secret, err := s.functions.CreateFunctionWithSecret(ctx, cmd)
	if err != nil {
		return nil, err
	}
	// target 签名密钥明文一次性回显（v4 §3 实施勘误；之后任何面只出指纹）。
	return &serverv1.CreateFunctionResponse{Function: mapFunction(fn), TargetSecret: secret}, nil
}

func (s *FunctionsService) ListFunctions(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListFunctionsResponse, error) {
	params, err := crud.ParseListParams(req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	fns, err := s.functions.ListFunctions(ctx, projectID)
	if err != nil {
		return nil, err
	}
	page, info, err := crud.SliceOffsetPage(fns, params)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*serverv1.Function, len(page))
	for i := range page {
		out[i] = mapFunction(&page[i])
	}
	return &serverv1.ListFunctionsResponse{
		Functions: out,
		Meta:      apishared.OffsetPageMeta(info),
	}, nil
}

func (s *FunctionsService) GetFunction(ctx context.Context, req *serverv1.GetFunctionRequest) (*serverv1.Function, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	fn, err := s.functions.GetFunction(ctx, projectID, req.GetFunctionId())
	if err != nil {
		return nil, err
	}
	return mapFunction(fn), nil
}

func (s *FunctionsService) UpdateFunction(ctx context.Context, req *serverv1.UpdateFunctionRequest) (*serverv1.Function, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId())
	cmd := appfunctions.UpdateFunctionCommand{
		ProjectID:  projectID,
		FunctionID: req.GetFunctionId(),
	}
	if req.Name != nil {
		cmd.Name = req.Name
	}
	if req.TimeoutSeconds != nil {
		t := int(req.GetTimeoutSeconds())
		cmd.TimeoutSeconds = &t
	}
	if req.TargetUrl != nil {
		cmd.TargetURL = req.TargetUrl
	}
	if req.Enabled != nil {
		cmd.Enabled = req.Enabled
	}
	// 客户端调用面策略（P2）：proto3 optional 的 presence 语义——未设置不修改。
	if req.ClientCallable != nil {
		cmd.ClientCallable = req.ClientCallable
	}
	if req.ClientAnonymousAllowed != nil {
		cmd.ClientAnonymousAllowed = req.ClientAnonymousAllowed
	}
	if req.ClientPerUserLimit != nil {
		l := int(req.GetClientPerUserLimit())
		cmd.ClientPerUserLimit = &l
	}
	if req.ClientLimitWindow != nil {
		cmd.ClientLimitWindow = req.ClientLimitWindow
	}
	fn, err := s.functions.UpdateFunction(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapFunction(fn), nil
}

func (s *FunctionsService) DeleteFunction(ctx context.Context, req *serverv1.GetFunctionRequest) (*sharedv1.Empty, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId())
	if err := s.functions.DeleteFunction(ctx, projectID, req.GetFunctionId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

// InvokeFunction 调用一个函数（v4 §2：CreateExecution 改名——一个动作一个
// 名字，与 client 面同名对齐；async 经 option）。
func (s *FunctionsService) InvokeFunction(ctx context.Context, req *serverv1.InvokeFunctionRequest) (*serverv1.Invocation, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId()+":invoke")
	cmd := appfunctions.InvokeCommand{
		ProjectID:  projectID,
		FunctionID: req.GetFunctionId(),
		Data:       req.GetData(),
		Async:      req.GetAsync(),
	}

	if !cmd.Async {
		// 同步调用兜底超时：fn.TimeoutSeconds + 60s 余量（gRPC 直连路径），
		// 超时映射 DeadlineExceeded（HTTP 504）。
		fn, err := s.functions.GetFunction(ctx, projectID, req.GetFunctionId())
		if err != nil {
			return nil, err
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(fn.TimeoutSeconds)*time.Second+60*time.Second)
		defer cancel()
	}

	rec, err := s.functions.Invoke(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapInvocation(rec), nil
}

func (s *FunctionsService) ListInvocations(ctx context.Context, req *serverv1.ListInvocationsRequest) (*serverv1.ListInvocationsResponse, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	flt := domainfunctions.InvocationListFilter{Status: req.GetStatus()}
	if ts := req.GetCreatedAfter(); ts != nil {
		flt.CreatedAfter = ts.AsTime()
	}
	if ts := req.GetCreatedBefore(); ts != nil {
		flt.CreatedBefore = ts.AsTime()
	}
	recs, next, err := s.functions.ListInvocations(ctx, projectID, req.GetFunctionId(), int(req.GetPageSize()), req.GetPageToken(), flt)
	if err != nil {
		return nil, err
	}
	resp := &serverv1.ListInvocationsResponse{
		Invocations: make([]*serverv1.Invocation, len(recs)),
		Meta:        &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), NextPageToken: next},
	}
	for i := range recs {
		resp.Invocations[i] = mapInvocation(&recs[i])
	}
	return resp, nil
}

func (s *FunctionsService) GetInvocation(ctx context.Context, req *serverv1.GetInvocationRequest) (*serverv1.Invocation, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.functions.GetInvocation(ctx, projectID, req.GetFunctionId(), req.GetInvocationId())
	if err != nil {
		return nil, err
	}
	return mapInvocation(rec), nil
}

// SetFunctionScopes 全量替换函数 declared_scopes（P0 执行身份）。
func (s *FunctionsService) SetFunctionScopes(ctx context.Context, req *serverv1.SetFunctionScopesRequest) (*serverv1.Function, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId()+"/scopes")
	fn, err := s.functions.SetFunctionScopes(ctx, projectID, req.GetFunctionId(), req.GetDeclaredScopes())
	if err != nil {
		return nil, err
	}
	return mapFunction(fn), nil
}

// RotateFunctionSecret 轮换 endpoint target 签名密钥（新明文一次性回显）。
func (s *FunctionsService) RotateFunctionSecret(ctx context.Context, req *serverv1.RotateFunctionSecretRequest) (*serverv1.FunctionSecret, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId()+":rotate-secret")
	fn, secret, err := s.functions.RotateFunctionSecret(ctx, projectID, req.GetFunctionId())
	if err != nil {
		return nil, err
	}
	return &serverv1.FunctionSecret{
		FunctionId:        fn.ID,
		Secret:            secret,
		SecretFingerprint: fn.TargetSecretFingerprint,
	}, nil
}

// ——触发器管理（P1 触发器模块）——

func (s *FunctionsService) CreateFunctionTrigger(ctx context.Context, req *serverv1.CreateFunctionTriggerRequest) (*serverv1.FunctionTrigger, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId()+"/triggers")
	cmd := appfunctions.CreateTriggerCommand{
		ProjectID:  projectID,
		FunctionID: req.GetFunctionId(),
		Type:       req.GetType(),
		HTTP:       mapHTTPTriggerConfig(req.GetHttp()),
		Cron:       mapCronTriggerConfig(req.GetCron()),
		Events:     req.GetEvent().GetEvents(), // v3 §4.1：type=event 专用
		Enabled:    nil,                        // 创建恒启用；启停经删除重建（P1 无 UpdateFunctionTrigger RPC）。
	}
	trg, err := s.functions.CreateFunctionTrigger(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapFunctionTrigger(trg), nil
}

func (s *FunctionsService) ListFunctionTriggers(ctx context.Context, req *serverv1.GetFunctionRequest) (*serverv1.ListFunctionTriggersResponse, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	trgs, err := s.functions.ListFunctionTriggers(ctx, projectID, req.GetFunctionId())
	if err != nil {
		return nil, err
	}
	resp := &serverv1.ListFunctionTriggersResponse{Triggers: make([]*serverv1.FunctionTrigger, len(trgs))}
	for i := range trgs {
		resp.Triggers[i] = mapFunctionTrigger(&trgs[i])
	}
	return resp, nil
}

func (s *FunctionsService) DeleteFunctionTrigger(ctx context.Context, req *serverv1.DeleteFunctionTriggerRequest) (*sharedv1.Empty, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId()+"/triggers/"+req.GetTriggerId())
	if err := s.functions.DeleteFunctionTrigger(ctx, projectID, req.GetFunctionId(), req.GetTriggerId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *FunctionsService) RotateFunctionTriggerToken(ctx context.Context, req *serverv1.RotateFunctionTriggerTokenRequest) (*serverv1.FunctionTrigger, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId()+"/triggers/"+req.GetTriggerId()+":rotate-token")
	trg, err := s.functions.RotateFunctionTriggerToken(ctx, projectID, req.GetFunctionId(), req.GetTriggerId())
	if err != nil {
		return nil, err
	}
	return mapFunctionTrigger(trg), nil
}

func mapHTTPTriggerConfig(cfg *serverv1.HttpTriggerConfig) domainfunctions.TriggerConfig {
	if cfg == nil {
		return domainfunctions.TriggerConfig{}
	}
	out := domainfunctions.TriggerConfig{ResponseMode: cfg.GetResponseMode()}
	if cfg.AckBody != nil {
		out.AckBody = *cfg.AckBody
	}
	if cfg.Handshake != nil {
		out.Handshake = *cfg.Handshake
	}
	if cfg.BodyLimitBytes != nil {
		out.BodyLimitBytes = int(*cfg.BodyLimitBytes)
	}
	return out
}

func mapCronTriggerConfig(cfg *serverv1.CronTriggerConfig) domainfunctions.TriggerConfig {
	if cfg == nil {
		return domainfunctions.TriggerConfig{}
	}
	return domainfunctions.TriggerConfig{Expr: cfg.GetExpr(), Misfire: cfg.GetMisfire()}
}

func mapFunctionTrigger(trg *domainfunctions.Trigger) *serverv1.FunctionTrigger {
	if trg == nil {
		return nil
	}
	out := &serverv1.FunctionTrigger{
		Id:         trg.ID,
		FunctionId: trg.FunctionID,
		Type:       trg.Type,
		Enabled:    trg.Enabled,
		CreatedAt:  timestamppb.New(trg.CreatedAt),
		UpdatedAt:  timestamppb.New(trg.UpdatedAt),
	}
	switch trg.Type {
	case domainfunctions.TriggerTypeHTTP:
		out.ResponseMode = trg.Config.ResponseMode
		out.AckBody = trg.Config.AckBody
		out.Handshake = trg.Config.Handshake
		out.BodyLimitBytes = int32(trg.Config.EffectiveBodyLimit())
		out.Token = trg.Token
		out.InvokePath = "/f/" + trg.ProjectID + "/" + trg.Token
	case domainfunctions.TriggerTypeCron:
		out.Expr = trg.Config.Expr
		out.Misfire = trg.Config.Misfire
		if trg.NextRunAt != nil {
			out.NextRunAt = timestamppb.New(*trg.NextRunAt)
		}
	case domainfunctions.TriggerTypeEvent:
		out.Events = trg.Config.Events
	}
	return out
}

func mapFunction(fn *domainfunctions.Function) *serverv1.Function {
	if fn == nil {
		return nil
	}
	return &serverv1.Function{
		Id:             fn.ID,
		ProjectId:      fn.ProjectID,
		Name:           fn.Name,
		TimeoutSeconds: int32(fn.TimeoutSeconds),
		Enabled:        fn.Enabled,
		DeclaredScopes: fn.DeclaredScopes,
		// 客户端调用面策略（P2，设计 §4）。
		ClientCallable:         fn.ClientCallable,
		ClientAnonymousAllowed: fn.ClientAnonymousAllowed,
		ClientPerUserLimit:     int32(fn.ClientPerUserLimit),
		ClientLimitWindow:      fn.ClientLimitWindow,
		// endpoint target（v4 §3）：密钥任何形态不回显，只出指纹。
		Target:    mapFunctionTarget(fn),
		CreatedAt: timestamppb.New(fn.CreatedAt),
		UpdatedAt: timestamppb.New(fn.UpdatedAt),
	}
}

func mapFunctionTarget(fn *domainfunctions.Function) *serverv1.FunctionTarget {
	if fn == nil || fn.TargetURL == "" {
		return nil
	}
	out := &serverv1.FunctionTarget{
		Url:               fn.TargetURL,
		SecretFingerprint: fn.TargetSecretFingerprint,
	}
	if !fn.TargetSecretRotatedAt.IsZero() {
		out.SecretRotatedAt = timestamppb.New(fn.TargetSecretRotatedAt)
	}
	return out
}

func mapInvocation(rec *domainfunctions.InvocationRecord) *serverv1.Invocation {
	if rec == nil {
		return nil
	}
	return &serverv1.Invocation{
		Id:                rec.ID,
		FunctionId:        rec.FunctionID,
		Status:            rec.Status,
		Response:          rec.Response,
		StatusCode:        int32(rec.StatusCode),
		DurationMs:        rec.DurationMS,
		Error:             rec.Error,
		ResponseTruncated: rec.ResponseTruncated,
		CreatedAt:         timestamppb.New(rec.CreatedAt),
		UpdatedAt:         timestamppb.New(rec.UpdatedAt),
	}
}
