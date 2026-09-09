package storage

import (
	"context"
	"fmt"

	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
)

// objectStorePurger 用 ObjectStore 的 List+Delete 组合实现 Purger 端口，
// 复用 app/storage DeleteBucket 的「按前缀清尾」清尾逻辑（Round4 J5-2）：
// ListObjects(prefix, recursive) 分批枚举后逐个 Delete，幂等可重试。
type objectStorePurger struct {
	store domainstorage.ObjectStore
}

// NewObjectPurger 从同一 ObjectStore 实例派生 Purger（共享底层客户端与
// 配置；Wire 注入，不重复构造 MinIO client）。
func NewObjectPurger(store domainstorage.ObjectStore) domainstorage.Purger {
	return &objectStorePurger{store: store}
}

func (p *objectStorePurger) PurgePrefix(ctx context.Context, bucket, prefix string) (int, error) {
	objects, err := p.store.List(ctx, bucket, prefix)
	if err != nil {
		return 0, fmt.Errorf("list objects under %s/: %w", prefix, err)
	}
	purged := 0
	for _, obj := range objects {
		if derr := p.store.Delete(ctx, bucket, obj.Key); derr != nil {
			return purged, fmt.Errorf("delete object %s: %w", obj.Key, derr)
		}
		purged++
	}
	return purged, nil
}
