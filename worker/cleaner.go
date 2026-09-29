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

const (
	// chunkCleanerInterval 是周期清理间隔（每小时一次）。
	chunkCleanerInterval = time.Hour
	// chunkCleanerTimeout 是单轮清理的预算上限（批量删除有界；WithoutCancel
	// 使停机不腰斩在途轮次）。
	chunkCleanerTimeout = 10 * time.Minute
)

// chunkCleanerInitialDelay 是启动后首次执行的延迟（1 分钟）。
const chunkCleanerInitialDelay = time.Minute

// ChunkCleaner 周期清理孤儿分片对象（会话过期/abort/complete 删除失败残留，
// 见 internal/app/storage/cleanup.go 的 CleanupOrphanChunks，48h 阈值）。
type ChunkCleaner struct {
	cleaner OrphanChunkCleaner
	logger  *slog.Logger
	loop    *Periodic
}

// NewChunkCleaner creates the orphan chunk cleanup service.
func NewChunkCleaner(cleaner OrphanChunkCleaner, logger *slog.Logger) *ChunkCleaner {
	if logger == nil {
		logger = slog.Default()
	}
	c := &ChunkCleaner{cleaner: cleaner, logger: logger}
	c.loop = NewPeriodic("chunk-cleaner", chunkCleanerInterval, chunkCleanerTimeout, c.runOnce, logger).
		After(chunkCleanerInitialDelay)
	return c
}

func (c *ChunkCleaner) Name() string { return "chunk-cleaner" }

func (c *ChunkCleaner) Init(ctx lynx.AppContext) error {
	return nil
}

func (c *ChunkCleaner) Start(ctx context.Context) error { return c.loop.Start(ctx) }

func (c *ChunkCleaner) Stop(ctx context.Context) error {
	return nil
}

func (c *ChunkCleaner) runOnce(ctx context.Context) error {
	removed, err := c.cleaner.CleanupOrphanChunks(ctx)
	if err != nil {
		return err
	}
	if removed > 0 {
		c.logger.Info("cleaned orphan chunks", "removed", removed)
	}
	return nil
}
