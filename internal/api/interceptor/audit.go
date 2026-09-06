package interceptor

import (
	"context"
	"log/slog"
	"time"

	"github.com/torchwooddev/torchwood/internal/domain/audit"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

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
	// 预置审计资源可变持有者：handler 内的 WithAuditResource 原地写入后，
	// 本中间件在 handler 返回后仍能读取（context 值不可变，需共享可变槽）。
	ctx = contexts.WithAuditResourceHolder(ctx)
	resp, err := handler(ctx, req)
	if a.repo == nil {
		return resp, err
	}

	entry := &audit.Entry{
		Action: info.FullMethod,
		Status: auditStatus(err),
	}
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
	}
	if p, ok := contexts.Principal(ctx); ok && p != nil {
		entry.ActorID = string(p.ActorID)
		entry.ActorKind = string(p.ActorKind)
		entry.ProjectID = p.ProjectID
	}
	if resID := contexts.AuditResource(ctx); resID != "" {
		entry.ResourceID = resID
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
