package functions

import (
	"log/slog"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// Functions 是 Functions 服务的 use-case 聚合（v4：路由与身份层——
// docs/design/functions-v4.md §0。打包/构建/部署/池/运行时/变量已随执行面
// 外包整体退役；本聚合保留调用面（幂等/限频/并发闸）、触发器调度、
// twx_ 执行身份、invocation 记账与事件轴投递）。
type Functions struct {
	cfg *config.AppConfig
	// invoker 是 RPC 轴调用端口（v4 §4.1 JSON-RPC 2.0 over 签名 HTTP）。
	invoker domainfunctions.Invoker
	// notifier 是事件轴投递端口（v4 §4.2 CloudEvents 1.0——cron/事件触发）。
	notifier   domainfunctions.Notifier
	repo       domainfunctions.FunctionRepo
	queue      shared.Queue
	usage      domainbilling.UsageCounter // 可选：函数调用时长计量
	projects   projects.Repository        // 可选：启动对账枚举项目（nil 则 Recover 空操作）
	scanCursor appshared.ProjectRotation  // RecoverOrphanInvocations 轮转游标（串行）
	// execTokens 是执行身份铸造/吊销端口（P0 执行身份；可选：nil 时调用
	// 不注入 X-Tw-Execution-Token，函数无平台身份——declared_scopes 语义为
	// 增强而非执行前提）。
	execTokens domainfunctions.ExecutionTokenService
	// triggers 是函数触发器仓储（P1 触发器模块；nil 时触发器相关入口
	// fail-fast——由 Wire 装配，测试按需注入）。
	triggers domainfunctions.TriggerRepo
	// log 是包内 best-effort 日志（cron dispatch 失败等；nil 回落 slog.Default）。
	log *slog.Logger
	// cache 是函数 30s 进程内缓存（P0.5 热路径清账；见 cache.go）。
	cache *fnCache
	// clientQuota 是客户端调用面每用户限频端口（P2；nil 时跳过 Redis 直接
	// 走 DB 计数降级——语义仍 fail-closed，见 clientinvoke.go）。
	clientQuota domainfunctions.ClientQuotaLimiter
	// userGate 是每用户并发闸门（P2：进程内 keyed 信号量，默认每用户 8）。
	userGate *userGateLimiter
	// encKey 是 target 签名密钥的 secretbox 密钥材料（security.encryption_key，
	// 回退 jwt.secret——与 webhook 密钥 / OAuth client secret 同一派生入口）。
	encKey string
	// secretGrace 是 target 密钥轮换宽限窗（旧钥 v2 签名窗口；webhooks 同构
	// 语义）。<=0 回落领域缺省。
	secretGrace time.Duration
	// now 可注入（测试）；nil = time.Now。
	now func() time.Time
}

// NewFunctions 是 Wire 装配入口（server/worker 同构单构造）：注入双轴调用
// 端口（Invoker/Notifier）、仓储、队列与身份/限频端口。测试侧同包可直构
// struct 字面量或经本构造注入 fake。
func NewFunctions(cfg *config.AppConfig, invoker domainfunctions.Invoker, notifier domainfunctions.Notifier, repo domainfunctions.FunctionRepo, queue shared.Queue, usage domainbilling.UsageCounter, projectRepo projects.Repository, execTokens domainfunctions.ExecutionTokenService, triggers domainfunctions.TriggerRepo, clientQuota domainfunctions.ClientQuotaLimiter) *Functions {
	f := &Functions{
		cfg:         cfg,
		invoker:     invoker,
		notifier:    notifier,
		repo:        repo,
		queue:       queue,
		usage:       usage,
		projects:    projectRepo,
		execTokens:  execTokens,
		triggers:    triggers,
		clientQuota: clientQuota,
		cache:       newFnCache(),
		now:         time.Now,
	}
	f.initUserGate(cfg)
	f.initSecrets(cfg)
	return f
}

// initUserGate 初始化每用户并发闸门（P2）：config functions.client_invoke
// 未配置时取默认（每用户 8，队首超时 10s）。
func (f *Functions) initUserGate(cfg *config.AppConfig) {
	limit := int(cfg.GetFunctions().GetClientInvoke().GetPerUserConcurrency())
	if limit <= 0 {
		limit = defaultPerUserConcurrency
	}
	timeout := parseDurationValue(cfg.GetFunctions().GetClientInvoke().GetQueueHeadTimeout(), defaultUserQueueHeadTimeout)
	f.userGate = newUserGateLimiter(limit, timeout)
}

// initSecrets 初始化 target 签名密钥的加密材料与轮换宽限窗（模式照抄
// webhooks Service：EncryptionSecret 回退 jwt.secret 时启动告警）。
func (f *Functions) initSecrets(cfg *config.AppConfig) {
	if cfg == nil {
		return
	}
	key, fallback := config.EncryptionSecret(cfg)
	f.encKey = key
	if fallback {
		f.logger().Warn("functions: security.encryption_key not set; falling back to jwt.secret for target secret encryption")
	}
	f.secretGrace = parseDurationValue(cfg.GetFunctions().GetWebhooks().GetSecretGracePeriod(), domainfunctions.TargetSecretRotationWindow)
	if f.secretGrace <= 0 {
		f.secretGrace = domainfunctions.TargetSecretRotationWindow
	}
}

// logger 返回包内日志器（nil 安全）。
func (f *Functions) logger() *slog.Logger {
	if f.log != nil {
		return f.log
	}
	return slog.Default()
}

// nowFn 返回时钟（nil 安全）。
func (f *Functions) nowFn() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}
