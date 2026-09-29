package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
)

const (
	assetExpirerInterval = time.Minute
	// assetExpirerTimeout 是单轮到期扫描的预算上限（有界批量；短于扫描
	// 周期，对齐 cron 领取的 50s/1min 比例；WithoutCancel 使停机不腰斩
	// 在途轮次）。
	assetExpirerTimeout = 50 * time.Second
)

// AssetExpirer 周期扫描到期持有：产 expire 流水并删行（v3 设计 §2.6）。
type AssetExpirer struct {
	assets *appassets.Assets
	logger *slog.Logger
	loop   *Periodic
}

// NewAssetExpirer creates the expired-holding sweeper.
func NewAssetExpirer(assets *appassets.Assets, logger *slog.Logger) *AssetExpirer {
	if logger == nil {
		logger = slog.Default()
	}
	c := &AssetExpirer{assets: assets, logger: logger}
	c.loop = NewPeriodic("asset-expirer", assetExpirerInterval, assetExpirerTimeout, c.runOnce, logger)
	return c
}

func (c *AssetExpirer) Name() string { return "asset-expirer" }

func (c *AssetExpirer) Init(ctx lynx.AppContext) error { return nil }

func (c *AssetExpirer) Start(ctx context.Context) error { return c.loop.Start(ctx) }

func (c *AssetExpirer) Stop(ctx context.Context) error { return nil }

func (c *AssetExpirer) runOnce(ctx context.Context) error {
	n, err := c.assets.ExpireDue(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	if n > 0 {
		c.logger.Info("expired asset holdings", "count", n)
	}
	return nil
}
