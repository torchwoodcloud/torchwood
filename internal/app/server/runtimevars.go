package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/pkg/uow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RuntimeVars 是 server 面（Console / Server API）运行时变量用例
// （docs/design/runtime-vars.md §2.5/§2.6）：格式/类型/JSON/限额校验（D13）
// + 写路径单事务编排（LockHead → 变更 vars 行 → 快照回读 → InsertVersion →
// BumpRevisionAndPrune，规范调用序见 port 注释）+ 回滚编排。快照 JSON
// {key: {t, v, d, c}} 的编解码是本层职责（格式与 repo 集成测试的模拟实现
// 一致）。错误直接 status.Error（P3-18 约定）。
type RuntimeVars struct {
	repo projects.RuntimeVarRepository
	tx   uow.Runner
}

func NewRuntimeVars(repo projects.RuntimeVarRepository, tx uow.Runner) *RuntimeVars {
	return &RuntimeVars{repo: repo, tx: tx}
}

// D13 限额（集合级）：集合 ≤20/项目、vars ≤500/集合、单值 ≤64KB（JSON 字节）、
// 集合总值 ≤1MB。版本保留窗口（50 版）由 repo 的 BumpRevisionAndPrune 执行。
const (
	RuntimeVarsMaxSetsPerProject = 20
	RuntimeVarsMaxKeysPerSet     = 500
	RuntimeVarsMaxValueBytes     = 64 << 10 // 64KB
	RuntimeVarsMaxTotalBytes     = 1 << 20  // 1MB
	// RuntimeVarsMaxJSONDepth 是 json 值嵌套深度上限（D3：64KB 的 "[[[[…" 可
	// 构造 ~3 万层，encoding/json 万层栈保护错误不可读，显式限制给出友好错误）。
	RuntimeVarsMaxJSONDepth = 100
)

// runtimeVarNamePattern 是 key 与 var_set_id 共用的格式约束（D13）；
// 差异仅在长度（key ≤64、var_set_id ≤40）。
var runtimeVarNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ---------------------------------------------------------------------------
// 值输入（proto 无关）：oneof 判别 + 标准化 JSON 文本
// ---------------------------------------------------------------------------

// RuntimeVarValueInput 是写侧值输入：ValueType 为类型锚点（D3），Value 为
// 标准化 JSON 文本（标量即 JSON 标量，如 `"hello"` / `42` / `true`）。
type RuntimeVarValueInput struct {
	ValueType string
	Value     string
}

// StringVarValue 构造 string 值（JSON 文本转义由本层负责）。
func StringVarValue(s string) RuntimeVarValueInput {
	b, _ := json.Marshal(s)
	return RuntimeVarValueInput{ValueType: projects.RuntimeVarTypeString, Value: string(b)}
}

// IntegerVarValue 构造 integer 值（±2^53−1 边界由 protovalidate 写入拦截，
// 这里不重复断言——int64 宿主类型本身承载更大值，超界请求到不了本层）。
func IntegerVarValue(n int64) RuntimeVarValueInput {
	return RuntimeVarValueInput{ValueType: projects.RuntimeVarTypeInteger, Value: strconv.FormatInt(n, 10)}
}

// FloatVarValue 构造 float 值；NaN/Inf 不是合法 JSON number，直接拒绝。
func FloatVarValue(f float64) (RuntimeVarValueInput, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return RuntimeVarValueInput{}, status.Error(codes.InvalidArgument, "float value is not a valid JSON number")
	}
	return RuntimeVarValueInput{ValueType: projects.RuntimeVarTypeFloat, Value: string(b)}, nil
}

// BoolVarValue 构造 boolean 值。
func BoolVarValue(b bool) RuntimeVarValueInput {
	return RuntimeVarValueInput{ValueType: projects.RuntimeVarTypeBoolean, Value: strconv.FormatBool(b)}
}

// JSONVarValue 构造 json 值：校验合法 JSON + 嵌套深度 ≤100，文本原样存储
// （PG JSONB 落库时自行归一键序/空白）。
func JSONVarValue(s string) (RuntimeVarValueInput, error) {
	depth, err := jsonDepth(s)
	if err != nil {
		return RuntimeVarValueInput{}, status.Error(codes.InvalidArgument, "json_value is not valid JSON")
	}
	if depth > RuntimeVarsMaxJSONDepth {
		return RuntimeVarValueInput{}, status.Errorf(codes.InvalidArgument,
			"json_value nesting depth %d exceeds limit %d", depth, RuntimeVarsMaxJSONDepth)
	}
	return RuntimeVarValueInput{ValueType: projects.RuntimeVarTypeJSON, Value: s}, nil
}

// jsonDepth 返回 JSON 文本的嵌套深度（顶层标量 = 0）；非法 JSON 返回错误
// （空串/尾随垃圾同拒）。json.Valid 先行把关，再用 json.Decoder.Token 流式
// 走查深度：不递归，深度本身不受调用栈限制。
func jsonDepth(s string) (int, error) {
	if !json.Valid([]byte(s)) {
		return 0, fmt.Errorf("invalid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	depth, max := 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return max, nil
		}
		if err != nil {
			return 0, err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
				if depth > max {
					max = depth
				}
			case '}', ']':
				depth--
			}
		}
	}
}

// checkValueLimit 校验单值 ≤64KB（JSON 字节，D13）。输入文本长度 ≥ PG
// 归一化后的存储长度（空白/键序压缩方向），按输入度量是保守侧近似。
func checkValueLimit(v RuntimeVarValueInput) error {
	if len(v.Value) > RuntimeVarsMaxValueBytes {
		return status.Errorf(codes.ResourceExhausted,
			"runtime var value exceeds %d bytes (got %d)", RuntimeVarsMaxValueBytes, len(v.Value))
	}
	return nil
}

// checkSetLimit 校验集合总值 ≤1MB：以 Footprint 度量 + 本次写入增量估算
// （写事务内、heads 行已持锁，度量与写入之间无并发写插入）。
func checkSetLimit(currentBytes int64, oldValueLen, newValueLen int) error {
	total := currentBytes - int64(oldValueLen) + int64(newValueLen)
	if total > RuntimeVarsMaxTotalBytes {
		return status.Errorf(codes.ResourceExhausted,
			"runtime var set total size would exceed %d bytes (projected %d)", RuntimeVarsMaxTotalBytes, total)
	}
	return nil
}

// validateVarSetID / validateVarKey 校验格式（D13：var_set_id 同正则 ≤40、
// key ≤64）。protovalidate 已在拦截器链尾拦截 RPC 形状，这里兜底非 RPC 调用
// 方（测试/未来内部路径）。
func validateVarSetID(id string) error {
	if len(id) == 0 || len(id) > 40 || !runtimeVarNamePattern.MatchString(id) {
		return status.Error(codes.InvalidArgument, "var_set_id must match ^[a-z_][a-z0-9_]*$ and be at most 40 characters")
	}
	return nil
}

func validateVarKey(key string) error {
	if len(key) == 0 || len(key) > 64 || !runtimeVarNamePattern.MatchString(key) {
		return status.Error(codes.InvalidArgument, "key must match ^[a-z_][a-z0-9_]*$ and be at most 64 characters")
	}
	return nil
}

func validateVisibility(v string) error {
	switch v {
	case projects.VarSetVisibilityPublic, projects.VarSetVisibilityPrivate:
		return nil
	default:
		return status.Error(codes.InvalidArgument, "visibility must be public or private")
	}
}

// ---------------------------------------------------------------------------
// 视图（资源投影：etag/revision/footprint）
// ---------------------------------------------------------------------------

// VarSetView 是 VarSet 的 API 投影：域行 + 当前 revision + footprint
// （Console 展示 var_count/total_bytes，§2.7）+ 对外 etag（D2 "{epoch}:{revision}"）。
type VarSetView struct {
	VarSet     projects.VarSet
	Revision   int64
	VarCount   int
	TotalBytes int64
	ETag       string
}

func (r *RuntimeVars) view(ctx context.Context, vs projects.VarSet) (*VarSetView, error) {
	revision, _, err := r.repo.GetHead(ctx, vs.ProjectID, vs.VarSetID)
	if err != nil {
		return nil, err
	}
	count, totalBytes, err := r.repo.Footprint(ctx, vs.ProjectID, vs.VarSetID)
	if err != nil {
		return nil, err
	}
	return &VarSetView{
		VarSet:     vs,
		Revision:   revision,
		VarCount:   count,
		TotalBytes: totalBytes,
		ETag:       vs.Epoch + ":" + strconv.FormatInt(revision, 10),
	}, nil
}

// ---------------------------------------------------------------------------
// 集合 CRUD
// ---------------------------------------------------------------------------

// CreateVarSetCommand 创建集合（D9 显式实体，无缺省集合）。epoch 由本层随机
// 生成（8 字节 hex，D2）；heads 行由 repo 在同一事务建立（revision=0）。
// 集合创建不入版本链（D10），actor 由审计拦截器按 FullMethod 自动记录。
type CreateVarSetCommand struct {
	ProjectID   string
	VarSetID    string
	Visibility  string // 空 = 缺省 public
	Description string
}

func (r *RuntimeVars) CreateVarSet(ctx context.Context, cmd CreateVarSetCommand) (*VarSetView, error) {
	if err := validateVarSetID(cmd.VarSetID); err != nil {
		return nil, err
	}
	visibility := cmd.Visibility
	if visibility == "" {
		visibility = projects.VarSetVisibilityPublic
	}
	if err := validateVisibility(visibility); err != nil {
		return nil, err
	}
	existing, err := r.repo.GetVarSet(ctx, cmd.ProjectID, cmd.VarSetID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, status.Error(codes.AlreadyExists, "var set already exists")
	}
	sets, err := r.repo.ListVarSets(ctx, cmd.ProjectID)
	if err != nil {
		return nil, err
	}
	if len(sets) >= RuntimeVarsMaxSetsPerProject {
		return nil, status.Errorf(codes.ResourceExhausted,
			"runtime var set limit reached (%d per project)", RuntimeVarsMaxSetsPerProject)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, status.Error(codes.Internal, "failed to generate var set epoch")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	vs := &projects.VarSet{
		ProjectID:   cmd.ProjectID,
		VarSetID:    cmd.VarSetID,
		Visibility:  visibility,
		Epoch:       hex.EncodeToString(b),
		Description: cmd.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := r.repo.CreateVarSet(ctx, vs); err != nil {
		// 并发同名创建漏过前置检查时由主键唯一约束兜底（race 窗口极小）。
		if isRuntimeVarUniqueViolation(err) {
			return nil, status.Error(codes.AlreadyExists, "var set already exists")
		}
		return nil, err
	}
	return r.view(ctx, *vs)
}

func (r *RuntimeVars) ListVarSets(ctx context.Context, projectID string) ([]VarSetView, error) {
	sets, err := r.repo.ListVarSets(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]VarSetView, 0, len(sets))
	for i := range sets {
		v, err := r.view(ctx, sets[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

func (r *RuntimeVars) GetVarSet(ctx context.Context, projectID, varSetID string) (*VarSetView, error) {
	vs, err := r.repo.GetVarSet(ctx, projectID, varSetID)
	if err != nil {
		return nil, err
	}
	if vs == nil {
		return nil, status.Error(codes.NotFound, "var set not found")
	}
	return r.view(ctx, *vs)
}

// UpdateVarSetCommand 只改集合元数据（D10：不 bump revision、不入版本链，
// 仅进审计）。nil = 不修改（PATCH 语义）。
type UpdateVarSetCommand struct {
	ProjectID   string
	VarSetID    string
	Visibility  *string
	Description *string
}

func (r *RuntimeVars) UpdateVarSet(ctx context.Context, cmd UpdateVarSetCommand) (*VarSetView, error) {
	existing, err := r.repo.GetVarSet(ctx, cmd.ProjectID, cmd.VarSetID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, status.Error(codes.NotFound, "var set not found")
	}
	cols := map[string]any{"updated_at": time.Now().UTC().Truncate(time.Microsecond)}
	if cmd.Visibility != nil {
		if err := validateVisibility(*cmd.Visibility); err != nil {
			return nil, err
		}
		cols["visibility"] = *cmd.Visibility
	}
	if cmd.Description != nil {
		cols["description"] = *cmd.Description
	}
	if err := r.repo.UpdateVarSet(ctx, cmd.ProjectID, cmd.VarSetID, cols); err != nil {
		return nil, err
	}
	return r.GetVarSet(ctx, cmd.ProjectID, cmd.VarSetID)
}

func (r *RuntimeVars) DeleteVarSet(ctx context.Context, projectID, varSetID string) error {
	existing, err := r.repo.GetVarSet(ctx, projectID, varSetID)
	if err != nil {
		return err
	}
	if existing == nil {
		return status.Error(codes.NotFound, "var set not found")
	}
	return r.repo.DeleteVarSet(ctx, projectID, varSetID)
}

// ---------------------------------------------------------------------------
// 变量 CRUD（写路径单事务编排）
// ---------------------------------------------------------------------------

// CreateRuntimeVarCommand 新建变量（D5 无 Upsert；重复 key → AlreadyExists）。
type CreateRuntimeVarCommand struct {
	ProjectID   string
	VarSetID    string
	Key         string
	Value       RuntimeVarValueInput
	Description string
	Actor       string
}

// UpdateRuntimeVarCommand 更新变量：Value 必填（完整新值，类型可随本次变更）；
// Description nil = 不修改。
type UpdateRuntimeVarCommand struct {
	ProjectID   string
	VarSetID    string
	Key         string
	Value       RuntimeVarValueInput
	Description *string
	Actor       string
}

func (r *RuntimeVars) CreateRuntimeVar(ctx context.Context, cmd CreateRuntimeVarCommand) (*projects.RuntimeVar, error) {
	if err := validateVarSetID(cmd.VarSetID); err != nil {
		return nil, err
	}
	if err := validateVarKey(cmd.Key); err != nil {
		return nil, err
	}
	if err := checkValueLimit(cmd.Value); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	var created *projects.RuntimeVar
	err := r.writeVar(ctx, cmd.ProjectID, cmd.VarSetID, projects.RuntimeVarActionCreate,
		"create: "+cmd.Key, cmd.Actor, func(ctx context.Context) error {
			existing, err := r.repo.GetRuntimeVar(ctx, cmd.ProjectID, cmd.VarSetID, cmd.Key)
			if err != nil {
				return err
			}
			if existing != nil {
				return status.Error(codes.AlreadyExists, "runtime var key already exists")
			}
			count, totalBytes, err := r.repo.Footprint(ctx, cmd.ProjectID, cmd.VarSetID)
			if err != nil {
				return err
			}
			if count+1 > RuntimeVarsMaxKeysPerSet {
				return status.Errorf(codes.ResourceExhausted,
					"runtime var set key limit reached (%d per set)", RuntimeVarsMaxKeysPerSet)
			}
			if err := checkSetLimit(totalBytes, 0, len(cmd.Value.Value)); err != nil {
				return err
			}
			v := &projects.RuntimeVar{
				ProjectID:   cmd.ProjectID,
				VarSetID:    cmd.VarSetID,
				Key:         cmd.Key,
				ValueType:   cmd.Value.ValueType,
				Value:       cmd.Value.Value,
				Description: cmd.Description,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			if err := r.repo.CreateRuntimeVar(ctx, v); err != nil {
				if isRuntimeVarUniqueViolation(err) {
					return status.Error(codes.AlreadyExists, "runtime var key already exists")
				}
				return err
			}
			created = v
			return nil
		})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (r *RuntimeVars) GetRuntimeVar(ctx context.Context, projectID, varSetID, key string) (*projects.RuntimeVar, error) {
	if _, found, err := r.repo.GetHead(ctx, projectID, varSetID); err != nil {
		return nil, err
	} else if !found {
		return nil, status.Error(codes.NotFound, "var set not found")
	}
	v, err := r.repo.GetRuntimeVar(ctx, projectID, varSetID, key)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, status.Error(codes.NotFound, "runtime var not found")
	}
	return v, nil
}

func (r *RuntimeVars) ListRuntimeVars(ctx context.Context, projectID, varSetID string) ([]projects.RuntimeVar, error) {
	if _, found, err := r.repo.GetHead(ctx, projectID, varSetID); err != nil {
		return nil, err
	} else if !found {
		return nil, status.Error(codes.NotFound, "var set not found")
	}
	return r.repo.ListRuntimeVars(ctx, projectID, varSetID)
}

func (r *RuntimeVars) UpdateRuntimeVar(ctx context.Context, cmd UpdateRuntimeVarCommand) (*projects.RuntimeVar, error) {
	if err := validateVarSetID(cmd.VarSetID); err != nil {
		return nil, err
	}
	if err := validateVarKey(cmd.Key); err != nil {
		return nil, err
	}
	if err := checkValueLimit(cmd.Value); err != nil {
		return nil, err
	}
	var updated *projects.RuntimeVar
	err := r.writeVar(ctx, cmd.ProjectID, cmd.VarSetID, projects.RuntimeVarActionUpdate,
		"update: "+cmd.Key, cmd.Actor, func(ctx context.Context) error {
			existing, err := r.repo.GetRuntimeVar(ctx, cmd.ProjectID, cmd.VarSetID, cmd.Key)
			if err != nil {
				return err
			}
			if existing == nil {
				return status.Error(codes.NotFound, "runtime var not found")
			}
			_, totalBytes, err := r.repo.Footprint(ctx, cmd.ProjectID, cmd.VarSetID)
			if err != nil {
				return err
			}
			// 旧值读自 DB（JSONB 归一化文本），len 即其 octet_length 贡献。
			if err := checkSetLimit(totalBytes, len(existing.Value), len(cmd.Value.Value)); err != nil {
				return err
			}
			cols := map[string]any{
				"value":      cmd.Value.Value,
				"value_type": cmd.Value.ValueType,
				"updated_at": time.Now().UTC().Truncate(time.Microsecond),
			}
			if cmd.Description != nil {
				cols["description"] = *cmd.Description
			}
			if err := r.repo.UpdateRuntimeVar(ctx, cmd.ProjectID, cmd.VarSetID, cmd.Key, cols); err != nil {
				return err
			}
			updated, err = r.repo.GetRuntimeVar(ctx, cmd.ProjectID, cmd.VarSetID, cmd.Key)
			return err
		})
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, status.Error(codes.Internal, "runtime var missing after update")
	}
	return updated, nil
}

func (r *RuntimeVars) DeleteRuntimeVar(ctx context.Context, projectID, varSetID, key, actor string) error {
	if err := validateVarSetID(varSetID); err != nil {
		return err
	}
	if err := validateVarKey(key); err != nil {
		return err
	}
	return r.writeVar(ctx, projectID, varSetID, projects.RuntimeVarActionDelete,
		"delete: "+key, actor, func(ctx context.Context) error {
			existing, err := r.repo.GetRuntimeVar(ctx, projectID, varSetID, key)
			if err != nil {
				return err
			}
			if existing == nil {
				return status.Error(codes.NotFound, "runtime var not found")
			}
			return r.repo.DeleteRuntimeVar(ctx, projectID, varSetID, key)
		})
}

// writeVar 是规范写路径编排（§2.5）：uow.Run 包单事务（clients.Database 的
// Run 即 RunInTx），事务内 LockHead（FOR UPDATE，串行化同集合并发写；
// found=false = 集合不存在 → NotFound）→ 变更 vars 行 → 快照回读（以写后
// DB 状态为准）→ InsertVersion（revision = LockHead 返回值 +1）→
// BumpRevisionAndPrune（淘汰窗口计入本次新版本行，语句序见 port 注释）。
func (r *RuntimeVars) writeVar(ctx context.Context, projectID, varSetID, action, summary, actor string,
	mutate func(ctx context.Context) error,
) error {
	return r.tx.Run(ctx, func(ctx context.Context) error {
		locked, found, err := r.repo.LockHead(ctx, projectID, varSetID)
		if err != nil {
			return err
		}
		if !found {
			return status.Error(codes.NotFound, "var set not found")
		}
		if err := mutate(ctx); err != nil {
			return err
		}
		vars, err := r.repo.ListRuntimeVars(ctx, projectID, varSetID)
		if err != nil {
			return err
		}
		snap, err := runtimeVarSnapshotJSON(vars)
		if err != nil {
			return err
		}
		if err := r.repo.InsertVersion(ctx, &projects.RuntimeVarVersion{
			ProjectID: projectID,
			VarSetID:  varSetID,
			Revision:  locked + 1,
			Vars:      snap,
			Action:    action,
			Summary:   summary,
			Actor:     actor,
			CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
		}); err != nil {
			return err
		}
		newRevision, err := r.repo.BumpRevisionAndPrune(ctx, projectID, varSetID)
		if err != nil {
			return err
		}
		if newRevision != locked+1 {
			return status.Errorf(codes.Internal, "revision bump mismatch: got %d want %d", newRevision, locked+1)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// 版本链
// ---------------------------------------------------------------------------

func (r *RuntimeVars) ListRuntimeVarVersions(ctx context.Context, projectID, varSetID string) ([]projects.RuntimeVarVersion, error) {
	if _, found, err := r.repo.GetHead(ctx, projectID, varSetID); err != nil {
		return nil, err
	} else if !found {
		return nil, status.Error(codes.NotFound, "var set not found")
	}
	return r.repo.ListVersions(ctx, projectID, varSetID)
}

// RuntimeVarVersionDetail 是版本全文视图：元数据 + 快照展开（key ASC；
// UpdatedAt 零值——快照不记录该列，D11）。
type RuntimeVarVersionDetail struct {
	Version projects.RuntimeVarVersion
	Vars    []projects.RuntimeVar
}

func (r *RuntimeVars) GetRuntimeVarVersion(ctx context.Context, projectID, varSetID string, revision int64) (*RuntimeVarVersionDetail, error) {
	if _, found, err := r.repo.GetHead(ctx, projectID, varSetID); err != nil {
		return nil, err
	} else if !found {
		return nil, status.Error(codes.NotFound, "var set not found")
	}
	ver, err := r.repo.GetVersion(ctx, projectID, varSetID, revision)
	if err != nil {
		return nil, err
	}
	if ver == nil {
		return nil, status.Error(codes.NotFound, "runtime var version not found")
	}
	vars, err := runtimeVarSnapshotVars(ver.Vars)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "corrupt version snapshot: %v", err)
	}
	return &RuntimeVarVersionDetail{Version: *ver, Vars: vars}, nil
}

// Rollback 回滚（§2.5）：LockHead → target == 当前 → InvalidArgument（D12）
// → 读目标版本（nil → NotFound "target revision pruned"）→ ReplaceVars →
// InsertVersion（action=rollback、summary="rollback to {n}"、vars=目标快照
// 原样）→ BumpRevisionAndPrune。不做限额校验：快照来源时点必然合规
// （写路径已拦），回滚不可能超限。
func (r *RuntimeVars) Rollback(ctx context.Context, projectID, varSetID string, targetRevision int64, actor string) (*projects.RuntimeVarVersion, error) {
	if err := validateVarSetID(varSetID); err != nil {
		return nil, err
	}
	var rolled *projects.RuntimeVarVersion
	err := r.tx.Run(ctx, func(ctx context.Context) error {
		locked, found, err := r.repo.LockHead(ctx, projectID, varSetID)
		if err != nil {
			return err
		}
		if !found {
			return status.Error(codes.NotFound, "var set not found")
		}
		if targetRevision == locked {
			return status.Errorf(codes.InvalidArgument,
				"target revision %d equals current revision (rollback to self is not a valid operation)", targetRevision)
		}
		ver, err := r.repo.GetVersion(ctx, projectID, varSetID, targetRevision)
		if err != nil {
			return err
		}
		if ver == nil {
			return status.Errorf(codes.NotFound, "target revision %d pruned", targetRevision)
		}
		vars, err := runtimeVarSnapshotVars(ver.Vars)
		if err != nil {
			return status.Errorf(codes.Internal, "corrupt version snapshot: %v", err)
		}
		if err := r.repo.ReplaceVars(ctx, projectID, varSetID, vars); err != nil {
			return err
		}
		rolled = &projects.RuntimeVarVersion{
			ProjectID: projectID,
			VarSetID:  varSetID,
			Revision:  locked + 1,
			Vars:      ver.Vars, // 目标快照原样
			Action:    projects.RuntimeVarActionRollback,
			Summary:   fmt.Sprintf("rollback to %d", targetRevision),
			Actor:     actor,
			CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
		}
		if err := r.repo.InsertVersion(ctx, rolled); err != nil {
			return err
		}
		newRevision, err := r.repo.BumpRevisionAndPrune(ctx, projectID, varSetID)
		if err != nil {
			return err
		}
		if newRevision != locked+1 {
			return status.Errorf(codes.Internal, "revision bump mismatch: got %d want %d", newRevision, locked+1)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rolled, nil
}

// ---------------------------------------------------------------------------
// 快照 JSON 编解码（app 层职责，格式 {key: {t, v, d, c}}，D11 全列四元组）
// ---------------------------------------------------------------------------

// runtimeVarSnapshotEntry 是快照单键结构（t=value_type、v=value、
// d=description、c=created_at 的 RFC3339Nano 文本）。
type runtimeVarSnapshotEntry struct {
	T string `json:"t"`
	V string `json:"v"`
	D string `json:"d"`
	C string `json:"c"`
}

func runtimeVarSnapshotJSON(vars []projects.RuntimeVar) (string, error) {
	m := make(map[string]runtimeVarSnapshotEntry, len(vars))
	for _, v := range vars {
		m[v.Key] = runtimeVarSnapshotEntry{
			T: v.ValueType,
			V: v.Value,
			D: v.Description,
			C: v.CreatedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func runtimeVarSnapshotVars(snap string) ([]projects.RuntimeVar, error) {
	var m map[string]runtimeVarSnapshotEntry
	if err := json.Unmarshal([]byte(snap), &m); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // key ASC（与 ListRuntimeVars 的序一致）
	out := make([]projects.RuntimeVar, 0, len(keys))
	for _, k := range keys {
		e := m[k]
		c, err := time.Parse(time.RFC3339Nano, e.C)
		if err != nil {
			return nil, err
		}
		out = append(out, projects.RuntimeVar{
			Key:         k,
			ValueType:   e.T,
			Value:       e.V,
			Description: e.D,
			CreatedAt:   c,
		})
	}
	return out, nil
}

// isRuntimeVarUniqueViolation 报告 repo 返回的错误是否为 PG 唯一约束冲突
// （并发兜底映射 409；与 infra 侧 assets/leaderboards 的判定同口径）。
func isRuntimeVarUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLSTATE 23505") || strings.Contains(s, "unique constraint")
}
