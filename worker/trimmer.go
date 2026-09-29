package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
)

const (
	// streamTrimInterval 是队列 stream 裁剪周期。
	streamTrimInterval = 10 * time.Minute
	// streamTrimTimeout 是单轮 XTRIM 的预算上限（近似裁剪，O(被裁剪部分)，
	// 有界操作；超时由下一轮收敛）。
	streamTrimTimeout = 10 * time.Second
)

// streamTrimMaxLen 是 functions-executions stream 的近似裁剪水位
// （P1-15：XADD 不设 MaxLen 保未投递消息，裁剪交给本低频任务；APPROX
// 语义下单次 O(被裁剪部分)，水位远高于正常积压）。
const streamTrimMaxLen = 100000

// StreamTrimmer 周期 XTRIM 队列 stream（与 ChunkCleaner 同框架）。
type StreamTrimmer struct {
	queue domainshared.Queue
	loop  *Periodic
}

// NewStreamTrimmer creates the queue stream trim service.
func NewStreamTrimmer(queue domainshared.Queue, logger *slog.Logger) *StreamTrimmer {
	t := &StreamTrimmer{queue: queue}
	t.loop = NewPeriodic("stream-trimmer", streamTrimInterval, streamTrimTimeout, t.runOnce, logger)
	return t
}

func (t *StreamTrimmer) Name() string { return "stream-trimmer" }

func (t *StreamTrimmer) Init(ctx lynx.AppContext) error { return nil }

func (t *StreamTrimmer) Start(ctx context.Context) error { return t.loop.Start(ctx) }

func (t *StreamTrimmer) Stop(ctx context.Context) error { return nil }

func (t *StreamTrimmer) runOnce(ctx context.Context) error {
	return t.queue.Trim(ctx, domainshared.QueueFunctionsExecutions, streamTrimMaxLen)
}
