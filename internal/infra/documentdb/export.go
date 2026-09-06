// 项目级导出（转出 POC 门禁 B5，redesign §4.7/§9.3 教训 4/§10.1）：
// `torchwood admin export` 的执行体——catalog 快照（manifest）+ 每业务集合
// 全行 NDJSON（to_jsonb(d.*) 形态，含 _acl/_version）+ snapshot_seq。
//
// snapshot_seq 与 `:changes` 续接语义（§10.1 一致性窗口闭合）：全部读取
// （outbox max(seq)、catalog 两表、集合行）包在**单一 REPEATABLE READ 快照
// 事务**内——快照后提交的写入不在导出行中、其全局 seq 必大于 snapshot_seq
// （seq 是全局分配序，000002 identity 单调），因此
// `:changes?since_seq=<snapshot_seq>` 恰返回导出之后的变更，无重无漏。
//
// 流式纪律：集合逐页 keyset（_id 游标）读取写盘，不整表载内存；catalog 与
// outbox 聚合为单行查询。物理名随 manifest 记录（内部实现细节，不出现在
// 任何 API 响应；导出物是运维工件而非 API 面）。
package documentdb

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/uptrace/bun"

	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/pkg/ident"
)

// ExportFormatVersion 是导出 manifest 的格式版本：不兼容变更递增，导入器
// 只接受自己认识的版本。
const ExportFormatVersion = 1

// exportPageSize 是单页 keyset 扫描的行数（流式翻页，非整表载入）。
const exportPageSize = 500

// ExportManifest 是导出目录的 manifest.json：项目 catalog 快照 + 数据文件
// 索引 + snapshot_seq。attrs/indexes/permissions 保存 catalog JSONB 原文，
// 导入原样写回（逐字段往返保真）。
type ExportManifest struct {
	FormatVersion int                  `json:"format_version"`
	ProjectID     string               `json:"project_id"`
	SnapshotSeq   int64                `json:"snapshot_seq"`
	ExportedAt    time.Time            `json:"exported_at"`
	Databases     []ExportedDatabase   `json:"databases"`
	Collections   []ExportedCollection `json:"collections"`
}

// ExportedDatabase 是 catalog_databases 一行的快照。
type ExportedDatabase struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ExportedCollection 是 catalog_collections 一行的快照（attrs/indexes/
// permissions 为 catalog JSONB 原文）+ 数据文件索引。
type ExportedCollection struct {
	DatabaseID       string    `json:"database_id"`
	CollectionID     string    `json:"collection_id"`
	Name             string    `json:"name"`
	PhysicalName     string    `json:"physical_name"`
	DocumentSecurity bool      `json:"document_security"`
	Disabled         bool      `json:"disabled"`
	Permissions      string    `json:"permissions"`
	Attrs            string    `json:"attrs"`
	Indexes          string    `json:"indexes"`
	SchemaVersion    int64     `json:"schema_version"`
	DDLSeq           int64     `json:"ddl_seq"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	DataFile         string    `json:"data_file"`
	RowCount         int64     `json:"row_count"`
}

// ExportProject 把项目（catalog + 全部业务集合行）流式导出到 outDir：
//
//	<outDir>/manifest.json            快照与索引（最后写出，见下）
//	<outDir>/data/collection-NNNNNN.ndjson  每集合一个文件，行 = to_jsonb(d.*)
//
// manifest 在全部数据文件成功落盘后写出——半途失败的目录没有 manifest，
// 导入器拒收（可重跑覆盖）。返回写出的 manifest。
func ExportProject(ctx context.Context, db *clients.Database, projectID, outDir string) (*ExportManifest, error) {
	if projectID == "" {
		return nil, fmt.Errorf("export: project_id is required")
	}
	if err := ident.ValidateSchemaResourceID(projectID); err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(outDir, "data"), 0o755); err != nil {
		return nil, fmt.Errorf("export: create out dir: %w", err)
	}

	manifest := &ExportManifest{
		FormatVersion: ExportFormatVersion,
		ProjectID:     projectID,
		ExportedAt:    time.Now().UTC(),
		Databases:     []ExportedDatabase{},
		Collections:   []ExportedCollection{},
	}

	// 单一 REPEATABLE READ 快照：outbox max(seq)（snapshot_seq）、catalog
	// 两表与集合行读自同一快照——一致性窗口语义见包注释。身份 tw_system：
	// catalog/outbox SELECT + BYPASSRLS 全行读（§3.2 #8：平台侧旁路走
	// tw_system，不进 GUC 白名单）。
	tx, err := db.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, fmt.Errorf("export: begin snapshot tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := clients.InjectExecIdentity(ctx, tx,
		clients.ExecIdentity{Role: clients.RoleSystem}); err != nil {
		return nil, fmt.Errorf("export: inject identity: %w", err)
	}

	// snapshot_seq = 快照内 outbox 全局最大 seq（分配序，跨项目共享单调）。
	// 空表取 0：`:changes?since_seq=0` 从最老可用事件起，语义仍成立。
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM document_events_outbox`,
	).Scan(&manifest.SnapshotSeq); err != nil {
		return nil, fmt.Errorf("export: read snapshot_seq: %w", err)
	}

	// catalog_databases 全行（业务库；catalog 不含 sentinel，防御不在此）。
	var dbRows []model.DocumentDatabase
	if err := tx.NewSelect().Model(&dbRows).
		Where("project_id = ?", projectID).
		OrderExpr("database_id ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("export: read catalog_databases: %w", err)
	}
	for i := range dbRows {
		manifest.Databases = append(manifest.Databases, ExportedDatabase{
			ID:        dbRows[i].DatabaseID,
			Name:      dbRows[i].Name,
			CreatedAt: dbRows[i].CreatedAt,
			UpdatedAt: dbRows[i].UpdatedAt,
		})
	}

	// catalog_collections 全行（排除 sentinel 项目数据面——系统集合的物理表
	// 是静态平面，不属于业务文档面，随 projectschema 迁移重建）。
	var collRows []model.DocumentCollection
	if err := tx.NewSelect().Model(&collRows).
		Where("project_id = ? AND database_id <> ?", projectID, ident.ProjectDataPlaneID).
		OrderExpr("database_id ASC").OrderExpr("collection_id ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("export: read catalog_collections: %w", err)
	}
	for i := range collRows {
		schema, err := ident.SchemaName(projectID, collRows[i].DatabaseID)
		if err != nil {
			return nil, fmt.Errorf("export: resolve schema: %w", err)
		}
		dataFile := fmt.Sprintf("collection-%06d.ndjson", len(manifest.Collections)+1)
		rowCount, err := exportCollectionRows(ctx, tx, schema, collRows[i].PhysicalName,
			filepath.Join(outDir, filepath.FromSlash("data"), dataFile))
		if err != nil {
			return nil, err
		}
		manifest.Collections = append(manifest.Collections, ExportedCollection{
			DatabaseID:       collRows[i].DatabaseID,
			CollectionID:     collRows[i].CollectionID,
			Name:             collRows[i].Name,
			PhysicalName:     collRows[i].PhysicalName,
			DocumentSecurity: collRows[i].DocumentSecurity,
			Disabled:         collRows[i].Disabled,
			Permissions:      collRows[i].Permissions,
			Attrs:            collRows[i].Attrs,
			Indexes:          collRows[i].Indexes,
			SchemaVersion:    collRows[i].SchemaVersion,
			DDLSeq:           collRows[i].DDLSeq,
			CreatedAt:        collRows[i].CreatedAt,
			UpdatedAt:        collRows[i].UpdatedAt,
			DataFile:         "data/" + dataFile,
			RowCount:         rowCount,
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("export: commit snapshot tx: %w", err)
	}

	// manifest 最后写出：数据文件全部成功才落 manifest，半成品目录无 manifest
	// 可被导入器识别拒收。
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("export: marshal manifest: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(outDir, "manifest.json"), mb); err != nil {
		return nil, fmt.Errorf("export: write manifest: %w", err)
	}
	return manifest, nil
}

// exportRow 是 to_jsonb(d.*) 单列扫描行。
type exportRow struct {
	Doc json.RawMessage `bun:"doc"`
}

// exportCollectionRows 把一个集合物理表逐页 keyset 导出为 NDJSON
// （行 = to_jsonb(d.*)，含 _acl/_version/_id 等系统列与用户列），返回行数。
func exportCollectionRows(ctx context.Context, tx bun.Tx, schema, physical, path string) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("export: create data file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)

	tbl := tableName(schema, physical) + ` AS d`
	var lastID string
	var total int64
	for {
		var rows []exportRow
		err := tx.NewSelect().ColumnExpr(`to_jsonb(d.*) AS doc`).
			TableExpr(tbl).
			Where(`d._id > ?`, lastID).
			Order(`d._id ASC`).
			Limit(exportPageSize).
			Scan(ctx, &rows)
		if err != nil {
			return 0, fmt.Errorf("export: scan %s: %w", tbl, err)
		}
		if len(rows) == 0 {
			break
		}
		for i := range rows {
			if _, err := w.Write(rows[i].Doc); err != nil {
				return 0, fmt.Errorf("export: write %s: %w", path, err)
			}
			if _, err := w.WriteString("\n"); err != nil {
				return 0, fmt.Errorf("export: write %s: %w", path, err)
			}
			// 行内 _id 稳定存在（NOT NULL 主键列），解析失败即导出物损坏，
			// fail-fast（游标错位比崩溃更糟）。
			var row struct {
				ID string `json:"_id"`
			}
			if err := json.Unmarshal(rows[i].Doc, &row); err != nil {
				return 0, fmt.Errorf("export: decode _id from %s: %w", path, err)
			}
			lastID = row.ID
			total++
		}
		if len(rows) < exportPageSize {
			break
		}
	}
	if err := w.Flush(); err != nil {
		return 0, fmt.Errorf("export: flush %s: %w", path, err)
	}
	return total, nil
}

// writeFileAtomic 先写临时文件再 rename 覆盖，避免 manifest 半行被读到。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
