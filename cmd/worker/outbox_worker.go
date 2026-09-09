package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/lynx-go/lynx"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
)

// OutboxWorkerService 是 OutboxWorker 的 lynx.Service 壳：Start 启动
// 领取循环（XADD 后只标 dispatched_at），Stop 取消后等当前轮结束。
type OutboxWorkerService struct {
	worker *infraevents.OutboxWorker
	logger *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewOutboxWorkerService 构造 outbox 领取服务。
func NewOutboxWorkerService(worker *infraevents.OutboxWorker, logger *slog.Logger) *OutboxWorkerService {
	if logger == nil {
		logger = slog.Default()
	}
	return &OutboxWorkerService{worker: worker, logger: logger}
}

func (s *OutboxWorkerService) Name() string { return "outbox-worker" }

func (s *OutboxWorkerService) Init(ctx lynx.AppContext) error { return nil }

func (s *OutboxWorkerService) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Go(func() {
		if err := s.worker.Run(runCtx); err != nil && ctx.Err() == nil {
			s.logger.Error("outbox worker stopped with error", "error", err)
		}
	})

	// Start 必须阻塞到应用上下文取消（lynx 把 Start 作为 run.Group actor）。
	<-ctx.Done()
	return nil
}

func (s *OutboxWorkerService) Stop(ctx context.Context) error {
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
	s.logger.Info("outbox worker stopped")
	return nil
}
