// 文档 CRUD：Insert/Upsert/Update/Delete 事务体、OCC、冲突锁、事件发布、行→JSON 扫描。
package documentdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

func (p *postgresDocumentDB) CreateDocument(ctx context.Context, projectID, databaseID, collectionID string, doc databases.Document, perms []databases.Permission, principal databases.Principal) (databases.Document, error) {
	var out databases.Document
	err := p.withDocumentTx(ctx, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		created, err := p.createDocument(txCtx, projectID, databaseID, collectionID, doc, perms, principal)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	if err != nil {
		return out, p.mapError(err)
	}
	return out, nil
}

// createDocument 是 CreateDocument 的事务体：数据行（含 _acl 内嵌 ACE，阶段③
// 包 A）在同一事务内写入，无跨表权限步骤；权限写入失败导致文档 fail-open 的
// 窗口从结构上消灭。
func (p *postgresDocumentDB) createDocument(ctx context.Context, projectID, databaseID, collectionID string, doc databases.Document, perms []databases.Permission, principal databases.Principal) (databases.Document, error) {
	_, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return doc, err
	}
	if doc.ID == "" {
		doc.ID = idgen.UUID().String()
	} else if err := validateDocID(doc.ID); err != nil {
		return doc, err
	}
	tbl := tableName(schema, physical)
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	if err := p.requireVersionColumn(ctx, schema, physical, isSystem); err != nil {
		return doc, err
	}

	// Check collection-level accessibility before inserting. 集合级 create 判定
	//（阶段③包 C）：业务集合退役应用层检查——INSERT WITH CHECK policy 即判定
	//（拒绝 → 42501 → PERMISSION_DENIED）；sentinel 系统集合保留应用层。
	if !principal.BypassesDocumentACL() {
		if isWriteProtectedSystemCollection(databaseID, collectionID) {
			return doc, ErrPermissionDenied
		}
		coll, err := p.GetCollection(ctx, projectID, databaseID, collectionID)
		if err != nil {
			return doc, err
		}
		if err := p.ensureCollectionAccessible(coll, principal); err != nil {
			return doc, err
		}
		if databases.IsSystemCollection(projectID, databaseID, collectionID) &&
			coll != nil && !databases.CollectionAllows(coll.Permissions, "create", databases.ExpandPermissionRoles(principal.Roles)) {
			return doc, ErrPermissionDenied
		}
		// B4 生命周期：deprecated/migrating 属性写入拒收（coll 已加载，复用）。
		if err := p.lifecycleViolation(ctx, projectID, databaseID, collectionID, coll, dataKeys(doc.Data)); err != nil {
			return doc, err
		}
	}

	vectorCols, err := p.vectorColumnsOf(ctx, projectID, databaseID, collectionID, schema, physical)
	if err != nil {
		return doc, err
	}
	columns, placeholders, args, err := buildInsertParts(doc, vectorCols)
	if err != nil {
		return doc, err
	}
	createdBy := userIDFromPrincipal(principal)
	if createdBy != "" {
		if columns != "" {
			columns += ", "
			placeholders += ", "
		}
		columns += quoteIdent("_created_by") + ", " + quoteIdent("_updated_by")
		placeholders += "?, ?"
		args = append(args, createdBy, createdBy)
	}
	// _acl 随 INSERT 携带（R16 ③ 恢复通道：新行无旧行、可见性校验不适用，
	// 内容治理在 app 层授予校验；INSERT 列授权已恢复 _acl）。
	if len(perms) > 0 {
		if columns != "" {
			columns += ", "
			placeholders += ", "
		}
		columns += quoteIdent("_acl")
		placeholders += "?::text[]"
		args = append(args, aclParam(perms))
	}
	args = append([]any{doc.ID}, args...)
	allPlaceholders := "?"
	if columns != "" {
		allPlaceholders = "?, " + placeholders
		columns = ", " + columns
	}
	sql := fmt.Sprintf(`INSERT INTO %s (_id%s) VALUES (%s)`, tbl, columns, allPlaceholders)
	if _, err := p.conn(ctx).ExecContext(ctx, sql, args...); err != nil {
		if isUniqueViolation(err) {
			return doc, fmt.Errorf("%w: %s", ErrDuplicateKey, err.Error())
		}
		return doc, fmt.Errorf("insert document: %w", err)
	}
	created, err := p.GetDocument(ctx, projectID, databaseID, collectionID, doc.ID, databases.SystemPrincipal)
	if err != nil {
		return doc, err
	}
	if created == nil {
		return doc, errors.New("document not found after insert")
	}
	// create 事件：acl=写后（读回已含 _perms）；系统集合不发布。
	if err := p.publishDocumentEvent(ctx, projectID, databaseID, collectionID, created.ID,
		domainevents.EventDocumentsCreate, created.Version, created,
		created.Permissions, len(created.Permissions) > 0, nil); err != nil {
		return doc, err
	}
	return *created, nil
}

// UpsertDocument inserts doc, or when a row already matches conflictColumns
// (which must correspond to a unique index on the collection table), updates
// its data, _updated_at, _updated_by and replaces document permissions with
// perms (Appwrite-style replace semantics). The insert and update are
// performed atomically by a single INSERT ... ON CONFLICT (...) DO UPDATE SET
// statement.
func (p *postgresDocumentDB) UpsertDocument(ctx context.Context, projectID, databaseID, collectionID string, doc databases.Document, conflictColumns []string, perms []databases.Permission, principal databases.Principal) (databases.Document, error) {
	if err := validateDocID(doc.ID); err != nil {
		return doc, p.mapError(err)
	}
	if len(conflictColumns) == 0 {
		return doc, p.mapError(status.Error(codes.InvalidArgument, "conflict columns are required"))
	}
	conflictCols := make([]string, 0, len(conflictColumns))
	for _, col := range conflictColumns {
		if !safeNameRe.MatchString(col) || strings.HasPrefix(col, "_") {
			return doc, status.Error(codes.InvalidArgument, fmt.Sprintf("invalid conflict column: %s", col))
		}
		conflictCols = append(conflictCols, quoteIdent(col))
	}
	var out databases.Document
	err := p.withDocumentTx(ctx, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		upserted, err := p.upsertDocument(txCtx, projectID, databaseID, collectionID, doc, conflictCols, conflictColumns, perms, principal)
		if err != nil {
			return err
		}
		out = upserted
		return nil
	})
	if err != nil {
		return out, p.mapError(err)
	}
	return out, nil
}

// upsertDocument 是 UpsertDocument 的事务体（P0-1 TOCTOU 修复）：
// 预查与 INSERT ... ON CONFLICT DO UPDATE 包在同一事务，并对
// (_tenant, 冲突列值) 取 pg_advisory_xact_lock 串行化并发 upsert——
// 攻击者的预查发生在受害者提交之后（取锁后重查命中行），按 update
// 权限拒绝，无法再借 create 权限改写他人行；权限替换与读回同事务。
func (p *postgresDocumentDB) upsertDocument(ctx context.Context, projectID, databaseID, collectionID string, doc databases.Document, conflictCols, conflictColumns []string, perms []databases.Permission, principal databases.Principal) (databases.Document, error) {
	internalID, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return doc, err
	}
	tbl := tableName(schema, physical)
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	if err := p.requireVersionColumn(ctx, schema, physical, isSystem); err != nil {
		return doc, err
	}

	conflictValues, err := conflictArgs(conflictColumns, doc.Data)
	if err != nil {
		return doc, err
	}

	// 串行化同一冲突值上的并发 upsert（锁键仅用于互斥，碰撞仅导致轻微串行）。
	// 对所有主体（含 System）生效：System 并发插入若发生在预查与 INSERT 之间，
	// 会令 DO UPDATE 分支静默命中他人行（P0-1 残余窗口）。
	if _, err := p.conn(ctx).ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext(?), hashtext(?))`,
		strconv.FormatInt(internalID, 10), conflictLockKey(conflictValues)); err != nil {
		return doc, fmt.Errorf("acquire upsert lock: %w", err)
	}

	// 预查冲突目标行（所有主体）：命中的 _id 决定后续权限检查、权限替换与读回
	// 的目标（P2-1：新 _id + 冲突值命中他人行时，数据/权限作用在目标行上）。
	var targetID string
	row := p.conn(ctx).QueryRowContext(ctx, fmt.Sprintf(
		`SELECT _id FROM %s WHERE _tenant = ? AND %s LIMIT 1`,
		tbl, conflictWhereClause(conflictColumns)),
		append([]any{internalID}, conflictValues...)...)
	if err := row.Scan(&targetID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return doc, err
	}

	if !principal.BypassesDocumentACL() {
		if isWriteProtectedSystemCollection(databaseID, collectionID) {
			return doc, ErrPermissionDenied
		}
		coll, err := p.GetCollection(ctx, projectID, databaseID, collectionID)
		if err != nil {
			return doc, err
		}
		if err := p.ensureCollectionAccessible(coll, principal); err != nil {
			return doc, err
		}
		// B4 生命周期：deprecated/migrating 属性写入拒收（coll 已加载，复用）。
		if err := p.lifecycleViolation(ctx, projectID, databaseID, collectionID, coll, dataKeys(doc.Data)); err != nil {
			return doc, err
		}
		// 权限分叉以「conflictColumns 命中的目标行」为准，而非 doc.ID（P0-1）：
		// 命中已存在行 → 对该行做文档级 update 检查（UpdateDocument 语义）；
		// 未命中 → 纯插入 → 集合级 create 权限（CreateDocument 语义）。
		// 判定执行点（阶段③包 C）：业务集合交由语句自身——预查经 SELECT
		// policy 过滤（不可见 ⇒ 视为纯插入），ON CONFLICT 的 WITH CHECK
		//（create）与 UPDATE USING（update）共同裁决（§3.2 #7：upsert 需同时
		// 持有 create 与 update——语义有意收紧）；sentinel 保留应用层检查。
		if databases.IsSystemCollection(projectID, databaseID, collectionID) {
			if targetID != "" {
				if err := p.checkDocumentPermission(ctx, projectID, databaseID, collectionID, tbl, targetID, internalID, "update", principal, coll); err != nil {
					return doc, err
				}
			} else if !databases.CollectionAllows(coll.Permissions, "create", databases.ExpandPermissionRoles(principal.Roles)) {
				return doc, ErrPermissionDenied
			}
		}
		// conflictColumns 必须精确命中集合的一个 unique 索引（属性集相等，
		// 无序比较）——否则 ON CONFLICT 仲裁索引不存在，PG 报 42P10；前置校验
		// 给出可操作的错误。Bypass 主体不取 catalog，靠 42P10 兜底。
		if err := validateConflictColumns(coll, conflictColumns); err != nil {
			return doc, err
		}
	}
	// 目标行的实际 ID：预查命中用目标行，纯插入用 doc.ID。
	effectiveID := doc.ID
	if targetID != "" {
		effectiveID = targetID
	}

	// 更新支事件 acl=写前快照：在 upsert 语句之前抓拍（阶段③包 A——语句自身
	// 会替换 _acl，快照必须先行；旧实现的清/写分离已内嵌为 SET 子句）。
	var prePerms []databases.Permission
	var preHasPerms bool
	if targetID != "" && !isSystem && p.pub != nil {
		prePerms, preHasPerms, err = p.getDocumentACL(ctx, tbl, effectiveID, internalID)
		if err != nil {
			return doc, err
		}
	}

	// 分支执行（阶段③包 C：拆掉 INSERT ... ON CONFLICT——PG 的 ON CONFLICT
	// 推测插入要求"拟插入行通过 SELECT policy"，与 tw_visible 的文档可见性
	// 语义结构性冲突；预查分支 + 普通 INSERT/UPDATE 替代，advisory lock 已
	// 串行化同冲突键的并发 upsert（P0-1），分支判定无竞态）。
	//   - 纯插入：普通 INSERT（RLS INSERT WITH CHECK 裁决集合级 create）；
	//   - 命中目标：普通 UPDATE（RLS UPDATE USING 裁决文档级 update）。
	//（语义变化：与并发普通 Create 撞唯一键时不再转 update 支，报 DuplicateKey
	// 可重试——窗口极窄，记入文档。）
	vectorCols, err := p.vectorColumnsOf(ctx, projectID, databaseID, collectionID, schema, physical)
	if err != nil {
		return doc, err
	}
	columns, placeholders, args, err := buildInsertParts(doc, vectorCols)
	if err != nil {
		return doc, err
	}
	createdBy := userIDFromPrincipal(principal)
	if createdBy != "" {
		if columns != "" {
			columns += ", "
			placeholders += ", "
		}
		columns += quoteIdent("_created_by") + ", " + quoteIdent("_updated_by")
		placeholders += "?, ?"
		args = append(args, createdBy, createdBy)
	}
	if targetID == "" {
		// 插入支：_acl 随 INSERT 携带（R16 ③ 恢复通道；INSERT 无旧行复检）。
		if len(perms) > 0 {
			if columns != "" {
				columns += ", "
				placeholders += ", "
			}
			columns += quoteIdent("_acl")
			placeholders += "?::text[]"
			args = append(args, aclParam(perms))
		}
		args = append([]any{doc.ID}, args...)
		allPlaceholders := "?"
		if columns != "" {
			allPlaceholders = "?, " + placeholders
			columns = ", " + columns
		}
		sql := fmt.Sprintf(`INSERT INTO %s (_id%s) VALUES (%s)`, tbl, columns, allPlaceholders)
		if _, err := p.conn(ctx).ExecContext(ctx, sql, args...); err != nil {
			if isUniqueViolation(err) {
				return doc, fmt.Errorf("%w: %s", ErrDuplicateKey, err.Error())
			}
			return doc, fmt.Errorf("upsert document: %w", err)
		}
	} else {
		// 更新支：数据 + 审计列盲写（不做 OCC），用户集合 _version +1；_acl 不进
		// 主语句（SELECT policy 新行复检），走随后的 tw_system 第二语句。
		setParts, setArgs, err := buildUpdateParts(doc, userIDFromPrincipal(principal), vectorCols)
		if err != nil {
			return doc, err
		}
		if len(setParts) == 0 {
			return doc, status.Error(codes.InvalidArgument, "no fields to upsert")
		}
		if !isSystem {
			setParts = append(setParts, "_version = _version + 1")
		}
		setArgs = append(setArgs, effectiveID, internalID)
		res, err := p.conn(ctx).ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s SET %s WHERE _id = ? AND _tenant = ?`, tbl, strings.Join(setParts, ", ")),
			setArgs...)
		if err != nil {
			if isUniqueViolation(err) {
				return doc, fmt.Errorf("%w: %s", ErrDuplicateKey, err.Error())
			}
			return doc, fmt.Errorf("upsert document: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return doc, err
		}
		if n != 1 {
			return doc, fmt.Errorf("%w", databases.ErrDocumentNotFound)
		}
	}
	// 更新支的 _acl 替换（tw_set_document_acl，阶段③-b 包 C）：作用于目标行
	//（effectiveID）；行级权限已由语句的 WITH CHECK（create）与 UPDATE USING
	//（update）共同裁决。tw_system 身份切换的第二语句路径已退役（函数
	// SECURITY DEFINER owner=tw_system 承载 BYPASSRLS 语义）。
	if targetID != "" && len(perms) > 0 {
		if err := p.setDocumentACL(ctx, schema, physical, internalID, effectiveID, perms); err != nil {
			return doc, fmt.Errorf("replace upserted acl: %w", err)
		}
	}
	upserted, err := p.GetDocument(ctx, projectID, databaseID, collectionID, effectiveID, databases.SystemPrincipal)
	if err != nil {
		return doc, err
	}
	if upserted == nil {
		return doc, errors.New("document not found after upsert")
	}
	// upsert 事件：插入支 → create（acl=写后），更新支 → update（acl=写前）；
	// 系统集合不发布。
	if targetID == "" {
		if err := p.publishDocumentEvent(ctx, projectID, databaseID, collectionID, upserted.ID,
			domainevents.EventDocumentsCreate, upserted.Version, upserted,
			upserted.Permissions, len(upserted.Permissions) > 0, nil); err != nil {
			return doc, err
		}
	} else if err := p.publishDocumentEvent(ctx, projectID, databaseID, collectionID, upserted.ID,
		domainevents.EventDocumentsUpdate, upserted.Version, upserted, prePerms, preHasPerms, nil); err != nil {
		return doc, err
	}
	return *upserted, nil
}

// conflictLockKey 序列化冲突列值作为 advisory lock 键；仅用于互斥，
// 碰撞只造成轻微串行化，不影响正确性。采用 JSON 编码（而非分隔符拼接）：
// 拼接 + "\x00" 分隔在值本身含 \x00 或空串时存在歧义（R02-P2-2），
// JSON 对字符串转义与类型（int64 vs 字符串 "1"）均无歧义。
func conflictLockKey(values []any) string {
	b, err := json.Marshal(values)
	if err != nil {
		// 文档数据理论上均为 JSON 兼容类型；失败时回退空串，
		// 仅导致这些值共享同一锁键（轻微串行化），不影响正确性。
		return ""
	}
	return string(b)
}

func (p *postgresDocumentDB) GetDocument(ctx context.Context, projectID, databaseID, collectionID, docID string, principal databases.Principal) (*databases.Document, error) {
	var doc *databases.Document
	err := p.withDocumentTx(ctx, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		got, err := p.getDocument(txCtx, projectID, databaseID, collectionID, docID, principal)
		if err != nil {
			return err
		}
		doc = got
		return nil
	})
	if err != nil {
		return nil, p.mapError(err)
	}
	return doc, nil
}

// getDocument 是 GetDocument 的事务体（A1：读同走显式事务承载 GUC 注入）。
func (p *postgresDocumentDB) getDocument(ctx context.Context, projectID, databaseID, collectionID, docID string, principal databases.Principal) (*databases.Document, error) {
	if err := validateDocID(docID); err != nil {
		return nil, err
	}
	internalID, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, err
	}
	vectorCols, err := p.vectorColumnsOf(ctx, projectID, databaseID, collectionID, schema, physical)
	if err != nil {
		return nil, err
	}
	row := p.conn(ctx).QueryRowContext(ctx, fmt.Sprintf(`SELECT %s AS doc FROM %s d WHERE d._id = ? AND d._tenant = ?`, vectorProjection(vectorCols), tableName(schema, physical)), docID, internalID)
	doc, err := scanDocumentJSON(row)
	if err != nil {
		return nil, p.mapError(err)
	}
	if doc == nil {
		return nil, nil
	}
	// B4 读屏蔽：deprecated 属性读回剥离（数据仍在，RestoreAttribute 可回滚；
	// 集合无 deprecated 属性时 masking 为零改动跳过——代价一次 catalog 点查，
	// 优化挂账：负缓存 + DDL 失效）。
	if !databases.IsSystemCollection(projectID, databaseID, collectionID) {
		coll, cerr := p.GetCollection(ctx, projectID, databaseID, collectionID)
		if cerr != nil {
			return nil, p.mapError(cerr)
		}
		if coll != nil {
			maskDeprecatedDocument(coll.Attributes, doc)
		}
	}
	// 权限回填已免费化（阶段③包 A）：to_jsonb(d.*) 含 _acl，parseDocumentJSON
	// 顺带解析为 doc.Permissions。判定执行点（阶段③包 C）：业务集合由 SELECT
	// policy 隐式过滤（0 行 → nil → NotFound，防枚举）；sentinel 系统集合（静态
	// 平面，预决策 9）保留应用层判定。
	if !principal.BypassesDocumentACL() && databases.IsSystemCollection(projectID, databaseID, collectionID) {
		coll, cerr := p.getCollectionForAccess(ctx, projectID, databaseID, collectionID)
		if cerr != nil {
			return nil, p.mapError(cerr)
		}
		if cerr := p.checkDocumentACL(coll, doc.Permissions, len(doc.Permissions) > 0, "read", principal); cerr != nil {
			return nil, p.mapError(cerr)
		}
	}
	return doc, nil
}

func (p *postgresDocumentDB) UpdateDocument(ctx context.Context, projectID, databaseID, collectionID string, update databases.DocumentUpdate, principal databases.Principal) (databases.Document, error) {
	var out databases.Document
	err := p.withDocumentTx(ctx, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		updated, err := p.updateDocument(txCtx, projectID, databaseID, collectionID, update, principal)
		if err != nil {
			return err
		}
		out = updated
		return nil
	})
	if err != nil {
		return out, p.mapError(err)
	}
	return out, nil
}

// updateDocument 是 UpdateDocument 的事务体：数据语句与 _perms 替换在同一
// 事务内，任一步失败整体回滚；目标文档不存在时返回 ErrDocumentNotFound。
// 用户集合（非系统）强制 OCC：ExpectedVersion 必填且须等于当前行 _version；
// SkipVersion（仅 Bulk 内部循环）跳过校验但同样 _version = _version + 1。
func (p *postgresDocumentDB) updateDocument(ctx context.Context, projectID, databaseID, collectionID string, update databases.DocumentUpdate, principal databases.Principal) (databases.Document, error) {
	doc := update.Document
	if err := validateDocID(doc.ID); err != nil {
		return doc, err
	}
	internalID, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return doc, err
	}
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	if err := p.requireVersionColumn(ctx, schema, physical, isSystem); err != nil {
		return doc, err
	}
	occ := false
	if !isSystem && !update.SkipVersion {
		if update.ExpectedVersion <= 0 {
			return doc, databases.ErrVersionRequired
		}
		occ = true
	}
	// B4 生命周期：deprecated/migrating 属性写入拒收（data/increment/
	// array_updates 三通道同查）。
	if !principal.BypassesDocumentACL() {
		incKeys := make([]string, 0, len(update.Increment))
		for k := range update.Increment {
			incKeys = append(incKeys, k)
		}
		arrKeys := make([]string, 0, len(update.ArrayUpdates))
		for k := range update.ArrayUpdates {
			arrKeys = append(arrKeys, k)
		}
		if err := p.lifecycleViolation(ctx, projectID, databaseID, collectionID, nil,
			dataKeys(doc.Data), keysSet(incKeys), keysSet(arrKeys)); err != nil {
			return doc, err
		}
	}
	// 非 System 且非文档 owner（user:<id> 匹配）时，禁止写入写保护系统集合（纵深防御，
	// 与 CreateDocument/DeleteDocument 对齐，安全评审 C1 第 2 层）。
	// owner 例外：end-user 自助路径（UpdateAccount/UpdatePrefs）以 user:<id> 角色更新自己的 users 文档。
	if !principal.BypassesDocumentACL() &&
		!principal.HasRole(fmt.Sprintf("user:%s", doc.ID)) &&
		isWriteProtectedSystemCollection(databaseID, collectionID) {
		return doc, ErrPermissionDenied
	}
	tbl := tableName(schema, physical)
	// 判定与事件写前 ACL 快照共用一次 _acl 点查（阶段③包 A）。
	// 判定执行点（阶段③包 C）：业务集合的 update 权限由 UPDATE policy 的
	// USING 承载（tw_can(update)），应用层检查退役；sentinel 保留（D3：仅查
	// update 不预检 read 的语义由 policy USING 等价保持——可写即可读）。
	var prePerms []databases.Permission
	var preHasPerms bool
	if !principal.BypassesDocumentACL() && isSystem {
		coll, cerr := p.getCollectionForAccess(ctx, projectID, databaseID, collectionID)
		if cerr != nil {
			return doc, cerr
		}
		prePerms, preHasPerms, err = p.getDocumentACL(ctx, tbl, doc.ID, internalID)
		if err != nil {
			return doc, err
		}
		if cerr := p.checkDocumentACL(coll, prePerms, preHasPerms, "update", principal); cerr != nil {
			return doc, cerr
		}
	} else if !isSystem && p.pub != nil {
		prePerms, preHasPerms, err = p.getDocumentACL(ctx, tbl, doc.ID, internalID)
		if err != nil {
			return doc, err
		}
	}
	updatedBy := userIDFromPrincipal(principal)
	vectorCols, err := p.vectorColumnsOf(ctx, projectID, databaseID, collectionID, schema, physical)
	if err != nil {
		return doc, err
	}
	setParts, args, err := buildUpdateParts(doc, updatedBy, vectorCols)
	if err != nil {
		return doc, err
	}
	incParts, incArgs := buildIncrementParts(update.Increment)
	setParts = append(setParts, incParts...)
	args = append(args, incArgs...)
	// 数组列原子更新（阶段③-b 预决策 3）：需要 catalog attrs 做白名单与
	// 元素类型 cast，仅在使用时取一次集合。
	var arrParts []string
	var arrArgs []any
	if len(update.ArrayUpdates) > 0 {
		coll, err := p.GetCollection(ctx, projectID, databaseID, collectionID)
		if err != nil {
			return doc, err
		}
		if coll == nil {
			return doc, status.Error(codes.NotFound, "collection not found")
		}
		arrParts, arrArgs, err = buildArrayParts(update.ArrayUpdates, doc.Data, coll.Attributes)
		if err != nil {
			return doc, err
		}
		setParts = append(setParts, arrParts...)
		args = append(args, arrArgs...)
	}
	if len(setParts) == 0 && len(update.Permissions) == 0 {
		return doc, fmt.Errorf("%w", databases.ErrNoFieldsToUpdate)
	}
	if len(setParts) == 0 {
		// R02-P1-4：仅权限变更时同样刷新审计列，保证 _updated_at/_updated_by
		// 反映"最后修改"语义（buildUpdateParts 对空 Data 返回空 setParts）。
		setParts = append(setParts, "_updated_at = ?")
		args = append(args, time.Now())
		if updatedBy != "" {
			setParts = append(setParts, quoteIdent("_updated_by")+" = ?")
			args = append(args, updatedBy)
		}
	}
	// _acl 不进主语句（阶段③包 C）：UPDATE 修改 SELECT policy USING 引用的列
	//（_acl）时，PG 会以新行复检 SELECT policy——自锁（新 _acl 排除自己）即被
	// 拒，WITH CHECK(true) 无法单独豁免（PG 18 实证，预决策 4 的"允许自锁"
	// 经此路径保持）。拆为同事务内 tw_system 身份的第二条语句；行级 update
	// 权限已由主语句的 UPDATE policy USING 裁决（0 行即失败先行返回）。
	if len(setParts) > 0 {
		if !isSystem {
			// 每次成功写 +1（含权限-only 更新与 Increment；SkipVersion 同样 +1）。
			setParts = append(setParts, "_version = _version + 1")
		}
		args = append(args, doc.ID, internalID)
		where := "_id = ? AND _tenant = ?"
		if occ {
			where += " AND _version = ?"
			args = append(args, update.ExpectedVersion)
		}
		sqlText := fmt.Sprintf(`UPDATE %s SET %s WHERE %s`, tbl, strings.Join(setParts, ", "), where)
		res, err := p.conn(ctx).ExecContext(ctx, sqlText, args...)
		if err != nil {
			if isUniqueViolation(err) {
				return doc, fmt.Errorf("%w: %s", ErrDuplicateKey, err.Error())
			}
			return doc, fmt.Errorf("update document: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return doc, err
		}
		if affected == 0 {
			// 三态区分（阶段③包 C：UPDATE policy USING 参与 0 行成因；含
			// SkipVersion 路径——RLS 下静默跳过会让随后的系统语句越权）：
			// 探测（经 SELECT policy，可写即可读）→ 不可见 ⇒ NotFound；
			// 可见且 version 不符 ⇒ VersionMismatch；可见且 version 相符
			// ⇒ UPDATE policy 拒绝 ⇒ PERMISSION_DENIED。
			var existsVersion int64
			err := p.conn(ctx).QueryRowContext(ctx,
				fmt.Sprintf(`SELECT _version FROM %s WHERE _id = ? AND _tenant = ?`, tbl),
				doc.ID, internalID,
			).Scan(&existsVersion)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return doc, fmt.Errorf("%w", databases.ErrDocumentNotFound)
				}
				return doc, err
			}
			if occ && existsVersion != update.ExpectedVersion {
				// B10 §10.1：冲突携带探测读到的当前 _version（零额外查询），
				// app 层塞进 ErrorInfo metadata 的 current_version。
				return doc, &databases.VersionConflictError{CurrentVersion: existsVersion}
			}
			return doc, ErrPermissionDenied
		}
	}
	// _acl 替换（tw_set_document_acl，阶段③-b 包 C）：非空权限集整体替换（与旧
	// "先清后写 _perms"同语义）；同事务原子生效。tw_system 身份切换的第二语句
	// 路径已退役（函数 SECURITY DEFINER owner=tw_system 承载 BYPASSRLS 语义，
	// 自锁规避不变）。
	if len(update.Permissions) > 0 {
		if err := p.setDocumentACL(ctx, schema, physical, internalID, doc.ID, update.Permissions); err != nil {
			return doc, fmt.Errorf("replace document acl: %w", err)
		}
	}
	// D5：尾随读回用 SystemPrincipal（与 CreateDocument 一致）——B1 下把文档权限
	// 改成不含自己 read 的集合后数据已提交，若仍以调用方 principal 读回会返回
	// PermissionDenied（半完成状态）。
	updated, err := p.GetDocument(ctx, projectID, databaseID, collectionID, doc.ID, databases.SystemPrincipal)
	if err != nil {
		return doc, err
	}
	if updated == nil {
		return doc, fmt.Errorf("%w", databases.ErrDocumentNotFound)
	}
	// update / increment 事件：acl=写前快照；系统集合不发布。
	if err := p.publishDocumentEvent(ctx, projectID, databaseID, collectionID, updated.ID,
		domainevents.EventDocumentsUpdate, updated.Version, updated, prePerms, preHasPerms, nil); err != nil {
		return doc, err
	}
	return *updated, nil
}

func (p *postgresDocumentDB) DeleteDocument(ctx context.Context, projectID, databaseID, collectionID, docID string, opts databases.DeleteOptions, principal databases.Principal) error {
	return p.mapError(p.withDocumentTx(ctx, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		return p.deleteDocument(txCtx, projectID, databaseID, collectionID, docID, opts, principal)
	}))
}

// deleteDocument 是 DeleteDocument 的事务体：数据行删除即权限消亡（_acl 内嵌
// 行内，阶段③包 A——无跨表清理步骤）。用户集合（非系统）强制 OCC：
// ExpectedVersion 必填且须等于当前行 _version（行锁下比较，防止竞态）。
func (p *postgresDocumentDB) deleteDocument(ctx context.Context, projectID, databaseID, collectionID, docID string, opts databases.DeleteOptions, principal databases.Principal) error {
	if err := validateDocID(docID); err != nil {
		return err
	}
	internalID, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return err
	}
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	if err := p.requireVersionColumn(ctx, schema, physical, isSystem); err != nil {
		return err
	}
	tbl := tableName(schema, physical)
	if !principal.BypassesDocumentACL() && isWriteProtectedSystemCollection(databaseID, collectionID) {
		return ErrPermissionDenied
	}
	// 判定执行点（阶段③包 C）：业务集合的 delete 权限由 DELETE policy USING
	// 承载（不可见/不可删 → 0 行 → 存在性探测 → NotFound）；sentinel 保留
	// 应用层判定。事件写前 ACL 快照两模式都取。
	var prePerms []databases.Permission
	var preHasPerms bool
	if !principal.BypassesDocumentACL() && isSystem {
		coll, cerr := p.getCollectionForAccess(ctx, projectID, databaseID, collectionID)
		if cerr != nil {
			return cerr
		}
		prePerms, preHasPerms, err = p.getDocumentACL(ctx, tbl, docID, internalID)
		if err != nil {
			return err
		}
		if cerr := p.checkDocumentACL(coll, prePerms, preHasPerms, "delete", principal); cerr != nil {
			return cerr
		}
	} else if !isSystem && p.pub != nil {
		prePerms, preHasPerms, err = p.getDocumentACL(ctx, tbl, docID, internalID)
		if err != nil {
			return err
		}
	}
	if !isSystem {
		// 用户集合：取删除前 _version（delete 事件 version/acl 都基于写前）。
		// 无锁预读：FOR UPDATE 会叠加 UPDATE policy（PG 锁语句语义），delete-only
		// 用户被误拒——OCC 原子性由 DELETE 语句内的 _version 守卫承载
		//（compare-and-delete 单语句，防竞态等价）。
		var currentVersion int64
		err := p.conn(ctx).QueryRowContext(ctx,
			fmt.Sprintf(`SELECT _version FROM %s WHERE _id = ? AND _tenant = ?`, tbl),
			docID, internalID,
		).Scan(&currentVersion)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w", databases.ErrDocumentNotFound)
			}
			return err
		}
		if !opts.SkipVersion {
			if opts.ExpectedVersion <= 0 {
				return databases.ErrVersionRequired
			}
			if currentVersion != opts.ExpectedVersion {
				return &databases.VersionConflictError{CurrentVersion: currentVersion}
			}
		}
		// 分叉：有 publisher 时删除后带删除前 version 发 delete 事件；无
		// publisher 走下方公共路径。删除行数校验：可见（预读已过 SELECT
		// policy）且 OCC 守卫命中时 0 行 ⇒ 并发改写（VersionMismatch）或
		// DELETE policy 拒绝（PERMISSION_DENIED）。
		occ := !opts.SkipVersion
		if p.pub != nil {
			if err := p.execDeleteVersioned(ctx, tbl, docID, internalID, occ, opts.ExpectedVersion); err != nil {
				return err
			}
			return p.publishDocumentEvent(ctx, projectID, databaseID, collectionID, docID,
				domainevents.EventDocumentsDelete, currentVersion, nil, prePerms, preHasPerms, nil)
		}
		return p.execDeleteVersioned(ctx, tbl, docID, internalID, occ, opts.ExpectedVersion)
	}
	// sentinel 公共路径：不做存在性检查、无 RLS（静态平面，预决策 9）。
	_, err = p.conn(ctx).ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE _id = ? AND _tenant = ?`, tbl), docID, internalID)
	return err
}

// execDeleteVersioned 执行单文档 DELETE：OCC 时带 _version 守卫（compare-and-
// delete，与旧 FOR UPDATE 预读+比较的防竞态语义等价且不误拒 delete-only 用户）；
// 0 行 ⇒ OCC 守卫未命中（VersionMismatch）或 DELETE policy 拒绝
// （PERMISSION_DENIED——预读已过 SELECT policy，行可见）。
func (p *postgresDocumentDB) execDeleteVersioned(ctx context.Context, tbl, docID string, tenant int64, occ bool, expected int64) error {
	where := "_id = ? AND _tenant = ?"
	args := []any{docID, tenant}
	if occ {
		where += " AND _version = ?"
		args = append(args, expected)
	}
	res, err := p.conn(ctx).ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s`, tbl, where), args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if occ {
			var cur int64
			perr := p.conn(ctx).QueryRowContext(ctx,
				fmt.Sprintf(`SELECT _version FROM %s WHERE _id = ? AND _tenant = ?`, tbl),
				docID, tenant).Scan(&cur)
			if errors.Is(perr, sql.ErrNoRows) {
				return fmt.Errorf("%w", databases.ErrDocumentNotFound)
			}
			if perr != nil {
				return perr
			}
			if cur != expected {
				return &databases.VersionConflictError{CurrentVersion: cur}
			}
		}
		return ErrPermissionDenied
	}
	return nil
}

// publishDocumentEvent 在文档写成功读回之后、函数返回之前（仍在外层
// RunInTx 内）把事件写入 outbox，与文档行、_perms 同 COMMIT（v2 设计 §3.3）。
// 未注入 EventPublisher（单测）或系统集合时为空操作；acl 快照取当时
// collection ACL + 调用方提供的文档 _perms（create=写后 / update、delete=写前）。
// coll 为调用方已获取的集合（R5-P2-8/P2-6：Bulk 批量路径取一次复用，消除
// 每文档一次 GetCollection 的 N+1）；nil 时内部获取（单条路径行为不变）。
func (p *postgresDocumentDB) publishDocumentEvent(
	ctx context.Context,
	projectID, databaseID, collectionID, docID, event string,
	version int64,
	data *databases.Document,
	docPerms []databases.Permission,
	docHasPerms bool,
	coll *databases.Collection,
) error {
	if p.pub == nil {
		return nil
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return nil
	}
	if coll == nil {
		var err error
		coll, err = p.GetCollection(ctx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
	}
	if coll == nil || coll.IsSystem {
		return nil
	}
	return p.pub.Publish(ctx, domainevents.Envelope{
		Event:        event,
		ProjectID:    projectID,
		DatabaseID:   databaseID,
		CollectionID: collectionID,
		DocumentID:   docID,
		Version:      version,
		CreatedAt:    time.Now(),
		Data:         data,
		ACL: domainevents.ACLSnapshot{
			DocumentSecurity:      coll.DocumentSecurity,
			CollectionPermissions: coll.Permissions,
			DocumentPermissions:   docPerms,
			DocHasPerms:           docHasPerms,
		},
	})
}

// buildInsertParts 拼接数据列 INSERT 片段。数组值（阶段③-b）编码为 PG 数组
// 字面量字符串，目标列类型由 INSERT VALUES 推断（text[]/bigint[] 等按列解析，
// 与标量值的绑定路径同机制）；vector 值（会话 #10）编码为 pgvector 字面量 +
// ?::vector 绑定（列类型信息由 vectorCols 提供，nil = 无 vector 列）。
func buildInsertParts(doc databases.Document, vectorCols map[string]int) (columns string, placeholders string, args []any, err error) {
	if len(doc.Data) == 0 {
		return "", "", nil, nil
	}
	var cols []string
	var phs []string
	for k, v := range doc.Data {
		if !safeNameRe.MatchString(k) || strings.HasPrefix(k, "_") {
			continue
		}
		if dims, isVec := vectorCols[k]; isVec {
			if verr := validateVectorValue(k, v, dims); verr != nil {
				return "", "", nil, verr
			}
			lit, ok := pgVectorLiteral(v)
			if !ok {
				return "", "", nil, status.Error(codes.InvalidArgument, fmt.Sprintf(
					"attribute %q: vector value must be a JSON array of numbers", k))
			}
			cols = append(cols, quoteIdent(k))
			phs = append(phs, "?::vector")
			args = append(args, lit)
			continue
		}
		cols = append(cols, quoteIdent(k))
		if lit, isArr := pgArrayLiteral(v); isArr {
			phs = append(phs, "?")
			args = append(args, lit)
			continue
		}
		phs = append(phs, "?")
		args = append(args, v)
	}
	return strings.Join(cols, ", "), strings.Join(phs, ", "), args, nil
}

// conflictArgs extracts the conflict column values from doc.Data in
// conflictColumns order; a missing value is an InvalidArgument error.
func conflictArgs(conflictColumns []string, data map[string]any) ([]any, error) {
	args := make([]any, 0, len(conflictColumns))
	for _, col := range conflictColumns {
		v, ok := data[col]
		if !ok {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("missing value for conflict column: %s", col))
		}
		args = append(args, v)
	}
	return args, nil
}

// conflictWhereClause builds an equality predicate matching rows on the
// conflict columns, e.g. `"email" = ? AND "user_id" = ?`.
func conflictWhereClause(conflictColumns []string) string {
	parts := make([]string, 0, len(conflictColumns))
	for _, col := range conflictColumns {
		parts = append(parts, fmt.Sprintf("%s = ?", quoteIdent(col)))
	}
	return strings.Join(parts, " AND ")
}

func buildUpdateParts(doc databases.Document, updatedBy string, vectorCols map[string]int) (setParts []string, args []any, err error) {
	for k, v := range doc.Data {
		if !safeNameRe.MatchString(k) || strings.HasPrefix(k, "_") {
			continue
		}
		// vector 值（会话 #10）：字面量 + ::vector cast + 维度校验（同 INSERT）。
		if dims, isVec := vectorCols[k]; isVec {
			if verr := validateVectorValue(k, v, dims); verr != nil {
				return nil, nil, verr
			}
			lit, ok := pgVectorLiteral(v)
			if !ok {
				return nil, nil, status.Error(codes.InvalidArgument, fmt.Sprintf(
					"attribute %q: vector value must be a JSON array of numbers", k))
			}
			setParts = append(setParts, fmt.Sprintf("%s = ?::vector", quoteIdent(k)))
			args = append(args, lit)
			continue
		}
		// 数组值（阶段③-b）：整列替换语义，字面量 + 目标列推断（同 INSERT）。
		if lit, isArr := pgArrayLiteral(v); isArr {
			setParts = append(setParts, fmt.Sprintf("%s = ?", quoteIdent(k)))
			args = append(args, lit)
			continue
		}
		setParts = append(setParts, fmt.Sprintf("%s = ?", quoteIdent(k)))
		args = append(args, v)
	}
	if len(setParts) == 0 {
		return nil, nil, nil
	}
	setParts = append(setParts, "_updated_at = ?")
	args = append(args, time.Now())
	if updatedBy != "" {
		setParts = append(setParts, quoteIdent("_updated_by")+" = ?")
		args = append(args, updatedBy)
	}
	return setParts, args, nil
}

// userIDFromPrincipal extracts the first "user:"-prefixed role ID from the
// principal's roles, or "" when no user role is held.
// userIDFromPrincipal 返回写入审计列（_created_by/_updated_by）的归因主体：
// user:<id> 角色优先（存裸 id，兼容既有语义）；否则 API key 主体存 "key:<id>"
// （redesign §10.2-1：keys 写入行为可归因，原实现 keys-only 主体审计列为空）。
func userIDFromPrincipal(p databases.Principal) string {
	for _, r := range p.Roles {
		if strings.HasPrefix(r, "user:") {
			return strings.TrimPrefix(r, "user:")
		}
	}
	if p.KeyID != "" {
		return "key:" + p.KeyID
	}
	return ""
}

// validateConflictColumns 校验 Upsert 的 conflictColumns 精确命中集合的一个
// unique 索引（属性集相等，无序比较）。coll 为 nil（不可达防御）时放行，
// 由 PG 42P10 兜底。
func validateConflictColumns(coll *databases.Collection, conflictColumns []string) error {
	if coll == nil {
		return nil
	}
	sorted := append([]string(nil), conflictColumns...)
	sort.Strings(sorted)
	for _, idx := range coll.Indexes {
		if !strings.EqualFold(idx.Type, "unique") {
			continue
		}
		idxAttrs := append([]string(nil), idx.Attributes...)
		sort.Strings(idxAttrs)
		if slices.Equal(sorted, idxAttrs) {
			return nil
		}
	}
	return status.Errorf(codes.InvalidArgument,
		"conflict columns %v must match a unique index on collection %q", conflictColumns, coll.ID)
}

func scanDocumentJSON(scanner interface{ Scan(dest ...any) error }) (*databases.Document, error) {
	var raw []byte
	if err := scanner.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return parseDocumentJSON(raw)
}

// parseDocumentJSON 解析 to_jsonb(d.*) 行载荷：GetDocument 点查与 Bulk
// UPDATE ... RETURNING 写后快照共用同一扫描语义（R5-P2-6）。
func parseDocumentJSON(raw []byte) (*databases.Document, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	doc := &databases.Document{Data: make(map[string]any)}
	if v, ok := payload["_id"].(string); ok {
		doc.ID = v
	}
	if v, ok := payload["_tenant"].(float64); ok {
		doc.Tenant = int64(v)
	}
	if v, ok := payload["_created_at"].(string); ok {
		doc.CreatedAt, _ = time.Parse(time.RFC3339Nano, v)
	}
	if v, ok := payload["_updated_at"].(string); ok {
		doc.UpdatedAt, _ = time.Parse(time.RFC3339Nano, v)
	}
	if v, ok := payload["_created_by"].(string); ok {
		doc.CreatedBy = v
	}
	if v, ok := payload["_updated_by"].(string); ok {
		doc.UpdatedBy = v
	}
	// _version：用户集合有列时读取；存量表尚未 reconcile（缺列）视为 1，不当硬错
	// （读路径禁止 DDL；与成功补列后的 DEFAULT 1 回填语义一致）。
	if v, ok := payload["_version"].(float64); ok {
		doc.Version = int64(v)
	} else {
		doc.Version = 1
	}
	// _acl：to_jsonb(d.*) 把 text[] 投影为 JSON 字符串数组，顺带解析为
	// doc.Permissions（阶段③包 A 权限回填免费化——List/Get 零额外查询）。
	if rawACL, ok := payload["_acl"].([]any); ok {
		items := make([]string, 0, len(rawACL))
		for _, item := range rawACL {
			if s, isStr := item.(string); isStr {
				items = append(items, s)
			}
		}
		doc.Permissions = parseACLStrings(items)
	}
	for k, v := range payload {
		if strings.HasPrefix(k, "_") {
			continue
		}
		doc.Data[k] = v
	}
	return doc, nil
}

func isUniqueViolation(err error) bool {
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		return pgErr.Field('C') == "23505"
	}
	return strings.Contains(err.Error(), "SQLSTATE 23505") || strings.Contains(err.Error(), "unique constraint")
}

func validateDocID(docID string) error {
	if docID == "" {
		return status.Error(codes.InvalidArgument, "document id is required")
	}
	if !docIDRe.MatchString(docID) {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("invalid document id: %s", docID))
	}
	return nil
}
