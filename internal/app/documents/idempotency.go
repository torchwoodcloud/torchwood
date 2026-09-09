// 写幂等（redesign §4.1/§10.1）：request_id 24h 去重，键作用域
// (project_id, actor_id, request_id)。本文件是核层通用包裹器与指纹构造。
package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/app/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxRequestIDLen 限制幂等键长度（对齐常见 Idempotency-Key 头上限）。
const maxRequestIDLen = 191

// idempotencyWaitBudget 是同 key in-flight 的短轮询等待上限：超时返回
// IDEMPOTENCY.IN_PROGRESS（Aborted）。包级变量便于测试缩短。
var idempotencyWaitBudget = 2 * time.Second

// idempotencyPollInterval 是同 key in-flight 的轮询间隔。
const idempotencyPollInterval = 100 * time.Millisecond

// requestFingerprint 计算 method + 请求体的规范序列化指纹：encoding/json 对
// map 按键排序、struct 按字段序，天然规范形；同 key 不同指纹 = KEY_CONFLICT。
func requestFingerprint(method string, body any) string {
	b, err := json.Marshal(body)
	if err != nil {
		// 指纹体字段均为 JSON-safe 类型；兜底退化为格式化串，保持可区分。
		b = []byte(fmt.Sprintf("%#v", body))
	}
	sum := sha256.Sum256(append([]byte(method+"\n"), b...))
	return hex.EncodeToString(sum[:])
}

// sortedStrings 返回排序副本：批 ID / conflict columns 是集合语义，重试顺序
// 不同不应判 KEY_CONFLICT。
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// normalizeData 统一 nil 与空 map（指纹与执行体看到同一形态）。
func normalizeData(data map[string]any) map[string]any {
	if data == nil {
		return map[string]any{}
	}
	return data
}

// documentFingerprintBody 是单文档写（Create/Update/Upsert/Delete）的指纹体；
// 各方法只填自己的字段，方法名前缀保证跨方法不碰撞。
type documentFingerprintBody struct {
	Database        string                           `json:"database"`
	Collection      string                           `json:"collection"`
	Document        string                           `json:"document"`
	Data            map[string]any                   `json:"data"`
	Permissions     []databases.Permission           `json:"permissions"`
	Increment       map[string]int64                 `json:"increment,omitempty"`
	ArrayUpdates    map[string]databases.ArrayUpdate `json:"array_updates,omitempty"`
	ConflictColumns []string                         `json:"conflict_columns,omitempty"`
	Version         *int64                           `json:"version,omitempty"`
}

// bulkFingerprintBody 是 Bulk 写的指纹体（ID 集合排序规范化）。
type bulkFingerprintBody struct {
	Database    string                 `json:"database"`
	Collection  string                 `json:"collection"`
	DocumentIDs []string               `json:"document_ids"`
	Data        map[string]any         `json:"data,omitempty"`
	Permissions []databases.Permission `json:"permissions,omitempty"`
}

// txFingerprintBody 是 execute-tx 的指纹体：op 顺序即执行序（有序），op 内
// conflict_columns 排序规范化（无序命中语义）。
type txFingerprintBody struct {
	Database string                    `json:"database"`
	Mode     databases.TransactionMode `json:"mode"`
	Ops      []databases.TransactionOp `json:"ops"`
}

func txOpsFingerprint(ops []databases.TransactionOp) []databases.TransactionOp {
	out := make([]databases.TransactionOp, len(ops))
	copy(out, ops)
	for i := range out {
		if len(out[i].ConflictColumns) > 0 {
			out[i].ConflictColumns = sortedStrings(out[i].ConflictColumns)
		}
	}
	return out
}

// idempotentExec 包裹写执行体（Go 方法不允许类型参数，故为自由函数）：
//   - requestID 空 / store 未注入 / actor 无稳定归因身份 → 直接执行（幂等关闭）；
//   - 同 key 不同指纹 → InvalidArgument（IDEMPOTENCY.KEY_CONFLICT）；
//   - 同 key 成功记录 → 反序列化原响应返回（replayed=true，不重复执行）；
//   - 同 key in-flight → 短轮询（≤idempotencyWaitBudget），超时 Aborted
//     （IDEMPOTENCY.IN_PROGRESS，retryable）；
//   - 执行失败 → Release 释放占位（失败不缓存），重试重新执行；
//   - 执行成功 → Complete 缓存响应（best-effort：写已成功，缓存失败只损失
//     重放能力，不回滚写入）。
//
// T 需可 JSON 往返（databases.Document / bulk 包装 / tx 结果均满足）。
func idempotentExec[T any](
	ctx context.Context,
	idem databases.IdempotencyStore,
	projectID, requestID, method string,
	principal databases.Principal,
	fingerprintBody any,
	exec func(context.Context) (T, error),
) (T, bool, error) {
	var zero T
	if idem == nil || requestID == "" {
		result, err := exec(ctx)
		return result, false, err
	}
	if len(requestID) > maxRequestIDLen {
		return zero, false, status.Errorf(codes.InvalidArgument, "request_id exceeds maximum length of %d", maxRequestIDLen)
	}
	actor := principal.StableActorID()
	if actor == "" {
		result, err := exec(ctx)
		return result, false, err
	}
	key := databases.IdempotencyKey{ProjectID: projectID, ActorID: actor, RequestID: requestID}
	fingerprint := requestFingerprint(method, fingerprintBody)

	deadline := time.Now().Add(idempotencyWaitBudget)
	for {
		claim, err := idem.TryClaim(ctx, key, fingerprint)
		if err != nil {
			if databases.ErrorDomainCode(err) != "" {
				return zero, false, shared.MapDocumentDBError(err)
			}
			return zero, false, status.Errorf(codes.Unavailable, "idempotency store unavailable: %v", err)
		}
		switch claim.State {
		case databases.IdempotencyClaimAcquired:
			result, err := exec(ctx)
			if err != nil {
				// 失败不缓存：释放占位行，重试重新执行。
				_ = idem.Release(ctx, key, claim.Token)
				return zero, false, err
			}
			if payload, merr := json.Marshal(result); merr == nil {
				_ = idem.Complete(ctx, key, claim.Token, payload)
			}
			return result, false, nil
		case databases.IdempotencyClaimDone:
			var cached T
			if err := json.Unmarshal(claim.Payload, &cached); err != nil {
				return zero, false, status.Error(codes.Internal, "idempotency cache payload is corrupted")
			}
			return cached, true, nil
		case databases.IdempotencyClaimInFlight:
			if time.Now().After(deadline) {
				return zero, false, shared.DomainStatus(databases.ErrCodeIdempotencyInProgress)
			}
			timer := time.NewTimer(idempotencyPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return zero, false, status.FromContextError(ctx.Err()).Err()
			case <-timer.C:
			}
		}
	}
}
