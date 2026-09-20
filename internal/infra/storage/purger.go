package storage

import (
	"context"
	"errors"
	"fmt"

	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
)

// purgeErrorSummaryCap 汇总错误里逐条列出的失败上限：防大前缀 + 长时间后端
// 故障时错误消息无限膨胀；超出部分以计数带过。
const purgeErrorSummaryCap = 10

// objectStorePurger 用 ObjectStore 的 List+Delete 组合实现 Purger 端口，
// 复用 app/storage DeleteBucket 的「按前缀清尾」清尾逻辑（Round4 J5-2）：
// 枚举后逐个 Delete，幂等可重试。P2 修复：
//   - 枚举优先走 PrefixStreamer 流式回调（minio ListObjects 本身分页拉取），
//     不再把全量清单物化进内存；未实现流式的 store（测试内存实现等）回退 List。
//   - 单个对象删除失败不再首败即中断：累计跳过、继续清剩余对象，末尾汇总
//     报告失败明细（重试调用方仍可整体重入，List+Delete 幂等）。
type objectStorePurger struct {
	store domainstorage.ObjectStore
}

// NewObjectPurger 从同一 ObjectStore 实例派生 Purger（共享底层客户端与
// 配置；Wire 注入，不重复构造 MinIO client）。
func NewObjectPurger(store domainstorage.ObjectStore) domainstorage.Purger {
	return &objectStorePurger{store: store}
}

func (p *objectStorePurger) PurgePrefix(ctx context.Context, bucket, prefix string) (int, error) {
	var purged int
	var delErrs []error
	deleteOne := func(key string) {
		if err := p.store.Delete(ctx, bucket, key); err != nil {
			delErrs = append(delErrs, fmt.Errorf("delete object %s: %w", key, err))
			return
		}
		purged++
	}

	if streamer, ok := p.store.(domainstorage.PrefixStreamer); ok {
		// 流式路径：fn 返回错误即终止枚举（ctx 取消等）；单对象删除失败由
		// deleteOne 记账后继续。
		err := streamer.StreamPrefix(ctx, bucket, prefix, func(obj domainstorage.ObjectMeta) error {
			deleteOne(obj.Key)
			return ctx.Err()
		})
		if err != nil {
			return purged, fmt.Errorf("list objects under %s/: %w", prefix, err)
		}
	} else {
		objects, err := p.store.List(ctx, bucket, prefix)
		if err != nil {
			return 0, fmt.Errorf("list objects under %s/: %w", prefix, err)
		}
		for _, obj := range objects {
			deleteOne(obj.Key)
		}
	}

	if len(delErrs) > 0 {
		return purged, summarizePurgeErrors(prefix, purged, delErrs)
	}
	return purged, nil
}

// summarizePurgeErrors 把累计的删除失败折成单条错误：逐条明细至多
// purgeErrorSummaryCap 条，附带成功/失败计数供调用方评估重试范围。
func summarizePurgeErrors(prefix string, purged int, delErrs []error) error {
	listed := delErrs
	if len(listed) > purgeErrorSummaryCap {
		listed = listed[:purgeErrorSummaryCap]
	}
	joined := errors.Join(listed...)
	return fmt.Errorf("purge %s/: deleted %d objects, %d deletions failed: %w",
		prefix, purged, len(delErrs), joined)
}
