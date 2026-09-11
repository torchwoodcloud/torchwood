package leaderboards

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxRequestIDLen       = 191
	idempotencyWaitBudget = 2 * time.Second
	idempotencyPollEvery  = 100 * time.Millisecond
)

// submitFingerprintBody 是 submit 的指纹体：同 key 不同请求 = KEY_CONFLICT。
type submitFingerprintBody struct {
	Board    string `json:"board"`
	Subject  string `json:"subject"`
	Value    int64  `json:"value"`
	Tiebreak *int64 `json:"tiebreak,omitempty"`
	Period   string `json:"period"`
}

func requestFingerprint(method string, body any) string {
	b, err := json.Marshal(body)
	if err != nil {
		b = []byte(method)
	}
	sum := sha256.Sum256(append([]byte(method+"\n"), b...))
	return hex.EncodeToString(sum[:])
}

// stableActorID 取 principal 的稳定归因身份：ActorID 优先，终端用户回落
// UserID（client 面）。
func stableActorID(ctx context.Context) string {
	p, ok := contexts.Principal(ctx)
	if !ok || p == nil {
		return ""
	}
	if p.ActorID != "" {
		return string(p.ActorID)
	}
	return p.UserID
}

// submitWithIdempotency 包裹 doSubmit（语义对齐 documents 的写幂等）：
//   - requestID 空 / store 未注入 / 无稳定归因身份 → 直接执行；
//   - 同 key 不同指纹 → InvalidArgument（KEY_CONFLICT）；
//   - 同 key 成功记录 → 反序列化原快照返回（replayed=true，quota 不重复计）；
//   - 同 key in-flight → 短轮询，超时 Aborted；
//   - 失败释放、成功缓存（best-effort）。
func (a *Leaderboards) submitWithIdempotency(ctx context.Context, projectID string, cmd SubmitCommand) (*domainleaderboards.Snapshot, bool, error) {
	if a.idem == nil || cmd.RequestID == "" {
		snap, err := a.doSubmit(ctx, projectID, cmd)
		return snap, false, mapLeaderboardError(err)
	}
	if len(cmd.RequestID) > maxRequestIDLen {
		return nil, false, status.Errorf(codes.InvalidArgument, "request_id exceeds maximum length of %d", maxRequestIDLen)
	}
	actor := stableActorID(ctx)
	if actor == "" {
		snap, err := a.doSubmit(ctx, projectID, cmd)
		return snap, false, mapLeaderboardError(err)
	}
	key := databases.IdempotencyKey{ProjectID: projectID, ActorID: actor, RequestID: cmd.RequestID}
	fingerprint := requestFingerprint("leaderboards:submit", submitFingerprintBody{
		Board:    cmd.BoardID,
		Subject:  cmd.SubjectID,
		Value:    cmd.Value,
		Tiebreak: cmd.Tiebreak,
		Period:   cmd.Period,
	})

	deadline := time.Now().Add(idempotencyWaitBudget)
	for {
		claim, err := a.idem.TryClaim(ctx, key, fingerprint)
		if err != nil {
			if errors.Is(err, databases.ErrIdempotencyKeyConflict) {
				return nil, false, status.Error(codes.InvalidArgument, "idempotency key conflict: same request_id with different payload")
			}
			return nil, false, status.Errorf(codes.Unavailable, "idempotency store unavailable: %v", err)
		}
		switch claim.State {
		case databases.IdempotencyClaimAcquired:
			snap, err := a.doSubmit(ctx, projectID, cmd)
			if err != nil {
				_ = a.idem.Release(ctx, key, claim.Token)
				return nil, false, mapLeaderboardError(err)
			}
			if payload, merr := json.Marshal(snap); merr == nil {
				_ = a.idem.Complete(ctx, key, claim.Token, payload)
			}
			return snap, false, nil
		case databases.IdempotencyClaimDone:
			var cached domainleaderboards.Snapshot
			if err := json.Unmarshal(claim.Payload, &cached); err != nil {
				return nil, false, status.Error(codes.Internal, "idempotency cache payload is corrupted")
			}
			return &cached, true, nil
		case databases.IdempotencyClaimInFlight:
			if time.Now().After(deadline) {
				return nil, false, status.Error(codes.Aborted, "idempotency: same request_id is in progress, retry later")
			}
			timer := time.NewTimer(idempotencyPollEvery)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, false, status.FromContextError(ctx.Err()).Err()
			case <-timer.C:
			}
		}
	}
}
