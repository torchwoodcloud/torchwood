package clientgrpc

import (
	"context"
	"encoding/base64"
	"time"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AssetsService 是终端用户资产面 gRPC handler（只读）。
type AssetsService struct {
	clientv1.UnimplementedAssetsServiceServer
	assets *appassets.Assets
}

// NewAssetsService constructs the client assets service.
func NewAssetsService(assets *appassets.Assets) *AssetsService {
	return &AssetsService{assets: assets}
}

func (s *AssetsService) ListAssetDefs(ctx context.Context, req *clientv1.ListAssetDefsRequest) (*clientv1.ListAssetDefsResponse, error) {
	before, err := decodeAssetCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	defs, err := s.assets.ListClientDefs(ctx, int(req.GetPageSize()), before)
	if err != nil {
		return nil, err
	}
	out := make([]*clientv1.AssetDef, len(defs))
	for i := range defs {
		mapped, err := mapClientAssetDef(&defs[i])
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(defs) > 0 {
		meta.NextPageToken = encodeAssetCursor(defs[len(defs)-1].CreatedAt)
	}
	return &clientv1.ListAssetDefsResponse{Defs: out, Meta: meta}, nil
}

func (s *AssetsService) ListMyAssets(ctx context.Context, req *clientv1.ListMyAssetsRequest) (*clientv1.ListMyAssetsResponse, error) {
	before, err := decodeAssetCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	rows, err := s.assets.ListMyAssets(ctx, int(req.GetPageSize()), before)
	if err != nil {
		return nil, err
	}
	out := make([]*clientv1.AssetHolding, len(rows))
	for i := range rows {
		mapped, err := mapClientHolding(rows[i])
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(rows) > 0 {
		meta.NextPageToken = encodeAssetCursor(rows[len(rows)-1].Holding.CreatedAt)
	}
	return &clientv1.ListMyAssetsResponse{Holdings: out, Meta: meta}, nil
}

func (s *AssetsService) ListMyAssetLedger(ctx context.Context, req *clientv1.ListMyAssetLedgerRequest) (*clientv1.ListMyAssetLedgerResponse, error) {
	before, err := decodeAssetCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	rows, err := s.assets.ListMyLedger(ctx, req.GetDefCode(), int(req.GetPageSize()), before)
	if err != nil {
		return nil, err
	}
	out := make([]*clientv1.AssetLedgerEntry, len(rows))
	for i := range rows {
		out[i] = mapClientLedger(rows[i])
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(rows) > 0 {
		meta.NextPageToken = encodeAssetCursor(rows[len(rows)-1].Entry.CreatedAt)
	}
	return &clientv1.ListMyAssetLedgerResponse{Entries: out, Meta: meta}, nil
}

func encodeAssetCursor(t time.Time) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano)))
}

func decodeAssetCursor(token string) (time.Time, error) {
	if token == "" {
		return time.Time{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, string(raw))
}

func mapClientAssetDef(d *domainassets.Def) (*clientv1.AssetDef, error) {
	meta, err := rawToStruct(d.Metadata)
	if err != nil {
		return nil, err
	}
	out := &clientv1.AssetDef{
		Id:             d.ID,
		Code:           d.Code,
		Name:           d.Name,
		Class:          string(d.Class),
		Decimals:       d.Decimals,
		Tradable:       d.Tradable,
		UniquePerOwner: d.UniquePerOwner,
		Upgradeable:    d.Upgradeable,
		Metadata:       meta,
	}
	if d.MaxQuantity != nil {
		out.MaxQuantity = d.MaxQuantity
	}
	if d.ExpiresIn != nil {
		out.ExpiresIn = d.ExpiresIn
	}
	return out, nil
}

func mapClientHolding(v appassets.HoldingView) (*clientv1.AssetHolding, error) {
	meta, err := rawToStruct(v.Holding.Metadata)
	if err != nil {
		return nil, err
	}
	out := &clientv1.AssetHolding{
		Id:       v.Holding.ID,
		DefId:    v.Holding.DefID,
		DefCode:  v.DefCode,
		Class:    string(v.Class),
		Quantity: v.Holding.Quantity,
		Level:    v.Holding.Level,
		Metadata: meta,
	}
	if v.Holding.ExpiresAt != nil {
		out.ExpiresAt = timestamppb.New(*v.Holding.ExpiresAt)
	}
	return out, nil
}

func mapClientLedger(v appassets.LedgerView) *clientv1.AssetLedgerEntry {
	out := &clientv1.AssetLedgerEntry{
		Id:            v.Entry.ID,
		DefId:         v.Entry.DefID,
		DefCode:       v.DefCode,
		Kind:          string(v.Entry.Kind),
		Delta:         v.Entry.Delta,
		QuantityAfter: v.Entry.QuantityAfter,
		RefType:       v.Entry.RefType,
		RefId:         v.Entry.RefID,
		CreatedAt:     timestamppb.New(v.Entry.CreatedAt),
	}
	if v.Entry.ExpiresAt != nil {
		out.ExpiresAt = timestamppb.New(*v.Entry.ExpiresAt)
	}
	return out
}
