package interceptor

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
)

// 通用 API 限流默认值（roadmap §3.4）：固定窗口，60s 量级。
const (
	defaultRateLimitWindow = 60 * time.Second
	defaultIPRateLimit     = 300  // per-IP：每分钟几百
	defaultUserRateLimit   = 1000 // per-user：每分钟千级
	defaultAPIKeyRateLimit = 6000 // per-API-key：每分钟数千
)

// 熔断降级参数（Round4 J5-1，产品决策 E-1：熔断短窗放行 + 观测分离）。
// 仅作用于通用 API 限流面；登录/MFA 等局部频控不经过本拦截器，
// 维持 fail-closed（部署文档声明 Redis 为登录面强依赖、需 HA）。
const (
	// rateLimitBreakerFailThreshold 是触发熔断的连续基础设施错误次数。
	rateLimitBreakerFailThreshold = 5
	// rateLimitBreakerFailWindow 是失败计数的滑动窗口：仅当阈值次错误
	// 全部落在该窗口内才熔断（零星抖动不熔断）。
	rateLimitBreakerFailWindow = 10 * time.Second
	// rateLimitBreakerOpenDuration 是熔断放行的短窗时长；窗口结束后进入
	// 半开态，下一次真实探测成功即恢复 closed。
	rateLimitBreakerOpenDuration = 30 * time.Second
)

// rateLimitExemptPrefixes 是限流豁免的 gRPC 框架内置服务（健康检查与
// reflection），避免探活/服务发现被业务限流打挂；与 server 侧
// authzExemptServicePrefixes 维持同一白名单。
var rateLimitExemptPrefixes = []string{
	"/grpc.health.v1.",
	"/grpc.reflection.",
}

// RateLimitInterceptor 按 API Key > user > IP 三个维度对经过 server gRPC
// 管道的所有业务 RPC 做通用限流（roadmap §3.4）：带 API Key principal 的
// 请求按 API Key 维度计数，带 user/session principal 的请求按 user 维度
// 计数，未认证请求按客户端 IP 计数；同一请求只按命中的一个维度计数。
// 与注册/匿名会话/MFA/OTP 发送 4 处局部限流的键空间不同，互不冲突、叠加生效。
type RateLimitInterceptor struct {
	limiter domainauth.RateLimiter
	enabled bool
	ip      rateLimitDimension
	user    rateLimitDimension
	apiKey  rateLimitDimension

	// 熔断器状态（Round4 J5-1）。brMu 保护下列字段；进程级单实例
	// （Redis 故障是全局的，不分维度熔断）。
	brMu        sync.Mutex
	brFails     int       // 当前滑动窗口内的连续基础设施错误次数
	brFirstFail time.Time // 窗口内首次错误时间（用于滑动窗口判定）
	brOpenUntil time.Time // 非零且在未来 = 熔断放行中；过期后进入半开探测
}

// rateLimitDimension 是单一维度的限流参数：key 前缀 + 窗口阈值。
type rateLimitDimension struct {
	keyPrefix string
	limit     int
	window    time.Duration
}

func (d rateLimitDimension) key(id string) string {
	return d.keyPrefix + id
}

// NewRateLimitInterceptor 从配置构造通用限流拦截器。security.rate_limit
// 未配置或部分配置时启用内置默认值（总开关默认 true）；limiter 复用
// domainauth.RateLimiter 端口（Redis 固定窗口实现）。
func NewRateLimitInterceptor(limiter domainauth.RateLimiter, cfg *config.AppConfig) *RateLimitInterceptor {
	rl := cfg.GetSecurity().GetRateLimit()
	// proto3 optional：仅显式配置为 false 才关闭，未设置视为开启（默认 true）。
	enabled := rl == nil || rl.Enabled == nil || rl.GetEnabled()
	return &RateLimitInterceptor{
		limiter: limiter,
		enabled: enabled,
		ip:      resolveRateLimitDimension("api:ip:", rl.GetIp(), defaultIPRateLimit),
		user:    resolveRateLimitDimension("api:user:", rl.GetUser(), defaultUserRateLimit),
		apiKey:  resolveRateLimitDimension("api:apikey:", rl.GetApiKey(), defaultAPIKeyRateLimit),
	}
}

// resolveRateLimitDimension 应用单维度配置，未配置/非法值回落默认值。
func resolveRateLimitDimension(keyPrefix string, d *config.Security_RateLimit_Dimension, defaultLimit int) rateLimitDimension {
	dim := rateLimitDimension{keyPrefix: keyPrefix, limit: defaultLimit, window: defaultRateLimitWindow}
	if d == nil {
		return dim
	}
	if d.GetLimit() > 0 {
		dim.limit = int(d.GetLimit())
	}
	if w := parseRateLimitWindow(d.GetWindow()); w > 0 {
		dim.window = w
	}
	return dim
}

// parseRateLimitWindow 解析窗口时长字符串（如 "60s"）；空/非法/非正返回 0。
func parseRateLimitWindow(s string) time.Duration {
	if s == "" {
		return 0
	}
	w, err := time.ParseDuration(s)
	if err != nil || w <= 0 {
		return 0
	}
	return w
}

func (r *RateLimitInterceptor) UnaryRateLimitMiddleware(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if !r.enabled || rateLimitExempt(info.FullMethod) {
		return handler(ctx, req)
	}
	dim, key := r.dimension(ctx)
	if dim == nil || key == "" {
		// 没有可用维度（如未认证且解析不到 IP）时不计数，放行给后续链路。
		return handler(ctx, req)
	}
	// 超限返回 ResourceExhausted（grpc-gateway 映射 HTTP 429），Redis 故障
	// 由端口实现返回 Internal。ResourceExhausted 统一携带
	// google.rpc.RetryInfo detail：端口实现已附精确剩余窗口时尊重原值，
	// 否则以本维度整窗口作为保守估计（Round4 J3-6）。
	//
	// 熔断降级（Round4 J5-1）：limiter 基础设施错误先判熔断——熔断放行中
	// 直接跳过 Redis 往返放行请求（fail-open），否则维持 fail-closed 透传
	// Internal；半开探测成功后回到正常判定。
	if r.breakerPassing() {
		return handler(ctx, req)
	}
	if err := r.limiter.Allow(ctx, key, dim.limit, dim.window); err != nil {
		return nil, r.onLimiterResult(ctx, err, dim.window)
	}
	r.onLimiterSuccess()
	return handler(ctx, req)
}

// onLimiterResult 区分正常拒绝与基础设施错误并维护熔断器：
//   - ResourceExhausted（窗口超限）：计 rejected 指标、透传，不影响熔断器
//     （能给出精确计数说明 Redis 健康）；
//   - 其他错误（Redis 故障等）：计 infra_error 指标并累计失败；未达熔断
//     条件时 fail-closed 透传原错误。
func (r *RateLimitInterceptor) onLimiterResult(ctx context.Context, err error, window time.Duration) error {
	if status.Code(err) == codes.ResourceExhausted {
		RateLimitRejectedTotal.Inc()
		return withRetryInfoFallback(err, window)
	}
	RateLimitInfraErrorTotal.Inc()
	if open := r.recordBreakerFailure(time.Now()); open {
		slog.ErrorContext(ctx, "rate limiter circuit breaker opened; failing open for "+
			"business RPCs (login/MFA throttles remain fail-closed)",
			"threshold", rateLimitBreakerFailThreshold,
			"window", rateLimitBreakerFailWindow.String(),
			"open_for", rateLimitBreakerOpenDuration.String())
	}
	return err
}

// withRetryInfoFallback 为无 RetryInfo detail 的 ResourceExhausted 错误补上
// 以 window 为建议退避的 detail；其他 code 或已有 detail 时原样返回。
func withRetryInfoFallback(err error, window time.Duration) error {
	if status.Code(err) != codes.ResourceExhausted || window <= 0 {
		return err
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	for _, d := range st.Details() {
		if _, has := d.(*errdetails.RetryInfo); has {
			return err
		}
	}
	enriched, derr := st.WithDetails(&errdetails.RetryInfo{
		RetryDelay: durationpb.New(window),
	})
	if derr != nil {
		return err
	}
	return enriched.Err()
}

// breakerPassing 报告熔断器当前是否处于「放行」状态：open 短窗内返回 true
// （调用方跳过 limiter 直接放行）；窗口过期后返回 false，让下一次请求以
// 真实 limiter 调用充当半开探测（成功即恢复 closed，见 onLimiterSuccess）。
func (r *RateLimitInterceptor) breakerPassing() bool {
	r.brMu.Lock()
	defer r.brMu.Unlock()
	return r.brOpenUntil.After(time.Now())
}

// recordBreakerFailure 记录一次 limiter 基础设施错误。返回 true 表示本次
// 调用开启了（或重新开启了）熔断放行短窗：
//   - closed 态：滑动窗口（10s）内连续错误达到阈值才开启——零星抖动不熔断；
//   - 半开态（短窗已过、探测请求真实调用了 limiter）：失败即重新开启，
//     不必重新累计阈值（经典断路器语义，避免每次窗口过期都漏过 N 个失败）。
//
// 触发本次记录的请求本身始终由调用方 fail-closed 拒绝。
func (r *RateLimitInterceptor) recordBreakerFailure(now time.Time) bool {
	r.brMu.Lock()
	defer r.brMu.Unlock()
	halfOpen := !r.brOpenUntil.IsZero() && !r.brOpenUntil.After(now)
	r.brFails++
	if halfOpen {
		r.brOpenUntil = now.Add(rateLimitBreakerOpenDuration)
		r.brFails = 0
		r.brFirstFail = time.Time{}
		return true
	}
	if r.brFails == 1 {
		r.brFirstFail = now
	} else if now.Sub(r.brFirstFail) > rateLimitBreakerFailWindow {
		// 错误未落在同一滑动窗口内：不足以判定「持续故障」，重新计数。
		r.brFails = 1
		r.brFirstFail = now
	}
	if r.brFails >= rateLimitBreakerFailThreshold {
		r.brOpenUntil = now.Add(rateLimitBreakerOpenDuration)
		r.brFails = 0
		r.brFirstFail = time.Time{}
		return true
	}
	return false
}

// onLimiterSuccess 在真实 limiter 调用成功后复位熔断器：清除失败计数并
// 关闭放行窗（半开探测成功即宣告恢复 closed，回到正常限流判定）。
func (r *RateLimitInterceptor) onLimiterSuccess() {
	r.brMu.Lock()
	defer r.brMu.Unlock()
	r.brFails = 0
	r.brFirstFail = time.Time{}
	r.brOpenUntil = time.Time{}
}

// dimension 按优先级选择限流维度：API Key principal > user/session
// principal > 客户端 IP。同一请求只命中一个维度（不做叠加计数）。
func (r *RateLimitInterceptor) dimension(ctx context.Context) (*rateLimitDimension, string) {
	if p, ok := contexts.Principal(ctx); ok && p != nil {
		if p.APIKeyID != "" {
			if id := string(p.ActorID); id != "" {
				return &r.apiKey, r.apiKey.key(id)
			}
			return &r.apiKey, r.apiKey.key(p.APIKeyID)
		}
		if id := string(p.ActorID); id != "" {
			return &r.user, r.user.key(id)
		}
		if p.UserID != "" {
			return &r.user, r.user.key(p.UserID)
		}
	}
	// 未认证（或 public 方法无 principal）：按客户端 IP 计数。IP 来自
	// ClientInfoInterceptor，已经过 trusted-proxy 校验。
	if ci := contexts.ClientInfoFrom(ctx); ci.IP != "" {
		return &r.ip, r.ip.key(ci.IP)
	}
	return nil, ""
}

func rateLimitExempt(fullMethod string) bool {
	for _, prefix := range rateLimitExemptPrefixes {
		if strings.HasPrefix(fullMethod, prefix) {
			return true
		}
	}
	return false
}
