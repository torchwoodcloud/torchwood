package functionsdispatcher

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/pkg/config"
)

// Service 是 functions-dispatcher 的 lynx 服务装配：HTTP API + reaper 周期
// 对账（独立二进制 cmd/functions-dispatcher；不进 server/worker wire）。
type Service struct {
	cfg    *config.AppConfig
	logger *slog.Logger
	pool   *PoolManager
	http   *http.Server

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewService 构造 dispatcher 服务（lynx actor）。
func NewService(cfg *config.AppConfig, rdb *redis.Client, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	daemon := NewDockerDaemon(cfg)
	pool := NewPoolManager(daemon, NewRedisRegistry(rdb), PoolConfigFromConfig(cfg))
	addr := cfg.GetFunctions().GetDispatcher().GetAddr()
	if addr == "" {
		addr = ":9070"
	}
	srv := newDispatchServer(pool, daemon, cfg.GetFunctions().GetDispatcher().GetSharedToken())
	s := &Service{
		cfg:    cfg,
		logger: logger,
		pool:   pool,
		http: &http.Server{
			Addr: addr,
			// 内网 API：显式拒绝慢速攻击面（读头超时）。
			ReadHeaderTimeout: 10 * time.Second,
			Handler:           srv,
		},
	}
	return s
}

// Name 实现 lynx.Service。
func (s *Service) Name() string { return "functions-dispatcher" }

// Init 实现 lynx.Service。
func (s *Service) Init(_ lynx.AppContext) error { return nil }

// Start 拉起 HTTP 监听与 reaper 周期对账；阻塞到 ctx 取消（lynx actor 契约）。
func (s *Service) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		cancel()
		return err
	}
	s.wg.Go(func() {
		if serveErr := s.http.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			s.logger.Error("dispatcher http server failed", "error", serveErr)
		}
	})
	s.wg.Go(func() {
		ticker := time.NewTicker(s.pool.cfg.ReaperInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				reapCtx, reapCancel := context.WithTimeout(context.WithoutCancel(runCtx), dockerCleanupTimeout)
				s.pool.Reaper(reapCtx)
				reapCancel()
			}
		}
	})
	s.logger.Info("functions-dispatcher started", "addr", s.http.Addr,
		"max_resident_instances", s.pool.cfg.MaxResidentInstances)

	<-ctx.Done()
	return nil
}

// Stop 优雅关停：先停 reaper，再排空在途 HTTP（drain 语义与全局一致）。
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, dockerCleanupTimeout)
	defer shutdownCancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		s.logger.Warn("dispatcher http shutdown", "error", err)
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
	s.logger.Info("functions-dispatcher stopped")
	return nil
}
