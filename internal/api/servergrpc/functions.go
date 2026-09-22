package servergrpc

import (
	"context"
	"time"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appfunctions "github.com/torchwoodcloud/torchwood/internal/app/functions"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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

func (s *FunctionsService) ListRuntimes(ctx context.Context, _ *sharedv1.Empty) (*serverv1.ListRuntimesResponse, error) {
	if _, err := s.projectID(ctx); err != nil {
		return nil, err
	}
	out := s.functions.ListRuntimes()
	resp := &serverv1.ListRuntimesResponse{Runtimes: make([]*serverv1.RuntimeInfo, len(out))}
	for i, r := range out {
		ri := &serverv1.RuntimeInfo{
			Id:         r.ID,
			Name:       r.Name,
			Entrypoint: r.Entrypoint,
			// 运行时指定扩展（functions-runtime-selection.md §1/§6）：
			// family 对账轴 + 生命周期状态 + 缺省标记，Console 徽章与 CLI
			// 提示的数据源。
			Family:    r.Family,
			Status:    r.Status,
			IsDefault: r.IsDefault,
		}
		if r.EolAt != nil {
			ri.EolAt = timestamppb.New(*r.EolAt)
		}
		resp.Runtimes[i] = ri
	}
	return resp, nil
}

func (s *FunctionsService) ListSpecifications(ctx context.Context, _ *sharedv1.Empty) (*serverv1.ListSpecificationsResponse, error) {
	if _, err := s.projectID(ctx); err != nil {
		return nil, err
	}
	out := s.functions.ListSpecifications()
	resp := &serverv1.ListSpecificationsResponse{Specifications: make([]*serverv1.SpecificationInfo, len(out))}
	for i, sp := range out {
		resp.Specifications[i] = &serverv1.SpecificationInfo{Id: sp.ID, Cpu: sp.CPU, Memory: sp.Memory}
	}
	return resp, nil
}

func (s *FunctionsService) CreateFunction(ctx context.Context, req *serverv1.CreateFunctionRequest) (*serverv1.Function, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetId())
	cmd := appfunctions.CreateFunctionCommand{
		ID:             req.GetId(),
		ProjectID:      projectID,
		Name:           req.GetName(),
		Runtime:        req.GetRuntime(),
		Entrypoint:     req.GetEntrypoint(),
		Spec:           req.GetSpec(),
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
	fn, err := s.functions.CreateFunction(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapFunction(fn), nil
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
	start := params.Offset
	if start > len(fns) {
		start = len(fns)
	}
	end := start + int(params.PageSize)
	if end > len(fns) {
		end = len(fns)
	}
	page := fns[start:end]
	hasMore := end < len(fns)
	info := crud.BuildPaginationInfo(params, len(fns), hasMore)
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
	out := make([]*serverv1.Function, len(page))
	for i := range page {
		out[i] = mapFunction(&page[i])
	}
	return &serverv1.ListFunctionsResponse{
		Functions: out,
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      info.PageSize,
			TotalCount:    int32(info.TotalCount),
			NextPageToken: nextToken,
			PrevPageToken: prevToken,
		},
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
	if req.Entrypoint != nil {
		cmd.Entrypoint = req.Entrypoint
	}
	if req.TimeoutSeconds != nil {
		t := int(req.GetTimeoutSeconds())
		cmd.TimeoutSeconds = &t
	}
	if req.Spec != nil {
		cmd.Spec = req.Spec
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
	// 池策略（v3 §5/OQ2）：proto3 optional 的 presence 语义——未设置不修改；
	// 值域由 protovalidate 兜底，min≤max 跨字段校验在 app 用例层。
	if req.MinInstances != nil {
		v := int(req.GetMinInstances())
		cmd.MinInstances = &v
	}
	if req.MaxInstances != nil {
		v := int(req.GetMaxInstances())
		cmd.MaxInstances = &v
	}
	if req.IdleTtlSeconds != nil {
		v := int(req.GetIdleTtlSeconds())
		cmd.IdleTTLSeconds = &v
	}
	if req.MaxRequestsPerInstance != nil {
		v := int(req.GetMaxRequestsPerInstance())
		cmd.MaxRequestsPerInstance = &v
	}
	if req.Concurrency != nil {
		v := int(req.GetConcurrency())
		cmd.Concurrency = &v
	}
	// 运行时（functions-runtime-selection.md §3）：proto3 optional 的
	// presence 语义——未设置不修改；存在性/状态门在 app 用例层。
	if req.Runtime != nil {
		cmd.Runtime = req.Runtime
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

func (s *FunctionsService) CreateDeployment(ctx context.Context, req *serverv1.CreateDeploymentRequest) (*serverv1.Deployment, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId())
	// 部署源 oneof 分发（二期起，设计 §0）：code = zip 通道（≤1MiB；大包另经
	// serverhttp multipart，仍 zip-only）；git 映射为命令 Git 字段（packer
	// 物化为 zip）；image 映射为命令 Image 字段（BYO 免构建路径，三期阶段 1
	// ——DispatcherExecutor.ImportImage 阶段 2 接线，当前 Unimplemented 透
	// 传）。三者皆空 = InvalidArgument。
	cmd := appfunctions.CreateDeploymentCommand{
		ProjectID:  projectID,
		FunctionID: req.GetFunctionId(),
	}
	switch req.GetSource().(type) {
	case *serverv1.CreateDeploymentRequest_Code:
		cmd.Code = req.GetCode()
	case *serverv1.CreateDeploymentRequest_Git:
		cmd.Git = mapGitSource(req.GetGit())
	case *serverv1.CreateDeploymentRequest_Image:
		cmd.Image = mapImageSource(req.GetImage())
	default:
		return nil, status.Error(codes.InvalidArgument, "source is required: set either code (zip), git, or image")
	}
	dep, err := s.functions.CreateDeployment(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapDeployment(dep), nil
}

func (s *FunctionsService) ListDeployments(ctx context.Context, req *serverv1.ListDeploymentsRequest) (*serverv1.ListDeploymentsResponse, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	deps, next, err := s.functions.ListDeployments(ctx, projectID, req.GetFunctionId(), int(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	resp := &serverv1.ListDeploymentsResponse{
		Deployments: make([]*serverv1.Deployment, len(deps)),
		Meta:        &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), NextPageToken: next},
	}
	for i := range deps {
		resp.Deployments[i] = mapDeployment(&deps[i])
	}
	return resp, nil
}

func (s *FunctionsService) GetDeployment(ctx context.Context, req *serverv1.GetDeploymentRequest) (*serverv1.Deployment, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	dep, err := s.functions.GetDeployment(ctx, projectID, req.GetFunctionId(), req.GetDeploymentId())
	if err != nil {
		return nil, err
	}
	return mapDeployment(dep), nil
}

func (s *FunctionsService) DeleteDeployment(ctx context.Context, req *serverv1.GetDeploymentRequest) (*sharedv1.Empty, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId()+"/deployments/"+req.GetDeploymentId())
	if err := s.functions.DeleteDeployment(ctx, projectID, req.GetFunctionId(), req.GetDeploymentId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *FunctionsService) SetVariables(ctx context.Context, req *serverv1.SetVariablesRequest) (*serverv1.Variables, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId())
	vars := make(map[string]string, len(req.GetVariables()))
	for _, v := range req.GetVariables() {
		vars[v.GetKey()] = v.GetValue()
	}
	out, err := s.functions.SetVariables(ctx, projectID, req.GetFunctionId(), vars)
	if err != nil {
		return nil, err
	}
	return mapVariables(out), nil
}

func (s *FunctionsService) GetVariables(ctx context.Context, req *serverv1.GetFunctionRequest) (*serverv1.Variables, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	vars, err := s.functions.GetVariables(ctx, projectID, req.GetFunctionId())
	if err != nil {
		return nil, err
	}
	return mapVariables(vars), nil
}

func (s *FunctionsService) CreateExecution(ctx context.Context, req *serverv1.CreateExecutionRequest) (*serverv1.Execution, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, req.GetFunctionId()+"/executions")
	cmd := appfunctions.CreateExecutionCommand{
		ProjectID:  projectID,
		FunctionID: req.GetFunctionId(),
		Data:       req.GetData(),
		Async:      req.GetAsync(),
	}
	if req.DeploymentId != nil {
		cmd.DeploymentID = req.GetDeploymentId()
	}

	if !cmd.Async {
		// 同步执行兜底超时：fn.TimeoutSeconds + 60s 余量（gRPC 直连路径），
		// 超时映射 DeadlineExceeded（HTTP 504）。
		fn, err := s.functions.GetFunction(ctx, projectID, req.GetFunctionId())
		if err != nil {
			return nil, err
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(fn.TimeoutSeconds)*time.Second+60*time.Second)
		defer cancel()
	}

	rec, err := s.functions.CreateExecution(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapExecution(rec), nil
}

func (s *FunctionsService) ListExecutions(ctx context.Context, req *serverv1.ListExecutionsRequest) (*serverv1.ListExecutionsResponse, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	flt := domainfunctions.ExecutionListFilter{Status: req.GetStatus()}
	if ts := req.GetCreatedAfter(); ts != nil {
		flt.CreatedAfter = ts.AsTime()
	}
	if ts := req.GetCreatedBefore(); ts != nil {
		flt.CreatedBefore = ts.AsTime()
	}
	recs, next, err := s.functions.ListExecutions(ctx, projectID, req.GetFunctionId(), int(req.GetPageSize()), req.GetPageToken(), flt)
	if err != nil {
		return nil, err
	}
	resp := &serverv1.ListExecutionsResponse{
		Executions: make([]*serverv1.Execution, len(recs)),
		Meta:       &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), NextPageToken: next},
	}
	for i := range recs {
		resp.Executions[i] = mapExecution(&recs[i])
	}
	return resp, nil
}

func (s *FunctionsService) GetExecution(ctx context.Context, req *serverv1.GetExecutionRequest) (*serverv1.Execution, error) {
	projectID, err := s.projectID(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.functions.GetExecution(ctx, projectID, req.GetFunctionId(), req.GetExecutionId())
	if err != nil {
		return nil, err
	}
	return mapExecution(rec), nil
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
		Runtime:        fn.Runtime,
		Entrypoint:     fn.Entrypoint,
		TimeoutSeconds: int32(fn.TimeoutSeconds),
		Spec:           fn.Spec,
		Enabled:        fn.Enabled,
		DeclaredScopes: fn.DeclaredScopes,
		// 客户端调用面策略（P2，设计 §4）。
		ClientCallable:         fn.ClientCallable,
		ClientAnonymousAllowed: fn.ClientAnonymousAllowed,
		ClientPerUserLimit:     int32(fn.ClientPerUserLimit),
		ClientLimitWindow:      fn.ClientLimitWindow,
		// 池策略只读视图（v3 §5/OQ2；管理经 UpdateFunction optional ×5）。
		MinInstances:           int32(fn.MinInstances),
		MaxInstances:           int32(fn.MaxInstances),
		IdleTtlSeconds:         int32(fn.IdleTTLSeconds),
		MaxRequestsPerInstance: int32(fn.MaxRequestsPerInstance),
		Concurrency:            int32(fn.Concurrency),
		CreatedAt:              timestamppb.New(fn.CreatedAt),
		UpdatedAt:              timestamppb.New(fn.UpdatedAt),
	}
}

func mapDeployment(d *domainfunctions.Deployment) *serverv1.Deployment {
	if d == nil {
		return nil
	}
	return &serverv1.Deployment{
		Id:         d.ID,
		FunctionId: d.FunctionID,
		Size:       d.Size,
		Status:     d.Status,
		Error:      d.Error,
		// 部署源只读投影（二期，迁移 000023）：git url/钉死 SHA/子目录原样；
		// zip 源四字段恒为 source_type=zip + 空串。漏映射不报编译错，投影
		// 形状由 functions_test.go 显式断言（设计 §0）。
		SourceType: d.SourceType,
		SourceUrl:  d.SourceURL,
		SourceRef:  d.SourceRef,
		SourceDir:  d.SourceDir,
		// 构建所用 runtime ID 快照（functions-runtime-selection.md §4，迁移
		// 000025）：INSERT 期写全、之后不可变。
		Runtime:   d.Runtime,
		CreatedAt: timestamppb.New(d.CreatedAt),
		UpdatedAt: timestamppb.New(d.UpdatedAt),
	}
}

// mapGitSource 把 proto GitSource 映射为领域值对象（一次性凭证随结构体
// 仅在请求生命周期内存活——不落库不回显，D8）。
func mapGitSource(g *serverv1.GitSource) *domainfunctions.GitSource {
	if g == nil {
		return nil
	}
	return &domainfunctions.GitSource{
		URL:       g.GetUrl(),
		Ref:       g.GetRef(),
		Directory: g.GetDirectory(),
		Username:  g.GetUsername(),
		Token:     g.GetToken(),
	}
}

// mapImageSource 把 proto ImageSource 映射为领域值对象（三期阶段 1，设计
// §3；一次性 registry 凭证随结构体仅在请求生命周期内存活——不落库不回显，
// D8）。
func mapImageSource(g *serverv1.ImageSource) *domainfunctions.ImageSource {
	if g == nil {
		return nil
	}
	return &domainfunctions.ImageSource{
		Reference:        g.GetImage(),
		RegistryUsername: g.GetRegistryUsername(),
		RegistryToken:    g.GetRegistryToken(),
	}
}

func mapVariables(vars map[string]string) *serverv1.Variables {
	out := &serverv1.Variables{}
	for k, v := range vars {
		out.Variables = append(out.Variables, &serverv1.Variable{Key: k, Value: v})
	}
	return out
}

func mapExecution(rec *domainfunctions.ExecutionRecord) *serverv1.Execution {
	if rec == nil {
		return nil
	}
	return &serverv1.Execution{
		Id:                rec.ID,
		FunctionId:        rec.FunctionID,
		DeploymentId:      rec.DeploymentID,
		Status:            rec.Status,
		Response:          rec.Response,
		Stdout:            rec.Stdout,
		Stderr:            rec.Stderr,
		StatusCode:        int32(rec.StatusCode),
		DurationMs:        rec.DurationMS,
		Error:             rec.Error,
		ResponseTruncated: rec.ResponseTruncated,
		StdoutTruncated:   rec.StdoutTruncated,
		StderrTruncated:   rec.StderrTruncated,
		CreatedAt:         timestamppb.New(rec.CreatedAt),
		UpdatedAt:         timestamppb.New(rec.UpdatedAt),
	}
}
