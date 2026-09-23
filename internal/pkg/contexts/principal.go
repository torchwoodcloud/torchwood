package contexts

import (
	"context"

	"github.com/lynx-go/grpcapi/contextx"
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

// WithAuditResource 预置审计资源可变槽（grpcapi 阶段 1 门面化）：存储机制
// 委托 grpcapi/contextx.AuditTrail（与库生态共享同一 ctx 槽），公开签名与
// 语义保持不变——ctx 链中已有审计槽（audit 拦截器预置）时原地写入并返回
// 原 ctx，否则按不可变方式派生新 context（无拦截器链路的直调场景）。
func WithAuditResource(ctx context.Context, resourceID string) context.Context {
	if t := contextx.AuditTrailFrom(ctx); t != nil {
		contextx.SetAuditResource(ctx, resourceID)
		return ctx
	}
	return context.WithValue(ctx, ContextKeyAuditResource, resourceID)
}

// WithAuditResourceHolder pre-populates the mutable audit resource holder.
// 仅由 audit 拦截器调用；普通代码应使用 WithAuditResource。
func WithAuditResourceHolder(ctx context.Context) context.Context {
	return contextx.WithAuditTrail(ctx, contextx.NewAuditTrail())
}

// AuditResource returns the audit resource id stored in ctx, if any.
func AuditResource(ctx context.Context) string {
	if t := contextx.AuditTrailFrom(ctx); t != nil {
		return t.Resource()
	}
	v, _ := ctx.Value(ContextKeyAuditResource).(string)
	return v
}

// auditMetadataHolder 是审计扩展元数据的可变持有者（本地桥接层，grpcapi
// 阶段 1 门面化）：库版 contextx.AuditTrail 的元数据为 map[string]string 且
// setter 无返回值（无槽 no-op），与 torchwood 的 map[string]any 取值 +
// 无槽时不可变派生回退语义有出入（audit_diff 等用例回填结构化 map、直调
// 场景依赖派生语义），故元数据槽保留本地实现——不改库（DESIGN §6 裁决 6
// 的桥接授权）。
type auditMetadataHolder struct{ metadata map[string]any }

func (h *auditMetadataHolder) set(key string, value any) {
	if h.metadata == nil {
		h.metadata = map[string]any{}
	}
	h.metadata[key] = value
}

// SetAuditMetadata 记录一个结构化审计扩展键（保留键：changes、resource_name）。
// 当 ctx 链中已有持有者（audit 拦截器预置）时原地写入，否则按不可变方式
// 派生新 context（无拦截器链路的直调场景）。
func SetAuditMetadata(ctx context.Context, key string, value any) context.Context {
	if h, ok := ctx.Value(ContextKeyAuditMetadata).(*auditMetadataHolder); ok {
		h.set(key, value)
		return ctx
	}
	h := &auditMetadataHolder{}
	h.set(key, value)
	return context.WithValue(ctx, ContextKeyAuditMetadata, h)
}

// WithAuditMetadataHolder pre-populates the mutable audit metadata holder.
// 仅由 audit 拦截器调用；普通代码应使用 SetAuditMetadata。
func WithAuditMetadataHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, ContextKeyAuditMetadata, &auditMetadataHolder{metadata: map[string]any{}})
}

// AuditMetadata returns the audit extension metadata stored in ctx (may be nil)。
func AuditMetadata(ctx context.Context) map[string]any {
	if h, ok := ctx.Value(ContextKeyAuditMetadata).(*auditMetadataHolder); ok {
		return h.metadata
	}
	return nil
}
