package worker

import (
	"context"
	"log/slog"
	"time"
)

// Periodic 是周期作业骨架——worker 侧全部 ticker 循环（12 个作业 + Worker
// 内部 3 个 loop）的单一实现：ticker+select 循环、启动即跑/首轮延迟、单轮
// 预算、停机日志。
//
// 单轮预算 = context.WithTimeout(context.WithoutCancel(ctx), budget)：
// WithoutCancel 使优雅关停不腰斩在途轮次（轮次在预算边界自然收束，作业幂等
// 由下一轮收敛）；budget 必须 > 0 且短于 interval（钉死的常量在各作业文件，
// 与 interval 的关系由各作业测试断言）。业务轮次 fn 返回 error 由骨架记
// Error 日志（关停引发的失败以 parent ctx.Err() 为哨兵抑制）；需要逐条
// 消息语义的多步轮次（如 analytics maintenance）可自行记日志并返回 nil。
type Periodic struct {
	name         string
	interval     time.Duration
	budget       time.Duration
	runAtStart   bool
	initialDelay time.Duration
	logger       *slog.Logger
	fn           func(ctx context.Context) error
}

// NewPeriodic 构造周期作业。name 用于日志前缀；fn 在每轮收到带预算的派生 ctx。
func NewPeriodic(name string, interval, budget time.Duration, fn func(ctx context.Context) error, logger *slog.Logger) *Periodic {
	if interval <= 0 {
		panic("periodic: interval must be positive")
	}
	if budget <= 0 {
		panic("periodic: budget must be positive")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Periodic{name: name, interval: interval, budget: budget, fn: fn, logger: logger}
}

// RunAtStart 启动即跑一轮（worker 重启补齐错过的窗口——幂等作业适用）。
func (p *Periodic) RunAtStart() *Periodic {
	p.runAtStart = true
	return p
}

// After 把首轮推迟到 initialDelay 之后（此后仍按 interval 周期；ticker 从
// 启动时刻起算，语义对齐原 ChunkCleaner 的 timer+ticker 并行形态）。独占
// RunAtStart 语义。
func (p *Periodic) After(initialDelay time.Duration) *Periodic {
	p.initialDelay = initialDelay
	return p
}

// Start 阻塞运行周期循环到 ctx 取消（lynx actor 契约：Start 不得立即返回）。
func (p *Periodic) Start(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	switch {
	case p.initialDelay > 0:
		delay := time.NewTimer(p.initialDelay)
		defer delay.Stop()
		select {
		case <-ctx.Done():
			p.logger.Info(p.name + " stopped")
			return nil
		case <-delay.C:
			p.runOnce(ctx)
		}
	case p.runAtStart:
		p.runOnce(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			p.logger.Info(p.name + " stopped")
			return nil
		case <-ticker.C:
			p.runOnce(ctx)
		}
	}
}

// runOnce 执行单轮：派生预算 ctx 并调用 fn；关停引发的失败不记日志。
func (p *Periodic) runOnce(ctx context.Context) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.budget)
	defer cancel()
	if err := p.fn(rctx); err != nil && ctx.Err() == nil {
		p.logger.Error(p.name+" round failed", "error", err)
	}
}
