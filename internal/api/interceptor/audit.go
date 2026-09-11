package interceptor

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// auditFrameworkPrefixes 是不落审计的 gRPC 框架内置服务（监控/探针高频
// 轮询，纯噪声）。
var auditFrameworkPrefixes = []string{
	"/grpc.health.v1.",
	"/grpc.reflection.",
}

// auditSilentClientMethods 是 client 面 AccountService 中的高频无害动作：
// 例行 token 刷新与偏好读写是"日常无害操作"（每活跃用户 15 分钟一次的
// RefreshToken 是最大的单点噪声源），不构成审计事件——异常场景（失效/
// 被盗凭证）由 auth 层拒绝审计覆盖。Me 是高频自查读（动词不带读前缀，
// 显式列出）。
var auditSilentClientMethods = map[string]bool{
	"/torchwood.client.v1.AccountService/RefreshToken": true,
	"/torchwood.client.v1.AccountService/GetPrefs":     true,
	"/torchwood.client.v1.AccountService/UpdatePrefs":  true,
	"/torchwood.client.v1.AccountService/Me":           true,
}

// auditSilentServerMethods 是 server/console 面显式登记的高频写动作豁免
// （与 auditSilentClientMethods 同一纪律：非读动词默认落审计，豁免必须显式
// 登记——新增即护栏测试同步）。首例：Analytics 摄入是事件数据通道而非
// 管理变更（D13，docs/design/analytics.md；限流默认档允许单用户
// 1000 请求/min × 批 100 事件，落审计即纯噪声且体量碾压一切管理动作——
// 事件的业务载体是 analytics_events 表本身）。
var auditSilentServerMethods = map[string]bool{
	"/torchwood.server.v1.AnalyticsService/IngestEvents": true,
}

// auditRowEligible 判定一次 unary 调用是否落 audit_logs 行（噪声治理：
// 日常无害操作不进审计——量大且无安全价值）：
//   - 框架内置服务（grpc.health.v1/grpc.reflection）：不记；
//   - 管理面（server.v1/console.v1）：仅非读方法（读方法=Console/CLI 的
//     日常浏览查询，噪声），auditSilentServerMethods 显式登记者除外（D13）；
//   - client 面：仅 AccountService 的非读安全动作（登录/登出/账号与凭证
//     变更——端用户账号日志 GET /v1/account/logs 的数据来源）。其余
//     client 服务（文档/函数执行/支付/资产等数据面）的审计载体是事件流
//     与业务记录（如 function_executions），不在此重复记账；
//   - 未知命名空间：偏向多记（未来新增面默认可审计，豁免需显式登记）。
//
// 拒绝（auth 层 writeDenyAudit）与登录限速审计不经此门，全部保留——
// 它们是安全信号而非噪声。
func auditRowEligible(fullMethod string) bool {
	for _, prefix := range auditFrameworkPrefixes {
		if strings.HasPrefix(fullMethod, prefix) {
			return false
		}
	}
	_, verb := splitFullMethod(fullMethod)
	switch {
	case strings.HasPrefix(fullMethod, "/torchwood.server.v1."), strings.HasPrefix(fullMethod, "/torchwood.console.v1."):
		if auditSilentServerMethods[fullMethod] {
			return false
		}
		return !isReadVerb(verb)
	case strings.HasPrefix(fullMethod, "/torchwood.client.v1."):
		if !strings.HasPrefix(fullMethod, "/torchwood.client.v1.AccountService/") || auditSilentClientMethods[fullMethod] {
			return false
		}
		return !isReadVerb(verb)
	default:
		return true
	}
}

type AuditInterceptor struct {
	repo    audit.Repository
	logger  *slog.Logger
	trusted *TrustedProxies
}

func NewAuditInterceptor(repo audit.Repository) *AuditInterceptor {
	return &AuditInterceptor{repo: repo, logger: slog.Default()}
}

// WithLogger 替换审计写入失败告警所用的 logger（默认 slog.Default()），返回自身便于链式调用。
func (a *AuditInterceptor) WithLogger(l *slog.Logger) *AuditInterceptor {
	if l != nil {
		a.logger = l
	}
	return a
}

// WithTrustedProxies 配置可信代理网段，用于回退路径的 X-Forwarded-For 解析
// （正常链路 ClientInfo 已携带校验后 IP，此配置仅在无 ClientInfo 时生效，避免伪造）。
func (a *AuditInterceptor) WithTrustedProxies(trusted *TrustedProxies) *AuditInterceptor {
	a.trusted = trusted
	return a
}

func (a *AuditInterceptor) UnaryAuditMiddleware(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	// 噪声治理准入：日常无害操作（读浏览/框架探针/数据面高频）不落审计行
	// （判定语义见 auditRowEligible）。原 InvokeFunction 豁免（审计载体=
	// function_executions）已被 client 面规则覆盖。
	if !auditRowEligible(info.FullMethod) {
		return handler(ctx, req)
	}
	// 预置审计资源/元数据可变持有者：handler 内的 WithAuditResource/
	// SetAuditMetadata 原地写入后，本中间件在 handler 返回后仍能读取
	// （context 值不可变，需共享可变槽）。
	ctx = contexts.WithAuditResourceHolder(ctx)
	ctx = contexts.WithAuditMetadataHolder(ctx)
	resp, err := handler(ctx, req)
	if a.repo == nil {
		return resp, err
	}

	entry := &audit.Entry{
		Action: info.FullMethod,
		Status: auditStatus(err),
	}
	var principalUA string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		// 优先使用 ClientInfoInterceptor 写入的、经过 trusted-proxy 校验的 IP；
		// 仅在链路中没有 ClientInfo（如测试直挂 audit）时退化为直接读头部。
		if ci := contexts.ClientInfoFrom(ctx); ci.IP != "" || ci.UserAgent != "" {
			entry.IP = ci.IP
			entry.UserAgent = ci.UserAgent
		} else {
			xff := firstMetadataValue(md, "x-forwarded-for")
			if xff == "" {
				xff = firstMetadataValue(md, "grpcgateway-x-forwarded-for")
			}
			realIP := firstMetadataValue(md, "x-real-ip")
			if a.trusted != nil {
				entry.IP = a.trusted.ResolveClientIP(PeerIP(ctx), xff, realIP)
			} else if peerIP := PeerIP(ctx); peerIP != "" {
				entry.IP = peerIP
			} else {
				// 无 peer 且无可信配置（测试直调）时不信任 XFF，回退为空，避免伪造。
				entry.IP = ""
			}
			entry.UserAgent = firstMetadataValue(md, "grpcgateway-user-agent")
			if entry.UserAgent == "" {
				entry.UserAgent = firstMetadataValue(md, "user-agent")
			}
		}
		principalUA = entry.UserAgent
	}
	if p, ok := contexts.Principal(ctx); ok && p != nil {
		entry.ActorID = string(p.ActorID)
		entry.ActorKind = string(p.ActorKind)
		entry.ProjectID = p.ProjectID
		if client := auditClientChannel(p, principalUA); len(client) > 0 {
			entry.SetMetadata("client", client)
		}
	}
	if resID := contexts.AuditResource(ctx); resID != "" {
		entry.ResourceID = resID
	}
	// 管理面写操作：脱敏请求摘要（成功/失败均记——被拒的变更尝试同样
	// 是审计证据；protojson presence 语义使更新类请求只含被改字段）。
	if summary := auditRequestSummary(info.FullMethod, req); summary != "" {
		entry.SetMetadata("request", summary)
	}
	// app 用例回填的结构化扩展（试点：Functions Update 的 before/after diff）。
	for k, v := range contexts.AuditMetadata(ctx) {
		entry.SetMetadata(k, v)
	}
	// R01-F7-6：审计落库带 3s 超时且不继承 RPC 取消（WithoutCancel），
	// 失败只 Warn，不得阻塞或影响 RPC 响应。
	insertCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if logErr := a.repo.Insert(insertCtx, entry); logErr != nil {
		a.logger.WarnContext(ctx, "audit log insert failed",
			slog.String("method", info.FullMethod),
			slog.String("error", logErr.Error()))
	}
	return resp, err
}

func auditStatus(err error) string {
	if err == nil {
		return "success"
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		return st.Code().String()
	}
	return "error"
}
