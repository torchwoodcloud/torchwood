package functions

import (
	"context"
	"regexp"
	"strings"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// functionIDPattern 限制 Function ID 字符集与长度（防路径穿越拼入 zip 路径
// 与镜像名；须以小写字母/数字开头，仅含小写字母/数字/下划线/连字符，最长 64；
// 大写禁用：Docker 镜像仓库/标签名只允许小写，见 G6-3/R08-P1-1）。
var functionIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

const (
	// minTimeoutSeconds / maxTimeoutSeconds 是函数超时允许范围（§5.2）。
	minTimeoutSeconds = 1
	maxTimeoutSeconds = 300
	// maxSyncTimeoutSeconds 限制同步执行超时（grpc-gateway WriteTimeout 余量）。
	maxSyncTimeoutSeconds = 30
	// defaultTimeoutSeconds 是创建函数未显式指定 timeout_seconds 时的服务端默认值
	// （与 Console 前端默认值 15 及 DB 列 DEFAULT 15 保持一致）。
	defaultTimeoutSeconds = 15

	// ——池策略值域（P0.5 执行器 v2 + v3 多路复用；与 DB CHECK 约束同源，
	// 迁移 000014/000017；docs/design/functions-v3.md §5/OQ2）——
	minMinInstances    = 0
	minMaxInstances    = 1
	minIdleTTLSeconds  = 30
	minMaxRequests     = 1
	minConcurrency     = 1
	maxConcurrency     = 16 // DB CHECK 与迁移 000017 同源（D2：8×16=128 够单机拓扑）
	defaultConcurrency = 1
)

type CreateFunctionCommand struct {
	ID             string
	ProjectID      string
	Name           string
	Runtime        string
	Entrypoint     string
	TimeoutSeconds *int
	Spec           string
	Enabled        *bool
	DeclaredScopes []string
	// ——客户端调用面策略（P2，设计 §4；nil = 未设置）——
	ClientCallable         *bool
	ClientAnonymousAllowed *bool
	ClientPerUserLimit     *int
	ClientLimitWindow      *string
}

type UpdateFunctionCommand struct {
	ProjectID  string
	FunctionID string
	Name       *string
	Entrypoint *string
	// TimeoutSeconds 范围 [1,300]（Create 同）。
	TimeoutSeconds *int
	Spec           *string
	Enabled        *bool
	// ——客户端调用面策略（P2，设计 §4；nil = 未设置，不修改）——
	ClientCallable         *bool
	ClientAnonymousAllowed *bool
	ClientPerUserLimit     *int
	ClientLimitWindow      *string
	// ——池策略（v3 §5/OQ2；nil = 未设置，不修改）——
	MinInstances           *int
	MaxInstances           *int
	IdleTTLSeconds         *int
	MaxRequestsPerInstance *int
	Concurrency            *int
}

func (f *Functions) CreateFunction(ctx context.Context, cmd CreateFunctionCommand) (*domainfunctions.Function, error) {
	// 纵深防御（G2-1/R06-P0，G12 产品决策 B 调整）：函数写操作允许 console
	// admin 会话与 API key（拦截器分别以 adminRoleMethodRules 管角色、
	// apiKeyScopeRules 管 scope），此处兜底拒绝端用户/匿名的直接调用。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if !idgen.ID(cmd.ID).IsValid() {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if !functionIDPattern.MatchString(cmd.ID) {
		return nil, status.Error(codes.InvalidArgument, "invalid function id: must match ^[a-z0-9][a-z0-9_-]{0,63}$")
	}
	if strings.TrimSpace(cmd.Name) == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if !runtimeExists(cmd.Runtime) {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported runtime %q", cmd.Runtime)
	}
	timeoutSeconds := defaultTimeoutSeconds
	if cmd.TimeoutSeconds != nil {
		timeoutSeconds = *cmd.TimeoutSeconds
	}
	if timeoutSeconds < minTimeoutSeconds || timeoutSeconds > maxTimeoutSeconds {
		return nil, status.Errorf(codes.InvalidArgument, "timeout_seconds must be between %d and %d", minTimeoutSeconds, maxTimeoutSeconds)
	}
	if cmd.Spec == "" {
		cmd.Spec = "shared-1x"
	}
	if !specificationExists(cmd.Spec) {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported spec %q", cmd.Spec)
	}
	if cmd.Entrypoint == "" {
		cmd.Entrypoint = defaultEntrypoint(cmd.Runtime)
	}
	enabled := true
	if cmd.Enabled != nil {
		enabled = *cmd.Enabled
	}
	// 执行身份声明（P0）：词表/形态校验 + 去重排序；空集合法（= 无平台
	// 访问权限，fail-closed 默认）。
	declaredScopes, err := domainfunctions.NormalizeDeclaredScopes(cmd.DeclaredScopes)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	now := time.Now()
	fn := &domainfunctions.Function{
		ID:             cmd.ID,
		ProjectID:      cmd.ProjectID,
		Name:           cmd.Name,
		Runtime:        cmd.Runtime,
		Entrypoint:     cmd.Entrypoint,
		TimeoutSeconds: timeoutSeconds,
		Spec:           cmd.Spec,
		Enabled:        enabled,
		DeclaredScopes: declaredScopes,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// 客户端调用面策略（P2）：anonymous 一期禁用、callable 要求 limit>=1、
	// 窗口词表校验（设计 §4；存量全 FALSE ⇒ fail-closed）。
	if err := applyClientPolicy(fn, cmd.ClientCallable, cmd.ClientAnonymousAllowed, cmd.ClientPerUserLimit, cmd.ClientLimitWindow); err != nil {
		return nil, err
	}
	if err := f.repo.CreateFunction(ctx, fn); err != nil {
		return nil, err
	}
	f.cache.invalidate(cmd.ProjectID, cmd.ID)
	return fn, nil
}

func (f *Functions) ListFunctions(ctx context.Context, projectID string) ([]domainfunctions.Function, error) {
	return f.repo.ListFunctions(ctx, projectID)
}

func (f *Functions) GetFunction(ctx context.Context, projectID, functionID string) (*domainfunctions.Function, error) {
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	return fn, nil
}

func (f *Functions) UpdateFunction(ctx context.Context, cmd UpdateFunctionCommand) (*domainfunctions.Function, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	fn, err := f.repo.GetFunction(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	before := *fn
	if cmd.Name != nil {
		if strings.TrimSpace(*cmd.Name) == "" {
			return nil, status.Error(codes.InvalidArgument, "name is required")
		}
		fn.Name = *cmd.Name
	}
	if cmd.Entrypoint != nil {
		fn.Entrypoint = *cmd.Entrypoint
	}
	if cmd.TimeoutSeconds != nil {
		if *cmd.TimeoutSeconds < minTimeoutSeconds || *cmd.TimeoutSeconds > maxTimeoutSeconds {
			return nil, status.Errorf(codes.InvalidArgument, "timeout_seconds must be between %d and %d", minTimeoutSeconds, maxTimeoutSeconds)
		}
		fn.TimeoutSeconds = *cmd.TimeoutSeconds
	}
	if cmd.Spec != nil {
		if !specificationExists(*cmd.Spec) {
			return nil, status.Errorf(codes.InvalidArgument, "unsupported spec %q", *cmd.Spec)
		}
		fn.Spec = *cmd.Spec
	}
	if cmd.Enabled != nil {
		fn.Enabled = *cmd.Enabled
	}
	// 客户端调用面策略（P2）：未设置字段不修改；anonymous 一期禁用。
	if err := applyClientPolicy(fn, cmd.ClientCallable, cmd.ClientAnonymousAllowed, cmd.ClientPerUserLimit, cmd.ClientLimitWindow); err != nil {
		return nil, err
	}
	// 池策略（v3 §5/OQ2）：未设置字段不修改；值域兜底 + min≤max 跨字段校验
	// （protovalidate 管形状，业务规则在 app 层）。
	if err := applyPoolPolicy(fn, cmd.MinInstances, cmd.MaxInstances, cmd.IdleTTLSeconds, cmd.MaxRequestsPerInstance, cmd.Concurrency); err != nil {
		return nil, err
	}
	fn.UpdatedAt = time.Now()
	// before/after diff 落审计 metadata.changes（成功/失败均记变更尝试）。
	recordAuditChanges(ctx, &before, fn)
	if err := f.repo.UpdateFunction(ctx, fn); err != nil {
		return nil, err
	}
	// 缓存失效（P0.5）：同进程即时、跨实例 30s 收敛（cache.go 注释）。
	f.cache.invalidate(cmd.ProjectID, cmd.FunctionID)
	return fn, nil
}

// applyClientPolicy 校验并把客户端调用面策略应用到函数记录（P2，设计 §4）：
//   - client_anonymous_allowed=true 一期显式报错（Q4 拍板：匿名会话可无限
//     新造，per-user 限频对匿名形同虚设；字段保留，匿名 IP 兜底做好后再开）；
//   - client_callable=true 要求有效配额 client_per_user_limit >= 1
//     （无配额限频的公开调用面等于不限）；
//   - client_limit_window 词表 minute|hour|day（DB CHECK 与 proto 校验之外的
//     app 层兜底）；未设置时缺省 day。
//
// nil 字段 = 未设置（不修改现有值；Create 路径现有值即零值 FALSE）。
func applyClientPolicy(fn *domainfunctions.Function, callable, anonymous *bool, limit *int, window *string) error {
	if anonymous != nil && *anonymous {
		return status.Error(codes.InvalidArgument, "client_anonymous_allowed is not open in this release (一期未开放)")
	}
	if limit != nil {
		if *limit < 0 {
			return status.Error(codes.InvalidArgument, "client_per_user_limit must be >= 0")
		}
		fn.ClientPerUserLimit = *limit
	}
	if window != nil {
		if !domainfunctions.IsValidClientLimitWindow(*window) {
			return status.Errorf(codes.InvalidArgument, "client_limit_window must be one of %q, %q, %q",
				domainfunctions.ClientLimitWindowMinute, domainfunctions.ClientLimitWindowHour, domainfunctions.ClientLimitWindowDay)
		}
		fn.ClientLimitWindow = *window
	}
	if callable != nil {
		fn.ClientCallable = *callable
	}
	if fn.ClientCallable {
		if fn.ClientPerUserLimit < 1 {
			return status.Error(codes.InvalidArgument, "client_callable requires client_per_user_limit >= 1")
		}
		if fn.ClientLimitWindow == "" {
			fn.ClientLimitWindow = domainfunctions.ClientLimitWindowDay
		}
	}
	return nil
}

// applyPoolPolicy 校验并把池策略应用到函数记录（v3 多路复用 §5/OQ2；
// P0.5 执行器 v2 五列语义见迁移 000014/000017 与 functions-v3.md §1.5）：
//   - 单字段值域与 DB CHECK 同源：min>=0、max>=1、idle_ttl>=30、
//     max_requests>=1、concurrency 1..16（protovalidate 已在 API 边界拦截，
//     此处兜底防绕过传输层的直接调用）；
//   - 跨字段：min_instances <= max_instances（Update 的跨字段比较对象是
//     应用后的终值——单独调 min 时也要与存量 max 比较，反之亦然）；
//   - concurrency>1 要求函数可重入（契约文档化，不做平台硬校验，D1/D2）；
//     旧模板部署按 1 降级执行（D7，运行时语义）。
//
// nil 字段 = 未设置（不修改现有值）。
func applyPoolPolicy(fn *domainfunctions.Function, minInstances, maxInstances, idleTTL, maxRequests, concurrency *int) error {
	if minInstances != nil {
		if *minInstances < minMinInstances {
			return status.Errorf(codes.InvalidArgument, "min_instances must be >= %d", minMinInstances)
		}
		fn.MinInstances = *minInstances
	}
	if maxInstances != nil {
		if *maxInstances < minMaxInstances {
			return status.Errorf(codes.InvalidArgument, "max_instances must be >= %d", minMaxInstances)
		}
		fn.MaxInstances = *maxInstances
	}
	if idleTTL != nil {
		if *idleTTL < minIdleTTLSeconds {
			return status.Errorf(codes.InvalidArgument, "idle_ttl_seconds must be >= %d", minIdleTTLSeconds)
		}
		fn.IdleTTLSeconds = *idleTTL
	}
	if maxRequests != nil {
		if *maxRequests < minMaxRequests {
			return status.Errorf(codes.InvalidArgument, "max_requests_per_instance must be >= %d", minMaxRequests)
		}
		fn.MaxRequestsPerInstance = *maxRequests
	}
	if concurrency != nil {
		if *concurrency < minConcurrency || *concurrency > maxConcurrency {
			return status.Errorf(codes.InvalidArgument, "concurrency must be between %d and %d", minConcurrency, maxConcurrency)
		}
		fn.Concurrency = *concurrency
	}
	if fn.MinInstances > fn.MaxInstances {
		return status.Errorf(codes.InvalidArgument, "min_instances (%d) must be <= max_instances (%d)", fn.MinInstances, fn.MaxInstances)
	}
	if fn.Concurrency < minConcurrency {
		// 存量零值防御（理论不可达：建列带 DEFAULT 1）；保持 DB CHECK 对齐。
		fn.Concurrency = defaultConcurrency
	}
	return nil
}

func (f *Functions) DeleteFunction(ctx context.Context, projectID, functionID string) error {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return err
	}
	if fn == nil {
		return status.Error(codes.NotFound, "function not found")
	}
	deps, err := f.repo.ListDeployments(ctx, projectID, functionID)
	if err != nil {
		return err
	}
	// 先 DB 级联删除，再清理镜像与本地 zip（全部幂等，失败仅记日志）。
	if err := f.repo.DeleteFunction(ctx, projectID, functionID); err != nil {
		return err
	}
	f.cache.invalidate(projectID, functionID)
	for i := range deps {
		_ = f.executor.RemoveImage(ctx, deps[i].FunctionID, deps[i].ID)
		_ = removeZip(deps[i].ProjectID, deps[i].FunctionID, deps[i].ID)
	}
	return nil
}

// SetFunctionScopes 全量替换函数 declared_scopes（P0 执行身份，镜像
// SetVariables 的 PUT 全量替换先例；UpdateFunctionRequest 无 repeated 字段
// 的未设置语义，故独立 RPC）。校验与去重同 CreateFunction；空集合法
// （撤销全部平台访问）。
func (f *Functions) SetFunctionScopes(ctx context.Context, projectID, functionID string, scopes []string) (*domainfunctions.Function, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	normalized, err := domainfunctions.NormalizeDeclaredScopes(scopes)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	fn.DeclaredScopes = normalized
	fn.UpdatedAt = time.Now()
	if err := f.repo.UpdateFunction(ctx, fn); err != nil {
		return nil, err
	}
	f.cache.invalidate(projectID, functionID)
	return fn, nil
}
