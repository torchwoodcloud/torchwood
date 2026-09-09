package contexts

import (
	"context"

	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
)

func WithPrincipal(ctx context.Context, p *shared.Principal) context.Context {
	return context.WithValue(ctx, ContextKeyPrincipal, p)
}

func Principal(ctx context.Context) (*shared.Principal, bool) {
	v := ctx.Value(ContextKeyPrincipal)
	p, ok := v.(*shared.Principal)
	return p, ok && p != nil
}

func WithProjectID(ctx context.Context, projectID string) context.Context {
	return context.WithValue(ctx, ContextKeyProjectID, projectID)
}

func ProjectID(ctx context.Context) (string, bool) {
	v := ctx.Value(ContextKeyProjectID)
	s, ok := v.(string)
	return s, ok && s != ""
}

// auditResourceHolder 是审计资源的可变持有者：audit 拦截器在请求 ctx 中预置
// 空持有者，handler 内的 WithAuditResource 通过 ctx 链查找并原地写入，使上游
// 中间件在 handler 返回后仍能读到（context 值不可变，跨函数边界共享需可变槽）。
type auditResourceHolder struct{ resource string }

// WithAuditResource attaches the resource id being acted upon (e.g. the
// project id targeted by a delete) so the audit interceptor can record it.
// 当 ctx 链中已有审计资源持有者（audit 拦截器预置）时原地写入，否则按
// 不可变方式派生新 context（无拦截器链路的直调场景）。
func WithAuditResource(ctx context.Context, resourceID string) context.Context {
	if h, ok := ctx.Value(ContextKeyAuditResource).(*auditResourceHolder); ok {
		h.resource = resourceID
		return ctx
	}
	return context.WithValue(ctx, ContextKeyAuditResource, resourceID)
}

// WithAuditResourceHolder pre-populates the mutable audit resource holder.
// 仅由 audit 拦截器调用；普通代码应使用 WithAuditResource。
func WithAuditResourceHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, ContextKeyAuditResource, &auditResourceHolder{})
}

// AuditResource returns the audit resource id stored in ctx, if any.
func AuditResource(ctx context.Context) string {
	if h, ok := ctx.Value(ContextKeyAuditResource).(*auditResourceHolder); ok {
		return h.resource
	}
	v, _ := ctx.Value(ContextKeyAuditResource).(string)
	return v
}
