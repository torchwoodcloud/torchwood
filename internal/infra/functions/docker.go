package functions

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// zip 解压限制（§5.4 防 zip 炸弹）。
const (
	maxZipEntries       = 1000
	maxZipEntryBytes    = 100 << 20 // 单条 ≤ 100 MiB
	maxZipTotalBytes    = 200 << 20 // 总解压 ≤ 200 MiB
	maxBuildLogBytes    = 64 << 10  // 构建日志截断 64KB
	maxBuildLogLine     = 4 << 20   // 单行构建日志上限（Scanner 缓冲，超长即报错）
	maxContainerOutSize = 1 << 20   // stdout/stderr 缓冲上限（结果在 app 层再截断 64KB）
	// maxExecEnvBudgetBytes 是 data + env 合并预算（execve 32KiB 单参数硬限制）。
	maxExecEnvBudgetBytes = 32 << 10
	// dockerCleanupTimeout 是容器停止/删除等清理操作的独立超时：清理不继承
	// 已超时的 runCtx，也不能用无超时的 Background（daemon 挂起会无限阻塞）。
	dockerCleanupTimeout = 30 * time.Second
)

// zipExtractLimits 是 extractZip 的解压预算（防 zip 炸弹）。
// 校验分两层：声明侧（UncompressedSize64，快速拒绝）+ 写入侧（按实际写入
// 字节计数，防声明大小与实际内容不符的伪造 zip）。
type zipExtractLimits struct {
	maxEntries    int
	maxEntryBytes int64
	maxTotalBytes int64
}

var defaultZipExtractLimits = zipExtractLimits{
	maxEntries:    maxZipEntries,
	maxEntryBytes: maxZipEntryBytes,
	maxTotalBytes: maxZipTotalBytes,
}

// errZipBudgetExceeded 表示按实际字节计数的解压预算超限（写入侧兜底）。
var errZipBudgetExceeded = errors.New("extracted size exceeds budget")

// budgetWriter 包装目标 writer，按实际写入字节计数；超预算拒绝写入并返回
// errZipBudgetExceeded（调用方负责清理半成品文件）。
type budgetWriter struct {
	dst     io.Writer
	limit   int64
	written int64
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	if b.limit-b.written < int64(len(p)) {
		return 0, errZipBudgetExceeded
	}
	n, err := b.dst.Write(p)
	b.written += int64(n)
	return n, err
}

// specResources 是资源规格 → 容器配额映射（与 app 层 runtimes 表一致，
// infra 不依赖 app 包，自持一份兜底）。
var specResources = map[string]struct {
	cpu    float64
	memory int64
}{
	"shared-1x": {cpu: 0.5, memory: 256 << 20},
	"shared-2x": {cpu: 1.0, memory: 512 << 20},
}

// ResourceSpec 是资源规格的配额投影（functions-dispatcher 复用同一映射）。
type ResourceSpec struct {
	Memory   int64
	NanoCPUs int64
}

// SpecResources 把规格名映射为容器配额；未知规格回落 shared-1x。
func SpecResources(spec string) ResourceSpec {
	res := specResources[spec]
	if res.cpu <= 0 {
		res = specResources["shared-1x"]
	}
	return ResourceSpec{Memory: res.memory, NanoCPUs: int64(res.cpu * 1e9)}
}

// DockerExecutor 是真实 Docker 执行器（v1：每请求一容器，回退执行模型）：
// Build（zip → 镜像）+ Execute（run 容器）。类型导出供组合根按
// functions.executor 配置选择 v1/v2 实现（ProvideExecutor）。
type DockerExecutor struct {
	cfg *config.AppConfig
	cli *client.Client

	// initErr 是 client 构造失败的错误（配置错误延迟到首次调用暴露）。
	initErr error

	// netMu/netReady 保护网络就绪状态：按网络名缓存成功，失败不缓存（可重试）。
	netMu    sync.Mutex
	netReady map[string]bool
}

// perProjectNetworkPrefix 是默认 per-project 函数执行网络的前缀
// （Round4 J5-4）：完整网络名为 tw-func-<project.id>。project.id 已过
// ident 白名单（^[a-z][a-z0-9]{0,27}$），可直接用作网络名后缀。
const perProjectNetworkPrefix = "tw-func-"

// perProjectInternalNetworkSuffix 是 internal 变体网络的后缀（P2 egress
// 默认 deny，设计 Security #6）：tw-func-<project.id>-int，docker
// internal: true——阻断外网出口、网内互通保留（dispatcher/平台回访地址
// 仍可达）。不可信函数（client_callable 或存在 http/cron 触发器）容器
// attach 该网络而非常规网络。
const perProjectInternalNetworkSuffix = "-int"

// NewDockerExecutor creates a Docker-based functions executor (v1).
func NewDockerExecutor(cfg *config.AppConfig) *DockerExecutor {
	d := &DockerExecutor{cfg: cfg, netReady: map[string]bool{}}
	host := cfg.GetFunctions().GetDocker().GetHost()
	// WithAPIVersionNegotiation：与 daemon 协商 API 版本，避免客户端默认
	// 版本高于 daemon（如 CI runner 上 daemon 1.48 vs 客户端 1.51）导致
	// "client version is too new" 构建失败。
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		d.initErr = fmt.Errorf("create docker client: %w", err)
		return d
	}
	d.cli = cli
	return d
}

// client 返回 docker client；构造失败时返回 initErr。
func (d *DockerExecutor) client() (*client.Client, error) {
	if d.cli == nil {
		return nil, d.initErr
	}
	return d.cli, nil
}

// imageName 组装镜像名（转发导出版 ImageName，dispatcher 复用同一约定）。
func (d *DockerExecutor) imageName(functionID, deploymentID string) string {
	return ImageName(d.cfg, functionID, deploymentID)
}

// ImageName 返回函数部署镜像名：{registry}/func-{functionID}-{deploymentID}
// （registry 取 functions.docker.registry，默认 torchwood-funcs）。
func ImageName(cfg *config.AppConfig, functionID, deploymentID string) string {
	registry := cfg.GetFunctions().GetDocker().GetRegistry()
	if registry == "" {
		registry = "torchwood-funcs"
	}
	// 兜底兼容历史大写 functionID（Docker 镜像仓库/标签名只允许小写，G6-3）。
	return fmt.Sprintf("%s/func-%s-%s", registry, strings.ToLower(functionID), deploymentID)
}

// ResolveNetworkName 解析函数执行容器网络名（Round4 J5-4；导出供
// functions-dispatcher 与 v1 执行器保持同一约定）：
//   - 显式配置 functions.docker.network 时使用该全局网络（opt-in；跨项目
//     函数容器同网互通，存在横向访问风险，见 config.yaml.template 警告）；
//   - 未配置（默认）时使用 per-project 网络 tw-func-<project.id>，项目间
//     容器互不可达，实现租户网络隔离。
//
// projectID 为空且未配置全局网络时返回错误（fail-closed，不回落共享网络）。
func ResolveNetworkName(cfg *config.AppConfig, projectID string) (string, error) {
	if name := cfg.GetFunctions().GetDocker().GetNetwork(); name != "" {
		return name, nil
	}
	if projectID == "" {
		return "", status.Error(codes.InvalidArgument, "project id is required for function execution")
	}
	// 纵深防御：即便上游漏校验，也不让非法字符进入 docker 网络名。
	if err := ident.ValidateSchemaResourceID(projectID); err != nil {
		return "", status.Errorf(codes.InvalidArgument, "invalid project id for function execution: %v", err)
	}
	return perProjectNetworkPrefix + projectID, nil
}

// resolveNetwork 转发到导出版 ResolveNetworkName。
func (d *DockerExecutor) resolveNetwork(projectID string) (string, error) {
	return ResolveNetworkName(d.cfg, projectID)
}

// ResolveInternalNetworkName 解析 internal 变体网络名（P2 egress 默认 deny；
// 导出供 functions-dispatcher 与 v1 执行器保持同一约定）：常规网络名 +
// "-int" 后缀（tw-func-<project>-int；显式全局网络配置同样加后缀）。
// projectID 校验与 ResolveNetworkName 同源。
func ResolveInternalNetworkName(cfg *config.AppConfig, projectID string) (string, error) {
	base, err := ResolveNetworkName(cfg, projectID)
	if err != nil {
		return "", err
	}
	return base + perProjectInternalNetworkSuffix, nil
}

// ensureNetwork 检查指定 bridge 网络存在，不存在则创建（幂等；失败不缓存）。
// internal=true 时创建 docker internal 网络（无外网出口，网内互通保留）。
// 网络创建后保留不删：函数容器按执行即起即毁，但并发/排队中的容器可能仍挂载
// 在该网络上，删除会打断在途执行；docker 网络本身无状态、开销可忽略，
// 生命周期随 daemon，无需清理任务。
func (d *DockerExecutor) ensureNetwork(ctx context.Context, name string, internal bool) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	d.netMu.Lock()
	defer d.netMu.Unlock()
	if d.netReady[name] {
		return nil
	}
	if _, err := cli.NetworkInspect(ctx, name, network.InspectOptions{}); err == nil {
		d.netReady[name] = true
		return nil
	}
	opts := network.CreateOptions{Driver: "bridge"}
	if internal {
		opts.Internal = true
	}
	if _, createErr := cli.NetworkCreate(ctx, name, opts); createErr != nil {
		// 创建失败但网络可能已被并发创建。
		if _, inspectErr := cli.NetworkInspect(ctx, name, network.InspectOptions{}); inspectErr == nil {
			d.netReady[name] = true
			return nil
		}
		return fmt.Errorf("ensure network %q: %w", name, createErr)
	}
	d.netReady[name] = true
	return nil
}

// Build 将 zip 代码包解压校验后构建为镜像 {registry}/func-{functionID}-{deploymentID}。
func (d *DockerExecutor) Build(ctx context.Context, functionID, deploymentID, zipPath string) error {
	cli, err := d.client()
	if err != nil {
		return err
	}

	buildDir, err := os.MkdirTemp("", "torchwood-build-*")
	if err != nil {
		return fmt.Errorf("create build dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(buildDir) }()

	runtime, err := extractZip(zipPath, buildDir)
	if err != nil {
		return err
	}
	dockerfile, err := dockerfileFor(runtime)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("write dockerfile: %w", err)
	}

	tarCtx, err := tarDir(buildDir)
	if err != nil {
		return fmt.Errorf("tar build context: %w", err)
	}
	opts := build.ImageBuildOptions{
		Tags:       []string{d.imageName(functionID, deploymentID)},
		Dockerfile: "Dockerfile",
		Remove:     true,
	}
	resp, err := cli.ImageBuild(ctx, tarCtx, opts)
	if err != nil {
		// daemon 侧构建失败时错误消息包含构建日志尾部。
		return fmt.Errorf("docker build failed: %s", truncateLog(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	// 读取构建输出（保留尾部 64KB）并扫描流内 {"error":...} JSON：
	// BuildKit 模式下构建失败不返回 Go error，只在流末尾携带 error 消息。
	log, buildErr := readBuildOutput(resp.Body)
	if buildErr != nil {
		return buildError(buildErr, log)
	}
	return nil
}

// Execute 运行构建产物镜像（安全基线 + TW_DATA 环境变量注入 + 超时清理）。
func (d *DockerExecutor) Execute(ctx context.Context, exec functions.Execution) (*functions.ExecutionResult, error) {
	if exec.DeploymentID == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment id is required")
	}
	cli, err := d.client()
	if err != nil {
		return nil, err
	}
	// egress 分类选网（P2 安全切片）：不可信函数（client_callable / 存在
	// http/cron 触发器，分类在 app 层完成）容器走 internal 变体网络——出网
	// 全 deny、网内互通保留；可信（server key 触发）保持常规网络。
	networkName, err := d.resolveNetwork(exec.ProjectID)
	if err != nil {
		return nil, err
	}
	networkInternal := false
	if exec.EgressUntrusted {
		networkName, err = ResolveInternalNetworkName(d.cfg, exec.ProjectID)
		if err != nil {
			return nil, err
		}
		networkInternal = true
	}
	observeEgressClass(exec.ProjectID, exec.EgressUntrusted)
	if err := d.ensureNetwork(ctx, networkName, networkInternal); err != nil {
		return nil, err
	}

	// execve 32KiB 单参数硬限制：data + env 合并预算兜底（app 层已校验）。
	budget := len(exec.Data)
	for k, v := range exec.Env {
		budget += len(k) + len(v)
	}
	if budget > maxExecEnvBudgetBytes {
		return nil, status.Errorf(codes.InvalidArgument, "data and environment variables exceed combined maximum of %d bytes", maxExecEnvBudgetBytes)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeoutFromExec(exec))
	defer cancel()

	res := specResources[exec.Spec]
	if res.cpu <= 0 {
		res = specResources["shared-1x"]
	}
	stopTimeout := 5
	env := []string{"TW_DATA=" + exec.Data}
	for k, v := range exec.Env {
		env = append(env, k+"="+v)
	}

	cfg := &container.Config{
		Image:       d.imageName(exec.FunctionID, exec.DeploymentID),
		Env:         env,
		StopTimeout: &stopTimeout,
	}
	hostCfg := &container.HostConfig{
		NetworkMode:    container.NetworkMode(networkName),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": ""},
		Resources: container.Resources{
			Memory:    res.memory,
			NanoCPUs:  int64(res.cpu * 1e9),
			PidsLimit: int64Ptr(512),
		},
	}

	created, err := cli.ContainerCreate(runCtx, cfg, hostCfg, &network.NetworkingConfig{}, nil, "")
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	containerID := created.ID
	// 独立超时 ctx：runCtx 此时可能已超时/取消，而 daemon 挂起时清理操作
	// 若用无超时的 Background 会无限阻塞调用链（defer 路径同样适用）。
	remove := func() {
		ctx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		defer cancel()
		_ = cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
	}
	defer remove()

	attach, err := cli.ContainerAttach(runCtx, containerID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("attach container: %w", err)
	}
	defer attach.Close()

	if err := cli.ContainerStart(runCtx, containerID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}

	// 流式收集 stdout/stderr（stdcopy 解复用）。
	var stdoutBuf, stderrBuf limitedBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = stdcopy.StdCopy(&stdoutBuf, &stderrBuf, attach.Reader)
	}()

	started := time.Now()
	waitCh, errCh := cli.ContainerWait(runCtx, containerID, container.WaitConditionNotRunning)
	select {
	case <-runCtx.Done():
		// 超时或调用方取消：停止并强制清理容器（无残留）。ContainerStop 使
		// 容器退出后 docker client 内部 goroutine 会向无缓冲的结果通道发送，
		// 此处必须异步排空 waitCh/errCh，否则该 goroutine（含 HTTP resp）
		// 永久阻塞泄漏——每次超时执行泄漏一个。独立超时 ctx：daemon 挂起时
		// 清理不无限阻塞调用链。
		stopCtx, stopCancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		_ = cli.ContainerStop(stopCtx, containerID, container.StopOptions{})
		stopCancel()
		go func() {
			select {
			case <-waitCh:
			case <-errCh:
			}
		}()
		<-done
		return nil, runCtx.Err()
	case err := <-errCh:
		<-done
		return nil, fmt.Errorf("wait container: %w", err)
	case wait := <-waitCh:
		if wait.Error != nil {
			<-done
			return nil, fmt.Errorf("container exited with error: %s", truncateLog(wait.Error.Message))
		}
		<-done
		statusCode := int(wait.StatusCode)

		stdout := stdoutBuf.String()
		stderr := stderrBuf.String()
		result := &functions.ExecutionResult{
			StatusCode: statusCode,
			Stdout:     stdout,
			Stderr:     stderr,
			Response:   parseResponse(stdout),
			DurationMS: time.Since(started).Milliseconds(),
		}
		return result, nil
	}
}

// RemoveImage 删除构建产物镜像（幂等）。
func (d *DockerExecutor) RemoveImage(ctx context.Context, functionID, deploymentID string) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	_, err = cli.ImageRemove(ctx, d.imageName(functionID, deploymentID), image.RemoveOptions{})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// extractZip 解压 zip 到 destDir（防 zip 炸弹与路径穿越），返回 runtime ID。
func extractZip(zipPath, destDir string) (string, error) {
	return extractZipWithLimits(zipPath, destDir, defaultZipExtractLimits)
}

// ExtractZip 是 extractZip 的导出版（functions-dispatcher 的 v2 构建复用
// 同一防 zip 炸弹/路径穿越预算）。
func ExtractZip(zipPath, destDir string) (string, error) {
	return extractZip(zipPath, destDir)
}

// extractZipWithLimits 是 extractZip 的可注入预算版本（测试用）：除声明侧
// UncompressedSize64 预检外，写入侧按实际字节计数强制预算，超限清理半成品。
func extractZipWithLimits(zipPath, destDir string, limits zipExtractLimits) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", status.Error(codes.InvalidArgument, "invalid zip file")
	}
	defer func() { _ = zr.Close() }()

	if len(zr.File) > limits.maxEntries {
		return "", status.Errorf(codes.InvalidArgument, "zip contains too many entries (max %d)", limits.maxEntries)
	}
	// declaredTotal 基于声明大小快速预检（低成本拒绝明显超限）；
	// actualTotal 按实际写入字节累计（防御声明大小与实际不符的伪造 zip）。
	var declaredTotal uint64
	var actualTotal int64
	hasIndexJS := false
	hasMainPy := false
	root := filepath.Clean(destDir)

	for _, f := range zr.File {
		if f.Mode()&os.ModeSymlink != 0 {
			return "", status.Error(codes.InvalidArgument, "zip entry is a symlink")
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > uint64(limits.maxEntryBytes) {
			_ = os.RemoveAll(destDir)
			return "", status.Errorf(codes.InvalidArgument, "zip entry %q exceeds %d bytes", f.Name, limits.maxEntryBytes)
		}
		declaredTotal += f.UncompressedSize64
		if declaredTotal > uint64(limits.maxTotalBytes) {
			_ = os.RemoveAll(destDir)
			return "", status.Errorf(codes.InvalidArgument, "zip total uncompressed size exceeds %d bytes", limits.maxTotalBytes)
		}

		// zip slip：Clean 后必须仍位于解压根目录内。
		name := filepath.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if name == "." || name == ".." || filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
			return "", status.Errorf(codes.InvalidArgument, "zip entry %q escapes root", f.Name)
		}
		target := filepath.Join(root, name)
		if !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return "", status.Errorf(codes.InvalidArgument, "zip entry %q escapes root", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", fmt.Errorf("create zip entry dir: %w", err)
		}
		src, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("open zip entry %q: %w", f.Name, err)
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			_ = src.Close()
			return "", fmt.Errorf("write zip entry %q: %w", f.Name, err)
		}
		// 写入侧按实际字节计数（不再仅信任 UncompressedSize64 声明值），
		// 预算（单条目或总预算）超限报错并清理整个解压目标目录——RemoveAll
		// 仅作用于本函数持有的 destDir 子树（zip-slip 校验保证条目写入始终
		// 位于其内），不会误删目录外内容，也不残留已解压的前序条目。
		n, copyErr := io.Copy(&budgetWriter{dst: dst, limit: limits.maxEntryBytes}, src)
		actualTotal += n
		_ = src.Close()
		closeErr := dst.Close()
		if copyErr != nil {
			_ = os.RemoveAll(destDir)
			if errors.Is(copyErr, errZipBudgetExceeded) {
				return "", status.Errorf(codes.InvalidArgument, "zip entry %q exceeds %d byte extraction budget", f.Name, limits.maxEntryBytes)
			}
			return "", fmt.Errorf("extract zip entry %q: %w", f.Name, copyErr)
		}
		if actualTotal > limits.maxTotalBytes {
			_ = os.RemoveAll(destDir)
			return "", status.Errorf(codes.InvalidArgument, "zip total uncompressed size exceeds %d bytes", limits.maxTotalBytes)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close zip entry %q: %w", f.Name, closeErr)
		}

		switch name {
		case "index.js":
			hasIndexJS = true
		case "main.py":
			hasMainPy = true
		}
	}

	switch {
	case hasIndexJS:
		return "node-18.0", nil
	case hasMainPy:
		return "python-3.11", nil
	default:
		return "", status.Error(codes.InvalidArgument, "missing entrypoint file: expected index.js (node) or main.py (python)")
	}
}

// dockerfileFor 生成运行时 Dockerfile（data 经 TW_DATA 环境变量传递，禁止拼接进命令）。
func dockerfileFor(runtime string) (string, error) {
	switch runtime {
	case "node-18.0":
		return "FROM node:18-alpine\n" +
			"WORKDIR /app\n" +
			"COPY . .\n" +
			"USER node\n" +
			`CMD ["node","-e","const {main}=require('./index');Promise.resolve(main(JSON.parse(process.env.TW_DATA||'{}'))).then(r=>console.log(JSON.stringify(r))).catch(e=>{console.error(e);process.exit(1)})"]` + "\n", nil
	case "python-3.11":
		return "FROM python:3.11-alpine\n" +
			"WORKDIR /app\n" +
			"COPY . .\n" +
			"USER 1000\n" +
			`CMD ["python","-c","import json,os,main;r=main.main(json.loads(os.environ.get('TW_DATA','{}')));print(json.dumps(r))"]` + "\n", nil
	default:
		return "", status.Errorf(codes.InvalidArgument, "unsupported runtime %q", runtime)
	}
}

// tarDir 将目录打包为 docker build context 的 tar 流。
func tarDir(dir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		// 镜像内文件 mode 不得依赖构建进程状态（与 functionsdispatcher/daemon.go
		// tarDir 同约定、同事故链）：上游 0644 写盘先被进程 umask 掩蔽，FileInfoHeader
		// 保留磁盘实际 mode 经 COPY 进镜像，模板 USER node（非 root）读用户代码
		// 即 EACCES。文件恒 0644、目录恒 0755（x 位供子目录遍历）、属主归零。
		if d.IsDir() {
			hdr.Mode = 0o755
			hdr.Name += "/"
		} else {
			hdr.Mode = 0o644
		}
		hdr.Uid = 0
		hdr.Gid = 0
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !d.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, f)
			_ = f.Close()
			if copyErr != nil {
				return copyErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// parseResponse 取 stdout 末行为合法 JSON 则原样返回，否则空串。
func parseResponse(stdout string) string {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if json.Valid([]byte(last)) {
		return last
	}
	return ""
}

// readBuildOutput 逐行读取 docker build 输出流，保留尾部 maxBuildLogBytes 字节，
// 并扫描 `{"error":...}` / `{"errorDetail":{"message":...}}` JSON（BuildKit 失败
// 消息位于流末尾，不产生 Go error）。返回 (日志尾部, 构建错误)。
func readBuildOutput(r io.Reader) (string, error) {
	var log tailBuffer
	var buildErr error
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxBuildLogLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = log.Write(append(line, '\n'))
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Error != "" {
			buildErr = errors.New(msg.Error)
			break
		}
		if msg.ErrorDetail.Message != "" {
			buildErr = errors.New(msg.ErrorDetail.Message)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return log.String(), err
	}
	return log.String(), buildErr
}

// ReadBuildOutput 是 readBuildOutput 的导出版（functions-dispatcher 的 v2
// 构建复用同一 BuildKit error 流解析）。
func ReadBuildOutput(r io.Reader) (string, error) {
	return readBuildOutput(r)
}

// buildError 组合构建失败错误：错误消息在前，日志尾部按总预算 64KB 裁剪在后。
func buildError(buildErr error, log string) error {
	msg := truncateLog(buildErr.Error())
	header := "docker build failed: " + msg + "\nbuild log tail:\n"
	room := maxBuildLogBytes - len(header)
	if room < 0 {
		room = 0
	}
	if len(log) > room {
		log = log[len(log)-room:]
	}
	return errors.New(header + log)
}

func truncateLog(s string) string {
	if len(s) <= maxBuildLogBytes {
		return s
	}
	return s[:maxBuildLogBytes]
}

// TruncateBuildLog 是 truncateLog 的导出版（构建日志裁剪口径共用）。
func TruncateBuildLog(s string) string { return truncateLog(s) }

func timeoutFromExec(exec functions.Execution) time.Duration {
	if exec.Timeout <= 0 {
		return 15 * time.Second
	}
	return time.Duration(exec.Timeout) * time.Second
}

func int64Ptr(v int64) *int64 { return &v }

// limitedBuffer 是带上限的 bytes.Buffer（防止恶意输出耗尽内存）。
type limitedBuffer struct {
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > maxContainerOutSize {
		remaining := maxContainerOutSize - b.buf.Len()
		if remaining > 0 {
			_, _ = b.buf.Write(p[:remaining])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string { return b.buf.String() }

// tailBuffer 仅保留最后 maxBuildLogBytes 字节（构建失败原因通常在输出末尾）。
type tailBuffer struct {
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > maxBuildLogBytes {
		drop := len(b.buf) - maxBuildLogBytes
		copy(b.buf, b.buf[drop:])
		b.buf = b.buf[:maxBuildLogBytes]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
