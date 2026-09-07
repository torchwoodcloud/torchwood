package servergrpc

import (
	"context"
	"encoding/json"
	"time"

	serverv1 "github.com/torchwooddev/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
	appassets "github.com/torchwooddev/torchwood/internal/app/assets"
	domainassets "github.com/torchwooddev/torchwood/internal/domain/assets"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AssetsService 是资产管理面 gRPC handler（薄：scope / 角色在拦截器，
// 主体断言在 use-case）。
type AssetsService struct {
	serverv1.UnimplementedAssetsServiceServer
	assets *appassets.Assets
}

// NewAssetsService constructs the server assets service.
func NewAssetsService(assets *appassets.Assets) *AssetsService {
	return &AssetsService{assets: assets}
}

func (s *AssetsService) CreateAssetDef(ctx context.Context, req *serverv1.CreateAssetDefRequest) (*serverv1.AssetDef, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	meta, err := structToRaw(req.GetMetadata())
	if err != nil {
		return nil, err
	}
	def, err := s.assets.CreateDef(withAuditResource(ctx, req.GetCode()), appassets.CreateDefCommand{
		Code:           req.GetCode(),
		Name:           req.GetName(),
		Class:          domainassets.Class(req.GetClass()),
		Decimals:       req.GetDecimals(),
		MaxQuantity:    req.MaxQuantity,
		ExpiresIn:      req.ExpiresIn,
		Tradable:       req.GetTradable(),
		UniquePerOwner: req.GetUniquePerOwner(),
		Upgradeable:    req.GetUpgradeable(),
		Metadata:       meta,
	})
	if err != nil {
		return nil, err
	}
	return mapServerAssetDef(def)
}

func (s *AssetsService) ListAssetDefs(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListAssetDefsResponse, error) {
	before, err := decodeServerOrderCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	defs, err := s.assets.ListDefs(ctx, true, int(req.GetPageSize()), before)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.AssetDef, len(defs))
	for i := range defs {
		mapped, err := mapServerAssetDef(&defs[i])
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(defs) > 0 {
		meta.NextPageToken = encodeServerOrderCursor(defs[len(defs)-1].CreatedAt)
	}
	return &serverv1.ListAssetDefsResponse{Defs: out, Meta: meta}, nil
}

func (s *AssetsService) GetAssetDef(ctx context.Context, req *serverv1.GetAssetDefRequest) (*serverv1.AssetDef, error) {
	// def_id required 由 buf.validate 注解在 validate 拦截器承担（09-api-guide §2.3）。
	def, err := s.assets.GetDef(ctx, req.GetDefId())
	if err != nil {
		return nil, err
	}
	return mapServerAssetDef(def)
}

func (s *AssetsService) UpdateAssetDef(ctx context.Context, req *serverv1.UpdateAssetDefRequest) (*serverv1.AssetDef, error) {
	// def_id required 同上，由 buf.validate 注解承担。
	cmd := appassets.UpdateDefCommand{DefID: req.GetDefId()}
	if req.Name != nil {
		n := req.GetName()
		cmd.Name = &n
	}
	if req.Decimals != nil {
		d := req.GetDecimals()
		cmd.Decimals = &d
	}
	if req.MaxQuantity != nil {
		if req.GetMaxQuantity() <= 0 {
			cmd.ClearMax = true
		} else {
			v := req.GetMaxQuantity()
			cmd.MaxQuantity = &v
		}
	}
	if req.ExpiresIn != nil {
		if req.GetExpiresIn() <= 0 {
			cmd.ClearExpiresIn = true
		} else {
			v := req.GetExpiresIn()
			cmd.ExpiresIn = &v
		}
	}
	if req.Tradable != nil {
		v := req.GetTradable()
		cmd.Tradable = &v
	}
	if req.UniquePerOwner != nil {
		v := req.GetUniquePerOwner()
		cmd.UniquePerOwner = &v
	}
	if req.Upgradeable != nil {
		v := req.GetUpgradeable()
		cmd.Upgradeable = &v
	}
	if req.GetMetadata() != nil {
		raw, err := structToRaw(req.GetMetadata())
		if err != nil {
			return nil, err
		}
		cmd.Metadata = raw
	}
	if req.Status != nil {
		st := domainassets.DefStatus(req.GetStatus())
		cmd.Status = &st
	}
	def, err := s.assets.UpdateDef(withAuditResource(ctx, req.GetDefId()), cmd)
	if err != nil {
		return nil, err
	}
	return mapServerAssetDef(def)
}

func (s *AssetsService) DeleteAssetDef(ctx context.Context, req *serverv1.DeleteAssetDefRequest) (*sharedv1.Empty, error) {
	// def_id required 同上，由 buf.validate 注解承担。
	if err := s.assets.DeleteDef(withAuditResource(ctx, req.GetDefId()), req.GetDefId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *AssetsService) Grant(ctx context.Context, req *serverv1.GrantRequest) (*serverv1.AssetOpResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	meta, err := structToRaw(req.GetMetadata())
	if err != nil {
		return nil, err
	}
	var expires *time.Time
	if req.ExpiresAt != nil {
		t := req.GetExpiresAt().AsTime()
		expires = &t
	}
	var level int32
	if req.Level != nil {
		level = req.GetLevel()
	}
	res, err := s.assets.Grant(withAuditResource(ctx, req.GetOwnerId()), appassets.GrantCommand{
		OwnerID:        req.GetOwnerId(),
		DefCode:        req.GetDefCode(),
		Quantity:       req.GetQuantity(),
		ExpiresAt:      expires,
		Level:          level,
		Metadata:       meta,
		IdempotencyKey: req.GetIdempotencyKey(),
		RefType:        req.GetRefType(),
		RefID:          req.GetRefId(),
	})
	if err != nil {
		return nil, err
	}
	return mapOpResult(res)
}

func (s *AssetsService) Consume(ctx context.Context, req *serverv1.ConsumeRequest) (*serverv1.AssetOpResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	res, err := s.assets.Consume(withAuditResource(ctx, req.GetOwnerId()), appassets.ConsumeCommand{
		OwnerID:        req.GetOwnerId(),
		DefCode:        req.GetDefCode(),
		Quantity:       req.GetQuantity(),
		IdempotencyKey: req.GetIdempotencyKey(),
		RefType:        req.GetRefType(),
		RefID:          req.GetRefId(),
	})
	if err != nil {
		return nil, err
	}
	return mapOpResult(res)
}

func (s *AssetsService) Transfer(ctx context.Context, req *serverv1.TransferRequest) (*serverv1.AssetOpResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	res, err := s.assets.Transfer(withAuditResource(ctx, req.GetFromOwnerId()), appassets.TransferCommand{
		FromOwnerID:    req.GetFromOwnerId(),
		ToOwnerID:      req.GetToOwnerId(),
		DefCode:        req.GetDefCode(),
		Quantity:       req.GetQuantity(),
		IdempotencyKey: req.GetIdempotencyKey(),
		RefType:        req.GetRefType(),
		RefID:          req.GetRefId(),
	})
	if err != nil {
		return nil, err
	}
	return mapOpResult(res)
}

func (s *AssetsService) Mutate(ctx context.Context, req *serverv1.MutateRequest) (*serverv1.AssetOpResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	cmd := appassets.MutateCommand{
		HoldingID:      req.GetHoldingId(),
		IdempotencyKey: req.GetIdempotencyKey(),
		RefType:        req.GetRefType(),
		RefID:          req.GetRefId(),
	}
	if req.Level != nil {
		v := req.GetLevel()
		cmd.Level = &v
	}
	if req.ExpiresAt != nil {
		t := req.GetExpiresAt().AsTime()
		cmd.ExpiresAt = &t
	}
	if req.GetMetadata() != nil {
		raw, err := structToRaw(req.GetMetadata())
		if err != nil {
			return nil, err
		}
		cmd.Metadata = raw
	}
	res, err := s.assets.Mutate(withAuditResource(ctx, req.GetHoldingId()), cmd)
	if err != nil {
		return nil, err
	}
	return mapOpResult(res)
}

func (s *AssetsService) Expire(ctx context.Context, req *serverv1.ExpireRequest) (*serverv1.AssetOpResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	res, err := s.assets.Expire(withAuditResource(ctx, req.GetHoldingId()), appassets.ExpireCommand{
		HoldingID:      req.GetHoldingId(),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, err
	}
	return mapOpResult(res)
}

func (s *AssetsService) ListUserAssets(ctx context.Context, req *serverv1.ListUserAssetsRequest) (*serverv1.ListUserAssetsResponse, error) {
	// owner_id required 同上，由 buf.validate 注解承担。
	before, err := decodeServerOrderCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	rows, err := s.assets.ListUserAssets(ctx, req.GetOwnerId(), int(req.GetPageSize()), before)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.AssetHolding, len(rows))
	for i := range rows {
		mapped, err := mapServerHolding(rows[i])
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(rows) > 0 {
		meta.NextPageToken = encodeServerOrderCursor(rows[len(rows)-1].Holding.CreatedAt)
	}
	return &serverv1.ListUserAssetsResponse{Holdings: out, Meta: meta}, nil
}

func (s *AssetsService) ListUserLedger(ctx context.Context, req *serverv1.ListUserLedgerRequest) (*serverv1.ListUserLedgerResponse, error) {
	// owner_id required 同上，由 buf.validate 注解承担。
	before, err := decodeServerOrderCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	rows, err := s.assets.ListUserLedger(ctx, req.GetOwnerId(), req.GetDefCode(), int(req.GetPageSize()), before)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.AssetLedgerEntry, len(rows))
	for i := range rows {
		out[i] = mapServerLedgerView(rows[i])
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(rows) > 0 {
		meta.NextPageToken = encodeServerOrderCursor(rows[len(rows)-1].Entry.CreatedAt)
	}
	return &serverv1.ListUserLedgerResponse{Entries: out, Meta: meta}, nil
}

func (s *AssetsService) Reconcile(ctx context.Context, _ *serverv1.ReconcileRequest) (*serverv1.ReconcileResponse, error) {
	report, err := s.assets.Reconcile(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.AssetDrift, len(report.Drifts))
	for i, d := range report.Drifts {
		out[i] = &serverv1.AssetDrift{
			OwnerId:     d.OwnerID,
			DefId:       d.DefID,
			HoldingQty:  d.HoldingQty,
			ReplayedQty: d.ReplayedQty,
			Detail:      d.Detail,
		}
	}
	return &serverv1.ReconcileResponse{
		ZeroDrift:  report.ZeroDrift,
		Holdings:   int32(report.Holdings),
		Entries:    int32(report.Entries),
		DriftCount: int32(len(report.Drifts)),
		Drifts:     out,
	}, nil
}

func mapOpResult(res *appassets.OpResult) (*serverv1.AssetOpResponse, error) {
	if res == nil {
		return &serverv1.AssetOpResponse{}, nil
	}
	out := make([]*serverv1.AssetLedgerEntry, len(res.Entries))
	for i := range res.Entries {
		out[i] = mapServerLedger(&res.Entries[i])
	}
	return &serverv1.AssetOpResponse{Entries: out, IdempotentReplay: res.IdempotentReplay}, nil
}

func mapServerAssetDef(d *domainassets.Def) (*serverv1.AssetDef, error) {
	if d == nil {
		return nil, status.Error(codes.NotFound, "asset def not found")
	}
	meta, err := rawToStructServer(d.Metadata)
	if err != nil {
		return nil, err
	}
	out := &serverv1.AssetDef{
		Id:             d.ID,
		ProjectId:      d.ProjectID,
		Code:           d.Code,
		Name:           d.Name,
		Class:          string(d.Class),
		Decimals:       d.Decimals,
		Tradable:       d.Tradable,
		UniquePerOwner: d.UniquePerOwner,
		Upgradeable:    d.Upgradeable,
		Metadata:       meta,
		Status:         string(d.Status),
		CreatedAt:      timestamppb.New(d.CreatedAt),
		UpdatedAt:      timestamppb.New(d.UpdatedAt),
	}
	if d.MaxQuantity != nil {
		out.MaxQuantity = d.MaxQuantity
	}
	if d.ExpiresIn != nil {
		out.ExpiresIn = d.ExpiresIn
	}
	return out, nil
}

func mapServerHolding(v appassets.HoldingView) (*serverv1.AssetHolding, error) {
	meta, err := rawToStructServer(v.Holding.Metadata)
	if err != nil {
		return nil, err
	}
	out := &serverv1.AssetHolding{
		Id:       v.Holding.ID,
		OwnerId:  v.Holding.OwnerID,
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

func mapServerLedgerView(v appassets.LedgerView) *serverv1.AssetLedgerEntry {
	out := mapServerLedger(&v.Entry)
	out.DefCode = v.DefCode
	return out
}

func mapServerLedger(e *domainassets.LedgerEntry) *serverv1.AssetLedgerEntry {
	out := &serverv1.AssetLedgerEntry{
		Id:             e.ID,
		HoldingId:      e.HoldingID,
		OwnerId:        e.OwnerID,
		DefId:          e.DefID,
		Kind:           string(e.Kind),
		Delta:          e.Delta,
		QuantityAfter:  e.QuantityAfter,
		RefType:        e.RefType,
		RefId:          e.RefID,
		IdempotencyKey: e.IdempotencyKey,
		CreatedAt:      timestamppb.New(e.CreatedAt),
	}
	if e.ExpiresAt != nil {
		out.ExpiresAt = timestamppb.New(*e.ExpiresAt)
	}
	return out
}

func structToRaw(s *structpb.Struct) (json.RawMessage, error) {
	if s == nil {
		return nil, nil
	}
	return json.Marshal(s.AsMap())
}
