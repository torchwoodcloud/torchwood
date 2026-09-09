package bunrepo

import (
	"context"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
)

type outboxRepo struct {
	db *clients.Database
}

func NewOutboxRepository(db *clients.Database) events.OutboxRepository {
	return &outboxRepo{db: db}
}

func (r *outboxRepo) ListDeadLetters(ctx context.Context, projectID string, pageSize int32, pageToken string) ([]events.DeadLetter, int64, string, error) {
	params, err := crud.ParseListParams(pageSize, pageToken, "", "")
	if err != nil {
		return nil, 0, "", err
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var total int
	if total, err = r.db.Conn(ctx2).NewSelect().Model((*model.DocumentEventsOutboxDead)(nil)).Where("project_id = ?", projectID).Count(ctx2); err != nil {
		return nil, 0, "", err
	}
	var rows []model.DocumentEventsOutboxDead
	if err := r.db.Conn(ctx2).NewSelect().Model(&rows).Where("project_id = ?", projectID).Order("created_at DESC").Offset(params.Offset).Limit(int(params.PageSize)).Scan(ctx2); err != nil {
		return nil, 0, "", err
	}
	out := make([]events.DeadLetter, len(rows))
	for i, row := range rows {
		ch := ""
		if row.Channel != nil {
			ch = *row.Channel
		}
		out[i] = events.DeadLetter{
			EventID:   row.EventID,
			ProjectID: row.ProjectID,
			Topic:     row.Topic,
			Channel:   ch,
			Payload:   row.Payload,
			Attempts:  int32(row.Attempts),
			LastError: row.LastError,
			CreatedAt: row.CreatedAt,
		}
	}
	hasMore := params.Offset+len(rows) < total
	info := crud.BuildPaginationInfo(params, total, hasMore)
	var nextToken string
	if info.HasNext {
		token, err := crud.EncodePageToken(info.NextOffset)
		if err != nil {
			return nil, 0, "", err
		}
		nextToken = token
	}
	return out, int64(total), nextToken, nil
}

func (r *outboxRepo) ReplayDeadLetter(ctx context.Context, eventID, projectID string) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.db.RunInTx(ctx2, func(txCtx context.Context) error {
		var dead model.DocumentEventsOutboxDead
		if err := r.db.Conn(txCtx).NewSelect().Model(&dead).Where("event_id = ?", eventID).Where("project_id = ?", projectID).For("UPDATE").Scan(txCtx); err != nil {
			// 未在 dead 中找到：检查是否已在 outbox（幂等：之前已 replay）
			if cnt, err2 := r.db.Conn(txCtx).NewSelect().Model((*model.DocumentEventsOutbox)(nil)).Where("event_id = ?", eventID).Count(txCtx); err2 != nil {
				return err2
			} else if cnt > 0 {
				return nil
			}
			return err
		}
		ch := dead.Channel
		// 列白名单：seq 为 GENERATED ALWAYS AS IDENTITY（000002），重放
		// 插入同样不得显式提供。
		if _, err := r.db.Conn(txCtx).NewInsert().Model(&model.DocumentEventsOutbox{
			EventID:     dead.EventID,
			ProjectID:   dead.ProjectID,
			Topic:       dead.Topic,
			Channel:     ch,
			Payload:     dead.Payload,
			CreatedAt:   dead.CreatedAt,
			AvailableAt: time.Now(),
			Attempts:    0,
		}).Column("event_id", "project_id", "topic", "channel", "payload", "created_at", "available_at", "attempts").
			On("CONFLICT (event_id) DO NOTHING").Exec(txCtx); err != nil {
			return err
		}
		_, err := r.db.Conn(txCtx).NewDelete().Model((*model.DocumentEventsOutboxDead)(nil)).Where("event_id = ?", eventID).Exec(txCtx)
		return err
	})
}
