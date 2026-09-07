package clientgrpc

import (
	"context"
	"fmt"

	clientv1 "github.com/torchwooddev/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
	"github.com/torchwooddev/torchwood/internal/app/client"
	"github.com/torchwooddev/torchwood/internal/app/documents"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type DatabasesService struct {
	clientv1.UnimplementedDatabasesServiceServer
	databases *client.Databases
}

func NewDatabasesService(databases *client.Databases) *DatabasesService {
	return &DatabasesService{databases: databases}
}

func (s *DatabasesService) CreateDocument(ctx context.Context, req *clientv1.CreateDocumentRequest) (*sharedv1.Document, error) {
	if req.GetDocumentId() != "" {
		ctx = contexts.WithAuditResource(ctx, "databases/"+req.GetDatabaseId()+"/collections/"+req.GetCollectionId()+"/documents/"+req.GetDocumentId())
	} else {
		ctx = contexts.WithAuditResource(ctx, "databases/"+req.GetDatabaseId()+"/collections/"+req.GetCollectionId())
	}
	data := map[string]any{}
	if req.GetData() != nil {
		data = req.GetData().AsMap()
	}
	perms, err := parseOptionalPermissions(req.GetPermissions())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	doc, replayed, err := s.databases.CreateDocument(ctx, req.GetDatabaseId(), req.GetCollectionId(), req.GetDocumentId(), data, perms, requestIDFromMeta(ctx, req.GetRequestId()))
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	return mapClientDocument(doc)
}

func (s *DatabasesService) ListDocuments(ctx context.Context, req *clientv1.ListDocumentsRequest) (*clientv1.ListDocumentsResponse, error) {
	projectID, err := resolveProjectID(ctx, req.GetProjectId())
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, "databases/"+req.GetDatabaseId()+"/collections/"+req.GetCollectionId())
	q, err := documents.BindListQuery(req.GetPageSize(), req.GetPageToken(), req.GetQuery())
	if err != nil {
		return nil, err
	}
	ast, err := documents.ResolveQuery(q)
	if err != nil {
		return nil, err
	}
	result, err := s.databases.ListDocuments(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), q)
	if err != nil {
		return nil, err
	}
	docs := result.Documents
	out := make([]*sharedv1.Document, len(docs))
	for i := range docs {
		mapped, err := mapClientDocument(&docs[i])
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	return &clientv1.ListDocumentsResponse{
		Documents: out,
		Meta:      &sharedv1.ListResponseMeta{PageSize: ast.PageSize, TotalCount: int32(result.TotalCount), NextPageToken: result.NextPageToken},
		Distances: result.Distances,
	}, nil
}

func (s *DatabasesService) GetDocument(ctx context.Context, req *clientv1.GetDocumentRequest) (*sharedv1.Document, error) {
	projectID, err := resolveProjectID(ctx, req.GetProjectId())
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, "databases/"+req.GetDatabaseId()+"/collections/"+req.GetCollectionId()+"/documents/"+req.GetDocumentId())
	doc, err := s.databases.GetDocument(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), req.GetDocumentId())
	if err != nil {
		return nil, err
	}
	return mapClientDocument(doc)
}

func (s *DatabasesService) UpdateDocument(ctx context.Context, req *clientv1.UpdateDocumentRequest) (*sharedv1.Document, error) {
	ctx = contexts.WithAuditResource(ctx, "databases/"+req.GetDatabaseId()+"/collections/"+req.GetCollectionId()+"/documents/"+req.GetDocumentId())
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
		req.GetDatabaseId(),
		req.GetCollectionId(),
		req.GetDocumentId(),
		updateData(req.GetData()),
		perms,
		req.GetIncrement(),
		arrayUpdates,
		version,
		requestIDFromMeta(ctx, req.GetRequestId()),
	)
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	return mapClientDocument(doc)
}

func (s *DatabasesService) UpsertDocument(ctx context.Context, req *clientv1.UpsertDocumentRequest) (*sharedv1.Document, error) {
	ctx = contexts.WithAuditResource(ctx, "databases/"+req.GetDatabaseId()+"/collections/"+req.GetCollectionId()+"/documents/"+req.GetDocumentId())
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
		req.GetDatabaseId(),
		req.GetCollectionId(),
		req.GetDocumentId(),
		data,
		req.GetConflictColumns(),
		perms,
		requestIDFromMeta(ctx, req.GetRequestId()),
	)
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	return mapClientDocument(doc)
}

func (s *DatabasesService) DeleteDocument(ctx context.Context, req *clientv1.DeleteDocumentRequest) (*sharedv1.Empty, error) {
	ctx = contexts.WithAuditResource(ctx, "databases/"+req.GetDatabaseId()+"/collections/"+req.GetCollectionId()+"/documents/"+req.GetDocumentId())
	var version *int64
	if req.Version != nil {
		v := req.GetVersion()
		version = &v
	}
	replayed, err := s.databases.DeleteDocument(ctx, req.GetDatabaseId(), req.GetCollectionId(), req.GetDocumentId(), version, requestIDFromMeta(ctx, req.GetRequestId()))
	if err != nil {
		return nil, err
	}
	markReplayed(ctx, replayed)
	return &sharedv1.Empty{}, nil
}

func (s *DatabasesService) CountDocuments(ctx context.Context, req *clientv1.CountDocumentsRequest) (*clientv1.CountDocumentsResponse, error) {
	projectID, err := resolveProjectID(ctx, req.GetProjectId())
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, "databases/"+req.GetDatabaseId()+"/collections/"+req.GetCollectionId())
	q, err := documents.BindListQuery(0, "", req.GetQuery())
	if err != nil {
		return nil, err
	}
	count, err := s.databases.CountDocuments(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(), q)
	if err != nil {
		return nil, err
	}
	return &clientv1.CountDocumentsResponse{Count: count}, nil
}

// ListChanges 事件补偿（阶段④ §4.5，与 Server 面同用例核心）。
func (s *DatabasesService) ListChanges(ctx context.Context, req *clientv1.ListChangesRequest) (*clientv1.ListChangesResponse, error) {
	projectID, err := resolveProjectID(ctx, req.GetProjectId())
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, "databases/"+req.GetDatabaseId()+"/collections/"+req.GetCollectionId())
	// since_seq >= 0 由 ListChangesRequest 的 buf.validate gte 注解在
	// validate 拦截器承担（09-api-guide §2.3）。
	changes, hasMore, nextSinceSeq, err := s.databases.ListChanges(ctx, projectID, req.GetDatabaseId(), req.GetCollectionId(),
		databases.ListChangesOptions{SinceSeq: req.GetSinceSeq(), Limit: int(req.GetLimit())})
	if err != nil {
		return nil, err
	}
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
			doc, err := mapClientDocument(c.Data)
			if err != nil {
				return nil, err
			}
			mapped.Data = doc
		}
		out = append(out, mapped)
	}
	return &clientv1.ListChangesResponse{Changes: out, HasMore: hasMore, NextSinceSeq: nextSinceSeq}, nil
}

func resolveProjectID(ctx context.Context, reqProjectID string) (string, error) {
	if reqProjectID != "" {
		return reqProjectID, nil
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if values := md.Get("X-Torchwood-Project"); len(values) > 0 && values[0] != "" {
			return values[0], nil
		}
	}
	if p, ok := contexts.Principal(ctx); ok && p.ProjectID != "" {
		return p.ProjectID, nil
	}
	return "", status.Error(codes.InvalidArgument, "project_id is required")
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

// markReplayed 在幂等重放时下发 x-torchwood-replayed 响应头（HTTP 面由网关
// outgoing matcher 透传为响应头）。非 gRPC 传输 ctx 下静默跳过。
func markReplayed(ctx context.Context, replayed bool) {
	if !replayed {
		return
	}
	_ = grpc.SetHeader(ctx, metadata.Pairs("x-torchwood-replayed", "true"))
}

func mapClientDocument(doc *databases.Document) (*sharedv1.Document, error) {
	if doc == nil {
		return nil, nil
	}
	data, err := structpb.NewStruct(doc.Data)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "document data is not serializable")
	}
	out := &sharedv1.Document{
		Id:        doc.ID,
		Data:      data,
		CreatedAt: timestamppb.New(doc.CreatedAt),
		UpdatedAt: timestamppb.New(doc.UpdatedAt),
		Version:   doc.Version,
	}
	for _, p := range doc.Permissions {
		out.Permissions = append(out.Permissions, databases.FormatPermissionString(p))
	}
	return out, nil
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

func parseOptionalPermissions(items []string) ([]databases.Permission, error) {
	if len(items) == 0 {
		return nil, nil
	}
	return databases.ParsePermissionStrings(items)
}
