package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

// runtimeVarVersionRetention 是版本链保留窗口（D11：保留最近 50 版/集合，
// 含回滚产生的版本行）。
const runtimeVarVersionRetention = 50

// runtimeVarRepo 是 RuntimeVars 四表的仓储实现（迁移 000011，docs/design/
// runtime-vars.md §2.2/§2.5），server 面全量 port 与 client 面窄 port 双实现。
// 写方法经 r.db.Conn(ctx) 感知调用方事务（apikey 模式）。
type runtimeVarRepo struct {
	db *clients.Database
}

var (
	_ projects.RuntimeVarRepository = (*runtimeVarRepo)(nil)
	_ projects.RuntimeVarPublicRead = (*runtimeVarRepo)(nil)
)

// NewRuntimeVarRepository 提供 server 面（Console / Server API）全量 port。
func NewRuntimeVarRepository(db *clients.Database) projects.RuntimeVarRepository {
	return &runtimeVarRepo{db: db}
}

// NewRuntimeVarPublicRead 复用同一 runtimeVarRepo 结构体提供 client 面窄
// port（NewProjectSettingsWriter 同模式）：client app 用例只注入可见性过滤
// 后的读面，全量 port 不进 client 装配——wire 误接线为编译错误。
func NewRuntimeVarPublicRead(db *clients.Database) projects.RuntimeVarPublicRead {
	return &runtimeVarRepo{db: db}
}

// ---------------------------------------------------------------------------
// 集合（runtime_var_sets）
// ---------------------------------------------------------------------------

// CreateVarSet 建集合行 + heads 行（revision=0）同一事务（heads 行与集合
// 同生共死不变量的写入口）；感知调用方事务时并入。
func (r *runtimeVarRepo) CreateVarSet(ctx context.Context, vs *projects.VarSet) error {
	return r.db.RunInTx(ctx, func(ctx context.Context) error {
		if _, err := r.db.Conn(ctx).NewInsert().Model(mapVarSetToModel(vs)).Exec(ctx); err != nil {
			return err
		}
		head := &model.RuntimeVarHead{
			ProjectID: vs.ProjectID,
			VarSetID:  vs.VarSetID,
			UpdatedAt: vs.CreatedAt,
		}
		_, err := r.db.Conn(ctx).NewInsert().Model(head).Exec(ctx)
		return err
	})
}

func (r *runtimeVarRepo) GetVarSet(ctx context.Context, projectID, varSetID string) (*projects.VarSet, error) {
	m := new(model.RuntimeVarSet)
	err := r.db.Conn(ctx).NewSelect().Model(m).
		Where("project_id = ? AND var_set_id = ?", projectID, varSetID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapVarSetToDomain(m), nil
}

func (r *runtimeVarRepo) ListVarSets(ctx context.Context, projectID string) ([]projects.VarSet, error) {
	var ms []model.RuntimeVarSet
	err := r.db.Conn(ctx).NewSelect().Model(&ms).
		Where("project_id = ?", projectID).
		Order("created_at DESC").
		Order("var_set_id ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]projects.VarSet, len(ms))
	for i := range ms {
		out[i] = *mapVarSetToDomain(&ms[i])
	}
	return out, nil
}

// runtimeVarSetUpdateCols 是 UpdateVarSet 的列白名单：集合元数据仅此三列
// 可改（D10：元数据变更不 bump revision、不入版本链）；var_set_id/epoch/
// created_at 不可经此通道修改。
var runtimeVarSetUpdateCols = map[string]struct{}{
	"visibility":  {},
	"description": {},
	"updated_at":  {},
}

func (r *runtimeVarRepo) UpdateVarSet(ctx context.Context, projectID, varSetID string, cols map[string]any) error {
	if len(cols) == 0 {
		return nil
	}
	for col := range cols {
		if _, ok := runtimeVarSetUpdateCols[col]; !ok {
			return fmt.Errorf("runtime var set update: column %q not updatable", col)
		}
	}
	q := r.db.Conn(ctx).NewUpdate().Model((*model.RuntimeVarSet)(nil)).
		Where("project_id = ? AND var_set_id = ?", projectID, varSetID)
	for _, col := range colsToColumns(cols) {
		q = q.Set(col+" = ?", cols[col])
	}
	_, err := q.Exec(ctx)
	return err
}

// DeleteVarSet 删除集合行；vars/heads/versions 经 FK CASCADE 连带销毁。
func (r *runtimeVarRepo) DeleteVarSet(ctx context.Context, projectID, varSetID string) error {
	_, err := r.db.Conn(ctx).NewDelete().Model((*model.RuntimeVarSet)(nil)).
		Where("project_id = ? AND var_set_id = ?", projectID, varSetID).
		Exec(ctx)
	return err
}

// ---------------------------------------------------------------------------
// 版本头（runtime_var_heads）
// ---------------------------------------------------------------------------

// GetHead 无锁读集合当前 revision；0 行 = 集合不存在（heads 行与集合同生共死）。
func (r *runtimeVarRepo) GetHead(ctx context.Context, projectID, varSetID string) (int64, bool, error) {
	var revision int64
	err := r.db.Conn(ctx).NewSelect().Model((*model.RuntimeVarHead)(nil)).
		Column("revision").
		Where("project_id = ? AND var_set_id = ?", projectID, varSetID).
		Scan(ctx, &revision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return revision, true, nil
}

// LockHead 以 SELECT ... FOR UPDATE 锁 heads 行（§2.5：写事务首步，串行化同
// 集合并发写）；必须在事务内调用，锁持有至提交/回滚。0 行 = 集合不存在。
func (r *runtimeVarRepo) LockHead(ctx context.Context, projectID, varSetID string) (int64, bool, error) {
	var revision int64
	err := r.db.Conn(ctx).QueryRowContext(ctx, `
		SELECT revision FROM runtime_var_heads
		WHERE project_id = ? AND var_set_id = ?
		FOR UPDATE
	`, projectID, varSetID).Scan(&revision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return revision, true, nil
}

// BumpRevisionAndPrune 将 heads.revision 原子 +1（updated_at 同步 NOW()）并
// 淘汰保留窗口外最老版本（保留最近 50 版，D11）。必须与同一写事务内的
// LockHead 配对调用（heads 行已持锁，UPDATE 不会缺行；缺行 = 集合被并发
// 删除，显式报错防误当成功）。规范调用序见 port 注释：InsertVersion 先于
// 本方法（淘汰窗口须计入本次新版本行，同事务内语句序对外不可见）。
func (r *runtimeVarRepo) BumpRevisionAndPrune(ctx context.Context, projectID, varSetID string) (int64, error) {
	var newRevision int64
	err := r.db.Conn(ctx).NewUpdate().
		Model((*model.RuntimeVarHead)(nil)).
		Set("revision = revision + 1").
		Set("updated_at = NOW()").
		Where("project_id = ? AND var_set_id = ?", projectID, varSetID).
		Returning("revision").
		Scan(ctx, &newRevision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("runtime var head (%s, %s) not found: bump requires LockHead in the same transaction", projectID, varSetID)
		}
		return 0, err
	}
	// 淘汰窗口外最老版本：revision 不在最近 50 版集合内的版本行删除。
	_, err = r.db.Conn(ctx).NewDelete().
		Model((*model.RuntimeVarVersion)(nil)).
		Where("project_id = ? AND var_set_id = ?", projectID, varSetID).
		Where("revision NOT IN (?)", r.db.Conn(ctx).NewSelect().
			Model((*model.RuntimeVarVersion)(nil)).
			Column("revision").
			Where("project_id = ? AND var_set_id = ?", projectID, varSetID).
			Order("revision DESC").
			Limit(runtimeVarVersionRetention)).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return newRevision, nil
}

// ---------------------------------------------------------------------------
// 变量（runtime_vars）
// ---------------------------------------------------------------------------

func (r *runtimeVarRepo) CreateRuntimeVar(ctx context.Context, v *projects.RuntimeVar) error {
	_, err := r.db.Conn(ctx).NewInsert().Model(mapRuntimeVarToModel(v)).Exec(ctx)
	return err
}

func (r *runtimeVarRepo) GetRuntimeVar(ctx context.Context, projectID, varSetID, key string) (*projects.RuntimeVar, error) {
	m := new(model.RuntimeVar)
	err := r.db.Conn(ctx).NewSelect().Model(m).
		Where("project_id = ? AND var_set_id = ? AND key = ?", projectID, varSetID, key).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapRuntimeVarToDomain(m), nil
}

func (r *runtimeVarRepo) ListRuntimeVars(ctx context.Context, projectID, varSetID string) ([]projects.RuntimeVar, error) {
	var ms []model.RuntimeVar
	err := r.db.Conn(ctx).NewSelect().Model(&ms).
		Where("project_id = ? AND var_set_id = ?", projectID, varSetID).
		Order("key ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]projects.RuntimeVar, len(ms))
	for i := range ms {
		out[i] = *mapRuntimeVarToDomain(&ms[i])
	}
	return out, nil
}

// runtimeVarUpdateCols 是 UpdateRuntimeVar 的列白名单（value/value_type/
// description/updated_at）：value 是完整新值（JSON 文本），类型可随本次变更
// （与 value_type 同步写）；key/created_at 不可经此通道修改。
var runtimeVarUpdateCols = map[string]struct{}{
	"value":       {},
	"value_type":  {},
	"description": {},
	"updated_at":  {},
}

func (r *runtimeVarRepo) UpdateRuntimeVar(ctx context.Context, projectID, varSetID, key string, cols map[string]any) error {
	if len(cols) == 0 {
		return nil
	}
	for col := range cols {
		if _, ok := runtimeVarUpdateCols[col]; !ok {
			return fmt.Errorf("runtime var update: column %q not updatable", col)
		}
	}
	q := r.db.Conn(ctx).NewUpdate().Model((*model.RuntimeVar)(nil)).
		Where("project_id = ? AND var_set_id = ? AND key = ?", projectID, varSetID, key)
	for _, col := range colsToColumns(cols) {
		q = q.Set(col+" = ?", cols[col])
	}
	_, err := q.Exec(ctx)
	return err
}

func (r *runtimeVarRepo) DeleteRuntimeVar(ctx context.Context, projectID, varSetID, key string) error {
	_, err := r.db.Conn(ctx).NewDelete().Model((*model.RuntimeVar)(nil)).
		Where("project_id = ? AND var_set_id = ? AND key = ?", projectID, varSetID, key).
		Exec(ctx)
	return err
}

// Footprint 度量集合当前尺寸（D13 限额口径）：var 行数 + value 列 JSON 文本
// 字节数总和（octet_length(value::text)，JSONB 归一化后的实际存储文本）。
func (r *runtimeVarRepo) Footprint(ctx context.Context, projectID, varSetID string) (int, int64, error) {
	var count, totalBytes int64
	err := r.db.Conn(ctx).QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(octet_length(value::text)), 0)
		FROM runtime_vars
		WHERE project_id = ? AND var_set_id = ?
	`, projectID, varSetID).Scan(&count, &totalBytes)
	if err != nil {
		return 0, 0, err
	}
	return int(count), totalBytes, nil
}

// ReplaceVars 回滚整替（§2.5）：删全部 var 行后按快照重插。key/value_type/
// value/description/created_at 取输入（快照）值；updated_at 由实现统一置为
// 回滚时刻（同一整替全部行取同一时刻——回滚是一次新的写）。整替原子：
// 感知调用方事务时并入，否则自带事务。
func (r *runtimeVarRepo) ReplaceVars(ctx context.Context, projectID, varSetID string, vars []projects.RuntimeVar) error {
	return r.db.RunInTx(ctx, func(ctx context.Context) error {
		if _, err := r.db.Conn(ctx).NewDelete().Model((*model.RuntimeVar)(nil)).
			Where("project_id = ? AND var_set_id = ?", projectID, varSetID).
			Exec(ctx); err != nil {
			return err
		}
		if len(vars) == 0 {
			return nil
		}
		ms := make([]model.RuntimeVar, 0, len(vars))
		replacedAt := time.Now().UTC().Truncate(time.Microsecond)
		for i := range vars {
			ms = append(ms, model.RuntimeVar{
				ProjectID:   projectID,
				VarSetID:    varSetID,
				Key:         vars[i].Key,
				ValueType:   vars[i].ValueType,
				Value:       vars[i].Value,
				Description: vars[i].Description,
				CreatedAt:   vars[i].CreatedAt,
				UpdatedAt:   replacedAt,
			})
		}
		_, err := r.db.Conn(ctx).NewInsert().Model(&ms).Exec(ctx)
		return err
	})
}

// ---------------------------------------------------------------------------
// 版本链（runtime_var_versions）
// ---------------------------------------------------------------------------

func (r *runtimeVarRepo) InsertVersion(ctx context.Context, v *projects.RuntimeVarVersion) error {
	_, err := r.db.Conn(ctx).NewInsert().Model(mapVersionToModel(v)).Exec(ctx)
	return err
}

// GetVersion 单查版本（含快照全文）；不存在（含已被窗口淘汰）返回 (nil, nil)，
// 由 app 层统一映射 NotFound "target revision pruned"。
func (r *runtimeVarRepo) GetVersion(ctx context.Context, projectID, varSetID string, revision int64) (*projects.RuntimeVarVersion, error) {
	m := new(model.RuntimeVarVersion)
	err := r.db.Conn(ctx).NewSelect().Model(m).
		Where("project_id = ? AND var_set_id = ? AND revision = ?", projectID, varSetID, revision).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapVersionToDomain(m), nil
}

// ListVersions 按 revision DESC（最新在前）列出版本行（含 vars 全文；app 层
// 对 ListRuntimeVarVersions 按需裁剪为仅元数据）。
func (r *runtimeVarRepo) ListVersions(ctx context.Context, projectID, varSetID string) ([]projects.RuntimeVarVersion, error) {
	var ms []model.RuntimeVarVersion
	err := r.db.Conn(ctx).NewSelect().Model(&ms).
		Where("project_id = ? AND var_set_id = ?", projectID, varSetID).
		Order("revision DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]projects.RuntimeVarVersion, len(ms))
	for i := range ms {
		out[i] = *mapVersionToDomain(&ms[i])
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// client 面可见性过滤读（RuntimeVarPublicRead）
// ---------------------------------------------------------------------------

// GetVisibleVars 单条 JOIN（sets LEFT JOIN heads LEFT JOIN vars）完成可见性
// 过滤 + revision/vars 一致读取（§2.4 对抗审查修正：消除两独立 SELECT 间写
// 入提交的 etag/内容瞬时错配；方向本身安全，此为免费加固）。
// private 且 !principalAllowed 的 WHERE 不成立与集合不存在同返回 0 行——
// 对匿名探测者不可区分，不确认私有集合存在。
func (r *runtimeVarRepo) GetVisibleVars(ctx context.Context, projectID, varSetID string, principalAllowed bool) ([]projects.RuntimeVar, string, int64, error) {
	rows, err := r.db.Conn(ctx).QueryContext(ctx, `
		SELECT s.epoch,
		       COALESCE(h.revision, 0) AS revision,
		       v.key,
		       v.value_type,
		       v.value::text AS value,
		       v.description,
		       v.created_at,
		       v.updated_at
		FROM runtime_var_sets s
		LEFT JOIN runtime_var_heads h
		       ON h.project_id = s.project_id AND h.var_set_id = s.var_set_id
		LEFT JOIN runtime_vars v
		       ON v.project_id = s.project_id AND v.var_set_id = s.var_set_id
		WHERE s.project_id = ? AND s.var_set_id = ?
		  AND (s.visibility = 'public' OR ?)
		ORDER BY v.key ASC
	`, projectID, varSetID, principalAllowed)
	if err != nil {
		return nil, "", 0, err
	}
	defer func() { _ = rows.Close() }()

	var (
		vars     = []projects.RuntimeVar{}
		epoch    string
		revision int64
		sawRow   bool
	)
	for rows.Next() {
		var (
			key         sql.NullString
			valueType   sql.NullString
			value       sql.NullString
			description sql.NullString
			createdAt   sql.NullTime
			updatedAt   sql.NullTime
		)
		if err := rows.Scan(&epoch, &revision, &key, &valueType, &value, &description, &createdAt, &updatedAt); err != nil {
			return nil, "", 0, err
		}
		sawRow = true
		if !key.Valid {
			// 可见但空集：LEFT JOIN 占位行，epoch/revision 已取到。
			continue
		}
		vars = append(vars, projects.RuntimeVar{
			ProjectID:   projectID,
			VarSetID:    varSetID,
			Key:         key.String,
			ValueType:   valueType.String,
			Value:       value.String,
			Description: description.String,
			CreatedAt:   createdAt.Time,
			UpdatedAt:   updatedAt.Time,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, "", 0, err
	}
	if !sawRow {
		// 集合不存在 / private 且未授权：三种情况合并返回零值，不可区分。
		return nil, "", 0, nil
	}
	return vars, epoch, revision, nil
}

// ---------------------------------------------------------------------------
// 映射
// ---------------------------------------------------------------------------

func mapVarSetToModel(vs *projects.VarSet) *model.RuntimeVarSet {
	return &model.RuntimeVarSet{
		ProjectID:   vs.ProjectID,
		VarSetID:    vs.VarSetID,
		Visibility:  vs.Visibility,
		Epoch:       vs.Epoch,
		Description: vs.Description,
		CreatedAt:   vs.CreatedAt,
		UpdatedAt:   vs.UpdatedAt,
	}
}

func mapVarSetToDomain(m *model.RuntimeVarSet) *projects.VarSet {
	return &projects.VarSet{
		ProjectID:   m.ProjectID,
		VarSetID:    m.VarSetID,
		Visibility:  m.Visibility,
		Epoch:       m.Epoch,
		Description: m.Description,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}

func mapRuntimeVarToModel(v *projects.RuntimeVar) *model.RuntimeVar {
	return &model.RuntimeVar{
		ProjectID:   v.ProjectID,
		VarSetID:    v.VarSetID,
		Key:         v.Key,
		ValueType:   v.ValueType,
		Value:       v.Value,
		Description: v.Description,
		CreatedAt:   v.CreatedAt,
		UpdatedAt:   v.UpdatedAt,
	}
}

func mapRuntimeVarToDomain(m *model.RuntimeVar) *projects.RuntimeVar {
	return &projects.RuntimeVar{
		ProjectID:   m.ProjectID,
		VarSetID:    m.VarSetID,
		Key:         m.Key,
		ValueType:   m.ValueType,
		Value:       m.Value,
		Description: m.Description,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}

func mapVersionToModel(v *projects.RuntimeVarVersion) *model.RuntimeVarVersion {
	return &model.RuntimeVarVersion{
		ProjectID: v.ProjectID,
		VarSetID:  v.VarSetID,
		Revision:  v.Revision,
		Vars:      v.Vars,
		Action:    v.Action,
		Summary:   v.Summary,
		Actor:     v.Actor,
		CreatedAt: v.CreatedAt,
	}
}

func mapVersionToDomain(m *model.RuntimeVarVersion) *projects.RuntimeVarVersion {
	return &projects.RuntimeVarVersion{
		ProjectID: m.ProjectID,
		VarSetID:  m.VarSetID,
		Revision:  m.Revision,
		Vars:      m.Vars,
		Action:    m.Action,
		Summary:   m.Summary,
		Actor:     m.Actor,
		CreatedAt: m.CreatedAt,
	}
}
