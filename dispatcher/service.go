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

// Service 是 dispatcher 的 lynx 服务装配：HTTP API + reaper 周期
// 对账（独立二进制 cmd/dispatcher；不进 server/worker wire）。
// 四期 4a-1 起兼节点注册面（M2）：启动即自注册 + 周期心跳
// （torchwood:fnnodes:<node_id>，TTL 心跳刷新），reaper 的多机收窄（M8）
// 依赖该心跳面判定节点存活。
type Service struct {
	cfg    *config.AppConfig
	logger *slog.Logger
	pool   *PoolManager
	http   *http.Server
	// registry 是实例+节点注册表（节点心跳经同一端口写入）。
	registry Registry
	// nodeID/nodeURL 是本进程节点身份（ResolveNodeIdentity 解析：config
	// node_id/node_url，缺省 hostname / addr 端口推导）。
	nodeID  string
	nodeURL string

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
	registry := NewRedisRegistry(rdb)
	pool := NewPoolManager(daemon, registry, PoolConfigFromConfig(cfg))
	nodeID, nodeURL := ResolveNodeIdentity(cfg)
	// 节点身份注入（M2/M3/M8）：spawn 固化进实例记录 + BuildResponse.node_id
	// + reaper 对账收窄判定基准。
	pool.SetNodeID(nodeID)
	// 节点转发客户端（四期 4a-2 M3 路由层）：实例亲和/BuildNode 冷启动的
	// 跨节点手段；节点身份与共享 token 同源注入（节点间鉴权与
	// server→dispatcher 同一面）。
	pool.SetForwarder(newNodeForwarder(nodeID, cfg.GetFunctions().GetDispatcher().GetSharedToken()))
	addr := cfg.GetFunctions().GetDispatcher().GetAddr()
	if addr == "" {
		addr = ":9070"
	}
	srv := newDispatchServer(pool, daemon, cfg.GetFunctions().GetDispatcher().GetSharedToken())
	s := &Service{
		cfg:      cfg,
		logger:   logger,
		pool:     pool,
		registry: registry,
		nodeID:   nodeID,
		nodeURL:  nodeURL,
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
				reapCtx, reapCancel := context.WithTimeout(context.WithoutCancel(runCtx), dockerCleanupTimeout)
				s.pool.Reaper(reapCtx)
				reapCancel()
			}
		}
	})
	// 节点自注册 + 心跳（M2）：启动即注册一次（不等首个 tick），此后每
	// nodeHeartbeatInterval 续期一次（TTL defaultNodeTTL）。正常关停不注销
	// ——TTL 自然过期即失联，M8 死节点收敛据此接管记录清理。
	s.wg.Go(func() {
		s.beatNode()
		ticker := time.NewTicker(nodeHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				s.beatNode()
			}
		}
	})
	s.logger.Info("dispatcher started", "addr", s.http.Addr,
		"node", s.nodeID, "node_url", s.nodeURL,
		"max_resident_instances", s.pool.cfg.MaxResidentInstances)

	<-ctx.Done()
	return nil
}

// beatNode 写一次节点心跳（独立短超时，不占用 reaper 预算）。心跳失败不
// 致命：TTL 内的下一 tick 重试；连续失败由 TTL 过期呈现失联（M8 死节点
// 收敛的判定以键存在性为准，观测面按日志告警）。
func (s *Service) beatNode() {
	ctx, cancel := context.WithTimeout(context.Background(), nodeHBTimeout)
	defer cancel()
	rec := NodeRecord{NodeID: s.nodeID, URL: s.nodeURL, HbUnixMS: time.Now().UnixMilli()}
	if err := s.registry.SaveNode(ctx, rec, defaultNodeTTL); err != nil {
		s.logger.Warn("dispatcher node heartbeat failed", "node", s.nodeID, "error", err)
	}
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
	s.logger.Info("dispatcher stopped")
	return nil
}
