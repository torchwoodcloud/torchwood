package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
)

// OrphanChunkCleaner 抽象 Storage 的孤儿分片清理能力（wire 绑定到
// *storage.Storage；测试可用 fake 替换）。导出是为了 cmd/worker 装配处
// 跨包 wire.Bind。
type OrphanChunkCleaner interface {
	CleanupOrphanChunks(ctx context.Context) (int, error)
}

// chunkCleanerInterval 是周期清理间隔（每小时一次）。
const chunkCleanerInterval = time.Hour

// chunkCleanerInitialDelay 是启动后首次执行的延迟（1 分钟）。
const chunkCleanerInitialDelay = time.Minute

// ChunkCleaner 周期清理孤儿分片对象（会话过期/abort/complete 删除失败残留，
// 见 internal/app/storage/cleanup.go 的 CleanupOrphanChunks，48h 阈值）。
type ChunkCleaner struct {
	cleaner      OrphanChunkCleaner
	logger       *slog.Logger
	interval     time.Duration
	initialDelay time.Duration
}

// NewChunkCleaner creates the orphan chunk cleanup service.
func NewChunkCleaner(cleaner OrphanChunkCleaner, logger *slog.Logger) *ChunkCleaner {
	if logger == nil {
		logger = slog.Default()
	}
	return &ChunkCleaner{
		cleaner:      cleaner,
		logger:       logger,
		interval:     chunkCleanerInterval,
		initialDelay: chunkCleanerInitialDelay,
	}
}

func (c *ChunkCleaner) Name() string { return "chunk-cleaner" }

func (c *ChunkCleaner) Init(ctx lynx.AppContext) error {
	return nil
}

// Start 周期执行清理：启动延迟 initialDelay 后执行一次，此后每 interval 一次；
// 失败仅记日志；阻塞到 ctx 取消。
func (c *ChunkCleaner) Start(ctx context.Context) error {
	delay := time.NewTimer(c.initialDelay)
	defer delay.Stop()
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("chunk cleaner stopped")
			return nil
		case <-delay.C:
			c.runOnce(ctx)
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

func (c *ChunkCleaner) Stop(ctx context.Context) error {
	return nil
}

func (c *ChunkCleaner) runOnce(ctx context.Context) {
	removed, err := c.cleaner.CleanupOrphanChunks(ctx)
	if err != nil {
		c.logger.Error("cleanup orphan chunks failed", "error", err)
		return
	}
	if removed > 0 {
		c.logger.Info("cleaned orphan chunks", "removed", removed)
	}
}
