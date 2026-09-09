package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/internal/domain/storage"
)

// redisUploadSessionStore 将上传会话持久化到 Redis：
//   - `torchwood:upload:{uploadID}` Hash：全部元数据字段（metadata/permissions JSON 序列化）；
//   - `torchwood:upload:{uploadID}:parts` Set：已收分片号；
//   - TTL 24h，Create 与 MarkChunk 都刷新 EXPIRE；
//   - complete 互斥锁：`torchwood:upload:{uploadID}:lock` SETNX 1h（随机 token 校验持有者）。
type redisUploadSessionStore struct {
	rdb *redis.Client
}

// completeLockTTL 是 complete 互斥锁的持有时间（超时自动释放，防宕机死锁；
// 1h 需覆盖最坏情况的长 Compose 时间，配合 IsLockOwner 二次确认防止误删）。
const completeLockTTL = time.Hour

// NewRedisUploadSessionStore creates a Redis-backed upload session store.
func NewRedisUploadSessionStore(rdb *redis.Client) storage.UploadSessionStore {
	return &redisUploadSessionStore{rdb: rdb}
}

func uploadSessionKey(uploadID string) string {
	return "torchwood:upload:" + uploadID
}

func uploadPartsKey(uploadID string) string {
	return "torchwood:upload:" + uploadID + ":parts"
}

func uploadLockKey(uploadID string) string {
	return "torchwood:upload:" + uploadID + ":lock"
}

func (s *redisUploadSessionStore) Create(ctx context.Context, up *storage.UploadSession) error {
	metadata, err := json.Marshal(up.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	permissions, err := json.Marshal(up.Permissions)
	if err != nil {
		return fmt.Errorf("marshal permissions: %w", err)
	}
	key := uploadSessionKey(up.ID)
	_, err = s.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, key, map[string]any{
			"project_id":    up.ProjectID,
			"bucket_id":     up.BucketID,
			"file_id":       up.FileID,
			"owner_user_id": up.OwnerUserID,
			"name":          up.Name,
			"mime_type":     up.MimeType,
			"size":          up.Size,
			"metadata":      string(metadata),
			"permissions":   string(permissions),
			"chunk_size":    up.ChunkSize,
			"part_count":    up.PartCount,
			"created_at":    up.CreatedAt.UTC().Format(time.RFC3339Nano),
			"expires_at":    up.ExpiresAt.UTC().Format(time.RFC3339Nano),
		})
		pipe.Expire(ctx, key, storage.UploadSessionTTL)
		return nil
	})
	return err
}

func (s *redisUploadSessionStore) Get(ctx context.Context, uploadID string) (*storage.UploadSession, error) {
	key := uploadSessionKey(uploadID)
	m, err := s.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, nil
	}
	parts, err := s.rdb.SMembers(ctx, uploadPartsKey(uploadID)).Result()
	if err != nil {
		return nil, err
	}
	up := &storage.UploadSession{
		ID:          uploadID,
		ProjectID:   m["project_id"],
		BucketID:    m["bucket_id"],
		FileID:      m["file_id"],
		OwnerUserID: m["owner_user_id"],
		Name:        m["name"],
		MimeType:    m["mime_type"],
		Received:    map[int]bool{},
	}
	if up.Size, err = strconv.ParseInt(m["size"], 10, 64); err != nil {
		return nil, fmt.Errorf("parse size: %w", err)
	}
	if up.ChunkSize, err = strconv.ParseInt(m["chunk_size"], 10, 64); err != nil {
		return nil, fmt.Errorf("parse chunk_size: %w", err)
	}
	if up.PartCount, err = strconv.Atoi(m["part_count"]); err != nil {
		return nil, fmt.Errorf("parse part_count: %w", err)
	}
	if up.CreatedAt, err = time.Parse(time.RFC3339Nano, m["created_at"]); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if up.ExpiresAt, err = time.Parse(time.RFC3339Nano, m["expires_at"]); err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}
	if raw := m["metadata"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &up.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	if raw := m["permissions"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &up.Permissions); err != nil {
			return nil, fmt.Errorf("unmarshal permissions: %w", err)
		}
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("parse part %q: %w", p, err)
		}
		up.Received[n] = true
	}
	return up, nil
}

// MarkChunk 原子标记分片已收并刷新 TTL；会话 key 已不存在（删除/过期）时
// 静默返回 nil，避免 SADD 重建无 TTL 的孤儿 parts key。
func (s *redisUploadSessionStore) MarkChunk(ctx context.Context, uploadID string, partNumber int) error {
	key := uploadSessionKey(uploadID)
	exists, err := s.rdb.Exists(ctx, key).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	_, err = s.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.SAdd(ctx, uploadPartsKey(uploadID), partNumber)
		pipe.Expire(ctx, key, storage.UploadSessionTTL)
		pipe.Expire(ctx, uploadPartsKey(uploadID), storage.UploadSessionTTL)
		return nil
	})
	return err
}

// CountChunks 返回已收分片数（SCARD 原子、准确；会话不存在返回 0）。
func (s *redisUploadSessionStore) CountChunks(ctx context.Context, uploadID string) (int, error) {
	n, err := s.rdb.SCard(ctx, uploadPartsKey(uploadID)).Result()
	return int(n), err
}

func (s *redisUploadSessionStore) Delete(ctx context.Context, uploadID string) error {
	_, err := s.rdb.Del(ctx, uploadSessionKey(uploadID), uploadPartsKey(uploadID), uploadLockKey(uploadID)).Result()
	return err
}

func (s *redisUploadSessionStore) LockComplete(ctx context.Context, uploadID string) (string, bool, error) {
	token := randomLockToken()
	ok, err := s.rdb.SetNX(ctx, uploadLockKey(uploadID), token, completeLockTTL).Result()
	return token, ok, err
}

// IsLockOwner 校验锁仍由 token 持有者持有（锁不存在或已被其他 complete 以新 token
// 重新获取时返回 false）。
func (s *redisUploadSessionStore) IsLockOwner(ctx context.Context, uploadID, token string) (bool, error) {
	v, err := s.rdb.Get(ctx, uploadLockKey(uploadID)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == token, nil
}

// unlockCompleteScript compare-and-del：仅当锁值仍为持有者 token 时才删除，
// 防止误删 TTL 过期后已被其他 complete 重新持有的锁（写法同 pkg/semaphore）。
const unlockCompleteScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`

func (s *redisUploadSessionStore) UnlockComplete(ctx context.Context, uploadID, token string) error {
	_, err := s.rdb.Eval(ctx, unlockCompleteScript, []string{uploadLockKey(uploadID)}, token).Result()
	return err
}

// randomLockToken 生成 complete 锁随机 token（16 字节随机数 hex）。
func randomLockToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
