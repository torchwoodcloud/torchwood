package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// Bucket represents a storage bucket (metadata lives in the dynamic document DB).
type Bucket struct {
	ID          string
	ProjectID   string
	Name        string
	Permissions []string
	Public      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// File represents a stored file (metadata lives in the dynamic document DB).
type File struct {
	ID          string
	ProjectID   string
	BucketID    string
	Name        string
	MimeType    string
	Size        int64
	Metadata    map[string]string
	OwnerUserID string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Usage aggregates storage statistics for a project.
type Usage struct {
	Buckets   int64
	Files     int64
	TotalSize int64
}

// ObjectMeta 是对象存储中一个对象的元数据（用于后台清理/前缀扫描）。
type ObjectMeta struct {
	Key          string
	LastModified time.Time
}

// DefaultBucketName 是未配置 storage.s3.bucket 时的默认 bucket 名。
// S3/MinIO bucket 命名规范要求全小写（大写名 MakeBucket 会失败），
// app 层 defaultBucketName 与 infra 层默认值共用此单一常量。
const DefaultBucketName = "torchwood-files"

// ErrObjectNotFound 表示对象在底层存储中不存在（Get 按键寻址的 miss 形态；
// 供跨域调用方 errors.Is 区分 miss 与传输/暂态错误——先例：functions 的
// zip 持久层拉回复核链路）。
var ErrObjectNotFound = errors.New("object not found")

// ObjectStore abstracts binary object storage (S3 / MinIO).
type ObjectStore interface {
	// EnsureBucket creates the underlying S3 bucket if it does not exist.
	EnsureBucket(ctx context.Context, name string) error
	// Put uploads an object with the given key.
	Put(ctx context.Context, bucket, key string, data io.Reader, size int64, contentType string) error
	// Get downloads an object.
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	// Delete removes an object.
	Delete(ctx context.Context, bucket, key string) error
	// Compose 将 srcKeys 按序服务端合并为 dstKey（映射 minio-go ComposeObject；
	// 约束：除最后一个源外每个源 ≥ 5MiB、源数 ≤ 10000、目标对象 Content-Type
	// 无法设置（多源路径忽略，对象 mime 恒为 octet-stream，以文档 mime 为准））。
	Compose(ctx context.Context, bucket, dstKey string, srcKeys []string) error
	// List 列出 bucket 下指定前缀的对象（recursive）。
	List(ctx context.Context, bucket, prefix string) ([]ObjectMeta, error)
	// Ping probes connectivity to the underlying store.
	Ping(ctx context.Context) error
}

// PrefixStreamer 是 ObjectStore 的可选扩展接口：流式枚举指定前缀下的对象
// （分页拉取、逐条回调，不把全量清单物化进内存；P2 修复：大前缀清理曾整表
// 入内存）。不强制所有 ObjectStore 实现提供——消费方（Purger）类型断言使用，
// 未实现时回退 List 全量路径。
type PrefixStreamer interface {
	// StreamPrefix 逐个回调前缀下的对象；fn 返回错误（如 ctx 取消）即终止
	// 枚举并透传该错误。
	StreamPrefix(ctx context.Context, bucket, prefix string, fn func(ObjectMeta) error) error
}
