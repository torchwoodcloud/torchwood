// 在线索引通道（转出 POC 门禁 B3，redesign §2-C5 / §4.4 / §6 会话 #10 偏差②）：
// 存量表的索引 DDL 走 catalog 两阶段状态机（building→active|failed）+ 事务外
// CREATE INDEX CONCURRENTLY（独立连接独立事务——CIC 不能在事务块内运行，这是
// 两阶段状态机存在的结构性原因，而非可选优化）+ `lock_timeout` 短超时重试。
//
// 分界（预决策 1）：建集合时的既有索引（含默认时间索引/_acl GIN）维持事务内
// （createCollectionIndex，新表无并发读者，CONCURRENTLY 无意义）；仅存量表的
// DDL touch / repair / reconcile 路径走 CONCURRENTLY。
//
// 状态机：
//
//	事务 A（短）：catalog 写入索引条目 status=building + ddl_seq CAS
//	事务外    ：CREATE INDEX CONCURRENTLY IF NOT EXISTS（tw_owner 会话身份，
//	            lock_timeout 2s 重试 ≤3；失败残留的 INVALID 索引随即清理）
//	事务 B（短）：成功 → status=active；失败 → status=failed（可重入）
//
// 中断恢复：进程崩溃留 building 行 → 后台 reconcile（schema_reconcile.go）扫
// 超时 building 行，按 pg_index 有效性分流（valid 补 active；INVALID/缺失
// DROP 后重置 building 重入）；同 ID 重复 CreateIndex 同样可重入（reentry）。
package documentdb

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

const (
	// onlineIndexLockTimeout 是 CIC 等待表锁的单次上限（C5：lock_timeout=2s）。
	// CIC 只取 ShareUpdateExclusiveLock（与读写 DML 不冲突），超时通常意味着
	// 另一个 DDL/维护操作长期持锁——退避重试而非无限等待。
	onlineIndexLockTimeout = 2 * time.Second
	// onlineIndexMaxAttempts 是 CIC 的总尝试上限（含首次）。
	onlineIndexMaxAttempts = 3
)

// errOnlineIndexRetryable 判定 CIC 失败是否值得重试：55P03 lock_not_available
// （lock_timeout 触发）与 40P01 deadlock_detected——均为瞬态锁竞争；其余失败
// （唯一冲突、语法、列缺失等）重试无意义。
func errOnlineIndexRetryable(err error) bool {
	var fielder pgErrorFielder
	if !errors.As(err, &fielder) {
		return false
	}
	switch fielder.Field('C') {
	case "55P03", "40P01":
		return true
	}
	return false
}

// buildIndexStatement 生成索引 DDL（CONCURRENTLY 通道与事务内通道共用同一
// 表达式构建，防两通道形态漂移）。concurrently=true 时插入 CONCURRENTLY
// 关键字（仅限事务外独立执行）。形态约束判定（vector/数组/hnsw/fulltext）
// 与 createCollectionIndex 既有语义逐字同源。
func buildIndexStatement(schema, physical string, idx databases.Index, attrs []databases.Attribute, concurrently bool) (string, error) {
	vectorAttrs := map[string]int{}
	arrayAttrs := map[string]bool{}
	deprecatedAttrs := map[string]bool{}
	for _, a := range attrs {
		if strings.ToLower(a.Type) == "vector" {
			vectorAttrs[a.Key] = a.Dims
		}
		if a.Array {
			arrayAttrs[a.Key] = true
		}
		if a.StatusOrDefault() == databases.AttrStatusDeprecated {
			deprecatedAttrs[a.Key] = true
		}
	}
	hasArrayCol := false
	hasVectorCol := false
	for _, attr := range idx.Attributes {
		if arrayAttrs[attr] {
			hasArrayCol = true
		}
		if _, ok := vectorAttrs[attr]; ok {
			hasVectorCol = true
		}
		if deprecatedAttrs[attr] {
			// B4：deprecated 属性不可作为索引目标（读屏蔽后索引永不命中）。
			return "", status.Errorf(codes.InvalidArgument,
				"attribute %q is deprecated and cannot be indexed", attr)
		}
	}
	if hasVectorCol || strings.ToLower(idx.Type) == "hnsw" {
		if strings.ToLower(idx.Type) != "hnsw" {
			return "", status.Error(codes.InvalidArgument,
				fmt.Sprintf("%s indexes do not support vector attributes", idx.Type))
		}
		if !hasVectorCol {
			return "", status.Error(codes.InvalidArgument,
				"hnsw indexes require a vector attribute")
		}
		if hasArrayCol {
			return "", status.Error(codes.InvalidArgument, "hnsw indexes do not support array attributes")
		}
	}
	if hasArrayCol {
		switch strings.ToLower(idx.Type) {
		case "unique":
			return "", status.Error(codes.InvalidArgument, "unique indexes do not support array attributes")
		case "fulltext":
			return "", status.Error(codes.InvalidArgument, "fulltext indexes do not support array attributes")
		}
		if len(idx.Attributes) != 1 {
			return "", status.Error(codes.InvalidArgument, "array attributes support single-column indexes only")
		}
	}
	var plainCols, orderedCols []string
	for i, attr := range idx.Attributes {
		if !safeNameRe.MatchString(attr) {
			return "", fmt.Errorf("invalid index attribute: %s", attr)
		}
		quoted := quoteIdent(attr)
		plainCols = append(plainCols, quoted)
		order := ""
		if i < len(idx.Orders) && strings.EqualFold(idx.Orders[i], "desc") {
			order = " DESC"
		}
		orderedCols = append(orderedCols, quoted+order)
	}
	idxName := quoteIdent(fmt.Sprintf("idx_%s_%s", physical, idx.ID))
	conc := ""
	if concurrently {
		conc = " CONCURRENTLY"
	}
	switch strings.ToLower(idx.Type) {
	case "unique":
		return fmt.Sprintf(`CREATE UNIQUE INDEX%s IF NOT EXISTS %s ON %s (%s)`, conc, idxName, tableName(schema, physical), strings.Join(orderedCols, ", ")), nil
	case "fulltext":
		// W-E：表达式与查询编译（to_tsvector('simple', "col"::text)）逐字对齐。
		if len(plainCols) == 1 {
			return fmt.Sprintf(`CREATE INDEX%s IF NOT EXISTS %s ON %s USING gin(to_tsvector('simple', %s::text))`, conc, idxName, tableName(schema, physical), plainCols[0]), nil
		}
		return fmt.Sprintf(`CREATE INDEX%s IF NOT EXISTS %s ON %s USING gin(to_tsvector('simple', %s))`, conc, idxName, tableName(schema, physical), strings.Join(plainCols, " || ' ' || ")), nil
	case "hnsw":
		opClass, err := hnswOpClass(idx.DistanceMetric)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`CREATE INDEX%s IF NOT EXISTS %s ON %s USING hnsw (%s %s)`, conc, idxName, tableName(schema, physical), plainCols[0], opClass), nil
	default:
		if hasArrayCol {
			return fmt.Sprintf(`CREATE INDEX%s IF NOT EXISTS %s ON %s USING gin (%s array_ops)`, conc, idxName, tableName(schema, physical), plainCols[0]), nil
		}
		return fmt.Sprintf(`CREATE INDEX%s IF NOT EXISTS %s ON %s (%s)`, conc, idxName, tableName(schema, physical), strings.Join(orderedCols, ", ")), nil
	}
}

// physicalIndexName 拼出物理索引名（不带引号；供 catalog/pg_index 查找比对）。
func physicalIndexName(physical, indexID string) string {
	return fmt.Sprintf("idx_%s_%s", physical, indexID)
}

// dropIndexStatement 生成 DROP INDEX IF EXISTS 语句（reconcile/repair 复用）。
func dropIndexStatement(schema, physicalIndexName string) string {
	return fmt.Sprintf(`DROP INDEX IF EXISTS %s.%s`, quoteIdent(schema), quoteIdent(physicalIndexName))
}

// createIndexConcurrently 在**事务外独立连接**上执行 CREATE INDEX CONCURRENTLY
// （CIC 不能在事务块内运行）。连接身份模型（redesign §3.2）：单一变色龙
// authenticator + 会话级 SET ROLE tw_owner（CIC 无事务可挂 SET LOCAL ROLE；
// tw_owner 是表 owner，CIC 需要其所有权）+ 会话级 lock_timeout；用毕 RESET
// ROLE 归还连接，RESET 失败则经 driver.ErrBadConn 把连接从池中剔除（绝不让
// 带着高权限角色的会话回池复用）。
func (p *postgresDocumentDB) createIndexConcurrently(ctx context.Context, schema, physical string, idx databases.Index, attrs []databases.Attribute) error {
	stmt, err := buildIndexStatement(schema, physical, idx, attrs, true)
	if err != nil {
		return err
	}
	idxName := physicalIndexName(physical, idx.ID)
	dropStmt := dropIndexStatement(schema, idxName)

	sqlDB := p.db.DB.DB // bun.DB 内嵌 *sql.DB
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for concurrent index build: %w", err)
	}
	resetDone := false
	defer func() {
		if resetDone {
			_ = conn.Close()
			return
		}
		// RESET 失败/未执行（错误/panic 路径）：连接可能带着 tw_owner 会话
		// 角色，经 ErrBadConn 强制出池销毁，绝不回池复用。
		_ = conn.Raw(func(driverConn any) error { return driver.ErrBadConn })
	}()
	// SET ROLE + lock_timeout 会话级生效（单批次单往返，pgdriver simple protocol）。
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf(`SET ROLE %s; SET lock_timeout TO '%s'`, clients.RoleOwner, onlineIndexLockTimeout.String()),
	); err != nil {
		return fmt.Errorf("set role for concurrent index build: %w", err)
	}
	defer func() {
		// 与 ctx 解耦：父 ctx 取消时也要把角色复位，保住归还连接的洁净。
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), `RESET ROLE`); err == nil {
			resetDone = true
		}
	}()

	var lastErr error
	for attempt := 1; attempt <= onlineIndexMaxAttempts; attempt++ {
		if lastErr != nil {
			// 上次失败的 CIC 会残留 INVALID 索引（PG 语义：CIC 失败不自动
			// 清除），重试前必须清掉，否则 IF NOT EXISTS 命中残留名直接跳过、
			// 换汤不换药地再报错。
			if _, err := conn.ExecContext(ctx, dropStmt); err != nil {
				return fmt.Errorf("drop stale invalid index %s before retry: %w", idxName, err)
			}
		}
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			lastErr = err
			if errOnlineIndexRetryable(err) && attempt < onlineIndexMaxAttempts {
				slog.Warn("concurrent index build retry",
					"schema", schema, "table", physical, "index", idxName,
					"attempt", attempt, "error", err)
				// 有界退避：锁竞争方通常是短 DDL，固定短等待即可。
				select {
				case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
				continue
			}
			return fmt.Errorf("create index concurrently %s: %w", idxName, err)
		}
		return nil
	}
	return lastErr
}

// indexBeginBuilding 是事务 A（短）：catalog 写入索引条目 status=building +
// ddl_seq CAS。**纯 catalog DML**——不触碰物理表（非并发 CREATE INDEX 一律
// 取 SHARE 锁，IF NOT EXISTS 的存在性检查在开锁之后，任何锁型 DDL 都会阻塞
// 并发读写，与在线通道的目的相悖）；存量表的默认索引/RLS/列授权自愈由
// schema_reconcile.go（CONCURRENTLY 通道）承担，CreateIndex 不再是 DDL touch
// 汇聚点（B3 语义收窄）。返回值 reentry 报告既有同 ID 条目被重入（building/
// failed 残留——中断恢复路径），调用方据此在事务外先清理残留 INVALID 索引
// （createIndexConcurrently 的重试前清理已覆盖）。
func (p *postgresDocumentDB) indexBeginBuilding(ctx context.Context, projectID, databaseID, collectionID string, idx databases.Index) (reentry bool, err error) {
	txErr := p.withOwnerTx(ctx, func(txCtx context.Context) error {
		row, err := p.loadCollectionRow(txCtx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		idxs, err := decodeIndexes(row.Indexes)
		if err != nil {
			return err
		}
		reentry = false
		kept := make([]databases.Index, 0, len(idxs)+1)
		for _, i := range idxs {
			if i.ID != idx.ID {
				kept = append(kept, i)
				continue
			}
			switch i.StatusOrDefault() {
			case databases.IndexStatusActive:
				return fmt.Errorf("%w: index %q already exists", ErrDuplicateKey, idx.ID)
			default:
				// building/failed 残留：重入（判据原文"building 残留可重入"）。
				reentry = true
			}
		}
		idx.Status = databases.IndexStatusBuilding
		kept = append(kept, idx)
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
	})
	if txErr != nil {
		return false, txErr
	}
	return reentry, nil
}

// indexSetStatus 是事务 B（短）：把索引条目置为终态（active/failed）。
// 条目缺失（并发 DeleteIndex 先行）返回 (false, nil)——调用方跳过。
func (p *postgresDocumentDB) indexSetStatus(ctx context.Context, projectID, databaseID, collectionID, indexID, status string) (bool, error) {
	found := false
	txErr := p.withOwnerTx(ctx, func(txCtx context.Context) error {
		row, err := p.loadCollectionRow(txCtx, projectID, databaseID, collectionID)
		if err != nil {
			return err
		}
		idxs, err := decodeIndexes(row.Indexes)
		if err != nil {
			return err
		}
		changed := false
		for i := range idxs {
			if idxs[i].ID != indexID {
				continue
			}
			idxs[i].Status = status
			changed = true
			found = true
		}
		if !changed {
			return nil
		}
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
	})
	if txErr != nil {
		return false, txErr
	}
	return found, nil
}
