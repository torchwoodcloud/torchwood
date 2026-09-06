// Package events 提供用户集合文档写事件的 transactional outbox 适配器
// （v2 设计 §3）：把信封（含服务端 acl 快照）INSERT 进
// public.document_events_outbox，与文档写同一段事务。
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	domainevents "github.com/torchwooddev/torchwood/internal/domain/events"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/pkg/idgen"
)

// maxEnvelopeBytes 是信封 JSON 的载荷上限（1 MiB，阶段④对齐 H1 文档写入
// 上限——写入面已拒超限载荷，事件面正常路径不再截断；超限仅防御性截断
// + truncated=true，业务写不回滚）。单属性 256 KiB 上限（app 层校验）
// 保证 1 MiB 信封只会因极端 _acl 列表触发防御路径（H2 _acl ≤64 ACE 落地
// 前的兜底）。
const maxEnvelopeBytes = 1 << 20

// outboxNotifyChannel 是 outbox INSERT 唤醒信号频道（阶段④ §4.5）：
// 空载荷纯信号——NOTIFY 在 commit 时才投递，与「事件已可领」天然对齐；
// PG 对同事务内多次相同 (channel, payload) 的 NOTIFY 合并为一条，
// execute-tx 100 op 批只产生一次唤醒。
const outboxNotifyChannel = "tw_outbox"

type eventOutbox struct {
	db *clients.Database
}

// NewEventOutbox 构造 EventPublisher 的 outbox 实现。调用方应在 uow.Run
// 内 Publish，与业务写同一工作单元；实现仍从 ctx 读取连接（Conn(ctx)）：
// 已在工作单元内则复用外层事务（与文档行同 COMMIT），未在工作单元内则
// 自行短事务插入。
func NewEventOutbox(db *clients.Database) *eventOutbox {
	return &eventOutbox{db: db}
}

func (o *eventOutbox) Publish(ctx context.Context, ev domainevents.Envelope) error {
	if ev.EventID == "" {
		ev.EventID = idgen.ULID().String()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	if ev.TransactionID == "" {
		ev.TransactionID = domainevents.TransactionIDFrom(ctx)
	}
	payload, err := marshalEnvelope(ev)
	if err != nil {
		return err
	}
	insert := func(ctx context.Context) error {
		// B-1 隔离锚点：topic（= CollectionChannel()）是全局命名空间、不含
		// project 维度——跨项目同名集合共享同一 topic。行级隔离由本列的
		// project_id 保证，消费端（:changes 扫描 / Hub 扇出）必须按其等值
		// 过滤；详见 envelope.CollectionChannel 的锚点注释。
		topic := ev.CollectionChannel()
		var channel *string
		if ev.IsEconomy() {
			// 经济事件：topic 落事件名，channel 落显式扇出频道（D17）。
			topic = ev.Event
			ch := ev.Channel
			channel = &ch
		}
		// 列白名单：seq 为 GENERATED ALWAYS AS IDENTITY（000028），bun 无
		// identity 特判，不排除会显式插 0 被 PG 拒绝。
		if _, err := o.db.Conn(ctx).NewInsert().Model(&model.DocumentEventsOutbox{
			EventID:     ev.EventID,
			ProjectID:   ev.ProjectID,
			Topic:       topic,
			Channel:     channel,
			Payload:     payload,
			CreatedAt:   ev.CreatedAt,
			AvailableAt: time.Now(),
		}).Column("event_id", "project_id", "topic", "channel", "payload", "created_at", "available_at").Exec(ctx); err != nil {
			return err
		}
		// 唤醒信号由 000028 的 AFTER INSERT 触发器发出（同事务、随 commit
		// 投递、同事务多次自动合并）——应用侧零额外语句，Bulk 语句数预算
		//（R5-P2-6）不受事件路径影响。
		return nil
	}
	if clients.InTx(ctx) {
		return insert(ctx)
	}
	return o.db.RunInTx(ctx, func(txCtx context.Context) error {
		return insert(txCtx)
	})
}

// marshalEnvelope 序列化完整信封并按 1 MiB 上限做防御性截断：
//  1. 整体超限 → 去掉 data、标记 truncated（业务写不回滚）；
//  2. acl 仍超限（极端权限列表）→ 截断 acl 数组并记日志：先按平均条目
//     字节一次跳删到目标附近（逐条重 marshal 是 O(n²)，数万条权限下
//     分钟级不可用），残余量再逐条收敛。
//
// 经济事件（Domain 非空）载荷小且无 acl / data，直接序列化。
func marshalEnvelope(ev domainevents.Envelope) (json.RawMessage, error) {
	if ev.IsEconomy() {
		return json.Marshal(envelopePayloadMap(ev))
	}
	payload, err := json.Marshal(envelopePayloadMap(ev))
	if err != nil {
		return nil, err
	}
	if len(payload) <= maxEnvelopeBytes {
		return payload, nil
	}
	ev.Truncated = true
	ev.Data = nil
	payload, err = json.Marshal(envelopePayloadMap(ev))
	if err != nil {
		return nil, err
	}
	if total := len(ev.ACL.CollectionPermissions) + len(ev.ACL.DocumentPermissions); total > 0 && len(payload) > maxEnvelopeBytes {
		drop := (len(payload)-maxEnvelopeBytes)*total/len(payload) + 1
		for drop > 0 && (len(ev.ACL.CollectionPermissions) > 0 || len(ev.ACL.DocumentPermissions) > 0) {
			if len(ev.ACL.CollectionPermissions) > 0 {
				ev.ACL.CollectionPermissions = ev.ACL.CollectionPermissions[:len(ev.ACL.CollectionPermissions)-1]
			}
			if len(ev.ACL.DocumentPermissions) > 0 {
				ev.ACL.DocumentPermissions = ev.ACL.DocumentPermissions[:len(ev.ACL.DocumentPermissions)-1]
			}
			drop--
		}
		payload, err = json.Marshal(envelopePayloadMap(ev))
		if err != nil {
			return nil, err
		}
	}
	for len(payload) > maxEnvelopeBytes && (len(ev.ACL.CollectionPermissions) > 0 || len(ev.ACL.DocumentPermissions) > 0) {
		if len(ev.ACL.CollectionPermissions) > 0 {
			ev.ACL.CollectionPermissions = ev.ACL.CollectionPermissions[:len(ev.ACL.CollectionPermissions)-1]
		}
		if len(ev.ACL.DocumentPermissions) > 0 {
			ev.ACL.DocumentPermissions = ev.ACL.DocumentPermissions[:len(ev.ACL.DocumentPermissions)-1]
		}
		payload, err = json.Marshal(envelopePayloadMap(ev))
		if err != nil {
			return nil, err
		}
	}
	if len(payload) > maxEnvelopeBytes {
		slog.Warn("outbox envelope exceeds size limit after acl truncation",
			"event_id", ev.EventID, "event", ev.Event, "bytes", len(payload))
	}
	return payload, nil
}

// envelopePayloadMap 组装 outbox.payload：ClientPayload()（无 acl）之上
// 追加服务端投递过滤用的 acl 快照（经济事件无 acl，设计 §5.1）。
// 与 outbox.payload 同形的完整信封也是 Redis Stream 条目的载荷
// （含 acl，Hub 侧再剥；worker 领取行后回填 Seq 再序列化，seq 进条目）。
func envelopePayloadMap(ev domainevents.Envelope) map[string]any {
	m := ev.ClientPayload()
	if ev.IsEconomy() {
		return m
	}
	m["acl"] = map[string]any{
		"document_security":      ev.ACL.DocumentSecurity,
		"collection_permissions": permissionStrings(ev.ACL.CollectionPermissions),
		"document_permissions":   permissionStrings(ev.ACL.DocumentPermissions),
		"doc_has_perms":          ev.ACL.DocHasPerms,
	}
	return m
}

func permissionStrings(perms []databases.Permission) []string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, databases.FormatPermissionString(p))
	}
	return out
}

var _ shared.EventPublisher = (*eventOutbox)(nil)
