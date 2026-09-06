package interceptor

import (
	"context"
	"log/slog"
	"strings"
	"time"

	domainbilling "github.com/torchwooddev/torchwood/internal/domain/billing"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
)

// usageExemptPrefixes 是不计入 API 调用次数的 gRPC 框架服务（健康检查 /
// reflection）。Realtime 走 WebSocket，不经过本拦截器（D18 不计量）。
var usageExemptPrefixes = []string{
	"/grpc.health.v1.",
	"/grpc.reflection.",
}

// UsageMeter 是用量计数端口（由 domain/billing.UsageCounter 满足）。
type UsageMeter interface {
	Incr(ctx context.Context, projectID, metric string, delta int64) error
}

// UsageInterceptor 在鉴权之后按 project 对业务 RPC 计数（设计 §4.1：
// API 调用次数）。Redis 故障不影响请求（best-effort）。
type UsageInterceptor struct {
	meter  UsageMeter
	logger *slog.Logger
}

// NewUsageInterceptor 构造 API 调用计量拦截器；meter 为 nil 时放行且不计数。
func NewUsageInterceptor(meter UsageMeter) *UsageInterceptor {
	return &UsageInterceptor{meter: meter, logger: slog.Default()}
}

// WithLogger 替换计量失败所用 logger。
func (u *UsageInterceptor) WithLogger(l *slog.Logger) *UsageInterceptor {
	if l != nil {
		u.logger = l
	}
	return u
}

// UnaryUsageMiddleware 在 handler 返回后对带 project 的业务 RPC +1。
func (u *UsageInterceptor) UnaryUsageMiddleware(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	u.observe(ctx, info.FullMethod)
	return resp, err
}

func (u *UsageInterceptor) observe(ctx context.Context, fullMethod string) {
	if u == nil || u.meter == nil || usageExempt(fullMethod) {
		return
	}
	projectID := projectIDFromCtx(ctx)
	if projectID == "" {
		return
	}
	meterCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 200*time.Millisecond)
	defer cancel()
	if err := u.meter.Incr(meterCtx, projectID, domainbilling.MetricAPICalls, 1); err != nil && u.logger != nil {
		u.logger.WarnContext(ctx, "usage meter incr failed",
			slog.String("method", fullMethod),
			slog.String("project_id", projectID),
			slog.String("error", err.Error()),
		)
	}
}

func projectIDFromCtx(ctx context.Context) string {
	if p, ok := contexts.Principal(ctx); ok && p != nil && p.ProjectID != "" {
		return p.ProjectID
	}
	if id, ok := contexts.ProjectID(ctx); ok {
		return id
	}
	return ""
}

func usageExempt(fullMethod string) bool {
	for _, prefix := range usageExemptPrefixes {
		if strings.HasPrefix(fullMethod, prefix) {
			return true
		}
	}
	return false
}
