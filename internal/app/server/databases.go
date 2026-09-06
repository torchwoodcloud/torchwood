package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/torchwooddev/torchwood/internal/app/documents"
	"github.com/torchwooddev/torchwood/internal/app/shared"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/pkg/ident"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var identifierRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// collectionIDRe 收紧集合 ID 为小写（2026-09-06 勘误：物理表名 = collectionID，
// 阶段②随机物理名退役；小写使 psql/pg_dump 等运维路径免引号直用）。attribute
// key 等其余标识符不受此限，仍走 identifierRe。
var collectionIDRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// 标识符长度上限（POC 期入口治理，封死 PG 63 字节截断把两个仅超长部分不同
// 的名字映射到同一物理对象的问题）。
const (
	// maxCollectionIDLen 约束物理表名（= collectionID），并为索引名
	// idx_<coll>_<id> / idx_<coll>_tenant_created 的前缀段留出预算。
	maxCollectionIDLen = 40
	// maxAttributeKeyLen 对齐物理列名 63 字节上限。
	maxAttributeKeyLen = 63
	// maxIndexIDLen 约束索引名后缀段（组合长度另见 validateIndexNameLen）。
	maxIndexIDLen = 40
)

type Databases struct {
	projectRepo projects.Repository
	docDB       databases.DocumentDB
	idem        databases.IdempotencyStore
	docs        *documents.Documents
}

func NewDatabases(projectRepo projects.Repository, docDB databases.DocumentDB, idem databases.IdempotencyStore) *Databases {
	return &Databases{projectRepo: projectRepo, docDB: docDB, idem: idem, docs: documents.New(docDB, idem)}
}

func (d *Databases) documentsCore() *documents.Documents {
	if d.docs != nil {
		return d.docs
	}
	return documents.New(d.docDB, d.idem)
}

func allowPrivilegedGrant(principal databases.Principal) bool {
	return principal.PlatformAdmin || principal.HasRole("keys")
}

func (d *Databases) resolveProject(ctx context.Context, projectID string) (*projects.Project, error) {
	p, err := d.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	return p, nil
}

func (d *Databases) CreateDatabase(ctx context.Context, projectID, id, name string) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(id); err != nil {
		return err
	}
	if name == "" {
		return status.Error(codes.InvalidArgument, "name is required")
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	return d.docDB.CreateDatabase(ctx, projectID, id, name)
}

func (d *Databases) ListDatabases(ctx context.Context, projectID string) ([]databases.Database, error) {
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	list, err := d.docDB.ListDatabases(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]databases.Database, 0, len(list))
	for _, db := range list {
		if db.ID == ident.ProjectDataPlaneID {
			continue
		}
		out = append(out, db)
	}
	return out, nil
}

func (d *Databases) GetDatabase(ctx context.Context, projectID, databaseID string) (*databases.Database, error) {
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return nil, err
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	return d.docDB.GetDatabase(ctx, projectID, databaseID)
}

func (d *Databases) DeleteDatabase(ctx context.Context, projectID, databaseID string) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	return d.docDB.DeleteDatabase(ctx, projectID, databaseID)
}

func (d *Databases) CreateCollection(ctx context.Context, projectID, databaseID, collectionID, name string, attrs []databases.Attribute, idxs []databases.Index, perms []databases.Permission, documentSecurity bool) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if err := d.validateCollectionID(collectionID); err != nil {
		return status.Error(codes.InvalidArgument, "id is required")
	}
	if name == "" {
		return status.Error(codes.InvalidArgument, "name is required")
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	for _, attr := range attrs {
		if err := d.validateAttribute(attr); err != nil {
			return err
		}
	}
	// 每集合列数软预算（redesign §11-J H2）：PG 1600 列硬限留余量，建集合时
	// 一次性声明即前置拒绝（域码 CATALOG.COLUMN_LIMIT_EXCEEDED）。
	if len(attrs) > documents.MaxCollectionColumns {
		return shared.DomainStatusWithViolations(databases.ErrCodeColumnLimitExceeded,
			shared.FieldViolation{Field: "attributes",
				Description: fmt.Sprintf("collection defines %d attributes, exceeds the %d-column soft limit", len(attrs), documents.MaxCollectionColumns)})
	}
	for _, idx := range idxs {
		if err := d.ValidateIndex(idx); err != nil {
			return err
		}
		if err := validateIndexNameLen(collectionID, idx.ID); err != nil {
			return err
		}
	}
	if len(perms) == 0 {
		perms = databases.DefaultCollectionPermissions()
	}
	return d.docDB.CreateCollection(ctx, projectID, databaseID, collectionID, name, attrs, idxs, perms, documentSecurity)
}

// validateAttribute 拒绝系统保留列（含 _version/_acl——系统列不可为数组由此
// 保持），并校验数组属性元素类型（阶段③-b 预决策 1：array=true 已合法，元素
// 仅限标量子集；adapter 的 validateArrayAttribute 是第二道防线）。
// vector 属性（会话 #10 预决策 1）：dims 必填且 2..2000（pgvector 可索引
// 上限）；default_value 与 array=true 对 vector 拒绝（无向量缺省语义；
// vector[] 无对应物理形态）。
func (d *Databases) validateAttribute(attr databases.Attribute) error {
	if _, ok := databases.ReservedAttributeKeys[attr.Key]; ok {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("attribute key %q is reserved", attr.Key))
	}
	if attr.Array {
		switch strings.ToLower(attr.Type) {
		case "string", "integer", "float", "boolean", "datetime":
		default:
			return status.Error(codes.InvalidArgument, fmt.Sprintf(
				"attribute %q: array supports string, integer, float, boolean, datetime element types", attr.Key))
		}
	}
	if strings.ToLower(attr.Type) == "vector" {
		if attr.Dims < 2 || attr.Dims > 2000 {
			return status.Error(codes.InvalidArgument, fmt.Sprintf(
				"attribute %q: vector requires dims between 2 and 2000", attr.Key))
		}
		if attr.Default != nil {
			return status.Error(codes.InvalidArgument, fmt.Sprintf(
				"attribute %q: vector attributes do not support default_value", attr.Key))
		}
	} else if attr.Dims != 0 {
		return status.Error(codes.InvalidArgument, fmt.Sprintf(
			"attribute %q: dims is only valid for vector attributes", attr.Key))
	}
	return nil
}

func (d *Databases) ListCollections(ctx context.Context, projectID, databaseID string, q databases.ListQuery) ([]databases.Collection, int64, string, error) {
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return nil, 0, "", err
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return nil, 0, "", err
	}
	cols, meta, err := d.docDB.ListCollections(ctx, projectID, databaseID, q)
	if err != nil {
		return nil, 0, "", shared.MapDocumentDBError(err)
	}
	return cols, meta.TotalCount, meta.NextPageToken, nil
}

func (d *Databases) GetCollection(ctx context.Context, projectID, databaseID, collectionID string) (*databases.Collection, error) {
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return nil, err
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	return d.docDB.GetCollection(ctx, projectID, databaseID, collectionID)
}

func (d *Databases) DeleteCollection(ctx context.Context, projectID, databaseID, collectionID string) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	return d.docDB.DeleteCollection(ctx, projectID, databaseID, collectionID)
}

func (d *Databases) UpdateCollection(ctx context.Context, projectID, databaseID, collectionID string, patch databases.CollectionPatch, principal databases.Principal) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if err := d.validateCollectionID(collectionID); err != nil {
		return status.Error(codes.InvalidArgument, "collection_id is required")
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	if patch.Permissions != nil {
		if err := databases.ValidateGrantablePermissions(principal, *patch.Permissions, allowPrivilegedGrant(principal)); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	return d.docDB.UpdateCollection(ctx, projectID, databaseID, collectionID, patch)
}

func (d *Databases) CreateAttribute(ctx context.Context, projectID, databaseID, collectionID string, attr databases.Attribute) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if err := d.validateCollectionID(collectionID); err != nil {
		return status.Error(codes.InvalidArgument, "collection_id is required")
	}
	if err := d.ValidateIdentifier(attr.Key); err != nil {
		return status.Error(codes.InvalidArgument, "key is required")
	}
	if err := d.ValidateAttributeType(attr.Type); err != nil {
		return err
	}
	if err := d.validateAttribute(attr); err != nil {
		return err
	}
	attr.Type = strings.ToLower(attr.Type)
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	// 每集合列数软预算（redesign §11-J H2）：存量 attrs + 本次新增 ≤200
	//（域码 CATALOG.COLUMN_LIMIT_EXCEEDED）。集合不存在时放行交由 adapter
	// 报 NotFound，避免重复存在性语义。
	col, err := d.docDB.GetCollection(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return shared.MapDocumentDBError(err)
	}
	if col != nil && len(col.Attributes)+1 > documents.MaxCollectionColumns {
		return shared.DomainStatusWithViolations(databases.ErrCodeColumnLimitExceeded,
			shared.FieldViolation{Field: "key",
				Description: fmt.Sprintf("adding attribute %q would make %d attributes, exceeds the %d-column soft limit", attr.Key, len(col.Attributes)+1, documents.MaxCollectionColumns)})
	}
	return d.docDB.CreateAttribute(ctx, projectID, databaseID, collectionID, attr)
}

func (d *Databases) CreateIndex(ctx context.Context, projectID, databaseID, collectionID string, idx databases.Index) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if err := d.validateCollectionID(collectionID); err != nil {
		return status.Error(codes.InvalidArgument, "collection_id is required")
	}
	if err := d.ValidateIndex(idx); err != nil {
		return err
	}
	if err := validateIndexNameLen(collectionID, idx.ID); err != nil {
		return err
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	return d.docDB.CreateIndex(ctx, projectID, databaseID, collectionID, idx)
}

func (d *Databases) DeleteAttribute(ctx context.Context, projectID, databaseID, collectionID, key string) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if err := d.validateCollectionID(collectionID); err != nil {
		return status.Error(codes.InvalidArgument, "collection_id is required")
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	return d.docDB.DeleteAttribute(ctx, projectID, databaseID, collectionID, key)
}

func (d *Databases) DeleteIndex(ctx context.Context, projectID, databaseID, collectionID, indexID string) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if err := d.validateCollectionID(collectionID); err != nil {
		return status.Error(codes.InvalidArgument, "collection_id is required")
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	return d.docDB.DeleteIndex(ctx, projectID, databaseID, collectionID, indexID)
}

// schemaEvolution 探测可选的 schema 演进端口（B4：仅 postgres 适配器实现；
// 测试替身按需实现，未实现面 → Unimplemented）。
func (d *Databases) schemaEvolution() (databases.SchemaEvolution, error) {
	if evo, ok := d.docDB.(databases.SchemaEvolution); ok {
		return evo, nil
	}
	return nil, status.Error(codes.Unimplemented, "schema evolution is not supported by this backend")
}

// RetireAttribute 是删列两段的段二（B4 §4.6）：deprecated 属性物理删列
// （不可逆）；swap 后迁移残留旧列的退役同入口。
func (d *Databases) RetireAttribute(ctx context.Context, projectID, databaseID, collectionID, key string) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if err := d.validateCollectionID(collectionID); err != nil {
		return status.Error(codes.InvalidArgument, "collection_id is required")
	}
	if err := d.ValidateIdentifier(key); err != nil {
		return status.Error(codes.InvalidArgument, "key is required")
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	evo, err := d.schemaEvolution()
	if err != nil {
		return err
	}
	return evo.RetireAttribute(ctx, projectID, databaseID, collectionID, key)
}

// RestoreAttribute 回滚生命周期：deprecated → active；migrating → 中止迁移
// 并恢复 active（§4.6 可回滚语义）。
func (d *Databases) RestoreAttribute(ctx context.Context, projectID, databaseID, collectionID, key string) error {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if err := d.validateCollectionID(collectionID); err != nil {
		return status.Error(codes.InvalidArgument, "collection_id is required")
	}
	if err := d.ValidateIdentifier(key); err != nil {
		return status.Error(codes.InvalidArgument, "key is required")
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	evo, err := d.schemaEvolution()
	if err != nil {
		return err
	}
	return evo.RestoreAttribute(ctx, projectID, databaseID, collectionID, key)
}

// MigrateAttribute 创建 copy 迁移任务（B4 §4.6 收紧/改类型行）：目标定义
// 走 CreateAttribute 同款校验后交适配器（迁移分诊在 infra）。
func (d *Databases) MigrateAttribute(ctx context.Context, projectID, databaseID, collectionID, key string, target databases.Attribute) (*databases.AttributeMigration, error) {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return nil, err
	}
	if err := d.validateCollectionID(collectionID); err != nil {
		return nil, status.Error(codes.InvalidArgument, "collection_id is required")
	}
	if err := d.ValidateIdentifier(key); err != nil {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}
	if err := d.ValidateAttributeType(target.Type); err != nil {
		return nil, err
	}
	if err := d.validateAttribute(target); err != nil {
		return nil, err
	}
	target.Type = strings.ToLower(target.Type)
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return nil, shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	evo, err := d.schemaEvolution()
	if err != nil {
		return nil, err
	}
	return evo.MigrateAttribute(ctx, projectID, databaseID, collectionID, key, target)
}

func (d *Databases) ensureCollection(ctx context.Context, projectID, databaseID, collectionID string, principal databases.Principal) error {
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if err := d.ValidateIdentifier(collectionID); err != nil {
		return status.Error(codes.InvalidArgument, "collection_id is required")
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	// 禁止直接写入系统集合（项目数据面 sentinel），先于 GetCollection 拦截
	// （避免泄露集合存在性，安全评审 C1 第 1 层）。对外 database_id 已拒 `_`，
	// 本守卫是 adapter 绕过时的纵深防御。
	if databases.IsSystemCollection(projectID, databaseID, collectionID) {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	col, err := d.docDB.GetCollection(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return err
	}
	if col == nil {
		return status.Error(codes.NotFound, "collection not found")
	}
	if col.Disabled && !principal.IsSystem() && !principal.PlatformAdmin {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	return nil
}

func (d *Databases) ensureReadableCollection(ctx context.Context, projectID, databaseID, collectionID string, principal databases.Principal) error {
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return err
	}
	if err := d.ValidateIdentifier(collectionID); err != nil {
		return status.Error(codes.InvalidArgument, "collection_id is required")
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return err
	}
	col, err := d.docDB.GetCollection(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return err
	}
	if col == nil {
		return status.Error(codes.NotFound, "collection not found")
	}
	if col.Disabled && !principal.IsSystem() && !principal.PlatformAdmin {
		return shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	return nil
}

func (d *Databases) CreateDocument(
	ctx context.Context,
	projectID, databaseID, collectionID, documentID string,
	data map[string]any,
	perms []databases.Permission,
	principal databases.Principal,
	requestID string,
) (*databases.Document, bool, error) {
	if err := d.ensureCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return nil, false, err
	}
	// C1：Server 空文档 ACE 私有（不再回落到集合 read:any）。
	// WHY: 默认集合已去 read:any，但历史集合仍可能含它；空 ACE 若仍 docHasPerms=false 会对 guest 可读。
	// 修订（2026-08 回归）：占位 ACE 给创建者凭证角色保留读写删（与 user:
	// owner 分支同构）——原 read:__private__ 不匹配任何常规角色，keys 创建
	// 的文档自己都读不回，Server API 的创建→读改删往返整体断裂；guest/any
	// 仍被剔除，docHasPerms=true 依旧关闭集合回落，C1 目标不变。
	seeded := false
	if len(perms) == 0 {
		perms = seedDocumentPermissions(principal)
		seeded = true
	}
	// seeded 时 ACE 全部为系统推导（非调用方授予意图），不受授予者校验约束。
	return d.documentsCore().CreateDocument(ctx, projectID, databaseID, collectionID, documentID, data, perms, principal, requestID, documents.WriteOptions{
		AllowPrivilegedGrant: seeded || allowPrivilegedGrant(principal),
	})
}

// ownerUserRole 返回 principal 的首个 user:<id> 角色（owner ACE 语义），
// 无用户角色时返回空串。
func ownerUserRole(principal databases.Principal) string {
	for _, r := range principal.Roles {
		if strings.HasPrefix(r, "user:") {
			return r
		}
	}
	return ""
}

// seedDocumentPermissions 生成空 ACE 写入的创建者种子（CreateDocument /
// UpsertDocument 共用）：API key 主体 → key:<自身id>（B14 per-key 私有，与
// user 主体 owner ACE 同构）→ owner user 角色 → creatorSeedRole（admin 等
// 常规凭证角色）→ __private__ 纯私有标记。返回的 perms 恒非空。
func seedDocumentPermissions(principal databases.Principal) []databases.Permission {
	if principal.KeyID != "" {
		return creatorSeedPerms("key:" + principal.KeyID)
	}
	role := ownerUserRole(principal)
	if role == "" {
		role = creatorSeedRole(principal)
	}
	if role != "" {
		return creatorSeedPerms(role)
	}
	// 无常规角色可绑定（如仅特权旁路的主体）：纯私有标记。
	return []databases.Permission{{Type: "read", Role: "__private__"}}
}

// creatorSeedPerms 是创建者种子的 read/update/delete 三连 ACE（占位私有：
// 集合回落关闭，仅创建者身份可读写删——跨身份协作需显式授予 ACE）。
func creatorSeedPerms(role string) []databases.Permission {
	return []databases.Permission{
		{Type: "read", Role: role},
		{Type: "update", Role: role},
		{Type: "delete", Role: role},
	}
}

// creatorSeedRole 返回空 ACE 文档占位绑定用的创建者常规角色（首个非 user:
// 前缀、非合成的角色，如 admin）；找不到时返回空串。API key 主体不走本函数
// （B14 后种子绑 key:<id>，见 seedDocumentPermissions），存量非 key 主体
// （手工构造的无 KeyID keys 主体）维持绑 keys 的历史行为。
func creatorSeedRole(principal databases.Principal) string {
	for _, r := range principal.Roles {
		switch r {
		case "", "any", "guest", "__system__", "__private__":
			continue
		}
		if strings.HasPrefix(r, "user:") {
			continue
		}
		return r
	}
	return ""
}

func (d *Databases) ListDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	q databases.Query,
	principal databases.Principal,
) (*documents.ListDocumentsResult, error) {
	if err := d.ensureReadableCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return nil, err
	}
	return d.documentsCore().ListDocuments(ctx, projectID, databaseID, collectionID, q, principal)
}

func (d *Databases) GetDocument(
	ctx context.Context,
	projectID, databaseID, collectionID, documentID string,
	principal databases.Principal,
) (*databases.Document, error) {
	if err := d.ensureReadableCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return nil, err
	}
	return d.documentsCore().GetDocument(ctx, projectID, databaseID, collectionID, documentID, principal)
}

func (d *Databases) UpdateDocument(
	ctx context.Context,
	projectID, databaseID, collectionID, documentID string,
	data map[string]any,
	perms []databases.Permission,
	increment map[string]int64,
	arrayUpdates map[string]databases.ArrayUpdate,
	principal databases.Principal,
	version *int64,
	requestID string,
) (*databases.Document, bool, error) {
	if err := d.ensureCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return nil, false, err
	}
	return d.documentsCore().UpdateDocument(ctx, projectID, databaseID, collectionID, documentID, data, perms, increment, arrayUpdates, principal, version, requestID, documents.WriteOptions{
		AllowPrivilegedGrant: allowPrivilegedGrant(principal),
	})
}

func (d *Databases) UpsertDocument(
	ctx context.Context,
	projectID, databaseID, collectionID, documentID string,
	data map[string]any,
	conflictColumns []string,
	perms []databases.Permission,
	principal databases.Principal,
	requestID string,
) (*databases.Document, bool, error) {
	if err := d.ensureCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return nil, false, err
	}
	// 空 ACE 种子与 CreateDocument 同语义（seedDocumentPermissions）：原实现
	// 对非 user 主体种 read:__private__，keys 主体 upsert 后自己都读不回；
	// 更新支还会把目标行 ACL 整体替换为 __private__，直接锁死既有文档。
	seeded := false
	if len(perms) == 0 {
		perms = seedDocumentPermissions(principal)
		seeded = true
	}
	return d.documentsCore().UpsertDocument(ctx, projectID, databaseID, collectionID, documentID, data, conflictColumns, perms, principal, requestID, documents.WriteOptions{
		AllowPrivilegedGrant: seeded || allowPrivilegedGrant(principal),
	})
}

func (d *Databases) BulkUpdateDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	documentIDs []string,
	data map[string]any,
	perms []databases.Permission,
	principal databases.Principal,
	requestID string,
) (int64, bool, error) {
	if err := d.ensureCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return 0, false, err
	}
	return d.documentsCore().BulkUpdateDocuments(ctx, projectID, databaseID, collectionID, documentIDs, data, perms, principal, requestID, documents.WriteOptions{
		AllowPrivilegedGrant: allowPrivilegedGrant(principal),
	})
}

func (d *Databases) BulkDeleteDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	documentIDs []string,
	principal databases.Principal,
	requestID string,
) (int64, bool, error) {
	if err := d.ensureCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return 0, false, err
	}
	return d.documentsCore().BulkDeleteDocuments(ctx, projectID, databaseID, collectionID, documentIDs, principal, requestID)
}

func (d *Databases) DeleteDocument(
	ctx context.Context,
	projectID, databaseID, collectionID, documentID string,
	principal databases.Principal,
	version *int64,
	requestID string,
) (bool, error) {
	if err := d.ensureCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return false, err
	}
	return d.documentsCore().DeleteDocument(ctx, projectID, databaseID, collectionID, documentID, principal, version, requestID)
}

func (d *Databases) CountDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	q databases.Query,
	principal databases.Principal,
) (int64, error) {
	if err := d.ensureReadableCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return 0, err
	}
	return d.documentsCore().CountDocuments(ctx, projectID, databaseID, collectionID, q, principal)
}

// AggregateDocuments 聚合入口（redesign §4.1 + §10.5 P1）：读语义守卫与
// CountDocuments 同链；权限过滤聚合见 infra（D1）。
func (d *Databases) AggregateDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	q databases.Query,
	aggs []databases.AggregateSpec,
	groupBy string,
	principal databases.Principal,
) ([]databases.AggregateGroup, error) {
	if err := d.ensureReadableCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return nil, err
	}
	return d.documentsCore().AggregateDocuments(ctx, projectID, databaseID, collectionID, q, aggs, groupBy, principal)
}

// ListChanges 事件补偿入口（阶段④ §4.5）：读语义守卫与 ListDocuments 同链；
// 可见性过滤（快照 ACL + 当前 principal）在 infra ChangeFeed。返回续传游标
// nextSinceSeq（R15 两级语义：满页 = 末条返回 seq；扫描触顶 = 越过不可见
// 块的扫描位置）。
func (d *Databases) ListChanges(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	opts databases.ListChangesOptions,
	principal databases.Principal,
) ([]databases.DocumentChange, bool, int64, error) {
	if err := d.ensureReadableCollection(ctx, projectID, databaseID, collectionID, principal); err != nil {
		return nil, false, 0, err
	}
	changes, hasMore, nextSinceSeq, err := d.docDB.ListChanges(ctx, projectID, databaseID, collectionID, opts, principal)
	if err != nil {
		return nil, false, 0, shared.MapDocumentDBError(err)
	}
	return changes, hasMore, nextSinceSeq, nil
}

func (d *Databases) MapAttributeType(t string) string {
	return strings.ToLower(t)
}

// ExecuteTransactions 是 Server 面的事务内核入口（redesign §4.8 Phase 1）：
// 逐 op 集合守卫、create/upsert 空 ACE 种子（与单文档 API 同语义）、grant
// 展开校验，随后交 infra 单事务执行器。
func (d *Databases) ExecuteTransactions(
	ctx context.Context,
	projectID, databaseID string,
	ops []databases.TransactionOp,
	mode databases.TransactionMode,
	principal databases.Principal,
	requestID string,
) ([]databases.TransactionOpResult, bool, error) {
	if err := shared.RequireServerPrincipal(ctx); err != nil {
		return nil, false, err
	}
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return nil, false, err
	}
	if len(ops) == 0 {
		return nil, false, status.Error(codes.InvalidArgument, "ops is required")
	}
	if len(ops) > documents.MaxBulkOperations {
		return nil, false, status.Errorf(codes.InvalidArgument, "ops count %d exceeds maximum of %d", len(ops), documents.MaxBulkOperations)
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return nil, false, err
	}
	callerAllowed := allowPrivilegedGrant(principal)
	for i := range ops {
		op := &ops[i]
		// 守卫失败 → InvalidArgument + BadRequest violations（字段路径
		// ops[i].<sub>，redesign §4.1 机器可读定位）。
		violation := func(field, format string, args ...any) error {
			return shared.DomainStatusWithViolations(databases.ErrCodeInvalidArgument,
				shared.FieldViolation{Field: fmt.Sprintf("ops[%d].%s", i, field), Description: fmt.Sprintf(format, args...)})
		}
		switch op.Type {
		case databases.TransactionOpCreate, databases.TransactionOpUpdate, databases.TransactionOpUpsert, databases.TransactionOpDelete:
		default:
			return nil, false, violation("type", "invalid op type %q", op.Type)
		}
		if err := d.validateCollectionID(op.CollectionID); err != nil {
			return nil, false, violation("collection_id", "invalid collection id %q", op.CollectionID)
		}
		if databases.IsSystemCollection(projectID, databaseID, op.CollectionID) {
			return nil, false, violation("collection_id", "system collection %q is not writable via transactions", op.CollectionID)
		}
		if err := documents.ValidateDocumentPayload(op.Data); err != nil {
			return nil, false, violation("data", "%v", err)
		}
		// 数组原子更新上限（redesign §11-J H2 + 转出 POC B1）：进事务前拒绝；
		// 列白名单/同列冲突等语义校验在 adapter 依 catalog attrs 进行。
		if err := documents.ValidateArrayUpdates(op.ArrayUpdates); err != nil {
			return nil, false, violation("array_updates", "%v", err)
		}
		// create/upsert 空 ACE 种子与单文档 API 同语义（防锁死；update/delete 的
		// 空 permissions 语义是"不变更文档 ACL"，不种子）。
		// grant 豁免严格 per-op（Phase 1 裁决③）：种子 op 仅豁免自身——种子是
		// 系统推导、非调用方授予意图；豁免外溢到其他 op 会放行显式越权授予。
		opAllowed := callerAllowed
		if len(op.Permissions) == 0 && (op.Type == databases.TransactionOpCreate || op.Type == databases.TransactionOpUpsert) {
			op.Permissions = seedDocumentPermissions(principal)
			opAllowed = true
		}
		// _acl ACE 数上限（redesign §11-J H2）：种子 ≤3 条天然合法；显式
		// permissions 超限在进事务前拒绝（域码 DOCUMENT.ACL_TOO_LARGE）。
		if len(op.Permissions) > documents.MaxDocumentACL {
			return nil, false, shared.DomainStatusWithViolations(databases.ErrCodeACLTooLarge,
				shared.FieldViolation{Field: fmt.Sprintf("ops[%d].permissions", i),
					Description: fmt.Sprintf("permissions has %d access control entries, exceeds the %d-entry limit", len(op.Permissions), documents.MaxDocumentACL)})
		}
		perms, err := applyTxGrant(principal, op.Permissions, opAllowed)
		if err != nil {
			return nil, false, violation("permissions", "%v", err)
		}
		op.Permissions = perms
	}
	return d.documentsCore().ExecuteTransactions(ctx, projectID, databaseID, ops, mode, principal, requestID)
}

// applyTxGrant 是 op 级 grant 展开（空列表跳过校验，与 update 语义一致）。
func applyTxGrant(principal databases.Principal, perms []databases.Permission, allowPrivileged bool) ([]databases.Permission, error) {
	if len(perms) == 0 {
		return nil, nil
	}
	perms, err := documents.ApplyGrant(principal, perms, allowPrivileged)
	if err != nil {
		return nil, err
	}
	return perms, nil
}

func (d *Databases) ValidateIdentifier(id string) error {
	if id == "" {
		return status.Error(codes.InvalidArgument, "identifier is required")
	}
	if len(id) > maxAttributeKeyLen {
		return status.Errorf(codes.InvalidArgument, "identifier %q exceeds maximum length of %d", id, maxAttributeKeyLen)
	}
	if !identifierRe.MatchString(id) {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("identifier %q must match %s", id, identifierRe.String()))
	}
	return nil
}

// validateCollectionID 叠加集合 ID 专用约束：小写字符集（物理表名 =
// collectionID，2026-09-06 勘误）+ 长度上限（表名 + 索引名前缀段预算）。
func (d *Databases) validateCollectionID(id string) error {
	if id == "" {
		return status.Error(codes.InvalidArgument, "collection id is required")
	}
	if len(id) > maxCollectionIDLen {
		return status.Errorf(codes.InvalidArgument, "collection id %q exceeds maximum length of %d", id, maxCollectionIDLen)
	}
	if !collectionIDRe.MatchString(id) {
		return status.Errorf(codes.InvalidArgument,
			"collection id %q must match %s (lowercase: it doubles as the physical table name)", id, collectionIDRe.String())
	}
	return nil
}

// validateIndexNameLen 校验物理索引名 idx_<coll>_<id> 的拼接长度：静态上限
// （coll ≤40 + id ≤40）封不死组合（最长 85 字节 > PG 63），必须叠加本校验。
func validateIndexNameLen(collectionID, indexID string) error {
	if n := 4 + len(collectionID) + 1 + len(indexID); n > maxAttributeKeyLen {
		return status.Errorf(codes.InvalidArgument,
			"index name idx_%s_%s is %d bytes, exceeds the %d-byte identifier limit (shorten collection id or index id)",
			collectionID, indexID, n, maxAttributeKeyLen)
	}
	return nil
}

func (d *Databases) ValidateAttributeType(t string) error {
	if t == "" {
		return status.Error(codes.InvalidArgument, "type is required")
	}
	switch strings.ToLower(t) {
	case "string", "integer", "float", "boolean", "datetime", "email", "url", "json", "vector":
		return nil
	default:
		return status.Error(codes.InvalidArgument,
			fmt.Sprintf("invalid attribute type %q (allowed: string, integer, float, boolean, datetime, email, url, json, vector)", t))
	}
}

func (d *Databases) ValidateIndex(idx databases.Index) error {
	if err := d.ValidateIdentifier(idx.ID); err != nil {
		return status.Error(codes.InvalidArgument, "id is required")
	}
	if len(idx.ID) > maxIndexIDLen {
		return status.Errorf(codes.InvalidArgument, "index id %q exceeds maximum length of %d", idx.ID, maxIndexIDLen)
	}
	if idx.Type == "" {
		return status.Error(codes.InvalidArgument, "type is required")
	}
	switch strings.ToLower(idx.Type) {
	case "key", "unique", "fulltext", "hnsw":
	default:
		return status.Error(codes.InvalidArgument,
			fmt.Sprintf("invalid index type %q (allowed: key, unique, fulltext, hnsw)", idx.Type))
	}
	if len(idx.Attributes) == 0 {
		return status.Error(codes.InvalidArgument, "attributes is required")
	}
	for _, attr := range idx.Attributes {
		if err := d.ValidateIdentifier(attr); err != nil {
			return status.Error(codes.InvalidArgument, fmt.Sprintf("invalid index attribute %q", attr))
		}
	}
	// hnsw 索引（会话 #10 预决策 2）：单列；metric ∈ COSINE|L2|INNER_PRODUCT
	//（缺省 COSINE 归一——handler 落库前统一归一大写形态）；orders 无意义拒绝；
	// 非 hnsw 设置 metric 拒绝。vector 列约束（目标列必须是声明的 vector 属性）
	// 在 adapter 的 createCollectionIndex 二道防线（需 catalog attrs）。
	metric := idx.DistanceMetric
	if strings.ToLower(idx.Type) == "hnsw" {
		if len(idx.Attributes) != 1 {
			return status.Error(codes.InvalidArgument, "hnsw index requires exactly one attribute")
		}
		if len(idx.Orders) > 0 {
			return status.Error(codes.InvalidArgument, "hnsw index does not support orders")
		}
		if metric == "" {
			metric = "COSINE"
		}
	} else if metric != "" {
		return status.Error(codes.InvalidArgument, "distance_metric is only valid for hnsw indexes")
	}
	if metric != "" {
		switch strings.ToUpper(metric) {
		case "COSINE", "L2", "INNER_PRODUCT":
		default:
			return status.Error(codes.InvalidArgument, "distance_metric must be COSINE, L2, or INNER_PRODUCT")
		}
	}
	return nil
}
