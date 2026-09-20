package functions

import (
	"context"
	"errors"
)

// DefaultZipBucketName 是未配置 functions.storage.bucket 时的部署代码包
// 专用桶名（与用户 storage 的 DefaultBucketName "torchwood-files" 物理隔离；
// app 层注释与 infra 层默认值共用此单一常量，先例 storage.DefaultBucketName）。
const DefaultZipBucketName = "torchwood-functions"

// ErrZipNotFound 表示持久层无该部署代码包对象（区别于传输/暂态错误——调用方
// 据此走「放弃重建 / 回退 git 重物化」分支，暂态错误则让路待下次执行重进）。
var ErrZipNotFound = errors.New("code package not found in object store")

// ZipStore 是部署代码包的持久对象存储端口（zip 持久层）：本地盘
// <TempDir>/torchwood-functions 仍是构建输入的第一层，本端口承载原始字节
// 的持久副本——盘上 zip 丢失（磁盘清理 / 环境迁移 / 多节点不共享盘）时，
// 部署重建链路可从持久层拉回复核后重建（zip 源自愈；git 源优先拉回、
// miss 时回退 packer 重物化）。桶是平台内部资源，不进用户 bucket 命名空间。
// nil（旧构造 / 测试）时写路径仅落本地盘、重建链路退化为既有声明边界
// （zip 源盘缺失不可自愈）。
type ZipStore interface {
	// Put 持久化部署代码包（同键覆盖幂等）。
	Put(ctx context.Context, projectID, functionID, deploymentID string, zip []byte) error
	// Get 取回部署代码包；持久层无该对象时返回 ErrZipNotFound。
	Get(ctx context.Context, projectID, functionID, deploymentID string) ([]byte, error)
	// Remove 删除部署代码包（幂等：删除不存在的对象不是错误）。
	Remove(ctx context.Context, projectID, functionID, deploymentID string) error
}
