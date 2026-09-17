package functionspacker

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/lynx-go/lynx"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// shutdownDrain 是 HTTP 在途请求的关停排水预算：packer 无状态，在途 pack
// 结果调用方（server）可整请求重试，短排水即可；上限受 lynx 进程级关停
// 预算约束。
const shutdownDrain = 15 * time.Second

// Service 是 functions-packer 的 lynx 服务装配：单 HTTP API 面（独立二进制
// cmd/functions-packer；不进 server/worker wire）。无 Redis/DB/docker 依赖
// ——进程可独立重启/扩缩，OOM 只影响 git 部署自身（设计 §2 资源画像）。
// server 与 packer 两进程共用同一 config schema：本进程只消费
// functions.packer 的 addr/shared_token 及预算字段，不消费 url（那是
// server 侧寻址本服务的基址）。
type Service struct {
	cfg    *config.AppConfig
	logger *slog.Logger
	http   *http.Server
	// 派生自 config 的运行参数（归一后），仅供启动日志与观测。
	concurrency  int
	fetchTimeout time.Duration

	wg sync.WaitGroup
}

// NewService 构造 packer 服务（lynx actor）：解析预算、安装带 SSRF 防护
// 的 go-git http/https 传输（进程级全局，见 installGuardedHTTPTransport）、
// 组装并发自限的 packServer。
func NewService(cfg *config.AppConfig, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	pc := cfg.GetFunctions().GetPacker()

	opts := PackOptions{
		MaxRepoBytes:  pc.GetMaxRepoBytes(),
		MaxZipBytes:   pc.GetMaxZipBytes(),
		FetchTimeout:  parseDuration(pc.GetFetchTimeout()),
		AllowInsecure: pc.GetAllowInsecure(),
	}
	opts = normalizePackOptions(opts)
	concurrency := int(pc.GetConcurrency())
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}

	addr := pc.GetAddr()
	if addr == "" {
		addr = defaultAddr
	}
	installGuardedHTTPTransport(opts.AllowInsecure)
	srv := newPackServer(pc.GetSharedToken(), opts, concurrency)
	return &Service{
		cfg:          cfg,
		logger:       logger,
		http: &http.Server{
			Addr: addr,
			// 内网 API：显式拒绝慢速攻击面（读头超时）。
			ReadHeaderTimeout: 10 * time.Second,
			Handler:           srv,
		},
		concurrency:  concurrency,
		fetchTimeout: opts.FetchTimeout,
	}
}

// parseDuration 解析 config 时长串；空/非法/非正值回落缺省（与
// config.proto「空/非法值回落默认」注释一致）。
func parseDuration(raw string) time.Duration {
	if raw == "" {
		return 0 // 归一到缺省交给 normalizePackOptions
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// Name 实现 lynx.Service。
func (s *Service) Name() string { return "functions-packer" }

// Init 实现 lynx.Service。
func (s *Service) Init(_ lynx.AppContext) error { return nil }

// Start 拉起 HTTP 监听；阻塞到 ctx 取消（lynx actor 契约）。
func (s *Service) Start(ctx context.Context) error {
	// ListenConfig.Listen（noctx）：ctx 取消即关监听，与 Stop 的
	// http.Shutdown 双保险收口——无需独立 cancel 机制。
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.http.Addr)
	if err != nil {
		return err
	}
	s.wg.Go(func() {
		if serveErr := s.http.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			s.logger.Error("packer http server failed", "error", serveErr)
		}
	})
	s.logger.Info("functions-packer started", "addr", s.http.Addr,
		"concurrency", s.concurrency, "fetch_timeout", s.fetchTimeout)

	<-ctx.Done()
	return nil
}

// Stop 优雅关停：排空在途 HTTP（短排水，packer 无状态可整请求重试）。
func (s *Service) Stop(ctx context.Context) error {
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, shutdownDrain)
	defer shutdownCancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		s.logger.Warn("packer http shutdown", "error", err)
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
	s.logger.Info("functions-packer stopped")
	return nil
}
