package dispatcher

import (
	"archive/tar"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	"github.com/docker/docker/pkg/stdcopy"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/functions/runner"
	"github.com/torchwoodcloud/torchwood/internal/infra/functions/runner/gorunner"
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
	// InstanceLogsTail 返回容器日志尾部（stdout/stderr 解复用合并，取末尾
	// limit 字节）：验证 spawn 失败的第一现场（panic/协议未实现都在容器
	// stdout，设计 §1「失败时回收容器日志尾部」）；容器已退出仍可读。
	InstanceLogsTail(ctx context.Context, containerID string, limit int64) (string, error)
	// BuildImage 以 runner 模板构建镜像（zip 字节内联；构建期不执行用户
	// 代码的不变量由模板层保持——runner 仅被 COPY/编译）。构建链载荷一期
	// 定稿形态（设计 §0），runtime 对账与 go bootstrap 生成在实现内完成。
	// 四期 4b（M1）：build → verify → push 顺序定稿（验证失败不污染
	// registry）——routing_mode="registry" 且 registry_push=true 时，构建
	// 与验证成功后把镜像 push 到 functions.docker.registry（镜像全局化）；
	// push 失败 = 构建失败（deployment failed）。
	BuildImage(ctx context.Context, opts BuildImageOptions) error
	// EnsureImage 确保部署镜像在本节点 docker 可用（四期 4b M1 registry
	// 模式冷启动的前置钩子）：本地命中（ImageInspect）零网络直接返回；
	// miss → ImagePull（RegistryAuth 留空 = daemon 侧已登录凭证）后返回；
	// pull 失败以明确错误收场（含 pull 摘要）。调用方（池）按 routing_mode
	// 门控——local 模式镜像不分发，不会到达这里。
	EnsureImage(ctx context.Context, imageRef string) error
	// ImportImage 拉取外部镜像导入为平台镜像（三期阶段三，设计 §3）：host
	// 名称级校验 → pull（ExpectedDigest 本地命中零 pull）→ digest 钉死 →
	// retag 进平台命名并删原始引用标签 → 强制契约验证 spawn → 返回钉死
	// digest（调用方落 deployment.source_ref）。编排见 importImage。
	ImportImage(ctx context.Context, opts ImportImageOptions) (string, error)
	// RemoveImage 删除镜像（幂等）。
	RemoveImage(ctx context.Context, functionID, deploymentID string) error
}

// ImportImageOptions 是 ImportImage 的入参（镜像源导入链全量载荷，与
// BuildImageOptions 同风格；由 handleImportImage 从 ImportImageRequest 组装）。
type ImportImageOptions struct {
	ProjectID    string
	FunctionID   string
	DeploymentID string
	// Reference 是用户提交的原始镜像引用（host/repo[:tag|@sha256:...]），
	// 与部署行 source_url 同值。
	Reference string
	// RegistryUsername/RegistryToken 是一次性 registry 凭证（base64
	// RegistryAuth 后单次转发 daemon pull；不落库不落日志，D8）。
	RegistryUsername string
	RegistryToken    string
	// ExpectedDigest 非空 = 幂等补拉/复检：本地平台镜像已持有该 digest 时
	// 零 pull 直接确认；解析结果与该值不一致 InvalidArgument（防 tag 漂移）。
	ExpectedDigest string
	// FunctionTimeoutSeconds 是旧池 drain 宽限上限（由 handleImportImage 在
	// 导入成功后消费，与 handleBuild 的 drain 同语义；ImportImage 实现自身
	// 不消费——保留在 opts 供 fake 断言与 drain 联动收敛在单一载荷）。
	FunctionTimeoutSeconds int64
	// Env 是契约验证 spawn 携带的函数 variables（仅验证 spawn 消费）。
	Env map[string]string
	// EgressUntrusted：untrusted 函数的验证实例挂 internal 变体网络（A1）。
	EgressUntrusted bool
}

// imageClient 收窄镜像导入/推送/补拉依赖的 docker 镜像操作面（真实实现 =
// *client.Client；单测注入 fake 驱动 pull→inspect→tag→remove 与
// push→pull 序列的确定性验证，不依赖真实 daemon——与 netCli 同款收窄注入
// 模式）。
type imageClient interface {
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	ImagePush(ctx context.Context, ref string, options image.PushOptions) (io.ReadCloser, error)
	ImageInspect(ctx context.Context, imageID string, inspectOpts ...client.ImageInspectOption) (image.InspectResponse, error)
	ImageTag(ctx context.Context, source, target string) error
	ImageRemove(ctx context.Context, imageID string, options image.RemoveOptions) ([]image.DeleteResponse, error)
}

// BuildImageOptions 是 BuildImage 的入参（设计 §0 定稿形态）：构建链载荷
// 全量（zip + 上下文字段），由 handleBuild 从 BuildRequest 组装。
type BuildImageOptions struct {
	ProjectID    string
	FunctionID   string
	DeploymentID string
	// Zip 是 zip 字节（base64 解码后；dispatcher 内网 API 的传输形态）。
	Zip []byte
	// Runtime 是 fn.runtime 原值：与 zip 探测结果对账（D7），不一致
	// InvalidArgument；空 = 跳过对账（兼容历史调用方）。
	Runtime string
	// FunctionTimeoutSeconds 是旧池 drain 宽限上限（由 handleBuild 在构建
	// 成功后消费，BuildImage 实现自身不消费——保留在 opts 供 fake 断言与
	// 未来 verify/drain 联动收敛在单一载荷）。
	FunctionTimeoutSeconds int64
	// Env 是验证 spawn 携带的函数 variables（Verify=true 时阶段 3 消费）。
	Env map[string]string
	// EgressUntrusted：untrusted 函数的验证实例挂 internal 变体网络（A1）。
	EgressUntrusted bool
	// Verify：构建成功后 spawn 池外验证实例（D10；阶段 3 实现）。
	Verify bool
}

// SpawnOptions 是 SpawnInstance 的入参。
type SpawnOptions struct {
	ProjectID string
	FunctionID string
	Image     string
	Network   string
	// Env 为容器环境变量（用户 variables + runner 控制变量；不含
	// TW_DATA/TW_EXECUTION_TOKEN——v2 语义下二者经请求体/分发 header 传递）。
	Env         []string
	Spec        string // 资源规格（shared-1x / shared-2x）
	MaxRequests int    // 写入 TW_MAX_REQUESTS（runner 自回收阈值）
	// Name 为容器可读名（tw-fn-<project>-<function>-<rand>，四期运维可读
	// 性；空 = Docker 随机名兜底）。同函数多实例靠随机后缀唯一。
	Name string
}

// containerName 生成可读容器名：tw-fn-<project>-<function>-<rand6>。
// 与镜像名 func-<fid>-<did> 同族的可读性——docker ps 一眼对上项目/函数
// （四期运维诉求：随机名 stoic_engelbart 无法定位函数）。project/function
// 上游已过 ID 白名单（project ^[a-z][a-z0-9]{0,27}$、function
// ^[a-z0-9][a-z0-9_-]{0,63}$），此处防御性小写化 + 非法字符折叠为 '-'；
// 随机后缀（6 hex）保证同函数多实例/反复部署在全 daemon 唯一（总长
// ≤106 < Docker 名上限）。历史遗留大写 functionID 同步小写（G6-3 同口径）。
func containerName(projectID, functionID string) string {
	sanitize := func(s string) string {
		s = strings.ToLower(s)
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
				return r
			default:
				return '-'
			}
		}, s)
	}
	suffix := make([]byte, 3)
	_, _ = cryptorand.Read(suffix)
	return fmt.Sprintf("tw-fn-%s-%s-%s", sanitize(projectID), sanitize(functionID), hex.EncodeToString(suffix))
}

// dockerCleanupTimeout 是清理类操作（stop/remove）的独立超时：不继承已
// 取消的执行 ctx，也不允许 daemon 挂起时无限阻塞（与 v1 同约定）。
const dockerCleanupTimeout = 30 * time.Second

// 验证 spawn 的 health 探针节拍（对齐池启动握手 spawnInstance：单次探针
// 2s 超时、轮询间隔 100ms；预算本身 = 本进程 boot_timeout，构造时解析）。
const (
	verifyProbeTimeout = 2 * time.Second
	verifyPollInterval = 100 * time.Millisecond
)

// networkClient 收窄 EnsureProjectNetwork 依赖的 docker 网络操作面（真实
// 实现 = *client.Client；单测注入 fake 驱动 attach 幂等/自愈路径的确定性
// 验证，不依赖真实 daemon）。
type networkClient interface {
	NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
}

// dockerDaemon 是 Daemon 的真实实现（进程内唯一 docker.sock 持有方）。
type dockerDaemon struct {
	cfg *config.AppConfig
	cli *client.Client
	// netCli 是 cli 的网络操作收窄视图（生产与 cli 同一对象）；独立字段
	// 仅为单测可注入。
	netCli networkClient
	// imgCli 是 cli 的镜像操作收窄视图（生产与 cli 同一对象；独立字段仅为
	// 单测可注入——与 netCli 同款）。
	imgCli imageClient
	// selfContainerID 非空 = dispatcher 自身运行在容器内（自 attach 需要）。
	selfContainerID string
	// ——部署后验证 spawn 参数（D10；从 config 一次性解析，与池共享语义）——
	// bootTimeout 是验证实例 health 探针预算（= 池 boot_timeout，同一 config
	// 键同一解析规则；语义上嵌套在 build_timeout 预算内，设计 §1）。
	bootTimeout time.Duration
	// maxRequestsDefault 是验证实例 TW_MAX_REQUESTS 注入值（对齐池缺省 1000）。
	maxRequestsDefault int
}

// NewDockerDaemon 构造真实 daemon 实现。docker client 构造失败延迟到首次
// 调用暴露（与 dockerExecutor 同策略）。
func NewDockerDaemon(cfg *config.AppConfig) Daemon {
	// 验证 spawn 参数与池同源解析（本进程 boot_timeout / MaxRequests 缺省）；
	// daemon 与池各自消费同一份 config 值，不引入构造参数（最小侵入）。
	pc := PoolConfigFromConfig(cfg)
	d := &dockerDaemon{
		cfg:                cfg,
		bootTimeout:        pc.BootTimeout,
		maxRequestsDefault: pc.MaxRequestsDefault,
	}
	host := cfg.GetFunctions().GetDocker().GetHost()
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		d.cli = nil
		return d
	}
	d.cli = cli
	d.netCli = cli
	d.imgCli = cli
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

// attach 冲突的两类报错文本判据：前者是容器已在网（幂等吞掉）；后者是网络
// 里存在同名 endpoint 残留——endpoint 默认以容器名命名，守护进程重启/容器
// 非优雅重建后，dispatcher 自建的 tw-func-* 网络（不受 compose 管理，跨重建
// 存活）会残留上一代同名容器的 endpoint 记录，attach 从此确定性失败（2026-09-14
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

// EnsureProjectNetwork 解析/创建项目网络并自 attach。常规网络命名与 v1
// dockerExecutor 同一约定（tw-func-<project>；显式 functions.docker.network
// 覆盖为全局共享网络）；untrusted=true 时为 internal 变体
// tw-func-<project>-int（internal: true，出网全 deny——P2 egress 默认 deny，
// 分类在 app 层完成，daemon 只按标志选网）。
func (d *dockerDaemon) EnsureProjectNetwork(ctx context.Context, projectID string, untrusted bool) (string, error) {
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
	// 回访容器 attach（P2 部署前提）：函数经容器名 DNS 回访平台 API
	// （functions.execution.api_base_url 指向该名字）。untrusted 函数在
	// internal 网络（无 NAT 出口），这是其回访平台的唯一通路。attach 失败
	// 不阻断执行——函数可能无需回访平台，但每次都记警告（部署应在首次
	// 执行前修正 callback_container 配置）。幂等与自愈同 ensureConnected。
	if cbName := d.cfg.GetFunctions().GetDispatcher().GetCallbackContainer(); cbName != "" {
		if err := ensureConnected(ctx, cli, name, cbName); err != nil {
			slog.Warn("dispatcher: attach callback container to function network failed; platform callbacks from functions may be unreachable",
				"network", name, "container", cbName, "error", err)
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
	created, err := cli.ContainerCreate(ctx, cfg, hostCfg, &network.NetworkingConfig{}, nil, opts.Name)
	if err != nil {
		return Instance{}, fmt.Errorf("create resident instance %s: %w", opts.Name, err)
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

// BuildImage 以 runner 模板构建镜像：构建上下文准备（zip 解压校验 → runtime
// 对账 → Go bootstrap 生成 / node runner 写入 → Dockerfile 渲染，见
// prepareBuildContext）→ tar build context → docker build。构建期不执行
// 用户代码。
//
// 四期 4b（M1 顺序裁决，设计 §4「构建 → 验证 spawn → push」）：构建成功
// 后先验证 spawn（配置开启时）再 push——验证失败直接以失败收场，不污染
// registry；push 门控与失败语义见 pushBuiltImage。
func (d *dockerDaemon) BuildImage(ctx context.Context, opts BuildImageOptions) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	buildDir, err := os.MkdirTemp("", "torchwood-dispatch-build-*")
	if err != nil {
		return fmt.Errorf("create build dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(buildDir) }()

	if err := prepareBuildContext(buildDir, opts); err != nil {
		return err
	}

	tarCtx, err := tarDir(buildDir)
	if err != nil {
		return fmt.Errorf("tar build context: %w", err)
	}
	ref := infrafunctions.ImageName(d.cfg, opts.FunctionID, opts.DeploymentID)
	buildOpts := build.ImageBuildOptions{
		Tags:       []string{ref},
		Dockerfile: "Dockerfile",
		Remove:     true,
	}
	resp, err := cli.ImageBuild(ctx, tarCtx, buildOpts)
	if err != nil {
		return fmt.Errorf("docker build failed: %s", infrafunctions.TruncateBuildLog(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	// BuildKit 失败在流内 error JSON，复用 v1 读取/裁剪逻辑。
	_, buildErr := infrafunctions.ReadBuildOutput(resp.Body)
	if buildErr != nil {
		return buildErr
	}
	// 部署后验证 spawn（D10，设计 §1）：opts.Verify=false（config
	// verify_build 显式关闭）跳过整段。
	if opts.Verify {
		if err := d.verifyBuild(ctx, opts); err != nil {
			return err
		}
	}
	// M1 镜像全局化（四期 4b）：registry 模式且 registry_push=true 时把
	// 构建产物 push 到 functions.docker.registry；local 模式（及未开 push
	// 的 registry 模式——启动期校验已拒绝该组合，此处兜底）到此为止。
	if !dispatcherPushEnabled(d.cfg) {
		return nil
	}
	imgs, err := d.images()
	if err != nil {
		return err
	}
	return pushBuiltImage(ctx, imgs, ref)
}

// dispatcherPushEnabled 报告构建产物是否应 push 到 registry（M1 门控）：
// routing_mode="registry" 且 registry_push=true。local 模式忽略
// registry_push（镜像不分发，配 true 也不 push）；启动期
// ValidateFunctionsRoutingConfig 已保证 registry 模式必开 push，此处按
// 字面语义独立判定，不依赖调用方先过校验。
func dispatcherPushEnabled(cfg *config.AppConfig) bool {
	d := cfg.GetFunctions().GetDispatcher()
	return config.NormalizedFunctionsRoutingMode(d.GetRoutingMode()) == config.FunctionsRoutingModeRegistry && d.GetRegistryPush()
}

// pushBuiltImage 把构建产物镜像 push 到 registry（M1）：push 失败 = 构建
// 失败（deployment failed，错误含 push 摘要）——registry 模式下镜像全局化
// 是跨节点冷启动的硬前提，静默吞掉 push 失败会把故障推迟到首次跨节点
// spawn（更难归因），故在构建链上立即失败。RegistryAuth 留空 = 用 daemon
// 侧已登录凭证（docker login 的主机级凭证即 push 凭证，不新增凭证配置面）；
// 无整体超时，由调用方 ctx（构建链 build_timeout 预算）封顶。push 的
// 失败详情（认证失败/manifest 错误）与 BuildKit error 流同形，复用同一
// 读取/裁剪逻辑。
func pushBuiltImage(ctx context.Context, imgs imageClient, ref string) error {
	pr, err := imgs.ImagePush(ctx, ref, image.PushOptions{})
	if err != nil {
		return fmt.Errorf("docker push %q failed: %s", ref, infrafunctions.TruncateBuildLog(err.Error()))
	}
	_, streamErr := infrafunctions.ReadBuildOutput(pr)
	_ = pr.Close()
	if streamErr != nil {
		return fmt.Errorf("docker push %q failed: %s", ref, infrafunctions.TruncateBuildLog(streamErr.Error()))
	}
	return nil
}

// EnsureImage 确保部署镜像在本节点可用（M1 registry 模式冷启动前置钩子，
// 编排见 ensureImage）。
func (d *dockerDaemon) EnsureImage(ctx context.Context, imageRef string) error {
	imgs, err := d.images()
	if err != nil {
		return err
	}
	return ensureImage(ctx, imgs, imageRef)
}

// ensureImage 是 EnsureImage 的编排实现：本地命中（ImageInspect 成功）零
// 网络直接返回；miss（NotFound）→ ImagePull（RegistryAuth 留空 = daemon
// 侧已登录凭证）；pull 失败（传输错误或流内 JSON error——registry 认证
// 失败/引用不存在在此冒出）以明确错误收场（含 pull 摘要），由池记为
// spawn 失败现场。inspect 的非 NotFound 错误原样上抛（daemon 不可达时
// 后续 pull 也必败，fail-fast）。幂等：并发 pull 由池的 spawn 锁收敛。
func ensureImage(ctx context.Context, imgs imageClient, imageRef string) error {
	if _, err := imgs.ImageInspect(ctx, imageRef); err == nil {
		return nil // 本地命中：直接 spawn
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect image %q: %w", imageRef, err)
	}
	pr, err := imgs.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("docker pull %q failed: %s", imageRef, infrafunctions.TruncateBuildLog(err.Error()))
	}
	_, streamErr := infrafunctions.ReadBuildOutput(pr)
	_ = pr.Close()
	if streamErr != nil {
		return fmt.Errorf("docker pull %q failed: %s", imageRef, infrafunctions.TruncateBuildLog(streamErr.Error()))
	}
	return nil
}

// verifyBuild 是 dockerDaemon 的验证 spawn 入口：探针复用池的 HTTP runner
// 客户端（同一 /_tw/health 契约面），预算与 TW_MAX_REQUESTS 注入值取构造时
// 解析的池参数（本进程 boot_timeout，设计 §1）。
func (d *dockerDaemon) verifyBuild(ctx context.Context, opts BuildImageOptions) error {
	return spawnVerifyInstance(ctx, d, newHTTPRunner(), opts, verifySpawnConfig{
		Image:       infrafunctions.ImageName(d.cfg, opts.FunctionID, opts.DeploymentID),
		BootTimeout: d.bootTimeout,
		MaxRequests: d.maxRequestsDefault,
	})
}

// images 返回镜像操作收窄视图（与 client() 同约定：client 构造失败时
// Internal 暴露）。
func (d *dockerDaemon) images() (imageClient, error) {
	if d.imgCli == nil {
		return nil, status.Error(codes.Internal, "docker client unavailable (dispatcher requires docker.sock)")
	}
	return d.imgCli, nil
}

// ImportImage 拉取外部镜像导入为平台镜像（三期阶段三，设计 §3）：编排见
// importImage。pull 超时 = 调用方 ctx：首次导入随请求 ctx（与 git pack 同
// 形），worker 补构建 / ready 门禁复检经 buildDeployment 的 build_timeout
// 预算（WithoutCancel 解耦，天然覆盖 image 分流）。
func (d *dockerDaemon) ImportImage(ctx context.Context, opts ImportImageOptions) (string, error) {
	imgs, err := d.images()
	if err != nil {
		return "", err
	}
	return importImage(ctx, d, imgs, newHTTPRunner(), d.cfg, opts, d.bootTimeout, d.maxRequestsDefault)
}

// importImage 是 ImportImage 的编排实现（镜像源免构建路径，设计 §3）：
//
//	host 名称级校验 → 取得钉死内容（ExpectedDigest 本地命中零 pull；引用本地
//	已存在 = 本地构建/本地 tag 直导场景直接用本地内容；否则 pull，RegistryAuth
//	内联单次转发）→ digest 钉死（引用带 @sha256 时校验一致，防 tag 漂移）→
//	retag 进平台命名（「镜像名 = 平台命名」不变式，池 spawn/RemoveImage 零
//	改动）→ 删原始引用标签（防本地 daemon 残留；reference 即 target 时跳过）
//	→ 强制契约验证 spawn（D10 镜像源同款：health 探针/池外实例/带
//	variables/egress 选网/失败日志尾回收；config verify_build 关不掉——设计
//	§3「强制」）→ 返回钉死 digest。
//
// 镜像原语收窄在 imageClient、探针收窄在 healthProber 上（真实 = docker
// client / httpRunner；单测 = fake 表驱动调用序列与失败路径），验证 spawn
// 复用一期 spawnVerifyInstance。
func importImage(ctx context.Context, d Daemon, imgs imageClient, probe healthProber, cfg *config.AppConfig, opts ImportImageOptions, bootTimeout time.Duration, maxRequests int) (string, error) {
	// 1) registry host 名称级校验（设计 §3 安全基线的可实现口径：名称级
	//    校验 + 白名单 + allow_insecure 显式开关；失败 InvalidArgument 明示
	//    命中规则）。先于一切 docker 操作——本地直导路径同样受准入口径约束。
	if err := validateImageRegistryHost(parseImageReferenceHost(opts.Reference),
		cfg.GetFunctions().GetImage().GetAllowedRegistries(),
		cfg.GetFunctions().GetImage().GetAllowInsecure()); err != nil {
		return "", err
	}

	target := infrafunctions.ImageName(cfg, opts.FunctionID, opts.DeploymentID)

	// 2) 幂等（设计 §3「本地命中则零 pull」）：ExpectedDigest 非空且本地
	//    平台镜像可判定持有时零 pull/retag 直接契约验证并确认（worker 补拉
	//    / ready 门禁复检）。非 NotFound 的 inspect 错误原样上抛（daemon
	//    不可达时后续操作也必败，fail-fast）。
	if opts.ExpectedDigest != "" {
		ins, err := imgs.ImageInspect(ctx, target)
		switch {
		case err == nil && localImageMatches(ins, opts.ExpectedDigest):
			if err := verifyImported(ctx, d, probe, opts, target, bootTimeout, maxRequests); err != nil {
				return "", err
			}
			return opts.ExpectedDigest, nil
		case err != nil && !errdefs.IsNotFound(err):
			return "", fmt.Errorf("inspect platform image %q: %w", target, err)
		}
	}

	// 3) 取得钉死内容：引用本地已存在（本地构建/本地 tag 直导场景，任务
	//    口径「本地构建/本地 tag 场景 reference 就是本地 tag」）直接用本地
	//    内容——跳过 pull，不触网（内容陈旧风险由部署者承担，digest 钉死值
	//    在 source_url/source_ref 上诚实可见）；未命中（NotFound）才 pull
	//    （凭证空 = 匿名；RegistryAuth 是 base64 JSON 单次转发 daemon，不落
	//    库不落日志）。pull 失败流内 JSON error（与 BuildKit error 流同形，
	//    复用同一解析——registry 认证失败/引用不存在在此冒出）。
	refIns, err := imgs.ImageInspect(ctx, opts.Reference)
	switch {
	case errdefs.IsNotFound(err):
		pullResp, pullErr := imgs.ImagePull(ctx, opts.Reference, image.PullOptions{RegistryAuth: registryAuth(opts.RegistryUsername, opts.RegistryToken)})
		if pullErr != nil {
			return "", fmt.Errorf("docker pull %q: %w", opts.Reference, pullErr)
		}
		if _, streamErr := infrafunctions.ReadBuildOutput(pullResp); streamErr != nil {
			_ = pullResp.Close()
			return "", status.Errorf(codes.InvalidArgument, "docker pull %q failed: %s",
				opts.Reference, infrafunctions.TruncateBuildLog(streamErr.Error()))
		}
		_ = pullResp.Close()
		refIns, err = imgs.ImageInspect(ctx, opts.Reference)
		if err != nil {
			return "", fmt.Errorf("inspect %q after pull: %w", opts.Reference, err)
		}
	case err != nil:
		return "", fmt.Errorf("inspect %q: %w", opts.Reference, err)
	}

	// 4) digest 钉死：提取 RepoDigest 的 @sha256 部分；引用自带 @sha256
	//    校验一致（tag 漂移 → InvalidArgument）；调用方预期 digest 不一致
	//    同样拒绝。
	digest, err := pinnedDigest(opts.Reference, refIns)
	if err != nil {
		return "", err
	}
	if opts.ExpectedDigest != "" && digest != opts.ExpectedDigest {
		return "", status.Errorf(codes.InvalidArgument,
			"pulled image digest %q does not match expected digest %q (tag drift; redeploy pinning the exact digest)",
			digest, opts.ExpectedDigest)
	}

	// 5) retag 进平台命名并删除原始引用标签（设计 §0：retag 后全链路零
	//    改动且删除语义干净）。原始引用删除防本地 daemon 残留；本地构建/
	//    本地 tag 场景 reference 可能就是平台镜像名自身（相同则跳过）；
	//    同镜像多标签下 Force=false 只摘标签不删数据，NotFound 容忍（引用
	//    已不存在 = 已无残留）。
	if err := imgs.ImageTag(ctx, opts.Reference, target); err != nil {
		return "", fmt.Errorf("tag %q -> %q: %w", opts.Reference, target, err)
	}
	if opts.Reference != target {
		if _, err := imgs.ImageRemove(ctx, opts.Reference, image.RemoveOptions{Force: false}); err != nil && !errdefs.IsNotFound(err) {
			return "", fmt.Errorf("remove original reference %q: %w", opts.Reference, err)
		}
	}

	// 6) 强制契约验证 spawn + 返回钉死 digest。
	if err := verifyImported(ctx, d, probe, opts, target, bootTimeout, maxRequests); err != nil {
		return "", err
	}
	return digest, nil
}

// verifyImported 对导入后的平台镜像执行强制契约验证 spawn（与 build 验证
// 同一机制：池外实例、带函数 variables、egress 选网、失败日志尾回收）。
func verifyImported(ctx context.Context, d Daemon, probe healthProber, opts ImportImageOptions, image string, bootTimeout time.Duration, maxRequests int) error {
	return spawnVerifyInstance(ctx, d, probe, BuildImageOptions{
		ProjectID:       opts.ProjectID,
		FunctionID:      opts.FunctionID,
		DeploymentID:    opts.DeploymentID,
		Env:             opts.Env,
		EgressUntrusted: opts.EgressUntrusted,
	}, verifySpawnConfig{
		Image:       image,
		BootTimeout: bootTimeout,
		MaxRequests: maxRequests,
	})
}

// registryAuth 构造 docker RegistryAuth（base64 JSON 形态
// {"username","password","serveraddress":""}；凭证空 = 匿名 pull，返回空串
// ——空 RegistryAuth 时 daemon 走匿名）。一次性凭证只在内存存在（D8）。
func registryAuth(username, token string) string {
	if username == "" && token == "" {
		return ""
	}
	raw, err := json.Marshal(map[string]string{
		"username":      username,
		"password":      token,
		"serveraddress": "",
	})
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// localImageMatches 判定本地平台镜像是否命中预期 digest（幂等零 pull 的
// 判定核心）：
//   - RepoDigests 任一条目命中，或 Image ID 直接过命中 → 命中（严格可证；
//     ID 命中覆盖本地构建/本地 tag 场景——未经 registry 的镜像无
//     RepoDigest，Image ID（config digest）是唯一内容寻址钉死值）；
//   - RepoDigests 非空且全不命中 → 不命中（可证的本地内容漂移 → 走重拉
//     对账，比对后拒绝）；
//   - RepoDigests 为空且 ID 不命中 → 命中：这是首次导入自身的稳态——原始
//     引用已删除，registry manifest digest 关联随原始引用摘除，而 Image ID
//     ≠ manifest digest，本地内容无法严格证伪。平台命名 tag 属平台专属
//     命名空间（外部改动不在威胁模型），其内容在首次导入时已钉死 digest
//     并通过契约验证——target 存在即视为幂等命中（「本地命中零 pull」承诺
//     的可实现形态；M7 补拉语义只在镜像真正缺失时触发）。
func localImageMatches(ins image.InspectResponse, digest string) bool {
	for _, rd := range ins.RepoDigests {
		if digestFromRepoDigest(rd) == digest {
			return true
		}
	}
	if ins.ID == digest {
		return true
	}
	if len(ins.RepoDigests) > 0 {
		return false // 可证漂移：本地内容确定性不匹配 → 走重拉对账
	}
	// 不可证伪态（首次导入稳态）：target 存在即视为幂等命中；退化 inspect
	//（无 ID 无 RepoDigests）不视为命中，落回 pull 路径。
	return ins.ID != ""
}

// pinnedDigest 从 pull 后的 inspect 结果提取钉死 digest：优先在 RepoDigests
// 中命中引用自带 @sha256 的条目（多仓库镜像的 RepoDigests 顺序不稳定，防
// 误判）；无自带 digest 时取 RepoDigests[0]；RepoDigests 为空（本地构建/
// docker load 的镜像不经 registry）回落镜像 ID——内容寻址钉死值仍成立，
// registry 摘要缺失对调用方诚实可见。
func pinnedDigest(reference string, ins image.InspectResponse) (string, error) {
	refDigest := referenceDigest(reference)
	if refDigest != "" {
		for _, rd := range ins.RepoDigests {
			if digestFromRepoDigest(rd) == refDigest {
				return refDigest, nil
			}
		}
		if ins.ID == refDigest {
			return refDigest, nil
		}
		return "", status.Errorf(codes.InvalidArgument,
			"image reference digest %q does not match any pulled repo digests %v (tag drift)",
			refDigest, ins.RepoDigests)
	}
	if len(ins.RepoDigests) > 0 {
		return digestFromRepoDigest(ins.RepoDigests[0]), nil
	}
	if ins.ID != "" {
		return ins.ID, nil
	}
	return "", status.Errorf(codes.Internal, "image %q inspect returned no digest to pin", reference)
}

// referenceDigest 提取引用中的 digest 部分（"repo@sha256:..." →
// "sha256:..."；无 digest 引用返回空串）。
func referenceDigest(reference string) string {
	if i := strings.Index(reference, "@"); i >= 0 {
		return reference[i+1:]
	}
	return ""
}

// digestFromRepoDigest 提取 RepoDigest 条目的 digest 部分
// （"alpine@sha256:..." → "sha256:..."）。
func digestFromRepoDigest(repoDigest string) string {
	if i := strings.Index(repoDigest, "@"); i >= 0 {
		return repoDigest[i+1:]
	}
	return repoDigest
}

// verifySpawnConfig 是验证 spawn 的参数包：镜像名 + 探针预算 + 注入值。
// PollInterval 零值取 verifyPollInterval（单测注入加速/确定性）。
type verifySpawnConfig struct {
	Image        string
	BootTimeout  time.Duration
	MaxRequests  int
	PollInterval time.Duration
}

// healthProber 是验证探针的最小抽象（runnerClient 的 Health 面收窄；生产 =
// httpRunner，单测 = fake 表驱动）。
type healthProber interface {
	Health(ctx context.Context, ip string) error
}

// spawnVerifyInstance 执行一次部署后验证 spawn（设计 §1/D10）：build 成功后
// spawn 一枚验证实例 → /_tw/health 轮询（预算 = BootTimeout）→ 就绪即回收。
//
// 池外语义（设计 §1）：走 daemon 原语直连——不进 Redis 注册表、不受
// MaxResidentInstances 约束、不参与 reaper 对账（BuildImage 无池依赖，天然
// 满足），用完即删；验证范围 = health 探针，不做 invoke——invoke 需要平台
// 构造 TW_DATA 并执行用户代码（副作用不可控），违反「部署期不执行用户代码」
// 不变量（对抗审查 A5 裁决，残余风险由运行期 transport-error 杀实例兜底）。
//
// 失败处置：回收容器日志尾部（64KB 上限）拼进错误——运行期错误（panic/协议
// 未实现）在容器 stdout，第一现场必须带回 deployment.error；编译错误在构建
// 日志、不经此路径。无论成败容器以独立 cleanup ctx Stop(SIGKILL)+Remove
// （不继承已取消/临期的构建 ctx，dockerCleanupTimeout 同约定）。
func spawnVerifyInstance(ctx context.Context, d Daemon, probe healthProber, opts BuildImageOptions, vc verifySpawnConfig) error {
	// egress 分类与执行一致（对抗审查 A1 最强修复）：untrusted 函数的验证
	// 实例挂 internal 变体网络（与池 spawnInstance 同路）——验证期不得给
	// 不可信镜像开跳出网窗口。
	network, err := d.EnsureProjectNetwork(ctx, opts.ProjectID, opts.EgressUntrusted)
	if err != nil {
		return fmt.Errorf("verify spawn: ensure network: %w", err)
	}
	// env 组装与池 spawnInstance 同源（「携带函数当前 variables，与执行时
	// 同源组装」）：TW_DATA/TW_EXECUTION_TOKEN 不进容器 env（常驻的是容器
	// 不是凭证）；TW_MAX_REQUESTS/TW_DRAIN_TIMEOUT_MS 由 SpawnInstance 统一
	// 追加，此处不重复注入。
	env := make([]string, 0, len(opts.Env))
	for k, v := range opts.Env {
		if k == "TW_DATA" || k == "TW_EXECUTION_TOKEN" {
			continue
		}
		env = append(env, k+"="+v)
	}
	inst, err := d.SpawnInstance(ctx, SpawnOptions{
		ProjectID:   opts.ProjectID,
		FunctionID:  opts.FunctionID,
		Image:       vc.Image,
		Network:     network,
		Env:         env,
		Spec:        "shared-1x",
		MaxRequests: vc.MaxRequests,
		Name:        containerName(opts.ProjectID, opts.FunctionID),
	})
	if err != nil {
		return fmt.Errorf("verify spawn: %w", err)
	}
	// 无论成败回收容器（SIGKILL：验证实例无在途请求，无需 drain 宽限）。
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerCleanupTimeout)
		defer cancel()
		_ = d.StopInstance(cctx, inst.ContainerID, 0)
		_ = d.RemoveInstance(cctx, inst.ContainerID)
	}()

	interval := vc.PollInterval
	if interval <= 0 {
		interval = verifyPollInterval
	}
	deadline := time.Now().Add(vc.BootTimeout)
	for {
		pctx, pcancel := context.WithTimeout(ctx, verifyProbeTimeout)
		err := probe.Health(pctx, inst.IP)
		pcancel()
		if err == nil {
			return nil // 验证通过：静默（成功不产生 deployment.error）
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			// 失败处置：日志读取与后续清理同用脱离构建 ctx 的独立 ctx
			//（预算到点时构建 ctx 可能已取消/临期）。
			tailCtx, tailCancel := context.WithTimeout(context.WithoutCancel(ctx), dockerCleanupTimeout)
			tail, tailErr := d.InstanceLogsTail(tailCtx, inst.ContainerID, maxLogTailBytes)
			tailCancel()
			if tailErr != nil {
				tail = fmt.Sprintf("<container logs unavailable: %v>", tailErr)
			}
			return fmt.Errorf("verification failed: function image did not become healthy within %s (last probe error: %v); container log tail:\n%s",
				vc.BootTimeout, err, tail)
		}
		if !defaultSleep(ctx, interval) {
			// sleep 期间 ctx 取消：下一轮探针立即失败并走上面的日志回收路径。
			continue
		}
	}
}

// InstanceLogsTail 返回容器日志尾部（stdout/stderr 解复用合并，取末尾 limit
// 字节）：验证 spawn 失败的第一现场诊断面。spawn 的容器无 TTY——daemon 返回
// 多路复用流，stdcopy 解复用（与集成测试 runOneShot 同款）。
func (d *dockerDaemon) InstanceLogsTail(ctx context.Context, containerID string, limit int64) (string, error) {
	cli, err := d.client()
	if err != nil {
		return "", err
	}
	logs, err := cli.ContainerLogs(ctx, containerID, container.LogsOptions{
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

// prepareBuildContext 在 buildDir 准备镜像构建上下文（无 docker 依赖，纯
// 文件编排，单测以临时 zip 直接驱动）。顺序敏感（Go 一期接线，设计 §1）：
//
//  1. 解压 zip 并探测部署源（ExtractZip；twmain/ 保留目录冲突在此探测为
//     TwmainConflict 标记——基于 zip 条目清单而非落盘目录）；
//  2. runtime 一致性对账（D7）：opts.Runtime 非空且 ≠ 探测结果 →
//     InvalidArgument（错误信息含两侧值）；
//  3. go 分支：gorunner.DetectEntry 扫根包选入口（此时尚无 twmain/，探测
//     时点正确）→ RenderBootstrap 渲染 → 写 <buildDir>/twmain/{runtime,main}.go
//     ——写入必须在 DockerfileFor 渲染之前（模板 COPY twmain/）；
//  4. node 分支照旧写 .tw-runner.js；最后渲染 Dockerfile（缺 go.sum /
//     twmain 冲突等拒收错误在此冒出）。
func prepareBuildContext(buildDir string, opts BuildImageOptions) error {
	tmpZip, err := os.CreateTemp("", "torchwood-dispatch-src-*.zip")
	if err != nil {
		return fmt.Errorf("stage zip: %w", err)
	}
	defer func() { _ = os.Remove(tmpZip.Name()) }()
	if _, err := tmpZip.Write(opts.Zip); err != nil {
		_ = tmpZip.Close()
		return fmt.Errorf("stage zip: %w", err)
	}
	_ = tmpZip.Close()

	// 解压 + 探测：复用 v1 防炸弹/路径穿越预算，条目预算用放宽版（二期
	// 阶段 3，设计 §2 条目维链条）——BuildImage 无法区分 zip/git 源（同
	// base64 内联通道），统一放宽到 packer 物化口径（条目 5000；单条
	// 100MiB / 总量 200MiB 解压预算维持），防 git 源合法 zip 被默认 1000
	// 条目预算击毙。SourceContents 附带部署源探测结果，逐字段映射为
	// runner 包的模板载体（runner 保持叶子资产包，不 import infra/functions
	// 根包）。
	contents, err := infrafunctions.ExtractZipRelaxed(tmpZip.Name(), buildDir)
	if err != nil {
		return err
	}

	// runtime 一致性对账（D7）：探测结果必须与 fn.runtime 一致，不一致在
	// 构建期报 InvalidArgument（收严无存量负担）；opts.Runtime 为空跳过
	// （兼容未携带 runtime 的历史调用方）。
	if opts.Runtime != "" && opts.Runtime != contents.Runtime {
		return status.Errorf(codes.InvalidArgument,
			"runtime mismatch: function declares %q but source probes as %q (redeploy with the matching runtime)",
			opts.Runtime, contents.Runtime)
	}

	// go 分支：平台生成 twmain/ bootstrap（Detect → Render → 落盘），写入
	// 时点在 DockerfileFor 之前（模板 COPY twmain/）、在 DetectEntry 之后
	// （根包扫描不受影响——twmain 是子目录，且 TwmainConflict 基于 zip 条目
	// 清单）。权限 0644 对齐既有 runner 写入约定（镜像内 USER 须可读，
	// tarDir 收口点再归一化）。
	if contents.Runtime == "go-1.26" {
		if err := writeGoBootstrap(buildDir, contents.GoModulePath); err != nil {
			return err
		}
	}

	dockerfile, err := runner.DockerfileFor(runner.SourceContents{
		Runtime:        contents.Runtime,
		NodeDeps:       contents.NodeDeps,
		HasLockfile:    contents.HasLockfile,
		GoModulePath:   contents.GoModulePath,
		GoHasRequires:  contents.GoHasRequires,
		GoHasSum:       contents.GoHasSum,
		HasVendor:      contents.HasVendor,
		TwmainConflict: contents.TwmainConflict,
	})
	if err != nil {
		return err
	}
	// node 分支：runner 脚本随 COPY . . 进镜像（go 分支写入该文件无害——
	// go 模板不引用它）。构建产物经 COPY 进入镜像后须被镜像内 USER 读取
	// ——权限必须保持 world-readable（G306 误报：非机密，且收紧曾致容器
	// 秒退、健康握手永不 ready，CI e2e 实证）。
	if err := os.WriteFile(filepath.Join(buildDir, runner.RunnerFileName), runner.NodeRunnerJS(), 0o644); err != nil { // #nosec G306 -- 镜像内 USER 须可读
		return fmt.Errorf("write runner: %w", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil { // #nosec G306 -- 镜像内 USER 须可读
		return fmt.Errorf("write dockerfile: %w", err)
	}
	return nil
}

// writeGoBootstrap 在构建上下文生成保留目录 twmain/ 的两份 bootstrap 源码
// （AST 入口探测 → 渲染 → 落盘；Go 一期，设计 §1/D3 生成路线）。
func writeGoBootstrap(buildDir, modulePath string) error {
	entry, err := gorunner.DetectEntry(buildDir)
	if err != nil {
		return err
	}
	runtimeGo, mainGo, err := gorunner.RenderBootstrap(modulePath, entry)
	if err != nil {
		return err
	}
	twDir := filepath.Join(buildDir, "twmain")
	if err := os.MkdirAll(twDir, 0o755); err != nil {
		return fmt.Errorf("create twmain dir: %w", err)
	}
	// 0644 对齐既有 runner 写入注释（镜像内非 root USER 须可读；tarDir 收口
	// 点对落盘 mode 再归一化，防 umask 漂移）。
	if err := os.WriteFile(filepath.Join(twDir, "runtime.go"), runtimeGo, 0o644); err != nil { // #nosec G306 -- 镜像内 USER 须可读
		return fmt.Errorf("write twmain/runtime.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(twDir, "main.go"), mainGo, 0o644); err != nil { // #nosec G306 -- 镜像内 USER 须可读
		return fmt.Errorf("write twmain/main.go: %w", err)
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
		// 权限坏档修复（EACCES 秒退事故）：镜像内文件 mode 不得依赖 dispatcher
		// 进程状态。上游 os.WriteFile/OpenFile 声明的 0644 会先被进程 umask 掩蔽
		// （0644 & ~umask，umask 0077 时落盘 0600），而 FileInfoHeader 忠实保留
		// 磁盘实际 mode，经 COPY . . 原样进镜像——模板 USER node 读 .tw-runner.js
		// 即 EACCES、容器秒退（同构建代码先后产出坏/好镜像 = 进程 umask 随栈
		// redeploy 漂移的状态依赖）。在此单一收口点归一化：文件恒 0644、目录恒
		// 0755（x 位不可省，子目录用户代码要靠它遍历）、属主归零，镜像权限与
		// dispatcher 以何用户/何 umask 运行彻底解耦。不用模板 COPY --chmod：
		// 它对文件与目录只能给同一个 mode，顾此失彼。
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
			f, err := os.Open(path) // #nosec G304 -- path 由 WalkDir 从自有构建目录枚举（非用户输入）
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
