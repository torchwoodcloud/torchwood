// 停机补投的 outbox 区间扫描（functions v3 切片 D，docs/design/functions-v3.md
// §4.2/D12 对抗审查升格：XTRIM 不理会消费组进度，worker 停机超过 Stream 裁剪
// 窗口后恢复，functions-triggers 消费组会静默跳过被裁掉的条目——从 outbox 表
// （重放真源）按 seq 区间补投，复用 :changes?since_seq= 的「行存在即可读」
// 语义。查询实现参照 postgres_changes.go scanChanges，但跨集合跨项目：
// B-1 隔离锚点在消费端按信封 ProjectID 匹配（订阅是项目内的，天然收窄）。
package events

import (
	"context"

	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

// ScanOutboxSeqRange 返回 document_events_outbox 中 seq ∈ (after, upto] 的
// 行（seq 升序，批 ≤ limit）。补投专用：
//   - 不做可见性过滤（消费端是函数触发器，投递权限在执行期由 execution
//     principal + RLS 链路把关——与正常路径同语义，见 v3 Security #4）；
//   - 不含 published_at 条件（行存在 = 已 COMMIT，:changes 同语义）；
//   - 经济事件行也在区间内（seq 是全局分配序），由消费端 IsEconomy 跳过
//     匹配但照常推进水位（否则 gap 判定会误报）。
//
// 返回空切片表示区间已耗尽（或行已被 24h 清理窗口裁掉——诚实边界：窗口外
// 的极端停机（>24h）才真正丢失，v3 §4.2）。
func ScanOutboxSeqRange(ctx context.Context, db *clients.Database, after, upto int64, limit int) ([]model.DocumentEventsOutbox, error) {
	rows := make([]model.DocumentEventsOutbox, 0, limit)
	err := db.Conn(ctx).NewSelect().Model(&rows).
		Column("event_id", "seq", "project_id", "payload", "channel", "created_at", "attempts").
		Where("seq > ?", after).
		Where("seq <= ?", upto).
		Order("seq ASC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
