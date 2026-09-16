package projects

import (
	"context"
	"time"
)

// RuntimeVars 可见性（runtime_var_sets.visibility，docs/design/runtime-vars.md §2.4）。
// 可见性是数据级属性：private 集 = 项目用户可见、路人不可见；client 面的
// 过滤判定下推到 RuntimeVarPublicRead 实现（接口上不存在未过滤读）。
const (
	VarSetVisibilityPublic  = "public"
	VarSetVisibilityPrivate = "private"
)

// RuntimeVar 值类型（runtime_vars.value_type，D3 typed oneof 的存储锚点列）。
const (
	RuntimeVarTypeString  = "string"
	RuntimeVarTypeInteger = "integer"
	RuntimeVarTypeFloat   = "float"
	RuntimeVarTypeBoolean = "boolean"
	RuntimeVarTypeJSON    = "json"
)

// 版本链动作（runtime_var_versions.action）。集合元数据（visibility/
// description）变更不 bump revision、不入版本链（D10）。
const (
	RuntimeVarActionCreate   = "create"
	RuntimeVarActionUpdate   = "update"
	RuntimeVarActionDelete   = "delete"
	RuntimeVarActionRollback = "rollback"
)

// VarSet 是一个运行时变量集合（runtime_var_sets 行）。epoch 创建时随机
// （8 字节 hex），是对外 etag "{epoch}:{revision}" 的防碰撞因子：同名删除
// 重建后 revision 归零，epoch 使持久化了旧 etag 的客户端必然全量刷新（D2）。
type VarSet struct {
	ProjectID   string
	VarSetID    string
	Visibility  string
	Epoch       string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RuntimeVar 是单个类型化变量（runtime_vars 行）。Value 为标准化 JSON 文本
// （标量即 JSON 标量，如 `"hello"` / `42` / `true` / `{"a":1}`），ValueType
// 是类型锚点（D3）；类型创建时锁定、变更走显式 Update。
type RuntimeVar struct {
	ProjectID   string
	VarSetID    string
	Key         string
	ValueType   string
	Value       string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RuntimeVarVersion 是版本链中的一版（runtime_var_versions 行）。Vars 为写后
// DB 状态回读构造的全量快照 JSON 文本 {key: {t, v, d, c}}（全列四元组，D11；
// t=value_type、v=value、d=description、c=created_at 的 RFC3339 文本）。
// 快照从写后 DB 回读构造，不以内存态为准（§2.5）。
type RuntimeVarVersion struct {
	ProjectID string
	VarSetID  string
	Revision  int64
	Vars      string
	Action    string
	Summary   string
	Actor     string
	CreatedAt time.Time
}

// RuntimeVarRepository 是 server 面（Console / Server API）的 RuntimeVars
// 仓储端口。写方法感知调用方事务（ctx 携带事务时并入，见 clients.WithTx）；
// var 写与版本链写入由 app 用例层在单事务内编排，规范调用序（淘汰窗口必须
// 计入本次新版本行）：LockHead（FOR UPDATE）→ 变更 vars 行 → 快照回读 →
// InsertVersion（revision = LockHead 返回值 +1）→ BumpRevisionAndPrune（§2.5，
// 详见 BumpRevisionAndPrune 的调用序注释）。
type RuntimeVarRepository interface {
	// CreateVarSet 创建集合，同事务建立 heads 行（revision=0）；epoch 由
	// 调用方生成传入。(project_id, var_set_id) 冲突返回错误（app 层映射 409）。
	CreateVarSet(ctx context.Context, vs *VarSet) error
	// GetVarSet 单查集合；不存在返回 (nil, nil)。
	GetVarSet(ctx context.Context, projectID, varSetID string) (*VarSet, error)
	// ListVarSets 按项目列出集合（created_at DESC，var_set_id 决胜稳定排序）。
	ListVarSets(ctx context.Context, projectID string) ([]VarSet, error)
	// UpdateVarSet 只 SET cols 白名单列（visibility/description/updated_at），
	// 禁止整行覆盖；var_set_id/epoch/created_at 不可经此通道修改。集合元数据
	// 变更不 bump revision、不入版本链（D10）。
	UpdateVarSet(ctx context.Context, projectID, varSetID string, cols map[string]any) error
	// DeleteVarSet 删除集合；vars/heads/versions 经 FK CASCADE 连带销毁
	//（全部历史不可恢复，Console 二次确认警示）。
	DeleteVarSet(ctx context.Context, projectID, varSetID string) error

	// GetHead 读取集合当前 revision（无锁）。heads 行与集合同生共死，
	// 0 行（found=false）= 集合不存在。
	GetHead(ctx context.Context, projectID, varSetID string) (revision int64, found bool, err error)
	// LockHead 以 SELECT ... FOR UPDATE 锁 heads 行并读取当前 revision，返回
	// (revision, found, err)；0 行（found=false）= 集合不存在（DeleteVarSet
	// 竞态下防误当成功，app 层映射 NotFound）。必须在写事务内调用：锁持有至
	// 事务提交/回滚，串行化同集合并发写（§2.5）。
	LockHead(ctx context.Context, projectID, varSetID string) (revision int64, found bool, err error)
	// BumpRevisionAndPrune 将 heads.revision 原子 +1 并淘汰版本窗口外最老
	// 版本（保留最近 50 版/集合，D11），返回新 revision（= 本次写产生的版本
	// 行寻址号）。必须与同一写事务内的 LockHead 配对调用（行已持锁）。
	// 写事务内的规范调用序（淘汰窗口必须计入本次新版本行）：LockHead →
	// 变更 vars 行 → InsertVersion（revision = LockHead 返回值 +1）→
	// BumpRevisionAndPrune（bump 至同一 revision 并淘汰；同一事务内语句序
	// 对外不可见，与设计 §2.5「bump → 快照 → 插入 → 淘汰」的提交语义等价）。
	BumpRevisionAndPrune(ctx context.Context, projectID, varSetID string) (newRevision int64, err error)

	// CreateRuntimeVar 新建变量行（不做 Upsert，D5；主键冲突返回错误）。
	CreateRuntimeVar(ctx context.Context, v *RuntimeVar) error
	// GetRuntimeVar 单查变量；不存在返回 (nil, nil)。
	GetRuntimeVar(ctx context.Context, projectID, varSetID, key string) (*RuntimeVar, error)
	// ListRuntimeVars 按集合全量列出变量（key ASC）；server 面与快照回读共用。
	ListRuntimeVars(ctx context.Context, projectID, varSetID string) ([]RuntimeVar, error)
	// UpdateRuntimeVar 只 SET cols 白名单列（value/value_type/description/
	// updated_at），禁止整行覆盖；key/created_at 不可经此通道修改。value 是
	// 完整新值（JSON 文本），类型可随本次变更（与 value_type 同步写）。
	UpdateRuntimeVar(ctx context.Context, projectID, varSetID, key string, cols map[string]any) error
	// DeleteRuntimeVar 删除单个变量行。
	DeleteRuntimeVar(ctx context.Context, projectID, varSetID, key string) error

	// Footprint 返回集合当前尺寸：(var 行数, value 列 JSON 文本字节数总和)。
	// 限额口径（D13：vars ≤500/集合、总值 ≤1MB）的度量来源。
	Footprint(ctx context.Context, projectID, varSetID string) (count int, totalBytes int64, err error)
	// ReplaceVars 回滚整替：删除集合全部 var 行后按快照重插（§2.5）。输入行
	// 的 key/value_type/value/description/created_at 取快照值；updated_at 由
	// 实现统一置为回滚时刻（同一整替全部行取同一时刻）——回滚是一次新的写。
	// 必须在写事务内与 LockHead/BumpRevisionAndPrune 同事务调用。
	ReplaceVars(ctx context.Context, projectID, varSetID string, vars []RuntimeVar) error

	// InsertVersion 追加版本行（revision = 同事务 LockHead 返回值 +1；vars
	// 为写后 DB 回读的全量快照 JSON 文本，见 BumpRevisionAndPrune 的规范
	// 调用序）。
	InsertVersion(ctx context.Context, v *RuntimeVarVersion) error
	// GetVersion 单查版本（含快照全文）；不存在（含已被窗口淘汰）返回
	// (nil, nil)，app 层统一映射 NotFound "target revision pruned"。
	GetVersion(ctx context.Context, projectID, varSetID string, revision int64) (*RuntimeVarVersion, error)
	// ListVersions 按集合列出版本行（revision DESC，最新在前；含 vars 全文，
	// app 层对 ListRuntimeVarVersions 按需裁剪为仅元数据）。
	ListVersions(ctx context.Context, projectID, varSetID string) ([]RuntimeVarVersion, error)
}

// RuntimeVarPublicRead 是 client 面（匿名拉取端点）专用的窄端口：接口上
// 不存在未过滤读——可见性过滤（private 拒绝）做进实现，未来新增读路径或
// wire 误接线为编译错误（对抗审查修正，SettingsWriter 同模式，§2.4/§2.6）。
type RuntimeVarPublicRead interface {
	// GetVisibleVars 返回集合的可见全量快照。principalAllowed = 调用方凭证
	// 布尔（有有效 Principal 且项目匹配，app 用例层判定）。返回约定：
	//   - public 集合恒可见；private 集合仅 principalAllowed 时可见；
	//   - 可见时返回全量 vars（无变量时为空切片）+ epoch + 当前 revision；
	//   - private 且 !principalAllowed、或集合不存在 → (nil, "", 0, nil)
	//（三种情况合并，对匿名探测者不可区分，不确认私有集合存在）。
	// revision 与 vars 的读取单条 JOIN 完成（Read Committed 下消除两读间
	// 写入提交导致的 etag/内容瞬时错配，§2.4）。
	GetVisibleVars(ctx context.Context, projectID, varSetID string, principalAllowed bool) (vars []RuntimeVar, epoch string, revision int64, err error)
}
