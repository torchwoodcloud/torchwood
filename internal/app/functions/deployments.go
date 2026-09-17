package functions

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// defaultBuildTimeout 是构建整体超时缺省值（functions.dispatcher.build_timeout
// 空/非法时回落；对齐 worker 补构建原 workerRebuildTimeout=5m 口径）。
const defaultBuildTimeout = 5 * time.Minute

// maxDeploymentCodeBytes 是 zip 代码包上限（multipart 路径限流）。
const maxDeploymentCodeBytes = 50 << 20 // 50 MiB

// zipDir 是本地 zip 代码包根目录（单机部署假设：server 与 worker 共享文件系统）。
const zipDir = "torchwood-functions"

type CreateDeploymentCommand struct {
	ProjectID  string
	FunctionID string
	Code       []byte // zip 字节流
	// Git 是 git 仓库源（二期，设计 §2）：非 nil 时经 SourcePacker
	// （functions-packer 服务）物化为同一 zip 构建路径——pack 在 deployment
	// 行落库之前，失败路径无行无 zip；zip 路径行为完全不变。
	Git *domainfunctions.GitSource
}

func (f *Functions) CreateDeployment(ctx context.Context, cmd CreateDeploymentCommand) (*domainfunctions.Deployment, error) {
	// 纵深防御（G2-1/R06-P0，G12 调整）：部署写操作允许 admin 会话与 API key。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	// git 源分支（二期阶段 3 接线）：形状校验 → packer 物化 → 写盘 →
	// INSERT（source 投影 + 钉死 SHA + checksum）→ 与 zip 源同构构建。
	if cmd.Git != nil {
		return f.createDeploymentFromGit(ctx, cmd, cmd.Git)
	}
	if len(cmd.Code) == 0 {
		return nil, status.Error(codes.InvalidArgument, "code is required")
	}
	if len(cmd.Code) > maxDeploymentCodeBytes {
		return nil, status.Errorf(codes.InvalidArgument, "code exceeds maximum size of %d bytes", maxDeploymentCodeBytes)
	}
	if !isZip(cmd.Code) {
		return nil, status.Error(codes.InvalidArgument, "invalid zip file: missing PK zip signature")
	}
	fn, err := f.repo.GetFunction(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}

	now := time.Now()
	dep := &domainfunctions.Deployment{
		ID:         idgen.UUID().String(),
		FunctionID: cmd.FunctionID,
		ProjectID:  cmd.ProjectID,
		Size:       int64(len(cmd.Code)),
		Status:     domainfunctions.DeploymentStatusPending,
		// 模板版本化（P0.5）：记录构建所用 runner 模板版本（存量 deployment
		// 据此判定按新模板重建）。
		TemplateVersion: domainfunctions.RunnerTemplateVersion,
		// zip 通道源类型显式登记（迁移 000023；git 源在 packer 分支落 git 词表值）。
		SourceType: domainfunctions.DeploymentSourceZip,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := f.repo.CreateDeployment(ctx, dep); err != nil {
		return nil, err
	}

	path := zipPath(cmd.ProjectID, cmd.FunctionID, dep.ID)
	if err := writeZip(path, cmd.Code); err != nil {
		return nil, fmt.Errorf("write code package: %w", err)
	}

	// 同步构建（MVP 定案：不在独立构建队列，请求内完成；构建 ctx 与请求
	// ctx 解耦，见 buildDeployment）。
	if err := f.buildDeployment(ctx, fn, dep, path); err != nil {
		// 信号量满或状态写回失败：删除 deployment 行与本地 zip，避免残留 pending 行。
		_ = f.repo.DeleteDeployment(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID)
		_ = removeZip(cmd.ProjectID, cmd.FunctionID, dep.ID)
		return nil, err
	}
	return dep, nil
}

// buildDeployment 占用构建信号量并同步构建镜像；结果写入 dep 状态并落库。
// 信号量满仅返回 ResourceExhausted——是否清理 deployment 行与 zip 是调用方
// 的决策（CreateDeployment 清理本次刚建的 pending 行；worker 补构建路径
// 保留既有 deployment，靠队列重试在信号量释放后重建）。
//
// fn 是部署所属函数记录（BuildSpec 的 runtime/timeout/variables/egress
// 分类来源）。ctx 解耦（D11）：信号量获取之后的全部动作（building 落库 →
// executor.Build → 终态落库 → ActivateDeployment）运行在
// context.WithoutCancel + build_timeout 封顶的独立预算上——客户端断开后
// 构建继续、状态照常落库，客户端以 deployment.status 轮询兜底；孤儿构建
// 的信号量占用有界（build_timeout 到点释放，对抗审查 A6 接受）。
func (f *Functions) buildDeployment(ctx context.Context, fn *domainfunctions.Function, dep *domainfunctions.Deployment, path string) error {
	ok, release, err := f.getBuildSemaphore().TryAcquire(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "acquire build semaphore: %v", err)
	}
	if !ok {
		return status.Error(codes.ResourceExhausted, "too many concurrent builds")
	}
	defer release()

	buildCtx, cancelBuild := context.WithTimeout(context.WithoutCancel(ctx), f.buildTimeout())
	defer cancelBuild()

	dep.Status = domainfunctions.DeploymentStatusBuilding
	dep.UpdatedAt = time.Now()
	if err := f.repo.UpdateDeployment(buildCtx, dep); err != nil {
		return err
	}

	spec, err := f.buildSpec(buildCtx, fn, dep, path)
	if err != nil {
		return err
	}
	err = f.executor.Build(buildCtx, spec)
	dep.UpdatedAt = time.Now()
	if err != nil {
		dep.Status = domainfunctions.DeploymentStatusFailed
		dep.Error = truncate(err.Error(), maxOutputBytes)
		_ = f.repo.UpdateDeployment(buildCtx, dep)
		// 清理本地 zip 与可能残留的镜像（幂等）。zip 按源类型分流（设计
		// §2 重建语义）：zip 源构建失败即删（D13 一期形态）；git 源 zip
		// 保留——物化快照在盘上、worker 补构建不依赖一次性凭证， redeploy
		// 只在 zip 缺失（磁盘被清）时才必须。
		if dep.SourceType != domainfunctions.DeploymentSourceGit {
			_ = removeZip(dep.ProjectID, dep.FunctionID, dep.ID)
		}
		_ = f.executor.RemoveImage(buildCtx, dep.FunctionID, dep.ID)
		return nil
	}
	dep.Status = domainfunctions.DeploymentStatusReady
	dep.Error = ""
	// ready 转移走 ActivateDeployment：同一事务内维护
	// functions.latest_ready_deployment_id（热路径清账，P0.5——
	// selectDeployment 优先读指针，消灭 ListDeployments 全量拉取）。
	if err := f.repo.ActivateDeployment(buildCtx, dep); err != nil {
		return err
	}
	// 缓存失效：latest 指针投影随函数记录缓存（P0.5）。
	f.cache.invalidate(dep.ProjectID, dep.FunctionID)
	return nil
}

// buildSpec 组装 BuildSpec（构建链载荷一期定稿，设计 §0）：
//   - Runtime = fn.runtime 原值（daemon 侧 D7 对账基准）；
//   - FunctionTimeoutSeconds = fn.timeout_seconds（旧池 drain 宽限上限，
//     D14：载荷补齐后 drain 真正生效）；
//   - Env 与执行链 env 组装同源（sanitizeEnv 剔除非法键 + TW_API_BASE_URL
//     注入），不含 TW_EXECUTION_TOKEN——构建/验证期无执行身份（无常驻
//     凭证注入，验证 spawn 仅做 health 探针）；v2 语义下 TW_DATA 走请求体
//     通道，本就不在 env；
//   - EgressUntrusted 与执行路径同一分类规则（client_callable 或存在
//     http/cron 触发器 = 不可信，对抗审查 A1：验证实例与执行同网）；
//   - Verify 取 config functions.dispatcher.verify_build 的 presence 解析
//     （未配置 = 默认开启，显式 false 关闭，D10）。
func (f *Functions) buildSpec(ctx context.Context, fn *domainfunctions.Function, dep *domainfunctions.Deployment, zipPath string) (domainfunctions.BuildSpec, error) {
	vars, err := f.getCachedVariables(ctx, dep.ProjectID, dep.FunctionID)
	if err != nil {
		return domainfunctions.BuildSpec{}, err
	}
	env := sanitizeEnv(vars)
	if apiBaseURL := f.executionAPIBaseURL(); apiBaseURL != "" {
		env[twAPIBaseURLEnv] = apiBaseURL
	}
	return domainfunctions.BuildSpec{
		ProjectID:              dep.ProjectID,
		FunctionID:             dep.FunctionID,
		DeploymentID:           dep.ID,
		ZipPath:                zipPath,
		Runtime:                fn.Runtime,
		FunctionTimeoutSeconds: int64(fn.TimeoutSeconds),
		Env:                    env,
		EgressUntrusted:        fn.ClientCallable || f.hasTriggersCached(ctx, dep.ProjectID, dep.FunctionID),
		Verify:                 f.verifyBuildEnabled(),
	}, nil
}

// buildTimeout 解析 functions.dispatcher.build_timeout；空/非法回落默认 5m
// （parseDurationValue 同款风格，对齐 clientinvoke 的时长配置解析）。
func (f *Functions) buildTimeout() time.Duration {
	if f.cfg == nil {
		return defaultBuildTimeout
	}
	return parseDurationValue(f.cfg.GetFunctions().GetDispatcher().GetBuildTimeout(), defaultBuildTimeout)
}

// verifyBuildEnabled 解析 functions.dispatcher.verify_build（optional bool
// presence 语义，D10「默认 true 可关」）：dispatcher 段未配置、字段未设置
// → 默认开启；显式 false → 关闭。先例：security.rate_limit.enabled
// （internal/api/interceptor/ratelimit.go 同款 presence 归一）。
func (f *Functions) verifyBuildEnabled() bool {
	if f.cfg == nil {
		return true
	}
	d := f.cfg.GetFunctions().GetDispatcher()
	return d == nil || d.VerifyBuild == nil || d.GetVerifyBuild()
}

func (f *Functions) ListDeployments(ctx context.Context, projectID, functionID string) ([]domainfunctions.Deployment, error) {
	if _, err := f.repo.GetFunction(ctx, projectID, functionID); err != nil {
		return nil, err
	}
	return f.repo.ListDeployments(ctx, projectID, functionID)
}

func (f *Functions) GetDeployment(ctx context.Context, projectID, functionID, deploymentID string) (*domainfunctions.Deployment, error) {
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	dep, err := f.repo.GetDeployment(ctx, projectID, functionID, deploymentID)
	if err != nil {
		return nil, err
	}
	if dep == nil {
		return nil, status.Error(codes.NotFound, "deployment not found")
	}
	return dep, nil
}

// DeleteDeployment 删除顺序：先 DB 级联删除 → 再 docker image rm → 最后删本地 zip
// （全部幂等，失败仅记日志），避免进行中构建/执行读到半删除状态。
func (f *Functions) DeleteDeployment(ctx context.Context, projectID, functionID, deploymentID string) error {
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
	dep, err := f.repo.GetDeployment(ctx, projectID, functionID, deploymentID)
	if err != nil {
		return err
	}
	if dep == nil {
		return status.Error(codes.NotFound, "deployment not found")
	}
	if err := f.repo.DeleteDeployment(ctx, projectID, functionID, deploymentID); err != nil {
		return err
	}
	// 缓存失效：删除可能清掉 latest 指针（P0.5）。
	f.cache.invalidate(projectID, functionID)
	_ = f.executor.RemoveImage(ctx, functionID, deploymentID)
	_ = removeZip(projectID, functionID, deploymentID)
	return nil
}

// zipRoot 返回本地 zip 代码包根目录（单机部署假设）。
func zipRoot() string {
	return filepath.Join(os.TempDir(), zipDir)
}

// zipPath 返回本地 zip 代码包路径；functionID 等组件先 filepath.Base 消毒，
// 防止 `../../` 等路径穿越逃逸 zip 根目录。
func zipPath(projectID, functionID, deploymentID string) string {
	return filepath.Join(zipRoot(), filepath.Base(projectID), filepath.Base(functionID), filepath.Base(deploymentID)+".zip")
}

// assertZipDir 断言 path 的父目录位于 zip 根目录前缀内（纵深防御）。
func assertZipDir(path string) error {
	root := filepath.Clean(zipRoot())
	dir := filepath.Clean(filepath.Dir(path))
	if dir != root && !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		return fmt.Errorf("zip path escapes functions root: %q", path)
	}
	return nil
}

func writeZip(path string, code []byte) error {
	if err := assertZipDir(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, code, 0o600)
}

func removeZip(projectID, functionID, deploymentID string) error {
	path := zipPath(projectID, functionID, deploymentID)
	if err := assertZipDir(path); err != nil {
		return err
	}
	return os.Remove(path)
}

// isZip 校验 zip 魔数 PK\x03\x04（空 zip 为 PK\x05\x06，一并拒绝）。
func isZip(code []byte) bool {
	return len(code) >= 4 && bytes.Equal(code[:4], []byte{0x50, 0x4B, 0x03, 0x04})
}
