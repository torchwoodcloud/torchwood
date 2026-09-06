// schema 演进状态机（转出 POC 门禁 B4，redesign §4.6 / 预决策 3）：
//
//   - 删列两段：DeleteAttribute = 段一 deprecated（读投影屏蔽 + 查询白名单
//     拒绝 + 写入拒收，可回滚 RestoreAttribute）；RetireAttribute = 段二物理
//     删列（CASCADE，不可逆）。
//   - 改类型/收紧 = copy 迁移任务：新列（物理名 <key>__v<seq>，seq 取
//     schema_version+1）→ 异步批量回填（批 500 行、批间限速、游标可恢复，
//     MigrateAttribute 同 key 重入即续跑）→ ACCESS EXCLUSIVE 锁窗内全量
//     追平重算 + 行数校验 → 原子 swap（旧列 RENAME 为 <key>__d<seq> 留作
//     deprecated 残留、新列接管逻辑名）→ schema_version++（swap commit
//     递增——schema_version 的消费点）。迁移期间该属性写入拒收
//     （CATALOG.ATTRIBUTE_MIGRATING），读取服务旧列。
//   - 放宽（扩宽/required→optional）= 即时 ALTER（无 copy），schema_version
//     同样递增。
//
// attrs 保持"每逻辑 key 恰一条目"不变量：migrating/deprecated 是条目状态，
// swap 后旧列物理名只记迁移账本（catalog_migrations.old_physical）。
package documentdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/pkg/ident"
	"github.com/torchwooddev/torchwood/pkg/idgen"
)

// 迁移任务阶段（catalog_migrations.phase）。
const (
	migrationPhaseBackfilling = "backfilling"
	migrationPhaseSwapped     = "swapped"
	migrationPhaseRetired     = "retired"
	migrationPhaseFailed      = "failed"
)

const (
	// migrationBatchSize 是回填批行数（预决策 3：批 500 行游标）。
	migrationBatchSize = 500
	// migrationBatchPause 是批间限速停顿（回填让出 I/O；POC 固定值，
	// 配额化挂账运维调参）。
	migrationBatchPause = 5 * time.Millisecond
)

// 迁移期/废弃期属性的生命周期错误（infra 产 InvalidArgument 族——键级契约
// 违反与未知字段同族；域码经 errors.Is 判别映射）。
var (
	// ErrAttributeDeprecated 是写入/索引已 deprecated 属性的拒绝错误。
	ErrAttributeDeprecated = errors.New("attribute is deprecated")
	// ErrAttributeMigrating 是写入迁移中属性的拒绝错误（§4.6：迁移期间写按
	// 目标 schema 校验——本实现取统一写封锁形态，swap 后恢复）。
	ErrAttributeMigrating = errors.New("attribute is being migrated")
)

var _ databases.SchemaEvolution = (*postgresDocumentDB)(nil)

// lifecycleViolation 校验写载荷的键集（data/increment/array_updates）不含
// deprecated/migrating 属性。coll 为 nil 时加载（调用方已持有时传入复用，
// 免一次 catalog 点查）。Bypass 主体（内部信任路径）由调用方短路。
func (p *postgresDocumentDB) lifecycleViolation(ctx context.Context, projectID, databaseID, collectionID string, coll *databases.Collection, keysets ...map[string]struct{}) error {
	need := false
	for _, ks := range keysets {
		if len(ks) > 0 {
			need = true
		}
	}
	if !need {
		return nil
	}
	if coll == nil {
		var err error
		coll, err = p.GetCollection(ctx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		if coll == nil {
			return status.Error(codes.NotFound, "collection not found")
		}
	}
	state := map[string]string{} // key → deprecated|migrating
	for _, a := range coll.Attributes {
		switch a.StatusOrDefault() {
		case databases.AttrStatusDeprecated:
			state[a.Key] = databases.AttrStatusDeprecated
		case databases.AttrStatusMigrating:
			state[a.Key] = databases.AttrStatusMigrating
		}
	}
	if len(state) == 0 {
		return nil
	}
	for _, ks := range keysets {
		for k := range ks {
			switch state[k] {
			case databases.AttrStatusDeprecated:
				return fmt.Errorf("%w: %q", ErrAttributeDeprecated, k)
			case databases.AttrStatusMigrating:
				return fmt.Errorf("%w: %q", ErrAttributeMigrating, k)
			}
		}
	}
	return nil
}

// dataKeys 提取文档 Data 的键集（nil 安全）。
func dataKeys(data map[string]any) map[string]struct{} {
	if len(data) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(data))
	for k := range data {
		out[k] = struct{}{}
	}
	return out
}

// keysSet 把字符串切片转集合（nil 安全）。
func keysSet(keys []string) map[string]struct{} {
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		out[k] = struct{}{}
	}
	return out
}

// deprecatedKeysOf 返回集合中 deprecated 属性键集（nil = 无）。
func deprecatedKeysOf(attrs []databases.Attribute) map[string]struct{} {
	var out map[string]struct{}
	for _, a := range attrs {
		if a.StatusOrDefault() == databases.AttrStatusDeprecated {
			if out == nil {
				out = map[string]struct{}{}
			}
			out[a.Key] = struct{}{}
		}
	}
	return out
}

// maskDeprecatedData 是读投影屏蔽的 Go 侧执行（B4：deprecated 属性读回剥离
// ——读路径的 to_jsonb 载荷按物理列输出，deprecated 列仍在表内，扫描后在
// Data 上剥离，语义等价 SQL 投影 - 键）。inPlace 剥离；attrs 缺 deprecated
// 时零开销跳过。
func maskDeprecatedData(attrs []databases.Attribute, docs []databases.Document) {
	deprecated := deprecatedKeysOf(attrs)
	if len(deprecated) == 0 {
		return
	}
	for i := range docs {
		for k := range docs[i].Data {
			if _, bad := deprecated[k]; bad {
				delete(docs[i].Data, k)
			}
		}
	}
}

// maskDeprecatedDocument 是单文档形态的读屏蔽（GetDocument；doc 为 nil 安全）。
func maskDeprecatedDocument(attrs []databases.Attribute, doc *databases.Document) {
	if doc == nil {
		return
	}
	maskDeprecatedData(attrs, []databases.Document{*doc})
}

// ---------------------------------------------------------------------------
// MigrateAttribute
// ---------------------------------------------------------------------------

func (p *postgresDocumentDB) MigrateAttribute(ctx context.Context, projectID, databaseID, collectionID, key string, target databases.Attribute) (*databases.AttributeMigration, error) {
	if !safeNameRe.MatchString(key) {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("invalid attribute key: %s", key))
	}
	if target.Key != "" && target.Key != key {
		return nil, status.Error(codes.InvalidArgument, "migrate target key must match the migrated attribute")
	}
	target.Key = key
	target.Type = strings.ToLower(target.Type)
	if err := validateArrayAttribute(target); err != nil {
		return nil, err
	}
	if err := validateVectorAttribute(target); err != nil {
		return nil, err
	}

	_, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, p.mapError(err)
	}

	var (
		taskID      string
		resumed     bool
		instant     bool
		targetType  string
		newPhysical string
	)
	txErr := p.withOwnerTx(ctx, func(txCtx context.Context) error {
		row, err := p.loadCollectionRow(txCtx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		attrs, err := decodeAttributes(row.Attrs)
		if err != nil {
			return err
		}
		var current *databases.Attribute
		for i := range attrs {
			if attrs[i].Key == key {
				current = &attrs[i]
				break
			}
		}
		if current == nil {
			return status.Error(codes.NotFound, fmt.Sprintf("attribute %q not found", key))
		}
		switch current.StatusOrDefault() {
		case databases.AttrStatusDeprecated:
			return status.Error(codes.InvalidArgument,
				fmt.Sprintf("attribute %q is deprecated; restore it before migrating", key))
		case databases.AttrStatusRetired:
			return status.Error(codes.NotFound, fmt.Sprintf("attribute %q is retired", key))
		case databases.AttrStatusMigrating:
			// 重入：failed/backfilling 任务续跑（崩溃/失败恢复路径，判据
			// "可恢复"）。failed → 重置回 backfilling 再续跑。
			task, err := p.latestBackfillingTask(txCtx, projectID, databaseID, collectionID, key)
			if err != nil {
				return err
			}
			if task == nil {
				return status.Error(codes.Internal, "migrating attribute without backfilling task")
			}
			if _, err := p.conn(txCtx).ExecContext(txCtx,
				`UPDATE catalog_migrations SET phase = ?, error = '', updated_at = NOW() WHERE id = ?`,
				migrationPhaseBackfilling, task.ID); err != nil {
				return err
			}
			taskID = task.ID
			newPhysical = task.NewPhysical
			resumed = true
			return nil
		}

		// 新建任务。放宽/收紧/改类型的分诊：
		oldPg := attrPGType(*current)
		newPg := attrPGType(target)
		instantPath := false
		if oldPg == newPg {
			if current.Required && !target.Required {
				instantPath = true // required→optional：DROP NOT NULL（§4.6 放宽行）
			}
		} else if isVarcharWiden(oldPg, newPg) {
			instantPath = true // 扩宽：PG 元数据级 ALTER TYPE（§4.6 放宽行）
		}
		if instantPath {
			instant = true
			return p.applyInstantMigration(txCtx, row, attrs, *current, key, target, oldPg, newPg)
		}
		// copy 迁移（§4.6 收紧/改类型行）。required 收紧必须带 default
		//（§4.6 加列 required 行：无 default 的 NOT NULL 回填不可满足）。
		if target.Required && target.Default == nil && !current.Required {
			return status.Error(codes.InvalidArgument,
				fmt.Sprintf("migrating %q to required requires a default_value for backfill", key))
		}
		seq := row.SchemaVersion + 1
		newPhysical = fmt.Sprintf("%s__v%d", key, seq)
		if err := validatePhysicalNameLen("migrated column", newPhysical); err != nil {
			return err
		}
		// 新列：nullable、无 default（回填后 SET NOT NULL/DEFAULT）。
		newColSQL, err := attributeColumnSQL(databases.Attribute{
			Key: newPhysical, Type: target.Type, Size: target.Size,
			Array: target.Array, Dims: target.Dims,
		})
		if err != nil {
			return err
		}
		if _, err := p.conn(txCtx).ExecContext(txCtx,
			fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s`, tableName(schema, physical), newColSQL),
		); err != nil {
			return err
		}
		// attrs 条目 → migrating（读服务旧列；写拒收）；CAS。
		current.Status = databases.AttrStatusMigrating
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
		// 任务行（from/to 与 attrs JSONB 同构编解码）。
		fromJSON, err := encodeAttributes([]databases.Attribute{{Key: key, Type: current.Type, Size: current.Size, Required: current.Required, Array: current.Array, Default: current.Default, Dims: current.Dims, Status: databases.AttrStatusActive}})
		if err != nil {
			return err
		}
		toJSON, err := encodeAttributes([]databases.Attribute{target})
		if err != nil {
			return err
		}
		taskID = idgen.UUID().String()
		now := time.Now()
		m := &model.DocumentMigration{
			ID: taskID, ProjectID: projectID, DatabaseID: databaseID, CollectionID: collectionID,
			AttrKey: key, FromAttr: fromJSON, ToAttr: toJSON,
			OldPhysical: key, NewPhysical: newPhysical,
			Phase: migrationPhaseBackfilling, CreatedAt: now, UpdatedAt: now,
		}
		if _, err := p.conn(txCtx).NewInsert().Model(m).Exec(txCtx); err != nil {
			return err
		}
		targetType = newPg
		return nil
	})
	if txErr != nil {
		return nil, p.mapError(txErr)
	}

	// 即时放宽（无 copy 任务）：schema_version 已在事务内递增，直接读回。
	if instant {
		return p.readMigrationState(ctx, projectID, databaseID, collectionID, key)
	}

	if resumed {
		// 重入：从账本行重建快照（游标/目标定义都在任务行内）。
		t := p.resumeTask(ctx, taskID, schema, physical)
		if t.id != "" {
			p.kickBackfill(t)
		}
	} else {
		p.kickBackfill(backfillTask{
			id: taskID, projectID: projectID, databaseID: databaseID,
			collectionID: collectionID, schema: schema, physical: physical,
			attrKey: key, oldPhysical: key, newPhysical: newPhysical,
			targetType: targetType, toAttr: target,
		})
	}
	return p.readMigration(ctx, taskID)
}

// attrPGType 是属性的物理列类型单源（attributeColumnSQL 的类型分支同源）。
func attrPGType(a databases.Attribute) string {
	dataType := pgTypeFor(a.Type, a.Size)
	if a.Array {
		dataType = pgArrayTypeFor(a.Type)
	}
	if strings.ToLower(a.Type) == "vector" {
		dataType = fmt.Sprintf("VECTOR(%d)", a.Dims)
	}
	return dataType
}

// applyInstantMigration 是放宽路径（扩宽/required→optional）：即时 ALTER +
// attrs 回写 + schema_version++（同事务）。扩宽 = ALTER COLUMN TYPE
// （varchar 扩宽元数据级）；required→optional = DROP NOT NULL。
func (p *postgresDocumentDB) applyInstantMigration(ctx context.Context, row *model.DocumentCollection, attrs []databases.Attribute, current databases.Attribute, key string, target databases.Attribute, oldPg, newPg string) error {
	schema, err := ident.SchemaName(row.ProjectID, row.DatabaseID)
	if err != nil {
		return err
	}
	tbl := tableName(schema, row.PhysicalName)
	if oldPg != newPg {
		if _, err := p.conn(ctx).ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE %s ALTER COLUMN %s TYPE %s USING %s::%s`,
			tbl, quoteIdent(key), newPg, quoteIdent(key), newPg)); err != nil {
			return fmt.Errorf("widen column type: %w", err)
		}
	}
	if current.Required && !target.Required {
		if _, err := p.conn(ctx).ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL`, tbl, quoteIdent(key)),
		); err != nil {
			return fmt.Errorf("drop not null: %w", err)
		}
	}
	for i := range attrs {
		if attrs[i].Key == key {
			attrs[i] = target
			attrs[i].Status = databases.AttrStatusActive
		}
	}
	attrsJSON, err := encodeAttributes(attrs)
	if err != nil {
		return err
	}
	res, err := p.conn(ctx).ExecContext(ctx,
		`UPDATE catalog_collections SET attrs = ?, schema_version = schema_version + 1, updated_at = ?, ddl_seq = ddl_seq + 1 WHERE project_id = ? AND database_id = ? AND collection_id = ? AND ddl_seq = ?`,
		attrsJSON, time.Now(), row.ProjectID, row.DatabaseID, row.CollectionID, row.DDLSeq)
	if err != nil {
		return err
	}
	return requireCASApplied(res)
}

// isVarcharWiden 报告 varchar(n) → varchar(m>n)/TEXT 的扩宽关系（PG 元数据级
// ALTER TYPE，无重写）。
func isVarcharWiden(oldType, newType string) bool {
	if oldType == newType || oldType == "TEXT" {
		return false
	}
	var oldN int
	if _, err := fmt.Sscanf(oldType, "VARCHAR(%d)", &oldN); err != nil {
		return false
	}
	if newType == "TEXT" {
		return true
	}
	var newN int
	if _, err := fmt.Sscanf(newType, "VARCHAR(%d)", &newN); err != nil {
		return false
	}
	return newN > oldN
}

// ---------------------------------------------------------------------------
// 回填与 swap
// ---------------------------------------------------------------------------

// backfillTask 是回填 goroutine 的任务快照（创建事务提交后构建）。
type backfillTask struct {
	id                                  string
	projectID, databaseID, collectionID string
	schema, physical                    string
	attrKey                             string
	oldPhysical, newPhysical            string
	targetType                          string // 新列 PG 类型（cast 目标）
	toAttr                              databases.Attribute
}

// kickBackfill 拉起后台回填 goroutine（与请求 ctx 解耦；失败落 task
// phase=failed，attrs 维持 migrating——重入 MigrateAttribute 续跑或
// RestoreAttribute 中止）。
func (p *postgresDocumentDB) kickBackfill(task backfillTask) {
	go p.runBackfill(task)
}

func (p *postgresDocumentDB) runBackfill(task backfillTask) {
	ctx := context.Background()
	for {
		n, err := p.backfillBatch(ctx, task)
		if err != nil {
			p.failMigration(ctx, task, err)
			return
		}
		if n == 0 {
			break
		}
		time.Sleep(migrationBatchPause)
	}
	if err := p.swapMigration(ctx, task); err != nil {
		p.failMigration(ctx, task, err)
		return
	}
	slog.Info("attribute migration swapped",
		"project_id", task.projectID, "collection", task.collectionID,
		"attribute", task.attrKey, "new_physical", task.newPhysical)
}

// backfillBatch 执行一批回填（短事务）：游标（任务行 cursor_id）之后的前
// migrationBatchSize 行 old→new 拷贝，推进游标与计数。返回本批行数（0 = 回
// 填完成）。游标存任务行——goroutine 崩溃后重入天然从账本续跑。
//
// 执行身份 = tw_system（BYPASSRLS）：业务行受 FORCE RLS 管辖，tw_owner 无签名
// 角色不可见任何行（A6"owner 查询走 tw_system"同源）；任务账本已 GRANT 给
// tw_system（000003）。
func (p *postgresDocumentDB) backfillBatch(ctx context.Context, task backfillTask) (int, error) {
	n := 0
	txErr := p.withDocumentTx(ctx, clients.ExecIdentity{Role: clients.RoleSystem}, func(txCtx context.Context) error {
		var cursor string
		if err := p.conn(txCtx).QueryRowContext(txCtx,
			`SELECT COALESCE(cursor_id, '') FROM catalog_migrations WHERE id = ?`, task.id,
		).Scan(&cursor); err != nil {
			return err
		}
		tbl := tableName(task.schema, task.physical)
		var ids []string
		rows, err := p.conn(txCtx).QueryContext(txCtx, fmt.Sprintf(
			`SELECT _id FROM %s WHERE _tenant = (SELECT internal_id FROM public.projects WHERE id = ?)
			 AND %s IS NULL AND %s IS NOT NULL AND _id > ? ORDER BY _id LIMIT %d`,
			tbl, quoteIdent(task.newPhysical), quoteIdent(task.oldPhysical), migrationBatchSize),
			task.projectID, cursor)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		if len(ids) == 0 {
			return nil
		}
		n = len(ids)
		// cast 失败（如 string 列含非数值文本）→ 本批报错 → 任务 failed
		//（§4.6 validate：不兼容数据显式失败，不静默截断）。
		res, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
			`UPDATE %s SET %s = %s WHERE _tenant = (SELECT internal_id FROM public.projects WHERE id = ?) AND _id = ANY(?::text[])`,
			tbl, quoteIdent(task.newPhysical),
			fmt.Sprintf("%s::%s", quoteIdent(task.oldPhysical), task.targetType)),
			task.projectID, pgTextArray(ids))
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		// 推进任务账本（游标 = 本批末 _id；行数累计）。
		_, err = p.conn(txCtx).ExecContext(txCtx,
			`UPDATE catalog_migrations SET cursor_id = ?, rows_done = rows_done + ?, updated_at = NOW() WHERE id = ?`,
			ids[len(ids)-1], affected, task.id)
		return err
	})
	if txErr != nil {
		return 0, txErr
	}
	return n, nil
}

// swapMigration 是迁移 commit（短事务，锁窗）：
// ACCESS EXCLUSIVE 锁 → 全量追平重算（回填与并发写竞态的兜底，正确性锚点）→
// 行数校验 → 旧列 RENAME 残留 / 新列接管逻辑名 → required/DEFAULT 约束 →
// attrs 条目转 active（目标定义）→ schema_version++（CAS）→ 任务 swapped。
//
// 双身份编排（A6）：数据语句（追平/校验）以 tw_system 中段切换执行（BYPASSRLS
// ——tw_owner 受 FORCE RLS 不可见业务行），DDL 与 catalog 回写以 tw_owner 执行
// （表所有权）；退出前恢复 owner 身份，边界一致。
func (p *postgresDocumentDB) swapMigration(ctx context.Context, task backfillTask) error {
	return p.withOwnerTx(ctx, func(txCtx context.Context) error {
		row, err := p.loadCollectionRow(txCtx, task.projectID, task.databaseID, task.collectionID)
		if err != nil {
			return err
		}
		attrs, err := decodeAttributes(row.Attrs)
		if err != nil {
			return err
		}
		var current *databases.Attribute
		for i := range attrs {
			if attrs[i].Key == task.attrKey {
				current = &attrs[i]
				break
			}
		}
		if current == nil || current.StatusOrDefault() != databases.AttrStatusMigrating {
			return status.Error(codes.FailedPrecondition, "migration state changed concurrently")
		}
		tbl := tableName(task.schema, task.physical)
		// 锁窗：阻塞并发读写直到 commit（迁移 commit 的原子性来源；回填期
		// 不持锁——只在最后追平 + 换名窗口）。
		if _, err := p.conn(txCtx).ExecContext(txCtx,
			fmt.Sprintf(`LOCK TABLE %s IN ACCESS EXCLUSIVE MODE`, tbl)); err != nil {
			return err
		}
		// 中段切换 tw_system：数据语句可见全部行（BYPASSRLS）。
		idb := p.conn(txCtx)
		if err := clients.InjectExecIdentity(txCtx, idb, clients.ExecIdentity{Role: clients.RoleSystem}); err != nil {
			return err
		}
		castExpr := fmt.Sprintf("%s::%s", quoteIdent(task.oldPhysical), task.targetType)
		tenantExpr := `(SELECT internal_id FROM public.projects WHERE id = ?)`
		// 追平重算：回填后任何变更过的行（新旧值不等）在此重拷——增量回填 +
		// 竞态行的统一兜底（cast 失败在此显式失败）。
		if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
			`UPDATE %s SET %s = %s WHERE _tenant = %s AND %s IS DISTINCT FROM (%s)`,
			tbl, quoteIdent(task.newPhysical), castExpr, tenantExpr,
			quoteIdent(task.newPhysical), castExpr,
		), task.projectID); err != nil {
			return fmt.Errorf("migration catch-up: %w", err)
		}
		// 行数校验（§4.6 validate）。
		var oldCount, newCount int64
		if err := p.conn(txCtx).QueryRowContext(txCtx, fmt.Sprintf(
			`SELECT COUNT(*) FILTER (WHERE %s IS NOT NULL), COUNT(*) FILTER (WHERE %s IS NOT NULL) FROM %s WHERE _tenant = %s`,
			quoteIdent(task.oldPhysical), quoteIdent(task.newPhysical), tbl, tenantExpr,
		), task.projectID).Scan(&oldCount, &newCount); err != nil {
			return err
		}
		if oldCount != newCount {
			return status.Errorf(codes.Internal,
				"migration validate failed: old not-null %d != new not-null %d", oldCount, newCount)
		}
		// 切回 tw_owner：RENAME/约束 DDL 与 catalog 回写需要表所有权。
		if err := clients.InjectExecIdentity(txCtx, idb, clients.ExecIdentity{Role: clients.RoleOwner}); err != nil {
			return err
		}
		// 换名：旧列 <key> → <key>__d<seq>（deprecated 残留）；新列接管逻辑名。
		seq := row.SchemaVersion + 1
		deprecatedPhysical := fmt.Sprintf("%s__d%d", task.attrKey, seq)
		if err := validatePhysicalNameLen("deprecated column", deprecatedPhysical); err != nil {
			return err
		}
		if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
			`ALTER TABLE %s RENAME COLUMN %s TO %s`,
			tbl, quoteIdent(task.attrKey), quoteIdent(deprecatedPhysical)),
		); err != nil {
			return err
		}
		if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
			`ALTER TABLE %s RENAME COLUMN %s TO %s`,
			tbl, quoteIdent(task.newPhysical), quoteIdent(task.attrKey)),
		); err != nil {
			return err
		}
		// 目标约束（required/default——回填后可安全收紧）。
		if task.toAttr.Default != nil {
			def, err := formatDefault(task.toAttr.Default, task.toAttr.Type)
			if err != nil {
				return err
			}
			if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
				`ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s`,
				tbl, quoteIdent(task.attrKey), def)); err != nil {
				return err
			}
		}
		if task.toAttr.Required {
			if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
				`ALTER TABLE %s ALTER COLUMN %s SET NOT NULL`,
				tbl, quoteIdent(task.attrKey))); err != nil {
				return fmt.Errorf("set not null: %w", err)
			}
		}
		// tw_app 列级授权重刷（新列接管逻辑名后写路径授权口径不变）。
		if err := p.refreshColumnGrants(txCtx, task.schema, task.physical); err != nil {
			return err
		}
		// attrs：条目转目标定义 active（每 key 一条目不变量）。
		for i := range attrs {
			if attrs[i].Key == task.attrKey {
				attrs[i] = task.toAttr
				attrs[i].Status = databases.AttrStatusActive
			}
		}
		attrsJSON, err := encodeAttributes(attrs)
		if err != nil {
			return err
		}
		// schema_version++：迁移 commit 的消费点（预决策 3）。
		res, err := p.conn(txCtx).ExecContext(txCtx,
			`UPDATE catalog_collections SET attrs = ?, schema_version = schema_version + 1, updated_at = ?, ddl_seq = ddl_seq + 1 WHERE project_id = ? AND database_id = ? AND collection_id = ? AND ddl_seq = ?`,
			attrsJSON, time.Now(), task.projectID, task.databaseID, task.collectionID, row.DDLSeq)
		if err != nil {
			return err
		}
		if err := requireCASApplied(res); err != nil {
			return err
		}
		// 任务账本：swapped + 旧列新物理名（retire 的 DROP 目标）。
		_, err = p.conn(txCtx).ExecContext(txCtx,
			`UPDATE catalog_migrations SET phase = ?, old_physical = ?, rows_done = ?, updated_at = NOW() WHERE id = ?`,
			migrationPhaseSwapped, deprecatedPhysical, oldCount, task.id)
		return err
	})
}

// failMigration 落任务失败（attrs 维持 migrating——写拒收持续到重入续跑或
// RestoreAttribute 中止；显式不静默回滚，失败态可观测）。
func (p *postgresDocumentDB) failMigration(ctx context.Context, task backfillTask, cause error) {
	slog.Error("attribute migration failed (writes to the attribute are rejected until retried or restored)",
		"project_id", task.projectID, "collection", task.collectionID,
		"attribute", task.attrKey, "task", task.id, "error", cause)
	_, err := p.db.Conn(ctx).ExecContext(ctx,
		`UPDATE catalog_migrations SET phase = ?, error = ?, updated_at = NOW() WHERE id = ? AND phase = ?`,
		migrationPhaseFailed, cause.Error(), task.id, migrationPhaseBackfilling)
	if err != nil {
		slog.Error("record migration failure", "task", task.id, "error", err)
	}
}

// latestBackfillingTask 取同 key 的进行中/失败任务（重入恢复——failed 任务
// 重入时由调用方重置回 backfilling 续跑）。
func (p *postgresDocumentDB) latestBackfillingTask(ctx context.Context, projectID, databaseID, collectionID, key string) (*model.DocumentMigration, error) {
	var ms []model.DocumentMigration
	err := p.conn(ctx).NewSelect().Model(&ms).
		Where("project_id = ? AND database_id = ? AND collection_id = ? AND attr_key = ? AND phase IN (?, ?)",
			projectID, databaseID, collectionID, key, migrationPhaseBackfilling, migrationPhaseFailed).
		Order("created_at DESC").Limit(1).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if len(ms) == 0 {
		return nil, nil
	}
	return &ms[0], nil
}

// resumeTask 从任务行重建回填快照（重入路径：游标在账本内，批量天然从
// `new IS NULL AND _id > cursor` 续跑）。行缺失/解码失败返回零值任务
// （调用方以 id 判空跳过）。
func (p *postgresDocumentDB) resumeTask(ctx context.Context, taskID, schema, physical string) backfillTask {
	m := new(model.DocumentMigration)
	if err := p.conn(ctx).NewSelect().Model(m).Where("id = ?", taskID).Scan(ctx); err != nil {
		slog.Error("resume migration task: load failed", "task", taskID, "error", err)
		return backfillTask{}
	}
	toAttrs, err := decodeAttributes(m.ToAttr)
	if err != nil || len(toAttrs) == 0 {
		slog.Error("resume migration task: decode target failed", "task", taskID, "error", err)
		return backfillTask{}
	}
	return backfillTask{
		id: m.ID, projectID: m.ProjectID, databaseID: m.DatabaseID, collectionID: m.CollectionID,
		schema: schema, physical: physical, attrKey: m.AttrKey,
		oldPhysical: m.OldPhysical, newPhysical: m.NewPhysical,
		targetType: attrPGType(toAttrs[0]), toAttr: toAttrs[0],
	}
}

// readMigrationState 读回集合当前迁移相关状态（即时放宽路径的响应：无任务行，
// phase=swapped 表达"已完成"）。
func (p *postgresDocumentDB) readMigrationState(ctx context.Context, projectID, databaseID, collectionID, key string) (*databases.AttributeMigration, error) {
	row, err := p.loadCollectionRow(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, p.mapError(err)
	}
	return &databases.AttributeMigration{
		AttrKey:       key,
		Phase:         migrationPhaseSwapped,
		SchemaVersion: row.SchemaVersion,
	}, nil
}

// readMigration 读回任务形态（响应）。
func (p *postgresDocumentDB) readMigration(ctx context.Context, taskID string) (*databases.AttributeMigration, error) {
	m := new(model.DocumentMigration)
	if err := p.conn(ctx).NewSelect().Model(m).Where("id = ?", taskID).Scan(ctx); err != nil {
		return nil, p.mapError(err)
	}
	var version int64
	if err := p.conn(ctx).NewSelect().Model((*model.DocumentCollection)(nil)).
		Column("schema_version").
		Where("project_id = ? AND database_id = ? AND collection_id = ?", m.ProjectID, m.DatabaseID, m.CollectionID).
		Scan(ctx, &version); err != nil {
		return nil, p.mapError(err)
	}
	return &databases.AttributeMigration{
		ID: m.ID, AttrKey: m.AttrKey, Phase: m.Phase,
		OldPhysical: m.OldPhysical, NewPhysical: m.NewPhysical,
		RowsDone: m.RowsDone, SchemaVersion: version,
	}, nil
}

// ---------------------------------------------------------------------------
// Restore / Retire（删列两段 + 回滚）
// ---------------------------------------------------------------------------

func (p *postgresDocumentDB) RestoreAttribute(ctx context.Context, projectID, databaseID, collectionID, key string) error {
	_, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return p.mapError(err)
	}
	return p.mapError(p.withOwnerTx(ctx, func(txCtx context.Context) error {
		row, err := p.loadCollectionRow(txCtx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		attrs, err := decodeAttributes(row.Attrs)
		if err != nil {
			return err
		}
		var current *databases.Attribute
		for i := range attrs {
			if attrs[i].Key == key {
				current = &attrs[i]
				break
			}
		}
		if current == nil {
			return status.Error(codes.NotFound, fmt.Sprintf("attribute %q not found", key))
		}
		switch current.StatusOrDefault() {
		case databases.AttrStatusDeprecated:
			current.Status = databases.AttrStatusActive
		case databases.AttrStatusMigrating:
			// 中止迁移：新列删除、任务置 failed、条目恢复 active。（正在
			// 跑的回填 goroutine 随新列删除自然失败落账，无锁交互。）
			task, err := p.latestBackfillingTask(txCtx, projectID, databaseID, collectionID, key)
			if err != nil {
				return err
			}
			if task != nil {
				if _, err := p.conn(txCtx).ExecContext(txCtx,
					`UPDATE catalog_migrations SET phase = ?, error = ?, updated_at = NOW() WHERE id = ?`,
					migrationPhaseFailed, "aborted via RestoreAttribute", task.ID); err != nil {
					return err
				}
				if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
					`ALTER TABLE %s DROP COLUMN IF EXISTS %s`,
					tableName(schema, physical), quoteIdent(task.NewPhysical))); err != nil {
					return err
				}
			}
			current.Status = databases.AttrStatusActive
		default:
			return status.Error(codes.InvalidArgument, fmt.Sprintf("attribute %q is not deprecated or migrating", key))
		}
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
		return requireCASApplied(res)
	}))
}

func (p *postgresDocumentDB) RetireAttribute(ctx context.Context, projectID, databaseID, collectionID, key string) error {
	_, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return p.mapError(err)
	}
	return p.mapError(p.withOwnerTx(ctx, func(txCtx context.Context) error {
		row, err := p.loadCollectionRow(txCtx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		attrs, err := decodeAttributes(row.Attrs)
		if err != nil {
			return err
		}
		idx := -1
		for i := range attrs {
			if attrs[i].Key == key {
				idx = i
				break
			}
		}
		tbl := tableName(schema, physical)
		if idx >= 0 && attrs[idx].StatusOrDefault() == databases.AttrStatusDeprecated {
			// 删列段二：物理删列（CASCADE 清依赖物理索引）+ attrs 条目移除
			//（retired 即不在契约中，可重新创建同 key 属性）+ 同事务清理引用
			// 该属性的 catalog 索引条目（B8 语义随段二迁移至此）。
			if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
				`ALTER TABLE %s DROP COLUMN IF EXISTS %s CASCADE`, tbl, quoteIdent(key))); err != nil {
				return err
			}
			idxs, err := decodeIndexes(row.Indexes)
			if err != nil {
				return err
			}
			keptIdxs := make([]databases.Index, 0, len(idxs))
			for _, idx := range idxs {
				if !slices.Contains(idx.Attributes, key) {
					keptIdxs = append(keptIdxs, idx)
				}
			}
			idxsJSON, err := encodeIndexes(keptIdxs)
			if err != nil {
				return err
			}
			attrs = append(attrs[:idx], attrs[idx+1:]...)
			attrsJSON, err := encodeAttributes(attrs)
			if err != nil {
				return err
			}
			res, err := p.conn(txCtx).ExecContext(txCtx,
				`UPDATE catalog_collections SET attrs = ?, indexes = ?, updated_at = ?, ddl_seq = ddl_seq + 1 WHERE project_id = ? AND database_id = ? AND collection_id = ? AND ddl_seq = ?`,
				attrsJSON, idxsJSON, time.Now(), projectID, databaseID, collectionID, row.DDLSeq)
			if err != nil {
				return err
			}
			return requireCASApplied(res)
		}
		// swap 残留退役：latest swapped 任务（该 key）的旧列 DROP。
		var ms []model.DocumentMigration
		err = p.conn(txCtx).NewSelect().Model(&ms).
			Where("project_id = ? AND database_id = ? AND collection_id = ? AND attr_key = ? AND phase = ?",
				projectID, databaseID, collectionID, key, migrationPhaseSwapped).
			Order("created_at DESC").Limit(1).Scan(txCtx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return p.mapError(err)
		}
		if err != nil || len(ms) == 0 {
			return status.Error(codes.InvalidArgument,
				fmt.Sprintf("attribute %q has no deprecated column to retire", key))
		}
		if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(
			`ALTER TABLE %s DROP COLUMN IF EXISTS %s CASCADE`, tbl, quoteIdent(ms[0].OldPhysical))); err != nil {
			return err
		}
		_, err = p.conn(txCtx).ExecContext(txCtx,
			`UPDATE catalog_migrations SET phase = ?, updated_at = NOW() WHERE id = ?`,
			migrationPhaseRetired, ms[0].ID)
		return err
	}))
}
