package documentdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
)

func (p *postgresDocumentDB) ensureCollectionAccessible(coll *databases.Collection, principal databases.Principal) error {
	if coll == nil {
		return ErrPermissionDenied
	}
	if coll.Disabled && !principal.BypassesDocumentACL() {
		return ErrPermissionDenied
	}
	return nil
}

func (p *postgresDocumentDB) getCollectionForAccess(ctx context.Context, projectID, databaseID, collectionID string) (*databases.Collection, error) {
	coll, err := p.GetCollection(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, err
	}
	return coll, nil
}

// getDocumentACL 点查文档 _acl（事件写前快照与文档级判定共用数据源；阶段③
// 包 A 换轴：取代 _perms 表点查 getDocumentPermissions）。行缺失 → (nil,false,nil)
// （与旧空 _perms 语义一致：无 ACE → docHasPerms=false → 集合级回退分支）。
func (p *postgresDocumentDB) getDocumentACL(ctx context.Context, tbl, docID string, tenant int64) ([]databases.Permission, bool, error) {
	var raw []byte
	err := p.conn(ctx).QueryRowContext(ctx,
		fmt.Sprintf(`SELECT array_to_json(_acl) FROM %s WHERE _id = ? AND _tenant = ?`, tbl),
		docID, tenant,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	perms, err := parseACLJSON(raw)
	if err != nil {
		return nil, false, err
	}
	return perms, len(perms) > 0, nil
}

// getDocumentACLBatch 单条 IN 查询批量取回多文档 _acl；返回 docID -> perms 映射，
// 未命中文档不在 map 中（docHasPerms=false，与单条路径空切片语义一致）。
func (p *postgresDocumentDB) getDocumentACLBatch(ctx context.Context, tbl string, docIDs []string, tenant int64) (map[string][]databases.Permission, error) {
	byDoc := make(map[string][]databases.Permission, len(docIDs))
	err := eachIDChunk(docIDs, bulkInChunk, func(chunk []string) error {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, tenant)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := p.conn(ctx).QueryContext(ctx,
			fmt.Sprintf(`SELECT _id, array_to_json(_acl) FROM %s WHERE _tenant = ? AND _id IN (%s)`, tbl, inPlaceholders(len(chunk))),
			args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var docID string
			var raw []byte
			if err := rows.Scan(&docID, &raw); err != nil {
				return err
			}
			perms, err := parseACLJSON(raw)
			if err != nil {
				return err
			}
			byDoc[docID] = perms
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return byDoc, nil
}

// checkDocumentACL 对已获取的文档 ACE 做判定（coll 为调用方已获取的集合）。
// 判定仍是 AllowsDocumentAccess 单源（含 D1 系统集合豁免与 B1 空回退）；
// RLS 判定执行点在阶段③包 C 落地后，本路径收缩至 sentinel 系统集合。
func (p *postgresDocumentDB) checkDocumentACL(coll *databases.Collection, docPerms []databases.Permission, docHasPerms bool, permType string, principal databases.Principal) error {
	if principal.BypassesDocumentACL() {
		return nil
	}
	if err := p.ensureCollectionAccessible(coll, principal); err != nil {
		return err
	}
	if !databases.AllowsDocumentAccess(coll, docPerms, docHasPerms, permType, principal.Roles) {
		return ErrPermissionDenied
	}
	return nil
}

// checkDocumentPermission 校验文档级权限（点查 _acl + AllowsDocumentAccess）；
// coll 为调用方已获取的集合（避免重复查询 N+1），nil 时内部获取。
// 阶段③包 A：数据源从 _perms 表换为 _acl 列，判定模型不动。
func (p *postgresDocumentDB) checkDocumentPermission(
	ctx context.Context,
	projectID, databaseID, collectionID, tbl, docID string,
	tenant int64,
	permType string,
	principal databases.Principal,
	coll *databases.Collection,
) error {
	if principal.BypassesDocumentACL() {
		return nil
	}
	if coll == nil {
		var err error
		coll, err = p.getCollectionForAccess(ctx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
	}
	docPerms, docHasPerms, err := p.getDocumentACL(ctx, tbl, docID, tenant)
	if err != nil {
		return err
	}
	return p.checkDocumentACL(coll, docPerms, docHasPerms, permType, principal)
}

// listPermissionFilter 构造列表/count/聚合的文档级读过滤谓词（阶段③包 A：
// _perms EXISTS 子查询 → _acl 数组谓词，GIN（idx_<phys>_acl）可索引）。
// 语义与 AllowsDocumentAccess 的列表投影一致（B1）：
//   - 集合级有 read 且 docSec：文档 read ACE 命中（∨ write 可见性在包 C 的
//     tw_visible 落地）或空 _acl 回退集合级；
//   - 无集合级 read 但 docSec：仅文档 read ACE 命中；
//   - docSec=false / 系统集合：上层 SkipDocumentPermissionFilter 已跳过。
func (p *postgresDocumentDB) listPermissionFilter(
	ctx context.Context,
	coll *databases.Collection,
	principal databases.Principal,
) (where string, args []any, err error) {
	if principal.BypassesDocumentACL() {
		return "", nil, nil
	}
	if err := p.ensureCollectionAccessible(coll, principal); err != nil {
		return "", nil, err
	}
	if databases.ListAccessDenied(coll, principal.Roles) {
		return "", nil, ErrPermissionDenied
	}
	if databases.SkipDocumentPermissionFilter(coll, principal.Roles) {
		return "", nil, nil
	}

	expanded := databases.ExpandPermissionRoles(principal.Roles)
	// 用户集合 documentSecurity=true 且集合级有 read（系统集合+集合级 read 已在
	// 上方 Skip 分支返回）：文档 _acl 命中 read ACE 必须匹配读权限，空 _acl 由
	// 集合级 read 兜底（与 AllowsDocumentAccess 的 docHasPerms=false → collOK
	// 一致，B1）。租户隔离由主查询 d._tenant = ? 谓词承载（_acl 行内嵌）。
	if databases.CollectionAllows(coll.Permissions, "read", expanded) && coll.DocumentSecurity {
		return `(d._acl && ?::text[] OR cardinality(d._acl) = 0)`,
			[]any{pgTextArray(aclMatchKeys("read", expanded))}, nil
	}
	return `d._acl && ?::text[]`,
		[]any{pgTextArray(aclMatchKeys("read", expanded))}, nil
}

// missingRowsError 探测批量化路径的缺失行（RETURNING / FOR UPDATE 行集与
// 输入的差集）成因（阶段③包 C）：可见（经 SELECT policy）但未被主语句触及
// ⇒ 不可写 ⇒ ErrPermissionDenied；不可见 ⇒ 不存在 ⇒ ErrDocumentNotFound
// （"0 行⇒不存在"仅对不可见行成立——可见不可写行须回 PERMISSION_DENIED）。
func (p *postgresDocumentDB) missingRowsError(ctx context.Context, tbl string, missing []string, tenant int64) error {
	if len(missing) == 0 {
		return nil
	}
	visible := false
	err := eachIDChunk(missing, bulkInChunk, func(chunk []string) error {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, tenant)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := p.conn(ctx).QueryContext(ctx, fmt.Sprintf(
			`SELECT 1 FROM %s WHERE _tenant = ? AND _id IN (%s) LIMIT 1`, tbl, inPlaceholders(len(chunk))),
			args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		if rows.Next() {
			visible = true
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if visible {
		return ErrPermissionDenied
	}
	return fmt.Errorf("%w", databases.ErrDocumentNotFound)
}

func buildIncrementParts(increment map[string]int64) (setParts []string, args []any) {
	for k, delta := range increment {
		if !safeNameRe.MatchString(k) || strings.HasPrefix(k, "_") || delta == 0 {
			continue
		}
		setParts = append(setParts, fmt.Sprintf("%s = COALESCE(%s, 0) + ?", quoteIdent(k), quoteIdent(k)))
		args = append(args, delta)
	}
	return setParts, args
}

// buildArrayParts 编译数组列原子更新（阶段③-b 预决策 3；Intersect/Diff/
// Insert/Filter 补齐于转出 POC B1）为单语句 SET 子句，与 data/increment 拼进
// 同一 UPDATE：
//
//	append    → col = COALESCE(col, '{}'::T[]) || ?::T[]（NULL 列视为空数组）
//	prepend   → col = ?::T[] || COALESCE(col, '{}'::T[])
//	remove    → 差集；移空后为空数组（非 NULL）；NULL 列保持 NULL
//	unique    → 保首次出现序去重；NULL 列保持 NULL
//	intersect → col ∩ values（去重、保 col 首次出现序）；移空后空数组；NULL 保真
//	diff      → col 减 values（保序、不去重，与 remove 同构）；NULL 保真
//	insert    → 定点插入（0 基 index；越界 = 尾插；NULL 列视为空数组）
//	filter    → 受限形态 = 移除等于任一 values（与 remove 等价）；NULL 保真
//
// 添加类算子（append/prepend/insert）对 NULL 列归一（视为空数组），读改写类
// （remove/unique/intersect/diff/filter）保真——NULL 列保持 NULL。
// 校验（InvalidArgument）：键安全、列在 attrs 且 array=true、不与 data 同列
// （同一 SET 子句同列双赋值歧义）、APPEND/PREPEND/REMOVE/INTERSECT/DIFF/
// FILTER 要求 values >= 1、INSERT 要求 values 恰 1 且 index 已设置且 >= 0。
// SET 右侧的裸列名引用旧行值（UPDATE 语义），bulk 的 `UPDATE tbl AS d SET`
// 下同样合法。insert 无 PG 内建（array_insert 未进 PG 18）——以 unnest
// WITH ORDINALITY + UNION ALL 重排等价实现（position 越界自然退化为尾插）。
func buildArrayParts(arrayUpdates map[string]databases.ArrayUpdate, data map[string]any, attrs []databases.Attribute) (setParts []string, args []any, err error) {
	if len(arrayUpdates) == 0 {
		return nil, nil, nil
	}
	attrByKey := make(map[string]databases.Attribute, len(attrs))
	for _, a := range attrs {
		attrByKey[a.Key] = a
	}
	keys := make([]string, 0, len(arrayUpdates))
	for k := range arrayUpdates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		upd := arrayUpdates[k]
		if !safeNameRe.MatchString(k) || strings.HasPrefix(k, "_") {
			return nil, nil, status.Error(codes.InvalidArgument, fmt.Sprintf("invalid array update attribute: %s", k))
		}
		if _, dup := data[k]; dup {
			return nil, nil, status.Errorf(codes.InvalidArgument, "attribute %q appears in both data and array_updates", k)
		}
		attr, ok := attrByKey[k]
		if !ok {
			return nil, nil, status.Errorf(codes.InvalidArgument, "array update on unknown attribute: %s", k)
		}
		if !attr.Array {
			return nil, nil, status.Errorf(codes.InvalidArgument, "array update requires an array attribute: %s", k)
		}
		arrType := pgArrayTypeFor(attr.Type)
		col := quoteIdent(k)
		switch upd.Op {
		case databases.ArrayUpdateOpAppend, databases.ArrayUpdateOpPrepend:
			if len(upd.Values) < 1 {
				return nil, nil, status.Errorf(codes.InvalidArgument, "%s on %q requires at least 1 value", upd.Op, k)
			}
			if upd.Op == databases.ArrayUpdateOpAppend {
				setParts = append(setParts, fmt.Sprintf("%s = COALESCE(%s, '{}'::%s) || ?::%s", col, col, arrType, arrType))
			} else {
				setParts = append(setParts, fmt.Sprintf("%s = ?::%s || COALESCE(%s, '{}'::%s)", col, arrType, col, arrType))
			}
			args = append(args, pgTextArray(upd.Values))
		case databases.ArrayUpdateOpRemove, databases.ArrayUpdateOpDiff, databases.ArrayUpdateOpFilter:
			if len(upd.Values) < 1 {
				return nil, nil, status.Errorf(codes.InvalidArgument, "%s on %q requires at least 1 value", upd.Op, k)
			}
			// 移空 → COALESCE 兜底空数组（预决策：移空后为空数组非 NULL）。
			setParts = append(setParts, fmt.Sprintf(
				`%s = CASE WHEN %s IS NULL THEN NULL ELSE COALESCE((SELECT array_agg(e) FROM unnest(%s) e WHERE e <> ALL(?::%s)), '{}'::%s) END`,
				col, col, col, arrType, arrType))
			args = append(args, pgTextArray(upd.Values))
		case databases.ArrayUpdateOpUnique:
			// WITH ORDINALITY + min(o) 分组 + array_agg(ORDER BY o)：精确保
			// 首次出现序去重（DISTINCT 无法保序）。
			setParts = append(setParts, fmt.Sprintf(
				`%s = CASE WHEN %s IS NULL THEN NULL ELSE (SELECT array_agg(e ORDER BY o) FROM (SELECT e, min(o) AS o FROM unnest(%s) WITH ORDINALITY AS u(e, o) GROUP BY e) s) END`,
				col, col, col))
		case databases.ArrayUpdateOpIntersect:
			if len(upd.Values) < 1 {
				return nil, nil, status.Errorf(codes.InvalidArgument, "intersect on %q requires at least 1 value", k)
			}
			// 交集去重保原序：仅保留 col 中属于 values 的元素，min(o) 折叠
			// 重复（首次出现序）；移空 → 空数组。
			setParts = append(setParts, fmt.Sprintf(
				`%s = CASE WHEN %s IS NULL THEN NULL ELSE COALESCE((SELECT array_agg(e ORDER BY o) FROM (SELECT e, min(o) AS o FROM unnest(%s) WITH ORDINALITY AS u(e, o) WHERE e = ANY(?::%s) GROUP BY e) s), '{}'::%s) END`,
				col, col, col, arrType, arrType))
			args = append(args, pgTextArray(upd.Values))
		case databases.ArrayUpdateOpInsert:
			if len(upd.Values) != 1 {
				return nil, nil, status.Errorf(codes.InvalidArgument, "insert on %q requires exactly 1 value", k)
			}
			if upd.Index == nil {
				return nil, nil, status.Errorf(codes.InvalidArgument, "insert on %q requires index", k)
			}
			if *upd.Index < 0 {
				return nil, nil, status.Errorf(codes.InvalidArgument, "insert on %q requires a non-negative index", k)
			}
			// 0 基 index → 1 基 position = index + 1；越界（position > len+1）
			// 时旧行全部落在新元素之前，ORDER BY o 自然退化为尾插（append）。
			// 单元素绑定按元素类型 cast（pgArrayTypeFor 是 T[] 形态，去 [] 后缀）。
			pos := int64(*upd.Index) + 1
			elemType := strings.TrimSuffix(arrType, "[]")
			setParts = append(setParts, fmt.Sprintf(
				`%s = (SELECT array_agg(e ORDER BY o) FROM (SELECT e, CASE WHEN o >= ? THEN o + 1 ELSE o END AS o FROM unnest(COALESCE(%s, '{}'::%s)) WITH ORDINALITY AS u(e, o) UNION ALL SELECT ?::%s AS e, ? AS o) s)`,
				col, col, arrType, elemType))
			args = append(args, pos, upd.Values[0], pos)
		default:
			return nil, nil, status.Errorf(codes.InvalidArgument, "unsupported array update op: %s", upd.Op)
		}
	}
	return setParts, args, nil
}

func (p *postgresDocumentDB) BulkUpdateDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	documentIDs []string,
	data map[string]any,
	perms []databases.Permission,
	principal databases.Principal,
) (int64, error) {
	if len(documentIDs) == 0 {
		return 0, nil
	}
	// 整体包在带执行身份的单个事务里（A1：每请求一事务），中途失败整体回滚
	//（行为从"部分成功"收紧为"原子"）。
	var affected int64
	err := p.withDocumentTx(ctx, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		n, err := p.bulkUpdateDocuments(txCtx, projectID, databaseID, collectionID, documentIDs, data, perms, principal)
		if err != nil {
			return err
		}
		affected = n
		return nil
	})
	if err != nil {
		return 0, p.mapError(err)
	}
	return affected, nil
}

// bulkInChunk 是批量化语句的分片上限（PG 单语句 65535 参数上限的防御；
// MaxBulkOperations=1000 之下 IN 列表通常单批完成）。
const bulkInChunk = 900

// inPlaceholders 生成 n 个 ? 占位符（IN 列表用）。
func inPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// eachIDChunk 按上限分片遍历 docID 列表，fn 返回错误时短路。
func eachIDChunk(ids []string, chunkSize int, fn func(chunk []string) error) error {
	for start := 0; start < len(ids); start += chunkSize {
		end := min(start+chunkSize, len(ids))
		if err := fn(ids[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// dedupeIDs 去重保序：批量化后同一 _id 在 IN 列表 / 多值 VALUES 中重复出现
// 只会命中一行，bulk 语义按"唯一文档集合"执行（affected = 唯一文档数）。
func dedupeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// bulkPrefetchContext 承载批量化路径的共享预取结果：集合（一次 GetCollection
// 复用于逐文档权限判定与事件 ACL 快照）与 _acl 批量映射（既供权限判定也
// 作事件写前 ACL 快照）。
type bulkPrefetchContext struct {
	coll       *databases.Collection
	permsByDoc map[string][]databases.Permission
}

// prefetchBulkAccess 做批量化前置工作：sentinel 系统集合的逐文档 permType 权限
// 全量校验（同一 databases.AllowsDocumentAccess 判定）+ 事件所需写前 ACL 批量
// 预取。判定执行点（阶段③包 C）：业务集合的逐文档校验退役——UPDATE/DELETE
// policy USING 裁决（不可见/不可写 ⇒ RETURNING/受影响行缺失 ⇒ NotFound），
// 此处仅取事件快照。sentinel 被拒 → ErrPermissionDenied（整体回滚）。
func (p *postgresDocumentDB) prefetchBulkAccess(
	ctx context.Context,
	projectID, databaseID, collectionID, tbl string,
	ids []string,
	tenant int64,
	permType string,
	principal databases.Principal,
	needEventACL bool,
) (*bulkPrefetchContext, error) {
	pc := &bulkPrefetchContext{permsByDoc: map[string][]databases.Permission{}}
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	if !principal.BypassesDocumentACL() && isSystem {
		coll, err := p.GetCollection(ctx, projectID, databaseID, collectionID)
		if err != nil {
			return nil, err
		}
		if err := p.ensureCollectionAccessible(coll, principal); err != nil {
			return nil, err
		}
		pc.coll = coll
		if pc.permsByDoc, err = p.getDocumentACLBatch(ctx, tbl, ids, tenant); err != nil {
			return nil, err
		}
		for _, docID := range ids {
			docPerms := pc.permsByDoc[docID]
			if !databases.AllowsDocumentAccess(coll, docPerms, len(docPerms) > 0, permType, principal.Roles) {
				return nil, ErrPermissionDenied
			}
		}
	} else if needEventACL {
		var err error
		if pc.permsByDoc, err = p.getDocumentACLBatch(ctx, tbl, ids, tenant); err != nil {
			return nil, err
		}
	}
	return pc, nil
}

// collectionForEvents 取事件 ACL 快照所需的集合（Bulk 路径一次取回复用，
// 权限校验已取时直接复用；principal 旁路 ACL 时此处才产生一次查询）。
func (p *postgresDocumentDB) collectionForEvents(ctx context.Context, projectID, databaseID, collectionID string, pc *bulkPrefetchContext) (*databases.Collection, error) {
	if pc.coll != nil {
		return pc.coll, nil
	}
	return p.GetCollection(ctx, projectID, databaseID, collectionID)
}

// bulkUpdateDocuments 是 BulkUpdateDocuments 的事务体（R5-P2-6 批量化）：
// 先全量校验（docID、写保护系统集合、逐文档 update 权限——批量预取 _acl
// + 同一 AllowsDocumentAccess 判定），再单条 UPDATE ... IN ... RETURNING 取
// 写后快照（_acl 替换内嵌 SET 子句，阶段③包 A），随后每文档一条 outbox。
// 任一文档不存在（RETURNING 行数不足）或权限拒绝 → 整体回滚
// （all-or-nothing，与原逐条循环语义一致）。Bulk 是唯一 SkipVersion=true 的
// Update 调用方（LWW）：无 ExpectedVersion 比对，但非系统集合仍
// _version = _version + 1。
func (p *postgresDocumentDB) bulkUpdateDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	documentIDs []string,
	data map[string]any,
	perms []databases.Permission,
	principal databases.Principal,
) (int64, error) {
	for _, docID := range documentIDs {
		if err := validateDocID(docID); err != nil {
			return 0, err
		}
	}
	ids := dedupeIDs(documentIDs)
	internalID, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return 0, err
	}
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	if err := p.requireVersionColumn(ctx, schema, physical, isSystem); err != nil {
		return 0, err
	}
	tbl := tableName(schema, physical)
	// 写保护系统集合防线与单条 updateDocument 一致：owner（user:<docID>）
	// 例外，批量下要求每个文档都命中例外，否则整体拒绝。
	if !principal.BypassesDocumentACL() && isWriteProtectedSystemCollection(databaseID, collectionID) {
		for _, docID := range ids {
			if !principal.HasRole(fmt.Sprintf("user:%s", docID)) {
				return 0, ErrPermissionDenied
			}
		}
	}
	// B4 生命周期：deprecated/migrating 属性写入拒收（批 data 单次校验）。
	if !principal.BypassesDocumentACL() {
		if err := p.lifecycleViolation(ctx, projectID, databaseID, collectionID, nil, dataKeys(data)); err != nil {
			return 0, err
		}
	}
	wantEvents := p.pub != nil && !isSystem
	pc, err := p.prefetchBulkAccess(ctx, projectID, databaseID, collectionID, tbl, ids, internalID, "update", principal, wantEvents)
	if err != nil {
		return 0, err
	}
	if wantEvents {
		if pc.coll, err = p.collectionForEvents(ctx, projectID, databaseID, collectionID, pc); err != nil {
			return 0, err
		}
	}
	// 同一 data 组装 SET 子句（与 updateDocument 三分支一致：data 非空 →
	// 字段 + 审计列；仅权限变更 → 只刷审计列；两者皆空 → ErrNoFieldsToUpdate）。
	updatedBy := userIDFromPrincipal(principal)
	vectorCols, err := p.vectorColumnsOf(ctx, projectID, databaseID, collectionID, schema, physical)
	if err != nil {
		return 0, err
	}
	setParts, args, err := buildUpdateParts(databases.Document{Data: data}, updatedBy, vectorCols)
	if err != nil {
		return 0, err
	}
	if len(setParts) == 0 && len(perms) == 0 {
		return 0, fmt.Errorf("%w", databases.ErrNoFieldsToUpdate)
	}
	if len(setParts) == 0 {
		// R02-P1-4：仅权限变更时同样刷新审计列（_updated_at/_updated_by）。
		setParts = append(setParts, "_updated_at = ?")
		args = append(args, time.Now())
		if updatedBy != "" {
			setParts = append(setParts, quoteIdent("_updated_by")+" = ?")
			args = append(args, updatedBy)
		}
	}
	// _acl 不进主语句（阶段③包 C，与 updateDocument 同因）：PG 对修改 SELECT
	// policy 引用列的 UPDATE 以新行复检 SELECT policy（自锁被拒）；主语句裁决
	// 行级 update 权限，_acl 替换走随后的 tw_system 第二语句（限主语句实际
	// 更新过的行）。
	if !isSystem {
		// 每次成功写 +1（含权限-only 更新；SkipVersion 同样 +1）。
		setParts = append(setParts, "_version = _version + 1")
	}
	// 单条 UPDATE ... IN ... RETURNING：写后快照一步取回（与 GetDocument 的
	// 行→JSON 扫描同语义，_acl 含在载荷内顺带回填；vector 列投影覆盖为 JSON
	// 数组，与读回契约一致）；RETURNING 行数不足说明有文档不存在（权限已全
	// 过）→ ErrDocumentNotFound 整体回滚。
	byID := make(map[string]*databases.Document, len(ids))
	if err := eachIDChunk(ids, bulkInChunk, func(chunk []string) error {
		sqlArgs := make([]any, 0, len(args)+len(chunk)+1)
		sqlArgs = append(sqlArgs, args...)
		for _, id := range chunk {
			sqlArgs = append(sqlArgs, id)
		}
		sqlArgs = append(sqlArgs, internalID)
		rows, err := p.conn(ctx).QueryContext(ctx, fmt.Sprintf(
			`UPDATE %s AS d SET %s WHERE d._id IN (%s) AND d._tenant = ? RETURNING %s AS doc`,
			tbl, strings.Join(setParts, ", "), inPlaceholders(len(chunk)), vectorProjection(vectorCols)),
			sqlArgs...)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %s", ErrDuplicateKey, err.Error())
			}
			return fmt.Errorf("bulk update documents: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			doc, err := parseDocumentJSON(raw)
			if err != nil {
				return err
			}
			if doc != nil {
				byID[doc.ID] = doc
			}
		}
		return rows.Err()
	}); err != nil {
		return 0, err
	}
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := byID[id]; !ok {
			missing = append(missing, id)
		}
	}
	// 阶段③包 C：主语句（UPDATE policy）未触及的行 → 探测区分
	// 可见不可写（PERMISSION_DENIED）与不存在（NOT_FOUND）。
	if err := p.missingRowsError(ctx, tbl, missing, internalID); err != nil {
		return 0, err
	}
	// _acl 替换（tw_set_document_acl，阶段③-b 包 C）：仅作用于主语句实际更新过
	// 的行（byID 键集）——不可见/不可写的行已在主语句处缺失并整体回滚。
	// tw_system 身份切换的第二语句路径已退役（函数 SECURITY DEFINER owner=
	// tw_system 承载 BYPASSRLS 语义）。
	if len(perms) > 0 {
		updatedIDs := make([]string, 0, len(byID))
		for id := range byID {
			updatedIDs = append(updatedIDs, id)
		}
		if err := eachIDChunk(updatedIDs, bulkInChunk, func(chunk []string) error {
			for _, id := range chunk {
				if err := p.setDocumentACL(ctx, schema, physical, internalID, id, perms); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return 0, err
		}
	}
	// 每文档一条 update 事件（实时订阅按 doc 过滤）：version/data=写后
	//（RETURNING 快照，Permissions 由 _acl 解析——替换时为新 perms，否则为
	// 写前 perms），acl=写前（批量预取的 permsByDoc）。
	if wantEvents {
		for _, docID := range ids {
			updated := *byID[docID]
			// 替换路径的写后 _acl = 新 perms（系统语句已落）；未替换时 byID
			// 快照的 _acl 即当前值。
			if len(perms) > 0 {
				updated.Permissions = perms
			}
			pre := pc.permsByDoc[docID]
			if err := p.publishDocumentEvent(ctx, projectID, databaseID, collectionID, docID,
				domainevents.EventDocumentsUpdate, updated.Version, &updated, pre, len(pre) > 0, pc.coll); err != nil {
				return 0, err
			}
		}
	}
	return int64(len(ids)), nil
}

func (p *postgresDocumentDB) BulkDeleteDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	documentIDs []string,
	principal databases.Principal,
) (int64, error) {
	if len(documentIDs) == 0 {
		return 0, nil
	}
	var affected int64
	err := p.withDocumentTx(ctx, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		n, err := p.bulkDeleteDocuments(txCtx, projectID, databaseID, collectionID, documentIDs, principal)
		if err != nil {
			return err
		}
		affected = n
		return nil
	})
	if err != nil {
		return 0, p.mapError(err)
	}
	return affected, nil
}

// bulkDeleteDocuments 是 BulkDeleteDocuments 的事务体（R5-P2-6 批量化）：
// 先全量校验（docID、写保护系统集合、逐文档 delete 权限——批量预取 +
// 同一 AllowsDocumentAccess 判定），非系统集合再 FOR UPDATE 批量锁行取写前
// _version（行锁语义保留；缺失 → ErrDocumentNotFound 整体回滚），随后批量
// 删行（_acl 随行消亡，无跨表清理），最后每文档一条 delete outbox
// （version/acl 均写前）。Bulk 是唯一 SkipVersion=true 的 Delete 调用方
// （LWW）；系统集合与单条 deleteDocument 一致：不做存在性检查、不发事件。
func (p *postgresDocumentDB) bulkDeleteDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	documentIDs []string,
	principal databases.Principal,
) (int64, error) {
	for _, docID := range documentIDs {
		if err := validateDocID(docID); err != nil {
			return 0, err
		}
	}
	ids := dedupeIDs(documentIDs)
	internalID, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return 0, err
	}
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	if err := p.requireVersionColumn(ctx, schema, physical, isSystem); err != nil {
		return 0, err
	}
	tbl := tableName(schema, physical)
	// 写保护系统集合防线：delete 路径无 owner 例外（与 deleteDocument 一致）。
	if !principal.BypassesDocumentACL() && isWriteProtectedSystemCollection(databaseID, collectionID) {
		return 0, ErrPermissionDenied
	}
	wantEvents := p.pub != nil && !isSystem
	pc, err := p.prefetchBulkAccess(ctx, projectID, databaseID, collectionID, tbl, ids, internalID, "delete", principal, wantEvents)
	if err != nil {
		return 0, err
	}
	if wantEvents {
		if pc.coll, err = p.collectionForEvents(ctx, projectID, databaseID, collectionID, pc); err != nil {
			return 0, err
		}
	}
	// 写前 _version（无锁预读——FOR UPDATE 会叠加 UPDATE policy，delete-only
	// 用户被误拒；删除原子性由 DELETE policy + 事务承载）：事件 version 与 acl
	// 都基于写前状态；有文档不可见 → 整体回滚。
	versions := make(map[string]int64, len(ids))
	if !isSystem {
		if err := eachIDChunk(ids, bulkInChunk, func(chunk []string) error {
			sqlArgs := make([]any, 0, len(chunk)+1)
			for _, id := range chunk {
				sqlArgs = append(sqlArgs, id)
			}
			sqlArgs = append(sqlArgs, internalID)
			rows, err := p.conn(ctx).QueryContext(ctx, fmt.Sprintf(
				`SELECT d._id, d._version FROM %s AS d WHERE d._id IN (%s) AND d._tenant = ?`,
				tbl, inPlaceholders(len(chunk))),
				sqlArgs...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var docID string
				var version int64
				if err := rows.Scan(&docID, &version); err != nil {
					return err
				}
				versions[docID] = version
			}
			return rows.Err()
		}); err != nil {
			return 0, err
		}
		missing := make([]string, 0, len(ids))
		for _, id := range ids {
			if _, ok := versions[id]; !ok {
				missing = append(missing, id)
			}
		}
		// FOR UPDATE 叠加 UPDATE policy（PG 锁语句语义）——未锁到的行探测区分
		// 可见不可写（PERMISSION_DENIED）与不存在（NOT_FOUND）。
		if err := p.missingRowsError(ctx, tbl, missing, internalID); err != nil {
			return 0, err
		}
	}
	var deleted int64
	if err := eachIDChunk(ids, bulkInChunk, func(chunk []string) error {
		sqlArgs := make([]any, 0, len(chunk)+1)
		for _, id := range chunk {
			sqlArgs = append(sqlArgs, id)
		}
		sqlArgs = append(sqlArgs, internalID)
		res, err := p.conn(ctx).ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE _id IN (%s) AND _tenant = ?`, tbl, inPlaceholders(len(chunk))),
			sqlArgs...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		deleted += n
		return nil
	}); err != nil {
		return 0, err
	}
	if !isSystem && deleted != int64(len(ids)) {
		// 残留行探测：未删掉的行要么可见不可删（DELETE policy 拒绝 ⇒
		// PERMISSION_DENIED），要么不可见（防御性 NotFound——并发路径异常）。
		if err := p.missingRowsError(ctx, tbl, ids, internalID); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("%w", databases.ErrDocumentNotFound)
	}
	// 每文档一条 delete 事件：version=写前、acl=写前、data=nil。
	if wantEvents {
		for _, docID := range ids {
			pre := pc.permsByDoc[docID]
			if err := p.publishDocumentEvent(ctx, projectID, databaseID, collectionID, docID,
				domainevents.EventDocumentsDelete, versions[docID], nil, pre, len(pre) > 0, pc.coll); err != nil {
				return 0, err
			}
		}
	}
	return int64(len(ids)), nil
}

// DeleteAttribute 是删列两段的段一（转出 POC B4，redesign §4.6）：属性置
// deprecated——读投影屏蔽（读回剥离该键）、查询白名单拒绝、写入拒收；物理
// 列与数据保留（可回滚 RestoreAttribute）。段二 RetireAttribute 物理 DROP
// COLUMN（CASCADE 清依赖索引）+ attrs 条目移除。
// 语义升级自 B4 前的"即时物理删列"——契约对齐 §4.6 删列行（两段、可回滚）。
func (p *postgresDocumentDB) DeleteAttribute(ctx context.Context, projectID, databaseID, collectionID, key string) error {
	if _, err := p.resolveInternalID(ctx, projectID); err != nil {
		return p.mapError(err)
	}
	if !safeNameRe.MatchString(key) {
		return p.mapError(fmt.Errorf("invalid attribute key: %s", key))
	}
	_, _, _, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return p.mapError(err)
	}
	return p.mapError(p.db.RunInTx(ctx, func(txCtx context.Context) error {
		row, err := p.loadCollectionRow(txCtx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		attrs, err := decodeAttributes(row.Attrs)
		if err != nil {
			return err
		}
		found := false
		for i := range attrs {
			if attrs[i].Key != key {
				continue
			}
			found = true
			switch attrs[i].StatusOrDefault() {
			case databases.AttrStatusDeprecated, databases.AttrStatusRetired:
				return nil // 幂等：已 deprecated/retired
			case databases.AttrStatusMigrating:
				return fmt.Errorf("%w: %q", ErrAttributeMigrating, key)
			}
			attrs[i].Status = databases.AttrStatusDeprecated
		}
		// 属性不存在时保持旧语义（0 行静默成功）。
		if !found {
			return nil
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

func (p *postgresDocumentDB) DeleteIndex(ctx context.Context, projectID, databaseID, collectionID, indexID string) error {
	if _, err := p.resolveInternalID(ctx, projectID); err != nil {
		return p.mapError(err)
	}
	_, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return p.mapError(err)
	}
	// R02-P1-1：DROP INDEX 与 catalog 元数据删除包进同一事务，任一步失败
	// 整体回滚，避免"物理索引已删而 catalog 仍记录"。
	return p.mapError(p.db.RunInTx(ctx, func(txCtx context.Context) error {
		idxName := quoteIdent(fmt.Sprintf("idx_%s_%s", physical, indexID))
		if _, err := p.conn(txCtx).ExecContext(txCtx,
			fmt.Sprintf(`DROP INDEX IF EXISTS %s.%s`, quoteIdent(schema), idxName),
		); err != nil {
			return err
		}
		row, err := p.loadCollectionRow(txCtx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		idxs, err := decodeIndexes(row.Indexes)
		if err != nil {
			return err
		}
		// 索引不存在时保持旧语义（DELETE 0 行静默成功）。
		kept := make([]databases.Index, 0, len(idxs))
		found := false
		for _, idx := range idxs {
			if idx.ID == indexID {
				found = true
				continue
			}
			kept = append(kept, idx)
		}
		if !found {
			return nil
		}
		idxsJSON, err := encodeIndexes(kept)
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
