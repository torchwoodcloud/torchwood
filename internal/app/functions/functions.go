package functions

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	"github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/semaphore"
)

// Functions 是 Functions 服务的 use-case 聚合。
type Functions struct {
	cfg        *config.AppConfig
	executor   functions.Executor
	repo       functions.FunctionRepo
	queue      shared.Queue
	usage      domainbilling.UsageCounter // 可选：函数执行时长计量
	projects   projects.Repository        // 可选：启动对账枚举项目（nil 则 Recover 空操作）
	scanCursor appshared.ProjectRotation  // RecoverOrphanExecutions 轮转游标（串行）
	buildSem   semaphore.Semaphore
	// execTokens 是执行身份铸造/吊销端口（P0 执行身份；可选：nil 时执行
	// 不注入 TW_EXECUTION_TOKEN，函数无平台身份——declared_scopes 语义为
	// 增强而非执行前提）。
	execTokens functions.ExecutionTokenService
	// triggers 是函数触发器仓储（P1 触发器模块；nil 时触发器相关入口
	// fail-fast——由 Wire 装配，测试按需注入）。
	triggers functions.TriggerRepo
	// log 是包内 best-effort 日志（cron dispatch 失败等；nil 回落 slog.Default）。
	log *slog.Logger
	// cache 是函数/变量 30s 进程内缓存（P0.5 热路径清账；见 cache.go）。
	cache *fnCache
	// clientQuota 是客户端调用面每用户限频端口（P2；nil 时跳过 Redis 直接
	// 走 DB 计数降级——语义仍 fail-closed，见 clientinvoke.go）。
	clientQuota functions.ClientQuotaLimiter
	// packer 是 git 部署源打包端口（二期阶段 3，设计 §2；nil 时 git 部署
	// fail-fast 报「functions.packer.url is not configured」——worker 补构建
	// 读盘上 zip 不需要 packer，旧构造保持 nil，zip 路径不受影响）。
	packer functions.SourcePacker
	// zipStore 是部署代码包持久层端口（zip 持久桶；nil 时写路径仅落本地
	// 盘、重建链路退化为既有声明边界——zip 源盘缺失不可自愈。server 与
	// worker 均注入：worker 异步执行错误路径同样触发镜像缺失重建）。
	zipStore functions.ZipStore
	// userGate 是每用户并发闸门（P2：进程内 keyed 信号量，默认每用户 2）。
	userGate *userGateLimiter
	// rebuildMu/rebuilding 是镜像缺失自动重建的进程内在途去重（rebuild.go；
	// 并发执行同时命中同一缺失镜像只触发一次后台重建）。跨副本去重走
	// dedup 端口（server/worker 多副本都触发重建），本 map 恒参与、作
	// 单进程第一道闸。
	rebuildMu  sync.Mutex
	rebuilding map[rebuildKey]struct{}
	// dedup 是在途重建去重的跨进程端口（Redis SETNX；缺陷 B——进程内 map
	// 无法去重 server 与 worker 并发触发的重建）。nil = 回落纯进程内 map
	// （旧构造/测试兼容）；Redis 故障 fail-open（去重是效率优化，不阻断
	// 自愈链）。
	dedup RebuildDedup
}

func NewFunctions(cfg *config.AppConfig, executor functions.Executor, repo functions.FunctionRepo, queue shared.Queue) *Functions {
	f := &Functions{cfg: cfg, executor: executor, repo: repo, queue: queue, cache: newFnCache(), rebuilding: map[rebuildKey]struct{}{}}
	f.buildSem = semaphore.NewInMemory(4)
	f.initUserGate(cfg)
	return f
}

// NewFunctionsWithUsage 注入用量计数器、项目目录与执行身份端口（Wire）；
// 测试仍用 NewFunctions。triggers 是触发器仓储（P1）；zipStore 是部署
// 代码包持久层（worker 装配入口——worker 的异步执行错误路径同样触发
// 镜像缺失重建，需要从持久层拉回；测试传 nil 即旧语义）。sems.RebuildDedup
// 是在途重建跨进程去重端口（nil = 回落进程内 map）。
func NewFunctionsWithUsage(cfg *config.AppConfig, executor functions.Executor, repo functions.FunctionRepo, queue shared.Queue, usage domainbilling.UsageCounter, projectRepo projects.Repository, sems Semaphores, execTokens functions.ExecutionTokenService, triggers functions.TriggerRepo, zipStore functions.ZipStore) *Functions {
	f := NewFunctions(cfg, executor, repo, queue)
	f.usage = usage
	f.projects = projectRepo
	if sems.Build != nil {
		f.buildSem = sems.Build
	}
	if sems.RebuildDedup != nil {
		f.dedup = sems.RebuildDedup
	}
	f.execTokens = execTokens
	f.triggers = triggers
	f.zipStore = zipStore
	return f
}

// NewFunctionsWithClientQuota 是 Wire 装配入口（P2 客户端调用面）：在
// NewFunctionsWithUsage 之上注入每用户限频端口（Redis 固定窗口）。
// 测试侧仍用 NewFunctions/NewFunctionsWithUsage（clientQuota nil = 直接走
// DB 计数降级路径）。
func NewFunctionsWithClientQuota(cfg *config.AppConfig, executor functions.Executor, repo functions.FunctionRepo, queue shared.Queue, usage domainbilling.UsageCounter, projectRepo projects.Repository, sems Semaphores, execTokens functions.ExecutionTokenService, triggers functions.TriggerRepo, clientQuota functions.ClientQuotaLimiter) *Functions {
	f := NewFunctionsWithUsage(cfg, executor, repo, queue, usage, projectRepo, sems, execTokens, triggers, nil)
	f.clientQuota = clientQuota
	return f
}

// NewFunctionsWithSourcePacker 是 Wire 装配入口（二期阶段 3，git 部署源）：
// 在 NewFunctionsWithClientQuota 之上注入 SourcePacker（packer
// HTTP 客户端）与部署代码包持久层 zipStore。测试侧仍用 NewFunctions/
// NewFunctionsWithUsage/NewFunctionsWithClientQuota（packer/zipStore nil =
// git 源 fail-fast、代码包仅落本地盘，zip 路径不受影响）。
func NewFunctionsWithSourcePacker(cfg *config.AppConfig, executor functions.Executor, repo functions.FunctionRepo, queue shared.Queue, usage domainbilling.UsageCounter, projectRepo projects.Repository, sems Semaphores, execTokens functions.ExecutionTokenService, triggers functions.TriggerRepo, clientQuota functions.ClientQuotaLimiter, packer functions.SourcePacker, zipStore functions.ZipStore) *Functions {
	f := NewFunctionsWithClientQuota(cfg, executor, repo, queue, usage, projectRepo, sems, execTokens, triggers, clientQuota)
	f.packer = packer
	f.zipStore = zipStore
	return f
}

// initUserGate 初始化每用户并发闸门（P2）：config functions.client_invoke
// 未配置时取默认（每用户 2，队首超时 5s）。
func (f *Functions) initUserGate(cfg *config.AppConfig) {
	limit := int(cfg.GetFunctions().GetClientInvoke().GetPerUserConcurrency())
	if limit <= 0 {
		limit = defaultPerUserConcurrency
	}
	timeout := parseDurationValue(cfg.GetFunctions().GetClientInvoke().GetQueueHeadTimeout(), defaultUserQueueHeadTimeout)
	f.userGate = newUserGateLimiter(limit, timeout)
}

// logger 返回包内日志器（nil 安全）。
func (f *Functions) logger() *slog.Logger {
	if f.log != nil {
		return f.log
	}
	return slog.Default()
}

// WithSemaphores 注入分布式构建信号量（Wire 覆盖默认内存信号量）。
// buildSem 限制并发构建（默认 4）；执行并发不设全局信号量——常驻实例池由
// dispatcher 内部管控（池上限/有界排队，设计 §6）。
func (f *Functions) WithSemaphores(buildSem semaphore.Semaphore) *Functions {
	if buildSem != nil {
		f.buildSem = buildSem
	}
	return f
}

func (f *Functions) getBuildSemaphore() semaphore.Semaphore {
	if f.buildSem != nil {
		return f.buildSem
	}
	return semaphore.NewInMemory(4)
}

func sanitizeEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if strings.ContainsAny(k, "\n\r\x00") {
			continue
		}
		out[k] = v
	}
	return out
}

func (f *Functions) RuntimeImage(runtime string) string {
	registry := f.cfg.GetFunctions().GetDocker().GetRegistry()
	if registry == "" {
		registry = "torchwood-funcs"
	}
	return fmt.Sprintf("%s/runtime-%s:latest", registry, runtime)
}
