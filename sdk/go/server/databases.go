package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// DatabasesService 封装 Server API 的数据库管理服务
// （库/集合/属性/索引/文档，含 upsert 与批量操作）。
// 默认数据库见 [WithDatabaseID]，可用 [Client.UseDatabase] 切换。
type DatabasesService struct {
	c  *Client
	db string
}

// newDatabasesService 构造绑定指定 db 的文档服务副本。
func newDatabasesService(c *Client, db string) *DatabasesService {
	return &DatabasesService{c: c, db: db}
}

// CreateDatabase 创建数据库。
func (d *DatabasesService) CreateDatabase(ctx context.Context, id, name string) (*serverv1.Database, error) {
	return d.c.databases.CreateDatabase(ctx, &serverv1.CreateDatabaseRequest{
		Id:   id,
		Name: name,
	})
}

// GetDatabase 获取数据库。
func (d *DatabasesService) GetDatabase(ctx context.Context, id string) (*serverv1.Database, error) {
	return d.c.databases.GetDatabase(ctx, &serverv1.GetDatabaseRequest{Id: id})
}

// DeleteDatabase 删除数据库（级联清理其下集合）。
func (d *DatabasesService) DeleteDatabase(ctx context.Context, id string) error {
	_, err := d.c.databases.DeleteDatabase(ctx, &serverv1.GetDatabaseRequest{Id: id})
	return err
}

// ListDatabases 列出数据库。
func (d *DatabasesService) ListDatabases(ctx context.Context, queries []string, pageSize int32, pageToken string) (*serverv1.ListDatabasesResponse, error) {
	return d.c.databases.ListDatabases(ctx, &sharedv1.ListRequest{
		PageSize:  pageSize,
		PageToken: pageToken,
		Queries:   queries,
	})
}

// CreateCollection 创建集合。documentSecurity 开启文档级权限。
func (d *DatabasesService) CreateCollection(ctx context.Context, collectionID, name string, permissions []string, documentSecurity bool) (*serverv1.Collection, error) {
	return d.c.databases.CreateCollection(ctx, &serverv1.CreateCollectionRequest{
		DatabaseId:       d.db,
		Id:               collectionID,
		Name:             name,
		Permissions:      permissions,
		DocumentSecurity: &documentSecurity,
	})
}

// GetCollection 获取集合。
func (d *DatabasesService) GetCollection(ctx context.Context, collectionID string) (*serverv1.Collection, error) {
	return d.c.databases.GetCollection(ctx, &serverv1.GetCollectionRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
	})
}

// UpdateCollection 更新集合；name 空串与 permissions 为 nil 表示不修改，
// documentSecurity/disabled 用指针区分「不修改」与「显式设 false」。
func (d *DatabasesService) UpdateCollection(ctx context.Context, collectionID, name string, permissions []string, documentSecurity, disabled *bool) (*serverv1.Collection, error) {
	req := &serverv1.UpdateCollectionRequest{
		DatabaseId:       d.db,
		CollectionId:     collectionID,
		DocumentSecurity: documentSecurity,
		Disabled:         disabled,
	}
	// name 为 proto3 optional（R10-P1-6）：空串 = 不修改（保持旧签名语义）。
	if name != "" {
		req.Name = &name
	}
	if permissions != nil {
		req.Permissions = &serverv1.PermissionsUpdate{Values: permissions}
	}
	return d.c.databases.UpdateCollection(ctx, req)
}

// DeleteCollection 删除集合。
func (d *DatabasesService) DeleteCollection(ctx context.Context, collectionID string) error {
	_, err := d.c.databases.DeleteCollection(ctx, &serverv1.GetCollectionRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
	})
	return err
}

// ListCollections 按查询 DSL 列出集合。
func (d *DatabasesService) ListCollections(ctx context.Context, queries []string, pageSize int32, pageToken string) (*serverv1.ListCollectionsResponse, error) {
	return d.c.databases.ListCollections(ctx, &serverv1.ListCollectionsRequest{
		DatabaseId: d.db,
		Queries:    queries,
		PageSize:   pageSize,
		PageToken:  pageToken,
	})
}

// CreateAttribute 为集合添加属性（type 支持 string/integer/float/boolean/datetime/email/url/json 等）。
func (d *DatabasesService) CreateAttribute(ctx context.Context, collectionID, key, attrType string, size int32, required, array bool) (*serverv1.Attribute, error) {
	return d.c.databases.CreateAttribute(ctx, &serverv1.CreateAttributeRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Key:          key,
		Type:         attrType,
		Size:         size,
		Required:     required,
		Array:        array,
	})
}

// DeleteAttribute 删除集合属性（B4 删列两段段一：置 deprecated——读屏蔽写拒收，
// 可 RestoreAttribute 回滚；物理删列走 RetireAttribute）。
func (d *DatabasesService) DeleteAttribute(ctx context.Context, collectionID, key string) error {
	_, err := d.c.databases.DeleteAttribute(ctx, &serverv1.DeleteAttributeRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Key:          key,
	})
	return err
}

// RestoreAttribute 回滚属性生命周期（B4）：deprecated → active；migrating →
// 中止迁移并恢复 active。
func (d *DatabasesService) RestoreAttribute(ctx context.Context, collectionID, key string) error {
	_, err := d.c.databases.RestoreAttribute(ctx, &serverv1.RestoreAttributeRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Key:          key,
	})
	return err
}

// RetireAttribute 物理删列（B4 删列两段段二，不可逆）：deprecated 属性的物理
// 列或 copy 迁移 swap 后的旧列残留删除。
func (d *DatabasesService) RetireAttribute(ctx context.Context, collectionID, key string) error {
	_, err := d.c.databases.RetireAttribute(ctx, &serverv1.RetireAttributeRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Key:          key,
	})
	return err
}

// MigrateAttribute 创建 copy 迁移任务（B4）：改类型/收紧走异步回填 + 原子
// swap（响应为任务读回）；放宽（扩宽/required→optional）即时 ALTER。
func (d *DatabasesService) MigrateAttribute(ctx context.Context, collectionID, key, attrType string, size int32, required, array bool) (*serverv1.AttributeMigration, error) {
	return d.c.databases.MigrateAttribute(ctx, &serverv1.MigrateAttributeRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Key:          key,
		Type:         attrType,
		Size:         size,
		Required:     required,
		Array:        array,
	})
}

// CreateIndex 为集合创建索引（type 支持 key/unique/fulltext）。
func (d *DatabasesService) CreateIndex(ctx context.Context, collectionID, id, indexType string, attributes []string) (*serverv1.Index, error) {
	return d.c.databases.CreateIndex(ctx, &serverv1.CreateIndexRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Id:           id,
		Type:         indexType,
		Attributes:   attributes,
	})
}

// DeleteIndex 删除集合索引。
func (d *DatabasesService) DeleteIndex(ctx context.Context, collectionID, indexID string) error {
	_, err := d.c.databases.DeleteIndex(ctx, &serverv1.DeleteIndexRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		IndexId:      indexID,
	})
	return err
}

// CreateDocument 在集合中创建文档。
func (d *DatabasesService) CreateDocument(ctx context.Context, collectionID, documentID string, data map[string]any, permissions []string) (*sharedv1.Document, error) {
	st, err := toStruct(data)
	if err != nil {
		return nil, err
	}
	return d.c.databases.CreateDocument(ctx, &serverv1.CreateDocumentRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		DocumentId:   documentID,
		Data:         st,
		Permissions:  permissions,
	})
}

// GetDocument 读取文档，不存在时返回 codes.NotFound。
func (d *DatabasesService) GetDocument(ctx context.Context, collectionID, documentID string) (*sharedv1.Document, error) {
	return d.c.databases.GetDocument(ctx, &serverv1.GetDocumentRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		DocumentId:   documentID,
	})
}

// UpdateDocument 更新文档字段；increment 对数字字段做原子增量。
// version 为用户集合 OCC 版本（GetDocument 返回的 version），必填。
func (d *DatabasesService) UpdateDocument(ctx context.Context, collectionID, documentID string, data map[string]any, increment map[string]int64, permissions []string, version int64) (*sharedv1.Document, error) {
	st, err := toStruct(data)
	if err != nil {
		return nil, err
	}
	return d.c.databases.UpdateDocument(ctx, &serverv1.UpdateDocumentRequest{
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
	return d.c.databases.UpsertDocument(ctx, &serverv1.UpsertDocumentRequest{
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
	_, err := d.c.databases.DeleteDocument(ctx, &serverv1.DeleteDocumentRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		DocumentId:   documentID,
		Version:      &version,
	})
	return err
}

// DocumentsResult 是 ListDocuments 的结果（会话 #10 起 KNN 距离随行返回）：
// Distances 与 Documents 平行，仅 vector_search 查询时非空。
type DocumentsResult struct {
	Documents     []*sharedv1.Document
	NextPageToken string
	Distances     []float64
}

// ListDocuments 按 typed AST 查询列出文档（C7 单 AST：服务端不再消费 DSL
// 字符串；分页走 q.PageSize/q.PageToken）。vector_search（KNN）查询时
// result.Distances 与 result.Documents 平行回传距离。
func (d *DatabasesService) ListDocuments(ctx context.Context, collectionID string, q *sharedv1.Query) (*DocumentsResult, error) {
	req := &serverv1.ListDocumentsRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Query:        q,
	}
	resp, err := d.c.databases.ListDocuments(ctx, req)
	if err != nil {
		return nil, err
	}
	var next string
	if resp.Meta != nil {
		next = resp.Meta.NextPageToken
	}
	return &DocumentsResult{
		Documents:     resp.Documents,
		NextPageToken: next,
		Distances:     resp.Distances,
	}, nil
}

// DocumentsPager 是 ListDocuments 的 AIP-158 分页迭代器（P3-15）。
// 自动处理 page_token 续拉，直至 next_page_token 为空。vector_search（KNN，
// B2）同样支持：服务端发放 kvc: 距离游标，Next 透传续拉、跨页不重不漏。
type DocumentsPager struct {
	svc          *DatabasesService
	collectionID string
	query        *sharedv1.Query
	pageSize     int32
	nextToken    string
	done         bool
}

// NewDocumentsPager 创建文档分页迭代器（filter/orders 取自 q；pageSize 覆盖
// q.PageSize）。pageSize<=0 时默认 50。
func (d *DatabasesService) NewDocumentsPager(collectionID string, q *sharedv1.Query, pageSize int32) *DocumentsPager {
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return &DocumentsPager{svc: d, collectionID: collectionID, query: q, pageSize: pageSize}
}

// Next 拉取下一页，返回文档列表；已到末尾返回 nil,nil。
func (p *DocumentsPager) Next(ctx context.Context) ([]*sharedv1.Document, error) {
	if p.done {
		return nil, nil
	}
	// proto 消息含锁不可值拷贝；分页参数逐字段组装（filter/orders/select/
	// vector_search 指针共享只读）。
	q := &sharedv1.Query{PageSize: p.pageSize, PageToken: p.nextToken}
	if p.query != nil {
		q.Filter = p.query.GetFilter()
		q.Orders = p.query.GetOrders()
		q.Select = p.query.GetSelect()
		q.VectorSearch = p.query.GetVectorSearch()
	}
	res, err := p.svc.ListDocuments(ctx, p.collectionID, q)
	if err != nil {
		return nil, err
	}
	p.nextToken = res.NextPageToken
	if res.NextPageToken == "" {
		p.done = true
	}
	return res.Documents, nil
}

// All 顺序拉取所有页并合并（注意大集合内存占用）。
func (p *DocumentsPager) All(ctx context.Context) ([]*sharedv1.Document, error) {
	var all []*sharedv1.Document
	for {
		docs, err := p.Next(ctx)
		if err != nil {
			return nil, err
		}
		if len(docs) == 0 {
			break
		}
		all = append(all, docs...)
		if p.done {
			break
		}
	}
	return all, nil
}

// HasMore 报告是否还有更多页。
func (p *DocumentsPager) HasMore() bool { return !p.done }

// CountDocuments 按 typed AST 过滤统计文档数量（P3-9：独立 Request，不含分页字段）。
func (d *DatabasesService) CountDocuments(ctx context.Context, collectionID string, q *sharedv1.Query) (int64, error) {
	resp, err := d.c.databases.CountDocuments(ctx, &serverv1.CountDocumentsRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Query:        q,
	})
	if err != nil {
		return 0, err
	}
	return resp.Count, nil
}

// AggregateDocuments 在权限过滤后的可见行集上聚合（sum/avg/min/max +
// 可选单键 groupBy；groupBy 空 = 不分组）。q 为 typed AST 过滤
// （与 ListDocuments 同形；排序/分页算子无意义）。
// 结果类型化：integer 属性的 sum/min/max → AggregateValue_Int64Value
// （int64 精确，>2^53 可靠）；avg 与 float 属性 → AggregateValue_DoubleValue。
// 注意：文档 Data 的 number 通道是 double——业务值可能超过 2^53 时，
// 属性请用 integer（聚合 int64 通道）或 string 承载。
func (d *DatabasesService) AggregateDocuments(ctx context.Context, collectionID string, q *sharedv1.Query, aggregations []*serverv1.AggregateSpec, groupBy string) (*serverv1.AggregateDocumentsResponse, error) {
	req := &serverv1.AggregateDocumentsRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		Query:        q,
		Aggregations: aggregations,
	}
	if groupBy != "" {
		req.GroupBy = &groupBy
	}
	return d.c.databases.AggregateDocuments(ctx, req)
}

// ListChanges 拉取集合的已提交事件流（阶段④ §4.5 补偿 API）：seq 升序、
// 按本 key 的可见性过滤。sinceSeq 为续传游标（0 = 从最老可用事件起）；
// 返回变更列表、has_more 与续传游标 nextSinceSeq（R15 两级语义：满页 =
// 末条返回 seq；扫描触顶 = 越过不可见块的扫描位置）——续传**优先使用
// nextSinceSeq**，仅当为 0 时回退末条 change 的 seq；has_more=false 时
// nextSinceSeq 恒为 0。
// 游标早于重放窗口 → 错误域码 EVENTS.RESUME_EXPIRED（FailedPrecondition），
// 此时应全量重拉（ListDocuments）后以最新事件 Seq 重新续传。
// 事件语义：seq 集合内为分配序（跨文档不保证与提交序一致，且可能有
// 空洞——空洞 = 回滚事务，不丢事件）；同文档事件严格按 seq 全序；
// delete 事件 Data 为 nil（tombstone：document_id + version 标识删除）；
// transaction_id 非空表示来自 execute-tx 原子批（批内顺序 = op 序）。
// 客户端按 event_id 幂等去重（at-least-once）。
func (d *DatabasesService) ListChanges(ctx context.Context, collectionID string, sinceSeq int64, limit int32) ([]*sharedv1.Change, bool, int64, error) {
	resp, err := d.c.databases.ListChanges(ctx, &serverv1.ListChangesRequest{
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

// ChangesPager 是 ListChanges 的续传迭代器：自动以续传游标翻页——
// 优先 next_since_seq（越过不可见事件块，R15），0 时回退末条 change 的
// seq——直至 has_more=false。
type ChangesPager struct {
	svc          *DatabasesService
	collectionID string
	limit        int32
	sinceSeq     int64
	done         bool
}

// NewChangesPager 创建变更流续传迭代器（limit<=0 时默认 500）。
func (d *DatabasesService) NewChangesPager(collectionID string, sinceSeq int64, limit int32) *ChangesPager {
	if limit <= 0 {
		limit = 500
	}
	return &ChangesPager{svc: d, collectionID: collectionID, limit: limit, sinceSeq: sinceSeq}
}

// Next 拉取下一页；已到末尾返回 nil,nil。
func (p *ChangesPager) Next(ctx context.Context) ([]*sharedv1.Change, error) {
	if p.done {
		return nil, nil
	}
	changes, hasMore, nextSinceSeq, err := p.svc.ListChanges(ctx, p.collectionID, p.sinceSeq, p.limit)
	if err != nil {
		return nil, err
	}
	if nextSinceSeq > 0 {
		p.sinceSeq = nextSinceSeq
	} else if len(changes) > 0 {
		p.sinceSeq = changes[len(changes)-1].GetSeq()
	}
	p.done = !hasMore
	return changes, nil
}

// BulkUpdateDocuments 批量更新文档，返回受影响数量。
func (d *DatabasesService) BulkUpdateDocuments(ctx context.Context, collectionID string, documentIDs []string, data map[string]any, permissions []string) (*serverv1.BulkDocumentsResponse, error) {
	st, err := toStruct(data)
	if err != nil {
		return nil, err
	}
	return d.c.databases.BulkUpdateDocuments(ctx, &serverv1.BulkUpdateDocumentsRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		DocumentIds:  documentIDs,
		Data:         st,
		Permissions:  permissions,
	})
}

// BulkDeleteDocuments 批量删除文档，返回受影响数量。
func (d *DatabasesService) BulkDeleteDocuments(ctx context.Context, collectionID string, documentIDs []string) (*serverv1.BulkDocumentsResponse, error) {
	return d.c.databases.BulkDeleteDocuments(ctx, &serverv1.BulkDeleteDocumentsRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
		DocumentIds:  documentIDs,
	})
}

// ExecuteTransactions 在单事务内执行异构 op 批（事务内核 Phase 1）：
// ATOMIC（mode 为空/ATOMIC）任一 op 失败整批回滚；PARTIAL 逐 op 独立执行，
// 已成功不回滚，返回 per-op 结果。op 数上限 1000。
func (d *DatabasesService) ExecuteTransactions(ctx context.Context, ops []*serverv1.TransactionOp, mode serverv1.TransactionMode) (*serverv1.ExecuteTransactionsResponse, error) {
	return d.c.databases.ExecuteTransactions(ctx, &serverv1.ExecuteTransactionsRequest{
		DatabaseId: d.db,
		Ops:        ops,
		Mode:       mode,
	})
}

// ExportCollectionSchema 导出集合契约的 JSON Schema 2020-12 文档（B10，
// redesign §4.1 Agent 面 / §10.1）：响应 .Schema 即完整 JSON 文档（用户属性
// 类型映射 + required + 系统字段 readOnly 注释）。as 为导出形态，现阶段仅
// "jsonschema"（空串同义缺省）；集合不存在返回 codes.NotFound。
func (d *DatabasesService) ExportCollectionSchema(ctx context.Context, collectionID, as string) (*serverv1.ExportCollectionSchemaResponse, error) {
	req := &serverv1.ExportCollectionSchemaRequest{
		DatabaseId:   d.db,
		CollectionId: collectionID,
	}
	if as != "" {
		req.As = &as
	}
	return d.c.databases.ExportCollectionSchema(ctx, req)
}

// UseDatabase 返回绑定指定 databaseID 的文档服务副本。
func (c *Client) UseDatabase(databaseID string) *DatabasesService {
	return newDatabasesService(c, databaseID)
}
