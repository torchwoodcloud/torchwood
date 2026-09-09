package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
)

const assetExpirerInterval = time.Minute

// AssetExpirer 周期扫描到期持有：产 expire 流水并删行（v3 设计 §2.6）。
type AssetExpirer struct {
	assets   *appassets.Assets
	logger   *slog.Logger
	interval time.Duration
}

// NewAssetExpirer creates the expired-holding sweeper.
func NewAssetExpirer(assets *appassets.Assets, logger *slog.Logger) *AssetExpirer {
	if logger == nil {
		logger = slog.Default()
	}
	return &AssetExpirer{assets: assets, logger: logger, interval: assetExpirerInterval}
}

func (c *AssetExpirer) Name() string { return "asset-expirer" }

func (c *AssetExpirer) Init(ctx lynx.AppContext) error { return nil }

func (c *AssetExpirer) Start(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("asset expirer stopped")
			return nil
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

func (c *AssetExpirer) Stop(ctx context.Context) error { return nil }

func (c *AssetExpirer) runOnce(ctx context.Context) {
	n, err := c.assets.ExpireDue(ctx, time.Now().UTC())
	if err != nil {
		c.logger.Error("expire due asset holdings failed", "error", err)
		return
	}
	if n > 0 {
		c.logger.Info("expired asset holdings", "count", n)
	}
}
