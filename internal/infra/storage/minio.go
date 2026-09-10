package storage

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/pkg/config"
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
		return nil, err
	}
	// Check existence by reading stat.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "not found") {
			return nil, fmt.Errorf("object not found: %s/%s", bucket, key)
		}
		return nil, err
	}
	return obj, nil
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
