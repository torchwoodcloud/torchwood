package functions

import (
	"context"
	"regexp"
	"strings"
	"time"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	"github.com/torchwooddev/torchwood/pkg/idgen"
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
}

type UpdateFunctionCommand struct {
	ProjectID      string
	FunctionID     string
	Name           *string
	Entrypoint     *string
	TimeoutSeconds *int
	Spec           *string
	Enabled        *bool
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
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := f.repo.CreateFunction(ctx, fn); err != nil {
		return nil, err
	}
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
	fn.UpdatedAt = time.Now()
	if err := f.repo.UpdateFunction(ctx, fn); err != nil {
		return nil, err
	}
	return fn, nil
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
	for i := range deps {
		_ = f.executor.RemoveImage(ctx, deps[i].FunctionID, deps[i].ID)
		_ = removeZip(deps[i].ProjectID, deps[i].FunctionID, deps[i].ID)
	}
	return nil
}
