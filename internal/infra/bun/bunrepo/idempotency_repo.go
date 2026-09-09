package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

// IdempotencyStore 是 databases.IdempotencyStore 的 public 控制面实现
// （bun + 原生 SQL）：TryClaim 的原子性由主键唯一约束仲裁——并发 INSERT
// ON CONFLICT DO NOTHING 在唯一索引上串行等待，同一时刻至多一个调用方拿到
// Acquired。
type IdempotencyStore struct {
	db *clients.Database
}

func NewIdempotencyStore(db *clients.Database) *IdempotencyStore {
	return &IdempotencyStore{db: db}
}

var _ databases.IdempotencyStore = (*IdempotencyStore)(nil)

// 幂等行状态。
const (
	idempotencyStateInFlight = "in_flight"
	idempotencyStateDone     = "done"
)

// 认领有界重试（返工 R8）：INSERT 撞上未提交的冲突行会等待对端提交；若对端
// 随即 Release（删除行），紧随的 SELECT 撞 ErrNoRows——这是可自愈的瞬态，
// 重走 INSERT 多半可直接认领。小退避 + ≤3 次后仍失败才上抛（app 层映射
// Unavailable，调用方可重试）。
const (
	idempotencyClaimMaxAttempts  = 3
	idempotencyClaimRetryBackoff = 25 * time.Millisecond
)

func (s *IdempotencyStore) TryClaim(ctx context.Context, key databases.IdempotencyKey, fingerprint string) (databases.IdempotencyClaim, error) {
	conn := s.db.Conn(ctx)
	// 惰性清理：写入时顺带删除过期行（含崩溃残留的 in_flight）。清理是
	// best-effort——失败不阻断认领主路径，过期行也会在下方的重认领分支兜底。
	_, _ = conn.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)

	for attempt := 1; ; attempt++ {
		token := idgen.UUID().String()
		res, err := conn.ExecContext(ctx,
			`INSERT INTO idempotency_keys
			   (project_id, actor_id, request_id, fingerprint, claim_token, state, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, now() + (? * interval '1 second'))
			 ON CONFLICT (project_id, actor_id, request_id) DO NOTHING`,
			key.ProjectID, key.ActorID, key.RequestID, fingerprint, token,
			idempotencyStateInFlight, databases.IdempotencyInFlightTTL.Seconds(),
		)
		if err != nil {
			return databases.IdempotencyClaim{}, err
		}
		if inserted, _ := res.RowsAffected(); inserted > 0 {
			return databases.IdempotencyClaim{State: databases.IdempotencyClaimAcquired, Token: token}, nil
		}

		var rowFP, rowState string
		var rowPayload []byte
		err = conn.QueryRowContext(ctx,
			`SELECT fingerprint, state, response_payload FROM idempotency_keys
			 WHERE project_id = ? AND actor_id = ? AND request_id = ?`,
			key.ProjectID, key.ActorID, key.RequestID,
		).Scan(&rowFP, &rowState, &rowPayload)
		switch {
		case err == nil:
		case errors.Is(err, sql.ErrNoRows):
			// 竞态（R8）：冲突行在 INSERT 等待窗口内被对端 Release/清理删除。
			if attempt < idempotencyClaimMaxAttempts {
				timer := time.NewTimer(idempotencyClaimRetryBackoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return databases.IdempotencyClaim{}, ctx.Err()
				case <-timer.C:
				}
				continue
			}
			return databases.IdempotencyClaim{}, fmt.Errorf(
				"idempotency claim contention: conflicting row kept disappearing after %d attempts: %w", attempt, err)
		default:
			return databases.IdempotencyClaim{}, err
		}
		if rowFP != fingerprint {
			return databases.IdempotencyClaim{}, databases.ErrIdempotencyKeyConflict
		}
		if rowState == idempotencyStateDone {
			return databases.IdempotencyClaim{State: databases.IdempotencyClaimDone, Payload: rowPayload}, nil
		}
		return databases.IdempotencyClaim{State: databases.IdempotencyClaimInFlight}, nil
	}
}

func (s *IdempotencyStore) Complete(ctx context.Context, key databases.IdempotencyKey, token string, payload []byte) error {
	// payload 以 string 绑定并显式 ::jsonb：pgdriver 对 []byte 走 bytea 编码，
	// PG 无法按 jsonb 输入语法解析（22P02）。
	_, err := s.db.Conn(ctx).ExecContext(ctx,
		`UPDATE idempotency_keys
		 SET state = ?, response_payload = ?::jsonb, expires_at = now() + (? * interval '1 second'), updated_at = now()
		 WHERE project_id = ? AND actor_id = ? AND request_id = ? AND claim_token = ? AND state = ?`,
		idempotencyStateDone, string(payload), databases.IdempotencyDoneTTL.Seconds(),
		key.ProjectID, key.ActorID, key.RequestID, token, idempotencyStateInFlight,
	)
	// 0 行（token 因过期被重认领）不是错误：写已成功，只损失本条缓存。
	return err
}

func (s *IdempotencyStore) Release(ctx context.Context, key databases.IdempotencyKey, token string) error {
	_, err := s.db.Conn(ctx).ExecContext(ctx,
		`DELETE FROM idempotency_keys
		 WHERE project_id = ? AND actor_id = ? AND request_id = ? AND claim_token = ? AND state = ?`,
		key.ProjectID, key.ActorID, key.RequestID, token, idempotencyStateInFlight,
	)
	return err
}

// ExpireAt 供集成测试直接操纵 TTL（测试辅助，不进端口契约）。
func (s *IdempotencyStore) ExpireAt(ctx context.Context, key databases.IdempotencyKey, at time.Time) error {
	_, err := s.db.Conn(ctx).ExecContext(ctx,
		`UPDATE idempotency_keys SET expires_at = ? WHERE project_id = ? AND actor_id = ? AND request_id = ?`,
		at, key.ProjectID, key.ActorID, key.RequestID,
	)
	return err
}
