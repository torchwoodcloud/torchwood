package interceptor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/torchwooddev/torchwood/internal/domain/audit"
	"google.golang.org/grpc"
)

// auditTestRepo 在 Insert 入口捕获 ctx 状态（时间/截止），可阻塞/注入错误。
type auditTestRepo struct {
	gotCtx      context.Context
	errAtInsert error
	insertedAt  time.Time
	hasDeadline bool
	deadline    time.Time
	block       bool
	insertErr   error
}

func (r *auditTestRepo) Insert(ctx context.Context, _ *audit.Entry) error {
	r.gotCtx = ctx
	r.errAtInsert = ctx.Err()
	r.insertedAt = time.Now()
	if d, ok := ctx.Deadline(); ok {
		r.hasDeadline = true
		r.deadline = d
	}
	if r.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return r.insertErr
}

func (r *auditTestRepo) ListByActor(context.Context, string, string, int) ([]audit.Entry, error) {
	return nil, nil
}

func runAuditMiddleware(repo *auditTestRepo, callerCtx context.Context) (any, error) {
	a := NewAuditInterceptor(repo)
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Ok"}
	return a.UnaryAuditMiddleware(callerCtx, nil, info, func(context.Context, any) (any, error) {
		return "resp", nil
	})
}

// TestAuditInterceptor_InsertHasTimeout（R01-F7-6）：审计落库必须带
// 3s 超时 ctx；repo 阻塞时 RPC 响应不受影响、按时返回。
func TestAuditInterceptor_InsertHasTimeout(t *testing.T) {
	repo := &auditTestRepo{block: true}
	start := time.Now()
	resp, err := runAuditMiddleware(repo, context.Background())
	requireNoError(t, err)
	if resp != "resp" {
		t.Fatalf("expected handler response, got %v", resp)
	}

	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Fatalf("audit insert must not block RPC beyond 3s, took %v", elapsed)
	}
	if repo.gotCtx == nil || !repo.hasDeadline {
		t.Fatal("audit insert context must carry a 3s deadline")
	}
	if d := repo.deadline.Sub(repo.insertedAt); d < 2500*time.Millisecond || d > 3500*time.Millisecond {
		t.Fatalf("audit insert deadline must be ~3s, got %v", d)
	}
}

// TestAuditInterceptor_InsertErrorDoesNotAffectRPC（R01-F7-6）：落库失败只
// Warn，不得改变 RPC 响应/错误。
func TestAuditInterceptor_InsertErrorDoesNotAffectRPC(t *testing.T) {
	repo := &auditTestRepo{insertErr: errors.New("db down")}
	resp, err := runAuditMiddleware(repo, context.Background())
	requireNoError(t, err)
	if resp != "resp" {
		t.Fatalf("expected handler response, got %v", resp)
	}
}

// TestAuditInterceptor_InsertIgnoresCallerCancel（R01-F7-6）：WithTimeout
// 必须基于 WithoutCancel——调用方取消不得连带取消审计落库（否则长 RPC
// 的审计日志会随客户端断开而丢失）。errAtInsert 在 Insert 入口捕获，
// 避免受中间件退出时 defer cancel() 的影响。
func TestAuditInterceptor_InsertIgnoresCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := &auditTestRepo{block: false}
	resp, err := runAuditMiddleware(repo, ctx)
	requireNoError(t, err)
	if resp != "resp" {
		t.Fatalf("expected handler response, got %v", resp)
	}
	if repo.gotCtx == nil || repo.errAtInsert != nil {
		t.Fatal("audit insert context must not inherit caller cancellation")
	}
}
