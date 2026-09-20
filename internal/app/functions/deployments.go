package functions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// zipDir 是本地 zip 代码包根目录（构建输入第一层；持久副本在 zipStore
// 专用桶——盘上丢失可拉回，见 rebuild.go）。
const zipDir = "torchwood-functions"

type CreateDeploymentCommand struct {
	ProjectID  string
	FunctionID string
	Code       []byte // zip 字节流
	// Git 是 git 仓库源（二期，设计 §2）：非 nil 时经 SourcePacker
	// （packer 服务）物化为同一 zip 构建路径——pack 在 deployment
	// 行落库之前，失败路径无行无 zip；zip 路径行为完全不变。
	Git *domainfunctions.GitSource
	// Image 是 BYO 镜像源（三期阶段 1，设计 §3）：非 nil 时走免构建路径
	//（ImportImage 钉死 digest → INSERT（source_ref=digest）→ ready 门禁
	// 复检）；仅 runtime = "image" 的函数接受（源/运行时互斥，D7）。
	Image *domainfunctions.ImageSource
}

// imageRuntimeID 是 BYO 镜像专用 runtime ID（runtimes.go 表项同值；源/运行时
// 互斥判定的 image 侧基准）。
const imageRuntimeID = "image"

// validateSourceRuntimePair 校验部署源与函数运行时的互斥（D7 双向，设计 §0）：
//   - runtime = image 只接受 image 源（zip/git 一律拒绝）；
//   - runtime ≠ image（node/go）拒绝 image 源（zip/git 均可）。
//
// 错误为 InvalidArgument 且同时携带两侧取值（source 与 runtime），不做单侧
// 指认——双向互斥的任一侧违反都呈现同一事实。
func validateSourceRuntimePair(runtime, sourceType string) error {
	if (runtime == imageRuntimeID) == (sourceType == domainfunctions.DeploymentSourceImage) {
		return nil
	}
	return status.Errorf(codes.InvalidArgument,
		"deployment source %q and function runtime %q are incompatible: the image runtime only accepts image sources, and image sources require the image runtime",
		sourceType, runtime)
}

func (f *Functions) CreateDeployment(ctx context.Context, cmd CreateDeploymentCommand) (*domainfunctions.Deployment, error) {
	// 纵深防御（G2-1/R06-P0，G12 调整）：部署写操作允许 admin 会话与 API key。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	// 部署源分支：image（三期阶段 1，免构建路径）与 git（二期，packer 物化）
	// 均在 deployment 行落库之前完成源物化——失败路径无行无残留；zip 路径
	// 行为不变。
	if cmd.Image != nil {
		return f.createDeploymentFromImage(ctx, cmd, cmd.Image)
	}
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
	// 源/运行时互斥（D7 双向）：image runtime 只收 image 源，zip 源对 image
	// runtime 函数拒绝；运行时状态门：eol runtime 拒绝新的部署构建（存量
	// ready 部署执行面不受影响，functions-runtime-selection.md §6）。
	if err := validateSourceRuntimePair(fn.Runtime, domainfunctions.DeploymentSourceZip); err != nil {
		return nil, err
	}
	if err := validateRuntimeSelectable(fn.Runtime); err != nil {
		return nil, err
	}

	now := time.Now()
	// zip 源同填可复现性锚（repo.go Deployment.ContextSHA256 已声明 zip/git
	// 共用）：持久层拉回复核的强校验基准（无锚的存量 zip 部署回退 Size
	// 复核，见 rebuild.go verifyRestoredZip）。
	zipSum := sha256.Sum256(cmd.Code)
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
		// runtime 快照（迁移 000025）：INSERT 期写全、之后不可变——补构建/
		// 审计以行内快照为准（functions-runtime-selection.md §4）。
		Runtime:       fn.Runtime,
		ContextSHA256: hex.EncodeToString(zipSum[:]),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := f.repo.CreateDeployment(ctx, dep); err != nil {
		return nil, err
	}

	path := zipPath(cmd.ProjectID, cmd.FunctionID, dep.ID)
	if err := writeZip(path, cmd.Code); err != nil {
		return nil, fmt.Errorf("write code package: %w", err)
	}
	// 持久副本（zip 持久桶）：失败整体回滚（行 + 本地 zip）——持久层是
	// 重建自愈的源，best-effort 会静默退化回「zip 源不可自愈」声明边界。
	if err := f.storeZip(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID, cmd.Code); err != nil {
		_ = f.repo.DeleteDeployment(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID)
		_ = removeZip(cmd.ProjectID, cmd.FunctionID, dep.ID)
		return nil, fmt.Errorf("persist code package: %w", err)
	}

	// 同步构建（MVP 定案：不在独立构建队列，请求内完成；构建 ctx 与请求
	// ctx 解耦，见 buildDeployment）。
	if err := f.buildDeployment(ctx, fn, dep, path); err != nil {
		// 信号量满或状态写回失败：删除 deployment 行与代码包，避免残留 pending 行。
		_ = f.repo.DeleteDeployment(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID)
		f.removeCodePackage(ctx, cmd.ProjectID, cmd.FunctionID, dep.ID)
		return nil, err
	}
	return dep, nil
}

// buildDeployment 占用构建信号量并同步构建/导入镜像；结果写入 dep 状态并
// 落库。信号量满仅返回 ResourceExhausted——是否清理 deployment 行与 zip 是
// 调用方的决策（CreateDeployment 清理本次刚建的 pending 行；worker 补构建
// 路径保留既有 deployment，靠队列重试在信号量释放后重建）。
//
// fn 是部署所属函数记录（BuildSpec/ImportImageSpec 的 runtime/timeout/
// variables/egress 分类来源）。ctx 解耦（D11）：信号量获取之后的全部动作
// （building 落库 → executor.Build / executor.ImportImage → 终态落库 →
// ActivateDeployment）运行在 context.WithoutCancel + build_timeout 封顶的
// 独立预算上——客户端断开后构建继续、状态照常落库，客户端以
// deployment.status 轮询兜底；孤儿构建的信号量占用有界（build_timeout 到点
// 释放，对抗审查 A6 接受）。
//
// 产物化调用点按部署源分流（三期阶段 1，设计 §3「worker 补构建分流」）：
// image 源 → 幂等 ImportImage（spec 带预期 digest = 行内 source_ref，本地
// 命中则零 pull；一次性凭证不落库，本路径凭证恒空——私有镜像补拉失败标
// failed 属声明边界）；zip/git 源 → Build（盘上 zip 为输入）。worker 补构建
// 与首次部署共用本分流，无需感知源类型。
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

	var buildErr error
	var buildNode string
	if dep.SourceType == domainfunctions.DeploymentSourceImage {
		spec, specErr := f.importImageSpec(buildCtx, fn, dep.ID, &domainfunctions.ImageSource{Reference: dep.SourceURL}, dep.SourceRef)
		if specErr != nil {
			return specErr
		}
		// 镜像导入路径暂不落 build_node（四期 4a-1 范围仅 Build 响应通道；
		// BYO 镜像经 M1 registry 全局化后亲和语义弱化，随 4a-2 路由一并收口）。
		_, buildErr = f.executor.ImportImage(buildCtx, spec)
	} else {
		spec, specErr := f.buildSpec(buildCtx, fn, dep, path)
		if specErr != nil {
			return specErr
		}
		buildNode, buildErr = f.executor.Build(buildCtx, spec)
	}
	dep.UpdatedAt = time.Now()
	if buildErr != nil {
		dep.Status = domainfunctions.DeploymentStatusFailed
		dep.Error = truncate(buildErr.Error(), maxOutputBytes)
		_ = f.repo.UpdateDeployment(buildCtx, dep)
		// 清理本地与持久层代码包及可能残留的镜像（幂等）。zip 按源类型分流
		// （设计 §2 重建语义）：zip 源构建失败即删（D13 一期形态）；git 源
		// zip 保留——物化快照在盘上、worker 补构建不依赖一次性凭证， redeploy
		// 只在 zip 缺失（磁盘被清）时才必须。
		if dep.SourceType != domainfunctions.DeploymentSourceGit {
			f.removeCodePackage(buildCtx, dep.ProjectID, dep.FunctionID, dep.ID)
		}
		_ = f.executor.RemoveImage(buildCtx, dep.FunctionID, dep.ID)
		return nil
	}
	// 构建亲和落账（四期 4a-1，设计 §4 M5）：首个构建落成的 dispatcher
	// 节点 ID（BuildResponse.node_id）随成功路径经 UpdateDeployment 落库。
	// build_node 是构建后写入的操作列（迁移 000024；UpdateDeployment 列
	// 白名单已登记，与 INSERT 期写全、之后不可变的 source 快照列不同）。
	// 失败路径不落：failed 行无路由亲和语义。
	dep.BuildNode = buildNode
	if err := f.repo.UpdateDeployment(buildCtx, dep); err != nil {
		return err
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
//   - Runtime = dep.Runtime 行内快照（迁移 000025；快照忠实：补构建/审计
//     与首次构建永远同一 runtime，不随 UpdateFunction 变更漂移——
//     functions-runtime-selection.md §4。存量行迁移已回填，零值防御性回退
//     fn.Runtime）；
//   - FunctionTimeoutSeconds = fn.timeout_seconds（旧池 drain 宽限上限，
//     D14：载荷补齐后 drain 真正生效）；
//   - Env 与执行链 env 组装同源（见 buildFunctionEnv）；
//   - EgressUntrusted 与执行路径同一分类规则（client_callable 或存在
//     http/cron 触发器 = 不可信，对抗审查 A1：验证实例与执行同网）；
//   - Verify 取 config functions.dispatcher.verify_build 的 presence 解析
//     （未配置 = 默认开启，显式 false 关闭，D10）。
func (f *Functions) buildSpec(ctx context.Context, fn *domainfunctions.Function, dep *domainfunctions.Deployment, zipPath string) (domainfunctions.BuildSpec, error) {
	env, err := f.buildFunctionEnv(ctx, dep.ProjectID, dep.FunctionID)
	if err != nil {
		return domainfunctions.BuildSpec{}, err
	}
	runtime := dep.Runtime
	if runtime == "" {
		runtime = fn.Runtime
	}
	return domainfunctions.BuildSpec{
		ProjectID:              dep.ProjectID,
		FunctionID:             dep.FunctionID,
		DeploymentID:           dep.ID,
		ZipPath:                zipPath,
		Runtime:                runtime,
		FunctionTimeoutSeconds: int64(fn.TimeoutSeconds),
		Env:                    env,
		EgressUntrusted:        fn.ClientCallable || f.hasTriggersCached(ctx, dep.ProjectID, dep.FunctionID),
		Verify:                 f.verifyBuildEnabled(),
	}, nil
}

// buildFunctionEnv 组装构建/验证链 env（与执行链同源）：sanitizeEnv 剔除
// 非法键 + 注入 TW_API_BASE_URL；不含 TW_EXECUTION_TOKEN——构建/验证期无
// 执行身份（无常驻凭证注入，验证 spawn 仅做 health 探针）。
func (f *Functions) buildFunctionEnv(ctx context.Context, projectID, functionID string) (map[string]string, error) {
	vars, err := f.getCachedVariables(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	env := sanitizeEnv(vars)
	if apiBaseURL := f.executionAPIBaseURL(); apiBaseURL != "" {
		env[twAPIBaseURLEnv] = apiBaseURL
	}
	return env, nil
}

// importImageSpec 组装 ImportImageSpec（三期阶段 1，设计 §3）：Env 与
// EgressUntrusted 同构建链 buildSpec 组装（契约验证 spawn 携带函数
// variables、untrusted 验证实例挂 internal 变体网络——A1 同款约束）；
// expectedDigest 非空 = 幂等补拉/复检（ready 门禁与 worker 补构建），首次
// 导入为空。src 的一次性凭证仅随本 spec 进入调用栈，不落库。
func (f *Functions) importImageSpec(ctx context.Context, fn *domainfunctions.Function, deploymentID string, src *domainfunctions.ImageSource, expectedDigest string) (domainfunctions.ImportImageSpec, error) {
	env, err := f.buildFunctionEnv(ctx, fn.ProjectID, fn.ID)
	if err != nil {
		return domainfunctions.ImportImageSpec{}, err
	}
	return domainfunctions.ImportImageSpec{
		ProjectID:              fn.ProjectID,
		FunctionID:             fn.ID,
		DeploymentID:           deploymentID,
		Reference:              src.Reference,
		ExpectedDigest:         expectedDigest,
		RegistryUsername:       src.RegistryUsername,
		RegistryToken:          src.RegistryToken,
		FunctionTimeoutSeconds: int64(fn.TimeoutSeconds),
		Env:                    env,
		EgressUntrusted:        fn.ClientCallable || f.hasTriggersCached(ctx, fn.ProjectID, fn.ID),
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

// DeleteDeployment 删除顺序：先 DB 级联删除 → 再 docker image rm → 最后删
// 本地与持久层代码包（全部幂等，失败仅记日志），避免进行中构建/执行读到
// 半删除状态。
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
	f.removeCodePackage(ctx, projectID, functionID, deploymentID)
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

// storeZip 把部署代码包写入持久层（zipStore nil = 未注入，跳过——旧构造/
// 测试，语义与仅本地盘时代一致）。调用方对失败整体回滚：持久副本是重建
// 自愈的源，best-effort 会静默退化回「盘缺失即不可自愈」的声明边界。
func (f *Functions) storeZip(ctx context.Context, projectID, functionID, deploymentID string, zip []byte) error {
	if f.zipStore == nil {
		return nil
	}
	return f.zipStore.Put(ctx, projectID, functionID, deploymentID, zip)
}

// removeCodePackage 删除本地盘与持久层代码包（调用方均为清理路径：幂等、
// best-effort——失败仅记日志不影响主流程语义，镜像 removeZip 的 `_ =`
// 形态但保留可观测性）。
func (f *Functions) removeCodePackage(ctx context.Context, projectID, functionID, deploymentID string) {
	if err := removeZip(projectID, functionID, deploymentID); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.logger().Warn("functions: remove local code package failed",
			"project", projectID, "function", functionID, "deployment", deploymentID, "error", err)
	}
	if f.zipStore == nil {
		return
	}
	if err := f.zipStore.Remove(ctx, projectID, functionID, deploymentID); err != nil {
		f.logger().Warn("functions: remove stored code package failed",
			"project", projectID, "function", functionID, "deployment", deploymentID, "error", err)
	}
}

// isZip 校验 zip 魔数 PK\x03\x04（空 zip 为 PK\x05\x06，一并拒绝）。
func isZip(code []byte) bool {
	return len(code) >= 4 && bytes.Equal(code[:4], []byte{0x50, 0x4B, 0x03, 0x04})
}
