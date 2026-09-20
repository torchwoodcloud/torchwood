package storage

import (
	"context"
	"fmt"
	"io"
	"net/url"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// minioObjectStore is an S3-compatible ObjectStore implementation.
type minioObjectStore struct {
	client *minio.Client
	bucket string
}

// NewMinioObjectStore creates a new MinIO-backed object store.
func NewMinioObjectStore(cfg *config.AppConfig) (storage.ObjectStore, error) {
	s := cfg.GetStorage().GetS3()
	endpoint := s.GetEndpoint()
	useSSL := s.GetUseSsl()

	// If endpoint contains a scheme, extract it for SSL detection.
	if u, err := url.Parse(endpoint); err == nil && u.Scheme != "" {
		endpoint = u.Host
		if u.Scheme == "https" {
			useSSL = true
		}
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(s.GetAccessKeyId(), s.GetSecretAccessKey(), ""),
		Secure: useSSL,
		Region: s.GetRegion(),
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}

	bucket := s.GetBucket()
	if bucket == "" {
		// S3/MinIO bucket 命名规范要求全小写（大写名 MakeBucket 会失败）。
		bucket = storage.DefaultBucketName
	}

	return &minioObjectStore{client: client, bucket: bucket}, nil
}

func (m *minioObjectStore) EnsureBucket(ctx context.Context, name string) error {
	exists, err := m.client.BucketExists(ctx, name)
	if err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if exists {
		return nil
	}
	if err := m.client.MakeBucket(ctx, name, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		// P2 修复：BucketExists 与 MakeBucket 之间存在竞态（并发写路径同时
		// ensure、另一实例已建桶），S3 以 BucketAlreadyOwnedByYou/BucketAlreadyExists
		// 应答——视为创建成功，不再误报失败。
		if code := minio.ToErrorResponse(err).Code; code == "BucketAlreadyOwnedByYou" || code == "BucketAlreadyExists" {
			return nil
		}
		return fmt.Errorf("make bucket: %w", err)
	}
	return nil
}

func (m *minioObjectStore) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := m.client.PutObject(ctx, bucket, key, data, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (m *minioObjectStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	obj, err := m.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, m.mapObjectMiss(err, bucket, key)
	}
	// Check existence by reading stat.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, m.mapObjectMiss(err, bucket, key)
	}
	return obj, nil
}

// mapObjectMiss 把 S3 的对象缺失应答（NoSuchKey；HEAD 无响应体时 minio-go
// 合成 NotFound）归一为 ErrObjectNotFound 哨兵，供跨层 errors.Is 区分 miss 与
// 传输/暂态错误。P2 修复：改用 ToErrorResponse 的结构化 Code 判定，不再对错误
// 文本做子串匹配（文本随 SDK/后端措辞变化，且会误吞含同名字样的暂态错误）。
func (m *minioObjectStore) mapObjectMiss(err error, bucket, key string) error {
	if code := minio.ToErrorResponse(err).Code; code == "NoSuchKey" || code == "NotFound" {
		return fmt.Errorf("%w: %s/%s", storage.ErrObjectNotFound, bucket, key)
	}
	return err
}

func (m *minioObjectStore) Delete(ctx context.Context, bucket, key string) error {
	return m.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

// List 列出 bucket 下指定前缀的对象（recursive），收集 Key 与 LastModified。
func (m *minioObjectStore) List(ctx context.Context, bucket, prefix string) ([]storage.ObjectMeta, error) {
	opts := minio.ListObjectsOptions{Prefix: prefix, Recursive: true}
	var out []storage.ObjectMeta
	for info := range m.client.ListObjects(ctx, bucket, opts) {
		if info.Err != nil {
			return nil, info.Err
		}
		out = append(out, storage.ObjectMeta{Key: info.Key, LastModified: info.LastModified})
	}
	return out, nil
}

// StreamPrefix 实现域层 PrefixStreamer（P2 修复：PurgePrefix 不再把全量清单
// 物化进内存）。minio-go ListObjects 返回的 channel 本身就是分页拉取的迭代器，
// 逐条回调；fn 返回错误（如 ctx 取消）即停止枚举并透传。
func (m *minioObjectStore) StreamPrefix(ctx context.Context, bucket, prefix string, fn func(storage.ObjectMeta) error) error {
	opts := minio.ListObjectsOptions{Prefix: prefix, Recursive: true}
	for info := range m.client.ListObjects(ctx, bucket, opts) {
		if info.Err != nil {
			return info.Err
		}
		if err := fn(storage.ObjectMeta{Key: info.Key, LastModified: info.LastModified}); err != nil {
			return err
		}
	}
	return nil
}

// Compose 将 srcKeys 按序服务端合并为 dstKey（映射 minio-go ComposeObject）。
// 多源路径忽略目标 Content-Type（对象 mime 恒为 octet-stream，以文档 mime 为准）；
// 5MiB/10000 约束由服务端校验兜底，ComposeObject 失败（小片）返回错误透传。
func (m *minioObjectStore) Compose(ctx context.Context, bucket, dstKey string, srcKeys []string) error {
	srcs := make([]minio.CopySrcOptions, len(srcKeys))
	for i, k := range srcKeys {
		srcs[i] = minio.CopySrcOptions{Bucket: bucket, Object: k}
	}
	_, err := m.client.ComposeObject(ctx, minio.CopyDestOptions{Bucket: bucket, Object: dstKey}, srcs...)
	return err
}

func (m *minioObjectStore) Ping(ctx context.Context) error {
	_, err := m.client.BucketExists(ctx, m.bucketName())
	return err
}

func (m *minioObjectStore) bucketName() string {
	if m.bucket != "" {
		return m.bucket
	}
	return storage.DefaultBucketName
}

// DefaultBucket returns the configured default bucket name.
func (m *minioObjectStore) DefaultBucket() string { return m.bucketName() }
