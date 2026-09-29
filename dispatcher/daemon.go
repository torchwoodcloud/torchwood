package dispatcher

import (
	"archive/tar"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/fleetly/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/fleetly/shared/v1"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/functions/runner"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// 本文件是 dispatcher 的执行底座适配层中的 fleetly 实现（IMPL-T2-3 引入；
// IMPL-T2-5 起与 docker 直接执行形态并存，driver 选择见 driver.go，docker
// 实现在隔离子包 dockerdriver）。fleetly 形态：函数实例 = fleetly Tasks
// （swarm service 承载，稳定 DNS 名 + restart-condition none）、构建 =
// fleetly build-from-upload。池语义（租约/保温/熔断/TW_MAX_REQUESTS）在
// pool.go 不动，本层只换执行底座。
//
// 镜像引用寻址（审查裁决，见实施方案 §4 IMPL-T2-3）：
//   - torchwood server 侧的执行规格只携带**逻辑镜像名**
//     （functions.docker.registry/func-<function>-<deployment>，server 与
//     dispatcher 同源派生）；fleetly build-from-upload 的产物引用
//     （registry 模式 <host>/apps/<name>@sha256:<manifest>；本地模式
//     fleetly-local/<name>:<tag>）不可从逻辑名推导（tag 含平台 build id）；
//   - 故 dispatcher 在构建/导入成功时把「逻辑名 → 平台产物引用」写入
//     Redis 映射（torchwood:fnimg:<逻辑名>），spawn 时解析；映射缺失时以
//     原样引用交给平台解析（必然失败）→ FailedPrecondition +
//     ImageMissingMarker 上抛 → server 侧重建链路重新构建并刷新映射
//     （自愈路径复用既有 rebuild 语义）。
type Instance struct {
	// ContainerID 是实例句柄（fleetly 形态 = task id；docker 形态 = 容器 ID；
	// 停止/删除/探活/日志的寻址键）。
	ContainerID string
	// IP 是实例在作用域网络内的寻址目标（池内 HTTP 分发与健康探针；pool.go
	// 只消费其「网络内可达」语义）：fleetly 形态 = 稳定 DNS 名
	// （fleetly-task-<id>）；docker 形态 = bridge 网络容器 IP。
	IP string
}

// Daemon 是池管理器对执行底座的抽象（fake 测试用）。真实实现两形态
// （IMPL-T2-5）：fleetlyDaemon（本文件，fleetly Tasks/build API 客户端，
// 零 docker client）与 dockerdriver 包的 docker 直接执行（本仓唯一
// docker client 面）；driver 选择见 driver.go。
type Daemon interface {
	// EnsureProjectNetwork 确保项目任务网络存在并声明控制面挂靠（fleetly
	// EnsureTaskNetwork：task-group 网长活，ref = p<projectID> / q<projectID>
	// 两变体，后者 internal——不可信函数出网全 deny，DT-7）。untrusted=true
	// 时确保的是 internal 变体。members（控制面服务挂靠）随配置一次性声明，
	// 经 fleetly 发布管线重部署生效；返回网络名（fleetly-taskgroup-<ref>）。
	EnsureProjectNetwork(ctx context.Context, projectID string, untrusted bool) (string, error)
	// SpawnInstance 创建常驻任务实例（fleetly CreateTask：镜像/环境/资源/
	// 作用域；restart-condition none 与加固由平台强制），等待平台收敛到
	// running 后返回句柄（DNS 名为分发寻址）。
	SpawnInstance(ctx context.Context, opts SpawnOptions) (Instance, error)
	// InspectInstance 返回任务是否 running（fleetly GetTask；任务不存在返回
	// 非 nil 错误，池按幽灵记录清理）。
	InspectInstance(ctx context.Context, containerID string) (running bool, ip string, err error)
	// StopInstance 停止任务（fleetly StopTask；幂等）。timeout 语义迁移：
	// swarm 停机宽限由平台固定（stop_grace_period 5s），本参数不再改变
	// 停止行为（仅保留池调用签名）。
	StopInstance(ctx context.Context, containerID string, timeout time.Duration) error
	// RemoveInstance 删除任务（fleetly DeleteTask：停止 + 移除底座服务 +
	// 台账行；幂等，不存在视为成功）。
	RemoveInstance(ctx context.Context, containerID string) error
	// InstanceLogsTail 返回任务失败现场（fleetly GetTask 的 status/error/
	// stop_reason 投影）。诚实边界：任务容器日志归平台 VictoriaLogs 采集，
	// 机具令牌（tasks,build）无日志读面——本方法不回读容器 stdout/stderr，
	// 只回平台台账的失败原因（验证 spawn 失败的第一现场）。
	InstanceLogsTail(ctx context.Context, containerID string, limit int64) (string, error)
	// BuildImage 以 runner 模板构建镜像（zip 字节内联；构建期不执行用户
	// 代码的不变量由模板层保持——node runner 仅被 COPY，go 只编译不执行）。
	// 渲染上下文（.tw-runner.js + Dockerfile → tar）逻辑保留，构建本体经
	// fleetly BuildFromUpload（client-streaming；产物平台侧推 registry /
	// 本机装载，调用方零 push 凭证）；成功后登记逻辑名 → 产物引用映射，
	// 再按需做部署后验证 spawn（与旧链同序：验证失败不污染后续引用）。
	BuildImage(ctx context.Context, opts BuildImageOptions) error
	// ImportImage 导入外部镜像（BYO 镜像源）：host 名称级准入校验保留；
	// 拉取与 digest 钉定归 fleetly 平台（创建验证任务时解析，任务视图
	// image 字段即钉定引用）→ 强制契约验证 spawn → 返回钉死 digest（调用方
	// 落 deployment.source_ref）并登记逻辑名 → 产物引用映射。
	ImportImage(ctx context.Context, opts ImportImageOptions) (string, error)
	// RemoveImage 删除构建产物引用映射（幂等）。平台侧产物 GC 归平台；
	// 本仓只保证后续 spawn 不再解析到该引用（映射缺失 → 重建链路自愈）。
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
	// RegistryUsername/RegistryToken 是一次性 registry 凭证（历史字段：
	// fleetly 平台按平台 registry 设置解析/拉取镜像，不再接受逐次凭证；
	// 保留字段以兼容调用方载荷，dispatcher 侧不再消费——私有镜像请在
	// fleetly 平台侧配置 registry 凭证）。
	RegistryUsername string
	RegistryToken    string
	// ExpectedDigest 非空 = 幂等复检：映射已持有一致 digest 时零平台往返
	// 直接确认；否则创建验证任务重新解析，解析结果与该值不一致
	// InvalidArgument（防 tag 漂移）。
	ExpectedDigest string
	// FunctionTimeoutSeconds 是旧池 drain 宽限上限（由 handleImportImage 在
	// 导入成功后消费，与 handleBuild 的 drain 同语义；ImportImage 实现自身
	// 不消费——保留在 opts 供 fake 断言与 drain 联动收敛在单一载荷）。
	FunctionTimeoutSeconds int64
	// Env 是契约验证 spawn 携带的函数 variables（仅验证 spawn 消费）。
	Env map[string]string
	// EgressUntrusted：untrusted 函数的验证实例挂 internal 变体任务网络（A1）。
	EgressUntrusted bool
}

// BuildImageOptions 是 BuildImage 的入参（构建链载荷全量：zip + 上下文字段），
// 由 handleBuild 从 BuildRequest 组装。
type BuildImageOptions struct {
	ProjectID    string
	FunctionID   string
	DeploymentID string
	// Zip 是 zip 字节（base64 解码后；dispatcher 内网 API 的传输形态）。
	Zip []byte
	// Runtime 是 fn.runtime 原值：与 zip 探测的 family 对账（D7'，探测产出
	// 语言族、版本轴来自声明——functions-runtime-selection.md §2），family
	// 不一致或 ID 未知 → InvalidArgument；空 = 遗留调用方，渲染基准取探测
	// family 的首个 active 表项。
	Runtime string
	// FunctionTimeoutSeconds 是旧池 drain 宽限上限（由 handleBuild 在构建
	// 成功后消费，BuildImage 实现自身不消费——保留在 opts 供 fake 断言与
	// 未来 verify/drain 联动收敛在单一载荷）。
	FunctionTimeoutSeconds int64
	// Env 是验证 spawn 携带的函数 variables（Verify=true 时消费）。
	Env map[string]string
	// EgressUntrusted：untrusted 函数的验证实例挂 internal 变体网络（A1）。
	EgressUntrusted bool
	// Verify：构建成功后 spawn 池外验证实例（D10）。
	Verify bool
}

// SpawnOptions 是 SpawnInstance 的入参。
type SpawnOptions struct {
	ProjectID  string
	FunctionID string
	Image      string
	Network    string
	// Env 为容器环境变量（用户 variables + runner 控制变量；不含
	// TW_DATA/TW_EXECUTION_TOKEN——v2 语义下二者经请求体/分发 header 传递）。
	Env         []string
	Spec        string // 资源规格（shared-1x / shared-2x）
	MaxRequests int    // 写入 TW_MAX_REQUESTS（runner 自回收阈值）
	// Name 为任务可读名（tw-fn-<project>-<function>-<rand>，四期运维可读
	// 性；进 fleetly 任务 name 字段与视图）。
	Name string
}

// fleetlyTaskClient 收窄 fleetly Tasks/build 面（真实实现 = grpcFleetlyClient；
// 单测注入 fake 驱动收敛/配额/映射路径的确定性验证，不依赖真实平台）。
type fleetlyTaskClient interface {
	EnsureTaskNetwork(ctx context.Context, req *serverv1.EnsureTaskNetworkRequest) (*serverv1.EnsureTaskNetworkResponse, error)
	CreateTask(ctx context.Context, req *serverv1.CreateTaskRequest) (*serverv1.CreateTaskResponse, error)
	GetTask(ctx context.Context, req *serverv1.GetTaskRequest) (*serverv1.GetTaskResponse, error)
	StopTask(ctx context.Context, req *serverv1.StopTaskRequest) (*serverv1.StopTaskResponse, error)
	DeleteTask(ctx context.Context, req *serverv1.DeleteTaskRequest) (*serverv1.DeleteTaskResponse, error)
	BuildFromUpload(ctx context.Context, name, dockerfile string, contextTar io.Reader) (*serverv1.BuildFromUploadResponse, error)
}

// imageRefStore 是「逻辑镜像名 → 平台产物引用」映射的持久面（Redis 实现
// 挂 redisRegistry；测试注入内存 fake）。
type imageRefStore interface {
	SaveImageRef(ctx context.Context, logicalName, ref string) error
	LoadImageRef(ctx context.Context, logicalName string) (string, error)
	DeleteImageRef(ctx context.Context, logicalName string) error
}

// taskScope 是一个任务网络的作用域投影（网络名 → task-group ref + internal
// 变体；EnsureProjectNetwork 登记，SpawnInstance 消费）。
type taskScope struct {
	ref      string
	internal bool
}

// fleetlyDaemon 是 Daemon 的真实实现（fleetly Tasks/build API 客户端；
// 进程内零 docker client）。
type fleetlyDaemon struct {
	cfg    *config.AppConfig
	refs   imageRefStore
	cli    fleetlyTaskClient
	cliErr error

	// ——部署后验证 spawn 参数（D10；从 config 一次性解析，与池共享语义）——
	// bootTimeout 是任务平台收敛 + 验证实例 health 探针预算（= 池
	// boot_timeout，同一 config 键同一解析规则）。
	bootTimeout time.Duration
	// maxRequestsDefault 是验证实例 TW_MAX_REQUESTS 注入值（对齐池缺省 1000）。
	maxRequestsDefault int
	// pollInterval 是任务收敛轮询间隔（测试注入加速；缺省 200ms）。
	pollInterval time.Duration
	// sleep 可注入（表驱动测试）；生产用 defaultSleep（pool.go 同款）。
	sleep func(context.Context, time.Duration) bool
	// probe 是验证 spawn 的 health 探针（生产 = httpRunner；测试注入 fake）。
	probe HealthProber

	mu sync.Mutex
	// scopes 是本次进程内已 ensure 的任务网络投影（网络名 → 作用域）。
	scopes map[string]taskScope
	// membersEnsured 记录已随 ensure 声明过挂靠成员的网络（每网一次语义；
	// 进程重启后首轮重声明——平台侧 upsert 幂等，已存在成员零新增）。
	membersEnsured map[string]bool
}

// NewFleetlyDaemon 构造真实执行底座实现（fleetly Tasks/build API 客户端）。
// endpoint 解析序：显式 functions.fleetly.endpoint（scheme 选择传输模式，
// config.ParseFleetlyEndpoint）→ 平台物化的 FLEETLY_CONTROL_GRPC_ADDR
// （ctrlinject：集群内工作负载零配置回拨控制面；FLEETLY_CONTROL_TLS_NAME
// 非空 = TLS + ServerName 校验，空 = TLS off 明文）。两者皆缺 → 延迟到首次
// 调用暴露（与 dockerdriver.New 的 client 段同策略；组合根
// ValidateFunctionsDriverConfig 已做启动期 fail-fast）。
func NewFleetlyDaemon(cfg *config.AppConfig, refs imageRefStore) Daemon {
	var cli fleetlyTaskClient
	var cliErr error
	f := cfg.GetFunctions().GetFleetly()
	switch endpoint := strings.TrimSpace(f.GetEndpoint()); {
	case endpoint != "":
		cli, cliErr = newGRPCFleetlyClient(endpoint, f.GetToken())
	case os.Getenv(EnvControlGRPCAddr) != "":
		mode := config.FleetlyEndpointPlain
		serverName := strings.TrimSpace(os.Getenv(EnvControlTLSName))
		if serverName != "" {
			mode = config.FleetlyEndpointTLS
		}
		cli, cliErr = newGRPCFleetlyClientFor(os.Getenv(EnvControlGRPCAddr), f.GetToken(), mode, serverName)
	}
	return newFleetlyDaemon(cfg, refs, cli, cliErr)
}

// EnvControlGRPCAddr / EnvControlTLSName 是 fleetly 平台向任务 spec 物化的
// 控制面地址 env（engine ctrlinject：advertise:gRPC 端口 + 证书校验名；与
// exec relay 的 FLEETLY_CONTROL_ADDR 同族命名）。dispatcher 端点未显式配置
// 时回落到它们——零配置集群内回拨，值随环境自动正确。
const (
	EnvControlGRPCAddr = "FLEETLY_CONTROL_GRPC_ADDR"
	EnvControlTLSName  = "FLEETLY_CONTROL_TLS_NAME"
)

func newFleetlyDaemon(cfg *config.AppConfig, refs imageRefStore, cli fleetlyTaskClient, cliErr error) *fleetlyDaemon {
	pc := PoolConfigFromConfig(cfg)
	return &fleetlyDaemon{
		cfg:                cfg,
		refs:               refs,
		cli:                cli,
		cliErr:             cliErr,
		bootTimeout:        pc.BootTimeout,
		maxRequestsDefault: pc.MaxRequestsDefault,
		pollInterval:       200 * time.Millisecond,
		sleep:              defaultSleep,
		probe:              newHTTPRunner(),
		scopes:             map[string]taskScope{},
		membersEnsured:     map[string]bool{},
	}
}

func (d *fleetlyDaemon) client() (fleetlyTaskClient, error) {
	if d.cliErr != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "fleetly client unavailable: %v", d.cliErr)
	}
	if d.cli == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"fleetly client unavailable (functions.fleetly.endpoint is not configured)")
	}
	return d.cli, nil
}

// taskGroupRef 派生 task-group ref（fleetly 命名契约 [a-z0-9]{2,32}，不含
// '-'）：可信 = p<projectID>、不可信（internal 变体）= q<projectID>。首字符
// 区分两变体 ⇒ 结构上不相交（后缀方案会让 projectID 以 u 结尾的项目与
// 不可信变体撞名）；projectID 已过 ident 白名单（^[a-z][a-z0-9]{0,27}$），
// 总长 ≤29 在 32 上限内。
func taskGroupRef(projectID string, untrusted bool) (string, error) {
	if err := ident.ValidateSchemaResourceID(projectID); err != nil {
		return "", status.Errorf(codes.InvalidArgument, "invalid project id for function execution: %v", err)
	}
	if untrusted {
		return "q" + projectID, nil
	}
	return "p" + projectID, nil
}

// EnsureProjectNetwork 确保项目任务网络在位（fleetly EnsureTaskNetwork，
// 幂等长活）并按需声明控制面挂靠成员（每网一次；成员挂靠经平台发布管线
// 重部署生效，重复声明零新增）。
func (d *fleetlyDaemon) EnsureProjectNetwork(ctx context.Context, projectID string, untrusted bool) (string, error) {
	cli, err := d.client()
	if err != nil {
		return "", err
	}
	ref, err := taskGroupRef(projectID, untrusted)
	if err != nil {
		return "", err
	}
	req := &serverv1.EnsureTaskNetworkRequest{Ref: ref, Internal: untrusted}
	d.mu.Lock()
	alreadyDeclared := d.membersEnsured[ref]
	d.mu.Unlock()
	if !alreadyDeclared {
		req.Members = d.configuredMembers()
	}
	resp, err := cli.EnsureTaskNetwork(ctx, req)
	if err != nil {
		return "", fmt.Errorf("ensure task network %s: %w", ref, mapFleetlyError("ensure task network", err))
	}
	name := resp.GetName()
	if name == "" {
		return "", status.Error(codes.Internal, "fleetly EnsureTaskNetwork returned an empty network name")
	}
	d.mu.Lock()
	d.scopes[name] = taskScope{ref: ref, internal: untrusted}
	if len(req.Members) > 0 {
		d.membersEnsured[ref] = true
	}
	d.mu.Unlock()
	return name, nil
}

// configuredMembers 组装控制面挂靠成员（functions.fleetly.app +
// network_members；未配置 app 或成员时返回 nil——不声明挂靠）。
func (d *fleetlyDaemon) configuredMembers() []*serverv1.TaskNetworkMember {
	f := d.cfg.GetFunctions().GetFleetly()
	app := strings.TrimSpace(f.GetApp())
	if app == "" {
		return nil
	}
	members := make([]*serverv1.TaskNetworkMember, 0, len(f.GetNetworkMembers()))
	for _, service := range f.GetNetworkMembers() {
		service = strings.TrimSpace(service)
		if service == "" {
			continue
		}
		members = append(members, &serverv1.TaskNetworkMember{App: app, Service: service})
	}
	return members
}

// SpawnInstance 创建并启动常驻任务实例（fleetly CreateTask），等待平台
// 收敛到 running 后返回句柄。失败路径尽力回收任务（幂等）。
func (d *fleetlyDaemon) SpawnInstance(ctx context.Context, opts SpawnOptions) (Instance, error) {
	cli, err := d.client()
	if err != nil {
		return Instance{}, err
	}
	scope, err := d.scopeFor(opts.Network)
	if err != nil {
		return Instance{}, err
	}
	imageRef, err := d.resolveImageRef(ctx, opts.Image)
	if err != nil {
		return Instance{}, err
	}
	res := infrafunctions.SpecResources(opts.Spec)
	resp, err := cli.CreateTask(ctx, &serverv1.CreateTaskRequest{
		Name:  opts.Name,
		Image: imageRef,
		Env:   spawnEnv(opts),
		Scope: &serverv1.TaskScope{Kind: "task-group", Ref: scope.ref, Internal: scope.internal},
		// TTL = 平台上限（24h）：常驻实例的兜底回收。池语义（idle 回收/
		// max_requests 自退/熔断重建）仍自持——正常生命周期远短于兜底值，
		// 平台 TTL 只兜住「dispatcher 整体失联」的泄漏面。
		TtlSeconds:  taskTTLSeconds,
		CpuMillis:   res.NanoCPUs / 1e6,
		MemoryBytes: res.Memory,
	})
	if err != nil {
		return Instance{}, mapFleetlyError("create task", err)
	}
	task := resp.GetTask()
	taskID := task.GetId()
	if taskID == "" {
		return Instance{}, status.Error(codes.Internal, "fleetly CreateTask returned an empty task id")
	}
	if err := d.awaitTaskRunning(ctx, cli, taskID); err != nil {
		// 收敛失败（failed/超时）：尽力回收，避免平台侧悬挂非终态行。
		d.cleanupTask(taskID)
		return Instance{}, err
	}
	return Instance{ContainerID: taskID, IP: task.GetDnsName()}, nil
}

// awaitTaskRunning 等待平台把任务收敛到 running：queued → running（引擎
// 2s tick 级）；failed 立即上抛（含平台失败原因）；stopping/stopped/
// deleting 视为异常终态；预算 = bootTimeout（与健康探针共享同一键的语义
// 分层：平台收敛预算 + runner 就绪预算，最坏 2×bootTimeout）。
func (d *fleetlyDaemon) awaitTaskRunning(ctx context.Context, cli fleetlyTaskClient, taskID string) error {
	deadline := time.Now().Add(d.bootTimeout)
	for {
		resp, err := cli.GetTask(ctx, &serverv1.GetTaskRequest{Id: taskID})
		if err != nil {
			return mapFleetlyError("get task", err)
		}
		task := resp.GetTask()
		switch task.GetStatus() {
		case "running":
			return nil
		case "failed":
			reason := strings.TrimSpace(task.GetError())
			if reason == "" {
				reason = "task failed (no reason reported)"
			}
			return status.Errorf(codes.Internal, "task %s failed before running: %s", taskID, reason)
		case "stopping", "stopped", "deleting":
			return status.Errorf(codes.Internal, "task %s reached terminal state %q before running", taskID, task.GetStatus())
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return status.Errorf(codes.DeadlineExceeded,
				"task %s did not reach running within %s (status %q)", taskID, d.bootTimeout, task.GetStatus())
		}
		if !d.sleep(ctx, d.pollInterval) {
			return ctx.Err()
		}
	}
}

// scopeFor 取网络名对应作用域（EnsureProjectNetwork 登记；未登记 = 调用
// 顺序违约，显式失败——不猜测 ref/变体）。
func (d *fleetlyDaemon) scopeFor(network string) (taskScope, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	scope, ok := d.scopes[network]
	if !ok {
		return taskScope{}, status.Errorf(codes.FailedPrecondition,
			"task network %q is not known to this dispatcher process (EnsureProjectNetwork must run first)", network)
	}
	return scope, nil
}

// resolveImageRef 解析 spawn 镜像引用：映射命中取平台产物引用；未命中以
// 原样引用交给平台（映射缺失时逻辑名在平台侧必然解析失败 → 上层映射为
// ImageMissingMarker，触发 server 侧重建并刷新映射——自愈闭环）。
func (d *fleetlyDaemon) resolveImageRef(ctx context.Context, image string) (string, error) {
	if d.refs == nil {
		return image, nil
	}
	ref, err := d.refs.LoadImageRef(ctx, image)
	if err != nil {
		// 映射通道故障不静默降级：原样引用可能意外命中他处（错误镜像），
		// fail-closed 让 spawn 失败留现场。
		return "", status.Errorf(codes.Unavailable, "load image reference mapping for %q: %v", image, err)
	}
	if ref == "" {
		return image, nil
	}
	return ref, nil
}

// spawnEnv 组装任务 env：池的 []string 形态 → fleetly map；自回收控制键
// （TW_MAX_REQUESTS/TW_DRAIN_TIMEOUT_MS）由共享构造器 AppendRunnerControlEnv
// 单点注入（与 docker 直接形态同源）。
func spawnEnv(opts SpawnOptions) map[string]string {
	env := make(map[string]string, len(opts.Env)+2)
	for _, kv := range AppendRunnerControlEnv(opts.Env, opts.MaxRequests) {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		env[key] = value
	}
	return env
}

// InspectInstance 返回任务运行状态（fleetly GetTask）。
func (d *fleetlyDaemon) InspectInstance(ctx context.Context, containerID string) (bool, string, error) {
	cli, err := d.client()
	if err != nil {
		return false, "", err
	}
	resp, err := cli.GetTask(ctx, &serverv1.GetTaskRequest{Id: containerID})
	if err != nil {
		return false, "", mapFleetlyError("get task", err)
	}
	task := resp.GetTask()
	if task.GetStatus() == "running" {
		return true, task.GetDnsName(), nil
	}
	return false, "", nil
}

// StopInstance 停止任务（fleetly StopTask；幂等：停止中/已停止成功返回，
// 不存在视为已停止）。timeout 参数不再改变停机行为（平台 stop_grace 固定
// 5s——池的 drain 宽限语义在 swarm 底座上由平台常数承载）。
func (d *fleetlyDaemon) StopInstance(ctx context.Context, containerID string, _ time.Duration) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	if _, err := cli.StopTask(ctx, &serverv1.StopTaskRequest{Id: containerID}); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("stop task %s: %w", containerID, mapFleetlyError("stop task", err))
	}
	return nil
}

// RemoveInstance 删除任务（fleetly DeleteTask：停止 + 移除底座服务 + 台账
// 行；幂等，不存在视为成功）。
func (d *fleetlyDaemon) RemoveInstance(ctx context.Context, containerID string) error {
	cli, err := d.client()
	if err != nil {
		return err
	}
	if _, err := cli.DeleteTask(ctx, &serverv1.DeleteTaskRequest{Id: containerID}); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("remove task %s: %w", containerID, mapFleetlyError("delete task", err))
	}
	return nil
}

// cleanupTask 是失败路径的尽力回收（独立短超时 ctx；Stop+Delete 幂等）。
func (d *fleetlyDaemon) cleanupTask(taskID string) {
	cli, err := d.client()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), CleanupTimeout)
	defer cancel()
	_, _ = cli.StopTask(ctx, &serverv1.StopTaskRequest{Id: taskID})
	_, _ = cli.DeleteTask(ctx, &serverv1.DeleteTaskRequest{Id: taskID})
}

// InstanceLogsTail 返回任务台账的失败现场（status/error/stop_reason）。
// 诚实边界（见 Daemon 接口注释）：容器日志在平台 VictoriaLogs，机具令牌
// 无日志读面——不回读 stdout/stderr。
func (d *fleetlyDaemon) InstanceLogsTail(ctx context.Context, containerID string, _ int64) (string, error) {
	cli, err := d.client()
	if err != nil {
		return "", err
	}
	resp, err := cli.GetTask(ctx, &serverv1.GetTaskRequest{Id: containerID})
	if err != nil {
		return "", mapFleetlyError("get task", err)
	}
	task := resp.GetTask()
	var b strings.Builder
	fmt.Fprintf(&b, "task %s status=%s", task.GetId(), task.GetStatus())
	if v := strings.TrimSpace(task.GetError()); v != "" {
		fmt.Fprintf(&b, " error=%s", v)
	}
	if v := strings.TrimSpace(task.GetStopReason()); v != "" {
		fmt.Fprintf(&b, " stop_reason=%s", v)
	}
	if v := task.GetStartedAt(); v != nil {
		fmt.Fprintf(&b, " started_at=%s", v.AsTime().UTC().Format(time.RFC3339))
	}
	b.WriteString("\n(container logs are collected by the fleetly platform (VictoriaLogs); query them with `fleetly tasks logs <task-id>`)")
	return b.String(), nil
}

// BuildImage 以 runner 模板构建镜像：构建上下文准备（zip 解压校验 →
// runtime 对账 → Go bootstrap 生成 / node runner 写入 → Dockerfile 渲染，
// 见 prepareBuildContext）→ tar 流 → fleetly BuildFromUpload（平台侧
// buildkitd 构建并推 registry/装载；调用方零 push 凭证）。
//
// 顺序与旧链对齐：构建成功 → 登记逻辑名 → 产物引用映射 → 验证 spawn
// （配置开启时）；验证失败直接以失败收场，映射保留（下次重试/重建可复用）。
func (d *fleetlyDaemon) BuildImage(ctx context.Context, opts BuildImageOptions) error {
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

	name, err := uploadImageName(opts.FunctionID, opts.DeploymentID)
	if err != nil {
		return err
	}
	// 流式 tar：BuildFromUpload 失败路径必须 Close 读端——唤醒可能仍阻塞在
	// pipe 写侧的打包 goroutine（gRPC 传输不会读到 EOF）；成功路径由
	// 流发送完毕自然收尾。
	tarCtx := tarDir(buildDir)
	resp, err := cli.BuildFromUpload(ctx, name, "Dockerfile", tarCtx)
	_ = tarCtx.Close()
	if err != nil {
		return fmt.Errorf("fleetly build failed: %s", infrafunctions.TruncateBuildLog(err.Error()))
	}
	ref := buildImageRef(resp.GetBuild())
	if ref == "" {
		return status.Error(codes.Internal, "fleetly build succeeded but returned no image reference")
	}
	if d.refs == nil {
		return status.Error(codes.FailedPrecondition, "image reference store is not assembled: the built image cannot be addressed")
	}
	logical := infrafunctions.ImageName(d.cfg, opts.FunctionID, opts.DeploymentID)
	if err := d.refs.SaveImageRef(ctx, logical, ref); err != nil {
		// 映射写失败 = 构建失败：后续 spawn 无从解析产物引用（逻辑名在平台
		// 侧解析必然失败），静默继续只会把故障推迟到执行面。
		return fmt.Errorf("record built image reference %q: %w", logical, err)
	}
	// 部署后验证 spawn（D10）：opts.Verify=false（config verify_build 显式
	// 关闭）跳过整段。
	if opts.Verify {
		if err := d.verifyBuild(ctx, opts, ref); err != nil {
			return err
		}
	}
	return nil
}

// uploadImageName 派生 fleetly build-from-upload 的镜像仓组件名
// （[a-z0-9._-]，首末位字母数字，≤100）：func-<function>-<deployment>，
// 小写化 + 非法字符折叠。function/deployment ID 为平台 ID（UUID/ULID
// 级长度），总长在限制内。
func uploadImageName(functionID, deploymentID string) (string, error) {
	if strings.TrimSpace(functionID) == "" || strings.TrimSpace(deploymentID) == "" {
		return "", status.Error(codes.InvalidArgument, "function/deployment id cannot derive a valid image name")
	}
	name := sanitizeUploadName("func-" + strings.ToLower(functionID) + "-" + strings.ToLower(deploymentID))
	if name == "" {
		return "", status.Error(codes.InvalidArgument, "function/deployment id cannot derive a valid image name")
	}
	if len(name) > 100 {
		name = name[:100]
		name = strings.TrimRight(name, "._-")
		if name == "" {
			return "", status.Error(codes.InvalidArgument, "function/deployment id cannot derive a valid image name")
		}
	}
	return name, nil
}

func sanitizeUploadName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, s)
	return strings.Trim(s, "._-")
}

// buildImageRef 取构建产物引用：registry 模式 = image_ref（digest 钉定）；
// 本地模式 = image_ref（fleetly-local/<name>:<tag>）；image_ref 缺失时回落
// image_digest（防御异常平台响应）。
func buildImageRef(build *serverv1.BuildView) string {
	if build == nil {
		return ""
	}
	if ref := strings.TrimSpace(build.GetImageRef()); ref != "" {
		return ref
	}
	if digest := strings.TrimSpace(build.GetImageDigest()); digest != "" {
		return digest
	}
	return ""
}

// verifyBuild 是 fleetlyDaemon 的验证 spawn 入口：探针复用池的 HTTP runner
// 客户端（同一 /_tw/health 契约面），预算与 TW_MAX_REQUESTS 注入值取构造时
// 解析的池参数（本进程 boot_timeout，设计 §1）。镜像 = 平台产物引用（不经
// 逻辑名映射——构建刚产出的引用直接可用）。
func (d *fleetlyDaemon) verifyBuild(ctx context.Context, opts BuildImageOptions, imageRef string) error {
	return spawnVerifyInstance(ctx, d, d.probe, opts, verifySpawnConfig{
		Image:       imageRef,
		BootTimeout: d.bootTimeout,
		MaxRequests: d.maxRequestsDefault,
	})
}

// ImportImage 导入外部镜像（BYO 镜像源）：
//
//	host 名称级准入校验（保留）→ 幂等快速路径（映射已持有一致 digest →
//	零平台往返）→ 验证任务（fleetly CreateTask 解析/拉取/钉定镜像）→
//	digest 一致性校验（防 tag 漂移）→ 强制契约验证 spawn → 登记映射 →
//	返回钉死 digest（调用方落 deployment.source_ref）。
//
// 与旧链的语义差异（如实登记）：拉取/retag 归平台（无本地平台镜像命名），
// 「本地命中零 pull」由映射快速路径承载；映射丢失即重新验证（幂等可重入）。
func (d *fleetlyDaemon) ImportImage(ctx context.Context, opts ImportImageOptions) (string, error) {
	// 1) registry host 名称级校验（设计 §3 安全基线的可实现口径：名称级
	//    校验 + 白名单 + 信任级论证；失败 InvalidArgument 明示命中规则）。
	//    先于一切平台操作——幂等快速路径同样受准入口径约束。
	if err := validateImageRegistryHost(parseImageReferenceHost(opts.Reference),
		d.cfg.GetFunctions().GetImage().GetAllowedRegistries(),
		d.cfg.GetFunctions().GetImage().GetAllowInsecure()); err != nil {
		return "", err
	}
	if d.refs == nil {
		return "", status.Error(codes.FailedPrecondition, "image reference store is not assembled: the imported image cannot be addressed")
	}

	logical := infrafunctions.ImageName(d.cfg, opts.FunctionID, opts.DeploymentID)

	// 2) 幂等快速路径（映射命中且 digest 一致 → 零平台往返；worker 补拉/
	//    ready 门禁复检的「本地命中零 pull」等价形态）。
	if opts.ExpectedDigest != "" {
		ref, err := d.refs.LoadImageRef(ctx, logical)
		if err != nil {
			return "", status.Errorf(codes.Unavailable, "load image reference mapping for %q: %v", logical, err)
		}
		if ref != "" && digestOfReference(ref) == opts.ExpectedDigest {
			return opts.ExpectedDigest, nil
		}
	}

	// 3) 验证任务：平台解析/拉取用户引用并钉定 digest（任务视图 image =
	//    钉定引用），随后契约验证 spawn 探活。
	network, err := d.EnsureProjectNetwork(ctx, opts.ProjectID, opts.EgressUntrusted)
	if err != nil {
		return "", fmt.Errorf("verify import: ensure network: %w", err)
	}
	inst, cleanup, err := d.startVerificationTask(ctx, BuildImageOptions{
		ProjectID:       opts.ProjectID,
		FunctionID:      opts.FunctionID,
		DeploymentID:    opts.DeploymentID,
		Env:             opts.Env,
		EgressUntrusted: opts.EgressUntrusted,
	}, verifySpawnConfig{
		Image:       opts.Reference,
		BootTimeout: d.bootTimeout,
		MaxRequests: d.maxRequestsDefault,
	}, network)
	if err != nil {
		return "", fmt.Errorf("verify import: %w", err)
	}
	defer cleanup()

	pinnedRef, err := d.taskImageRef(ctx, inst.ContainerID)
	if err != nil {
		return "", err
	}
	digest := digestOfReference(pinnedRef)
	if digest == "" {
		return "", status.Errorf(codes.InvalidArgument,
			"image %q resolved to %q without a manifest digest (use a registry reference with digest pinning)", opts.Reference, pinnedRef)
	}
	if opts.ExpectedDigest != "" && digest != opts.ExpectedDigest {
		return "", status.Errorf(codes.InvalidArgument,
			"resolved image digest %q does not match expected digest %q (tag drift; redeploy pinning the exact digest)",
			digest, opts.ExpectedDigest)
	}

	if err := d.awaitVerificationHealthy(ctx, d.probe, inst, BuildImageOptions{
		ProjectID:       opts.ProjectID,
		FunctionID:      opts.FunctionID,
		DeploymentID:    opts.DeploymentID,
		Env:             opts.Env,
		EgressUntrusted: opts.EgressUntrusted,
	}, verifySpawnConfig{Image: pinnedRef, BootTimeout: d.bootTimeout, MaxRequests: d.maxRequestsDefault}); err != nil {
		return "", err
	}

	if err := d.refs.SaveImageRef(ctx, logical, pinnedRef); err != nil {
		return "", fmt.Errorf("record imported image reference %q: %w", logical, err)
	}
	return digest, nil
}

// taskImageRef 读取任务视图的钉定镜像引用（平台解析产物）。
func (d *fleetlyDaemon) taskImageRef(ctx context.Context, taskID string) (string, error) {
	cli, err := d.client()
	if err != nil {
		return "", err
	}
	resp, err := cli.GetTask(ctx, &serverv1.GetTaskRequest{Id: taskID})
	if err != nil {
		return "", mapFleetlyError("get task", err)
	}
	ref := strings.TrimSpace(resp.GetTask().GetImage())
	if ref == "" {
		return "", status.Errorf(codes.Internal, "task %s returned an empty pinned image reference", taskID)
	}
	return ref, nil
}

// digestOfReference 提取引用中的 digest（"repo@sha256:..." → "sha256:..."；
// 无 digest 返回空串）。
func digestOfReference(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	return ""
}

// RemoveImage 删除构建产物引用映射（幂等）。平台侧产物 GC 归平台（本仓无
// 镜像删除 API）；映射删除后 spawn 不再解析到该引用，server 重建链路自愈。
func (d *fleetlyDaemon) RemoveImage(ctx context.Context, functionID, deploymentID string) error {
	if d.refs == nil {
		return nil
	}
	logical := infrafunctions.ImageName(d.cfg, functionID, deploymentID)
	return d.refs.DeleteImageRef(ctx, logical)
}

// prepareBuildContext 在 buildDir 准备镜像构建上下文（纯文件编排，单测以
// 临时 zip 直接驱动）。顺序敏感（runtime 对账先于模板渲染）：
//
//  1. 解压 zip 并探测部署源（ExtractZipRelaxed）；
//  2. runtime 一致性对账（D7'）：探测产出语言族（family），版本轴来自
//     声明——opts.Runtime 非空且其 family ≠ 探测 family → InvalidArgument
//     （错误信息含声明 ID 与探测 family）；未知 runtime ID 同样拒绝
//     （fail-closed）；
//  3. node 分支写 .tw-runner.js；go 分支零平台注入（五期 5b：用户 zip 根 =
//     package main + SDK，构建 = go build .，无 bootstrap 生成/入口探测）；
//     最后渲染 Dockerfile（缺 go.sum 等拒收错误在此冒出）。
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
	// 条目预算击毙。SourceContents 附带部署源探测结果（family 标记），
	// 逐字段映射为 runner 包的模板载体（runner 保持叶子资产包，不 import
	// infra/functions 根包）。
	contents, err := infrafunctions.ExtractZipRelaxed(tmpZip.Name(), buildDir)
	if err != nil {
		return err
	}

	// runtime 一致性对账（D7'，functions-runtime-selection.md §2）：探测产出
	// 语言族，版本轴来自声明——声明的 runtime ID 的 family 必须与探测
	// family 一致，否则构建期 InvalidArgument。opts.Runtime 为空 = 遗留
	// 调用方（跳过对账）：渲染基准取探测 family 的首个 active 表项（平台
	// 缺省 runtime），不再隐含历史 node:18 耦合。
	renderRuntime := opts.Runtime
	if opts.Runtime != "" {
		declaredFamily := domainfunctions.FamilyOf(opts.Runtime)
		if declaredFamily == "" {
			return status.Errorf(codes.InvalidArgument, "unsupported runtime %q", opts.Runtime)
		}
		if declaredFamily != contents.Family {
			return status.Errorf(codes.InvalidArgument,
				"runtime mismatch: function declares %q (family %q) but source probes as %q family (redeploy with a runtime whose family matches the source)",
				opts.Runtime, declaredFamily, contents.Family)
		}
	} else {
		def, ok := domainfunctions.DefaultRuntimeForFamily(contents.Family)
		if !ok {
			return status.Errorf(codes.InvalidArgument, "source probes as %q family but no runtime is available for platform builds", contents.Family)
		}
		renderRuntime = def.ID
	}

	// engines.node 校验（functions-runtime-selection.md §5）：Node 生态正规
	// 声明位与所选 runtime 的 major 相交性——「本地 node 22、线上 node 18」
	// 类漂移在部署期显式化。与 D11 正交（纯读取 + 比较，零新执行面）。
	if err := checkNodeEngines(contents.NodeEngines, renderRuntime); err != nil {
		return err
	}

	dockerfile, err := runner.DockerfileFor(runner.SourceContents{
		Runtime:       renderRuntime,
		NodeDeps:      contents.NodeDeps,
		HasLockfile:   contents.HasLockfile,
		GoModulePath:  contents.GoModulePath,
		GoHasRequires: contents.GoHasRequires,
		GoHasSum:      contents.GoHasSum,
		HasVendor:     contents.HasVendor,
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

// containerName 生成可读任务名：tw-fn-<project>-<function>-<rand6>。
// 与镜像逻辑名同族的可读性——fleetly 任务视图一眼对上项目/函数。
// project/function 上游已过 ID 白名单（project ^[a-z][a-z0-9]{0,27}$、
// function ^[a-z0-9][a-z0-9_-]{0,63}$），此处防御性小写化 + 非法字符折叠为
// '-'；随机后缀（6 hex）保证同函数多实例/反复部署唯一。历史遗留大写
// functionID 同步小写（G6-3 同口径）。
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
	suffix := newSpawnLockToken()[:6]
	return fmt.Sprintf("tw-fn-%s-%s-%s", sanitize(projectID), sanitize(functionID), suffix)
}

// CleanupTimeout 是清理类平台/daemon 操作（stop/delete/remove）的独立超时：
// 不继承已取消的执行 ctx，也不允许平台挂起时无限阻塞（两种驱动形态同约定，
// dockerdriver 经 dispatcher.CleanupTimeout 消费同值）。
const CleanupTimeout = 30 * time.Second

// 验证 spawn 的 health 探针节拍（对齐池启动握手 spawnInstance：单次探针
// 2s 超时、轮询间隔 100ms；预算本身 = 本进程 boot_timeout，构造时解析）。
const (
	verifyProbeTimeout = 2 * time.Second
	verifyPollInterval = 100 * time.Millisecond
)

// taskTTLSeconds 是常驻任务实例的平台 TTL（秒）：平台上限 24h，作兜底
// 回收（dispatcher 整体失联时的泄漏面）；正常生命周期由池自持（idle 回收/
// max_requests 自退/熔断重建），远短于此。
const taskTTLSeconds = 86400

// verifySpawnConfig 是验证 spawn 的参数包：镜像引用 + 探针预算 + 注入值。
// PollInterval 零值取 verifyPollInterval（单测注入加速/确定性）。
type verifySpawnConfig struct {
	Image        string
	BootTimeout  time.Duration
	MaxRequests  int
	PollInterval time.Duration
}

// HealthProber 是验证探针的最小抽象（runnerClient 的 Health 面收窄；生产 =
// httpRunner，单测 = fake 表驱动）。IMPL-T2-5 导出：dockerdriver 驱动的
// 验证 spawn 复用同一探针注入缝。
type HealthProber interface {
	Health(ctx context.Context, ip string) error
}

// spawnVerifyInstance 执行一次部署后验证 spawn（设计 §1/D10）：spawn 一枚
// 验证任务 → /_tw/health 轮询（预算 = BootTimeout）→ 就绪即回收。
//
// 池外语义（设计 §1）：走 Daemon 原语直连——不进实例注册表、不受
// MaxResidentInstances 约束、不参与 reaper 对账（BuildImage 无池依赖，天然
// 满足），用完即删；验证范围 = health 探针，不做 invoke——invoke 需要平台
// 构造 TW_DATA 并执行用户代码（副作用不可控），违反「部署期不执行用户代码」
// 不变量（对抗审查 A5 裁决，残余风险由运行期 transport-error 杀实例兜底）。
//
// 失败处置：回收任务台账的失败现场（status/error/stop_reason）拼进错误——
// 运行期错误（panic/协议未实现）在任务 error 字段；编译错误在构建日志、
// 不经此路径。无论成败任务以独立 cleanup ctx Stop+Delete（不继承已取消/
// 临期的构建 ctx，CleanupTimeout 同约定）。
func spawnVerifyInstance(ctx context.Context, d Daemon, probe HealthProber, opts BuildImageOptions, vc verifySpawnConfig) error {
	// egress 分类与执行一致（对抗审查 A1 最强修复）：untrusted 函数的验证
	// 实例挂 internal 变体任务网络（与池 spawnInstance 同路）——验证期不得
	// 给不可信镜像开跳出网窗口。
	network, err := d.EnsureProjectNetwork(ctx, opts.ProjectID, opts.EgressUntrusted)
	if err != nil {
		return fmt.Errorf("verify spawn: ensure network: %w", err)
	}
	inst, err := spawnVerificationTask(ctx, d, opts, vc, network)
	if err != nil {
		return fmt.Errorf("verify spawn: %w", err)
	}
	// 无论成败回收任务（验证实例无在途请求，无需 drain 宽限）。
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), CleanupTimeout)
		defer cancel()
		_ = d.StopInstance(cctx, inst.ContainerID, 0)
		_ = d.RemoveInstance(cctx, inst.ContainerID)
	}()
	return awaitVerificationHealthy(ctx, d, probe, opts, vc, inst)
}

// spawnVerificationTask 创建验证任务（env 组装与池 spawnInstance 同源：
// TW_DATA/TW_EXECUTION_TOKEN 不进容器 env——常驻的是容器不是凭证；
// TW_MAX_REQUESTS/TW_DRAIN_TIMEOUT_MS 由 SpawnInstance 统一追加）。
func spawnVerificationTask(ctx context.Context, d Daemon, opts BuildImageOptions, vc verifySpawnConfig, network string) (Instance, error) {
	// env 组装与池 spawnInstance 同源（SanitizeRunnerEnv：TW_DATA/
	// TW_EXECUTION_TOKEN 不进容器 env——常驻的是容器不是凭证；
	// TW_MAX_REQUESTS/TW_DRAIN_TIMEOUT_MS 由驱动侧 AppendRunnerControlEnv
	// 统一追加）。
	return d.SpawnInstance(ctx, SpawnOptions{
		ProjectID:   opts.ProjectID,
		FunctionID:  opts.FunctionID,
		Image:       vc.Image,
		Network:     network,
		Env:         SanitizeRunnerEnv(opts.Env),
		Spec:        "shared-1x",
		MaxRequests: vc.MaxRequests,
		Name:        containerName(opts.ProjectID, opts.FunctionID),
	})
}

// awaitVerificationHealthy 轮询验证实例的 /_tw/health 直到就绪（预算 =
// BootTimeout）；失败时把任务台账失败现场拼进错误。
func awaitVerificationHealthy(ctx context.Context, d Daemon, probe HealthProber, opts BuildImageOptions, vc verifySpawnConfig, inst Instance) error {
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
			// 失败处置：台账读取与后续清理同用脱离构建 ctx 的独立 ctx
			//（预算到点时构建 ctx 可能已取消/临期）。
			tailCtx, tailCancel := context.WithTimeout(context.WithoutCancel(ctx), CleanupTimeout)
			tail, tailErr := d.InstanceLogsTail(tailCtx, inst.ContainerID, maxLogTailBytes)
			tailCancel()
			if tailErr != nil {
				tail = fmt.Sprintf("<task status unavailable: %v>", tailErr)
			}
			return fmt.Errorf("verification failed: function image did not become healthy within %s (last probe error: %v); task status:\n%s",
				vc.BootTimeout, err, tail)
		}
		if !defaultSleep(ctx, interval) {
			// sleep 期间 ctx 取消：下一轮探针立即失败并走上面的现场回收路径。
			continue
		}
	}
}

// 启动验证任务的统一入口（ImportImage 需要先读钉定引用再探活；
// spawnVerifyInstance 直接探活——两条腿共享同一创建语义）。
func (d *fleetlyDaemon) startVerificationTask(ctx context.Context, opts BuildImageOptions, vc verifySpawnConfig, network string) (Instance, func(), error) {
	inst, err := spawnVerificationTask(ctx, d, opts, vc, network)
	if err != nil {
		return Instance{}, nil, err
	}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), CleanupTimeout)
		defer cancel()
		_ = d.StopInstance(cctx, inst.ContainerID, 0)
		_ = d.RemoveInstance(cctx, inst.ContainerID)
	}
	return inst, cleanup, nil
}

// awaitVerificationHealthy 的 daemon 方法形态（ImportImage 腿复用；避免
// 暴露额外包级函数）。
func (d *fleetlyDaemon) awaitVerificationHealthy(ctx context.Context, probe HealthProber, inst Instance, opts BuildImageOptions, vc verifySpawnConfig) error {
	return awaitVerificationHealthy(ctx, d, probe, opts, vc, inst)
}

// tarDir 将目录流式打包为 build context tar（dispatcher 侧独立实现避免
// 导出面扩散）。
//
// 流式化（P2 S13）：原实现把整棵 tar 缓冲进 bytes.Buffer——50MiB zip 场景
// （解压预算 200MiB）构建峰值内存 ≈ tar 缓冲 + 解压目录 + base64 载荷
// ≈ 800MB 量级。现改 io.Pipe + goroutine 边 WalkDir 边写，调用方
// （BuildFromUpload 接受 io.Reader）直接消费读端，峰值内存降为单文件拷贝
// 缓冲。错误经 pipe 传播（CloseWithError，读侧以读错误收场——遍历目录是
// 刚由 prepareBuildContext 写出的自有文件，出错概率极低，接受错误文案
// 不经「tar build context」包装）。调用方在 BuildFromUpload 失败路径须
// Close 读端，唤醒阻塞中的写侧（防 goroutine 悬挂）。
//
// umask 归一化语义（EACCES 秒退事故的坏档修复）原样保留：文件恒 0644、
// 目录恒 0755、属主归零——见循环内注释。
func tarDir(dir string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeBuildContext(pw, dir))
	}()
	return pr
}

// writeBuildContext 是 tarDir 的写侧：遍历 dir 逐条目写入 tar 流（与流式化
// 前的字节语义一致）。
func writeBuildContext(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
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
		return err
	}
	return tw.Close()
}

// ——fleetly gRPC 客户端（Tasks/build 面；唯一平台通路）——

// grpcFleetlyClient 是 fleetlyTaskClient 的真实实现：grpc 连接 + Tasks/
// Builds 生成客户端 + Bearer 机具令牌（每请求 metadata；令牌不进日志/错误
// 文本）。传输安全由 endpoint scheme 显式选择（config.ParseFleetlyEndpoint
// 唯一真源，gRPC 社区约定 grpc/grpcs）：裸 host:port 与 grpc:// = 栈内明文
// （既有部署兼容，fleetlyd 与本栈同网络）；grpcs:// = TLS + 系统 CA 校验
// （?server_name= 覆盖 SNI；?insecure=true 跳过校验——按 IP 直连等无 SAN
// 形态）。
type grpcFleetlyClient struct {
	conn   *grpc.ClientConn
	tasks  serverv1.TasksServiceClient
	builds serverv1.BuildsServiceClient
}

func newGRPCFleetlyClient(endpoint, token string) (*grpcFleetlyClient, error) {
	ep, err := config.ParseFleetlyEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("dial fleetly endpoint %q: %w", endpoint, err)
	}
	return newGRPCFleetlyClientFor(ep.Addr, token, ep.Mode, ep.ServerName)
}

// newGRPCFleetlyClientFor 是客户端构造的实现位（addr/mode/serverName 已解
// 析）：显式 endpoint 路径（ParseFleetlyEndpoint 产物，serverName 空 =
// ServerName 缺省跟随拨号主机名）与平台物化回落路径（mode/serverName 由
// FLEETLY_CONTROL_* env 派生）共用。
func newGRPCFleetlyClientFor(addr, token string, mode config.FleetlyEndpointMode, serverName string) (*grpcFleetlyClient, error) {
	var transport credentials.TransportCredentials
	switch mode {
	case config.FleetlyEndpointTLS:
		transport = credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: serverName, // 空 = 缺省跟随拨号主机名；平台回落路径携带证书校验名
		})
	case config.FleetlyEndpointTLSInsecure:
		transport = credentials.NewTLS(&tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // 显式选择的跳过校验形态（按 IP 直连等无 SAN 场景）
		})
	default:
		transport = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(transport),
		grpc.WithPerRPCCredentials(fleetlyBearerCredentials{token: token, secure: mode != config.FleetlyEndpointPlain}),
	)
	if err != nil {
		return nil, fmt.Errorf("dial fleetly endpoint %q: %w", addr, err)
	}
	return &grpcFleetlyClient{
		conn:   conn,
		tasks:  serverv1.NewTasksServiceClient(conn),
		builds: serverv1.NewBuildsServiceClient(conn),
	}, nil
}

// Close 释放连接（进程级生命周期；供测试/未来重连面）。
func (c *grpcFleetlyClient) Close() error { return c.conn.Close() }

// fleetlyBearerCredentials 是机具令牌的 PerRPCCredentials 实现：每请求
// （一元与流式）自动携带 authorization metadata。secure 跟随 endpoint 传输
// 模式（TLS 拨号 = true，gRPC 传输层据此强制凭据只走安全连接）。
type fleetlyBearerCredentials struct {
	token  string
	secure bool
}

func (b fleetlyBearerCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b fleetlyBearerCredentials) RequireTransportSecurity() bool { return b.secure }

func (c *grpcFleetlyClient) EnsureTaskNetwork(ctx context.Context, req *serverv1.EnsureTaskNetworkRequest) (*serverv1.EnsureTaskNetworkResponse, error) {
	return c.tasks.EnsureTaskNetwork(ctx, req)
}

func (c *grpcFleetlyClient) CreateTask(ctx context.Context, req *serverv1.CreateTaskRequest) (*serverv1.CreateTaskResponse, error) {
	return c.tasks.CreateTask(ctx, req)
}

func (c *grpcFleetlyClient) GetTask(ctx context.Context, req *serverv1.GetTaskRequest) (*serverv1.GetTaskResponse, error) {
	return c.tasks.GetTask(ctx, req)
}

func (c *grpcFleetlyClient) StopTask(ctx context.Context, req *serverv1.StopTaskRequest) (*serverv1.StopTaskResponse, error) {
	return c.tasks.StopTask(ctx, req)
}

func (c *grpcFleetlyClient) DeleteTask(ctx context.Context, req *serverv1.DeleteTaskRequest) (*serverv1.DeleteTaskResponse, error) {
	return c.tasks.DeleteTask(ctx, req)
}

// uploadChunkBytes 是 BuildFromUpload 的分片大小（512KiB；服务端单帧上限
// 1MiB）。
const uploadChunkBytes = 512 << 10

// BuildFromUpload 上传构建上下文 tar 并等待构建终态（client-streaming：
// 首帧 metadata{name, dockerfile} + 后续 tar 分片）。服务端在流中 fail-closed
// 拒绝（超限/形态违约）时 Send 以 io.EOF 表达流终止——此时补一次
// CloseAndRecv 取真实错误信封（否则被吞成裸 EOF）。
func (c *grpcFleetlyClient) BuildFromUpload(ctx context.Context, name, dockerfile string, contextTar io.Reader) (*serverv1.BuildFromUploadResponse, error) {
	stream, err := c.builds.BuildFromUpload(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&serverv1.BuildFromUploadRequest{
		Payload: &serverv1.BuildFromUploadRequest_Metadata{
			Metadata: &serverv1.BuildFromUploadMetadata{Name: name, Dockerfile: dockerfile},
		},
	}); err != nil {
		return nil, err
	}
	buf := make([]byte, uploadChunkBytes)
	for {
		n, rerr := contextTar.Read(buf)
		if n > 0 {
			if serr := stream.Send(&serverv1.BuildFromUploadRequest{
				Payload: &serverv1.BuildFromUploadRequest_Chunk{Chunk: buf[:n]},
			}); serr != nil {
				if errors.Is(serr, io.EOF) {
					if _, rerr := stream.CloseAndRecv(); rerr != nil {
						return nil, rerr
					}
				}
				return nil, serr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	return stream.CloseAndRecv()
}

// ——错误映射（fleetly 信封码 → torchwood 执行面语义）——

// fleetlyErrorCode 提取 fleetly 错误信封的稳定错误码：gRPC status detail
// （*sharedv1.ErrorResponse）优先；无 detail 时按 "CODE: message" 前缀兜底
// （apperr.Error 的字符串形态）。
func fleetlyErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if st, ok := status.FromError(err); ok {
		for _, detail := range st.Details() {
			if env, ok := detail.(*sharedv1.ErrorResponse); ok && env.GetCode() != "" {
				return env.GetCode()
			}
		}
		msg := st.Message()
		if i := strings.Index(msg, ": "); i > 0 {
			msg = msg[:i]
		}
		if strings.HasPrefix(msg, "E_") {
			return msg
		}
		return ""
	}
	return ""
}

// mapFleetlyError 把 fleetly 错误映射为池可分类的 gRPC status：
//   - 配额触顶（E_TASK_QUOTA_EXCEEDED）→ ResourceExhausted（池的立即上抛
//     分类：fail-closed，不吞进排队等待——守卫③）；
//   - 镜像不可得（E_IMAGE_PULL_FAILED）→ FailedPrecondition +
//     ImageMissingMarker（server/worker 据此触发自动重建，与旧「本节点
//     镜像缺失」同链）；
//   - 输入/形态错误（E_TASK_UNSUPPORTED 等 E_ 码）→ InvalidArgument；
//   - 其余 → Internal（池按可重试吞错留现场）。
func mapFleetlyError(op string, err error) error {
	if err == nil {
		return nil
	}
	switch code := fleetlyErrorCode(err); code {
	case "E_TASK_QUOTA_EXCEEDED":
		return status.Errorf(codes.ResourceExhausted, "%s: %v", op, err)
	case "E_IMAGE_PULL_FAILED":
		return status.Errorf(codes.FailedPrecondition, "%s: %s: %v (rebuild required)",
			op, domainfunctions.ImageMissingMarker, err)
	case "E_TASK_UNSUPPORTED":
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}
