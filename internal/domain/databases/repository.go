package databases

import (
	"context"
	"time"
)

// Catalog 是只读 catalog：不跑 DDL、不 Apply 项目迁移。
type Catalog interface {
	GetDatabase(ctx context.Context, projectID, id string) (*Database, error)
	ListDatabases(ctx context.Context, projectID string) ([]Database, error)
	GetCollection(ctx context.Context, projectID, databaseID, collectionID string) (*Collection, error)
	ListCollections(ctx context.Context, projectID, databaseID string, q ListQuery) ([]Collection, ListMeta, error)
}

// SchemaApplier 变更 catalog / 集合 DDL。EnsureCatalog 是读旁路唯一允许
// projectschema.Apply 的出口（启动 / 建项）；GetCollection 禁止调用。
type SchemaApplier interface {
	CreateDatabase(ctx context.Context, projectID, id, name string) error
	DeleteDatabase(ctx context.Context, projectID, id string) error
	CreateCollection(ctx context.Context, projectID, databaseID, collectionID, name string, attrs []Attribute, idxs []Index, perms []Permission, documentSecurity bool) error
	UpdateCollection(ctx context.Context, projectID, databaseID, collectionID string, patch CollectionPatch) error
	DeleteCollection(ctx context.Context, projectID, databaseID, collectionID string) error
	CreateAttribute(ctx context.Context, projectID, databaseID, collectionID string, attr Attribute) error
	DeleteAttribute(ctx context.Context, projectID, databaseID, collectionID, key string) error
	CreateIndex(ctx context.Context, projectID, databaseID, collectionID string, idx Index) error
	DeleteIndex(ctx context.Context, projectID, databaseID, collectionID, indexID string) error
	EnsureCatalog(ctx context.Context, projectID string) error
}

// Documents 是文档 CRUD。Principal 是 ACL 主体（本波不去掉）。
// List 必须 SQL 下推 _perms，不得改成 fetch-then-Check。
type Documents interface {
	CreateDocument(ctx context.Context, projectID, databaseID, collectionID string, doc Document, perms []Permission, principal Principal) (Document, error)
	// UpsertDocument inserts doc, or when a row already matches the conflict
	// columns, updates its data, _updated_at, _updated_by and replaces its
	// document permissions with perms. conflictColumns must match a unique
	// index on the collection table.
	UpsertDocument(ctx context.Context, projectID, databaseID, collectionID string, doc Document, conflictColumns []string, perms []Permission, principal Principal) (Document, error)
	GetDocument(ctx context.Context, projectID, databaseID, collectionID, docID string, principal Principal) (*Document, error)
	UpdateDocument(ctx context.Context, projectID, databaseID, collectionID string, update DocumentUpdate, principal Principal) (Document, error)
	DeleteDocument(ctx context.Context, projectID, databaseID, collectionID, docID string, opts DeleteOptions, principal Principal) error
	ListDocuments(ctx context.Context, projectID, databaseID, collectionID string, q Query, principal Principal) (*DocumentList, error)
	CountDocuments(ctx context.Context, projectID, databaseID, collectionID string, q Query, principal Principal) (int64, error)
	// AggregateDocuments 在权限过滤后的可见行集上执行聚合（redesign §4.1 +
	// §11-J D1：过滤链先于 GROUP BY，不可见行不进聚合、键不泄露）。
	// 聚合目标必须是声明的数值属性（integer/float）；groupBy 为可选单键，
	// 须为已声明属性。空集：sum=0，avg/min/max 无值（Value=nil）。
	AggregateDocuments(ctx context.Context, projectID, databaseID, collectionID string, q Query, aggs []AggregateSpec, groupBy string, principal Principal) ([]AggregateGroup, error)
	BulkUpdateDocuments(ctx context.Context, projectID, databaseID, collectionID string, documentIDs []string, data map[string]any, perms []Permission, principal Principal) (int64, error)
	BulkDeleteDocuments(ctx context.Context, projectID, databaseID, collectionID string, documentIDs []string, principal Principal) (int64, error)
	// ExecuteTransactions 在单事务内顺序执行异构 op 批（事务内核 Phase 1，
	// redesign §4.8）：按 (_tenant, doc) 排序预取 advisory 锁防批间死锁，
	// 事件同事务且顺序 = op 序；ATOMIC 任一失败整批回滚（返回带 op index
	// 的错误），PARTIAL 逐 op savepoint 容错、已成功不回滚。
	ExecuteTransactions(ctx context.Context, projectID, databaseID string, ops []TransactionOp, mode TransactionMode, principal Principal) ([]TransactionOpResult, error)
}

// DocumentChange 是一条已提交的文档写事件（:changes / last_seq 重放的
// 出站形态，阶段④ §4.5）：seq 升序、按请求者可见性过滤；delete 事件
// Data 为 nil（天然 tombstone：document_id + version 标识删除）。
type DocumentChange struct {
	Seq           int64
	EventID       string
	Event         string
	DocumentID    string
	Version       int64
	TransactionID string // execute-tx 批标识（单文档路径为空）
	CreatedAt     time.Time
	Truncated     bool
	Data          *Document
}

// ListChangesOptions 是变更流查询参数。
type ListChangesOptions struct {
	// SinceSeq 是续传游标：返回 seq > SinceSeq 的已提交事件。0 = 从最老
	// 可用事件起（缺省，不判过期）；> 0 且早于该集合最老可用事件 →
	// ErrResumeExpired（增量不完整，指引全量重拉）。
	SinceSeq int64
	// DocumentID 非空时仅返回该文档的事件（WS 文档频道重放用）。
	DocumentID string
	// Limit 是返回条数上限（1..500，0 = 默认 500；超过上限按上限截断）。
	Limit int
}

// ChangeFeed 是事件补偿读取端口（阶段④ §4.5）：从 outbox 表读某集合的
// 已提交事件，按请求者可见性过滤（快照 ACL + 当前 principal，与 hub
// 扇出同语义）。返回 seq 升序的可见事件、has_more 与续传游标
// nextSinceSeq（R15 两级语义）：
//   - (a) 收满 limit+1 退出（可见事件充足）：nextSinceSeq = 末条*返回*
//     事件 seq，续传首条恰为第 limit+1 条；
//   - (b) 扫描上限退出（连续不可见块触顶）：nextSinceSeq = 内部扫描
//     位置（越过已判不可见的块），has_more=true；
//   - 自然耗尽：has_more=false、nextSinceSeq=0。
//
// 调用方续传优先使用 nextSinceSeq，仅当为 0 时回退末条事件 seq。
type ChangeFeed interface {
	ListChanges(ctx context.Context, projectID, databaseID, collectionID string, opts ListChangesOptions, principal Principal) (changes []DocumentChange, hasMore bool, nextSinceSeq int64, err error)
}

// DocumentDB 嵌入四端口，现有注入点多数不用改签名。
type DocumentDB interface {
	Catalog
	SchemaApplier
	Documents
	ChangeFeed
}

// SchemaEvolution 是 schema 演进生命周期的可选端口（转出 POC 门禁 B4，
// redesign §4.6）：以可选接口（非 DocumentDB 嵌入）演进，测试替身按需实现，
// 消费方经类型断言探测（对齐 InternalIDCacheInvalidator 先例）。
//
// 删列两段契约：DeleteAttribute（SchemaApplier 既有方法，语义升级）= 段一
// deprecated（读屏蔽写拒收，可回滚）；RetireAttribute = 段二物理删列（不可逆）；
// RestoreAttribute = deprecated/migrating 回滚 active。
type SchemaEvolution interface {
	// MigrateAttribute 创建 copy 迁移任务（改类型/收紧）：新列（物理名带版本
	// 后缀）→ 异步批量回填（批 500 行、限速、游标可恢复）→ 锁窗校验 → 原子
	// swap → 旧列 deprecated；schema_version 在 swap commit 递增。迁移期间该
	// 属性写入拒收（CATALOG.ATTRIBUTE_MIGRATING）；同 key 已有 backfilling
	// 任务时重入（从游标续跑）。放宽（扩宽/required→optional）即时 ALTER。
	MigrateAttribute(ctx context.Context, projectID, databaseID, collectionID, key string, target Attribute) (*AttributeMigration, error)
	// RetireAttribute 是删列段二：deprecated 属性的物理列 / swap 后迁移残留
	// 的旧列（latest swapped 任务）物理删除，不可逆。
	RetireAttribute(ctx context.Context, projectID, databaseID, collectionID, key string) error
	// RestoreAttribute 回滚：deprecated → active；migrating → 中止迁移
	//（删除新列、任务置 failed）并恢复 active。
	RestoreAttribute(ctx context.Context, projectID, databaseID, collectionID, key string) error
}

// AttributeMigration 是 copy 迁移任务的读回形态（MigrateAttribute 响应）。
type AttributeMigration struct {
	ID            string
	AttrKey       string
	Phase         string // backfilling | swapped | retired | failed
	OldPhysical   string // swap 后旧列物理名（retire 的 DROP 目标；机密细节不出 API）
	NewPhysical   string
	RowsDone      int64
	SchemaVersion int64
}

var (
	_ Catalog       = (DocumentDB)(nil)
	_ SchemaApplier = (DocumentDB)(nil)
	_ Documents     = (DocumentDB)(nil)
	_ ChangeFeed    = (DocumentDB)(nil)
)
