package client

import (
	"context"
	"fmt"

	"github.com/torchwoodcloud/torchwood/internal/app/documents"
	"github.com/torchwoodcloud/torchwood/internal/app/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

func (d *Databases) loadProject(ctx context.Context, projectID string) (*projects.Project, error) {
	if projectID == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	project, err := d.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	return project, nil
}

func clientActorOK(p *domainshared.Principal) bool {
	if p == nil || !p.IsAuthenticated() {
		return false
	}
	switch p.ActorKind {
	case domainshared.ActorKindEndUser, domainshared.ActorKindAdmin, domainshared.ActorKindService:
		return true
	default:
		return false
	}
}

// clientWriteActorOK 仅允许 EndUser 写文档（对齐拦截器 ACCESS_AUTHENTICATED 的 users 角色语义）。
// WHY: 拦截器对 Client 写已拒 API Key/admin，直调用例此前放宽为 Admin/Service，需收紧。
func clientWriteActorOK(p *domainshared.Principal) bool {
	if p == nil || !p.IsAuthenticated() {
		return false
	}
	return p.ActorKind == domainshared.ActorKindEndUser
}

func (d *Databases) resolveProject(ctx context.Context) (*projects.Project, databases.Principal, error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p.ProjectID == "" || !clientWriteActorOK(p) {
		return nil, databases.Principal{}, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	project, err := d.loadProject(ctx, p.ProjectID)
	if err != nil {
		return nil, databases.Principal{}, err
	}
	return project, p.DocPrincipal(), nil
}

func (d *Databases) resolveReadPrincipal(ctx context.Context, projectID string) (string, databases.Principal, error) {
	if p, ok := contexts.Principal(ctx); ok && clientActorOK(p) && p.ProjectID != "" {
		if projectID != "" && projectID != p.ProjectID {
			return "", databases.Principal{}, status.Error(codes.InvalidArgument, "project_id mismatch")
		}
		project, err := d.loadProject(ctx, p.ProjectID)
		if err != nil {
			return "", databases.Principal{}, err
		}
		return project.ID, p.DocPrincipal(), nil
	}
	if projectID == "" {
		if p, ok := contexts.Principal(ctx); ok && p.ProjectID != "" {
			projectID = p.ProjectID
		}
	}
	project, err := d.loadProject(ctx, projectID)
	if err != nil {
		return "", databases.Principal{}, err
	}
	return project.ID, databases.GuestPrincipal, nil
}

func (d *Databases) ensureCollection(ctx context.Context, databaseID, collectionID string) (string, databases.Principal, error) {
	project, principal, err := d.resolveProject(ctx)
	if err != nil {
		return "", databases.Principal{}, err
	}
	return d.ensureCollectionForProject(ctx, project.ID, databaseID, collectionID, principal, false)
}

func (d *Databases) ensureCollectionForRead(ctx context.Context, projectID, databaseID, collectionID string) (string, databases.Principal, error) {
	pid, principal, err := d.resolveReadPrincipal(ctx, projectID)
	if err != nil {
		return "", databases.Principal{}, err
	}
	return d.ensureCollectionForProject(ctx, pid, databaseID, collectionID, principal, true)
}

func (d *Databases) ensureCollectionForProject(ctx context.Context, projectID, databaseID, collectionID string, principal databases.Principal, readOnly bool) (string, databases.Principal, error) {
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return "", databases.Principal{}, err
	}
	if databases.IsSystemCollection(projectID, databaseID, collectionID) && !readOnly {
		return "", databases.Principal{}, shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	col, err := d.docDB.GetCollection(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return "", databases.Principal{}, err
	}
	if col == nil {
		return "", databases.Principal{}, status.Error(codes.NotFound, "collection not found")
	}
	if col.Disabled {
		return "", databases.Principal{}, shared.MapDocumentDBError(databases.ErrPermissionDenied)
	}
	return projectID, principal, nil
}

func (d *Databases) CreateDocument(
	ctx context.Context,
	databaseID, collectionID, documentID string,
	data map[string]any,
	perms []databases.Permission,
	requestID string,
) (*databases.Document, bool, error) {
	projectID, principal, err := d.ensureCollection(ctx, databaseID, collectionID)
	if err != nil {
		return nil, false, err
	}
	p, _ := contexts.Principal(ctx)
	if len(perms) == 0 {
		perms = ownerDocumentPermissions(p.OwnerID())
	}
	return d.documentsCore().CreateDocument(ctx, projectID, databaseID, collectionID, documentID, data, perms, principal, requestID, documents.WriteOptions{})
}

func (d *Databases) ListDocuments(
	ctx context.Context,
	projectID, databaseID, collectionID string,
	q databases.Query,
) (*documents.ListDocumentsResult, error) {
	pid, principal, err := d.ensureCollectionForRead(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, err
	}
	return d.documentsCore().ListDocuments(ctx, pid, databaseID, collectionID, q, principal)
}

func (d *Databases) GetDocument(ctx context.Context, projectID, databaseID, collectionID, documentID string) (*databases.Document, error) {
	pid, principal, err := d.ensureCollectionForRead(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, err
	}
	return d.documentsCore().GetDocument(ctx, pid, databaseID, collectionID, documentID, principal)
}

func (d *Databases) UpdateDocument(
	ctx context.Context,
	databaseID, collectionID, documentID string,
	data map[string]any,
	perms []databases.Permission,
	increment map[string]int64,
	arrayUpdates map[string]databases.ArrayUpdate,
	version *int64,
	requestID string,
) (*databases.Document, bool, error) {
	projectID, principal, err := d.ensureCollection(ctx, databaseID, collectionID)
	if err != nil {
		return nil, false, err
	}
	if err := shared.UpdateDocumentVersionRequired(version); err != nil {
		return nil, false, err
	}
	return d.documentsCore().UpdateDocument(ctx, projectID, databaseID, collectionID, documentID, data, perms, increment, arrayUpdates, principal, version, requestID, documents.WriteOptions{})
}

func (d *Databases) UpsertDocument(
	ctx context.Context,
	databaseID, collectionID, documentID string,
	data map[string]any,
	conflictColumns []string,
	perms []databases.Permission,
	requestID string,
) (*databases.Document, bool, error) {
	projectID, principal, err := d.ensureCollection(ctx, databaseID, collectionID)
	if err != nil {
		return nil, false, err
	}
	p, _ := contexts.Principal(ctx)
	if len(perms) == 0 {
		perms = ownerDocumentPermissions(p.OwnerID())
	}
	if len(data) == 0 {
		return nil, false, status.Error(codes.InvalidArgument, "data is required")
	}
	return d.documentsCore().UpsertDocument(ctx, projectID, databaseID, collectionID, documentID, data, conflictColumns, perms, principal, requestID, documents.WriteOptions{})
}

func (d *Databases) DeleteDocument(ctx context.Context, databaseID, collectionID, documentID string, version *int64, requestID string) (bool, error) {
	projectID, principal, err := d.ensureCollection(ctx, databaseID, collectionID)
	if err != nil {
		return false, err
	}
	return d.documentsCore().DeleteDocument(ctx, projectID, databaseID, collectionID, documentID, principal, version, requestID)
}

func (d *Databases) CountDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query) (int64, error) {
	pid, principal, err := d.ensureCollectionForRead(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return 0, err
	}
	return d.documentsCore().CountDocuments(ctx, pid, databaseID, collectionID, q, principal)
}

// ListChanges 事件补偿入口（阶段④ §4.5，与 Server 面同用例核心）：守卫
// 与 CountDocuments 同链；可见性过滤在 infra ChangeFeed。nextSinceSeq 为
// R15 扫描游标（两级语义见 ChangeFeed 端口注释）。
func (d *Databases) ListChanges(ctx context.Context, projectID, databaseID, collectionID string, opts databases.ListChangesOptions) ([]databases.DocumentChange, bool, int64, error) {
	pid, principal, err := d.ensureCollectionForRead(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, false, 0, err
	}
	changes, hasMore, nextSinceSeq, err := d.documentsCore().DocumentDB().ListChanges(ctx, pid, databaseID, collectionID, opts, principal)
	if err != nil {
		return nil, false, 0, shared.MapDocumentDBError(err)
	}
	return changes, hasMore, nextSinceSeq, nil
}

func ownerDocumentPermissions(userID string) []databases.Permission {
	userRole := fmt.Sprintf("user:%s", userID)
	return []databases.Permission{
		{Type: "read", Role: userRole},
		{Type: "update", Role: userRole},
		{Type: "delete", Role: userRole},
	}
}
