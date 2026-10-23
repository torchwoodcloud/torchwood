package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/grpcapi/server"
	"github.com/lynx-go/lynx"
	"google.golang.org/grpc"
	grpchealth "google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

// GRPCAPIService 是 grpcapi server.New 装配产物到 lynx 生命周期的适配器
// （lynx 深度集成）：grpc.Server 由 server.New 装配（服务注册、拦截器链、
// 启动期断言），传输层生命周期（listener/排水/健康轮询）按 lynxgrpc 语义
// 自管——lynxgrpc 不收预构建实例，故以裸 server 形态交接（库
// GRPCServer() 文档约定）。健康轮询语义对齐 lynxgrpc.WithHealthCheckers
// （10s 周期快照、逐 checker 限时并发、聚合到 "" 与 "grpc" 服务名）。
type GRPCAPIService struct {
	name     string
	addr     string
	gs       *grpc.Server
	api      *server.Server
	health   *grpchealth.Server
	checkers func() []lynx.Checker
	logger   *slog.Logger
	// timeout 是 GracefulStop 的上限（与 lynxgrpc WithTimeout 同源：
	// server.grpc.timeout，默认 30s）。
	timeout time.Duration

	started       atomic.Bool
	running       atomic.Bool
	stopRequested atomic.Bool
	ready         chan struct{}
	readyOnce     sync.Once
	mu            sync.Mutex
	listener      net.Listener
	pollCancel    context.CancelFunc
}

var _ lynx.Service = (*GRPCAPIService)(nil)

func NewGRPCAPIService(name, addr string, api *server.Server, gs *grpc.Server, checkers func() []lynx.Checker, timeout time.Duration, logger *slog.Logger) *GRPCAPIService {
	return &GRPCAPIService{
		name:     name,
		addr:     addr,
		gs:       gs,
		api:      api,
		health:   grpchealth.NewServer(),
		checkers: checkers,
		logger:   logger,
		timeout:  timeout,
		ready:    make(chan struct{}),
	}
}

func (s *GRPCAPIService) Name() string { return s.name }

// Init 复位一次性守卫（lynx 生命周期契约；对齐 lynxgrpc 的重启语义留口）。
func (s *GRPCAPIService) Init(_ lynx.AppContext) error {
	s.started.Store(false)
	s.stopRequested.Store(false)
	return nil
}

// Ready 在监听就绪后关闭（lynx ordered services 等待点；对齐 lynxgrpc）。
func (s *GRPCAPIService) Ready() <-chan struct{} { return s.ready }

// CheckHealth 实现健康检查：未运行即不健康。
func (s *GRPCAPIService) CheckHealth() error {
	if !s.running.Load() {
		return grpc.ErrServerStopped
	}
	return nil
}

func (s *GRPCAPIService) Start(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("grpc api service: Start called more than once")
	}
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		s.started.Store(false)
		return err
	}
	s.logger.InfoContext(ctx, "starting gRPC server, listening on "+lis.Addr().String())
	s.mu.Lock()
	s.listener = lis
	s.mu.Unlock()

	// lynx 风格健康面：自注册 grpc.health.v1（server.New 以 DisableHealth
	// 禁用了它的内置 health，避免双重注册），"" 与 "grpc" 恒 SERVING、
	// 依赖健康经轮询聚合（对齐 lynxgrpc.WithHealthCheckers 语义）。
	grpc_health_v1.RegisterHealthServer(s.gs, s.health)
	s.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	s.health.SetServingStatus(s.name, grpc_health_v1.HealthCheckResponse_SERVING)
	s.startHealthPoller()

	s.readyOnce.Do(func() { close(s.ready) })
	s.running.Store(true)
	defer s.running.Store(false)

	serveErr := s.gs.Serve(lis)
	// 正常关停：Stop 先关 listener 再 GracefulStop，Accept 因 listener 已
	// 关闭而失败——关停流程的一部分而非启动失败，归一化为 nil。
	if s.stopRequested.Load() && serveErr != nil {
		return nil
	}
	return serveErr
}

func (s *GRPCAPIService) Stop(ctx context.Context) error {
	s.logger.InfoContext(ctx, "stopping gRPC server")
	s.running.Store(false)
	s.stopRequested.Store(true)
	s.mu.Lock()
	lis := s.listener
	s.listener = nil
	if s.pollCancel != nil {
		s.pollCancel()
		s.pollCancel = nil
	}
	s.mu.Unlock()
	if lis != nil {
		if err := lis.Close(); err != nil {
			s.logger.ErrorContext(ctx, "error closing gRPC listener", "error", err)
		}
	}
	s.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	s.health.SetServingStatus(s.name, grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	// GracefulStop 上限 = min(ctx 剩余预算, timeout)（对齐 lynxgrpc）。
	stopCtx := ctx
	if s.timeout > 0 {
		var cancel context.CancelFunc
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := time.Until(deadline); remaining < s.timeout {
				stopCtx, cancel = context.WithTimeout(ctx, remaining)
			} else {
				stopCtx, cancel = context.WithTimeout(ctx, s.timeout)
			}
		} else {
			stopCtx, cancel = context.WithTimeout(ctx, s.timeout)
		}
		defer cancel()
	}
	done := make(chan struct{})
	go func() {
		s.gs.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-stopCtx.Done():
		s.gs.Stop() // 预算耗尽：强制关停，不放大排水窗口
		return fmt.Errorf("grpc api service: graceful stop timed out: %w", stopCtx.Err())
	}
}

// startHealthPoller 轮询依赖 checker 并聚合到健康服务（10s 周期，逐
// checker 5s 限时并发——挂死的 checker 按超时不健康，状态不冻结）。
func (s *GRPCAPIService) startHealthPoller() {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.pollCancel = cancel
	s.mu.Unlock()
	go func() {
		s.updateHealthStatus(ctx)
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.updateHealthStatus(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *GRPCAPIService) updateHealthStatus(ctx context.Context) {
	status := grpc_health_v1.HealthCheckResponse_SERVING
	if err := checkDependencies(ctx, s.checkers()); err != nil {
		status = grpc_health_v1.HealthCheckResponse_NOT_SERVING
	}
	s.health.SetServingStatus("", status)
	s.health.SetServingStatus(s.name, status)
}

// checkDependencies 并发执行 checker，任一失败（或超时/panic）即不健康。
func checkDependencies(ctx context.Context, checkers []lynx.Checker) error {
	if len(checkers) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	errCh := make(chan error, len(checkers))
	for _, c := range checkers {
		go func(c lynx.Checker) {
			errCh <- func() (err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("health checker %T panicked: %v", c, r)
					}
				}()
				return c.CheckHealth()
			}()
		}(c)
	}
	for range checkers {
		select {
		case err := <-errCh:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
