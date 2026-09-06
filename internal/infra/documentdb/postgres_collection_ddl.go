// 集合/属性/索引 DDL 与 catalog 元数据（public 全局两表，redesign §4.2/C1）：
// 建表、_acl 内嵌列 + GIN、_version 列生命周期、索引表达式构建（fulltext 对齐见
// W-E）。attrs/indexes 以 JSONB 合一：GetCollection 单查询读回全量契约（含 default）。
// _perms 表已退役（阶段③包 A）：文档 ACE 内嵌 _acl 列，存量死表不迁移。
package documentdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/pkg/crud"
	"github.com/torchwooddev/torchwood/pkg/ident"
)

func (p *postgresDocumentDB) CreateCollection(ctx context.Context, projectID, databaseID, collectionID, name string, attrs []databases.Attribute, idxs []databases.Index, perms []databases.Permission, documentSecurity bool) error {
	// sentinel 只允许系统名单集合（测试重建旧文档表）；生产入口已
	// RejectExternalDatabaseID，不得对 "_" 建业务集合。测试播种走停 000008
	// 的 fixture（静态表保持 sys_* staging 名），此处不做 EnsureCatalog——
	// Apply 会执行 000009 把 sys_* rename 成最终名，与播种表撞名；schema
	// 就绪由 fixture 的 ApplyUpTo / CreateProject 全量 Apply 保证。
	if databaseID == ident.ProjectDataPlaneID && !databases.IsSystemCollectionID(collectionID) {
		return status.Error(codes.InvalidArgument, "only system collections may be created in the project data plane")
	}
	// 长度二道防线（app 层已校验）：表名/列名 ≤63；索引名拼接 idx_<coll>_<id>
	// 与默认时间索引 idx_<coll>_tenant_created 均不得超 63（PG 截断防护）。
	if err := validatePhysicalNameLen("collection id", collectionID); err != nil {
		return p.mapError(err)
	}
	for _, attr := range attrs {
		if err := validateArrayAttribute(attr); err != nil {
			return p.mapError(err)
		}
		if err := validateVectorAttribute(attr); err != nil {
			return p.mapError(err)
		}
		if err := validatePhysicalNameLen("attribute key", attr.Key); err != nil {
			return p.mapError(err)
		}
	}
	for _, idx := range idxs {
		if err := validateIndexDefinition(idx); err != nil {
			return p.mapError(err)
		}
		if err := validateIndexNameLen(collectionID, idx.ID); err != nil {
			return p.mapError(err)
		}
	}
	if err := validateIndexNameLen(collectionID, "tenant_created"); err != nil {
		return p.mapError(err)
	}
	internalID, schema, err := p.documentSchema(ctx, projectID, databaseID)
	if err != nil {
		return p.mapError(err)
	}
	// R4-J5-3：_tenant 列默认值取实时 internal_id，防陈旧缓存烤进新表。
	if internalID, err = p.resolveInternalIDFresh(ctx, projectID); err != nil {
		return p.mapError(err)
	}

	// DDL 与 catalog 元数据包进同一事务（PG 支持事务内 DDL），任一步失败
	// 整体回滚，避免"物理表建成而元数据缺失"。
	var physicalName string
	txErr := p.withOwnerTx(ctx, func(txCtx context.Context) error {
		if err := p.ensureSchema(txCtx, schema); err != nil {
			return err
		}
		physical, _, err := p.insertCollectionMetadata(txCtx, projectID, databaseID, collectionID, name, attrs, idxs, perms, documentSecurity)
		if err != nil {
			return err
		}
		physicalName = physical
		// 物理表名 = collectionID（2026-09-06 勘误），索引名前缀段即逻辑 ID；
		// idx_<coll>_<id> 组合长度由入口与上方的 validateIndexNameLen 把关
		//（63 字节截断类缺陷仍被机制性拒绝，而非自然消失）。
		for _, idx := range idxs {
			if err := validateIndexNameLen(physical, idx.ID); err != nil {
				return err
			}
		}
		if err := validateIndexNameLen(physical, "tenant_created"); err != nil {
			return err
		}
		isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
		if err := p.createCollectionTable(txCtx, schema, physical, internalID, attrs, isSystem); err != nil {
			return err
		}
		// CREATE TABLE IF NOT EXISTS 不会给存量表补列；DDL 路径一次 reconcile。
		if err := p.reconcileVersionColumn(txCtx, schema, physical, isSystem); err != nil {
			return err
		}
		for _, idx := range idxs {
			if err := p.createCollectionIndex(txCtx, schema, physical, idx, attrs); err != nil {
				return err
			}
		}
		return nil
	})
	if txErr == nil && physicalName != "" {
		// vector 列缓存（会话 #10）：DDL 提交后刷新（事务内不缓存，回滚撤销）。
		p.storeVectorColumns(schema, physicalName, attrs)
		// 物理名缓存（B13c）：写穿覆盖——删建同逻辑 ID 后旧键若残留（跨实例
		// 语义），此处收敛为本实例新名。
		if databaseID != ident.ProjectDataPlaneID {
			p.storePhysicalName(projectID, databaseID, collectionID, physicalName)
		}
	}
	return p.mapError(txErr)
}

// insertCollectionMetadata 写入 catalog_collections 合一行（attrs/indexes/
// permissions JSONB）。physical_name 是 collectionID 的冗余投影（2026-09-06
// 勘误：阶段②随机物理名退役，物理表名 = 逻辑 collectionID，运维可读；redesign
// §4.2 标识符治理勘误登记），sentinel 系统集合同理（静态表名 = 逻辑名）。
// 系统集合命中既有行时幂等成功（复用既有行走后续 IF NOT EXISTS DDL），用户
// 集合返回 ErrDuplicateKey（→ AlreadyExists）。
func (p *postgresDocumentDB) insertCollectionMetadata(ctx context.Context, projectID, databaseID, collectionID, name string, attrs []databases.Attribute, idxs []databases.Index, perms []databases.Permission, documentSecurity bool) (string, bool, error) {
	attrsJSON, err := encodeAttributes(attrs)
	if err != nil {
		return "", false, err
	}
	idxsJSON, err := encodeIndexes(idxs)
	if err != nil {
		return "", false, err
	}
	permsJSON, err := encodePermissions(perms)
	if err != nil {
		return "", false, err
	}
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	now := time.Now()
	m := &model.DocumentCollection{
		ProjectID:        projectID,
		DatabaseID:       databaseID,
		CollectionID:     collectionID,
		Name:             name,
		PhysicalName:     collectionID,
		DocumentSecurity: documentSecurity,
		IsSystem:         isSystem,
		Permissions:      permsJSON,
		Attrs:            attrsJSON,
		Indexes:          idxsJSON,
		SchemaVersion:    1,
		DDLSeq:           1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	res, err := p.conn(ctx).NewInsert().Model(m).
		On("CONFLICT (project_id, database_id, collection_id) DO NOTHING").Exec(ctx)
	if err != nil {
		return "", false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if affected > 0 {
		return collectionID, false, nil
	}
	// 集合行已存在：系统集合视为幂等成功（并发首请求防护，物理表走后续
	// IF NOT EXISTS DDL）；用户集合返回 ErrDuplicateKey。
	if !isSystem {
		return "", false, ErrDuplicateKey
	}
	return collectionID, true, nil
}

func (p *postgresDocumentDB) GetCollection(ctx context.Context, projectID, databaseID, collectionID string) (*databases.Collection, error) {
	m := new(model.DocumentCollection)
	err := p.conn(ctx).NewSelect().Model(m).
		Where("project_id = ? AND database_id = ? AND collection_id = ?", projectID, databaseID, collectionID).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, p.mapError(err)
	}
	coll, mapErr := mapCollectionRow(m)
	if mapErr != nil {
		return nil, p.mapError(mapErr)
	}
	return coll, nil
}

func (p *postgresDocumentDB) ListCollections(ctx context.Context, projectID, databaseID string, q databases.ListQuery) ([]databases.Collection, databases.ListMeta, error) {
	pageSize := int(q.PageSize)
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > maxQueryLimit {
		pageSize = maxQueryLimit
	}
	offset := 0
	if q.PageToken != "" {
		off, err := crud.DecodePageToken(q.PageToken)
		if err != nil {
			return nil, databases.ListMeta{}, p.mapError(err)
		}
		offset = off
	}

	count, err := p.conn(ctx).NewSelect().Model((*model.DocumentCollection)(nil)).
		Where("project_id = ? AND database_id = ?", projectID, databaseID).Count(ctx)
	if err != nil {
		return nil, databases.ListMeta{}, p.mapError(err)
	}
	total := int64(count)

	var ms []model.DocumentCollection
	err = p.conn(ctx).NewSelect().Model(&ms).
		Where("project_id = ? AND database_id = ?", projectID, databaseID).
		Order("created_at DESC").
		Limit(pageSize).Offset(offset).Scan(ctx)
	if err != nil {
		return nil, databases.ListMeta{}, p.mapError(err)
	}
	if len(ms) == 0 {
		return []databases.Collection{}, databases.ListMeta{TotalCount: total}, nil
	}
	out := make([]databases.Collection, len(ms))
	for i := range ms {
		c, err := mapCollectionRow(&ms[i])
		if err != nil {
			return nil, databases.ListMeta{}, p.mapError(err)
		}
		out[i] = *c
	}
	meta := databases.ListMeta{TotalCount: total}
	if offset+len(ms) < int(total) {
		token, err := crud.EncodePageToken(offset + len(ms))
		if err != nil {
			return nil, databases.ListMeta{}, status.Error(codes.Internal, err.Error())
		}
		meta.NextPageToken = token
	}
	return out, meta, nil
}

func (p *postgresDocumentDB) DeleteCollection(ctx context.Context, projectID, databaseID, collectionID string) error {
	_, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return p.mapError(err)
	}
	delErr := p.withOwnerTx(ctx, func(txCtx context.Context) error {
		// 物理表按服务端分配名 DROP（逻辑/物理名解耦，预决策 2）；_acl 内嵌表内
		//（阶段③包 A），DROP 即随行消亡，无跨表权限残留可清理。
		if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, tableName(schema, physical))); err != nil {
			return err
		}
		// F4-2：物理表删除后同步清理 catalog 行（attrs/indexes 合一行随行消
		// 失），否则删了建不回来。
		_, err := p.conn(txCtx).NewDelete().Model((*model.DocumentCollection)(nil)).
			Where("project_id = ? AND database_id = ? AND collection_id = ?", projectID, databaseID, collectionID).Exec(txCtx)
		return err
	})
	if delErr == nil {
		// vector 列缓存（会话 #10）：表已删，清键防陈旧维度残留。
		p.dropVectorColumns(schema, physical)
		// 物理名缓存（B13c）：删建同逻辑 ID 必得新物理名，陈旧键必须失效。
		p.dropPhysicalName(projectID, databaseID, collectionID)
	}
	return p.mapError(delErr)
}

func (p *postgresDocumentDB) UpdateCollection(ctx context.Context, projectID, databaseID, collectionID string, patch databases.CollectionPatch) error {
	if _, err := p.resolveInternalID(ctx, projectID); err != nil {
		return p.mapError(err)
	}
	// 权限替换与字段更新同一事务（任一失败整体回滚，避免"权限已换、
	// 元数据未更"的半更新）；权限-only 变更同样刷 updated_at（审计列统一）。
	// ddl_seq CAS（阶段②包 B，预决策 6）：并发 schema 变更先行提交 → 0 行
	// 受影响 → ErrDDLConflict（CATALOG.DDL_CONFLICT，retryable）。
	return p.mapError(p.withOwnerTx(ctx, func(txCtx context.Context) error {
		// 空 patch（无权限、无字段）保持 no-op，不读行、不刷审计列。
		if patch.Permissions == nil && patch.Name == "" && patch.DocumentSecurity == nil && patch.Disabled == nil {
			return nil
		}
		row, err := p.loadCollectionRow(txCtx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		var sets []string
		var args []any
		if patch.Permissions != nil {
			permsJSON, err := encodePermissions(*patch.Permissions)
			if err != nil {
				return err
			}
			sets = append(sets, "permissions = ?")
			args = append(args, permsJSON)
		}
		if patch.Name != "" {
			sets = append(sets, "name = ?")
			args = append(args, patch.Name)
		}
		if patch.DocumentSecurity != nil {
			sets = append(sets, "document_security = ?")
			args = append(args, *patch.DocumentSecurity)
		}
		if patch.Disabled != nil {
			sets = append(sets, "disabled = ?")
			args = append(args, *patch.Disabled)
		}
		sets = append(sets, "updated_at = ?", "ddl_seq = ddl_seq + 1")
		args = append(args, time.Now(), projectID, databaseID, collectionID, row.DDLSeq)
		res, err := p.conn(txCtx).ExecContext(txCtx,
			`UPDATE catalog_collections SET `+strings.Join(sets, ", ")+
				` WHERE project_id = ? AND database_id = ? AND collection_id = ? AND ddl_seq = ?`,
			args...,
		)
		if err != nil {
			return err
		}
		return requireCASApplied(res)
	}))
}

// requireCASApplied 校验 ddl_seq CAS UPDATE 的受影响行数；0 行 = 并发 schema
// 变更先行提交（预决策 6）。
func requireCASApplied(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: concurrent schema modification", databases.ErrDDLConflict)
	}
	return nil
}

// loadCollectionRow 取 catalog_collections 合一行；行缺失 → NotFound（含
// attrs/indexes 供 read-modify-write 路径复用）。
func (p *postgresDocumentDB) loadCollectionRow(ctx context.Context, projectID, databaseID, collectionID string) (*model.DocumentCollection, error) {
	m := new(model.DocumentCollection)
	err := p.conn(ctx).NewSelect().Model(m).
		Where("project_id = ? AND database_id = ? AND collection_id = ?", projectID, databaseID, collectionID).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "collection not found")
		}
		return nil, err
	}
	return m, nil
}

func (p *postgresDocumentDB) CreateAttribute(ctx context.Context, projectID, databaseID, collectionID string, attr databases.Attribute) error {
	if _, err := p.resolveInternalID(ctx, projectID); err != nil {
		return p.mapError(err)
	}
	// 第二道防线（app 层已校验）：直调 adapter 也不得把系统保留列（含 _version）
	// 当作用户属性 ADD COLUMN——否则会与 OCC 列类型/语义冲突。
	if _, ok := databases.ReservedAttributeKeys[attr.Key]; ok {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("attribute key %q is reserved", attr.Key))
	}
	// 物理列防线（app 层已校验）：array=true 仅限标量元素子集（阶段③-b）。
	if err := validateArrayAttribute(attr); err != nil {
		return err
	}
	// vector 属性第二道防线（会话 #10）：dims 域、array/default 拒绝。
	if err := validateVectorAttribute(attr); err != nil {
		return err
	}
	// 长度二道防线（app 层已校验）：物理列名 ≤63（PG 截断防护）。
	if err := validatePhysicalNameLen("attribute key", attr.Key); err != nil {
		return err
	}
	_, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return p.mapError(err)
	}
	colSQL, err := attributeColumnSQL(attr)
	if err != nil {
		return p.mapError(err)
	}
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	txErr := p.withOwnerTx(ctx, func(txCtx context.Context) error {
		if err := p.reconcileVersionColumn(txCtx, schema, physical, isSystem); err != nil {
			return err
		}
		if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s`, tableName(schema, physical), colSQL)); err != nil {
			return err
		}
		// ADD COLUMN 之后重刷列级 GRANT（会话 #10）：reconcileVersionColumn 的
		// 授权清单在加列前生成，新列（含 vector）若无 INSERT/UPDATE 列授权，
		// tw_app 的后续写入会 42501——既有滞后缺陷随 vector 列一并修复。
		if !isSystem {
			if err := p.refreshColumnGrants(txCtx, schema, physical); err != nil {
				return err
			}
		}
		row, err := p.loadCollectionRow(txCtx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		attrs, err := decodeAttributes(row.Attrs)
		if err != nil {
			return err
		}
		// key 与 ID 唯一性由合一行内校验承载（旧四表靠 UNIQUE 约束 23505）。
		for _, a := range attrs {
			if a.Key == attr.Key || a.ID == attr.ID {
				return fmt.Errorf("%w: attribute %q already exists", ErrDuplicateKey, attr.Key)
			}
		}
		// default 与 DDL（attributeColumnSQL 的 DEFAULT）同源落 catalog：物理列
		// 生效但元数据缺失曾是契约断裂（GetCollection 读不回 default）。
		attrs = append(attrs, attr)
		attrsJSON, err := encodeAttributes(attrs)
		if err != nil {
			return err
		}
		res, err := p.conn(txCtx).ExecContext(txCtx,
			`UPDATE catalog_collections SET attrs = ?, updated_at = ?, ddl_seq = ddl_seq + 1 WHERE project_id = ? AND database_id = ? AND collection_id = ? AND ddl_seq = ?`,
			attrsJSON, time.Now(), projectID, databaseID, collectionID, row.DDLSeq)
		if err != nil {
			return err
		}
		if err := requireCASApplied(res); err != nil {
			return err
		}
		// vector 列缓存：事务内不写（回滚撤销），提交后由调用点刷新。
		return nil
	})
	if txErr == nil {
		// catalog attrs 已含新列；vectorColumns 缓存重建（无 vector 列时也
		// 覆盖为空 map，保证与已提交态一致）。
		row, err := p.loadCollectionRow(ctx, projectID, databaseID, collectionID)
		if err == nil {
			attrs, derr := decodeAttributes(row.Attrs)
			if derr == nil {
				p.storeVectorColumns(schema, physical, attrs)
			}
		}
	}
	return p.mapError(txErr)
}

func (p *postgresDocumentDB) CreateIndex(ctx context.Context, projectID, databaseID, collectionID string, idx databases.Index) error {
	if err := validateIndexDefinition(idx); err != nil {
		return p.mapError(err)
	}
	// 长度二道防线（app 层已校验）：索引名拼接 idx_<coll>_<id> ≤63。
	if err := validateIndexNameLen(collectionID, idx.ID); err != nil {
		return p.mapError(err)
	}
	_, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return p.mapError(err)
	}
	// sentinel 系统集合（静态平面，测试面）：走既有事务内通道（无 RLS/无并发
	// 读者语义，CONCURRENTLY 无意义）；生产入口已拒绝系统集合建索引。
	if databaseID == ident.ProjectDataPlaneID {
		return p.createIndexInTx(ctx, projectID, databaseID, collectionID, schema, physical, idx)
	}
	// 用户集合（B3 在线通道，预决策 1）：两阶段状态机——存量表有并发读者，
	// 事务内 CREATE INDEX 持 SHARE 锁会阻塞全部读写，CONCURRENTLY 不能在事务
	// 内运行，故拆 事务A(building) → 事务外 CIC → 事务B(active|failed)。
	// 注：事务 A 纯 catalog DML（postgres_online_ddl.go 注释）；存量表自愈
	//（默认索引/RLS/列授权）由 reconcile 扫描承担，不再搭车 DDL touch。
	if _, err := p.indexBeginBuilding(ctx, projectID, databaseID, collectionID, idx); err != nil {
		return p.mapError(err)
	}
	// 形态约束（vector/数组/hnsw/fulltext）在 CIC 前以事务内通道同源校验一次
	//（buildIndexStatement 与事务内通道共用；此处 dry-run 形态校验用——真实
	// 执行在事务外）。校验失败直接落 failed，不等 CIC 在独立连接上碰壁。
	attrs, defErr := p.collectionAttrsForIndex(ctx, projectID, databaseID, collectionID)
	if defErr == nil {
		if _, defErr = buildIndexStatement(schema, physical, idx, attrs, false); defErr != nil {
			_, _ = p.indexSetStatus(ctx, projectID, databaseID, collectionID, idx.ID, databases.IndexStatusFailed)
			return p.mapError(defErr)
		}
	}
	if err := p.createIndexConcurrently(ctx, schema, physical, idx, attrs); err != nil {
		// 失败清理：CIC 残留的 INVALID 索引必须删（pg 语义不清除），catalog
		// 置 failed（reconcile/repair 可重入）。两步各自尽力——清理失败仅记日志。
		if _, dropErr := p.execOwnerStatement(ctx, dropIndexStatement(schema, physicalIndexName(physical, idx.ID))); dropErr != nil {
			slog.Error("drop invalid index after failed concurrent build",
				"schema", schema, "table", physical, "index", idx.ID, "error", dropErr)
		}
		if _, markErr := p.indexSetStatus(ctx, projectID, databaseID, collectionID, idx.ID, databases.IndexStatusFailed); markErr != nil {
			slog.Error("mark index failed after failed concurrent build",
				"schema", schema, "table", physical, "index", idx.ID, "error", markErr)
		}
		return p.mapError(err)
	}
	// 事务 B：active。条目被并发 DeleteIndex 移除时（found=false），物理索引
	// 已被对方 IF EXISTS 语义"删除未遂"——此处补删对齐。
	found, err := p.indexSetStatus(ctx, projectID, databaseID, collectionID, idx.ID, databases.IndexStatusActive)
	if err != nil {
		// CAS 冲突等：物理索引有效但 catalog 未推进——reconcile 会按 valid+building
		// 分流补账；返回原错误保语义。
		return p.mapError(err)
	}
	if !found {
		if _, dropErr := p.execOwnerStatement(ctx, dropIndexStatement(schema, physicalIndexName(physical, idx.ID))); dropErr != nil {
			slog.Warn("drop orphaned index after concurrent delete",
				"schema", schema, "table", physical, "index", idx.ID, "error", dropErr)
		}
	}
	return nil
}

// createIndexInTx 是 sentinel 系统集合的既有事务内通道（B3 前行为原样保留）。
func (p *postgresDocumentDB) createIndexInTx(ctx context.Context, projectID, databaseID, collectionID, schema, physical string, idx databases.Index) error {
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	return p.mapError(p.withOwnerTx(ctx, func(txCtx context.Context) error {
		if err := p.reconcileVersionColumn(txCtx, schema, physical, isSystem); err != nil {
			return err
		}
		// 先读 catalog 行：数组列的索引形态判定（GIN array_ops / unique 拒绝）
		// 需要 attrs（阶段③-b 预决策 2）。
		row, err := p.loadCollectionRow(txCtx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		attrs, err := decodeAttributes(row.Attrs)
		if err != nil {
			return err
		}
		if err := p.createCollectionIndex(txCtx, schema, physical, idx, attrs); err != nil {
			return err
		}
		idxs, err := decodeIndexes(row.Indexes)
		if err != nil {
			return err
		}
		for _, i := range idxs {
			if i.ID == idx.ID {
				return fmt.Errorf("%w: index %q already exists", ErrDuplicateKey, idx.ID)
			}
		}
		idxs = append(idxs, idx)
		idxsJSON, err := encodeIndexes(idxs)
		if err != nil {
			return err
		}
		res, err := p.conn(txCtx).ExecContext(txCtx,
			`UPDATE catalog_collections SET indexes = ?, updated_at = ?, ddl_seq = ddl_seq + 1 WHERE project_id = ? AND database_id = ? AND collection_id = ? AND ddl_seq = ?`,
			idxsJSON, time.Now(), projectID, databaseID, collectionID, row.DDLSeq)
		if err != nil {
			return err
		}
		return requireCASApplied(res)
	}))
}

// collectionAttrsForIndex 读 catalog attrs（在线通道的形态判定输入）。
func (p *postgresDocumentDB) collectionAttrsForIndex(ctx context.Context, projectID, databaseID, collectionID string) ([]databases.Attribute, error) {
	row, err := p.loadCollectionRow(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, err
	}
	return decodeAttributes(row.Attrs)
}

// execOwnerStatement 以 tw_owner 身份执行单条语句（事务外清理路径：DROP
// INDEX 等幂等 DDL；独立短事务）。
func (p *postgresDocumentDB) execOwnerStatement(ctx context.Context, stmt string) (sql.Result, error) {
	var res sql.Result
	err := p.withOwnerTx(ctx, func(txCtx context.Context) error {
		var err error
		res, err = p.conn(txCtx).ExecContext(txCtx, stmt)
		return err
	})
	return res, err
}

// ensureSchema 确保 schema 存在（阶段③包 B：仅当本调用真正创建时顺带授权——
// USAGE 给 tw_app/tw_system；tw_owner 是创建者自有全权。sentinel 项目数据面
// schema 由 projectschema 0012 授权，此处已存在即跳过，避免非 owner GRANT 报错）。
func (p *postgresDocumentDB) ensureSchema(ctx context.Context, schema string) error {
	var exists bool
	if err := p.conn(ctx).QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = ?)`, schema,
	).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := p.conn(ctx).ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, quoteIdent(schema))); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := p.conn(ctx).ExecContext(ctx, fmt.Sprintf(
		`GRANT USAGE ON SCHEMA %s TO %s, %s`, quoteIdent(schema), clients.RoleApp, clients.RoleSystem)); err != nil {
		return fmt.Errorf("grant schema usage: %w", err)
	}
	return nil
}

// createCollectionTable 建集合物理表（physical = 服务端分配的物理表名；
// sentinel 系统集合 = 逻辑名，指向 tw_<project> 静态表）。
// _acl TEXT[] 内嵌文档 ACE（阶段③包 A）：所有文档表带列（含 sentinel 测试表）；
// GIN 索引仅用户集合建（&& 匹配可走索引，redesign §3.2 工程纪律）。
func (p *postgresDocumentDB) createCollectionTable(ctx context.Context, schema, physical string, tenant int64, attrs []databases.Attribute, isSystem bool) error {
	cols := []string{
		"_id TEXT NOT NULL",
		fmt.Sprintf("_tenant BIGINT NOT NULL DEFAULT %d", tenant),
		"_created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()",
		"_updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()",
		"_created_by TEXT",
		"_updated_by TEXT",
		"_acl TEXT[] NOT NULL DEFAULT '{}'",
	}
	// 仅用户集合追加 _version（OCC）；系统集合不加列。
	if !isSystem {
		cols = append(cols, "_version BIGINT NOT NULL DEFAULT 1")
	}
	for _, attr := range attrs {
		colSQL, err := attributeColumnSQL(attr)
		if err != nil {
			return err
		}
		cols = append(cols, colSQL)
	}
	cols = append(cols, "PRIMARY KEY (_tenant, _id)")
	existed := false
	if !isSystem {
		var err error
		existed, err = p.collectionTableExists(ctx, schema, physical)
		if err != nil {
			return err
		}
	}
	sql := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n\t\t%s\n\t)", tableName(schema, physical), strings.Join(cols, ",\n\t\t"))
	_, err := p.conn(ctx).ExecContext(ctx, sql)
	createdHere := !isSystem && !existed
	if err != nil && isUniqueViolation(err) {
		// 并发建同名表时 PG 可能在 pg_type 类型注册上撞唯一约束（既有竞态，
		// CreateCollection 并发首请求触发）：表已由另一事务建成，
		// 重试一次即变为幂等 no-op。
		_, err = p.conn(ctx).ExecContext(ctx, sql)
		createdHere = false
	}
	if err != nil {
		return err
	}
	// 本事务新建且带 _version：打标，避免同事务写路径把未提交列写入 cache。
	if createdHere {
		p.markVersionAlterTx(ctx, schema, physical)
	}
	// R5-J3-1（D-P1-3）：用户集合默认时间索引——新建即建，存量表经
	// IF NOT EXISTS 幂等（系统表跳过，与 _version 列的处理一致）。
	// _acl GIN（阶段③包 A）同位：tw_can 的 && 匹配与 listPermissionFilter
	// 的 _acl 过滤都消费该索引。
	if !isSystem {
		if err := p.ensureTenantCreatedIndex(ctx, schema, physical); err != nil {
			return err
		}
		if err := p.ensureACLIndex(ctx, schema, physical); err != nil {
			return err
		}
		// RLS 判定执行点（阶段③包 C）：四条 policy + FORCE + 列级 GRANT
		//（tw_app 排除 _tenant 写）。
		if err := p.ensureCollectionRLS(ctx, schema, physical); err != nil {
			return err
		}
	} else {
		// sentinel 系统集合（测试面）：静态平面独立授权（预决策 9），不启 RLS，
		// 保持表级授权。
		if _, err := p.conn(ctx).ExecContext(ctx, fmt.Sprintf(
			`GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO %s`,
			tableName(schema, physical), clients.RoleApp)); err != nil {
			return fmt.Errorf("grant table to app role: %w", err)
		}
		if _, err := p.conn(ctx).ExecContext(ctx, fmt.Sprintf(
			`GRANT ALL ON %s TO %s`,
			tableName(schema, physical), clients.RoleSystem)); err != nil {
			return fmt.Errorf("grant table to system role: %w", err)
		}
	}
	return nil
}

func (p *postgresDocumentDB) collectionTableExists(ctx context.Context, schema, physical string) (bool, error) {
	var exists bool
	err := p.conn(ctx).QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = ? AND table_name = ?)`,
		schema, physical,
	).Scan(&exists)
	return exists, err
}

// markVersionAlterTx 记录"本事务内新建 _version 列"的键。增长模型：每条
// DDL 事务一条（txid|schema.物理表名），随 txid 单调累积、进程生命周期内
// 不清理——量级为每集合创建/加列各一次（远小于业务写路径），可接受。
func (p *postgresDocumentDB) markVersionAlterTx(ctx context.Context, schema, physical string) {
	key := schema + "." + physical
	alterKey := p.txid(ctx) + "|" + key
	if alterKey != "|"+key {
		p.versionAlterTx.Store(alterKey, struct{}{})
	}
}

// requireVersionColumn 供文档写路径使用：只检查 _version 是否为 bigint，**不 ALTER**。
// 缺列 → ErrVersionColumnUnavailable；类型冲突 → ErrVersionColumnConflict。
func (p *postgresDocumentDB) requireVersionColumn(ctx context.Context, schema, physical string, isSystem bool) error {
	if isSystem {
		return nil
	}
	ready, err := p.versionColumnReady(ctx, schema, physical)
	if err != nil {
		return err
	}
	if !ready {
		return databases.ErrVersionColumnUnavailable
	}
	return nil
}

// reconcileVersionColumn 是用户集合 _version 列的 DDL 对账，仅写 DDL 调用：
//
//   - isSystem == true：直接返回（系统集合无 _version）。
//   - 列不存在 → ALTER TABLE ADD COLUMN IF NOT EXISTS _version BIGINT NOT NULL DEFAULT 1
//     （存量行 DEFAULT 1 回填）。
//   - 列已存在且为 bigint → 就绪。
//   - 列已存在但非 bigint（存量用户属性抢占 _version）→ fail-closed：
//     返回 ErrVersionColumnConflict，禁止对错误类型做 _version = _version + 1。
//
// 缓存：仅非事务内见到已提交的 int8 才写入 versionColumns。事务内 CREATE TABLE /
// ALTER 新建的列只打 versionAlterTx，不写 versionColumns（回滚会撤销列）。
// 文档写路径不得调用本函数：ADD COLUMN 是 AccessExclusiveLock。
// 键以物理表名为准（逻辑/物理名解耦后同名逻辑集合不串缓存）。
//
// R5-J3-1：入口先幂等补建默认时间索引（ensureTenantCreatedIndex）、_acl GIN
//（ensureACLIndex，阶段③包 A）与 RLS policy + 列级 GRANT（ensureCollectionRLS，
// 阶段③包 C）——本函数是用户集合所有 DDL touch（CreateCollection/
// CreateAttribute/CreateIndex）的汇聚点，存量集合在下次任意 DDL touch 时自动
// 补齐；放在 versionColumns 缓存短路之前，保证每次 touch 都对账（幂等重建）。
func (p *postgresDocumentDB) reconcileVersionColumn(ctx context.Context, schema, physical string, isSystem bool) error {
	if isSystem {
		return nil
	}
	if err := p.ensureTenantCreatedIndex(ctx, schema, physical); err != nil {
		return err
	}
	if err := p.ensureACLIndex(ctx, schema, physical); err != nil {
		return err
	}
	if err := p.ensureCollectionRLS(ctx, schema, physical); err != nil {
		return err
	}
	key := schema + "." + physical
	if _, ok := p.versionColumns.Load(key); ok {
		return nil
	}
	alterKey := p.txid(ctx) + "|" + key
	if alterKey != "|"+key {
		if _, ok := p.versionAlterTx.Load(alterKey); ok {
			// 本事务已 ALTER：列存在且类型必为 bigint（本函数保证），直接放行。
			return nil
		}
	}
	udtName, err := p.versionColumnType(ctx, schema, physical)
	switch {
	case err == nil && udtName == "int8":
		// 事务内可能是本事务 CREATE TABLE 刚加的列，回滚会撤销，不得缓存。
		if !clients.InTx(ctx) {
			p.versionColumns.Store(key, struct{}{})
		}
		return nil
	case errors.Is(err, sql.ErrNoRows):
		tbl := tableName(schema, physical)
		if _, err := p.conn(ctx).ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS _version BIGINT NOT NULL DEFAULT 1`, tbl),
		); err != nil {
			return fmt.Errorf("add version column: %w", err)
		}
		p.markVersionAlterTx(ctx, schema, physical)
		return nil
	case err != nil:
		return err
	default:
		// 用户属性占用了 _version 且类型不是 bigint：拒绝 OCC，禁止拼接
		// _version = _version + 1（会撞类型错误或静默截断）。
		slog.Error("version column exists with unsupported type; OCC disabled fail-closed",
			"schema", schema, "table", physical, "udt_name", udtName)
		return databases.ErrVersionColumnConflict
	}
}

// txid 返回当前事务 ID（文本）；非事务上下文或查询失败时返回 ""。
// 仅 cache miss 时调用（information_schema 查询旁的一次轻量查询）。
func (p *postgresDocumentDB) txid(ctx context.Context) string {
	var id string
	if err := p.conn(ctx).QueryRowContext(ctx, `SELECT txid_current()::text`).Scan(&id); err != nil {
		return ""
	}
	return id
}

// versionColumnType 查询 information_schema 中 _version 列的 udt_name；
// 列不存在返回 (sql.ErrNoRows, "")。
func (p *postgresDocumentDB) versionColumnType(ctx context.Context, schema, physical string) (string, error) {
	var udtName string
	err := p.conn(ctx).QueryRowContext(ctx,
		`SELECT udt_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = '_version'`,
		schema, physical,
	).Scan(&udtName)
	return udtName, err
}

// versionColumnReady 供读路径 $version 与写路径 requireVersionColumn 使用：
// cache miss 时只查 information_schema（不 ALTER）。
// 返回：
//   - (true, nil)：列可用（bigint）。
//   - (false, nil)：列不存在（尚未 reconcile）→ 调用方返回 version_column_unavailable。
//   - (false, ErrVersionColumnConflict)：列存在但非 bigint（用户属性抢占，
//     与 DDL reconcile 同语义 fail-closed）。
//
// 缓存与 reconcileVersionColumn 不同：文档写总在事务内，见到事务开始前已存在的
// int8 必须缓存，否则每次写都查 information_schema。本事务 CREATE TABLE / ALTER
// 新建的列（versionAlterTx）只放行 SQL、不写 versionColumns。
func (p *postgresDocumentDB) versionColumnReady(ctx context.Context, schema, physical string) (bool, error) {
	key := schema + "." + physical
	if _, ok := p.versionColumns.Load(key); ok {
		return true, nil
	}
	alterKey := p.txid(ctx) + "|" + key
	if alterKey != "|"+key {
		if _, ok := p.versionAlterTx.Load(alterKey); ok {
			// 本事务新建的列（未提交）：SQL 可用但不缓存。
			return true, nil
		}
	}
	udtName, err := p.versionColumnType(ctx, schema, physical)
	switch {
	case err == nil && udtName == "int8":
		p.versionColumns.Store(key, struct{}{})
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	default:
		// 读路径遇非 bigint _version：与写路径同码 fail-closed。
		return false, databases.ErrVersionColumnConflict
	}
}

// createCollectionIndex 建物理索引（事务内通道，预决策 1：仅建集合时的既有
// 索引与 sentinel 系统集合走此路径——新表无并发读者，CONCURRENTLY 无意义）。
// 表与索引名前缀均用物理表名（idx_<phys>_<id> 自然 ≤63，预决策 2）。
// 数组列（阶段③-b 预决策 2）：key 索引自动选 GIN array_ops（&&/@> 可走索引）；
// unique 对数组列拒绝（PG 数组无唯一约束语义）；fulltext 对数组列拒绝
//（search 编译 col::text 是整列文本投影，数组列语义误导）；数组列仅支持
// 单列索引（GIN 多列混排非数组列需 btree_gin 扩展，不引入）。
// hnsw（会话 #10 预决策 2）：目标列必须是声明的 vector 属性（非 vector 列/
// 系统列拒绝）；opclass 按 metric 映射（cosine/l2/ip）；同列可建多 metric
// 索引（索引 ID 唯一即可）；unique/fulltext 对 vector 列拒绝。
// SQL 表达式构建单源于 buildIndexStatement（B3：事务内/CONCURRENTLY 两通道
// 共享同一构建器，防形态漂移）；存量表的用户索引走 postgres_online_ddl.go
// 的两阶段 CONCURRENTLY 通道。
func (p *postgresDocumentDB) createCollectionIndex(ctx context.Context, schema, physical string, idx databases.Index, attrs []databases.Attribute) error {
	stmt, err := buildIndexStatement(schema, physical, idx, attrs, false)
	if err != nil {
		return err
	}
	_, err = p.conn(ctx).ExecContext(ctx, stmt)
	return err
}

// ensureTenantCreatedIndex 为用户集合幂等补建默认时间索引
// `idx_<phys>_tenant_created ON <tbl>(_tenant, _created_at, _id)`（R5-J3-1，
// D-P1-3）：列表默认排序 `ORDER BY _created_at DESC, _id DESC` 与 keyset 谓词
// `(d._created_at, d._id) < (?, ?)` 均消费该序，缺索引时大集合每页全表扫描
// +排序。PG b-tree 可反向扫描，服务 DESC 无需 DESC 关键字。索引名前缀段
// 用物理表名（与 createCollectionIndex 的 `idx_<phys>_<id>` 方案一致，自然
// ≤63）；IF NOT EXISTS 幂等，DDL 路径重复执行仅一次 catalog 查找。
func (p *postgresDocumentDB) ensureTenantCreatedIndex(ctx context.Context, schema, physical string) error {
	idxName := quoteIdent(fmt.Sprintf("idx_%s_tenant_created", physical))
	sql := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (_tenant, _created_at, _id)`, idxName, tableName(schema, physical))
	if _, err := p.conn(ctx).ExecContext(ctx, sql); err != nil {
		return fmt.Errorf("create tenant_created index: %w", err)
	}
	return nil
}

// ensureACLIndex 为用户集合幂等补建 _acl 的 GIN 索引（阶段③包 A，redesign
// §3.2 工程纪律：_acl 建 GIN，&& 可作索引条件）。索引名前缀段用物理表名
//（idx_<phys>_acl 自然 ≤63，与 idx_<phys>_tenant_created 同方案）；IF NOT EXISTS
// 幂等，DDL 路径重复执行仅一次 catalog 查找。
func (p *postgresDocumentDB) ensureACLIndex(ctx context.Context, schema, physical string) error {
	idxName := quoteIdent(fmt.Sprintf("idx_%s_acl", physical))
	sql := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s USING gin (_acl)`, idxName, tableName(schema, physical))
	if _, err := p.conn(ctx).ExecContext(ctx, sql); err != nil {
		return fmt.Errorf("create acl index: %w", err)
	}
	return nil
}

// hnswOpClass 映射距离度量到 pgvector HNSW opclass（会话 #10 预决策 2）。
func hnswOpClass(metric string) (string, error) {
	switch strings.ToUpper(metric) {
	case "", "COSINE":
		return "vector_cosine_ops", nil
	case "L2":
		return "vector_l2_ops", nil
	case "INNER_PRODUCT":
		return "vector_ip_ops", nil
	default:
		return "", status.Error(codes.InvalidArgument,
			fmt.Sprintf("hnsw distance_metric must be COSINE, L2, or INNER_PRODUCT, got %q", metric))
	}
}

// validateIndexDefinition 拒绝与查询编译器不相容的索引定义：fulltext 查询
// 按单列 to_tsvector("col"::text) 编译，多列拼接索引的表达式与任何单字段
// 查询都不匹配——索引永不命中，只会误导用户以为 search 有索引支撑。
// hnsw（会话 #10）：单列 + metric 白名单 + orders 拒绝（app 层同语义校验，
// 此处是直调 adapter 的第二道防线）。
func validateIndexDefinition(idx databases.Index) error {
	switch strings.ToLower(idx.Type) {
	case "fulltext":
		if len(idx.Attributes) != 1 {
			return status.Error(codes.InvalidArgument, "fulltext index requires exactly one attribute")
		}
	case "hnsw":
		if len(idx.Attributes) != 1 {
			return status.Error(codes.InvalidArgument, "hnsw index requires exactly one attribute")
		}
		if len(idx.Orders) > 0 {
			return status.Error(codes.InvalidArgument, "hnsw index does not support orders")
		}
		if _, err := hnswOpClass(idx.DistanceMetric); err != nil {
			return err
		}
	}
	return nil
}

// validateArrayAttribute 是 catalog 写入前的第二道防线（阶段③-b 预决策 1）：
// array=true 已是合法属性（PG 原生 T[] 列），但元素类型仅限标量子集
// string/integer/float/boolean/datetime——email/url/json 不开放（app 层已校验，
// 直调 adapter 亦不得绕过）。系统保留列仍由 ReservedAttributeKeys 先行拒绝，
// "_acl 不可为数组" 由该检查保持。
func validateArrayAttribute(attr databases.Attribute) error {
	if !attr.Array {
		return nil
	}
	switch strings.ToLower(attr.Type) {
	case "string", "integer", "float", "boolean", "datetime":
		return nil
	default:
		return status.Error(codes.InvalidArgument, fmt.Sprintf(
			"attribute %q: array supports string, integer, float, boolean, datetime element types", attr.Key))
	}
}

// vectorDimsRange 是 vector 属性的维度合法域（会话 #10 预决策 1）：
// 2..2000——pgvector HNSW 索引可容纳的维度上限。
const (
	vectorDimsMin = 2
	vectorDimsMax = 2000
)

// validateVectorAttribute 是 vector 属性的第二道防线（app 层已校验）：
// dims 必填且在合法域；array=true 与 default 对 vector 拒绝（无向量缺省
// 语义、vector[] 无对应物理形态——pgvector 数组列不开放）。
func validateVectorAttribute(attr databases.Attribute) error {
	if strings.ToLower(attr.Type) != "vector" {
		if attr.Dims != 0 {
			return status.Error(codes.InvalidArgument, fmt.Sprintf(
				"attribute %q: dims is only valid for vector attributes", attr.Key))
		}
		return nil
	}
	if attr.Dims < vectorDimsMin || attr.Dims > vectorDimsMax {
		return status.Error(codes.InvalidArgument, fmt.Sprintf(
			"attribute %q: vector requires dims between %d and %d", attr.Key, vectorDimsMin, vectorDimsMax))
	}
	if attr.Array {
		return status.Error(codes.InvalidArgument, fmt.Sprintf(
			"attribute %q: vector attributes do not support array", attr.Key))
	}
	if attr.Default != nil {
		return status.Error(codes.InvalidArgument, fmt.Sprintf(
			"attribute %q: vector attributes do not support default_value", attr.Key))
	}
	return nil
}

func attributeColumnSQL(attr databases.Attribute) (string, error) {
	if !safeNameRe.MatchString(attr.Key) {
		return "", fmt.Errorf("invalid attribute key: %s", attr.Key)
	}
	name := quoteIdent(attr.Key)
	// 阶段③-b 预决策 1：array=true 落地 PG 原生数组列 T[]（元素类型标量子集，
	// validateArrayAttribute 已拦截其余）。数组列不带 DEFAULT（字面量按元素
	// 类型格式化复杂，POC 不开放——缺省即 NULL，语义干净）。
	// vector 列（会话 #10）：pgvector 原生 VECTOR(dims)；default_value 由
	// validateVectorAttribute 拒绝，此处无 DEFAULT 分支。
	dataType := pgTypeFor(attr.Type, attr.Size)
	if attr.Array {
		dataType = pgArrayTypeFor(attr.Type)
	}
	if strings.ToLower(attr.Type) == "vector" {
		dataType = fmt.Sprintf("VECTOR(%d)", attr.Dims)
	}
	parts := []string{name, dataType}
	if attr.Required {
		parts = append(parts, "NOT NULL")
	}
	if attr.Default != nil && !attr.Array {
		def, err := formatDefault(attr.Default, attr.Type)
		if err != nil {
			return "", fmt.Errorf("invalid default for attribute %s: %w", attr.Key, err)
		}
		parts = append(parts, fmt.Sprintf("DEFAULT %s", def))
	}
	return strings.Join(parts, " "), nil
}

// pgArrayTypeFor 返回数组列的 PG 类型（DDL 列类型与 ?::T[] 参数 cast 单源）。
// string 数组统一 TEXT[]（size 对数组元素不生效）；元素类型白名单见
// validateArrayAttribute。
func pgArrayTypeFor(t string) string {
	switch strings.ToLower(t) {
	case "integer":
		return "BIGINT[]"
	case "float":
		return "DOUBLE PRECISION[]"
	case "boolean":
		return "BOOLEAN[]"
	case "datetime":
		return "TIMESTAMPTZ[]"
	default:
		return "TEXT[]"
	}
}

func pgTypeFor(t string, size int) string {
	switch strings.ToLower(t) {
	case "string", "email", "url":
		if size > 0 && size <= 64000 {
			return fmt.Sprintf("VARCHAR(%d)", size)
		}
		return "TEXT"
	case "integer":
		return "BIGINT"
	case "float":
		return "DOUBLE PRECISION"
	case "boolean":
		return "BOOLEAN"
	case "datetime":
		return "TIMESTAMPTZ"
	case "json":
		return "JSONB"
	default:
		return "TEXT"
	}
}

func formatDefault(v any, t string) (string, error) {
	switch strings.ToLower(t) {
	case "boolean":
		b, err := strconv.ParseBool(fmt.Sprint(v))
		if err != nil {
			return "", fmt.Errorf("boolean default: %w", err)
		}
		if b {
			return "TRUE", nil
		}
		return "FALSE", nil
	case "integer":
		n, err := strconv.ParseInt(fmt.Sprint(v), 10, 64)
		if err != nil {
			return "", fmt.Errorf("integer default: %w", err)
		}
		return strconv.FormatInt(n, 10), nil
	case "float":
		f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
		if err != nil {
			return "", fmt.Errorf("float default: %w", err)
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	default:
		return quoteLiteral(fmt.Sprint(v)), nil
	}
}

func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// mapCollectionRow 从 catalog_collections 合一行解码 domain Collection
//（attrs/indexes/permissions JSONB 单源读回，default/size/array 全字段）。
// PhysicalName 是内部实现细节，不进 domain 形状（不出现在任何 API 响应）。
func mapCollectionRow(m *model.DocumentCollection) (*databases.Collection, error) {
	c := &databases.Collection{
		ID:               m.CollectionID,
		DatabaseID:       m.DatabaseID,
		ProjectID:        m.ProjectID,
		Name:             m.Name,
		DocumentSecurity: m.DocumentSecurity,
		Disabled:         m.Disabled,
		IsSystem:         m.IsSystem,
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
	}
	perms, err := decodePermissions(m.Permissions)
	if err != nil {
		return nil, err
	}
	c.Permissions = perms
	attrs, err := decodeAttributes(m.Attrs)
	if err != nil {
		return nil, err
	}
	c.Attributes = attrs
	idxs, err := decodeIndexes(m.Indexes)
	if err != nil {
		return nil, err
	}
	c.Indexes = idxs
	return c, nil
}
