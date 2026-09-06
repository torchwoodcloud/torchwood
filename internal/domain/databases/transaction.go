package databases

import (
	"errors"
	"fmt"
)

// OpError 标记 ATOMIC 批内首个失败 op 及其 index（错误链保留底层哨兵供
// errors.Is 穿透；消息带 op[index] 供调用方定位）。
type OpError struct {
	Index int
	Err   error
}

func (e *OpError) Error() string { return fmt.Sprintf("op[%d]: %v", e.Index, e.Err) }
func (e *OpError) Unwrap() error { return e.Err }

// AsOpError 从错误链提取 OpError（非 OpError 返回 nil）。
func AsOpError(err error) *OpError {
	var oe *OpError
	if errors.As(err, &oe) {
		return oe
	}
	return nil
}

// 事务内核 op 模型（redesign §4.8/E1）：可序列化异构 op 列表在单事务内
// 顺序执行——Bulk 的泛化。op 数上限对齐 MaxBulkOperations（app 层校验）。

// MaxTransactionOps 是单事务 op 批的条数上限（与 Bulk 写入上限同值同源；
// app/documents.MaxBulkOperations 与 infra 执行器共同引用本常量——infra
// 不得 import app，共享上限只能放端口层）。
const MaxTransactionOps = 1000

// TransactionOpType 是 op 的操作类型。
type TransactionOpType string

const (
	TransactionOpCreate TransactionOpType = "create"
	TransactionOpUpdate TransactionOpType = "update"
	TransactionOpUpsert TransactionOpType = "upsert"
	TransactionOpDelete TransactionOpType = "delete"
)

// TransactionMode 是批执行模式：ATOMIC（默认）任一失败整批回滚；
// PARTIAL 逐 op 独立执行（savepoint），已成功不回滚，返回 per-op 结果。
type TransactionMode string

const (
	TransactionModeAtomic  TransactionMode = "atomic"
	TransactionModePartial TransactionMode = "partial"
)

// TransactionOp 是单个事务操作（复用旧 document_transaction_ops 字段族；
// 单 database 批——database 在请求级）。
type TransactionOp struct {
	Type         TransactionOpType
	CollectionID string
	DocumentID   string
	Data         map[string]any
	Permissions  []Permission
	Increment    map[string]int64
	// ArrayUpdates 是数组列原子更新（转出 POC B1）：仅 update op 消费，
	// 语义与 DocumentUpdate.ArrayUpdates 同源（buildArrayParts 单语句 SET）。
	ArrayUpdates    map[string]ArrayUpdate
	ExpectedVersion *int64 // 设置 → CAS；未设置 → 盲写 +1（LWW，仅 update）；0 → InvalidArgument
	ConflictColumns []string
}

// TransactionOpResult 是单个 op 的执行结果（PARTIAL 模式携带失败项；
// ATOMIC 失败走整批 gRPC 错误）。
type TransactionOpResult struct {
	Index      int
	OK         bool
	DocumentID string
	Version    int64
	ErrCode    string
	ErrMessage string
}
