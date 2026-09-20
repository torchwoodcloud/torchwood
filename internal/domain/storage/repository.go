package storage

import (
	"context"
	"errors"
)

var (
	ErrBucketIDRequired = errors.New("bucket id is required")
	ErrFileIDRequired   = errors.New("file id is required")
	ErrInvalidUpdate    = errors.New("invalid storage update")
)

// BucketRepository 把 bucket 元数据持久化到项目 schema。
type BucketRepository interface {
	Insert(ctx context.Context, projectID string, bucket *Bucket) error
	GetByID(ctx context.Context, projectID, id string) (*Bucket, error)
	// List 分页列出 project 的 bucket：SQL 侧 LIMIT/OFFSET 下推（P2 修复：
	// 旧实现全量捞取后内存分页）。返回过滤后总数供分页 token 计算；
	// limit<=0 由实现取默认、超上限由实现 clamp（对齐 users_repo 先例）。
	List(ctx context.Context, projectID string, limit, offset int) ([]*Bucket, int64, error)
	Count(ctx context.Context, projectID string) (int64, error)
	// Update 只 SET 点名列（name/public/permissions）；permissions JSON 为 PUT last-write-wins。
	Update(ctx context.Context, projectID, id string, cols map[string]any) error
	Delete(ctx context.Context, projectID, id string) error
}

// FileRepository 把 file 元数据持久化到项目 schema。
type FileRepository interface {
	Insert(ctx context.Context, projectID string, file *File) error
	GetByID(ctx context.Context, projectID, id string) (*File, error)
	// ListByBucket 分页列出 bucket 下文件：ownerUserID 非空时 SQL 侧下推
	// owner 过滤（A8 端用户隔离；P2 修复：旧实现全量捞取后内存过滤/分页），
	// limit/offset SQL 侧 LIMIT/OFFSET 下推。返回过滤后总数；
	// limit<=0 由实现取默认、超上限由实现 clamp（对齐 users_repo 先例）。
	ListByBucket(ctx context.Context, projectID, bucketID, ownerUserID string, limit, offset int) ([]*File, int64, error)
	Count(ctx context.Context, projectID string) (int64, error)
	// Update 只 SET 点名列（name/mime_type/metadata）；metadata 为 PUT last-write-wins。
	Update(ctx context.Context, projectID, id string, cols map[string]any) error
	Delete(ctx context.Context, projectID, id string) error
	// SumSize 供 billing 替代 SumDocumentField；无 project_id 列，靠 schema 限定。
	SumSize(ctx context.Context, projectID string) (int64, error)
}
