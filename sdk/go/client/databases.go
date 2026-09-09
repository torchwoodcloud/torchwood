package client

import (
	"context"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// DatabasesService 封装 Client API 的文档服务（动态文档 CRUD）。
// 默认数据库见 [WithDatabaseID]，可用 [Client.UseDatabase] 切换。
type DatabasesService struct {
	c  *Client
	db string
}

// newDatabasesService 构造绑定指定 db 的文档服务副本。
func newDatabasesService(c *Client, db string) *DatabasesService {
	return &DatabasesService{c: c, db: db}
}

// CreateDocument 在集合中创建文档。data 为任意 JSON 兼容结构。
func (d *DatabasesService) CreateDocument(ctx context.Context, collectionID, documentID string, data map[string]any, permissions []string) (*sharedv1.Document, error) {
	st, err := toStruct(data)
	if err != nil {
		return nil, err
	}
	return d.c.databases.CreateDocument(ctx, &clientv1.CreateDocumentRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		DocumentId:   documentID,
		Data:         st,
		Permissions:  permissions,
	})
}

// GetDocument 读取文档，不存在时返回 codes.NotFound。
func (d *DatabasesService) GetDocument(ctx context.Context, collectionID, documentID string) (*sharedv1.Document, error) {
	return d.c.databases.GetDocument(ctx, &clientv1.GetDocumentRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		DocumentId:   documentID,
	})
}

// UpdateDocument 更新文档字段；increment 对数字字段做原子增量
// （如 {"last_seq": 1}），data 与 increment 可同时使用。
// version 为用户集合 OCC 版本（GetDocument 返回的 version），必填。
func (d *DatabasesService) UpdateDocument(ctx context.Context, collectionID, documentID string, data map[string]any, increment map[string]int64, permissions []string, version int64) (*sharedv1.Document, error) {
	st, err := toStruct(data)
	if err != nil {
		return nil, err
	}
	return d.c.databases.UpdateDocument(ctx, &clientv1.UpdateDocumentRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		DocumentId:   documentID,
		Data:         st,
		Permissions:  permissions,
		Increment:    increment,
		Version:      &version,
	})
}

// UpsertDocument 按 conflictColumns（须匹配集合唯一索引）插入或更新文档。
func (d *DatabasesService) UpsertDocument(ctx context.Context, collectionID, documentID string, data map[string]any, conflictColumns, permissions []string) (*sharedv1.Document, error) {
	st, err := toStruct(data)
	if err != nil {
		return nil, err
	}
	return d.c.databases.UpsertDocument(ctx, &clientv1.UpsertDocumentRequest{
		DatabaseId:      d.db,
		CollectionId:    collectionID,
		DocumentId:      documentID,
		Data:            st,
		Permissions:     permissions,
		ConflictColumns: conflictColumns,
	})
}

// DeleteDocument 删除文档；version 为用户集合 OCC 版本（GetDocument 返回的 version），必填。
func (d *DatabasesService) DeleteDocument(ctx context.Context, collectionID, documentID string, version int64) error {
	_, err := d.c.databases.DeleteDocument(ctx, &clientv1.DeleteDocumentRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		DocumentId:   documentID,
		Version:      &version,
	})
	return err
}

// ListDocuments 按 typed AST 查询列出文档（C7 单 AST），返回文档列表与
// 下一页游标（空表示无更多）。分页走 q.PageSize/q.PageToken。
func (d *DatabasesService) ListDocuments(ctx context.Context, collectionID string, q *sharedv1.Query) ([]*sharedv1.Document, string, error) {
	req := &clientv1.ListDocumentsRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Query:        q,
	}
	resp, err := d.c.databases.ListDocuments(ctx, req)
	if err != nil {
		return nil, "", err
	}
	var next string
	if resp.Meta != nil {
		next = resp.Meta.NextPageToken
	}
	return resp.Documents, next, nil
}

// CountDocuments 按 typed AST 过滤统计文档数量。
func (d *DatabasesService) CountDocuments(ctx context.Context, collectionID string, q *sharedv1.Query) (int64, error) {
	// P3-9：CountDocuments 独立 Request（不暴露无效分页参数）。
	resp, err := d.c.databases.CountDocuments(ctx, &clientv1.CountDocumentsRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Query:        q,
	})
	if err != nil {
		return 0, err
	}
	return resp.Count, nil
}

// ListChanges 拉取集合的已提交事件流（阶段④ §4.5 补偿 API，语义详见
// Realtime 连接的 last_seq 续传约定）：seq 升序、按当前用户可见性过滤。
// 返回 nextSinceSeq 续传游标（R15 两级语义：满页 = 末条返回 seq；扫描
// 触顶 = 越过不可见块的扫描位置）——续传**优先使用本字段**，0 时回退
// 末条 change 的 seq；has_more=false 时恒为 0。游标早于重放窗口 →
// EVENTS.RESUME_EXPIRED（全量重拉后重新续传）。delete 事件为 tombstone。
func (d *DatabasesService) ListChanges(ctx context.Context, collectionID string, sinceSeq int64, limit int32) ([]*sharedv1.Change, bool, int64, error) {
	resp, err := d.c.databases.ListChanges(ctx, &clientv1.ListChangesRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		SinceSeq:     sinceSeq,
		Limit:        limit,
	})
	if err != nil {
		return nil, false, 0, err
	}
	return resp.Changes, resp.HasMore, resp.NextSinceSeq, nil
}

func toStruct(data map[string]any) (*structpb.Struct, error) {
	if len(data) == 0 {
		return nil, nil
	}
	return structpb.NewStruct(data)
}
