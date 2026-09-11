package functions

import (
	"context"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

// recordAuditChanges 计算 UpdateFunction 前后的字段级 diff（仅变更字段），
// 经 contexts.SetAuditMetadata 交给审计拦截器落库为
// metadata.changes = {"字段": {"from": 旧值, "to": 新值}}——结构化、非文本，
// 供"CLI 把某函数的日调用限额从 1000 调到 5000"这类审计查询直接消费。
//
// 这是 changes 通道的试点（模式可推广到其它资源）：GetFunction 已先读旧值，
// diff 零额外查询；失败路径同样记录（变更尝试也是审计证据，行内 status
// 区分成败）。
func recordAuditChanges(ctx context.Context, before, after *domainfunctions.Function) {
	if before == nil || after == nil {
		return
	}
	changes := map[string]any{}
	put := func(field string, from, to any) {
		changes[field] = map[string]any{"from": from, "to": to}
	}
	if before.Name != after.Name {
		put("name", before.Name, after.Name)
	}
	if before.Entrypoint != after.Entrypoint {
		put("entrypoint", before.Entrypoint, after.Entrypoint)
	}
	if before.TimeoutSeconds != after.TimeoutSeconds {
		put("timeout_seconds", before.TimeoutSeconds, after.TimeoutSeconds)
	}
	if before.Spec != after.Spec {
		put("spec", before.Spec, after.Spec)
	}
	if before.Enabled != after.Enabled {
		put("enabled", before.Enabled, after.Enabled)
	}
	if before.ClientCallable != after.ClientCallable {
		put("client_callable", before.ClientCallable, after.ClientCallable)
	}
	if before.ClientAnonymousAllowed != after.ClientAnonymousAllowed {
		put("client_anonymous_allowed", before.ClientAnonymousAllowed, after.ClientAnonymousAllowed)
	}
	if before.ClientPerUserLimit != after.ClientPerUserLimit {
		put("client_per_user_limit", before.ClientPerUserLimit, after.ClientPerUserLimit)
	}
	if before.ClientLimitWindow != after.ClientLimitWindow {
		put("client_limit_window", before.ClientLimitWindow, after.ClientLimitWindow)
	}
	if before.MinInstances != after.MinInstances {
		put("min_instances", before.MinInstances, after.MinInstances)
	}
	if before.MaxInstances != after.MaxInstances {
		put("max_instances", before.MaxInstances, after.MaxInstances)
	}
	if before.IdleTTLSeconds != after.IdleTTLSeconds {
		put("idle_ttl_seconds", before.IdleTTLSeconds, after.IdleTTLSeconds)
	}
	if before.MaxRequestsPerInstance != after.MaxRequestsPerInstance {
		put("max_requests_per_instance", before.MaxRequestsPerInstance, after.MaxRequestsPerInstance)
	}
	if before.Concurrency != after.Concurrency {
		put("concurrency", before.Concurrency, after.Concurrency)
	}
	if len(changes) == 0 {
		return
	}
	contexts.SetAuditMetadata(ctx, "changes", changes)
}
