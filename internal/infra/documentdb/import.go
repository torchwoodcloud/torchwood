// 项目级导入（转出 POC 门禁 B5）：`torchwood admin import` 的执行体——读
// ExportProject 产出的 manifest，重建 catalog（**沿用导出的 physical_name**，
// 数据文件按逻辑集合寻址、不与物理名重映射耦合）与集合物理表，再行级导入。
//
// DDL 复用现役代码路径：集合表经由与 CreateCollection 同一 DDL 汇聚点
//（createCollectionTable + reconcileVersionColumn + createCollectionIndex）——
// _version 列、默认时间索引、_acl GIN、RLS policy + FORCE、列级 GRANT 全部
// 走与在线建集合相同的函数，不另写一套建表 SQL。
//
// 行导入以 tw_system 身份直写 INSERT（表级 ALL 不受列授权限制，_acl 随行
// 保真携带；RLS 由 BYPASSRLS 旁路——导入是平台运维面，不经文档 API 授权），
// _id/_tenant/_acl/_version/时间戳原样保真，分批事务提交。
//
// 幂等语义：对 manifest 中每个集合先清位（DROP TABLE + 删 catalog 行）再
// 重建重灌——重跑导入即恢复到导出快照；库级 catalog 行 ON CONFLICT 跳过。
//
// snapshot_seq 闭合：导入完成后从 manifest 透出 snapshot_seq，调用方按
// `:changes?since_seq=<snapshot_seq>` 续接导出后的增量（`:changes` 已上线，
// 阶段④ §4.5；outbox 表在 public，不受 drop schema 影响）。
package documentdb

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/infra/projectschema"
	"github.com/torchwooddev/torchwood/pkg/ident"
)

// importBatchRows 是行导入的单事务批量（千行级 POC 数据下每事务亚秒，
// 同时封顶单事务体积）。
const importBatchRows = 500

// ImportReport 是 ImportProject 的结果：恢复清单 + snapshot_seq 续接指引。
type ImportReport struct {
	ProjectID           string   `json:"project_id"`
	DatabasesRestored   []string `json:"databases_restored"`
	CollectionsRestored []string `json:"collections_restored"`
	RowsImported        int64    `json:"rows_imported"`
	SnapshotSeq         int64    `json:"snapshot_seq"`
	ResumeHint          string   `json:"resume_hint"`
}

// ImportProject 从 inDir（ExportProject 产物）恢复项目数据。projectID 必须
// 与 manifest 一致（防误导入到其他项目）；目标项目必须已存在（项目行与
// 静态平面属控制面，不在 B5 文档面往返范围内——删项目的重建先经控制面建项）。
func ImportProject(ctx context.Context, db *clients.Database, projectID, inDir string) (*ImportReport, error) {
	if projectID == "" {
		return nil, fmt.Errorf("import: project_id is required")
	}
	manifestPath := filepath.Join(inDir, "manifest.json")
	mb, err := os.ReadFile(manifestPath) // #nosec G304 -- 路径由运维 CLI 参数给定的导入目录拼出
	if err != nil {
		return nil, fmt.Errorf("import: read manifest: %w", err)
	}
	var manifest ExportManifest
	if err := json.Unmarshal(mb, &manifest); err != nil {
		return nil, fmt.Errorf("import: parse manifest: %w", err)
	}
	if manifest.FormatVersion != ExportFormatVersion {
		return nil, fmt.Errorf("import: unsupported format_version %d (expect %d)",
			manifest.FormatVersion, ExportFormatVersion)
	}
	if manifest.ProjectID != projectID {
		return nil, fmt.Errorf("import: manifest project %q does not match --project %q",
			manifest.ProjectID, projectID)
	}

	p := &postgresDocumentDB{db: db}

	// 项目数据面 schema（静态平面）就绪（EnsureCatalog 同职责）；项目不存在
	// 时 projectschema.Apply 会在静态迁移的 FK/projects 行上 fail-fast。
	if err := projectschema.Apply(ctx, db, projectID); err != nil {
		return nil, fmt.Errorf("import: ensure project schema: %w", err)
	}
	internalID, err := p.resolveInternalIDFresh(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("import: resolve project: %w", err)
	}

	report := &ImportReport{
		ProjectID:           projectID,
		DatabasesRestored:   []string{},
		CollectionsRestored: []string{},
		SnapshotSeq:         manifest.SnapshotSeq,
		ResumeHint: fmt.Sprintf(
			"resume incremental changes with :changes?since_seq=%d (snapshot_seq of this export)",
			manifest.SnapshotSeq),
	}

	// ---- 阶段一：catalog 与物理表重建（tw_owner，DDL 面）----
	// 库先行：catalog_collections 对 catalog_databases 有 FK——所有涉及库的
	// catalog 行（manifest 声明 ∪ 集合所属库）必须先于集合行落库。
	restoredDBs := map[string]bool{}
	ensureDB := func(d ExportedDatabase) error {
		if restoredDBs[d.ID] {
			return nil
		}
		err := p.withOwnerTx(ctx, func(txCtx context.Context) error {
			schema, err := ident.SchemaName(projectID, d.ID)
			if err != nil {
				return err
			}
			if err := p.ensureSchema(txCtx, schema); err != nil {
				return err
			}
			return ensureCatalogDatabase(p.conn(txCtx), projectID, d)
		})
		if err != nil {
			return fmt.Errorf("restore database %s: %w", d.ID, err)
		}
		restoredDBs[d.ID] = true
		return nil
	}
	for _, d := range manifest.Databases {
		if err := ensureDB(d); err != nil {
			return nil, err
		}
	}
	for i := range manifest.Collections {
		if !restoredDBs[manifest.Collections[i].DatabaseID] {
			// manifest.Databases 未声明的孤儿集合所属库：补最小 catalog 行。
			if err := ensureDB(ExportedDatabase{
				ID:        manifest.Collections[i].DatabaseID,
				Name:      manifest.Collections[i].DatabaseID,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}); err != nil {
				return nil, err
			}
		}
	}
	for i := range manifest.Collections {
		c := &manifest.Collections[i]
		if err := ident.ValidateSchemaResourceID(c.DatabaseID); err != nil {
			return nil, fmt.Errorf("import: collection %q: %w", c.CollectionID, err)
		}
		schema, err := ident.SchemaName(projectID, c.DatabaseID)
		if err != nil {
			return nil, fmt.Errorf("import: resolve schema: %w", err)
		}
		attrs, err := decodeAttributes(c.Attrs)
		if err != nil {
			return nil, fmt.Errorf("import: decode attrs of %s/%s: %w", c.DatabaseID, c.CollectionID, err)
		}
		idxs, err := decodeIndexes(c.Indexes)
		if err != nil {
			return nil, fmt.Errorf("import: decode indexes of %s/%s: %w", c.DatabaseID, c.CollectionID, err)
		}
		// 物理表名 = collectionID（2026-09-06 勘误，redesign §4.2）：manifest
		// 的 physical_name 字段忽略——旧 base32 导出物导入时按逻辑名重算。
		physical := c.CollectionID
		err = p.withOwnerTx(ctx, func(txCtx context.Context) error {
			if err := p.ensureSchema(txCtx, schema); err != nil {
				return err
			}
			// 清位（幂等恢复）：物理表与 catalog 行一并清除后按 manifest 重建。
			if _, err := p.conn(txCtx).ExecContext(txCtx,
				fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, tableName(schema, physical))); err != nil {
				return err
			}
			if _, err := p.conn(txCtx).NewDelete().Model((*model.DocumentCollection)(nil)).
				Where("project_id = ? AND database_id = ? AND collection_id = ?",
					projectID, c.DatabaseID, c.CollectionID).Exec(txCtx); err != nil {
				return err
			}
			if err := insertCollectionMetadataNamed(p.conn(txCtx), projectID, c); err != nil {
				return err
			}
			// 与 CreateCollection 相同的 DDL 汇聚点：建表（系统列 + attrs 列 +
			// 默认索引 + RLS）→ _version reconcile → 声明索引。业务库集合恒
			// 用户集合（is_system=false，_version/RLS 全套）。
			if err := p.createCollectionTable(txCtx, schema, physical, internalID, attrs, false); err != nil {
				return err
			}
			if err := p.reconcileVersionColumn(txCtx, schema, physical, false); err != nil {
				return err
			}
			for _, idx := range idxs {
				if err := p.createCollectionIndex(txCtx, schema, physical, idx, attrs); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("import: rebuild %s/%s: %w", c.DatabaseID, c.CollectionID, err)
		}
		// 存在性缓存（原 B13c 物理名缓存）：import 清位重建后写穿，防本实例
		// 陈旧键指向已 DROP 的表。
		p.storePhysicalName(projectID, c.DatabaseID, c.CollectionID, physical)
		report.CollectionsRestored = append(report.CollectionsRestored,
			c.DatabaseID+"/"+c.CollectionID)
	}

	// manifest 声明的其余空库（无集合）也补 catalog 行——已在集合循环前置
	// 补齐，此处兜底遍历保持幂等。
	for _, d := range manifest.Databases {
		if err := ensureDB(d); err != nil {
			return nil, err
		}
	}
	for id := range restoredDBs {
		report.DatabasesRestored = append(report.DatabasesRestored, id)
	}
	sortStrings(report.DatabasesRestored)

	// ---- 阶段二：行导入（tw_system，分批事务）----
	for i := range manifest.Collections {
		c := &manifest.Collections[i]
		schema, err := ident.SchemaName(projectID, c.DatabaseID)
		if err != nil {
			return nil, fmt.Errorf("import: resolve schema: %w", err)
		}
		attrs, err := decodeAttributes(c.Attrs)
		if err != nil {
			return nil, fmt.Errorf("import: decode attrs of %s/%s: %w", c.DatabaseID, c.CollectionID, err)
		}
		if c.DataFile == "" {
			return nil, fmt.Errorf("import: %s/%s: manifest has no data_file", c.DatabaseID, c.CollectionID)
		}
		rows, err := importCollectionRows(ctx, db, schema, c.PhysicalName,
			filepath.Join(inDir, filepath.FromSlash(c.DataFile)), attrs)
		if err != nil {
			return nil, fmt.Errorf("import: rows of %s/%s: %w", c.DatabaseID, c.CollectionID, err)
		}
		if rows != c.RowCount {
			return nil, fmt.Errorf("import: %s/%s row count mismatch: data file has %d, manifest says %d",
				c.DatabaseID, c.CollectionID, rows, c.RowCount)
		}
		report.RowsImported += rows
	}
	return report, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// insertCollectionMetadataNamed 按其余 catalog 字段重建 catalog_collections
// 行（B5 import 专用）：DDLSeq/SchemaVersion/时间戳原样保真，后续
// CreateAttribute/CreateIndex 的 ddl_seq CAS 在恢复值上继续成立。物理名恒 =
// collectionID（2026-09-06 勘误），manifest 的 physical_name 字段忽略。
func insertCollectionMetadataNamed(idb bun.IDB, projectID string, c *ExportedCollection) error {
	if c.CollectionID == "" {
		return status.Error(codes.Internal, "manifest collection has empty collection_id")
	}
	perms, attrs, idxs := c.Permissions, c.Attrs, c.Indexes
	if perms == "" {
		perms = "[]"
	}
	if attrs == "" {
		attrs = "[]"
	}
	if idxs == "" {
		idxs = "[]"
	}
	schemaVersion, ddlSeq := c.SchemaVersion, c.DDLSeq
	if schemaVersion == 0 {
		schemaVersion = 1
	}
	if ddlSeq == 0 {
		ddlSeq = 1
	}
	m := &model.DocumentCollection{
		ProjectID:        projectID,
		DatabaseID:       c.DatabaseID,
		CollectionID:     c.CollectionID,
		Name:             c.Name,
		PhysicalName:     c.CollectionID,
		DocumentSecurity: c.DocumentSecurity,
		Disabled:         c.Disabled,
		IsSystem:         false,
		Permissions:      perms,
		Attrs:            attrs,
		Indexes:          idxs,
		SchemaVersion:    schemaVersion,
		DDLSeq:           ddlSeq,
		CreatedAt:        c.CreatedAt,
		UpdatedAt:        c.UpdatedAt,
	}
	_, err := idb.NewInsert().Model(m).Exec(context.Background())
	return err
}

// ensureCatalogDatabase 幂等补插 catalog_databases 行（沿用导出时间戳；
// 已存在的行不覆盖——恢复不重写目标库的名称改动）。
func ensureCatalogDatabase(idb bun.IDB, projectID string, d ExportedDatabase) error {
	m := &model.DocumentDatabase{
		ProjectID:  projectID,
		DatabaseID: d.ID,
		Name:       d.Name,
		CreatedAt:  d.CreatedAt,
		UpdatedAt:  d.UpdatedAt,
	}
	_, err := idb.NewInsert().Model(m).
		On("CONFLICT (project_id, database_id) DO NOTHING").Exec(context.Background())
	return err
}

// importRow 是一行 NDJSON 的原始键值（RawMessage 保真：datetime 字符串与
// json 列原文不经过 any 反序列化往返）。
type importRow map[string]json.RawMessage

// importColumn 是 INSERT 目标列的类型描述。
type importColumn struct {
	name string
	// attr 非空 = 用户属性列（按 catalog 类型编码）；空 = 系统列。
	attr *databases.Attribute
	// arrayType 是系统数组的元素类型（现仅 _acl 的 string）。
	arrayType string
	// required 标记 NOT NULL 系统列（缺键报错而非静默 NULL）。
	required bool
}

// cast 返回绑定参数的显式 PG cast（空串 = 无 cast，按参数类型隐式解析）。
func (c importColumn) cast() string {
	if c.attr != nil {
		if c.attr.Array {
			return pgArrayTypeFor(c.attr.Type)
		}
		switch strings.ToLower(c.attr.Type) {
		case "json":
			return "jsonb"
		case "vector":
			return "vector"
		}
		return ""
	}
	if c.arrayType != "" {
		return pgArrayTypeFor(c.arrayType)
	}
	return ""
}

// importCollectionRows 把 NDJSON 数据文件按批灌入物理表，返回导入行数。
// INSERT 列 = 系统列（_id/_tenant/时间戳/_created_by/_updated_by/_acl/_version）
// + attrs 列；值按 catalog 类型编码（含 vector/text[]/jsonb 的显式 cast）。
func importCollectionRows(ctx context.Context, db *clients.Database, schema, physical, path string, attrs []databases.Attribute) (int64, error) {
	f, err := os.Open(path) // #nosec G304 -- 路径来自 manifest 数据文件索引（导入目录内）
	if err != nil {
		return 0, fmt.Errorf("open data file: %w", err)
	}
	defer func() { _ = f.Close() }()

	cols := []importColumn{
		{name: "_id", required: true},
		{name: "_tenant", required: true},
		{name: "_created_at", required: true},
		{name: "_updated_at", required: true},
		{name: "_created_by"},
		{name: "_updated_by"},
		{name: "_acl", required: true, arrayType: "string"},
		{name: "_version", required: true},
	}
	for _, a := range attrs {
		cols = append(cols, importColumn{name: a.Key, attr: &a})
	}
	var colNames, placeholders []string
	for _, c := range cols {
		colNames = append(colNames, quoteIdent(c.name))
		if cast := c.cast(); cast != "" {
			placeholders = append(placeholders, "?::"+cast)
		} else {
			placeholders = append(placeholders, "?")
		}
	}
	insertSQL := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`,
		tableName(schema, physical), strings.Join(colNames, ", "), strings.Join(placeholders, ", "))

	// tw_system 身份的分批事务：表级 ALL 不受列授权限制，_acl 随 INSERT 携带。
	execBatch := func(batch [][]any) error {
		if len(batch) == 0 {
			return nil
		}
		return db.RunInTx(clients.WithExecIdentity(ctx, clients.ExecIdentity{Role: clients.RoleSystem}),
			func(txCtx context.Context) error {
				for _, args := range batch {
					if _, err := db.Conn(txCtx).ExecContext(txCtx, insertSQL, args...); err != nil {
						return err
					}
				}
				return nil
			})
	}

	var total int64
	var batch [][]any
	scanner := bufio.NewScanner(f)
	// 单行上限：文档载荷上限 1MiB（对齐 H1）+ JSONB 转义余量。
	scanner.Buffer(make([]byte, 0, 64*1024), 3<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row importRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return 0, fmt.Errorf("parse ndjson line: %w", err)
		}
		args, err := encodeImportRow(cols, row)
		if err != nil {
			return 0, err
		}
		batch = append(batch, args)
		total++
		if len(batch) >= importBatchRows {
			if err := execBatch(batch); err != nil {
				return 0, fmt.Errorf("insert row (_id=%s): %w", rowDebugID(row), err)
			}
			batch = batch[:0]
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan ndjson: %w", err)
	}
	if err := execBatch(batch); err != nil {
		return 0, fmt.Errorf("insert trailing batch: %w", err)
	}
	return total, nil
}

func rowDebugID(row importRow) string {
	var id string
	_ = json.Unmarshal(row["_id"], &id)
	return id
}

// encodeImportRow 按列清单把 NDJSON 行编码为绑定参数（与 insertSQL 列序一致）。
func encodeImportRow(cols []importColumn, row importRow) ([]any, error) {
	args := make([]any, 0, len(cols))
	for _, c := range cols {
		raw, ok := row[c.name]
		if !ok || string(raw) == "null" {
			if c.required {
				return nil, fmt.Errorf("column %s missing in export row", c.name)
			}
			args = append(args, nil)
			continue
		}
		v, err := decodeImportColumn(c, raw)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", c.name, err)
		}
		args = append(args, v)
	}
	return args, nil
}

func decodeImportColumn(c importColumn, raw json.RawMessage) (any, error) {
	if c.attr != nil {
		return decodeAttrValue(*c.attr, raw)
	}
	switch c.name {
	case "_id", "_created_by", "_updated_by":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return s, nil
	case "_tenant", "_version":
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expect integer: %w", err)
		}
		return n, nil
	case "_created_at", "_updated_at":
		return parseExportTime(raw)
	case "_acl":
		return decodeStringArray(raw)
	}
	return nil, fmt.Errorf("unknown system column")
}

// decodeAttrValue 按 catalog 属性类型把 JSON 值转为 PG 绑定参数。
func decodeAttrValue(a databases.Attribute, raw json.RawMessage) (any, error) {
	if a.Array {
		return decodeArrayValue(a.Type, raw)
	}
	switch strings.ToLower(a.Type) {
	case "string", "email", "url":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return s, nil
	case "integer":
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expect integer: %w", err)
		}
		return n, nil
	case "float":
		f, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return nil, fmt.Errorf("expect float: %w", err)
		}
		return f, nil
	case "boolean":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		return b, nil
	case "datetime":
		return parseExportTime(raw)
	case "json":
		// 原文绑定（cast jsonb）：避免 any 往返改变键序/数字形态。
		return string(raw), nil
	case "vector":
		// to_jsonb(vector) 输出字符串（原型 3 实证）——导出行为 "[1,2,3]"
		// 形态的 JSON 字符串；数组形态兼容手工构造的导入物。
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if !strings.HasPrefix(s, "[") {
				return nil, fmt.Errorf("expect vector text like \"[1,2,3]\", got %q", s)
			}
			return s, nil
		}
		var dims []float64
		if err := json.Unmarshal(raw, &dims); err != nil {
			return nil, fmt.Errorf("expect vector array or text: %w", err)
		}
		parts := make([]string, len(dims))
		for i, v := range dims {
			parts[i] = strconv.FormatFloat(v, 'g', -1, 64)
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	default:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return s, nil
	}
}

// decodeArrayValue 把数组属性 JSON 值编码为 PG 数组字面量（元素按属性类型
// 解析，避免 JSON number → float64 的整数精度损耗）。
func decodeArrayValue(t string, raw json.RawMessage) (any, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("expect array: %w", err)
	}
	parts := make([]string, 0, len(items))
	switch strings.ToLower(t) {
	case "integer":
		for _, it := range items {
			n, err := strconv.ParseInt(string(it), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("expect integer element: %w", err)
			}
			parts = append(parts, strconv.FormatInt(n, 10))
		}
	case "float":
		for _, it := range items {
			f, err := strconv.ParseFloat(string(it), 64)
			if err != nil {
				return nil, fmt.Errorf("expect float element: %w", err)
			}
			parts = append(parts, strconv.FormatFloat(f, 'g', -1, 64))
		}
	case "boolean":
		for _, it := range items {
			parts = append(parts, string(it))
		}
	default: // string / datetime / email / url：元素按字符串字面量渲染，PG 按元素类型转换
		for _, it := range items {
			var s string
			if err := json.Unmarshal(it, &s); err != nil {
				return nil, fmt.Errorf("expect string element: %w", err)
			}
			parts = append(parts, `"`+strings.ReplaceAll(s, `"`, `""`)+`"`)
		}
	}
	return `{` + strings.Join(parts, ",") + `}`, nil
}

// decodeStringArray 解析 _acl 的 JSON 字符串数组为 PG text[] 字面量。
func decodeStringArray(raw json.RawMessage) (string, error) {
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", fmt.Errorf("expect string array: %w", err)
	}
	return pgTextArray(items), nil
}

// parseExportTime 解析 to_jsonb 的 timestamptz 文本形态（RFC3339 带微秒）。
func parseExportTime(raw json.RawMessage) (time.Time, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("expect RFC3339 datetime: %w", err)
	}
	return t, nil
}
