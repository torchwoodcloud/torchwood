// 文档查询面：List/Count/Sum 与 keyset 分页 token（ka:/kb:，W-D）；KNN 距离
// 游标（kvc:，B2 多页 KNN）。
// 阶段③包 B：三个入口经 withDocumentTx 包进带 GUC 注入的只读事务（A1）。
package documentdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/pkg/query"
)

func (p *postgresDocumentDB) ListDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query, principal databases.Principal) (*databases.DocumentList, error) {
	var out *databases.DocumentList
	err := p.withDocumentTx(ctx, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		list, err := p.listDocuments(txCtx, projectID, databaseID, collectionID, q, principal)
		if err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, p.mapError(err)
	}
	return out, nil
}

func (p *postgresDocumentDB) listDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query, principal databases.Principal) (*databases.DocumentList, error) {
	internalID, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, err
	}
	parsed, err := astFrom(q)
	if err != nil {
		return nil, p.mapError(err)
	}
	tbl := tableName(schema, physical)

	// 非 System 路径显式获取集合一次（coll==nil → NotFound，行为从 403 收紧为 404），
	// 复用给权限过滤与字段白名单校验；System 信任路径零额外查询（跳过白名单）。
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	var coll *databases.Collection
	if !principal.BypassesDocumentACL() {
		coll, err = p.GetCollection(ctx, projectID, databaseID, collectionID)
		if err != nil {
			return nil, p.mapError(err)
		}
		if coll == nil {
			return nil, status.Error(codes.NotFound, "collection not found")
		}
	}

	whereParts := []string{"d._tenant = ?"}
	args := []any{internalID}
	// 判定执行点（阶段③包 C）：业务集合由 SELECT policy（tw_visible）隐式
	// 过滤——不拼权限 WHERE；sentinel 系统集合（静态平面，预决策 9）保留
	// 应用层过滤谓词。
	if !principal.BypassesDocumentACL() && isSystem {
		permWhere, permArgs, err := p.listPermissionFilter(ctx, coll, principal)
		if err != nil {
			return nil, p.mapError(err)
		}
		if permWhere != "" {
			whereParts = append(whereParts, permWhere)
			args = append(args, permArgs...)
		}
	}

	if coll != nil {
		if err := p.validateQueryFields(ctx, schema, physical, parsed, coll, collectionID, isSystem); err != nil {
			return nil, p.mapError(err)
		}
	}

	filterWhere, filterArgs, orderSQL, err := buildAppwriteQuery(parsed, arrayTypesOf(coll))
	if err != nil {
		return nil, p.mapError(err)
	}
	if filterWhere != "" {
		whereParts = append(whereParts, filterWhere)
		args = append(args, filterArgs...)
	}

	// KNN 分支（会话 #10 包 C，预决策 3/4/6）：iterative scan 语义 = 返回
	// k 个满足全部过滤（policy + filter）的近邻——SET LOCAL 开启（GUC 默认
	// off，原型 2 实证 off 时"先取全局 k 再滤"召回错误；事务级注入零残留）。
	// max_distance 不进 WHERE（原型 2 实证距离谓词使规划器放弃 HNSW 索引），
	// 在 top-k 结果上后置过滤（语义等价：top-k ⊇ 全部 ≤ 阈值的可见行）。
	// 距离随行返回（distances 与 documents 平行），不污染 Data/事件。
	if parsed.VectorSearch != nil {
		return p.listDocumentsKNN(ctx, projectID, databaseID, collectionID, schema, physical, tbl, parsed, whereParts, args)
	}

	// 页大小归一（C7/R9b）：astFrom 已把请求级 page_size 并进 AST，此处读
	// 归一结果；0/负数回退默认 50，上限 clamp（maxQueryLimit）。
	limit := parsed.Limit
	if limit == 0 {
		limit = int(parsed.PageSize)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > maxQueryLimit {
		limit = maxQueryLimit
	}
	// keyset-only（redesign C2 阶段①收敛）：offset() 算子与非 keyset token
	// 一律拒绝——offset 族 token 已停发（本方法只发 ka:/kb:），旧 token 到此
	// 显式失败，不再静默切回 OFFSET 语义。
	if parsed.Offset != 0 {
		return nil, p.mapError(status.Error(codes.InvalidArgument, "offset() is not supported on ListDocuments; use cursor pagination"))
	}
	if q.PageToken != "" {
		if id, kind, ok := decodeKeysetToken(q.PageToken); ok {
			if kind == "before" {
				parsed.CursorBefore = id
			} else {
				parsed.CursorAfter = id
			}
		} else {
			return nil, p.mapError(status.Error(codes.InvalidArgument, "invalid page token: keyset token required, offset tokens are no longer accepted"))
		}
	}

	cursor := ""
	cursorKind := ""
	if parsed.CursorAfter != "" {
		cursor, cursorKind = parsed.CursorAfter, "after"
	} else if parsed.CursorBefore != "" {
		cursor, cursorKind = parsed.CursorBefore, "before"
	}
	// 排序键全集（C2 完成态）：无显式排序默认 _created_at DESC；ORDER BY =
	// 全部排序键 + _id tiebreaker（方向随首键）——首页与 cursor 页同构的全序，
	// keyset token 跨页稳定性的机制保证。
	sortKeys := make([]sortKey, 0, len(parsed.Orders)+1)
	if len(parsed.Orders) == 0 {
		sortKeys = append(sortKeys, sortKey{field: "_created_at", dir: "DESC"})
	} else {
		for _, o := range parsed.Orders {
			field := mapQueryField(o.Attribute)
			if !safeNameRe.MatchString(field) {
				return nil, p.mapError(status.Error(codes.InvalidArgument, fmt.Sprintf("invalid order field: %s", o.Attribute)))
			}
			dir := "ASC"
			if o.Desc {
				dir = "DESC"
			}
			sortKeys = append(sortKeys, sortKey{field: field, dir: dir})
		}
	}
	var orderParts []string
	for _, k := range sortKeys {
		orderParts = append(orderParts, fmt.Sprintf("d.%s %s", quoteIdent(k.field), k.dir))
	}
	orderParts = append(orderParts, fmt.Sprintf("d._id %s", sortKeys[0].dir))
	orderSQL = "ORDER BY " + strings.Join(orderParts, ", ")

	if cursor != "" {
		if err := validateDocID(cursor); err != nil {
			return nil, p.mapError(err)
		}
		// cursor 行按 docID 定位，服务端查行取全部排序键值（token 仍只编码
		// docID；ka:/kb: 前缀 + docID，与单键时代同形态）。
		cols := make([]string, len(sortKeys))
		for i, k := range sortKeys {
			cols[i] = quoteIdent(k.field)
		}
		values := make([]any, len(sortKeys))
		scanVals := make([]any, len(sortKeys))
		for i := range values {
			scanVals[i] = &values[i]
		}
		err := p.conn(ctx).QueryRowContext(ctx,
			fmt.Sprintf(`SELECT %s FROM %s WHERE _id = ? AND _tenant = ?`, strings.Join(cols, ", "), tbl),
			cursor, internalID,
		).Scan(scanVals...)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, p.mapError(status.Error(codes.InvalidArgument, "cursor document not found"))
			}
			return nil, p.mapError(err)
		}
		// NULL 排序键不支持游标（预决策 4）：行比较谓词对 NULL 求值为 NULL
		// 会被静默排除，cursor 定位含 NULL 键的行即拒——要求调用方先过滤
		//（isNull/isNotNull）再分页。数据行含 NULL 键在续页中被跳过是同源
		// 已知限制（见 docs/developer/06-databases.md §9）。
		for i, v := range values {
			if v == nil {
				return nil, p.mapError(status.Error(codes.InvalidArgument,
					fmt.Sprintf("cursor order key %s is NULL; NULL sort keys are not supported by cursor pagination, filter them out first", sortKeys[i].field)))
			}
		}
		keysetSQL, keysetArgs, err := buildKeysetPredicate(sortKeys, values, cursor, cursorKind)
		if err != nil {
			return nil, p.mapError(err)
		}
		whereParts = append(whereParts, keysetSQL)
		args = append(args, keysetArgs...)
	}

	// W-D：keyset 续页不产出精确 total——COUNT 与数据查询同价（含 EXISTS
	// 权限子查询），对游标续页无意义且把翻页成本翻倍；首页保持精确计数
	//（proto 语义 total_count <= 0 = unknown）。
	var total int64
	if cursor == "" {
		countSQL := fmt.Sprintf(`SELECT COUNT(*) FROM %s d WHERE %s`, tbl, strings.Join(whereParts, " AND "))
		if err := p.conn(ctx).QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
			return nil, p.mapError(err)
		}
	}

	// vector 列（会话 #10）：读回投影逐列覆盖为 JSON 数组（原型 3：to_jsonb
	// 对 vector 输出字符串，Data 契约要求 JSON 数组）。
	vectorCols, err := p.vectorColumnsOf(ctx, projectID, databaseID, collectionID, schema, physical)
	if err != nil {
		return nil, p.mapError(err)
	}
	querySQL := fmt.Sprintf(`SELECT %s AS doc FROM %s d WHERE %s %s LIMIT ?`, vectorProjection(vectorCols), tbl, strings.Join(whereParts, " AND "), orderSQL)
	args = append(args, limit)

	rows, err := p.conn(ctx).QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, p.mapError(err)
	}
	defer func() { _ = rows.Close() }()

	var docs []databases.Document
	for rows.Next() {
		doc, err := scanDocumentJSON(rows)
		if err != nil {
			return nil, p.mapError(err)
		}
		docs = append(docs, *doc)
	}
	if err := rows.Err(); err != nil {
		return nil, p.mapError(err)
	}
	// B6：List 回传 permissions（与 Get 对齐）——_acl 含在 to_jsonb(d.*) 载荷内
	// 顺带解析（阶段③包 A 权限回填免费化，W-D 的批量 IN 查询已删除）。

	if len(parsed.Selects) > 0 {
		selected := make(map[string]struct{}, len(parsed.Selects))
		for _, s := range parsed.Selects {
			selected[mapQueryField(s)] = struct{}{}
		}
		for i := range docs {
			for k := range docs[i].Data {
				if _, ok := selected[k]; !ok {
					delete(docs[i].Data, k)
				}
			}
		}
	}
	// B4 读屏蔽：deprecated 属性读回剥离（coll 已由 validateQueryFields 加载
	// 时复用；SystemPrincipal 旁路路径此处补载——masking 是读契约，不分主体；
	// 无 deprecated 属性时零改动跳过）。
	if coll == nil {
		var cerr error
		coll, cerr = p.GetCollection(ctx, projectID, databaseID, collectionID)
		if cerr != nil {
			return nil, p.mapError(cerr)
		}
	}
	maskDeprecatedData(coll.Attributes, docs)

	// 满页即发 keyset 续页 token（编码边界行 id，方向沿用本次请求）；不满页
	// 无 next=尾页。has-more 以满页判定（续页无精确 total）。
	next := ""
	if len(docs) == limit {
		if cursorKind == "before" {
			next = encodeKeysetToken("before", docs[0].ID)
		} else {
			next = encodeKeysetToken("after", docs[len(docs)-1].ID)
		}
	}
	return &databases.DocumentList{
		Documents:     docs,
		TotalCount:    total,
		NextPageToken: next,
	}, nil
}

// listDocumentsKNN 是 vector_search 的查询管道（会话 #10 包 C；B2 多页 KNN）。
// 调用前提：validateQueryFields 已通过（vector 属性 + hnsw 索引 metric 匹配
// + 维度等长 + 与 orders 互斥在 codec 层拒绝）。whereParts/args 已含
// _tenant、sentinel 权限谓词与普通 filter（AND 组合）。
//
// 首页 SQL 形态：SELECT <proj>, (col <op> $vec) AS __dist ... WHERE <filters>
// ORDER BY col <op> $vec LIMIT k。RLS policy（tw_visible）作为 securityQuals
// 隐式过滤参与 iterative scan（GUC strict_order，原型 2 验证：1000 行 5 行
// 可见仍返回 5/5 正确近邻；off 时仅 1/5——"先取全局 k 再滤"）。strict_order
// 保序（distances 回传与文档顺序一致；relaxed_order 需外层重排，不采用）。
//
// 多页（B2）：pageToken 携带 kvc: 距离游标时走续页管道——(dist,_id) 精确
// 全序扫描 + 阈值谓词（见 knnContinuationSQL）。
func (p *postgresDocumentDB) listDocumentsKNN(ctx context.Context, projectID, databaseID, collectionID, schema, physical, tbl string, parsed *query.Query, whereParts []string, args []any) (*databases.DocumentList, error) {
	vs := parsed.VectorSearch
	// limit 即 k（预决策 3）：page_size/limit 语义合并，缺省 25（§4.1），
	// 上限 maxQueryLimit。
	k := parsed.Limit
	if k == 0 {
		k = int(parsed.PageSize)
	}
	if k <= 0 {
		k = 25
	}
	if k > maxQueryLimit {
		k = maxQueryLimit
	}
	// KNN 分支先于普通 list 的 offset 拒绝执行，此处补同款显式拒绝。
	if parsed.Offset != 0 {
		return nil, p.mapError(status.Error(codes.InvalidArgument, "offset() is not supported on vector_search; use kvc page tokens"))
	}
	// 续页游标：kvc: 前缀族（B2）；ka:/kb: keyset token 与垃圾 token 一律
	// InvalidArgument（keyset token 的续传键是排序键值，与距离序不兼容）。
	var cursor *knnCursor
	if parsed.PageToken != "" {
		c, ok := decodeKNNCursor(parsed.PageToken)
		if !ok {
			return nil, p.mapError(status.Error(codes.InvalidArgument,
				"invalid page token: vector_search pagination requires the kvc: cursor issued by the previous page"))
		}
		cursor = &c
	}

	// iterative scan 是执行期语义而非可选优化——不开则 RLS×KNN 召回错误。
	// SET LOCAL：事务级生效，与 app.roles GUC 同零残留模式（原型 2 验证）。
	// 续页为精确全序扫描（不依赖 HNSW），SET LOCAL 无害且保持单一代价路径。
	if _, err := p.conn(ctx).ExecContext(ctx, `SET LOCAL hnsw.iterative_scan = 'strict_order'`); err != nil {
		return nil, fmt.Errorf("enable iterative scan: %w", err)
	}
	// ef_search 查询级调参（B7）：仅调用方显式设置时注入（缺省不 emit 任何
	// 语句——行为与未提供字段时逐字节一致，pgvector 缺省 40）。取值域已在
	// validateVectorSearch 前置拒绝（[1,500]），此处按 int 字面量拼接安全
	//（SET LOCAL 不接受绑定参数）；同事务内生效、随事务消失零残留。
	if vs.EfSearch != nil {
		if _, err := p.conn(ctx).ExecContext(ctx,
			fmt.Sprintf(`SET LOCAL hnsw.ef_search = '%d'`, *vs.EfSearch)); err != nil {
			return nil, fmt.Errorf("set hnsw.ef_search: %w", err)
		}
	}

	op, err := distanceOp(vs.Metric)
	if err != nil {
		return nil, err
	}
	vecArg := pgVectorFloatLiteral(vs.Values)
	distExpr := fmt.Sprintf(`d.%s %s ?::vector`, quoteIdent(vs.Attribute), op)

	// 投影：全部 vector 列转 JSON 数组（目标列与非目标列一致契约）。
	vectorCols, err := p.vectorColumnsOf(ctx, projectID, databaseID, collectionID, schema, physical)
	if err != nil {
		return nil, err
	}

	var raw []knnRow

	if cursor == nil {
		// 首页：HNSW + iterative scan（会话 #10 原管道）。多取一行（k+1）：
		// 第 k+1 行的距离用来证明第 k 行所在距离组是否"完整"（无越页 tie）
		// ——完整则满页发射 k 行，仅边界 tie 组跨页时短页（见下）。
		// 占位符出现顺序 = SELECT 的距离表达式 → WHERE（_tenant/权限/filter）→
		// ORDER BY 的距离表达式 → LIMIT，参数严格按此序装配（原型期曾按
		// whereArgs→vec→vec→k 装配导致错位：internalID 被绑进 ::vector 报
		// 42846）。
		knnArgs := make([]any, 0, len(args)+3)
		knnArgs = append(knnArgs, vecArg)
		knnArgs = append(knnArgs, args...)
		knnArgs = append(knnArgs, vecArg, k+1)
		querySQL := fmt.Sprintf(
			`SELECT %s AS doc, %s AS __dist FROM %s d WHERE %s ORDER BY %s LIMIT ?`,
			vectorProjection(vectorCols), distExpr, tbl,
			strings.Join(whereParts, " AND "), distExpr)

		raw, err = p.scanKNNRows(ctx, querySQL, knnArgs)
		if err != nil {
			return nil, err
		}
	} else {
		// 续页：精确全序扫描（B2 形态 A）。HNSW 索引只能承载距离单键序，
		// 同距离 tie 组内的跨查询顺序不稳定——仅阈值谓词（无全序）会在 tie
		// 组跨页边界丢行；ORDER BY 距离+_id 全序使续页 = 全局 (dist,_id) 序
		// 的严格切片，不重不漏由结构保证。代价是续页放弃 HNSW（Seq Scan 精确
		// 求值），单页 KNN（绝大多数场景）不受影响。
		// 子查询形态：外层阈值/排序消费投影别名，距离表达式只绑一次向量
		//（首页路径的占位符错位教训，见上）。占位符顺序 = 内层 SELECT 向量 →
		// 内层 WHERE（_tenant/权限/filter）→ 外层阈值（dist,id,dist）→ LIMIT。
		querySQL := fmt.Sprintf(
			`SELECT sub.doc, sub.__dist FROM (SELECT %s AS doc, %s AS __dist, d._id AS __id FROM %s d WHERE %s) sub `+
				`WHERE (sub.__dist = ?::float8 AND sub.__id > ?) OR (sub.__dist > ?::float8) `+
				`ORDER BY sub.__dist ASC, sub.__id ASC LIMIT ?`,
			vectorProjection(vectorCols), distExpr, tbl,
			strings.Join(whereParts, " AND "))
		knnArgs := make([]any, 0, len(args)+6)
		knnArgs = append(knnArgs, vecArg)
		knnArgs = append(knnArgs, args...)
		// 空 id（首页 tie-trim 全裁发放的起点游标）→ `_id > ''` 恒真，
		// 阈值退化为 dist >= cursor.dist。
		knnArgs = append(knnArgs, cursor.dist, cursor.id, cursor.dist, k)

		raw, err = p.scanKNNRows(ctx, querySQL, knnArgs)
		if err != nil {
			return nil, err
		}
	}

	// 发射装配：max_distance 后置过滤（原型 2 实证：距离谓词进 WHERE 会使
	// 规划器放弃 HNSW 索引扫描；top-k 结果上过滤语义等价——top-k ⊇ 全部
	// ≤ 阈值行）。raw 按距离升序，超阈值必为后缀，可提前截断。
	docs := make([]databases.Document, 0, len(raw))
	dists := make([]float64, 0, len(raw))
	next := ""
	if cursor == nil {
		// (dist,_id) 稳定视图：HNSW 对同距离 tie 的输出序任意，切页边界与
		// 游标选取需要确定性视图。此排序只影响 tie 组内的发射顺序——边界
		// 之前的行全量发射、边界 tie 组全量顺延（见下），组内顺序不影响
		// 不重不漏。
		sort.Slice(raw, func(i, j int) bool {
			if raw[i].dist != raw[j].dist {
				return raw[i].dist < raw[j].dist
			}
			return raw[i].doc.ID < raw[j].doc.ID
		})
		// 完整距离组切页（B2）：发射前缀 raw[:L]，L 是 ≤ k 的最大"完整距离
		// 组"边界——每组是否完整（不含越页 tie）由组后首行证明：HNSW 对同距
		// tie 组的取舍任意，发射其中任意真子集都会让 (dist,_id) 阈值游标漏掉
		// 未发射的同距行，故 tie 组只能整组发射或整组顺延。取 k+1 行使常规
		// 情形（距离互异）满页发射 k 行；取尽（len(raw) ≤ k）时所有组自然完整。
		L := 0
		for L < len(raw) {
			e := L
			for e < len(raw) && raw[e].dist == raw[L].dist {
				e++
			}
			if e > k {
				break
			}
			L = e
			if L == k {
				break
			}
		}
		for _, r := range raw[:L] {
			if vs.MaxDistance != nil && r.dist > *vs.MaxDistance {
				break
			}
			docs = append(docs, r.doc)
			dists = append(dists, r.dist)
		}
		// 游标：最后发射行（组内全量发射 → 严格大于阈值即不重不漏）；边界
		// tie 组整组顺延时 L 不动在组前，游标取该组起点（id = ""）→ 下一页
		// 以 dist >= d 组起点精确重取（含组内全部行）。整组顺延且首组即溢出
		//（L == 0）时零发射 + 起点游标；max_distance 滤空发射集（整页超阈）
		// 时后续必然全超阈，直接收尾不发游标。
		if L < len(raw) && L > 0 && len(docs) > 0 {
			next = encodeKNNCursor(dists[len(dists)-1], docs[len(docs)-1].ID)
		} else if L == 0 && len(raw) > 0 && (vs.MaxDistance == nil || raw[0].dist <= *vs.MaxDistance) {
			next = encodeKNNCursor(raw[0].dist, "")
		}
	} else {
		for _, r := range raw {
			if vs.MaxDistance != nil && r.dist > *vs.MaxDistance {
				break
			}
			docs = append(docs, r.doc)
			dists = append(dists, r.dist)
		}
		// 续页为精确全序：满页发严格游标。发射空集时（整页超 max_distance）
		// 后续必然全超阈，直接收尾。
		if len(raw) == k && len(docs) > 0 {
			next = encodeKNNCursor(dists[len(dists)-1], docs[len(docs)-1].ID)
		}
	}

	// select 投影裁剪（与普通 list 同语义）。
	if len(parsed.Selects) > 0 {
		selected := make(map[string]struct{}, len(parsed.Selects))
		for _, s := range parsed.Selects {
			selected[mapQueryField(s)] = struct{}{}
		}
		for i := range docs {
			for kk := range docs[i].Data {
				if _, ok := selected[kk]; !ok {
					delete(docs[i].Data, kk)
				}
			}
		}
	}
	// B4 读屏蔽：deprecated 属性读回剥离（与普通 list 同语义；集合无
	// deprecated 属性时零改动跳过）。
	if coll, err := p.GetCollection(ctx, projectID, databaseID, collectionID); err == nil && coll != nil {
		maskDeprecatedData(coll.Attributes, docs)
	}
	return &databases.DocumentList{
		Documents: docs,
		// KNN 无精确 total（top-k 语义非全集计数）；NextPageToken = kvc: 距离
		// 游标（B2）。
		Distances:     dists,
		NextPageToken: next,
	}, nil
}

// knnRow 是 KNN 查询的一行（文档 + 距离）。
type knnRow struct {
	doc  databases.Document
	dist float64
}

// scanKNNRows 执行 KNN 查询并扫描 (doc, dist) 行（投影列序两处 SQL 一致）。
func (p *postgresDocumentDB) scanKNNRows(ctx context.Context, querySQL string, knnArgs []any) ([]knnRow, error) {
	rows, err := p.conn(ctx).QueryContext(ctx, querySQL, knnArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []knnRow
	for rows.Next() {
		var raw []byte
		var dist float64
		if err := rows.Scan(&raw, &dist); err != nil {
			return nil, err
		}
		doc, err := parseDocumentJSON(raw)
		if err != nil {
			return nil, err
		}
		if doc == nil {
			continue
		}
		out = append(out, knnRow{doc: *doc, dist: dist})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// sortKey 是一个排序键（物理列 + 方向）；keyset 谓词与 ORDER BY 同源消费。
type sortKey struct {
	field string
	dir   string
}

// buildKeysetPredicate 构造多键 keyset 谓词（C2 完成态）。cursor 行的排序键
// 值由服务端查行取得（values 与 sortKeys 一一对应），cursorID 是 _id。
//
// 方向一致（全 ASC 或全 DESC）→ 行比较 `(k1,…,kn,_id) op (?,…,?)`——形式
// 最简且 PG 可用 RowCompare 索引扫描；方向混合 → 逐键 OR 展开
// `k1 OP1 ? OR (k1 = ? AND k2 OP2 ?) OR … OR (k1 = ? AND … AND _id OPn ?)`
// （行比较无法表达逐键方向）。op 选取：after = ORDER BY 方向的"向后"，
// before = 反向（与单键时代语义一致）。
func buildKeysetPredicate(sortKeys []sortKey, values []any, cursorID, cursorKind string) (string, []any, error) {
	if len(sortKeys) == 0 || len(sortKeys) != len(values) {
		return "", nil, status.Error(codes.Internal, "keyset predicate requires matching sort keys and values")
	}
	keyOp := func(dir string) string {
		if dir == "DESC" {
			if cursorKind == "after" {
				return "<"
			}
			return ">"
		}
		if cursorKind == "after" {
			return ">"
		}
		return "<"
	}
	uniform := true
	for _, k := range sortKeys[1:] {
		if k.dir != sortKeys[0].dir {
			uniform = false
			break
		}
	}
	if uniform {
		op := keyOp(sortKeys[0].dir)
		cols := make([]string, 0, len(sortKeys)+1)
		phs := make([]string, 0, len(sortKeys)+1)
		outArgs := make([]any, 0, len(sortKeys)+1)
		for i, k := range sortKeys {
			cols = append(cols, "d."+quoteIdent(k.field))
			phs = append(phs, "?")
			outArgs = append(outArgs, values[i])
		}
		cols = append(cols, "d._id")
		phs = append(phs, "?")
		outArgs = append(outArgs, cursorID)
		return fmt.Sprintf("(%s) %s (%s)", strings.Join(cols, ", "), op, strings.Join(phs, ", ")), outArgs, nil
	}
	// 混合方向：OR 展开。第 i 项谓词前缀是 k1..k{i-1} 的等值链；末项是
	// _id tiebreaker（方向随首键）。占位符顺序与 args 严格对应。
	var terms []string
	var outArgs []any
	for i := range sortKeys {
		var parts []string
		for j := 0; j < i; j++ {
			parts = append(parts, fmt.Sprintf("d.%s = ?", quoteIdent(sortKeys[j].field)))
			outArgs = append(outArgs, values[j])
		}
		parts = append(parts, fmt.Sprintf("d.%s %s ?", quoteIdent(sortKeys[i].field), keyOp(sortKeys[i].dir)))
		outArgs = append(outArgs, values[i])
		terms = append(terms, "("+strings.Join(parts, " AND ")+")")
	}
	var idParts []string
	for j := range sortKeys {
		idParts = append(idParts, fmt.Sprintf("d.%s = ?", quoteIdent(sortKeys[j].field)))
		outArgs = append(outArgs, values[j])
	}
	idParts = append(idParts, fmt.Sprintf("d._id %s ?", keyOp(sortKeys[0].dir)))
	outArgs = append(outArgs, cursorID)
	terms = append(terms, "("+strings.Join(idParts, " AND ")+")")
	return "(" + strings.Join(terms, " OR ") + ")", outArgs, nil
}

// keyset token 是明文 "ka:<docID>" / "kb:<docID>"（与 crud 的结构化 offset
// token 不冲突：那边是版本化编码数据）。简单前缀+docID，无需防篡改——
// token 只承载定位语义，越权由查询 ACL 过滤兜底。多键排序下 token 仍只
// 编码 docID：服务端按 docID 查行取全部排序键值（C2 完成态）。
const (
	keysetAfterPrefix  = "ka:"
	keysetBeforePrefix = "kb:"
)

func encodeKeysetToken(kind, docID string) string {
	if kind == "before" {
		return keysetBeforePrefix + docID
	}
	return keysetAfterPrefix + docID
}

func decodeKeysetToken(token string) (id, kind string, ok bool) {
	if after, isAfter := strings.CutPrefix(token, keysetAfterPrefix); isAfter && after != "" {
		return after, "after", true
	}
	if before, isBefore := strings.CutPrefix(token, keysetBeforePrefix); isBefore && before != "" {
		return before, "before", true
	}
	return "", "", false
}

// knnCursor 是 vector_search 的续页游标（B2 多页 KNN）：天然续传键 =
// (距离, _id) 二元组。与 ka:/kb: keyset token 并列的新前缀族 kvc:。
const knnCursorPrefix = "kvc:"

// knnCursor 是 kvc: token 解码后的续传位置。id 为空 = "该距离起点"形态
// （首页 tie-trim 全裁时发放），谓词退化为 dist >= dist。
type knnCursor struct {
	dist float64
	id   string
}

// encodeKNNCursor 编码 kvc:<dist_hex16>:<docID>。距离用 float8 比特模式的
// 定长 16 位十六进制编码——精确往返、无浮点十进制解析歧义，负距离
// （inner_product 的 <#> 值域 (-inf,0]）原生支持。pgvector 距离算子返回
// float8，扫描值与谓词绑定值同源同型，等值比较无歧义。docID 允许 ':'
// （docIDRe），编码侧无歧义（hex 段定长），解码侧 Cut 取首段。
func encodeKNNCursor(dist float64, docID string) string {
	return knnCursorPrefix + fmt.Sprintf("%016x", math.Float64bits(dist)) + ":" + docID
}

// decodeKNNCursor 解码 kvc: token；非本前缀族 / hex 段非法 / NaN / docID
// 不合法（validateDocID）一律 ok=false，由调用方统一 InvalidArgument。
// token 无需防篡改（与 ka:/kb: 同纪律）：越权由查询 ACL 过滤兜底。
func decodeKNNCursor(token string) (knnCursor, bool) {
	rest, ok := strings.CutPrefix(token, knnCursorPrefix)
	if !ok {
		return knnCursor{}, false
	}
	hexPart, id, _ := strings.Cut(rest, ":")
	if len(hexPart) != 16 {
		return knnCursor{}, false
	}
	bits, err := strconv.ParseUint(hexPart, 16, 64)
	if err != nil {
		return knnCursor{}, false
	}
	dist := math.Float64frombits(bits)
	if math.IsNaN(dist) {
		return knnCursor{}, false
	}
	if id != "" && validateDocID(id) != nil {
		return knnCursor{}, false
	}
	return knnCursor{dist: dist, id: id}, true
}

func (p *postgresDocumentDB) CountDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query, principal databases.Principal) (int64, error) {
	var total int64
	err := p.withDocumentTx(ctx, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		n, err := p.countDocuments(txCtx, projectID, databaseID, collectionID, q, principal)
		if err != nil {
			return err
		}
		total = n
		return nil
	})
	return total, p.mapError(err)
}

func (p *postgresDocumentDB) countDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query, principal databases.Principal) (int64, error) {
	internalID, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return 0, err
	}
	parsed, err := astFrom(q)
	if err != nil {
		return 0, p.mapError(err)
	}
	// keyset-only（C2 收敛）：count 是过滤全集语义，offset() 无意义且原先
	// 仅作深翻页上限校验——显式拒绝（不再静默忽略）。排序/分页算子同理
	//（R9：静默 no-op 违背 §4.1 显式拒绝原则；R9b：分页字段归一进 AST 后
	// 一并拦截）。
	if err := rejectNonFilterOperators(parsed, "count"); err != nil {
		return 0, p.mapError(err)
	}
	tbl := tableName(schema, physical)

	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	var coll *databases.Collection
	if !principal.BypassesDocumentACL() {
		coll, err = p.GetCollection(ctx, projectID, databaseID, collectionID)
		if err != nil {
			return 0, p.mapError(err)
		}
		if coll == nil {
			return 0, p.mapError(status.Error(codes.NotFound, "collection not found"))
		}
	}

	whereParts := []string{"d._tenant = ?"}
	args := []any{internalID}
	// 判定执行点（阶段③包 C）：业务集合 policy 隐式过滤；sentinel 保留谓词。
	if !principal.BypassesDocumentACL() && isSystem {
		permWhere, permArgs, err := p.listPermissionFilter(ctx, coll, principal)
		if err != nil {
			return 0, p.mapError(err)
		}
		if permWhere != "" {
			whereParts = append(whereParts, permWhere)
			args = append(args, permArgs...)
		}
	}
	if coll != nil {
		if err := p.validateQueryFields(ctx, schema, physical, parsed, coll, collectionID, isSystem); err != nil {
			return 0, p.mapError(err)
		}
	}
	filterWhere, filterArgs, _, err := buildAppwriteQuery(parsed, arrayTypesOf(coll))
	if err != nil {
		return 0, p.mapError(err)
	}
	if filterWhere != "" {
		whereParts = append(whereParts, filterWhere)
		args = append(args, filterArgs...)
	}

	var total int64
	sql := fmt.Sprintf(`SELECT COUNT(*) FROM %s d WHERE %s`, tbl, strings.Join(whereParts, " AND "))
	err = p.conn(ctx).QueryRowContext(ctx, sql, args...).Scan(&total)
	return total, p.mapError(err)
}

// AggregateDocuments 在权限过滤后的可见行集上执行 sum/avg/min/max（可选
// 单键 group_by）。D1（§11-J 已裁决）：listPermissionFilter 的过滤链先于
// GROUP BY——不可见行的值不进聚合、group 键不泄露。聚合目标必须为声明的
// 数值属性（integer/float，System 主体一视同仁，防拼入任意列名）；group_by
// 须为已声明属性。空集语义：sum=0（COALESCE）、avg/min/max 无值（Value=nil）。
func (p *postgresDocumentDB) AggregateDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query, aggs []databases.AggregateSpec, groupBy string, principal databases.Principal) ([]databases.AggregateGroup, error) {
	var groups []databases.AggregateGroup
	err := p.withDocumentTx(ctx, p.execIdentity(ctx, projectID, principal), func(txCtx context.Context) error {
		gs, err := p.aggregateDocuments(txCtx, projectID, databaseID, collectionID, q, aggs, groupBy, principal)
		if err != nil {
			return err
		}
		groups = gs
		return nil
	})
	if err != nil {
		return nil, p.mapError(err)
	}
	return groups, nil
}

func (p *postgresDocumentDB) aggregateDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query, aggs []databases.AggregateSpec, groupBy string, principal databases.Principal) ([]databases.AggregateGroup, error) {
	if len(aggs) == 0 {
		return nil, p.mapError(status.Error(codes.InvalidArgument, "aggregations is required"))
	}
	for _, agg := range aggs {
		switch agg.Function {
		case databases.AggregateSum, databases.AggregateAvg, databases.AggregateMin, databases.AggregateMax:
		default:
			return nil, p.mapError(status.Error(codes.InvalidArgument, fmt.Sprintf("invalid aggregate function %q", agg.Function)))
		}
		if !validColumnName(agg.Field) {
			return nil, p.mapError(status.Error(codes.InvalidArgument, "invalid aggregate field name"))
		}
	}
	if groupBy != "" && !validColumnName(groupBy) {
		return nil, p.mapError(status.Error(codes.InvalidArgument, "invalid group_by field name"))
	}

	internalID, schema, physical, err := p.resolvePhysicalTable(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, p.mapError(err)
	}
	tbl := tableName(schema, physical)

	// 白名单校验（与旧 SumDocumentField 同纪律）：聚合目标 ∈ 声明属性且为
	// integer/float；group_by ∈ 声明属性（任意类型，键按 text 序列化）。
	coll, err := p.GetCollection(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, p.mapError(err)
	}
	if coll == nil {
		return nil, p.mapError(status.Error(codes.NotFound, "collection not found"))
	}
	attrs := map[string]string{}
	for _, attr := range coll.Attributes {
		attrs[attr.Key] = strings.ToLower(attr.Type)
	}
	for _, agg := range aggs {
		switch attrs[agg.Field] {
		case "integer", "float":
		default:
			return nil, p.mapError(status.Error(codes.InvalidArgument, fmt.Sprintf("field %s is not a numeric attribute", agg.Field)))
		}
	}
	if groupBy != "" {
		if _, ok := attrs[groupBy]; !ok {
			return nil, p.mapError(status.Error(codes.InvalidArgument, fmt.Sprintf("group_by field %s is not a declared attribute", groupBy)))
		}
	}

	parsed, err := astFrom(q)
	if err != nil {
		return nil, p.mapError(err)
	}
	// 整集语义只消费过滤算子：排序/分页算子显式拒绝（R9 + R9b 分页字段
	// 归一，§4.1 显式拒绝原则）。
	if err := rejectNonFilterOperators(parsed, "aggregate"); err != nil {
		return nil, p.mapError(err)
	}
	// 过滤/排序字段白名单与兄弟路径（List/Count）同源校验（R6）：未声明列
	// 不落 PG 42703、search 需 fulltext 索引、$version 过 readiness 检查。
	isSystem := databases.IsSystemCollection(projectID, databaseID, collectionID)
	if err := p.validateQueryFields(ctx, schema, physical, parsed, coll, collectionID, isSystem); err != nil {
		return nil, p.mapError(err)
	}

	whereParts := []string{"d._tenant = ?"}
	args := []any{internalID}
	// 判定执行点（阶段③包 C）：业务集合 policy 隐式过滤（D1：聚合先于
	// GROUP BY 的可见行集语义由 securityQuals 机制保证）；sentinel 保留谓词。
	if !principal.BypassesDocumentACL() && isSystem {
		permWhere, permArgs, err := p.listPermissionFilter(ctx, coll, principal)
		if err != nil {
			return nil, p.mapError(err)
		}
		if permWhere != "" {
			whereParts = append(whereParts, permWhere)
			args = append(args, permArgs...)
		}
	}
	// 聚合只消费过滤算子；排序/分页已在 rejectNonFilterOperators 显式拒绝。
	filterWhere, filterArgs, _, err := buildAppwriteQuery(parsed, arrayTypesOf(coll))
	if err != nil {
		return nil, p.mapError(err)
	}
	if filterWhere != "" {
		whereParts = append(whereParts, filterWhere)
		args = append(args, filterArgs...)
	}

	selects := make([]string, 0, len(aggs)+1)
	if groupBy != "" {
		selects = append(selects, fmt.Sprintf(`d.%s::text AS __group_key`, quoteIdent(groupBy)))
	}
	// 表达式按属性类型分流（预决策 5）：integer 属性的 sum/min/max 走 int64
	//（SUM(bigint) 在 PG 返回 numeric，显式 ::int8 并在溢出时拒绝；min/max
	// 原生 bigint）；avg 恒 float8（AVG(bigint) 返回 numeric → ::float8）；
	// float 属性恒 float8。
	resultInt64 := make([]bool, len(aggs))
	for i, agg := range aggs {
		fn := strings.ToUpper(string(agg.Function))
		isInteger := attrs[agg.Field] == "integer"
		resultInt64[i] = isInteger && (agg.Function == databases.AggregateSum ||
			agg.Function == databases.AggregateMin || agg.Function == databases.AggregateMax)
		var expr string
		switch {
		case resultInt64[i] && agg.Function == databases.AggregateSum:
			// 空集（全 NULL）定义为 0；溢出由 22003 检测拒绝。
			expr = fmt.Sprintf(`COALESCE(%s(d.%s), 0)::int8`, fn, quoteIdent(agg.Field))
		case resultInt64[i]:
			expr = fmt.Sprintf(`%s(d.%s)`, fn, quoteIdent(agg.Field))
		case agg.Function == databases.AggregateSum:
			expr = fmt.Sprintf(`COALESCE(%s(d.%s), 0)::float8`, fn, quoteIdent(agg.Field))
		default:
			expr = fmt.Sprintf(`%s(d.%s)::float8`, fn, quoteIdent(agg.Field))
		}
		selects = append(selects, expr)
	}
	querySQL := fmt.Sprintf(`SELECT %s FROM %s d WHERE %s`, strings.Join(selects, ", "), tbl, strings.Join(whereParts, " AND "))
	if groupBy != "" {
		querySQL += fmt.Sprintf(` GROUP BY d.%s ORDER BY d.%s`, quoteIdent(groupBy), quoteIdent(groupBy))
	}

	// mapAggregateError 把执行期 PG 错误按聚合语义翻译：::int8 溢出
	//（numeric_value_out_of_range，22003）→ ErrAggregateOverflow。
	mapAggregateError := func(err error) error {
		if err == nil {
			return nil
		}
		var fielder pgErrorFielder
		if errors.As(err, &fielder) && fielder.Field('C') == "22003" {
			return databases.ErrAggregateOverflow
		}
		return err
	}

	rows, err := p.conn(ctx).QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, p.mapError(mapAggregateError(err))
	}
	defer func() { _ = rows.Close() }()

	scanVals := make([]any, 0, len(aggs)+1)
	var groupKey sql.NullString
	if groupBy != "" {
		scanVals = append(scanVals, &groupKey)
	}
	// 混合类型列各自 Null 扫描（int64 列 → NullInt64，double 列 → NullFloat64）。
	int64Vals := make([]sql.NullInt64, len(aggs))
	floatVals := make([]sql.NullFloat64, len(aggs))
	for i := range aggs {
		if resultInt64[i] {
			scanVals = append(scanVals, &int64Vals[i])
		} else {
			scanVals = append(scanVals, &floatVals[i])
		}
	}

	buildValues := func() []databases.AggregateValue {
		vals := make([]databases.AggregateValue, 0, len(aggs))
		for i, agg := range aggs {
			v := databases.AggregateValue{Function: agg.Function, Field: agg.Field}
			switch {
			case resultInt64[i] && int64Vals[i].Valid:
				v.Kind, v.Int64 = databases.AggregateValueInt64, int64Vals[i].Int64
			case resultInt64[i]:
				// 空集 min/max：无值（sum 已 COALESCE 为 0，不会走到这里）。
			case floatVals[i].Valid:
				v.Kind, v.Double = databases.AggregateValueDouble, floatVals[i].Float64
			}
			vals = append(vals, v)
		}
		return vals
	}

	var groups []databases.AggregateGroup
	for rows.Next() {
		if err := rows.Scan(scanVals...); err != nil {
			return nil, p.mapError(mapAggregateError(err))
		}
		g := databases.AggregateGroup{Values: buildValues()}
		if groupBy != "" {
			if groupKey.Valid {
				key := groupKey.String
				g.GroupKey = &key
			}
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, p.mapError(mapAggregateError(err))
	}
	if groups == nil && groupBy == "" {
		// 无 group_by 时空集也返回一组（sum=0 / avg=min=max=nil）。
		g := databases.AggregateGroup{Values: make([]databases.AggregateValue, 0, len(aggs))}
		for i, agg := range aggs {
			v := databases.AggregateValue{Function: agg.Function, Field: agg.Field}
			if agg.Function == databases.AggregateSum {
				// sum 空集 = 0，类型跟随属性（integer → int64 0）。
				if resultInt64[i] {
					v.Kind = databases.AggregateValueInt64
				} else {
					v.Kind = databases.AggregateValueDouble
				}
			}
			g.Values = append(g.Values, v)
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// rejectNonFilterOperators 拒绝对整集语义（count/aggregate）无意义的排序/
// 分页算子（R9 + R9b）。R9b 归一：分页字段（page_size/page_token）在
// astFrom 已并进 AST，此处只查 AST——typed AST 的分页字段不再漏拦。
// vector_search 同拒（会话 #10）：KNN 的 limit 即 k 是 top-k 语义，与
// 整集计数/聚合语义不相容（可见行计数用普通 filter 即可）。
// kind 仅用于错误消息（"count"/"aggregate"）。
func rejectNonFilterOperators(parsed *query.Query, kind string) error {
	if parsed.VectorSearch != nil {
		return status.Error(codes.InvalidArgument,
			fmt.Sprintf("vector_search is not supported on %s; it is a top-k operator for list queries", kind))
	}
	if len(parsed.Orders) > 0 || parsed.PageSize != 0 || parsed.PageToken != "" ||
		parsed.Limit != 0 || parsed.Offset != 0 ||
		parsed.CursorAfter != "" || parsed.CursorBefore != "" {
		return status.Error(codes.InvalidArgument,
			fmt.Sprintf("orders and pagination are not supported on %s; use list queries", kind))
	}
	return nil
}

// validColumnName 限制字段名为安全的小写标识符（防 SQL 注入）。
func validColumnName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		case r == '_' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------
