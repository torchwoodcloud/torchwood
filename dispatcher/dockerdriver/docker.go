// Package dockerdriver 是 dispatcher 的 docker 直接执行底座（IMPL-T2-5
// 双执行底座的 docker 形态；从 git 54b666b 的 dispatcher/daemon.go 复活并
// 适配现行 Daemon 接口）：
//
//	driver="docker"（functions.driver）时，dispatcher 进程持有 docker.sock
//	（本包是本仓唯一的 docker client 面），函数实例 = 常驻容器（per-project
//	bridge 网络 tw-func-<project>[-int]，self attach + 回访容器 attach），
//	构建 = 本地 docker build（zip → tar → build），导入 = pull → digest 钉
//	死 → retag 平台命名。单机 local 形态为必达口径；多节点细胞模型与
//	registry push/pull 路由维持 IMPL-T2-3 的退役裁决不复归（镜像逻辑名即
//	本地 tag，零产物引用映射）。
//
// 池语义（租约/保温/熔断/TW_MAX_REQUESTS）在 dispatcher 包不动，本包只
// 是 Daemon 接口的另一执行底座实现；与 fleetly 形态共享构建上下文编排、
// tar 流、验证 spawn 与镜像源准入（dispatcher 包导出的驱动共享面）。
//
// 隔离边界：本包只被组合根（cmd/dispatcher）blank-import——fleetly 驱动
// 路径零 docker client 的机制断言以该包边界成立。
package dockerdriver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/torchwoodcloud/torchwood/dispatcher"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// defaultHost 是 functions.docker.host 未配置时的 docker daemon 缺省地址
// （容器形态经 docker.sock 挂载可达；非 Linux 宿主进程模式由
// TORCHWOOD_FUNCTIONS_DOCKER_HOST 显式指定）。
const defaultHost = "unix:///var/run/docker.sock"

// imageClient 收窄镜像导入依赖的 docker 镜像操作面（真实实现 =
// *client.Client；单测注入 fake 驱动 pull→inspect→tag→remove 序列的
// 确定性验证，不依赖真实 daemon）。
type imageClient interface {
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	ImageInspect(ctx context.Context, imageID string, inspectOpts ...client.ImageInspectOption) (image.InspectResponse, error)
	ImageTag(ctx context.Context, source, target string) error
	ImageRemove(ctx context.Context, imageID string, options image.RemoveOptions) ([]image.DeleteResponse, error)
}

// containerClient 是容器创建操作的收窄视图（真实实现 = *client.Client；独立
// 字段仅为单测可注入）。覆盖 SpawnInstance 的 create + start + 失败清理
// remove 原语（镜像缺失类型化上抛的判定面）。
type containerClient interface {
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
}

// lifecycleClient 是常驻实例生命周期操作的收窄视图（真实实现 =
// *client.Client；独立字段仅为单测可注入——与 netCli/imgCli/containerCli
// 同款收窄注入）。覆盖 Inspect/Stop/Remove/LogsTail 四原语：inspect 取 IP、
// stop 信号映射、remove 幂等、logs stdcopy 解复用（验证 spawn 失败的第一
// 现场诊断面）。
type lifecycleClient interface {
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)
}

// networkClient 收窄 EnsureProjectNetwork 依赖的 docker 网络操作面（真实
// 实现 = *client.Client；单测注入 fake 驱动 attach 幂等/自愈路径的确定性
// 验证，不依赖真实 daemon）。
type networkClient interface {
	NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
}

// daemon 是 dispatcher.Daemon 的 docker 直接执行实现（单机形态）。
type daemon struct {
	cfg *config.AppConfig
	cli *client.Client
	// netCli/imgCli/containerCli 是 cli 的收窄视图（生产与 cli 同一对象；
	// 独立字段仅为单测可注入——与 fleetly 形态的收窄注入同款）。
	netCli       networkClient
	imgCli       imageClient
	containerCli containerClient
	// lifecycleCli 是常驻实例生命周期四原语的收窄视图（生产 = cli；单测
	// 注入 fake 驱动 IP 提取/信号映射/幂等删/日志解复用的确定性验证）。
	lifecycleCli lifecycleClient
	// selfContainerID 非空 = dispatcher 自身运行在容器内（自 attach 需要）。
	selfContainerID string
	// probe 是验证 spawn 的 health 探针（生产 = dispatcher.NewHTTPProber；
	// 测试注入 fake）。
	probe dispatcher.HealthProber
	// ——部署后验证 spawn 参数（从 config 一次性解析，与池共享语义）——
	// bootTimeout 是验证实例 health 探针预算（= 池 boot_timeout）。
	bootTimeout time.Duration
	// maxRequestsDefault 是验证实例 TW_MAX_REQUESTS 注入值（对齐池缺省）。
	maxRequestsDefault int
}

// init 注册 docker 底座构造函数：组合根（cmd/dispatcher）blank-import 本
// 包后，dispatcher 按 functions.driver 选择本实现。
func init() {
	dispatcher.RegisterDockerDriver(New)
}

// New 构造 docker 直接执行底座（dispatcher.DockerDriverFactory 形态）。
// host 缺省 unix:///var/run/docker.sock（票面口径）；client 构造失败在
// 启动期显式暴露（组合根构造即失败，不留到首次调用）。
func New(cfg *config.AppConfig) (dispatcher.Daemon, error) {
	host := cfg.GetFunctions().GetDocker().GetHost()
	if strings.TrimSpace(host) == "" {
		host = defaultHost
	}
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client (host %q): %w", host, err)
	}
	pc := dispatcher.PoolConfigFromConfig(cfg)
	d := &daemon{
		cfg:                cfg,
		cli:                cli,
		netCli:             cli,
		imgCli:             cli,
		containerCli:       cli,
		lifecycleCli:       cli,
		probe:              dispatcher.NewHTTPProber(),
		bootTimeout:        pc.BootTimeout,
		maxRequestsDefault: pc.MaxRequestsDefault,
	}
	d.selfContainerID = detectSelfContainerID(cli)
	return d, nil
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

// ——attach 幂等与陈旧 endpoint 自愈（54b666b 语义原样保留）——

// attach 冲突的两类报错文本判据：前者是容器已在网（幂等吞掉）；后者是网络
// 里存在同名 endpoint 残留——endpoint 默认以容器名命名，守护进程重启/容器
// 非优雅重建后，自建的 tw-func-* 网络（不受 compose 管理，跨重建存活）会
// 残留上一代同名容器的 endpoint 记录，attach 从此确定性失败（2026-09-14
// dev 事故：monsters 项目函数队列永久卡死）。后者文本自 moby 2016 起稳定，
// 判据够窄，区别于已在网类冲突。
const (
	alreadyAttachedMsgFragment = "is already attached"
	staleEndpointMsgFragment   = "already exists in network"
)

// isAlreadyConnectedErr 判定 NetworkConnect 错误是否为"容器已在网"类幂等冲突。
func isAlreadyConnectedErr(err error) bool {
	return err != nil && (strings.Contains(err.Error(), alreadyAttachedMsgFragment) || errdefs.IsConflict(err))
}

// ensureConnected 将容器 attach 到指定网络：已在网类冲突幂等吞掉；检出陈旧
// endpoint 名冲突（staleEndpointMsgFragment）时 force disconnect 摘除残留
// 记录再重试一次 connect 自愈。disconnect 报 NotFound = 残留已被并发 healer
// 或外部摘除，视为已愈；重连撞上并发 healer 已挂成功（已在网类）同样吞掉。
// disconnect 失败（除 NotFound）或重连仍失败才上抛。
func ensureConnected(ctx context.Context, netCli networkClient, name, containerID string) error {
	err := netCli.NetworkConnect(ctx, name, containerID, &network.EndpointSettings{})
	if err == nil || isAlreadyConnectedErr(err) {
		return nil
	}
	if !strings.Contains(err.Error(), staleEndpointMsgFragment) {
		return err
	}
	if derr := netCli.NetworkDisconnect(ctx, name, containerID, true); derr != nil && !errdefs.IsNotFound(derr) {
		return fmt.Errorf("heal stale endpoint (disconnect %q from network %q): %w", containerID, name, derr)
	}
	if rerr := netCli.NetworkConnect(ctx, name, containerID, &network.EndpointSettings{}); rerr != nil && !isAlreadyConnectedErr(rerr) {
		return fmt.Errorf("heal stale endpoint (reconnect %q to network %q): %w", containerID, name, rerr)
	}
	return nil
}

// EnsureProjectNetwork 确保项目函数网络存在，并将 dispatcher 自身容器
// attach 进去（跨 bridge 无路由，dispatcher 不 join 网络则容器 IP 物理不可
// 达）。untrusted=true 时确保/attach 的是 internal 变体网络
// （tw-func-<project>-int，docker internal: true——出网全 deny）。宿主进程
// 模式（检测不到自身容器 ID）跳过自 attach——宿主可直接路由 user-defined
// bridge 网段。回访容器 attach（functions.docker.callback_container）：
// 函数经容器名 DNS 回访平台 API；attach 失败不阻断执行（函数可能无需回访
// 平台），但每次记警告（部署应在首次执行前修正配置）。
func (d *daemon) EnsureProjectNetwork(ctx context.Context, projectID string, untrusted bool) (string, error) {
	if d.netCli == nil {
		return "", status.Error(codes.Internal, "docker client unavailable (dispatcher requires docker.sock)")
	}
	cli := d.netCli
	var name string
	var err error
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
		// 自 attach（幂等 + 陈旧 endpoint 自愈，见 ensureConnected）。失败不
		// 静默——attach 不上去后续分发必然不可达，直接暴露错误。
		if err := ensureConnected(ctx, cli, name, d.selfContainerID); err != nil {
			return "", fmt.Errorf("attach dispatcher to network %q: %w", name, err)
		}
	}
	if cbName := strings.TrimSpace(d.cfg.GetFunctions().GetDocker().GetCallbackContainer()); cbName != "" {
		if err := ensureConnected(ctx, cli, name, cbName); err != nil {
			slog.Warn("dispatcher: attach callback container to function network failed; platform callbacks from functions may be unreachable",
				"network", name, "container", cbName, "error", err)
		}
	}
	return name, nil
}

// SpawnInstance 创建并启动常驻实例容器；返回前 inspect 取 IP（分配失败即
// 报错，不返回无 IP 的半成品实例）。加固与资源约束沿用 54b666b 语义
// （CapDrop ALL / no-new-privileges / 只读 rootfs / tmpfs /tmp / pids 上限）。
func (d *daemon) SpawnInstance(ctx context.Context, opts dispatcher.SpawnOptions) (dispatcher.Instance, error) {
	createCli := d.containerCli
	if createCli == nil {
		return dispatcher.Instance{}, status.Error(codes.Internal, "docker client unavailable (dispatcher requires docker.sock)")
	}
	res := infrafunctions.SpecResources(opts.Spec)
	stopTimeout := 10
	// 自回收控制键（TW_MAX_REQUESTS/TW_DRAIN_TIMEOUT_MS）与 fleetly 形态
	// 同源：共享构造器单点注入（漏注入 = runner 永不自退静默泄漏）。
	env := dispatcher.AppendRunnerControlEnv(opts.Env, opts.MaxRequests)
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
	created, err := createCli.ContainerCreate(ctx, cfg, hostCfg, &network.NetworkingConfig{}, nil, opts.Name)
	if err != nil {
		// 镜像缺失类型化上抛（执行链自愈，rebuild 语义）：local 形态无 pull
		// 自愈，spawn 必然反复失败——与其烧满队首超时，不如立即以
		// FailedPrecondition + ImageMissingMarker 上抛，server 侧据此触发
		// 自动重建。NotFound 兜底含网络缺失等形态，以 daemon 文案二次收窄。
		if errdefs.IsNotFound(err) && strings.Contains(err.Error(), "No such image") {
			return dispatcher.Instance{}, status.Errorf(codes.FailedPrecondition, "%s %q on this node (rebuild required): %v",
				domainfunctions.ImageMissingMarker, opts.Image, err)
		}
		return dispatcher.Instance{}, fmt.Errorf("create resident instance %s: %w", opts.Name, err)
	}
	cleanup := func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), dispatcher.CleanupTimeout)
		_ = createCli.ContainerRemove(rmCtx, created.ID, container.RemoveOptions{Force: true})
		cancel()
	}
	if err := createCli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		cleanup()
		return dispatcher.Instance{}, fmt.Errorf("start resident instance: %w", err)
	}
	running, ip, err := d.InspectInstance(ctx, created.ID)
	if err != nil || !running || ip == "" {
		cleanup()
		if err != nil {
			return dispatcher.Instance{}, fmt.Errorf("inspect resident instance: %w", err)
		}
		return dispatcher.Instance{}, status.Error(codes.Internal, "resident instance not running or has no IP")
	}
	return dispatcher.Instance{ContainerID: created.ID, IP: ip}, nil
}

// InspectInstance 返回运行状态与首个非空地址（bridge 网络 IP——池的 HTTP
// 分发与健康探针按它寻址；fleetly 形态的对应语义是任务 DNS 名）。
func (d *daemon) InspectInstance(ctx context.Context, containerID string) (bool, string, error) {
	ins, err := d.lifecycleCli.ContainerInspect(ctx, containerID)
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

// StopInstance 停止容器。timeout > 0 先 SIGTERM 宽限（drain），到点 daemon
// 侧转 SIGKILL；timeout <= 0 直接 SIGKILL（请求超时/崩溃回收路径）。
func (d *daemon) StopInstance(ctx context.Context, containerID string, timeout time.Duration) error {
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
	if err := d.lifecycleCli.ContainerStop(ctx, containerID, opts); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stop resident instance: %w", err)
	}
	return nil
}

// RemoveInstance 强删容器（幂等）。
func (d *daemon) RemoveInstance(ctx context.Context, containerID string) error {
	if err := d.lifecycleCli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove resident instance: %w", err)
	}
	return nil
}

// InstanceLogsTail 返回容器日志尾部（stdout/stderr 解复用合并，取末尾
// limit 字节）：验证 spawn 失败的第一现场诊断面。spawn 的容器无 TTY——
// daemon 返回多路复用流，stdcopy 解复用。容器已退出仍可读。
func (d *daemon) InstanceLogsTail(ctx context.Context, containerID string, limit int64) (string, error) {
	logs, err := d.lifecycleCli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "200",
	})
	if err != nil {
		return "", fmt.Errorf("container logs: %w", err)
	}
	defer func() { _ = logs.Close() }()
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, logs); err != nil {
		return "", fmt.Errorf("demux container logs: %w", err)
	}
	out := buf.Bytes()
	if int64(len(out)) > limit {
		out = out[int64(len(out))-limit:]
	}
	return string(out), nil
}

// BuildImage 以 runner 模板构建镜像（zip 字节内联；构建期不执行用户代码
// 的不变量由模板层保持）：构建上下文准备（与 fleetly 驱动共享的
// PrepareBuildContext）→ 流式 tar build context → docker build → 部署后
// 验证 spawn（配置开启时）。产物 tag = 镜像逻辑名（单机 local 形态：逻辑
// 名即本地 tag，零产物引用映射——偏离 54b666b 的 registry push 腿，见
// 实施记录）。
func (d *daemon) BuildImage(ctx context.Context, opts dispatcher.BuildImageOptions) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	buildDir, err := os.MkdirTemp("", "torchwood-dispatch-build-*")
	if err != nil {
		return fmt.Errorf("create build dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(buildDir) }()

	if err := dispatcher.PrepareBuildContext(buildDir, opts); err != nil {
		return err
	}

	ref := infrafunctions.ImageName(d.cfg, opts.FunctionID, opts.DeploymentID)
	buildOpts := build.ImageBuildOptions{
		Tags:       []string{ref},
		Dockerfile: "Dockerfile",
		Remove:     true,
	}
	// 流式 build context（共享 TarDir）：ImageBuild 失败路径必须 Close 读端
	// ——唤醒可能仍阻塞在 pipe 写侧的打包 goroutine；成功路径由 transport
	// 关闭请求 body，goroutine 自然收尾。
	tarCtx := dispatcher.TarDir(buildDir)
	resp, err := cli.ImageBuild(ctx, tarCtx, buildOpts)
	if err != nil {
		_ = tarCtx.Close()
		return fmt.Errorf("docker build failed: %s", infrafunctions.TruncateBuildLog(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	// BuildKit 失败在流内 error JSON，复用共享读取/裁剪逻辑。
	_, buildErr := infrafunctions.ReadBuildOutput(resp.Body)
	if buildErr != nil {
		return buildErr
	}
	// 部署后验证 spawn（D10）：opts.Verify=false（config verify_build 显式
	// 关闭）跳过整段。验证镜像 = 刚构建的本地 tag。
	if opts.Verify {
		return dispatcher.SpawnVerifyInstance(ctx, d, d.probe, opts, ref, d.bootTimeout, d.maxRequestsDefault)
	}
	return nil
}

// ImportImage 导入外部镜像（BYO 镜像源；编排见 importImage）。
func (d *daemon) ImportImage(ctx context.Context, opts dispatcher.ImportImageOptions) (string, error) {
	imgs, err := d.images()
	if err != nil {
		return "", err
	}
	return importImage(ctx, d, imgs, d.probe, d.cfg, opts, d.bootTimeout, d.maxRequestsDefault)
}

// images 返回镜像操作收窄视图（client 不可用时 Internal 暴露）。
func (d *daemon) images() (imageClient, error) {
	if d.imgCli == nil {
		return nil, status.Error(codes.Internal, "docker client unavailable (dispatcher requires docker.sock)")
	}
	return d.imgCli, nil
}

// RemoveImage 删除构建产物镜像（幂等；单机形态逻辑名即本地 tag）。
func (d *daemon) RemoveImage(ctx context.Context, functionID, deploymentID string) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	if _, err := cli.ImageRemove(ctx, infrafunctions.ImageName(d.cfg, functionID, deploymentID), image.RemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

func (d *daemon) client() (*client.Client, error) {
	if d.cli == nil {
		return nil, status.Error(codes.Internal, "docker client unavailable (dispatcher requires docker.sock)")
	}
	return d.cli, nil
}

func int64Ptr(v int64) *int64 { return &v }
