package bootkit

import (
	"context"
	"log/slog"
	"sync"

	"github.com/lynx-go/lynx"
)

// Runner 是可由 RunnerService 驱动的后台消费端口最小形态（如
// shared.RealtimeFanout、*infraevents.OutboxWorker）。
type Runner interface {
	Run(ctx context.Context) error
}

// RunnerService 把 Run(ctx) error 型后台消费端口包成 lynx.Service actor：
// Start 起协程跑 Run 并阻塞到应用上下文取消（lynx 把 Start 作为 run.Group
// actor），Stop 取消后等当前批处理完再退（有界排水）。
type RunnerService struct {
	name   string
	runner Runner
	logger *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewRunnerService 构造后台消费服务壳。
func NewRunnerService(name string, runner Runner, logger *slog.Logger) *RunnerService {
	if logger == nil {
		logger = slog.Default()
	}
	return &RunnerService{name: name, runner: runner, logger: logger}
}

func (s *RunnerService) Name() string { return s.name }

func (s *RunnerService) Init(ctx lynx.AppContext) error { return nil }

func (s *RunnerService) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Go(func() {
		if err := s.runner.Run(runCtx); err != nil && ctx.Err() == nil {
			s.logger.Error(s.name+" stopped with error", "error", err)
		}
	})

	// Start 必须阻塞到应用上下文取消（lynx 把 Start 作为 run.Group actor）。
	<-ctx.Done()
	return nil
}

func (s *RunnerService) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	s.logger.Info(s.name + " stopped")
	return nil
}
