package contexts

import (
	"context"
	"testing"
)

// TestAuditResource_WriteThrough (A8)：audit 拦截器预置持有者后，handler 内
// WithAuditResource 的写入通过 ctx 链对上游中间件可见（context 不可变，
// 跨函数边界共享可变槽）；无持有者时保持不可变派生语义。
func TestAuditResource_WriteThrough(t *testing.T) {
	ctx := context.Background()
	if got := AuditResource(ctx); got != "" {
		t.Fatalf("empty ctx should yield empty resource, got %q", got)
	}

	// 无持有者：不可变派生，原 ctx 不受影响。
	derived := WithAuditResource(ctx, "databases/app")
	if got := AuditResource(derived); got != "databases/app" {
		t.Fatalf("derived ctx should carry resource, got %q", got)
	}
	if got := AuditResource(ctx); got != "" {
		t.Fatalf("original ctx must stay empty, got %q", got)
	}

	// 有持有者（拦截器预置）：handler 内写入对上游可见，且不换 ctx 实例。
	interceptorCtx := WithAuditResourceHolder(ctx)
	handlerCtx := WithAuditResource(interceptorCtx, "databases/app/collections/posts/documents/doc-1")
	if handlerCtx != interceptorCtx {
		t.Fatal("write-through must reuse the same context instance")
	}
	if got := AuditResource(interceptorCtx); got != "databases/app/collections/posts/documents/doc-1" {
		t.Fatalf("interceptor should observe handler-written resource, got %q", got)
	}
}
