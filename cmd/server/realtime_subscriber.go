package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/lynx-go/lynx"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
)

// RealtimeSubscriberService 是 RealtimeFanout 的 lynx.Service 壳：
// Start 启动 Stream → Hub 消费循环（XAUTOCLAIM + XREADGROUP > → Dispatch
// → XACK → published_at），Stop 取消后处理完当前批再退。
type RealtimeSubscriberService struct {
	sub    shared.RealtimeFanout
	logger *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewRealtimeSubscriberService 构造实时订阅服务。
func NewRealtimeSubscriberService(sub shared.RealtimeFanout, logger *slog.Logger) *RealtimeSubscriberService {
	if logger == nil {
		logger = slog.Default()
	}
	return &RealtimeSubscriberService{sub: sub, logger: logger}
}

func (s *RealtimeSubscriberService) Name() string { return "realtime-subscriber" }

func (s *RealtimeSubscriberService) Init(ctx lynx.AppContext) error { return nil }

func (s *RealtimeSubscriberService) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Go(func() {
		if err := s.sub.Run(runCtx); err != nil && ctx.Err() == nil {
			s.logger.Error("realtime subscriber stopped with error", "error", err)
		}
	})

	// Start 必须阻塞到应用上下文取消（lynx 把 Start 作为 run.Group actor）。
	<-ctx.Done()
	return nil
}

func (s *RealtimeSubscriberService) Stop(ctx context.Context) error {
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
	s.logger.Info("realtime subscriber stopped")
	return nil
}
