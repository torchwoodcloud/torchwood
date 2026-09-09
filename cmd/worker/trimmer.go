package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
)

// streamTrimInterval 是队列 stream 裁剪周期。
const streamTrimInterval = 10 * time.Minute

// streamTrimMaxLen 是 functions-executions stream 的近似裁剪水位
// （P1-15：XADD 不设 MaxLen 保未投递消息，裁剪交给本低频任务；APPROX
// 语义下单次 O(被裁剪部分)，水位远高于正常积压）。
const streamTrimMaxLen = 100000

// StreamTrimmer 周期 XTRIM 队列 stream（与 ChunkCleaner 同框架）。
type StreamTrimmer struct {
	queue    domainshared.Queue
	logger   *slog.Logger
	interval time.Duration
}

// NewStreamTrimmer creates the queue stream trim service.
func NewStreamTrimmer(queue domainshared.Queue, logger *slog.Logger) *StreamTrimmer {
	if logger == nil {
		logger = slog.Default()
	}
	return &StreamTrimmer{queue: queue, logger: logger, interval: streamTrimInterval}
}

func (t *StreamTrimmer) Name() string { return "stream-trimmer" }

func (t *StreamTrimmer) Init(ctx lynx.AppContext) error { return nil }

func (t *StreamTrimmer) Start(ctx context.Context) error {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			t.runOnce(ctx)
		}
	}
}

func (t *StreamTrimmer) Stop(ctx context.Context) error { return nil }

func (t *StreamTrimmer) runOnce(ctx context.Context) {
	trimCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := t.queue.Trim(trimCtx, domainshared.QueueFunctionsExecutions, streamTrimMaxLen); err != nil {
		if ctx.Err() == nil {
			t.logger.Error("trim stream failed", "error", err)
		}
		return
	}
}
