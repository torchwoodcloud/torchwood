// 目录与 schema 寻址：database CRUD（public 全局 catalog，redesign §4.2/C1）、
// 两段式 schema 解析、内部租户 id。
package documentdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/projectschema"
	"github.com/torchwooddev/torchwood/pkg/ident"
)

func (p *postgresDocumentDB) CreateDatabase(ctx context.Context, projectID, id, name string) error {
	schema, err := p.businessSchema(projectID, id)
	if err != nil {
		return p.mapError(err)
	}
	// R02-P1-2：schema 与 catalog 元数据包进同一事务，任一步失败整体回滚，
	// 避免"schema 已建而元数据缺失"。catalog 已全局化（public 两表），建库
	// 不再依赖项目数据面 schema；_perms 表已退役（阶段③包 A，_acl 内嵌行内）。
	return p.mapError(p.withOwnerTx(ctx, func(txCtx context.Context) error {
		if err := p.ensureSchema(txCtx, schema); err != nil {
			return err
		}
		m := &model.DocumentDatabase{
			ProjectID:  projectID,
			DatabaseID: id,
			Name:       name,
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}
		_, err := p.conn(txCtx).NewInsert().Model(m).Exec(txCtx)
		return err
	}))
}

func (p *postgresDocumentDB) GetDatabase(ctx context.Context, projectID, id string) (*databases.Database, error) {
	m := new(model.DocumentDatabase)
	err := p.conn(ctx).NewSelect().Model(m).
		Where("project_id = ? AND database_id = ?", projectID, id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, p.mapError(err)
	}
	return mapDatabase(m), nil
}

func (p *postgresDocumentDB) ListDatabases(ctx context.Context, projectID string) ([]databases.Database, error) {
	var ms []model.DocumentDatabase
	err := p.conn(ctx).NewSelect().Model(&ms).
		Where("project_id = ?", projectID).Order("created_at DESC").Scan(ctx)
	if err != nil {
		return nil, p.mapError(err)
	}
	out := make([]databases.Database, len(ms))
	for i := range ms {
		out[i] = *mapDatabase(&ms[i])
	}
	return out, nil
}

func (p *postgresDocumentDB) DeleteDatabase(ctx context.Context, projectID, id string) error {
	schema, err := p.businessSchema(projectID, id)
	if err != nil {
		return p.mapError(err)
	}
	if err := p.withOwnerTx(ctx, func(txCtx context.Context) error {
		if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, quoteIdent(schema))); err != nil {
			return err
		}
		// F4-2：物理对象删除后必须清理元数据，否则同名库/集合无法重建。
		// catalog_collections 的 FK ON DELETE CASCADE 是兜底，显式删除保持
		// 与删除路径同事务的清晰边界。
		if _, err := p.conn(txCtx).NewDelete().Model((*model.DocumentCollection)(nil)).
			Where("project_id = ? AND database_id = ?", projectID, id).Exec(txCtx); err != nil {
			return err
		}
		_, err := p.conn(txCtx).NewDelete().Model((*model.DocumentDatabase)(nil)).
			Where("project_id = ? AND database_id = ?", projectID, id).Exec(txCtx)
		return err
	}); err != nil {
		return p.mapError(err)
	}
	// 物理名缓存（B13c）：库已删，批量失效该库全部集合键（同名库重建后的
	// 新集合得新物理名）。
	p.dropDatabasePhysicalNames(projectID, id)
	return nil
}

// EnsureCatalog 对项目数据面执行 projectschema.Apply。职责已收缩（阶段②包 A）：
// catalog 元数据在 public 全局两表（000003），此处仅确保项目 schema 存在——
// sentinel 系统集合的物理表寄居 tw_<project>，建集合前必须就绪；业务库两段式
// schema 与项目数据面无依赖，不走本函数。
func (p *postgresDocumentDB) EnsureCatalog(ctx context.Context, projectID string) error {
	return p.mapError(mapIdentError(projectschema.Apply(ctx, p.db, projectID)))
}

// documentSchema 解析文档读写 / CreateCollection 的目标 schema。
// 仅此处允许 sentinel → ProjectSchemaName（一段式 tw_<project>），
// 供测试在项目数据面重建旧文档表；生产入口已拒绝 database_id="_"。
func (p *postgresDocumentDB) documentSchema(ctx context.Context, projectID, databaseID string) (int64, string, error) {
	internalID, err := p.resolveInternalID(ctx, projectID)
	if err != nil {
		return 0, "", err
	}
	if databaseID == ident.ProjectDataPlaneID {
		schema, err := ident.ProjectSchemaName(projectID)
		return internalID, schema, mapIdentError(err)
	}
	schema, err := ident.SchemaName(projectID, databaseID)
	return internalID, schema, mapIdentError(err)
}

// businessSchema 供 CreateDatabase / DeleteDatabase：只许两段式。显式拒
// sentinel、拒一段式，DROP SCHEMA 永不映射到 tw_<project>。
func (p *postgresDocumentDB) businessSchema(projectID, databaseID string) (string, error) {
	if databaseID == ident.ProjectDataPlaneID {
		return "", status.Error(codes.InvalidArgument, "database_id is reserved")
	}
	schema, err := ident.SchemaName(projectID, databaseID)
	if err != nil {
		return "", mapIdentError(err)
	}
	if !ident.IsTwoSegmentSchema(schema) {
		return "", status.Error(codes.Internal, "refusing to DDL a non two-segment schema")
	}
	if one, err := ident.ProjectSchemaName(projectID); err == nil && schema == one {
		return "", status.Error(codes.Internal, "refusing to DROP/CREATE project data-plane schema")
	}
	return schema, nil
}

// resolvePhysicalTable 把 (project, database, collection) 解析为行查询/DDL 的
// 物理寻址（internalID, schema, 物理表名）。阶段②包 B（redesign §4.2 标识符
// 治理，预决策 4）：
//   - sentinel 直通逻辑名（物理表即 tw_<project> 静态表，零额外查询）；
//   - 业务库单条 catalog 点查取 physical_name——不依赖 GetCollection 返回的
//     coll 对象，System/bypass 聚合与列表路径（跳过 GetCollection）同样可用；
//   - 行缺失 → NotFound（物理名是内部实现细节，物理表与 catalog 行同生共死）。
//
// 物理名进程内缓存（B13c，转出 POC 落地；阶段②预决策 4 的"评估后置"收口）：
// 点查实测占业务查询单次往返的 ~26%（≥5% 判据 → 缓存），热路径命中后零额外
// 往返。失效面 = catalog_collections 的全部删除路径（DeleteCollection /
// DeleteDatabase / import 清位）+ CreateCollection 写穿覆盖；跨实例陈旧语义
// fail-loud（42P01 表不存在），无静默错写。
func (p *postgresDocumentDB) resolvePhysicalTable(ctx context.Context, projectID, databaseID, collectionID string) (int64, string, string, error) {
	internalID, schema, err := p.documentSchema(ctx, projectID, databaseID)
	if err != nil {
		return 0, "", "", err
	}
	if databaseID == ident.ProjectDataPlaneID {
		return internalID, schema, collectionID, nil
	}
	if cached, ok := p.physicalNameCache.Load(physicalNameCacheKey(projectID, databaseID, collectionID)); ok {
		physical, _ := cached.(string)
		return internalID, schema, physical, nil
	}
	var physical string
	err = p.conn(ctx).NewSelect().Model((*model.DocumentCollection)(nil)).
		Column("physical_name").
		Where("project_id = ? AND database_id = ? AND collection_id = ?", projectID, databaseID, collectionID).
		Scan(ctx, &physical)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", "", status.Error(codes.NotFound, "collection not found")
		}
		return 0, "", "", p.mapError(err)
	}
	p.physicalNameCache.Store(physicalNameCacheKey(projectID, databaseID, collectionID), physical)
	return internalID, schema, physical, nil
}

// physicalNameCacheKey 是 physicalNameCache 的键（\x1f 分隔，键段自身不含
// 该控制字符——project/database/collection ID 校验均限可见字符）。
func physicalNameCacheKey(projectID, databaseID, collectionID string) string {
	return projectID + "\x1f" + databaseID + "\x1f" + collectionID
}

// storePhysicalName 在 CreateCollection 提交后写穿覆盖（同名逻辑 ID 重建必得
// 新物理名，覆盖即失效）。
func (p *postgresDocumentDB) storePhysicalName(projectID, databaseID, collectionID, physical string) {
	p.physicalNameCache.Store(physicalNameCacheKey(projectID, databaseID, collectionID), physical)
}

// dropPhysicalName 在 DeleteCollection / import 清位后失效单键。
func (p *postgresDocumentDB) dropPhysicalName(projectID, databaseID, collectionID string) {
	p.physicalNameCache.Delete(physicalNameCacheKey(projectID, databaseID, collectionID))
}

// dropDatabasePhysicalNames 在 DeleteDatabase 后失效该库全部集合键。
func (p *postgresDocumentDB) dropDatabasePhysicalNames(projectID, databaseID string) {
	prefix := projectID + "\x1f" + databaseID + "\x1f"
	p.physicalNameCache.Range(func(k, _ any) bool {
		if ks, ok := k.(string); ok && strings.HasPrefix(ks, prefix) {
			p.physicalNameCache.Delete(k)
		}
		return true
	})
}

func tableName(schema, collectionID string) string {
	return quoteIdent(schema) + "." + quoteIdent(collectionID)
}

// InternalIDCacheInvalidator 是 internalIDCache 的进程内失效面：组合根把
// projectschema manager 的 Invalidate 桥接到此（结构化接口，避免
// documentdb ↔ projectschema 反向依赖）。Round4 J5-3。
type InternalIDCacheInvalidator interface {
	InvalidateInternalIDCache(projectID string)
}

// InvalidateInternalIDCache 清除项目的 internal_id 进程内缓存。项目删除后
// 同 ID 重建时，陈旧缓存的 internal_id 会以错误 _tenant 标签写入新实例，
// 造成静默数据分裂（audit §B-P3）。
func (p *postgresDocumentDB) InvalidateInternalIDCache(projectID string) {
	p.internalIDCache.Delete(projectID)
}

// resolveInternalIDFresh 绕过缓存强制重解析并回填。DDL 会把 internal_id 烤进
// collection 表的 _tenant 列默认值（createCollectionTable），若此刻缓存陈旧
// （项目删除重建窗口），整张新表的默认租户都会错（R4-J5-3），建表路径必须
// 取实时值。
func (p *postgresDocumentDB) resolveInternalIDFresh(ctx context.Context, projectID string) (int64, error) {
	p.internalIDCache.Delete(projectID)
	return p.resolveInternalID(ctx, projectID)
}

func (p *postgresDocumentDB) resolveInternalID(ctx context.Context, projectID string) (int64, error) {
	if cached, ok := p.internalIDCache.Load(projectID); ok {
		return cached.(int64), nil
	}
	var internalID int64
	err := p.conn(ctx).NewSelect().Model((*model.Project)(nil)).Column("internal_id").Where("id = ?", projectID).Scan(ctx, &internalID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("project not found: %s", projectID)
		}
		return 0, p.mapError(err)
	}
	p.internalIDCache.Store(projectID, internalID)
	return internalID, nil
}

func mapDatabase(m *model.DocumentDatabase) *databases.Database {
	if m == nil {
		return nil
	}
	return &databases.Database{
		ID:        m.DatabaseID,
		ProjectID: m.ProjectID,
		Name:      m.Name,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}
