package documents

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/torchwoodcloud/torchwood/internal/app/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MaxBulkOperations 是 Bulk 写入单次条数上限（A4），与 execute-tx op 批
// 上限同值同源（databases.MaxTransactionOps，端口层唯一常量）。
const MaxBulkOperations = databases.MaxTransactionOps

// 文档载荷上限（redesign §11-J H1）：请求载荷总量 1 MiB（对齐 Firestore 锚点）；
// 单属性值 256 KiB（与事件信封截断阈值 maxEnvelopeBytes 对齐——超限载荷即使
// 落库也会在 outbox 被截断，从源头拒绝）。域码 DOCUMENT.TOO_LARGE 随错误码
// 体系（redesign §4.1）正式化，现阶段以消息前缀承载。
const (
	MaxDocumentPayloadBytes  = 1 << 20
	MaxAttributePayloadBytes = 256 << 10
)

// 文档/集合结构上限族（redesign §11-J H2 决议值）：_acl ≤64 ACE（校验收敛
// app 层写路径——与种子/授予治理同层；RLS/adapter 不再设防，防御纵深已在
// 列授权与 tw_set_document_acl 函数通道）；数组属性 ≤1000 元素（data 通道
// 数组值 + array_updates 的 values；DDL 通道无此面——array=true 拒绝
// default_value）；每集合列数软限 200（PG 1600 列硬限留余量，DDL 前置拒绝）；
// object 嵌套 ≤8 层。
const (
	MaxDocumentACL       = 64
	MaxArrayElements     = 1000
	MaxCollectionColumns = 200
	MaxObjectDepth       = 8
)

// ValidateDocumentPayload 校验写入载荷大小与结构（Create/Update/Upsert/Bulk/
// execute-tx 共用）。Update 是部分更新，总量按本次提交的载荷计（合并后全量
// 在 infra 读回后自然受单属性与列宽约束）。超限/不可序列化 → InvalidArgument，
// 违规属性定位走 BadRequest violations（redesign §4.1，域码 TOO_LARGE/
// ATTRIBUTE_UNSERIALIZABLE）；数组元素数与 object 嵌套深度属同一结构尺寸族
// （redesign §11-J H2），复用 TOO_LARGE。
func ValidateDocumentPayload(data map[string]any) error {
	if len(data) == 0 {
		return nil
	}
	total := 0
	for k, v := range data {
		b, err := json.Marshal(v)
		if err != nil {
			return shared.DomainStatusWithViolations(databases.ErrCodeAttributeUnserializable,
				shared.FieldViolation{Field: "data." + k, Description: err.Error()})
		}
		if len(b) > MaxAttributePayloadBytes {
			return shared.DomainStatusWithViolations(databases.ErrCodeTooLarge,
				shared.FieldViolation{Field: "data." + k, Description: fmt.Sprintf("attribute %q is %d bytes, exceeds the %d-byte per-attribute limit", k, len(b), MaxAttributePayloadBytes)})
		}
		if n, ok := arrayValueLen(v); ok && n > MaxArrayElements {
			return shared.DomainStatusWithViolations(databases.ErrCodeTooLarge,
				shared.FieldViolation{Field: "data." + k, Description: fmt.Sprintf("attribute %q has %d array elements, exceeds the %d-element limit", k, n, MaxArrayElements)})
		}
		if err := checkObjectDepth(k, v, 1); err != nil {
			return err
		}
		total += len(b)
	}
	if total > MaxDocumentPayloadBytes {
		return shared.DomainStatusWithViolations(databases.ErrCodeTooLarge,
			shared.FieldViolation{Field: "data", Description: fmt.Sprintf("document payload is %d bytes, exceeds the %d-byte limit", total, MaxDocumentPayloadBytes)})
	}
	return nil
}

// arrayValueLen 返回数据通道数组值的元素数；非数组值返回第二返回值 false。
// 数组列的 JSON 反序列化产物为 []any，服务端构造路径（labels 等）为 []string。
func arrayValueLen(v any) (int, bool) {
	switch vv := v.(type) {
	case []any:
		return len(vv), true
	case []string:
		return len(vv), true
	}
	return 0, false
}

// checkObjectDepth 递归校验 object 嵌套 ≤ MaxObjectDepth 层（redesign §11-J
// H2）：顶层属性对象为第 1 层，map 计一层、数组透明（元素与所在层同深）；
// 超限 → InvalidArgument / DOCUMENT.TOO_LARGE。
func checkObjectDepth(field string, v any, depth int) error {
	switch vv := v.(type) {
	case map[string]any:
		if depth > MaxObjectDepth {
			return shared.DomainStatusWithViolations(databases.ErrCodeTooLarge,
				shared.FieldViolation{Field: "data." + field, Description: fmt.Sprintf("object nesting exceeds the %d-level limit", MaxObjectDepth)})
		}
		for _, e := range vv {
			if err := checkObjectDepth(field, e, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range vv {
			if err := checkObjectDepth(field, e, depth); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateACL 校验显式 permissions（_acl）的 ACE 数上限（redesign §11-J H2）。
// create/upsert 空 ACE 的系统种子（≤3 条）在包装层生成后进入本核，天然合法
// 无需豁免；模板展开（ExpandPermissionTemplates）为 1:1 替换不改变 ACE 数，
// 对请求侧计数即可。超限 → InvalidArgument / DOCUMENT.ACL_TOO_LARGE。
func validateACL(perms []databases.Permission) error {
	if len(perms) <= MaxDocumentACL {
		return nil
	}
	return shared.DomainStatusWithViolations(databases.ErrCodeACLTooLarge,
		shared.FieldViolation{Field: "permissions", Description: fmt.Sprintf("permissions has %d access control entries, exceeds the %d-entry limit", len(perms), MaxDocumentACL)})
}

// ValidateArrayUpdates 校验数组原子更新请求的 op 合法性与 values 元素数上限
// （redesign §11-J H2；列白名单/同列冲突等语义校验在 adapter 依 catalog attrs
// 进行）。UNIQUE 忽略 values，但同一请求通道统一封顶。键排序保证多键超限时
// 报错确定性。导出供 Server execute-tx 包装层 per-op 校验复用（转出 POC B1）。
func ValidateArrayUpdates(arrayUpdates map[string]databases.ArrayUpdate) error {
	keys := make([]string, 0, len(arrayUpdates))
	for k := range arrayUpdates {
		keys = append(keys, k)
	}
	for _, k := range sortedStrings(keys) {
		if n := len(arrayUpdates[k].Values); n > MaxArrayElements {
			return shared.DomainStatusWithViolations(databases.ErrCodeTooLarge,
				shared.FieldViolation{Field: "array_updates." + k, Description: fmt.Sprintf("array update %q has %d values, exceeds the %d-element limit", k, n, MaxArrayElements)})
		}
	}
	return nil
}

// WriteOptions 是 Client/Server 策略投影传给文档核的授权差异。
type WriteOptions struct {
	// AllowPrivilegedGrant 为 true 时跳过「必须持有被授予角色」
	// （Server platform admin / keys）；Client 为 false。
	AllowPrivilegedGrant bool
}

// Documents 是 Client/Server 共用的文档 CRUD 核：OCC、grant 展开/校验、
// MapDocumentDBError、request_id 写幂等。集合守卫、guest 读、owner 默认 ACE
// 留在包装层。
type Documents struct {
	docDB databases.DocumentDB
	// idem 为 nil 时写幂等关闭（测试 / 无 store 部署形态）。
	idem databases.IdempotencyStore
}

// New 构造文档核。docDB 不得为 nil；idem 为 nil 时幂等关闭。
func New(docDB databases.DocumentDB, idem databases.IdempotencyStore) *Documents {
	return &Documents{docDB: docDB, idem: idem}
}

// DocumentDB 返回注入的端口（包装层 catalog / EnsureCatalog 复用）。
func (d *Documents) DocumentDB() databases.DocumentDB { return d.docDB }

func (d *Documents) CreateDocument(
	ctx context.Context,
	projectID, databaseID, collectionID, documentID string,
	data map[string]any,
	perms []databases.Permission,
	principal databases.Principal,
	requestID string,
	opts WriteOptions,
) (*databases.Document, bool, error) {
	if len(data) == 0 {
		return nil, false, status.Error(codes.InvalidArgument, "data is required")
	}
	if err := ValidateDocumentPayload(data); err != nil {
		return nil, false, err
	}
	if err := validateACL(perms); err != nil {
		return nil, false, err
	}
	return idempotentExec(ctx, d.idem, projectID, requestID, "databases.CreateDocument", principal,
		documentFingerprintBody{
			Database:    databaseID,
			Collection:  collectionID,
			Document:    documentID,
			Data:        normalizeData(data),
			Permissions: perms,
		},
		func(ctx context.Context) (*databases.Document, error) {
			effPerms, err := applyGrant(principal, perms, opts.AllowPrivilegedGrant)
			if err != nil {
				return nil, err
			}
			created, err := d.docDB.CreateDocument(ctx, projectID, databaseID, collectionID, databases.Document{
				ID:   documentID,
				Data: data,
			}, effPerms, principal)
			if err != nil {
				return nil, shared.MapDocumentDBError(fmt.Errorf("create document: %w", err))
			}
			return &created, nil
		})
}

// ListDocumentsResult 是列表查询的出站聚合：Distances 仅 vector_search
// （KNN）查询非空（与 Documents 平行；会话 #10 预决策 4）。
type ListDocumentsResult struct {
	Documents     []databases.Document
	TotalCount    int64
	NextPageToken string
	Distances     []float64
}

func (d *Documents) ListDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	q databases.Query,
	principal databases.Principal,
) (*ListDocumentsResult, error) {
	ast, err := ResolveQuery(q)
	if err != nil {
		return nil, err
	}
	list, err := d.docDB.ListDocuments(ctx, projectID, databaseID, collectionID, databases.Query{
		AST:       ast,
		PageSize:  ast.PageSize,
		PageToken: ast.PageToken,
	}, principal)
	if err != nil {
		return nil, shared.MapDocumentDBError(err)
	}
	return &ListDocumentsResult{
		Documents:     list.Documents,
		TotalCount:    list.TotalCount,
		NextPageToken: list.NextPageToken,
		Distances:     list.Distances,
	}, nil
}

func (d *Documents) GetDocument(
	ctx context.Context,
	projectID, databaseID, collectionID, documentID string,
	principal databases.Principal,
) (*databases.Document, error) {
	doc, err := d.docDB.GetDocument(ctx, projectID, databaseID, collectionID, documentID, principal)
	if err != nil {
		return nil, shared.MapDocumentDBError(err)
	}
	if doc == nil {
		return nil, status.Error(codes.NotFound, "document not found")
	}
	return doc, nil
}

func (d *Documents) UpdateDocument(
	ctx context.Context,
	projectID, databaseID, collectionID, documentID string,
	data map[string]any,
	perms []databases.Permission,
	increment map[string]int64,
	arrayUpdates map[string]databases.ArrayUpdate,
	principal databases.Principal,
	version *int64,
	requestID string,
	opts WriteOptions,
) (*databases.Document, bool, error) {
	if err := shared.UpdateDocumentVersionRequired(version); err != nil {
		return nil, false, err
	}
	if len(data) == 0 && len(perms) == 0 && len(increment) == 0 && len(arrayUpdates) == 0 {
		return nil, false, status.Error(codes.InvalidArgument, "data, permissions, increment, or array_updates is required")
	}
	if err := ValidateDocumentPayload(data); err != nil {
		return nil, false, err
	}
	if err := validateACL(perms); err != nil {
		return nil, false, err
	}
	if err := ValidateArrayUpdates(arrayUpdates); err != nil {
		return nil, false, err
	}
	return idempotentExec(ctx, d.idem, projectID, requestID, "databases.UpdateDocument", principal,
		documentFingerprintBody{
			Database:     databaseID,
			Collection:   collectionID,
			Document:     documentID,
			Data:         normalizeData(data),
			Permissions:  perms,
			Increment:    increment,
			ArrayUpdates: arrayUpdates,
			Version:      version,
		},
		func(ctx context.Context) (*databases.Document, error) {
			effPerms := perms
			if len(perms) > 0 {
				var err error
				effPerms, err = applyGrant(principal, perms, opts.AllowPrivilegedGrant)
				if err != nil {
					return nil, err
				}
			}
			effData := data
			if len(effData) == 0 {
				effData = map[string]any{}
			}
			updated, err := d.docDB.UpdateDocument(ctx, projectID, databaseID, collectionID, databases.DocumentUpdate{
				Document:        databases.Document{ID: documentID, Data: effData},
				Permissions:     effPerms,
				Increment:       increment,
				ArrayUpdates:    arrayUpdates,
				ExpectedVersion: *version,
			}, principal)
			if err != nil {
				return nil, shared.MapDocumentDBError(fmt.Errorf("update document: %w", err))
			}
			return &updated, nil
		})
}

func (d *Documents) UpsertDocument(
	ctx context.Context,
	projectID, databaseID, collectionID, documentID string,
	data map[string]any,
	conflictColumns []string,
	perms []databases.Permission,
	principal databases.Principal,
	requestID string,
	opts WriteOptions,
) (*databases.Document, bool, error) {
	if len(data) == 0 {
		return nil, false, status.Error(codes.InvalidArgument, "data is required")
	}
	if err := ValidateDocumentPayload(data); err != nil {
		return nil, false, err
	}
	if len(conflictColumns) == 0 {
		return nil, false, status.Error(codes.InvalidArgument, "conflict_columns is required")
	}
	if err := validateACL(perms); err != nil {
		return nil, false, err
	}
	return idempotentExec(ctx, d.idem, projectID, requestID, "databases.UpsertDocument", principal,
		documentFingerprintBody{
			Database:        databaseID,
			Collection:      collectionID,
			Document:        documentID,
			Data:            normalizeData(data),
			Permissions:     perms,
			ConflictColumns: sortedStrings(conflictColumns),
		},
		func(ctx context.Context) (*databases.Document, error) {
			effPerms, err := applyGrant(principal, perms, opts.AllowPrivilegedGrant)
			if err != nil {
				return nil, err
			}
			upserted, err := d.docDB.UpsertDocument(ctx, projectID, databaseID, collectionID, databases.Document{
				ID:   documentID,
				Data: data,
			}, conflictColumns, effPerms, principal)
			if err != nil {
				return nil, shared.MapDocumentDBError(fmt.Errorf("upsert document: %w", err))
			}
			return &upserted, nil
		})
}

func (d *Documents) DeleteDocument(
	ctx context.Context,
	projectID, databaseID, collectionID, documentID string,
	principal databases.Principal,
	version *int64,
	requestID string,
) (bool, error) {
	if err := shared.UpdateDocumentVersionRequired(version); err != nil {
		return false, err
	}
	_, replayed, err := idempotentExec(ctx, d.idem, projectID, requestID, "databases.DeleteDocument", principal,
		documentFingerprintBody{
			Database:   databaseID,
			Collection: collectionID,
			Document:   documentID,
			Version:    version,
		},
		func(ctx context.Context) (struct{}, error) {
			err := d.docDB.DeleteDocument(ctx, projectID, databaseID, collectionID, documentID, databases.DeleteOptions{ExpectedVersion: *version}, principal)
			if err != nil {
				return struct{}{}, shared.MapDocumentDBError(err)
			}
			return struct{}{}, nil
		})
	return replayed, err
}

func (d *Documents) CountDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	q databases.Query,
	principal databases.Principal,
) (int64, error) {
	ast, err := ResolveQuery(q)
	if err != nil {
		return 0, err
	}
	count, err := d.docDB.CountDocuments(ctx, projectID, databaseID, collectionID, databases.Query{AST: ast}, principal)
	return count, shared.MapDocumentDBError(err)
}

// AggregateDocuments 聚合透传（redesign §4.1 + §10.5 P1）：权限过滤在
// infra 的 listPermissionFilter 链内完成（D1：聚合一律在可见行集上执行）；
// 数值/group_by 校验与空集语义见端口注释。
func (d *Documents) AggregateDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	q databases.Query,
	aggs []databases.AggregateSpec,
	groupBy string,
	principal databases.Principal,
) ([]databases.AggregateGroup, error) {
	ast, err := ResolveQuery(q)
	if err != nil {
		return nil, err
	}
	groups, err := d.docDB.AggregateDocuments(ctx, projectID, databaseID, collectionID, databases.Query{AST: ast}, aggs, groupBy, principal)
	return groups, shared.MapDocumentDBError(err)
}

func (d *Documents) BulkUpdateDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	documentIDs []string,
	data map[string]any,
	perms []databases.Permission,
	principal databases.Principal,
	requestID string,
	opts WriteOptions,
) (int64, bool, error) {
	if err := validateBulkIDs(documentIDs); err != nil {
		return 0, false, err
	}
	if err := ValidateDocumentPayload(data); err != nil {
		return 0, false, err
	}
	if err := validateACL(perms); err != nil {
		return 0, false, err
	}
	return idempotentExec(ctx, d.idem, projectID, requestID, "databases.BulkUpdateDocuments", principal,
		bulkFingerprintBody{
			Database:    databaseID,
			Collection:  collectionID,
			DocumentIDs: sortedStrings(documentIDs),
			Data:        normalizeData(data),
			Permissions: perms,
		},
		func(ctx context.Context) (int64, error) {
			effPerms := perms
			if len(perms) > 0 {
				var err error
				effPerms, err = applyGrant(principal, perms, opts.AllowPrivilegedGrant)
				if err != nil {
					return 0, err
				}
			}
			n, err := d.docDB.BulkUpdateDocuments(ctx, projectID, databaseID, collectionID, documentIDs, data, effPerms, principal)
			if err != nil {
				return 0, shared.MapDocumentDBError(err)
			}
			return n, nil
		})
}

func (d *Documents) BulkDeleteDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	documentIDs []string,
	principal databases.Principal,
	requestID string,
) (int64, bool, error) {
	if err := validateBulkIDs(documentIDs); err != nil {
		return 0, false, err
	}
	return idempotentExec(ctx, d.idem, projectID, requestID, "databases.BulkDeleteDocuments", principal,
		bulkFingerprintBody{
			Database:    databaseID,
			Collection:  collectionID,
			DocumentIDs: sortedStrings(documentIDs),
		},
		func(ctx context.Context) (int64, error) {
			n, err := d.docDB.BulkDeleteDocuments(ctx, projectID, databaseID, collectionID, documentIDs, principal)
			if err != nil {
				return 0, shared.MapDocumentDBError(err)
			}
			return n, nil
		})
}

func validateBulkIDs(documentIDs []string) error {
	if len(documentIDs) == 0 {
		return status.Error(codes.InvalidArgument, "document_ids is required")
	}
	if len(documentIDs) > MaxBulkOperations {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("document_ids exceeds maximum of %d", MaxBulkOperations))
	}
	return nil
}

// ApplyGrant 展开/校验授予（Server 事务 op 等包装层复用）。
func ApplyGrant(principal databases.Principal, perms []databases.Permission, allowPrivileged bool) ([]databases.Permission, error) {
	return applyGrant(principal, perms, allowPrivileged)
}

func applyGrant(principal databases.Principal, perms []databases.Permission, allowPrivileged bool) ([]databases.Permission, error) {
	perms = databases.ExpandPermissionTemplates(perms, principal.Roles)
	if err := databases.ValidateGrantablePermissions(principal, perms, allowPrivileged); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return perms, nil
}

// ExecuteTransactions 透传事务内核（redesign §4.8 Phase 1）。守卫、种子与
// grant 展开由包装层（app/server）完成；infra 单事务执行器保证原子性、
// 锁纪律与事件同事务。ATOMIC 失败映射域码错误。幂等覆盖整批：request_id
// 重放返回首次完整结果（含 PARTIAL per-op 结果，E2）。
func (d *Documents) ExecuteTransactions(
	ctx context.Context,
	projectID, databaseID string,
	ops []databases.TransactionOp,
	mode databases.TransactionMode,
	principal databases.Principal,
	requestID string,
) ([]databases.TransactionOpResult, bool, error) {
	return idempotentExec(ctx, d.idem, projectID, requestID, "databases.ExecuteTransactions", principal,
		txFingerprintBody{
			Database: databaseID,
			Mode:     mode,
			Ops:      txOpsFingerprint(ops),
		},
		func(ctx context.Context) ([]databases.TransactionOpResult, error) {
			results, err := d.docDB.ExecuteTransactions(ctx, projectID, databaseID, ops, mode, principal)
			if err != nil {
				// ATOMIC 失败：OpError 携带 index + 哨兵，映射为带 op 定位的域码错误。
				if oe := databases.AsOpError(err); oe != nil {
					if code := databases.ErrorDomainCode(oe.Err); code != "" {
						return nil, shared.DomainStatusWithOp(code, oe.Index)
					}
				}
				return nil, shared.MapDocumentDBError(fmt.Errorf("execute transactions: %w", err))
			}
			return results, nil
		})
}
