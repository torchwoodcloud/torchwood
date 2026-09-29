package dispatcher

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// Service 是 dispatcher 的 lynx 服务装配：HTTP API + reaper 周期对账
// （独立二进制 cmd/dispatcher；不进 server/worker wire）。执行底座按
// functions.driver 选择（IMPL-T2-5 双执行底座）：fleetly Tasks/build API
// 或 docker 直接执行（per-project bridge 网络 + 本地构建）；池语义两形态
// 共用。
type Service struct {
	cfg    *config.AppConfig
	logger *slog.Logger
	pool   *PoolManager
	http   *http.Server

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewService 构造 dispatcher 服务（lynx actor）。执行底座按
// functions.driver 选择（IMPL-T2-5 双执行底座：fleetly 平台 / docker 直接
// 执行），选择失败（驱动未设/未知/docker 驱动未链接）即拒绝启动。
func NewService(cfg *config.AppConfig, rdb *redis.Client, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	registry := NewRedisRegistry(rdb)
	daemon, err := newDaemonForConfig(cfg, registry)
	if err != nil {
		return nil, err
	}
	pool := NewPoolManager(daemon, registry, PoolConfigFromConfig(cfg))
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
	return s, nil
}

// Name 实现 lynx.Service。
func (s *Service) Name() string { return "dispatcher" }

// Init 实现 lynx.Service。
func (s *Service) Init(_ lynx.AppContext) error { return nil }

// Start 拉起 HTTP 监听与 reaper 周期对账；阻塞到 ctx 取消（lynx actor 契约）。
func (s *Service) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	// ListenConfig.Listen（noctx）：ctx 取消即关监听，与 Stop 的 http.Shutdown
	// 双保险收口。
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.http.Addr)
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
				reapCtx, reapCancel := context.WithTimeout(context.WithoutCancel(runCtx), CleanupTimeout)
				s.pool.Reaper(reapCtx)
				reapCancel()
			}
		}
	})
	s.logger.Info("dispatcher started", "addr", s.http.Addr,
		"driver", s.cfg.GetFunctions().GetDriver(),
		"fleetly_endpoint", s.cfg.GetFunctions().GetFleetly().GetEndpoint(),
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
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, CleanupTimeout)
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
	s.logger.Info("dispatcher stopped")
	return nil
}
