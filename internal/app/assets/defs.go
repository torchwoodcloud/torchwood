package assets

import (
	"context"
	"encoding/json"
	"time"

	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CreateDefCommand 是创建资产定义的入参。
type CreateDefCommand struct {
	Code           string
	Name           string
	Class          domainassets.Class
	Decimals       int32
	MaxQuantity    *int64
	ExpiresIn      *int64
	Tradable       bool
	UniquePerOwner bool
	Upgradeable    bool
	Metadata       json.RawMessage
}

// UpdateDefCommand 是更新资产定义的入参（未设置=不修改；class/code 不可变）。
type UpdateDefCommand struct {
	DefID          string
	Name           *string
	Decimals       *int32
	MaxQuantity    *int64
	ClearMax       bool
	ExpiresIn      *int64
	ClearExpiresIn bool
	Tradable       *bool
	UniquePerOwner *bool
	Upgradeable    *bool
	Metadata       json.RawMessage
	Status         *domainassets.DefStatus
}

// CreateDef 创建资产定义（Server 面，矩阵校验）。
func (a *Assets) CreateDef(ctx context.Context, cmd CreateDefCommand) (*domainassets.Def, error) {
	if err := requireAssetWrite(ctx); err != nil {
		return nil, err
	}
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	code, err := validateCode(cmd.Code)
	if err != nil {
		return nil, err
	}
	if cmd.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	now := a.ts()
	def := &domainassets.Def{
		ID:             newID(),
		ProjectID:      projectID,
		Code:           code,
		Name:           cmd.Name,
		Class:          cmd.Class,
		Decimals:       cmd.Decimals,
		MaxQuantity:    cmd.MaxQuantity,
		ExpiresIn:      cmd.ExpiresIn,
		Tradable:       cmd.Tradable,
		UniquePerOwner: cmd.UniquePerOwner,
		Upgradeable:    cmd.Upgradeable,
		Metadata:       cmd.Metadata,
		Status:         domainassets.DefStatusActive,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if def.Class == domainassets.ClassEntitlement {
		def.UniquePerOwner = true
		def.Tradable = false
	}
	if def.Class == domainassets.ClassCurrency {
		def.UniquePerOwner = true
	}
	if err := domainassets.ValidateDefMatrix(def); err != nil {
		return nil, mapWriteError(err)
	}
	if err := a.defs.Insert(ctx, def); err != nil {
		return nil, mapWriteError(err)
	}
	return def, nil
}

// GetDef 按 id 取定义（Server 面）。
func (a *Assets) GetDef(ctx context.Context, defID string) (*domainassets.Def, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	if defID == "" {
		return nil, status.Error(codes.InvalidArgument, "def_id is required")
	}
	def, err := a.defs.GetByID(ctx, projectID, defID)
	if err != nil {
		return nil, err
	}
	if def == nil {
		return nil, status.Error(codes.NotFound, "asset def not found")
	}
	return def, nil
}

// ListDefs 列出项目定义（Server 面含归档；Client 面仅 active）。
func (a *Assets) ListDefs(ctx context.Context, includeArchived bool, limit int, before time.Time) ([]domainassets.Def, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	limit, before = normalizeList(limit, before)
	return a.defs.List(ctx, projectID, includeArchived, limit, before)
}

// UpdateDef 更新定义（class/code 不可变；矩阵再校验）。
func (a *Assets) UpdateDef(ctx context.Context, cmd UpdateDefCommand) (*domainassets.Def, error) {
	if err := requireAssetWrite(ctx); err != nil {
		return nil, err
	}
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	if cmd.DefID == "" {
		return nil, status.Error(codes.InvalidArgument, "def_id is required")
	}
	def, err := a.defs.GetByID(ctx, projectID, cmd.DefID)
	if err != nil {
		return nil, err
	}
	if def == nil {
		return nil, status.Error(codes.NotFound, "asset def not found")
	}
	if cmd.Name != nil {
		if *cmd.Name == "" {
			return nil, status.Error(codes.InvalidArgument, "name is required")
		}
		def.Name = *cmd.Name
	}
	if cmd.Decimals != nil {
		def.Decimals = *cmd.Decimals
	}
	if cmd.ClearMax {
		def.MaxQuantity = nil
	} else if cmd.MaxQuantity != nil {
		def.MaxQuantity = cmd.MaxQuantity
	}
	if cmd.ClearExpiresIn {
		def.ExpiresIn = nil
	} else if cmd.ExpiresIn != nil {
		def.ExpiresIn = cmd.ExpiresIn
	}
	if cmd.Tradable != nil {
		def.Tradable = *cmd.Tradable
	}
	if cmd.UniquePerOwner != nil {
		def.UniquePerOwner = *cmd.UniquePerOwner
	}
	if cmd.Upgradeable != nil {
		def.Upgradeable = *cmd.Upgradeable
	}
	if cmd.Metadata != nil {
		def.Metadata = cmd.Metadata
	}
	if cmd.Status != nil {
		if !cmd.Status.IsValid() {
			return nil, status.Error(codes.InvalidArgument, "invalid status")
		}
		def.Status = *cmd.Status
	}
	if def.Class == domainassets.ClassEntitlement {
		def.Tradable = false
		def.UniquePerOwner = true
	}
	if err := domainassets.ValidateDefMatrix(def); err != nil {
		return nil, mapWriteError(err)
	}
	def.UpdatedAt = a.ts()
	if err := a.defs.Update(ctx, def); err != nil {
		return nil, err
	}
	return def, nil
}

// DeleteDef 归档定义（不硬删，流水仍引用 def_id）。
func (a *Assets) DeleteDef(ctx context.Context, defID string) error {
	if err := requireAssetWrite(ctx); err != nil {
		return err
	}
	archived := domainassets.DefStatusArchived
	_, err := a.UpdateDef(ctx, UpdateDefCommand{DefID: defID, Status: &archived})
	return err
}
