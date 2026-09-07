package servergrpc

import (
	"context"
	"fmt"
	"strings"

	serverv1 "github.com/torchwooddev/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
	"github.com/torchwooddev/torchwood/internal/app/documents"
	appserver "github.com/torchwooddev/torchwood/internal/app/server"
	"github.com/torchwooddev/torchwood/internal/app/shared"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/crud"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type DatabasesService struct {
	serverv1.UnimplementedDatabasesServiceServer
	databases *appserver.Databases
}

func NewDatabasesService(databases *appserver.Databases) *DatabasesService {
	return &DatabasesService{databases: databases}
}

func (s *DatabasesService) projectID(ctx context.Context) string {
	p, ok := contexts.Principal(ctx)
	if !ok {
		return ""
	}
	return p.ProjectID
}

// requestIDFromMeta 返回写请求的幂等键：proto 字段优先，回退 HTTP 网关映射
// 进来的 Idempotency-Key 头（grpc-gateway incoming metadata）。
func requestIDFromMeta(ctx context.Context, inRequestID string) string {
	if inRequestID != "" {
		return inRequestID
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if values := md.Get("idempotency-key"); len(values) > 0 && values[0] != "" {
			return values[0]
		}
	}
	return ""
}

// markReplayed 在幂等重放时下发 x-torchwood-replayed 响应头（gRPC response
// metadata；HTTP 面由网关 outgoing matcher 透传为响应头）。非 gRPC 传输 ctx
// 下静默跳过（直调用例）。
func markReplayed(ctx context.Context, replayed bool) {
	if !replayed {
		return
	}
	_ = grpc.SetHeader(ctx, metadata.Pairs("x-torchwood-replayed", "true"))
}

// auditResource* 构建审计资源路径（A8）：ResourceID 形如
// databases/{db}、databases/{db}/collections/{coll}、…/documents/{doc}。
func auditDatabaseResource(databaseID string) string {
	return "databases/" + databaseID
}

func auditCollectionResource(databaseID, collectionID string) string {
	return "databases/" + databaseID + "/collections/" + collectionID
}

func auditDocumentResource(databaseID, collectionID, documentID string) string {
	return "databases/" + databaseID + "/collections/" + collectionID + "/documents/" + documentID
}

func (s *DatabasesService) CreateDatabase(ctx context.Context, req *serverv1.CreateDatabaseRequest) (*serverv1.Database, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditDatabaseResource(req.GetId()))
	// id required 由 CreateDatabaseRequest 的 buf.validate 注解在 validate
	// 拦截器承担（09-api-guide §2.3）。
	if err := s.databases.CreateDatabase(ctx, projectID, req.GetId(), req.GetName()); err != nil {
		return nil, err
	}
	return &serverv1.Database{Id: req.GetId(), Name: req.GetName()}, nil
}

func (s *DatabasesService) ListDatabases(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListDatabasesResponse, error) {
	params, err := crud.ParseListParams(req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "databases")
	dbs, err := s.databases.ListDatabases(ctx, projectID)
	if err != nil {
		return nil, err
	}
	start := params.Offset
	if start > len(dbs) {
		start = len(dbs)
	}
	end := start + int(params.PageSize)
	if end > len(dbs) {
		end = len(dbs)
	}
	page := dbs[start:end]
	hasMore := end < len(dbs)
	info := crud.BuildPaginationInfo(params, len(dbs), hasMore)
	var nextToken, prevToken string
	if info.HasNext {
		if nextToken, err = crud.EncodePageToken(info.NextOffset); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if info.HasPrevious {
		if prevToken, err = crud.EncodePageToken(info.PreviousOffset); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	out := make([]*serverv1.Database, len(page))
	for i := range page {
		out[i] = mapDatabase(&page[i])
	}
	return &serverv1.ListDatabasesResponse{
		Databases: out,
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      info.PageSize,
			TotalCount:    int32(info.TotalCount),
			NextPageToken: nextToken,
			PrevPageToken: prevToken,
		},
	}, nil
}

func (s *DatabasesService) GetDatabase(ctx context.Context, req *serverv1.GetDatabaseRequest) (*serverv1.Database, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditDatabaseResource(req.GetId()))
	db, err := s.databases.GetDatabase(ctx, projectID, req.GetId())
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, status.Error(codes.NotFound, "database not found")
	}
	return mapDatabase(db), nil
}

func (s *DatabasesService) DeleteDatabase(ctx context.Context, req *serverv1.GetDatabaseRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditDatabaseResource(req.GetId()))
	if err := s.databases.DeleteDatabase(ctx, projectID, req.GetId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *DatabasesService) CreateCollection(ctx context.Context, req *serverv1.CreateCollectionRequest) (*serverv1.Collection, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetId()))
	perms, err := databases.ParsePermissionStrings(req.GetPermissions())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	documentSecurity := true
	if req.DocumentSecurity != nil {
		documentSecurity = req.GetDocumentSecurity()
	}
	if err := s.databases.CreateCollection(ctx, projectID, req.GetDatabaseId(), req.GetId(), req.GetName(), nil, nil, perms, documentSecurity); err != nil {
		return nil, err
	}
	// R02-P3-1：移除尾随 GetCollection 重查（adapter 已完成元数据写入），
	// 直接基于请求构造响应（与 CreateDatabase handler 模式一致）。
	return &serverv1.Collection{
		Id:               req.GetId(),
		DatabaseId:       req.GetDatabaseId(),
		Name:             req.GetName(),
		DocumentSecurity: documentSecurity,
		Permissions:      formatPermissionStrings(perms),
	}, nil
}

func (s *DatabasesService) ListCollections(ctx context.Context, req *serverv1.ListCollectionsRequest) (*serverv1.ListCollectionsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "databases/"+req.GetDatabaseId()+"/collections")
	cols, total, next, err := s.databases.ListCollections(ctx, projectID, req.GetDatabaseId(), databases.ListQuery{
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.Collection, len(cols))
	for i := range cols {
		out[i] = mapCollection(&cols[i])
	}
	return &serverv1.ListCollectionsResponse{
		Collections: out,
		Meta:        &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), TotalCount: int32(total), NextPageToken: next},
	}, nil
}

func (s *DatabasesService) GetCollection(ctx context.Context, req *serverv1.GetCollectionRequest) (*serverv1.Collection, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	col, err := s.databases.GetCollection(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId())
	if err != nil {
		return nil, err
	}
	if col == nil {
		return nil, status.Error(codes.NotFound, "collection not found")
	}
	return mapCollection(col), nil
}

func (s *DatabasesService) DeleteCollection(ctx context.Context, req *serverv1.GetCollectionRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	if err := s.databases.DeleteCollection(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *DatabasesService) UpdateCollection(ctx context.Context, req *serverv1.UpdateCollectionRequest) (*serverv1.Collection, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	patch := databases.CollectionPatch{}
	// name 为 optional（R10-P1-6）：未设置 = 不修改；设置非空串 = 更新
	// （adapter 对空串同样按不修改处理，清空语义暂不支持）。
	if req.Name != nil {
		patch.Name = req.GetName()
	}
	if req.Permissions != nil {
		perms, err := databases.ParsePermissionStrings(req.GetPermissions().GetValues())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		patch.Permissions = &perms
	}
	if req.DocumentSecurity != nil {
		v := req.GetDocumentSecurity()
		patch.DocumentSecurity = &v
	}
	if req.Disabled != nil {
		v := req.GetDisabled()
		patch.Disabled = &v
	}
	if err := s.databases.UpdateCollection(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), patch, dbPrincipal(ctx)); err != nil {
		return nil, err
	}
	col, err := s.databases.GetCollection(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId())
	if err != nil {
		return nil, err
	}
	if col == nil {
		return nil, status.Error(codes.NotFound, "collection not found")
	}
	return mapCollection(col), nil
}

func (s *DatabasesService) CreateAttribute(ctx context.Context, req *serverv1.CreateAttributeRequest) (*serverv1.Attribute, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	attr := databases.Attribute{
		ID:       req.GetKey(),
		Key:      req.GetKey(),
		Type:     req.GetType(),
		Size:     int(req.GetSize()),
		Required: req.GetRequired(),
		Array:    req.GetArray(),
		Dims:     int(req.GetDims()),
	}
	if req.GetDefaultValue() != "" {
		attr.Default = req.GetDefaultValue()
	}
	if err := s.databases.CreateAttribute(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), attr); err != nil {
		return nil, err
	}
	out := &serverv1.Attribute{
		Id:           attr.ID,
		Key:          attr.Key,
		Type:         attr.Type,
		Size:         int32(attr.Size),
		Required:     attr.Required,
		Array:        attr.Array,
		DefaultValue: req.GetDefaultValue(),
	}
	if attr.Dims != 0 {
		dims := int32(attr.Dims)
		out.Dims = &dims
	}
	return out, nil
}

func (s *DatabasesService) CreateIndex(ctx context.Context, req *serverv1.CreateIndexRequest) (*serverv1.Index, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	idx := databases.Index{
		ID:         req.GetId(),
		Type:       req.GetType(),
		Attributes: req.GetAttributes(),
		Orders:     req.GetOrders(),
	}
	// hnsw 的 metric 缺省 COSINE 归一（大写形态落 catalog——vector_search 的
	// 索引前置校验按大写比对）。
	if strings.ToLower(idx.Type) == "hnsw" {
		idx.DistanceMetric = strings.ToUpper(req.GetDistanceMetric())
		if idx.DistanceMetric == "" {
			idx.DistanceMetric = "COSINE"
		}
	}
	if err := s.databases.CreateIndex(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), idx); err != nil {
		return nil, err
	}
	out := &serverv1.Index{
		Id:         idx.ID,
		Type:       idx.Type,
		Attributes: idx.Attributes,
		Orders:     idx.Orders,
	}
	if idx.DistanceMetric != "" {
		out.DistanceMetric = &idx.DistanceMetric
	}
	return out, nil
}

func (s *DatabasesService) DeleteAttribute(ctx context.Context, req *serverv1.DeleteAttributeRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	if err := s.databases.DeleteAttribute(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), req.GetKey()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

// RestoreAttribute 回滚生命周期（B4 §4.6 可回滚）：deprecated → active；
// migrating → 中止迁移并恢复 active。
func (s *DatabasesService) RestoreAttribute(ctx context.Context, req *serverv1.RestoreAttributeRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	if err := s.databases.RestoreAttribute(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), req.GetKey()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

// RetireAttribute 是删列两段的段二（B4）：deprecated 属性物理删列 / swap 后
// 迁移残留旧列退役（不可逆）。
func (s *DatabasesService) RetireAttribute(ctx context.Context, req *serverv1.RetireAttributeRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	if err := s.databases.RetireAttribute(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), req.GetKey()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

// MigrateAttribute 创建 copy 迁移任务（B4）：响应为迁移任务读回（物理列名
// 不出契约）。
func (s *DatabasesService) MigrateAttribute(ctx context.Context, req *serverv1.MigrateAttributeRequest) (*serverv1.AttributeMigration, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	target := databases.Attribute{
		ID:       req.GetKey(),
		Key:      req.GetKey(),
		Type:     req.GetType(),
		Size:     int(req.GetSize()),
		Required: req.GetRequired(),
		Array:    req.GetArray(),
		Dims:     int(req.GetDims()),
	}
	if req.GetDefaultValue() != "" {
		target.Default = req.GetDefaultValue()
	}
	mig, err := s.databases.MigrateAttribute(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), req.GetKey(), target)
	if err != nil {
		return nil, err
	}
	return &serverv1.AttributeMigration{
		Id:            mig.ID,
		Key:           mig.AttrKey,
		Phase:         mig.Phase,
		RowsDone:      mig.RowsDone,
		SchemaVersion: mig.SchemaVersion,
	}, nil
}

func (s *DatabasesService) DeleteIndex(ctx context.Context, req *serverv1.DeleteIndexRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	if err := s.databases.DeleteIndex(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), req.GetIndexId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *DatabasesService) CreateDocument(ctx context.Context, req *serverv1.CreateDocumentRequest) (*sharedv1.Document, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if req.GetDocumentId() != "" {
		ctx = contexts.WithAuditResource(ctx, auditDocumentResource(req.GetDatabaseId(), req.GetCollectionId(), req.GetDocumentId()))
	} else {
		ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	}
	data := map[string]any{}
	if req.GetData() != nil {
		data = req.GetData().AsMap()
	}
	perms, err := parseOptionalPermissions(req.GetPermissions())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	doc, replayed, err := s.databases.CreateDocument(
		ctx,
		projectID,
		req.GetDatabaseId(),
		req.GetCollectionId(),
		req.GetDocumentId(),
		data,
		perms,
		dbPrincipal(ctx),
		requestIDFromMeta(ctx, req.GetRequestId()),
	)
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	return mapDocument(doc)
}

func (s *DatabasesService) ListDocuments(ctx context.Context, req *serverv1.ListDocumentsRequest) (*serverv1.ListDocumentsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	q, err := documents.BindListQuery(req.GetPageSize(), req.GetPageToken(), req.GetQuery())
	if err != nil {
		return nil, err
	}
	ast, err := documents.ResolveQuery(q)
	if err != nil {
		return nil, err
	}
	result, err := s.databases.ListDocuments(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), q, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	docs := result.Documents
	out := make([]*sharedv1.Document, len(docs))
	for i := range docs {
		mapped, err := mapDocument(&docs[i])
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	return &serverv1.ListDocumentsResponse{
		Documents: out,
		Meta:      &sharedv1.ListResponseMeta{PageSize: ast.PageSize, TotalCount: int32(result.TotalCount), NextPageToken: result.NextPageToken},
		// KNN 距离回传（会话 #10 预决策 4）：与 documents 平行，仅
		// vector_search 查询非空（max_distance 后置过滤后的行）。
		Distances: result.Distances,
	}, nil
}

func (s *DatabasesService) GetDocument(ctx context.Context, req *serverv1.GetDocumentRequest) (*sharedv1.Document, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditDocumentResource(req.GetDatabaseId(), req.GetCollectionId(), req.GetDocumentId()))
	doc, err := s.databases.GetDocument(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), req.GetDocumentId(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return mapDocument(doc)
}

func (s *DatabasesService) UpdateDocument(ctx context.Context, req *serverv1.UpdateDocumentRequest) (*sharedv1.Document, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditDocumentResource(req.GetDatabaseId(), req.GetCollectionId(), req.GetDocumentId()))
	perms, err := parseOptionalPermissions(req.GetPermissions())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	var version *int64
	if req.Version != nil {
		v := req.GetVersion()
		version = &v
	}
	arrayUpdates, err := mapArrayUpdates(req.GetArrayUpdates())
	if err != nil {
		return nil, err
	}
	doc, replayed, err := s.databases.UpdateDocument(
		ctx,
		projectID,
		req.GetDatabaseId(),
		req.GetCollectionId(),
		req.GetDocumentId(),
		updateData(req.GetData()),
		perms,
		req.GetIncrement(),
		arrayUpdates,
		dbPrincipal(ctx),
		version,
		requestIDFromMeta(ctx, req.GetRequestId()),
	)
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	return mapDocument(doc)
}

func (s *DatabasesService) UpsertDocument(ctx context.Context, req *serverv1.UpsertDocumentRequest) (*sharedv1.Document, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditDocumentResource(req.GetDatabaseId(), req.GetCollectionId(), req.GetDocumentId()))
	data := map[string]any{}
	if req.GetData() != nil {
		data = req.GetData().AsMap()
	}
	perms, err := parseOptionalPermissions(req.GetPermissions())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	doc, replayed, err := s.databases.UpsertDocument(
		ctx,
		projectID,
		req.GetDatabaseId(),
		req.GetCollectionId(),
		req.GetDocumentId(),
		data,
		req.GetConflictColumns(),
		perms,
		dbPrincipal(ctx),
		requestIDFromMeta(ctx, req.GetRequestId()),
	)
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	return mapDocument(doc)
}

func (s *DatabasesService) BulkUpdateDocuments(ctx context.Context, req *serverv1.BulkUpdateDocumentsRequest) (*serverv1.BulkDocumentsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	perms, err := parseOptionalPermissions(req.GetPermissions())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	n, replayed, err := s.databases.BulkUpdateDocuments(
		ctx,
		projectID,
		req.GetDatabaseId(),
		req.GetCollectionId(),
		req.GetDocumentIds(),
		updateData(req.GetData()),
		perms,
		dbPrincipal(ctx),
		requestIDFromMeta(ctx, req.GetRequestId()),
	)
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	return &serverv1.BulkDocumentsResponse{Affected: n}, nil
}

// ExecuteTransactions 在单事务内执行异构 op 批（事务内核 Phase 1）。
func (s *DatabasesService) ExecuteTransactions(ctx context.Context, req *serverv1.ExecuteTransactionsRequest) (*serverv1.ExecuteTransactionsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditDatabaseResource(req.GetDatabaseId()))
	// ops 非空由 ExecuteTransactionsRequest 的 buf.validate min_items 注解
	// 在 validate 拦截器承担（09-api-guide §2.3）；mode 的 UNSPECIFIED 容忍
	// 语义保留在下方 switch（上收 defined_only 会拒绝省略 mode 的存量客户端）。
	ops := make([]databases.TransactionOp, 0, len(req.GetOps()))
	for i, protoOp := range req.GetOps() {
		op, err := transactionOpFromProto(protoOp)
		if err != nil {
			return nil, shared.DomainStatusWithViolations(databases.ErrCodeInvalidArgument,
				shared.FieldViolation{Field: fmt.Sprintf("ops[%d]", i), Description: err.Error()})
		}
		ops = append(ops, op)
	}
	var mode databases.TransactionMode
	switch req.GetMode() {
	case serverv1.TransactionMode_TRANSACTION_MODE_UNSPECIFIED, serverv1.TransactionMode_TRANSACTION_MODE_ATOMIC:
		mode = databases.TransactionModeAtomic
	case serverv1.TransactionMode_TRANSACTION_MODE_PARTIAL:
		mode = databases.TransactionModePartial
	default:
		return nil, status.Errorf(codes.InvalidArgument, "invalid transaction mode %v", req.GetMode())
	}
	results, replayed, err := s.databases.ExecuteTransactions(ctx, projectID, req.GetDatabaseId(), ops, mode, dbPrincipal(ctx), requestIDFromMeta(ctx, req.GetRequestId()))
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	out := &serverv1.ExecuteTransactionsResponse{}
	for _, r := range results {
		res := &serverv1.TransactionOpResult{
			Index:        int32(r.Index),
			DocumentId:   r.DocumentID,
			Version:      r.Version,
			ErrorCode:    r.ErrCode,
			ErrorMessage: r.ErrMessage,
		}
		if r.OK {
			res.Status = serverv1.TransactionOpStatus_TRANSACTION_OP_STATUS_OK
		} else {
			res.Status = serverv1.TransactionOpStatus_TRANSACTION_OP_STATUS_ERROR
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

func transactionOpFromProto(in *serverv1.TransactionOp) (databases.TransactionOp, error) {
	var typ databases.TransactionOpType
	switch in.GetType() {
	case serverv1.TransactionOpType_TRANSACTION_OP_TYPE_CREATE:
		typ = databases.TransactionOpCreate
	case serverv1.TransactionOpType_TRANSACTION_OP_TYPE_UPDATE:
		typ = databases.TransactionOpUpdate
	case serverv1.TransactionOpType_TRANSACTION_OP_TYPE_UPSERT:
		typ = databases.TransactionOpUpsert
	case serverv1.TransactionOpType_TRANSACTION_OP_TYPE_DELETE:
		typ = databases.TransactionOpDelete
	default:
		return databases.TransactionOp{}, fmt.Errorf("invalid op type %v", in.GetType())
	}
	perms, err := parseOptionalPermissions(in.GetPermissions())
	if err != nil {
		return databases.TransactionOp{}, err
	}
	arrayUpdates, err := mapArrayUpdates(in.GetArrayUpdates())
	if err != nil {
		return databases.TransactionOp{}, err
	}
	op := databases.TransactionOp{
		Type:            typ,
		CollectionID:    in.GetCollectionId(),
		DocumentID:      in.GetDocumentId(),
		Data:            updateData(in.GetData()),
		Permissions:     perms,
		Increment:       in.GetIncrement(),
		ArrayUpdates:    arrayUpdates,
		ConflictColumns: in.GetConflictColumns(),
	}
	if in.ExpectedVersion != nil {
		v := *in.ExpectedVersion
		op.ExpectedVersion = &v
	}
	return op, nil
}

func (s *DatabasesService) BulkDeleteDocuments(ctx context.Context, req *serverv1.BulkDeleteDocumentsRequest) (*serverv1.BulkDocumentsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	n, replayed, err := s.databases.BulkDeleteDocuments(
		ctx,
		projectID,
		req.GetDatabaseId(),
		req.GetCollectionId(),
		req.GetDocumentIds(),
		dbPrincipal(ctx),
		requestIDFromMeta(ctx, req.GetRequestId()),
	)
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	return &serverv1.BulkDocumentsResponse{Affected: n}, nil
}

func (s *DatabasesService) DeleteDocument(ctx context.Context, req *serverv1.DeleteDocumentRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditDocumentResource(req.GetDatabaseId(), req.GetCollectionId(), req.GetDocumentId()))
	var version *int64
	if req.Version != nil {
		v := req.GetVersion()
		version = &v
	}
	replayed, err := s.databases.DeleteDocument(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), req.GetDocumentId(), dbPrincipal(ctx), version, requestIDFromMeta(ctx, req.GetRequestId()))
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	return &sharedv1.Empty{}, nil
}

func (s *DatabasesService) CountDocuments(ctx context.Context, req *serverv1.CountDocumentsRequest) (*serverv1.CountDocumentsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	q, err := documents.BindListQuery(0, "", req.GetQuery())
	if err != nil {
		return nil, err
	}
	count, err := s.databases.CountDocuments(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), q, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return &serverv1.CountDocumentsResponse{Count: count}, nil
}

// ListChanges 事件补偿（阶段④ §4.5）：seq 升序、按请求者可见性过滤；
// has_more 时以末条 seq 续传；游标过期 → FailedPrecondition
// EVENTS.RESUME_EXPIRED（指引全量重拉）。
func (s *DatabasesService) ListChanges(ctx context.Context, req *serverv1.ListChangesRequest) (*serverv1.ListChangesResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	// since_seq >= 0 由 ListChangesRequest 的 buf.validate gte 注解在
	// validate 拦截器承担（09-api-guide §2.3）。
	changes, hasMore, nextSinceSeq, err := s.databases.ListChanges(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(),
		databases.ListChangesOptions{SinceSeq: req.GetSinceSeq(), Limit: int(req.GetLimit())}, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	out, err := mapChanges(changes)
	if err != nil {
		return nil, err
	}
	return &serverv1.ListChangesResponse{Changes: out, HasMore: hasMore, NextSinceSeq: nextSinceSeq}, nil
}

// ExportCollectionSchema 导出集合契约的 JSON Schema 2020-12 文档（B10，
// redesign §4.1 Agent 面 / §10.1）。`as` 缺省 jsonschema；未知形态 →
// InvalidArgument（域码 violations 定位）。
func (s *DatabasesService) ExportCollectionSchema(ctx context.Context, req *serverv1.ExportCollectionSchemaRequest) (*serverv1.ExportCollectionSchemaResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	as := req.GetAs()
	if as == "" {
		as = "jsonschema"
	}
	if as != "jsonschema" {
		return nil, shared.DomainStatusWithViolations(databases.ErrCodeInvalidArgument,
			shared.FieldViolation{Field: "as", Description: fmt.Sprintf("unsupported export format %q (only %q)", as, "jsonschema")})
	}
	doc, err := s.databases.ExportCollectionSchema(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId())
	if err != nil {
		return nil, err
	}
	st, err := structpb.NewStruct(doc)
	if err != nil {
		return nil, status.Error(codes.Internal, "schema is not serializable")
	}
	return &serverv1.ExportCollectionSchemaResponse{Schema: st}, nil
}

// mapChanges 把领域 Change 映射为 wire 形态（Server/Client 两面共用语义，
// 各自持有同形实现——genproto 包不同）。
func mapChanges(changes []databases.DocumentChange) ([]*sharedv1.Change, error) {
	out := make([]*sharedv1.Change, 0, len(changes))
	for i := range changes {
		c := &changes[i]
		mapped := &sharedv1.Change{
			Seq:           c.Seq,
			EventId:       c.EventID,
			Event:         c.Event,
			DocumentId:    c.DocumentID,
			Version:       c.Version,
			TransactionId: c.TransactionID,
			Truncated:     c.Truncated,
			CreatedAt:     timestamppb.New(c.CreatedAt),
		}
		if c.Data != nil {
			doc, err := mapDocument(c.Data)
			if err != nil {
				return nil, err
			}
			mapped.Data = doc
		}
		out = append(out, mapped)
	}
	return out, nil
}

// AggregateDocuments 在权限过滤后的可见行集上聚合（redesign §4.1；D1）。
func (s *DatabasesService) AggregateDocuments(ctx context.Context, req *serverv1.AggregateDocumentsRequest) (*serverv1.AggregateDocumentsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, auditCollectionResource(req.GetDatabaseId(), req.GetCollectionId()))
	q, err := documents.BindListQuery(0, "", req.GetQuery())
	if err != nil {
		return nil, err
	}
	// aggregations 非空由 AggregateDocumentsRequest 的 buf.validate
	// min_items 注解在 validate 拦截器承担（09-api-guide §2.3）；逐项
	// function/field 校验留在下方循环（嵌套项定位的 FieldViolation 形态）。
	aggs := make([]databases.AggregateSpec, 0, len(req.GetAggregations()))
	for i, spec := range req.GetAggregations() {
		fn, ok := aggregateFunctionFromProto(spec.GetFunction())
		if !ok {
			return nil, shared.DomainStatusWithViolations(databases.ErrCodeInvalidArgument,
				shared.FieldViolation{Field: fmt.Sprintf("aggregations[%d].function", i), Description: fmt.Sprintf("invalid aggregate function %v", spec.GetFunction())})
		}
		if spec.GetField() == "" {
			return nil, shared.DomainStatusWithViolations(databases.ErrCodeInvalidArgument,
				shared.FieldViolation{Field: fmt.Sprintf("aggregations[%d].field", i), Description: "field is required"})
		}
		aggs = append(aggs, databases.AggregateSpec{Function: fn, Field: spec.GetField()})
	}
	groups, err := s.databases.AggregateDocuments(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), q, aggs, req.GetGroupBy(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	out := &serverv1.AggregateDocumentsResponse{}
	for _, g := range groups {
		pg := &serverv1.AggregateGroup{}
		if g.GroupKey != nil {
			pg.GroupKey = g.GroupKey
		}
		for _, v := range g.Values {
			pv := &serverv1.AggregateValue{
				Function: aggregateFunctionToProto(v.Function),
				Field:    v.Field,
			}
			switch v.Kind {
			case databases.AggregateValueInt64:
				pv.Result = &serverv1.AggregateValue_Int64Value{Int64Value: v.Int64}
			case databases.AggregateValueDouble:
				pv.Result = &serverv1.AggregateValue_DoubleValue{DoubleValue: v.Double}
			}
			pg.Values = append(pg.Values, pv)
		}
		out.Groups = append(out.Groups, pg)
	}
	return out, nil
}

func aggregateFunctionFromProto(fn serverv1.AggregateFunction) (databases.AggregateFunction, bool) {
	switch fn {
	case serverv1.AggregateFunction_AGGREGATE_FUNCTION_SUM:
		return databases.AggregateSum, true
	case serverv1.AggregateFunction_AGGREGATE_FUNCTION_AVG:
		return databases.AggregateAvg, true
	case serverv1.AggregateFunction_AGGREGATE_FUNCTION_MIN:
		return databases.AggregateMin, true
	case serverv1.AggregateFunction_AGGREGATE_FUNCTION_MAX:
		return databases.AggregateMax, true
	default:
		return "", false
	}
}

func aggregateFunctionToProto(fn databases.AggregateFunction) serverv1.AggregateFunction {
	switch fn {
	case databases.AggregateSum:
		return serverv1.AggregateFunction_AGGREGATE_FUNCTION_SUM
	case databases.AggregateAvg:
		return serverv1.AggregateFunction_AGGREGATE_FUNCTION_AVG
	case databases.AggregateMin:
		return serverv1.AggregateFunction_AGGREGATE_FUNCTION_MIN
	case databases.AggregateMax:
		return serverv1.AggregateFunction_AGGREGATE_FUNCTION_MAX
	default:
		return serverv1.AggregateFunction_AGGREGATE_FUNCTION_UNSPECIFIED
	}
}

func mapDatabase(c *databases.Database) *serverv1.Database {
	if c == nil {
		return nil
	}
	return &serverv1.Database{
		Id:        c.ID,
		Name:      c.Name,
		CreatedAt: timestamppb.New(c.CreatedAt),
		UpdatedAt: timestamppb.New(c.UpdatedAt),
	}
}

func mapCollection(c *databases.Collection) *serverv1.Collection {
	if c == nil {
		return nil
	}
	out := &serverv1.Collection{
		Id:               c.ID,
		DatabaseId:       c.DatabaseID,
		Name:             c.Name,
		DocumentSecurity: c.DocumentSecurity,
		Disabled:         c.Disabled,
		IsSystem:         c.IsSystem,
		CreatedAt:        timestamppb.New(c.CreatedAt),
		UpdatedAt:        timestamppb.New(c.UpdatedAt),
	}
	for _, p := range c.Permissions {
		out.Permissions = append(out.Permissions, p.Type+":"+p.Role)
	}
	for _, a := range c.Attributes {
		attr := &serverv1.Attribute{
			Id:       a.ID,
			Key:      a.Key,
			Type:     a.Type,
			Size:     int32(a.Size),
			Required: a.Required,
			Array:    a.Array,
		}
		if a.Default != nil {
			attr.DefaultValue = fmt.Sprint(a.Default)
		}
		if a.Dims != 0 {
			dims := int32(a.Dims)
			attr.Dims = &dims
		}
		// 生命周期状态（B4）：active 缺省省略（proto optional）。
		if st := a.StatusOrDefault(); st != databases.AttrStatusActive {
			statusStr := st
			attr.Status = &statusStr
		}
		out.Attributes = append(out.Attributes, attr)
	}
	for _, i := range c.Indexes {
		idx := &serverv1.Index{
			Id:         i.ID,
			Type:       i.Type,
			Attributes: i.Attributes,
			Orders:     i.Orders,
		}
		if i.DistanceMetric != "" {
			idx.DistanceMetric = &i.DistanceMetric
		}
		out.Indexes = append(out.Indexes, idx)
	}
	return out
}

func mapDocument(doc *databases.Document) (*sharedv1.Document, error) {
	if doc == nil {
		return nil, nil
	}
	data, err := structpb.NewStruct(doc.Data)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "document data is not serializable")
	}
	return &sharedv1.Document{
		Id:          doc.ID,
		Data:        data,
		CreatedAt:   timestamppb.New(doc.CreatedAt),
		UpdatedAt:   timestamppb.New(doc.UpdatedAt),
		Permissions: formatPermissionStrings(doc.Permissions),
		Version:     doc.Version,
	}, nil
}

func formatPermissionStrings(perms []databases.Permission) []string {
	if len(perms) == 0 {
		return nil
	}
	out := make([]string, len(perms))
	for i, p := range perms {
		out[i] = databases.FormatPermissionString(p)
	}
	return out
}

func updateData(s *structpb.Struct) map[string]any {
	if s == nil {
		return map[string]any{}
	}
	return s.AsMap()
}

// mapArrayUpdates 把 proto 数组列原子更新映射为 domain 形态（阶段③-b
// §10.5 P0 写侧；Intersect/Diff/Insert/Filter 补齐于转出 POC B1）；
// UNSPECIFIED op → InvalidArgument。
func mapArrayUpdates(in map[string]*sharedv1.ArrayUpdate) (map[string]databases.ArrayUpdate, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]databases.ArrayUpdate, len(in))
	for k, v := range in {
		if v == nil {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("array_updates[%s]: update is required", k))
		}
		var op string
		switch v.GetOp() {
		case sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_APPEND:
			op = databases.ArrayUpdateOpAppend
		case sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_PREPEND:
			op = databases.ArrayUpdateOpPrepend
		case sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_REMOVE:
			op = databases.ArrayUpdateOpRemove
		case sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_UNIQUE:
			op = databases.ArrayUpdateOpUnique
		case sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_INTERSECT:
			op = databases.ArrayUpdateOpIntersect
		case sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_DIFF:
			op = databases.ArrayUpdateOpDiff
		case sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_INSERT:
			op = databases.ArrayUpdateOpInsert
		case sharedv1.ArrayUpdateOp_ARRAY_UPDATE_OP_FILTER:
			op = databases.ArrayUpdateOpFilter
		default:
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("array_updates[%s]: op is required", k))
		}
		upd := databases.ArrayUpdate{Op: op, Values: v.GetValues()}
		if v.Index != nil {
			idx := v.GetIndex()
			upd.Index = &idx
		}
		out[k] = upd
	}
	return out, nil
}

// parseOptionalPermissions parses explicit permission strings into Permission
// values. An empty list yields nil (no document-level permissions), unlike
// ParsePermissionStrings which expands to DefaultCollectionPermissions.
func parseOptionalPermissions(items []string) ([]databases.Permission, error) {
	if len(items) == 0 {
		return nil, nil
	}
	return databases.ParsePermissionStrings(items)
}
