// ChangeFeed 实现（阶段④ §4.5）：从 document_events_outbox 读某集合的
// 已提交事件（outbox 行全部已 COMMIT——行存在即可读，与 published_at
// 无关），按请求者可见性过滤（快照 ACL + 当前 principal，与 hub 扇出
// 同语义，复用 events.VisibleTo），seq 升序返回。
package documentdb

import (
	"context"
	"fmt"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
)

const (
	// maxChangesLimit 是单次返回条数上限（:changes 契约）。
	maxChangesLimit = 500
	// defaultChangesLimit 是缺省条数。
	defaultChangesLimit = 500
	// changesScanBatch 是底层扫描页大小（可见性过滤在应用层做——不可见
	// 行不占返回额度，需翻页继续扫描）。
	changesScanBatch = 500
)

// maxChangesScanRows 是单次调用扫描行数硬上限（极端私有集合下防无界
// 扫描）。R15：上限退出改返回扫描游标 + has_more=true（越过已判不可见
// 的块续传），不再静默截断。var 而非 const：测试覆写缩短验证块场景
// （对齐 idempotencyWaitBudget 先例）。
var maxChangesScanRows = 50000

// ListChanges 返回 collection 中 seq > SinceSeq 的已提交事件（可见性过滤
// 后，seq 升序）。hasMore = 仍有更多可见事件或扫描触上限（上限后可能
// 仍有可见事件）；nextSinceSeq 是续传游标（R15 两级语义）：
//   - (a) 收满 limit+1 退出（可见事件充足）：= 最后一条*返回*的可见事件
//     seq——续传首条恰为第 limit+1 条，无重无漏；
//   - (b) 扫描上限退出：= 内部扫描位置（越过已判不可见的块）——续传
//     从块后继续，不再重扫；
//   - 自然耗尽：hasMore=false、nextSinceSeq=0。
//
// SinceSeq > 0 且早于该集合最老可用事件 → ErrResumeExpired。
//
// B-1（终局安全评审）：topic/频道 = databases.<db>.collections.<coll> 不含
// project 维度，跨项目同名集合共享同一全局 topic——扫描/游标判定必须强制
// outbox.project_id 等值过滤（has_more/游标推进在过滤后的项目行集上进行，
// 不可见行跳过语义与 R15 两级游标不受影响）。
func (p *postgresDocumentDB) ListChanges(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	opts databases.ListChangesOptions,
	principal databases.Principal,
) ([]databases.DocumentChange, bool, int64, error) {
	if opts.Limit <= 0 {
		opts.Limit = defaultChangesLimit
	}
	if opts.Limit > maxChangesLimit {
		opts.Limit = maxChangesLimit
	}
	if opts.SinceSeq < 0 {
		opts.SinceSeq = 0
	}
	topic := fmt.Sprintf("databases.%s.collections.%s", databaseID, collectionID)

	if opts.SinceSeq > 0 {
		if err := p.checkChangesCursor(ctx, projectID, topic, opts.SinceSeq); err != nil {
			return nil, false, 0, err
		}
	}

	// 扫描多取一条可见事件：collect limit+1，截断到 limit——has_more 由
	// 第 limit+1 条是否存在决定（退出 (a)）。
	out := make([]databases.DocumentChange, 0, opts.Limit+1)
	lastSeq := opts.SinceSeq
	scanned := 0
	scanCapped := false
	for len(out) <= opts.Limit {
		if scanned >= maxChangesScanRows {
			// 上限退出 (b)：循环顶检查——上一批必然是满批（不满批已自然
			// 耗尽退出），故此后可能仍有行。
			scanCapped = true
			break
		}
		rows, err := p.scanChanges(ctx, projectID, topic, lastSeq, opts.DocumentID, changesScanBatch)
		if err != nil {
			return nil, false, 0, err
		}
		if len(rows) == 0 {
			break
		}
		scanned += len(rows)
		for i := range rows {
			lastSeq = rows[i].Seq
			ev, err := infraevents.UnmarshalEnvelope(rows[i].Payload)
			if err != nil {
				// 坏载荷不可见地跳过（outbox 行存在但信封损坏——计数为
				// 已消费游标，不阻塞续传）。
				continue
			}
			if !domainevents.VisibleTo(ev.ACL, principal) {
				continue
			}
			out = append(out, databases.DocumentChange{
				Seq:           rows[i].Seq,
				EventID:       ev.EventID,
				Event:         ev.Event,
				DocumentID:    ev.DocumentID,
				Version:       ev.Version,
				TransactionID: ev.TransactionID,
				CreatedAt:     ev.CreatedAt,
				Truncated:     ev.Truncated,
				Data:          ev.Data,
			})
			if len(out) > opts.Limit {
				break
			}
		}
		if len(rows) < changesScanBatch {
			break // 自然耗尽
		}
	}
	switch {
	case len(out) > opts.Limit:
		// 退出 (a)：游标 = 末条返回 seq（内部扫描位置可能已越过第
		// limit+1 条可见事件，用它续转会漏掉该事件）。
		out = out[:opts.Limit]
		return out, true, out[len(out)-1].Seq, nil
	case scanCapped:
		// 退出 (b)：has_more=true（上限后可能仍有可见事件——静默 false
		// 即 R15 漏失根源）；游标 = 扫描位置（越过已判不可见的块）。
		return out, true, lastSeq, nil
	default:
		return out, false, 0, nil
	}
}

// checkChangesCursor 判定续传游标是否仍在可用窗口内：SinceSeq 早于该
// 集合最老可用事件（两者之间可能存在已被清理的行）→ ErrResumeExpired。
// 集合无任何行时不判过期（返回空集是合法的「已追平」）。
// B-1：MIN(seq) 在 project 过滤后的行集上判定——他项目同名集合的更老行
// 不得造成误判过期（也不得掩盖本项目的真实过期）。
func (p *postgresDocumentDB) checkChangesCursor(ctx context.Context, projectID, topic string, sinceSeq int64) error {
	var oldest *int64
	if err := p.conn(ctx).NewSelect().TableExpr("document_events_outbox").
		ColumnExpr("MIN(seq)").
		Where("project_id = ?", projectID).
		Where("topic = ?", topic).
		Scan(ctx, &oldest); err != nil {
		return err
	}
	if oldest != nil && sinceSeq < *oldest {
		return databases.ErrResumeExpired
	}
	return nil
}

// scanChanges 读一页原始行（不做可见性过滤），seq 升序。
// B-1：project_id 等值过滤——topic 为全局命名空间（不含 project 维度），
// 扫描面必须锁定请求者项目（隔离由消费端 project 过滤保证）。
func (p *postgresDocumentDB) scanChanges(ctx context.Context, projectID, topic string, sinceSeq int64, documentID string, limit int) ([]outboxScanRow, error) {
	q := p.conn(ctx).NewSelect().TableExpr("document_events_outbox").
		Column("seq", "payload").
		Where("project_id = ?", projectID).
		Where("topic = ?", topic).
		Where("seq > ?", sinceSeq).
		Order("seq ASC").
		Limit(limit)
	if documentID != "" {
		q = q.Where("payload->>'document_id' = ?", documentID)
	}
	var rows []outboxScanRow
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

type outboxScanRow struct {
	Seq     int64  `bun:"seq"`
	Payload []byte `bun:"payload"`
}

var _ databases.ChangeFeed = (*postgresDocumentDB)(nil)
