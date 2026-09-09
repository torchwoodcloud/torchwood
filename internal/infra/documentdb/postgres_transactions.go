// 事务内核执行器（redesign §4.8 Phase 1 / §11-E1）：异构 op 列表在单个
// RunInTx 内顺序执行——Bulk 的泛化，无暂存表、无会话。
//
//   - 锁纪律（E1）：按 (collection, documentID) 排序对批内全部 op 目标预取
//     pg_advisory_xact_lock，防并发批以不同顺序锁多行互锁；op 仍按请求序
//     执行（批内事件顺序 = op 序），语句级行锁自然与单文档路径串行化。
//   - ATOMIC（默认）：任一 op 失败整批回滚，返回带 op index 的错误。
//   - PARTIAL：逐 op SAVEPOINT 容错（PG 语句失败会 abort 事务，必须回滚到
//     savepoint 才能继续），已成功不回滚，返回 per-op 结果。
package documentdb

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

func (p *postgresDocumentDB) ExecuteTransactions(
	ctx context.Context, projectID, databaseID string,
	ops []databases.TransactionOp, mode databases.TransactionMode,
	principal databases.Principal,
) ([]databases.TransactionOpResult, error) {
	if len(ops) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ops is required")
	}
	if len(ops) > databases.MaxTransactionOps {
		return nil, status.Errorf(codes.InvalidArgument, "ops count %d exceeds maximum of %d", len(ops), databases.MaxTransactionOps)
	}
	if mode == "" {
		mode = databases.TransactionModeAtomic
	}
	switch mode {
	case databases.TransactionModeAtomic, databases.TransactionModePartial:
	default:
		return nil, status.Errorf(codes.InvalidArgument, "invalid transaction mode %q", mode)
	}

	var results []databases.TransactionOpResult
	// 批标识（阶段④ §4.8）：同批事件的 transaction_id 共享，顺序 = op 序；
	// 单文档写路径不置入（ctx 无值，Publish 读到空串）。
	txCtx0 := domainevents.WithTransactionID(ctx, idgen.ULID().String())
	// E1：RLS/GUC 一次注入（批首身份 = 请求 principal）、逐 op 判定。
	err := p.withDocumentTx(txCtx0, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		// 批间死锁防护：对批内全部 op 目标（排序后）预取事务级 advisory 锁。
		if err := p.lockTxTargets(txCtx, projectID, databaseID, ops); err != nil {
			return err
		}
		results = results[:0]
		for i, op := range ops {
			// PARTIAL：op 级 savepoint 容错；ATOMIC：失败即中止整批（RunInTx 回滚）。
			sp := "tx_op_" + strconv.Itoa(i)
			if mode == databases.TransactionModePartial {
				if _, err := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(`SAVEPOINT %s`, sp)); err != nil {
					return err
				}
			}
			docID, version, err := p.executeTxOp(txCtx, projectID, databaseID, op, principal)
			if err != nil {
				if mode == databases.TransactionModeAtomic {
					return &databases.OpError{Index: i, Err: err}
				}
				if _, rbErr := p.conn(txCtx).ExecContext(txCtx, fmt.Sprintf(`ROLLBACK TO SAVEPOINT %s`, sp)); rbErr != nil {
					return rbErr
				}
				results = append(results, databases.TransactionOpResult{
					Index:      i,
					OK:         false,
					ErrCode:    databases.ErrorDomainCode(p.mapError(err)),
					ErrMessage: err.Error(),
				})
				continue
			}
			results = append(results, databases.TransactionOpResult{
				Index:      i,
				OK:         true,
				DocumentID: docID,
				Version:    version,
			})
		}
		return nil
	})
	if err != nil {
		return nil, p.mapError(err)
	}
	return results, nil
}

// executeTxOp 在事务上下文内执行单个 op，返回（目标/实际文档 ID, 写后版本）。
// 复用单文档路径的事务体（它们感知 clients.InTx 并跳过自身 RunInTx），
// 权限判定、OCC、conflictColumns 前置校验与事件发布语义与单文档 API 完全同源。
func (p *postgresDocumentDB) executeTxOp(
	ctx context.Context, projectID, databaseID string,
	op databases.TransactionOp, principal databases.Principal,
) (string, int64, error) {
	switch op.Type {
	case databases.TransactionOpCreate:
		doc, err := p.createDocument(ctx, projectID, databaseID, op.CollectionID, databases.Document{
			ID: op.DocumentID, Data: op.Data,
		}, op.Permissions, principal)
		if err != nil {
			return "", 0, err
		}
		return doc.ID, doc.Version, nil

	case databases.TransactionOpUpdate:
		// Phase 1 裁决②：显式 ≤0 → InvalidArgument（version_invalid）；
		// 缺省（nil）→ 盲写 +1（LWW 契约，复用 SkipVersion 机制）。
		if op.ExpectedVersion != nil && *op.ExpectedVersion <= 0 {
			return "", 0, databases.ErrVersionInvalid
		}
		update := databases.DocumentUpdate{
			Document:     databases.Document{ID: op.DocumentID, Data: op.Data},
			Permissions:  op.Permissions,
			Increment:    op.Increment,
			ArrayUpdates: op.ArrayUpdates, // 转出 POC B1：execute-tx op 数组原子更新（buildArrayParts 同源）
		}
		if op.ExpectedVersion != nil {
			update.ExpectedVersion = *op.ExpectedVersion
		} else {
			update.SkipVersion = true
		}
		doc, err := p.updateDocument(ctx, projectID, databaseID, op.CollectionID, update, principal)
		if err != nil {
			return "", 0, err
		}
		return doc.ID, doc.Version, nil

	case databases.TransactionOpUpsert:
		if len(op.ConflictColumns) == 0 {
			return "", 0, status.Error(codes.InvalidArgument, "conflict columns are required")
		}
		// 与 UpsertDocument 入口一致的列校验（非法列精确报错，不静默丢弃）。
		conflictCols := make([]string, 0, len(op.ConflictColumns))
		for _, col := range op.ConflictColumns {
			if !safeNameRe.MatchString(col) || strings.HasPrefix(col, "_") {
				return "", 0, status.Errorf(codes.InvalidArgument, "invalid conflict column: %s", col)
			}
			conflictCols = append(conflictCols, quoteIdent(col))
		}
		doc, err := p.upsertDocument(ctx, projectID, databaseID, op.CollectionID, databases.Document{
			ID: op.DocumentID, Data: op.Data,
		}, conflictCols, op.ConflictColumns, op.Permissions, principal)
		if err != nil {
			return "", 0, err
		}
		return doc.ID, doc.Version, nil

	case databases.TransactionOpDelete:
		// delete 不支持盲删：缺省/≤0 均 version 类错误；显式 ≤0 按 裁决②
		// 为 version_invalid（缺省为 version_required，与单文档 API 一致）。
		if op.ExpectedVersion != nil && *op.ExpectedVersion <= 0 {
			return "", 0, databases.ErrVersionInvalid
		}
		var version int64
		if op.ExpectedVersion != nil {
			version = *op.ExpectedVersion
		}
		if err := p.deleteDocument(ctx, projectID, databaseID, op.CollectionID, op.DocumentID, databases.DeleteOptions{
			ExpectedVersion: version,
		}, principal); err != nil {
			return "", 0, err
		}
		return op.DocumentID, 0, nil

	default:
		return "", 0, status.Errorf(codes.InvalidArgument, "invalid transaction op type %q", op.Type)
	}
}

// lockTxTargets 对批内全部 op 目标按 (collection, documentID) 排序预取事务级
// advisory 锁：并发批以相同顺序拿锁，消除"批 A 锁 d1 等 d2、批 B 锁 d2 等 d1"
// 的批间互锁；单文档路径不受影响（语句级行锁仍与 op 串行化）。
//
// upsert 冲突值键预锁（转出 POC B13a，§4.8 裁决④挂账收口）：upsertDocument
// 执行时对 (_tenant, 冲突值) 取第二族 advisory 锁（P0-1 串行化），该族锁按 op
// 请求序获取——并发批以相反冲突键顺序执行时成环（死锁注入实测 8/8 轮 40P01）。
// 修复 = 批首把全部 upsert op 的冲突值键**排序后预锁**：两族锁各自全局有序，
// 且每批先取完 docID 族再取冲突键族，跨族不成环；upsertDocument 内的取锁在
// 同事务内可重入（即时返回），P0-1 语义不变。冲突列非法/缺值的 op 跳过预锁
// ——其执行体必然以 InvalidArgument 失败，无须互斥。
func (p *postgresDocumentDB) lockTxTargets(ctx context.Context, projectID, databaseID string, ops []databases.TransactionOp) error {
	internalID, err := p.resolveInternalID(ctx, projectID)
	if err != nil {
		return err
	}
	tenant := strconv.FormatInt(internalID, 10)
	type target struct{ collection, document string }
	targets := make([]target, 0, len(ops))
	conflictKeys := make([]string, 0, len(ops))
	seen := make(map[target]struct{}, len(ops))
	seenConflict := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		t := target{op.CollectionID, op.DocumentID}
		// upsert 的目标是冲突行（预查前未知），锁键取 collection+conflict 语义
		// 近似：与 upsertDocument 自身的 advisory lock 键（冲突值）不同键不互
		// 斥——由冲突值键族（下）与 op 执行时的行锁兜底，此处只需覆盖确定 ID
		// 的 op。
		if t.document == "" || t.collection == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		targets = append(targets, t)
		if op.Type == databases.TransactionOpUpsert && len(op.ConflictColumns) > 0 {
			values, err := conflictArgs(op.ConflictColumns, op.Data)
			if err != nil {
				continue // 非法冲突列/缺值：执行体必然 InvalidArgument，无须预锁
			}
			key := conflictLockKey(values)
			if _, dup := seenConflict[key]; dup {
				continue
			}
			seenConflict[key] = struct{}{}
			conflictKeys = append(conflictKeys, key)
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].collection != targets[j].collection {
			return targets[i].collection < targets[j].collection
		}
		return targets[i].document < targets[j].document
	})
	// 键族顺序：先 docID 族（已排序），后冲突值键族（排序后取锁）——两族各自
	// 全局有序 + 每批族间顺序一致 = 并发批拿锁全序兼容，无 hold-and-wait 环。
	sort.Strings(conflictKeys)
	for _, t := range targets {
		if _, err := p.conn(ctx).ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtext(?), hashtext(?))`,
			tenant, t.collection+"."+t.document); err != nil {
			return fmt.Errorf("acquire tx target lock: %w", err)
		}
	}
	for _, key := range conflictKeys {
		if _, err := p.conn(ctx).ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtext(?), hashtext(?))`,
			tenant, key); err != nil {
			return fmt.Errorf("acquire upsert conflict key lock: %w", err)
		}
	}
	return nil
}
