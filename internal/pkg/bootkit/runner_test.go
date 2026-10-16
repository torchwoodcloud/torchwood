package bootkit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeRunner struct {
	entered chan struct{}
	exited  chan struct{}
	ctxErr  error
}

func (f *fakeRunner) Run(ctx context.Context) error {
	close(f.entered)
	<-ctx.Done()
	f.ctxErr = ctx.Err()
	close(f.exited)
	return ctx.Err()
}

// 契约：Start 阻塞到应用上下文取消（run.Group actor）；Stop 取消 Run 的
// 上下文并有界等当前批退出；两步独立成立（排水路径先于应用取消）。
func TestRunnerServiceLifecycle(t *testing.T) {
	r := &fakeRunner{entered: make(chan struct{}), exited: make(chan struct{})}
	svc := NewRunnerService("test-runner", r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.Equal(t, "test-runner", svc.Name())

	appCtx, cancelApp := context.WithCancel(context.Background())
	startErr := make(chan error, 1)
	go func() { startErr <- svc.Start(appCtx) }()
	<-r.entered

	select {
	case err := <-startErr:
		t.Fatalf("Start 在应用上下文取消前就返回: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, svc.Stop(context.Background()))
	<-r.exited
	require.ErrorIs(t, r.ctxErr, context.Canceled)

	cancelApp()
	require.NoError(t, <-startErr)
}

func TestRunnerServiceStopWithoutStart(t *testing.T) {
	svc := NewRunnerService("idle", &fakeRunner{}, nil)
	require.NoError(t, svc.Stop(context.Background()))
}
