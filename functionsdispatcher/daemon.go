package functionsdispatcher

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/functions/runner"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Instance 是 spawn 成功后的容器句柄（IP 由 inspect 取得，供池内 HTTP 分发）。
type Instance struct {
	ContainerID string
	IP          string
}

// Daemon 是池管理器对 docker daemon 操作的抽象（fake 测试用；真实实现
// dockerDaemon 持有唯一 docker.sock 访问权）。
type Daemon interface {
	// EnsureProjectNetwork 确保项目函数网络存在，并将 dispatcher 自身容器
	// attach 进去（方案③核心：跨 bridge 无路由，dispatcher 不 join 网络
	// 则容器 IP 物理不可达）。untrusted=true 时确保/attach 的是 internal
	// 变体网络（tw-func-<project>-int，docker internal: true——出网全 deny；
	// P2 egress 默认 deny，设计 Security #6）——分发与函数回访平台 API 都走
	// 桥内可达地址，两类网络 dispatcher 都会随执行分布逐渐 join。宿主进程
	// 模式（检测不到自身容器 ID）跳过 attach——宿主可直接路由 user-defined
	// bridge 网段。
	EnsureProjectNetwork(ctx context.Context, projectID string, untrusted bool) (string, error)
	// SpawnInstance 创建并启动常驻实例容器（镜像/env/资源规格），返回句柄。
	SpawnInstance(ctx context.Context, opts SpawnOptions) (Instance, error)
	// InspectInstance 返回容器运行状态与 IP；容器不存在返回 errdefs.NotFound。
	InspectInstance(ctx context.Context, containerID string) (running bool, ip string, err error)
	// StopInstance 停止容器：timeout > 0 先 SIGTERM 宽限（drain），到点
	// daemon 侧转 SIGKILL；timeout <= 0 直接 SIGKILL（请求超时/崩溃回收路径，
	// 复用 v1 ContainerStop 原语的强制语义）。
	StopInstance(ctx context.Context, containerID string, timeout time.Duration) error
	// RemoveInstance 强制删除容器（幂等）。
	RemoveInstance(ctx context.Context, containerID string) error
	// BuildImage 以 v2 runner 模板构建镜像（zip 字节内联；构建期不执行用户
	// 代码的不变量由模板层保持——runner 仅被 COPY）。
	BuildImage(ctx context.Context, functionID, deploymentID string, zip []byte) error
	// RemoveImage 删除镜像（幂等）。
	RemoveImage(ctx context.Context, functionID, deploymentID string) error
}

// SpawnOptions 是 SpawnInstance 的入参。
type SpawnOptions struct {
	ProjectID string
	Image     string
	Network   string
	// Env 为容器环境变量（用户 variables + runner 控制变量；不含
	// TW_DATA/TW_EXECUTION_TOKEN——v2 语义下二者经请求体/分发 header 传递）。
	Env         []string
	Spec        string // 资源规格（shared-1x / shared-2x）
	MaxRequests int    // 写入 TW_MAX_REQUESTS（runner 自回收阈值）
}

// dockerCleanupTimeout 是清理类操作（stop/remove）的独立超时：不继承已
// 取消的执行 ctx，也不允许 daemon 挂起时无限阻塞（与 v1 同约定）。
const dockerCleanupTimeout = 30 * time.Second

// dockerDaemon 是 Daemon 的真实实现（进程内唯一 docker.sock 持有方）。
type dockerDaemon struct {
	cfg *config.AppConfig
	cli *client.Client
	// selfContainerID 非空 = dispatcher 自身运行在容器内（自 attach 需要）。
	selfContainerID string
}

// NewDockerDaemon 构造真实 daemon 实现。docker client 构造失败延迟到首次
// 调用暴露（与 dockerExecutor 同策略）。
func NewDockerDaemon(cfg *config.AppConfig) Daemon {
	d := &dockerDaemon{cfg: cfg}
	host := cfg.GetFunctions().GetDocker().GetHost()
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		d.cli = nil
		return d
	}
	d.cli = cli
	d.selfContainerID = detectSelfContainerID(cli)
	return d
}

// detectSelfContainerID 探测 dispatcher 自身容器 ID：容器内 HOSTNAME 即短
// ID，经 container inspect 验证（验证失败 = 宿主进程模式，跳过自 attach）。
func detectSelfContainerID(cli *client.Client) string {
	hn, err := os.Hostname()
	if err != nil || hn == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cli.ContainerInspect(ctx, hn); err != nil {
		return ""
	}
	return hn
}

func (d *dockerDaemon) client() (*client.Client, error) {
	if d.cli == nil {
		return nil, status.Error(codes.Internal, "docker client unavailable (dispatcher requires docker.sock)")
	}
	return d.cli, nil
}

// EnsureProjectNetwork 解析/创建项目网络并自 attach。常规网络命名与 v1
// dockerExecutor 同一约定（tw-func-<project>；显式 functions.docker.network
// 覆盖为全局共享网络）；untrusted=true 时为 internal 变体
// tw-func-<project>-int（internal: true，出网全 deny——P2 egress 默认 deny，
// 分类在 app 层完成，daemon 只按标志选网）。
func (d *dockerDaemon) EnsureProjectNetwork(ctx context.Context, projectID string, untrusted bool) (string, error) {
	cli, err := d.client()
	if err != nil {
		return "", err
	}
	var name string
	if untrusted {
		name, err = infrafunctions.ResolveInternalNetworkName(d.cfg, projectID)
	} else {
		name, err = infrafunctions.ResolveNetworkName(d.cfg, projectID)
	}
	if err != nil {
		return "", err
	}
	if _, err := cli.NetworkInspect(ctx, name, network.InspectOptions{}); err != nil {
		opts := network.CreateOptions{Driver: "bridge"}
		if untrusted {
			opts.Internal = true
		}
		if _, createErr := cli.NetworkCreate(ctx, name, opts); createErr != nil {
			// 创建失败但网络可能已被并发创建。
			if _, inspectErr := cli.NetworkInspect(ctx, name, network.InspectOptions{}); inspectErr != nil {
				return "", fmt.Errorf("ensure network %q: %w", name, createErr)
			}
		}
	}
	if d.selfContainerID != "" {
		// 自 attach（幂等：已在网/路由已存在的冲突类错误吞掉）。失败不静默
		// ——attach 不上去后续分发必然不可达，直接暴露错误。
		if err := cli.NetworkConnect(ctx, name, d.selfContainerID, &network.EndpointSettings{}); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, "is already attached") && !errdefs.IsConflict(err) {
				return "", fmt.Errorf("attach dispatcher to network %q: %w", name, err)
			}
		}
	}
	// 回访容器 attach（P2 部署前提）：函数经容器名 DNS 回访平台 API
	// （functions.execution.api_base_url 指向该名字）。untrusted 函数在
	// internal 网络（无 NAT 出口），这是其回访平台的唯一通路。attach 失败
	// 不阻断执行——函数可能无需回访平台，但每次都记警告（部署应在首次
	// 执行前修正 callback_container 配置）。已在网冲突幂等吞掉。
	if cbName := d.cfg.GetFunctions().GetDispatcher().GetCallbackContainer(); cbName != "" {
		if err := cli.NetworkConnect(ctx, name, cbName, &network.EndpointSettings{}); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, "is already attached") && !errdefs.IsConflict(err) {
				slog.Warn("functions-dispatcher: attach callback container to function network failed; platform callbacks from functions may be unreachable",
					"network", name, "container", cbName, "error", err)
			}
		}
	}
	return name, nil
}

// SpawnInstance 创建并启动容器；返回前 inspect 取 IP（分配失败即报错，
// 不返回无 IP 的半成品实例）。
func (d *dockerDaemon) SpawnInstance(ctx context.Context, opts SpawnOptions) (Instance, error) {
	cli, err := d.client()
	if err != nil {
		return Instance{}, err
	}
	res := infrafunctions.SpecResources(opts.Spec)
	stopTimeout := 10
	env := make([]string, 0, len(opts.Env)+2)
	env = append(env, opts.Env...)
	env = append(env,
		fmt.Sprintf("TW_MAX_REQUESTS=%d", opts.MaxRequests),
		fmt.Sprintf("TW_DRAIN_TIMEOUT_MS=%d", (10*time.Second).Milliseconds()),
	)
	cfg := &container.Config{
		Image:       opts.Image,
		Env:         env,
		StopTimeout: &stopTimeout,
	}
	hostCfg := &container.HostConfig{
		NetworkMode:    container.NetworkMode(opts.Network),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": ""},
		Resources: container.Resources{
			Memory:    res.Memory,
			NanoCPUs:  res.NanoCPUs,
			PidsLimit: int64Ptr(512),
		},
	}
	created, err := cli.ContainerCreate(ctx, cfg, hostCfg, &network.NetworkingConfig{}, nil, "")
	if err != nil {
		return Instance{}, fmt.Errorf("create resident instance: %w", err)
	}
	cleanup := func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		_ = cli.ContainerRemove(rmCtx, created.ID, container.RemoveOptions{Force: true})
		cancel()
	}
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		cleanup()
		return Instance{}, fmt.Errorf("start resident instance: %w", err)
	}
	running, ip, err := d.InspectInstance(ctx, created.ID)
	if err != nil || !running || ip == "" {
		cleanup()
		if err != nil {
			return Instance{}, fmt.Errorf("inspect resident instance: %w", err)
		}
		return Instance{}, status.Errorf(codes.Internal, "resident instance not running or has no IP")
	}
	return Instance{ContainerID: created.ID, IP: ip}, nil
}

// InspectInstance 返回运行状态与首个非空地址。
func (d *dockerDaemon) InspectInstance(ctx context.Context, containerID string) (bool, string, error) {
	cli, err := d.client()
	if err != nil {
		return false, "", err
	}
	ins, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return false, "", err
	}
	running := ins.State != nil && ins.State.Running
	if ins.NetworkSettings != nil {
		for _, ep := range ins.NetworkSettings.Networks {
			if ep != nil && ep.IPAddress != "" {
				return running, ep.IPAddress, nil
			}
		}
	}
	return running, "", nil
}

// StopInstance 停止容器。timeout <= 0 直接 SIGKILL。
func (d *dockerDaemon) StopInstance(ctx context.Context, containerID string, timeout time.Duration) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	opts := container.StopOptions{}
	if timeout > 0 {
		t := int(timeout.Seconds())
		if t == 0 {
			t = 1
		}
		opts.Timeout = &t
	} else {
		opts.Signal = "SIGKILL"
	}
	if err := cli.ContainerStop(ctx, containerID, opts); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stop resident instance: %w", err)
	}
	return nil
}

// RemoveInstance 强删容器（幂等）。
func (d *dockerDaemon) RemoveInstance(ctx context.Context, containerID string) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	if err := cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove resident instance: %w", err)
	}
	return nil
}

// BuildImage 以 v2 runner 模板构建镜像：zip 解压校验（复用 v1 的防炸弹
// 预算）→ 写入 runner + Dockerfile（CMD = runner，非 ENTRYPOINT）→ tar
// build context → docker build。构建期不执行用户代码。
func (d *dockerDaemon) BuildImage(ctx context.Context, functionID, deploymentID string, zip []byte) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	buildDir, err := os.MkdirTemp("", "torchwood-dispatch-build-*")
	if err != nil {
		return fmt.Errorf("create build dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(buildDir) }()

	tmpZip, err := os.CreateTemp("", "torchwood-dispatch-src-*.zip")
	if err != nil {
		return fmt.Errorf("stage zip: %w", err)
	}
	defer func() { _ = os.Remove(tmpZip.Name()) }()
	if _, err := tmpZip.Write(zip); err != nil {
		_ = tmpZip.Close()
		return fmt.Errorf("stage zip: %w", err)
	}
	_ = tmpZip.Close()

	// extractZip 与镜像名解析复用 v1 语义（防 zip 炸弹/路径穿越预算一致；
	// 镜像命名约定不变）。
	runtime, err := infrafunctions.ExtractZip(tmpZip.Name(), buildDir)
	if err != nil {
		return err
	}
	dockerfile, err := runner.DockerfileFor(runtime)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildDir, runner.RunnerFileName), runner.NodeRunnerJS(), 0o644); err != nil {
		return fmt.Errorf("write runner: %w", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("write dockerfile: %w", err)
	}

	tarCtx, err := tarDir(buildDir)
	if err != nil {
		return fmt.Errorf("tar build context: %w", err)
	}
	opts := build.ImageBuildOptions{
		Tags:       []string{infrafunctions.ImageName(d.cfg, functionID, deploymentID)},
		Dockerfile: "Dockerfile",
		Remove:     true,
	}
	resp, err := cli.ImageBuild(ctx, tarCtx, opts)
	if err != nil {
		return fmt.Errorf("docker build failed: %s", infrafunctions.TruncateBuildLog(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	// BuildKit 失败在流内 error JSON，复用 v1 读取/裁剪逻辑。
	_, buildErr := infrafunctions.ReadBuildOutput(resp.Body)
	if buildErr != nil {
		return buildErr
	}
	return nil
}

// RemoveImage 删除构建产物镜像（幂等）。
func (d *dockerDaemon) RemoveImage(ctx context.Context, functionID, deploymentID string) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	if _, err := cli.ImageRemove(ctx, infrafunctions.ImageName(d.cfg, functionID, deploymentID), image.RemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// tarDir 将目录打包为 build context tar（与 infra/functions 同构；dispatcher
// 侧独立实现避免导出面扩散）。
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
		if d.IsDir() {
			hdr.Name += "/"
		}
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

func int64Ptr(v int64) *int64 { return &v }
