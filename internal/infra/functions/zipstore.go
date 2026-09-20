package functions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// bucketZipStore 把 domainfunctions.ZipStore 适配到 ObjectStore 专用物理桶
// （functions.storage.bucket，缺省 domainfunctions.DefaultZipBucketName）。
// 桶是平台内部资源：不进用户 bucket 命名空间（项目 buckets 表无行、API
// 不可见），键布局沿用本地 zip 路径结构 <projectID>/<functionID>/
// <deploymentID>.zip。桶创建走写路径懒 ensure（与 app/storage 写路径同款；
// EnsureBucket 幂等）。
type bucketZipStore struct {
	objects storage.ObjectStore
	bucket  string
}

// NewBucketZipStore 构造部署代码包持久层适配器（复用 storage.s3 的
// endpoint/凭证连接同一对象存储后端）。
func NewBucketZipStore(cfg *config.AppConfig, objects storage.ObjectStore) domainfunctions.ZipStore {
	name := cfg.GetFunctions().GetStorage().GetBucket()
	if name == "" {
		name = domainfunctions.DefaultZipBucketName
	}
	return &bucketZipStore{objects: objects, bucket: name}
}

// zipObjectKey 拼对象键；空 ID 直接拒绝（纵深防御：正常调用链 ID 恒非空，
// 空段会产生意料外的前缀嵌套）。
func zipObjectKey(projectID, functionID, deploymentID string) (string, error) {
	if projectID == "" || functionID == "" || deploymentID == "" {
		return "", fmt.Errorf("zip object key requires non-empty project/function/deployment ids")
	}
	return path.Join(projectID, functionID, deploymentID+".zip"), nil
}

func (s *bucketZipStore) Put(ctx context.Context, projectID, functionID, deploymentID string, zip []byte) error {
	key, err := zipObjectKey(projectID, functionID, deploymentID)
	if err != nil {
		return err
	}
	if err := s.objects.EnsureBucket(ctx, s.bucket); err != nil {
		return fmt.Errorf("ensure bucket %s: %w", s.bucket, err)
	}
	if err := s.objects.Put(ctx, s.bucket, key, bytes.NewReader(zip), int64(len(zip)), "application/zip"); err != nil {
		return fmt.Errorf("put code package %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

func (s *bucketZipStore) Get(ctx context.Context, projectID, functionID, deploymentID string) ([]byte, error) {
	key, err := zipObjectKey(projectID, functionID, deploymentID)
	if err != nil {
		return nil, err
	}
	rc, err := s.objects.Get(ctx, s.bucket, key)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return nil, fmt.Errorf("%w: %s/%s", domainfunctions.ErrZipNotFound, s.bucket, key)
		}
		return nil, fmt.Errorf("get code package %s/%s: %w", s.bucket, key, err)
	}
	defer func() { _ = rc.Close() }()
	zip, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read code package %s/%s: %w", s.bucket, key, err)
	}
	return zip, nil
}

func (s *bucketZipStore) Remove(ctx context.Context, projectID, functionID, deploymentID string) error {
	key, err := zipObjectKey(projectID, functionID, deploymentID)
	if err != nil {
		return err
	}
	if err := s.objects.Delete(ctx, s.bucket, key); err != nil {
		return fmt.Errorf("delete code package %s/%s: %w", s.bucket, key, err)
	}
	return nil
}
