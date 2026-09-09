package client

import (
	"context"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
)

// AssetsService 封装 Client API 的只读资产查询（终端用户无写入口，红线 D6）。
type AssetsService struct{ c *Client }

// ListAssetDefs 列出项目内可见资产定义。
func (a *AssetsService) ListAssetDefs(ctx context.Context, pageSize int32, pageToken string) (*clientv1.ListAssetDefsResponse, error) {
	return a.c.assets.ListAssetDefs(ctx, &clientv1.ListAssetDefsRequest{
		PageSize:  pageSize,
		PageToken: pageToken,
	})
}

// ListMyAssets 列出本人未过期持有。quantity 为 int64 最小单位。
func (a *AssetsService) ListMyAssets(ctx context.Context, pageSize int32, pageToken string) (*clientv1.ListMyAssetsResponse, error) {
	return a.c.assets.ListMyAssets(ctx, &clientv1.ListMyAssetsRequest{
		PageSize:  pageSize,
		PageToken: pageToken,
	})
}

// ListMyAssetLedger 列出本人流水。
func (a *AssetsService) ListMyAssetLedger(ctx context.Context, defCode string, pageSize int32, pageToken string) (*clientv1.ListMyAssetLedgerResponse, error) {
	return a.c.assets.ListMyAssetLedger(ctx, &clientv1.ListMyAssetLedgerRequest{
		PageSize:  pageSize,
		PageToken: pageToken,
		DefCode:   defCode,
	})
}
