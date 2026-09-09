package databases

import (
	"time"

	"github.com/torchwoodcloud/torchwood/pkg/query"
)

type Attribute struct {
	ID       string
	Key      string
	Type     string // string, integer, float, boolean, datetime, email, url, json, vector
	Size     int
	Required bool
	Default  any
	Array    bool
	// Dims 是 vector 属性的维度（会话 #10）：仅 type=vector 非零，
	// 合法域 2..2000（pgvector 可索引上限）。维度变更 = 新列 + 数据重灌
	//（换模型即换列名，不走 schema 演进状态机）。
	Dims    int
	Options map[string]any
	// Status 是 schema 演进两段删列/迁移的生命周期状态（转出 POC B4，§4.6）：
	// active（可用）| deprecated（读屏蔽写拒收，可回滚）| retired（物理已删，
	// 不可逆）。空串 = active——存量 catalog 行零迁移。
	Status string
}

// 属性生命周期状态值（catalog attrs JSONB 的 status 字段，codec omitempty）。
const (
	AttrStatusActive     = "active"
	AttrStatusMigrating  = "migrating"
	AttrStatusDeprecated = "deprecated"
	AttrStatusRetired    = "retired"
)

// StatusOrDefault 归一属性状态：空串（存量行缺省形态）视为 active。
func (a Attribute) StatusOrDefault() string {
	if a.Status == "" {
		return AttrStatusActive
	}
	return a.Status
}

type Index struct {
	ID         string
	Type       string // key, unique, fulltext, hnsw
	Attributes []string
	Orders     []string
	// DistanceMetric 是 hnsw 索引的距离度量（会话 #10）：COSINE | L2 |
	// INNER_PRODUCT。仅 hnsw 类型非空；缺省 COSINE（app 层归一）。
	DistanceMetric string
	// Status 是在线 DDL 两阶段状态机的索引状态（转出 POC B3，redesign §2-C5）：
	// active（可用）| building（CONCURRENTLY 构建中）| failed（构建失败，残留
	// 已清理，可重入）。空串 = active——建集合时的既有索引（新表无并发读者，
	// 事务内建）与 B3 之前的存量 catalog 行均落缺省形态。
	Status string
}

// 索引两阶段状态机状态值（catalog indexes JSONB 的 status 字段，codec omitempty
// ——active 缺省不落盘，存量行零迁移）。
const (
	IndexStatusActive   = "active"
	IndexStatusBuilding = "building"
	IndexStatusFailed   = "failed"
)

// StatusOrDefault 归一索引状态：空串（存量行/事务内建的缺省形态）视为 active。
func (i Index) StatusOrDefault() string {
	if i.Status == "" {
		return IndexStatusActive
	}
	return i.Status
}

type Permission struct {
	Type string // read, create, update, delete
	Role string // any, users, user:{id}, keys, admin, group:{id}, ...
}

type Document struct {
	ID          string
	Tenant      int64
	Data        map[string]any
	Permissions []Permission
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CreatedBy   string
	UpdatedBy   string
	Version     int64 // 顶层；用户 collection 的 OCC `_version`；系统资源不经本字段；不进 Data
}

type Query struct {
	// Queries 是 DSL 字符串遗留字段：文档查询栈（List/Count/Aggregate）已单
	// AST 化不再消费；仅供边界邻居面（users/buckets/files 等系统静态表
	// listing）携带，勿在新代码使用。
	Queries   []string
	PageSize  int32
	PageToken string
	// AST is the typed query（C7 单一消费形态）。
	AST *query.Query
}

// ListQuery carries pagination parameters for collection listing.
type ListQuery struct {
	PageSize  int32
	PageToken string
}

// ListMeta reports pagination metadata for collection listing.
type ListMeta struct {
	TotalCount    int64
	NextPageToken string
}

type CollectionPatch struct {
	Name             string
	Permissions      *[]Permission
	DocumentSecurity *bool
	Disabled         *bool
}

// 数组列原子更新算子（阶段③-b §10.5 P0 写侧；Intersect/Diff/Insert/Filter
// 补齐于转出 POC B1）。查询侧不对应对齐：Appwrite 的 arrayIntersect/arrayDiff
// 是写侧算子，查询布尔谓词由 containsAny/containsAll 承担（无新增查询算子）。
const (
	ArrayUpdateOpAppend    = "append"
	ArrayUpdateOpPrepend   = "prepend"
	ArrayUpdateOpRemove    = "remove"
	ArrayUpdateOpUnique    = "unique"
	ArrayUpdateOpIntersect = "intersect"
	ArrayUpdateOpDiff      = "diff"
	ArrayUpdateOpInsert    = "insert"
	ArrayUpdateOpFilter    = "filter"
)

// ArrayUpdate 是单个数组列的原子更新（编译为单语句 SET 子句，与 data/
// increment 可组合）。APPEND/PREPEND/REMOVE/INTERSECT/DIFF/FILTER 要求
// Values >= 1；INSERT 要求 Values 恰 1；UNIQUE 忽略 Values。仅 array=true
// 属性可用（adapter 按 catalog attrs 校验）。
type ArrayUpdate struct {
	Op     string
	Values []string
	// Index 是 INSERT 的插入位置（0 基，Appwrite 对齐；越界 = 尾插）。
	// nil = 未设置（INSERT → InvalidArgument；其余 op 忽略）。
	Index *int32
}

type DocumentUpdate struct {
	Document    Document
	Permissions []Permission
	Increment   map[string]int64
	// ArrayUpdates 是数组列原子更新（阶段③-b）：键为属性 key，与 Document.Data
	// 同列冲突由 adapter 拒绝（同一 SET 子句内同列双赋值歧义）。
	ArrayUpdates map[string]ArrayUpdate
	// ExpectedVersion：用户集合且 !SkipVersion 时必填，须等于当前行 _version。
	ExpectedVersion int64
	// SkipVersion：Bulk 内部循环、Upsert 更新支专用。仍执行 _version = _version + 1。
	SkipVersion bool
}

// DeleteOptions 携带 DeleteDocument 的 OCC 参数。
type DeleteOptions struct {
	ExpectedVersion int64
	SkipVersion     bool
}

// ReservedAttributeKeys 是禁止作为用户属性的系统列（含 _version/_acl）。
// ValidateIdentifier 允许 "_" 前缀，必须在属性创建路径显式拒绝。
var ReservedAttributeKeys = map[string]struct{}{
	"_id": {}, "_tenant": {}, "_created_at": {}, "_updated_at": {},
	"_created_by": {}, "_updated_by": {}, "_version": {}, "_perms": {}, "_acl": {},
}

type DocumentList struct {
	Documents     []Document
	TotalCount    int64
	NextPageToken string
	// Distances 是 KNN 查询的距离回传（会话 #10 预决策 4）：与 Documents
	// 平行，仅 vector_search 查询时非空。不污染 Document.Data、不持久化、
	// 不进事件。
	Distances []float64
}

type Collection struct {
	ID               string
	DatabaseID       string
	ProjectID        string
	Name             string
	DocumentSecurity bool
	Disabled         bool
	IsSystem         bool
	Permissions      []Permission
	Attributes       []Attribute
	Indexes          []Index
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Database 是 catalog 中的库行，不再复用 Collection。
type Database struct {
	ID        string
	Name      string
	ProjectID string
	CreatedAt time.Time
	UpdatedAt time.Time
}
