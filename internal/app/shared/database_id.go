package shared

import (
	"errors"

	"github.com/torchwoodcloud/torchwood/pkg/ident"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MapIdentError 将 pkg/ident 的 charset 哨兵映射为 InvalidArgument。
func MapIdentError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ident.ErrInvalidSchemaResourceID) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return err
}

// RejectExternalDatabaseID 校验对外 database_id：charset 合法且不是项目数据面
// sentinel。挂到 Server/Client 库/集合/文档/事务入口。charset 已拒 `_`，显式
// 比较是皮带（与 ident.ValidateSchemaResourceID 同为 InvalidArgument）。
func RejectExternalDatabaseID(id string) error {
	if err := ident.ValidateSchemaResourceID(id); err != nil {
		return MapIdentError(err)
	}
	if id == ident.ProjectDataPlaneID {
		return status.Error(codes.InvalidArgument, "database_id is reserved")
	}
	return nil
}
