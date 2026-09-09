package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// AssetsService 封装 Server API 的资产管理（Def CRUD + 五动词 + 用户资产查询）。
// 写方法仅 system / admin-key；SDK 不提供终端用户写入口。
type AssetsService struct {
	c   *Client
	api serverv1.AssetsServiceClient
}

func (s *AssetsService) CreateAssetDef(ctx context.Context, req *serverv1.CreateAssetDefRequest) (*serverv1.AssetDef, error) {
	return s.api.CreateAssetDef(ctx, req)
}

func (s *AssetsService) ListAssetDefs(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListAssetDefsResponse, error) {
	return s.api.ListAssetDefs(ctx, req)
}

func (s *AssetsService) GetAssetDef(ctx context.Context, defID string) (*serverv1.AssetDef, error) {
	return s.api.GetAssetDef(ctx, &serverv1.GetAssetDefRequest{DefId: defID})
}

func (s *AssetsService) UpdateAssetDef(ctx context.Context, req *serverv1.UpdateAssetDefRequest) (*serverv1.AssetDef, error) {
	return s.api.UpdateAssetDef(ctx, req)
}

func (s *AssetsService) DeleteAssetDef(ctx context.Context, defID string) error {
	_, err := s.api.DeleteAssetDef(ctx, &serverv1.DeleteAssetDefRequest{DefId: defID})
	return err
}

func (s *AssetsService) Grant(ctx context.Context, req *serverv1.GrantRequest) (*serverv1.AssetOpResponse, error) {
	return s.api.Grant(ctx, req)
}

func (s *AssetsService) Consume(ctx context.Context, req *serverv1.ConsumeRequest) (*serverv1.AssetOpResponse, error) {
	return s.api.Consume(ctx, req)
}

func (s *AssetsService) Transfer(ctx context.Context, req *serverv1.TransferRequest) (*serverv1.AssetOpResponse, error) {
	return s.api.Transfer(ctx, req)
}

func (s *AssetsService) Mutate(ctx context.Context, req *serverv1.MutateRequest) (*serverv1.AssetOpResponse, error) {
	return s.api.Mutate(ctx, req)
}

func (s *AssetsService) Expire(ctx context.Context, req *serverv1.ExpireRequest) (*serverv1.AssetOpResponse, error) {
	return s.api.Expire(ctx, req)
}

func (s *AssetsService) Reconcile(ctx context.Context) (*serverv1.ReconcileResponse, error) {
	return s.api.Reconcile(ctx, &serverv1.ReconcileRequest{})
}

func (s *AssetsService) ListUserAssets(ctx context.Context, req *serverv1.ListUserAssetsRequest) (*serverv1.ListUserAssetsResponse, error) {
	return s.api.ListUserAssets(ctx, req)
}

func (s *AssetsService) ListUserLedger(ctx context.Context, req *serverv1.ListUserLedgerRequest) (*serverv1.ListUserLedgerResponse, error) {
	return s.api.ListUserLedger(ctx, req)
}
