package functions

import (
	"fmt"
	"log/slog"
	"strings"

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
	runSem     semaphore.Semaphore
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
	// userGate 是每用户并发闸门（P2：进程内 keyed 信号量，默认每用户 2）。
	userGate *userGateLimiter
}

func NewFunctions(cfg *config.AppConfig, executor functions.Executor, repo functions.FunctionRepo, queue shared.Queue) *Functions {
	f := &Functions{cfg: cfg, executor: executor, repo: repo, queue: queue, cache: newFnCache()}
	f.buildSem = semaphore.NewInMemory(4)
	f.runSem = semaphore.NewInMemory(16)
	f.initUserGate(cfg)
	return f
}

// NewFunctionsWithUsage 注入用量计数器、项目目录与执行身份端口（Wire）；
// 测试仍用 NewFunctions。triggers 是触发器仓储（P1）。
func NewFunctionsWithUsage(cfg *config.AppConfig, executor functions.Executor, repo functions.FunctionRepo, queue shared.Queue, usage domainbilling.UsageCounter, projectRepo projects.Repository, sems Semaphores, execTokens functions.ExecutionTokenService, triggers functions.TriggerRepo) *Functions {
	f := NewFunctions(cfg, executor, repo, queue)
	f.usage = usage
	f.projects = projectRepo
	if sems.Build != nil {
		f.buildSem = sems.Build
	}
	if sems.Run != nil {
		f.runSem = sems.Run
	}
	f.execTokens = execTokens
	f.triggers = triggers
	return f
}

// NewFunctionsWithClientQuota 是 Wire 装配入口（P2 客户端调用面）：在
// NewFunctionsWithUsage 之上注入每用户限频端口（Redis 固定窗口）。
// 测试侧仍用 NewFunctions/NewFunctionsWithUsage（clientQuota nil = 直接走
// DB 计数降级路径）。
func NewFunctionsWithClientQuota(cfg *config.AppConfig, executor functions.Executor, repo functions.FunctionRepo, queue shared.Queue, usage domainbilling.UsageCounter, projectRepo projects.Repository, sems Semaphores, execTokens functions.ExecutionTokenService, triggers functions.TriggerRepo, clientQuota functions.ClientQuotaLimiter) *Functions {
	f := NewFunctionsWithUsage(cfg, executor, repo, queue, usage, projectRepo, sems, execTokens, triggers)
	f.clientQuota = clientQuota
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

// WithSemaphores 注入分布式信号量（Wire 覆盖默认内存信号量）。
// buildSem 限制并发构建（默认 4），runSem 限制并发执行（默认 16）。
func (f *Functions) WithSemaphores(buildSem, runSem semaphore.Semaphore) *Functions {
	if buildSem != nil {
		f.buildSem = buildSem
	}
	if runSem != nil {
		f.runSem = runSem
	}
	return f
}

func (f *Functions) getBuildSemaphore() semaphore.Semaphore {
	if f.buildSem != nil {
		return f.buildSem
	}
	return semaphore.NewInMemory(4)
}

func (f *Functions) getRunSemaphore() semaphore.Semaphore {
	if f.runSem != nil {
		return f.runSem
	}
	return semaphore.NewInMemory(16)
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
