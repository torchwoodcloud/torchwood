package runbook

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// runbook 引擎阶段 C · 决策层（docs/design/runbook.md §2.3 reconcile 表、§2.4
// 动词表与 attribute 生命周期段，D6/D11/D12/D20）：动词 schema 校验、读回比较
// （D12 白名单 + 归一化）与逐动作的幂等 reconcile。本层只依赖 Caller
// 抽象（见 runbook_engine.go），不直接触碰 RPC 通道——测试注入假 caller 即可
// 覆盖全部分支。
//
// 字段命名契约：动作体允许的字段集 = 对应 *Request 消息的 proto 字段集
//（protojson camelCase；create_collection/create_index 的集合 ID 字段按
// CreateCollectionRequest/CreateIndexRequest 原名用 `id`，与 proto 一一对应）。
// attributes/indexes 是唯一例外：create_collection 的引擎复合字段，发送 RPC 前
// 由引擎拆解剥离。

// ---------------------------------------------------------------------------
// 动词 schema（静态表驱动，不依赖 genproto）
// ---------------------------------------------------------------------------

// runbookFieldKind 是动作体字段的粗粒度类型（存在性 + 类型双校验；取值合法性
// 由服务端 protovalidate 兜底）。
type runbookFieldKind int

const (
	runbookFieldString runbookFieldKind = iota
	runbookFieldBool
	runbookFieldInt
	runbookFieldStringList
	runbookFieldPermissionsUpdate // PermissionsUpdate{values:[]}（update_collection）
	runbookFieldAttributeList     // create_collection 内嵌属性声明（CreateAttributeRequest 去掉 db/coll 寻址字段）
	runbookFieldIndexList         // create_collection 内嵌索引声明（CreateIndexRequest 去掉 db/coll 寻址字段）
	runbookFieldStruct            // google.protobuf.Struct 字段（YAML 映射 ↔ protojson 对象，如 asset def metadata）
)

type runbookFieldSpec struct {
	name     string
	kind     runbookFieldKind
	required bool
}

// runbookVerbSpec 声明一个动词的写字段面与写 RPC 全名。
type runbookVerbSpec struct {
	rpc    string
	fields []runbookFieldSpec
}

// runbookDBMethod 拼接 DatabasesService 方法全名。
func runbookDBMethod(name string) string {
	return "/torchwood.server.v1.DatabasesService/" + name
}

// 必填口径：proto buf.validate required 注解（create_database.id）∪ §2.4 声明
// 引用键（reconcile 寻址必需）∪ app 层显式必填（create_database/create_
// collection 的 name，见 internal/app/server/databases.go）。
var runbookVerbSpecs = map[string]runbookVerbSpec{
	"create_database": {
		rpc: runbookDBMethod("CreateDatabase"),
		fields: []runbookFieldSpec{
			{name: "id", kind: runbookFieldString, required: true},
			{name: "name", kind: runbookFieldString, required: true},
		},
	},
	"delete_database": {
		rpc: runbookDBMethod("DeleteDatabase"),
		fields: []runbookFieldSpec{
			{name: "id", kind: runbookFieldString, required: true},
		},
	},
	"create_collection": {
		rpc: runbookDBMethod("CreateCollection"),
		fields: []runbookFieldSpec{
			{name: "database_id", kind: runbookFieldString, required: true},
			{name: "id", kind: runbookFieldString, required: true},
			{name: "name", kind: runbookFieldString, required: true},
			{name: "permissions", kind: runbookFieldStringList},
			{name: "document_security", kind: runbookFieldBool},
			{name: "attributes", kind: runbookFieldAttributeList},
			{name: "indexes", kind: runbookFieldIndexList},
		},
	},
	"update_collection": {
		rpc: runbookDBMethod("UpdateCollection"),
		fields: []runbookFieldSpec{
			{name: "database_id", kind: runbookFieldString, required: true},
			{name: "collection_id", kind: runbookFieldString, required: true},
			{name: "name", kind: runbookFieldString},
			{name: "permissions", kind: runbookFieldPermissionsUpdate},
			{name: "document_security", kind: runbookFieldBool},
			{name: "disabled", kind: runbookFieldBool},
		},
	},
	"delete_collection": {
		rpc: runbookDBMethod("DeleteCollection"),
		fields: []runbookFieldSpec{
			{name: "database_id", kind: runbookFieldString, required: true},
			{name: "collection_id", kind: runbookFieldString, required: true},
		},
	},
	"create_attribute": {
		rpc: runbookDBMethod("CreateAttribute"),
		fields: []runbookFieldSpec{
			{name: "database_id", kind: runbookFieldString, required: true},
			{name: "collection_id", kind: runbookFieldString, required: true},
			{name: "key", kind: runbookFieldString, required: true},
			{name: "type", kind: runbookFieldString},
			{name: "size", kind: runbookFieldInt},
			{name: "required", kind: runbookFieldBool},
			{name: "array", kind: runbookFieldBool},
			{name: "default_value", kind: runbookFieldString},
			{name: "dims", kind: runbookFieldInt},
		},
	},
	"delete_attribute": {
		rpc: runbookDBMethod("DeleteAttribute"),
		fields: []runbookFieldSpec{
			{name: "database_id", kind: runbookFieldString, required: true},
			{name: "collection_id", kind: runbookFieldString, required: true},
			{name: "key", kind: runbookFieldString, required: true},
		},
	},
	"restore_attribute": {
		rpc: runbookDBMethod("RestoreAttribute"),
		fields: []runbookFieldSpec{
			{name: "database_id", kind: runbookFieldString, required: true},
			{name: "collection_id", kind: runbookFieldString, required: true},
			{name: "key", kind: runbookFieldString, required: true},
		},
	},
	"create_index": {
		rpc: runbookDBMethod("CreateIndex"),
		fields: []runbookFieldSpec{
			{name: "database_id", kind: runbookFieldString, required: true},
			{name: "collection_id", kind: runbookFieldString, required: true},
			{name: "id", kind: runbookFieldString, required: true},
			{name: "type", kind: runbookFieldString},
			{name: "attributes", kind: runbookFieldStringList},
			{name: "orders", kind: runbookFieldStringList},
			{name: "distance_metric", kind: runbookFieldString},
		},
	},
	"delete_index": {
		rpc: runbookDBMethod("DeleteIndex"),
		fields: []runbookFieldSpec{
			{name: "database_id", kind: runbookFieldString, required: true},
			{name: "collection_id", kind: runbookFieldString, required: true},
			{name: "index_id", kind: runbookFieldString, required: true},
		},
	},
	// —— 阶段 D · 经济三域（docs/design/runbook.md §2.4 后三行；引用键与
	// reconcile 实现见 runbook_reconcile_econ.go）——
	"create_bucket": {
		rpc: runbookStorageMethod("CreateBucket"),
		fields: []runbookFieldSpec{
			{name: "name", kind: runbookFieldString, required: true},
			{name: "permissions", kind: runbookFieldStringList},
			{name: "public", kind: runbookFieldBool},
		},
	},
	// YAML `name` 是引用键（UpdateBucketRequest.id 由引擎按 name 解析注入）；
	// proto 的 optional name 与引用键同名——透传为同值自设，重命名不在本动词表达。
	"update_bucket": {
		rpc: runbookStorageMethod("UpdateBucket"),
		fields: []runbookFieldSpec{
			{name: "name", kind: runbookFieldString, required: true},
			{name: "public", kind: runbookFieldBool},
		},
	},
	"delete_bucket": {
		rpc: runbookStorageMethod("DeleteBucket"),
		fields: []runbookFieldSpec{
			{name: "name", kind: runbookFieldString, required: true},
		},
	},
	"create_asset_def": {
		rpc: runbookAssetsMethod("CreateAssetDef"),
		fields: []runbookFieldSpec{
			{name: "code", kind: runbookFieldString, required: true},
			{name: "name", kind: runbookFieldString, required: true},
			{name: "class", kind: runbookFieldString},
			{name: "decimals", kind: runbookFieldInt},
			{name: "max_quantity", kind: runbookFieldInt},
			{name: "expires_in", kind: runbookFieldInt},
			{name: "tradable", kind: runbookFieldBool},
			{name: "unique_per_owner", kind: runbookFieldBool},
			{name: "upgradeable", kind: runbookFieldBool},
			{name: "metadata", kind: runbookFieldStruct},
		},
	},
	// `code` 是引用键（def_id 由引擎反查注入并在发送前剥离——
	// UpdateAssetDefRequest 无 code 字段，class/code 不可变）。
	"update_asset_def": {
		rpc: runbookAssetsMethod("UpdateAssetDef"),
		fields: []runbookFieldSpec{
			{name: "code", kind: runbookFieldString, required: true},
			{name: "name", kind: runbookFieldString},
			{name: "decimals", kind: runbookFieldInt},
			{name: "max_quantity", kind: runbookFieldInt},
			{name: "expires_in", kind: runbookFieldInt},
			{name: "tradable", kind: runbookFieldBool},
			{name: "unique_per_owner", kind: runbookFieldBool},
			{name: "upgradeable", kind: runbookFieldBool},
			{name: "metadata", kind: runbookFieldStruct},
			{name: "status", kind: runbookFieldString},
		},
	},
	"delete_asset_def": {
		rpc: runbookAssetsMethod("DeleteAssetDef"),
		fields: []runbookFieldSpec{
			{name: "code", kind: runbookFieldString, required: true},
		},
	},
	"create_leaderboard_board": {
		rpc: runbookLeaderboardsMethod("CreateLeaderboardBoard"),
		fields: []runbookFieldSpec{
			{name: "id", kind: runbookFieldString, required: true},
			{name: "sort", kind: runbookFieldString},
			{name: "tiebreak_order", kind: runbookFieldString},
			{name: "tie_break", kind: runbookFieldString},
			{name: "period_kind", kind: runbookFieldString},
			{name: "period_tz", kind: runbookFieldString},
			{name: "policy", kind: runbookFieldString},
			{name: "value_min", kind: runbookFieldInt},
			{name: "value_max", kind: runbookFieldInt},
			{name: "client_submit", kind: runbookFieldBool},
			{name: "per_subject_submit_limit", kind: runbookFieldInt},
			{name: "retention_periods", kind: runbookFieldInt},
			{name: "subject_kind", kind: runbookFieldString},
		},
	},
	"update_leaderboard_board": {
		rpc: runbookLeaderboardsMethod("UpdateLeaderboardBoard"),
		fields: []runbookFieldSpec{
			{name: "board_id", kind: runbookFieldString, required: true},
			{name: "sort", kind: runbookFieldString},
			{name: "tiebreak_order", kind: runbookFieldString},
			{name: "clear_tiebreak", kind: runbookFieldBool},
			{name: "tie_break", kind: runbookFieldString},
			{name: "period_kind", kind: runbookFieldString},
			{name: "period_tz", kind: runbookFieldString},
			{name: "policy", kind: runbookFieldString},
			{name: "value_min", kind: runbookFieldInt},
			{name: "value_max", kind: runbookFieldInt},
			{name: "clear_value_bounds", kind: runbookFieldBool},
			{name: "client_submit", kind: runbookFieldBool},
			{name: "per_subject_submit_limit", kind: runbookFieldInt},
			{name: "retention_periods", kind: runbookFieldInt},
			{name: "subject_kind", kind: runbookFieldString},
		},
	},
}

// runbookEmbeddedAttributeFields 是 create_collection.attributes 列表项的字段面
// （CreateAttributeRequest 去掉 database_id/collection_id——引擎注入）。
var runbookEmbeddedAttributeFields = []runbookFieldSpec{
	{name: "key", kind: runbookFieldString, required: true},
	{name: "type", kind: runbookFieldString},
	{name: "size", kind: runbookFieldInt},
	{name: "required", kind: runbookFieldBool},
	{name: "array", kind: runbookFieldBool},
	{name: "default_value", kind: runbookFieldString},
	{name: "dims", kind: runbookFieldInt},
}

// runbookEmbeddedIndexFields 是 create_collection.indexes 列表项的字段面
// （CreateIndexRequest 去掉 database_id/collection_id——引擎注入）。
var runbookEmbeddedIndexFields = []runbookFieldSpec{
	{name: "id", kind: runbookFieldString, required: true},
	{name: "type", kind: runbookFieldString},
	{name: "attributes", kind: runbookFieldStringList},
	{name: "orders", kind: runbookFieldStringList},
	{name: "distance_metric", kind: runbookFieldString},
}

// validateRunbookVerbs 对全部文件的 up/down 动作做字段级校验（加载层只认结构，
// 词表归这里）。未知动词 / 未知字段 / 缺必填 / 类型不符均为加载期错误
// （D13 口径：错误输入直接 fail，不静默跳过）。
func validateRunbookVerbs(files []runbookFile) error {
	for i := range files {
		f := &files[i]
		for _, section := range []struct {
			name    string
			actions []runbookAction
		}{{"up", f.Up}, {"down", f.Down}} {
			for j, a := range section.actions {
				where := fmt.Sprintf("%s: %s action #%d", f.FileName, section.name, j+1)
				spec, ok := runbookVerbSpecs[a.Verb]
				if !ok {
					return fmt.Errorf("%s: unknown verb %q (supported: %s)", where, a.Verb, runbookSupportedVerbs())
				}
				if err := validateRunbookActionFields(spec.fields, a.Body, where); err != nil {
					return fmt.Errorf("%s: verb %q: %w", where, a.Verb, err)
				}
			}
		}
	}
	return nil
}

// validateRunbookActionFields 校验单个动作体：字段集 = 声明集，必填存在，
// 粗粒度类型匹配；复合列表项递归套用内嵌字段面。未知字段先于缺必填报错——
// 拼错字段名（如把 id 写成 collection_id）时，指向拼错本身比指向"缺 id"更有用。
func validateRunbookActionFields(fields []runbookFieldSpec, body map[string]any, where string) error {
	allowed := make(map[string]runbookFieldSpec, len(fields))
	for _, f := range fields {
		allowed[f.name] = f
	}
	for _, key := range sortedRunbookKeys(body) {
		v := body[key]
		spec, ok := allowed[key]
		if !ok {
			return fmt.Errorf("unknown field %q (allowed: %s; action body fields mirror the matching *Request proto message)", key, runbookFieldNames(fields))
		}
		if err := validateRunbookFieldValue(spec, v, where+"."+key); err != nil {
			return err
		}
	}
	for _, f := range fields {
		if !f.required {
			continue
		}
		v, ok := body[f.name]
		if !ok {
			return fmt.Errorf("missing required field %q (allowed: %s)", f.name, runbookFieldNames(fields))
		}
		if s, isStr := v.(string); f.kind == runbookFieldString && isStr && s == "" {
			return fmt.Errorf("required field %q must not be empty", f.name)
		}
	}
	return nil
}

// validateRunbookFieldValue 校验单字段的取值形态。
func validateRunbookFieldValue(spec runbookFieldSpec, v any, where string) error {
	switch spec.kind {
	case runbookFieldString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s must be a string, got %T", where, v)
		}
	case runbookFieldBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s must be a boolean, got %T", where, v)
		}
	case runbookFieldInt:
		if !isRunbookIntValue(v) {
			return fmt.Errorf("%s must be an integer, got %T", where, v)
		}
	case runbookFieldStringList:
		if _, err := runbookStringListValue(v, where); err != nil {
			return err
		}
	case runbookFieldPermissionsUpdate:
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be a mapping of the PermissionsUpdate message, e.g. {values: ['read:users']}", where)
		}
		for _, key := range sortedRunbookKeys(m) {
			if key != "values" {
				return fmt.Errorf("%s: unknown field %q (PermissionsUpdate only has values)", where, key)
			}
		}
		if _, err := runbookStringListValue(m["values"], where+".values"); err != nil {
			return err
		}
	case runbookFieldStruct:
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("%s must be a mapping (google.protobuf.Struct / JSON object), got %T", where, v)
		}
	case runbookFieldAttributeList, runbookFieldIndexList:
		items, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s must be a list of attribute/index declarations", where)
		}
		embedded := runbookEmbeddedAttributeFields
		if spec.kind == runbookFieldIndexList {
			embedded = runbookEmbeddedIndexFields
		}
		for i, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("%s[%d] must be a mapping", where, i)
			}
			if err := validateRunbookActionFields(embedded, m, fmt.Sprintf("%s[%d]", where, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// runbookStringListValue 校验字符串列表形态并返回展开结果。
func runbookStringListValue(v any, where string) ([]string, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of strings, got %T", where, v)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string, got %T", where, i, item)
		}
		out = append(out, s)
	}
	return out, nil
}

// isRunbookIntValue 容忍 YAML 解码出的各整型形态（int/int64/uint64/整值 float）。
func isRunbookIntValue(v any) bool {
	switch t := v.(type) {
	case int, int64, uint64:
		return true
	case float64:
		return t == float64(int64(t))
	default:
		return false
	}
}

func runbookFieldNames(fields []runbookFieldSpec) string {
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.required {
			names = append(names, f.name+"*")
		} else {
			names = append(names, f.name)
		}
	}
	return strings.Join(names, ", ")
}

func runbookSupportedVerbs() string {
	verbs := make([]string, 0, len(runbookVerbSpecs))
	for v := range runbookVerbSpecs {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs)
	return strings.Join(verbs, ", ")
}

func sortedRunbookKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// D12/D20 比较白名单与归一化
// ---------------------------------------------------------------------------

// runbookFieldDiff 是结构化漂移（D12）：字段/期望/实际，直接进错误输出。
type runbookFieldDiff struct {
	Field string `json:"field"`
	Want  any    `json:"want"`
	Got   any    `json:"got"`
}

// runbookDriftError 是 create 遇"已存在但不等"的 fail 载体（D6：永不自动
// update，迁移文件是不可变历史）。
type runbookDriftError struct {
	resource string
	diffs    []runbookFieldDiff
}

func (e *runbookDriftError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s already exists with a different configuration (D6: the engine never auto-updates):\n", e.resource)
	for _, d := range e.diffs {
		fmt.Fprintf(&b, "  %s: want %s, got %s\n", d.Field, renderRunbookValue(d.Want), renderRunbookValue(d.Got))
	}
	b.WriteString("fix: align the resource via a NEW runbook step, or repair it manually and re-run")
	return b.String()
}

func renderRunbookValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "<absent>"
	case []string:
		return "[" + strings.Join(t, ", ") + "]"
	case string:
		return strconv.Quote(t)
	case map[string]any, []any:
		// Struct 字段（asset def metadata 等）：JSON 规范形（键序无关）渲染。
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ---- want 侧（YAML 动作体，proto 字段名）提取：缺省按 proto3 零值 ----

func runbookWantStr(body map[string]any, key string) string {
	s, _ := body[key].(string)
	return s
}

func runbookWantBool(body map[string]any, key string) bool {
	b, _ := body[key].(bool)
	return b
}

func runbookWantInt(body map[string]any, key string) int64 {
	switch t := body[key].(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case uint64:
		if t > math.MaxInt64 {
			return 0
		}
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

func runbookWantStrList(body map[string]any, key string) []string {
	items, _ := body[key].([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

func runbookBodyList(body map[string]any, key string) []any {
	items, _ := body[key].([]any)
	return items
}

// ---- got 侧（protojson 读回，camelCase 键；零值字段被 protojson 省略）提取 ----

func runbookGotStr(got map[string]any, key string) string {
	s, _ := got[key].(string)
	return s
}

func runbookGotBool(got map[string]any, key string) bool {
	b, _ := got[key].(bool)
	return b
}

func runbookGotInt(got map[string]any, key string) int64 {
	switch t := got[key].(type) {
	case json.Number:
		n, _ := t.Int64()
		return n
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	}
	return 0
}

func runbookGotStrList(got map[string]any, key string) []string {
	items, _ := got[key].([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

func runbookAnyList(v any) []any {
	items, _ := v.([]any)
	return items
}

// compareRunbookDatabase：D12 白名单 database = name。
func compareRunbookDatabase(want, got map[string]any) []runbookFieldDiff {
	var diffs []runbookFieldDiff
	if w, g := runbookWantStr(want, "name"), runbookGotStr(got, "name"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "name", Want: w, Got: g})
	}
	return diffs
}

// compareRunbookCollection：D12 白名单 collection = name / permissions（D20 排序
// 归一）/ document_security / disabled。声明未写 permissions（或写空）时不比
// 较：服务端对空声明赋予缺省权限集（ParsePermissionStrings([]) → defaults），
// 声明侧无断言可比，强行比较必误报。
func compareRunbookCollection(want, got map[string]any) []runbookFieldDiff {
	var diffs []runbookFieldDiff
	if w, g := runbookWantStr(want, "name"), runbookGotStr(got, "name"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "name", Want: w, Got: g})
	}
	if decl := runbookBodyList(want, "permissions"); len(decl) > 0 {
		w, g := normalizeDeclaredRunbookPermissions(decl), normalizeGotRunbookPermissions(got["permissions"])
		if !runbookStrSlicesEqual(w, g) {
			diffs = append(diffs, runbookFieldDiff{Field: "permissions", Want: w, Got: g})
		}
	}
	// create 侧无 disabled 字段（CreateCollectionRequest 没有），期望恒 false；
	// document_security 是 optional bool，缺省 false。
	if w, g := runbookWantBool(want, "document_security"), runbookGotBool(got, "documentSecurity"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "document_security", Want: w, Got: g})
	}
	if g := runbookGotBool(got, "disabled"); g {
		diffs = append(diffs, runbookFieldDiff{Field: "disabled", Want: false, Got: g})
	}
	return diffs
}

// compareRunbookAttribute：D12 白名单 attribute = type/size/required/array/
// default_value/dims（dims 零值两侧等价：仅 vector 类型非零）。
func compareRunbookAttribute(want, got map[string]any) []runbookFieldDiff {
	var diffs []runbookFieldDiff
	if w, g := runbookWantStr(want, "type"), runbookGotStr(got, "type"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "type", Want: w, Got: g})
	}
	if w, g := runbookWantInt(want, "size"), runbookGotInt(got, "size"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "size", Want: w, Got: g})
	}
	if w, g := runbookWantBool(want, "required"), runbookGotBool(got, "required"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "required", Want: w, Got: g})
	}
	if w, g := runbookWantBool(want, "array"), runbookGotBool(got, "array"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "array", Want: w, Got: g})
	}
	if w, g := runbookWantStr(want, "default_value"), runbookGotStr(got, "defaultValue"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "default_value", Want: w, Got: g})
	}
	if w, g := runbookWantInt(want, "dims"), runbookGotInt(got, "dims"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "dims", Want: w, Got: g})
	}
	return diffs
}

// compareRunbookIndex：D12 白名单 index = type/attributes/orders/distance_metric。
// attributes/orders 是有序语义（索引列序）不排序；distance_metric 按服务端
// 归一口径比较（大写；hnsw 缺省 COSINE，见 CreateIndex 映射）。
func compareRunbookIndex(want, got map[string]any) []runbookFieldDiff {
	var diffs []runbookFieldDiff
	idxType := runbookWantStr(want, "type")
	if w, g := idxType, runbookGotStr(got, "type"); w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "type", Want: w, Got: g})
		idxType = runbookGotStr(got, "type")
	}
	if w, g := runbookWantStrList(want, "attributes"), runbookGotStrList(got, "attributes"); !runbookStrSlicesEqual(w, g) {
		diffs = append(diffs, runbookFieldDiff{Field: "attributes", Want: w, Got: g})
	}
	if w, g := runbookWantStrList(want, "orders"), runbookGotStrList(got, "orders"); !runbookStrSlicesEqual(w, g) {
		diffs = append(diffs, runbookFieldDiff{Field: "orders", Want: w, Got: g})
	}
	w := normalizeRunbookIndexMetric(idxType, runbookWantStr(want, "distance_metric"))
	g := normalizeRunbookIndexMetric(idxType, runbookGotStr(got, "distanceMetric"))
	if w != g {
		diffs = append(diffs, runbookFieldDiff{Field: "distance_metric", Want: w, Got: g})
	}
	return diffs
}

// normalizeRunbookIndexMetric 镜像服务端归一（internal/api/servergrpc/
// databases.go：大写化 + hnsw 缺省 COSINE）：声明未写 distance_metric 的
// hnsw 索引读回是 "COSINE"，不归一就会双跑误报漂移。
func normalizeRunbookIndexMetric(idxType, metric string) string {
	metric = strings.ToUpper(strings.TrimSpace(metric))
	if strings.EqualFold(strings.TrimSpace(idxType), "hnsw") && metric == "" {
		return "COSINE"
	}
	return metric
}

// normalizeDeclaredRunbookPermissions 镜像服务端 ParsePermissionStrings 的写侧
// 展开（"write:role" → create/update/delete:role）后排序（D20：权限数组顺序无
// 语义）。不展开的话声明 ["write:users"] 对读回 ["create:users","delete:users",
// "update:users"] 必误报漂移，双跑幂等被破坏。非法格式原样保留（diff 可读）。
func normalizeDeclaredRunbookPermissions(items []any) []string {
	var out []string
	for _, item := range items {
		s, _ := item.(string)
		s = strings.TrimSpace(s)
		typ, role, ok := strings.Cut(s, ":")
		if ok && typ == "write" && role != "" {
			out = append(out, "create:"+role, "update:"+role, "delete:"+role)
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func normalizeGotRunbookPermissions(v any) []string {
	out := runbookGotStrList(map[string]any{"permissions": v}, "permissions")
	sort.Strings(out)
	return out
}

func runbookStrSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// reconcile（幂等核心，每个动作先读后写）
// ---------------------------------------------------------------------------

// ActionReport 是一个（子）操作的进度与摘要报告。
type ActionReport struct {
	Verb   string `json:"verb"`
	Target string `json:"target"`
	Result string `json:"result"` // created | skipped | deleted | restored | updated | planned（dry-run）
}

// runbookActionOutcome 是一个声明动作的执行产物：Report 进 summary（声明动作
// 粒度），Events 是进度行粒度（create_collection 复合动作拆为集合/属性/索引
// 多个子操作）。
type runbookActionOutcome struct {
	Report ActionReport
	Events []ActionReport
}

// runbookReconciler 执行单个动作的幂等 reconcile（§2.3 表）。dryRun 时读路径
// 照常（计划要真实）、写路径短路为 planned。
type runbookReconciler struct {
	caller Caller
	dryRun bool
}

func (r *runbookReconciler) write(method string, req map[string]any) (map[string]any, error) {
	if r.dryRun {
		return nil, nil
	}
	return r.caller(method, req)
}

// done 把执行结果映射为 dry-run 下的 planned。
func (r *runbookReconciler) done(result string) string {
	if r.dryRun {
		return "planned"
	}
	return result
}

func runbookReport(verb, target, result string) ActionReport {
	return ActionReport{Verb: verb, Target: target, Result: result}
}

func runbookSimpleOutcome(verb, target, result string) runbookActionOutcome {
	rep := runbookReport(verb, target, result)
	return runbookActionOutcome{Report: rep, Events: []ActionReport{rep}}
}

// action 是动词分发入口（schema 校验已前置，这里不会遇到未知动词）。
func (r *runbookReconciler) action(a runbookAction) (runbookActionOutcome, error) {
	switch a.Verb {
	case "create_database":
		return r.createDatabase(a.Body)
	case "delete_database":
		return r.deleteDatabase(a.Body)
	case "create_collection":
		return r.createCollection(a.Body)
	case "update_collection":
		return r.updateCollection(a.Body)
	case "delete_collection":
		return r.deleteCollection(a.Body)
	case "create_attribute":
		return r.createAttribute(a.Body)
	case "delete_attribute":
		return r.deleteAttribute(a.Body)
	case "restore_attribute":
		return r.restoreAttribute(a.Body)
	case "create_index":
		return r.createIndex(a.Body)
	case "delete_index":
		return r.deleteIndex(a.Body)
	case "create_bucket":
		return r.createBucket(a.Body)
	case "update_bucket":
		return r.updateBucket(a.Body)
	case "delete_bucket":
		return r.deleteBucket(a.Body)
	case "create_asset_def":
		return r.createAssetDef(a.Body)
	case "update_asset_def":
		return r.updateAssetDef(a.Body)
	case "delete_asset_def":
		return r.deleteAssetDef(a.Body)
	case "create_leaderboard_board":
		return r.createLeaderboardBoard(a.Body)
	case "update_leaderboard_board":
		return r.updateLeaderboardBoard(a.Body)
	}
	return runbookActionOutcome{}, fmt.Errorf("unsupported verb %q", a.Verb)
}

// ---- 读路径 ----

// getDatabase 读库：NotFound 归一为 (nil, nil)（资源不存在）。
func (r *runbookReconciler) getDatabase(id string) (map[string]any, error) {
	resp, err := r.caller(runbookDBMethod("GetDatabase"), map[string]any{"id": id})
	if err != nil {
		if isRunbookCode(err, runbookCodeNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return resp, nil
}

// getCollection 读集合：NotFound 归一为 (nil, nil)。
func (r *runbookReconciler) getCollection(databaseID, collectionID string) (map[string]any, error) {
	resp, err := r.caller(runbookDBMethod("GetCollection"), map[string]any{
		"database_id":   databaseID,
		"collection_id": collectionID,
	})
	if err != nil {
		if isRunbookCode(err, runbookCodeNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return resp, nil
}

// requireCollection 读集合且 NotFound 为硬错误（attribute/index 动词的寻址
// 前置：集合不在，动作无从谈起）。
func (r *runbookReconciler) requireCollection(databaseID, collectionID string) (map[string]any, error) {
	resp, err := r.getCollection(databaseID, collectionID)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("collection %s.%s does not exist (create it in an earlier step or action first)", databaseID, collectionID)
	}
	return resp, nil
}

// runbookFindAttribute 在集合读回里找属性；retired 状态的条目仍会返回（生命
// 周期判定在调用方）。
func runbookFindAttribute(coll map[string]any, key string) map[string]any {
	for _, item := range runbookAnyList(coll["attributes"]) {
		m, ok := item.(map[string]any)
		if ok && runbookGotStr(m, "key") == key {
			return m
		}
	}
	return nil
}

// runbookFindIndex 在集合读回里按 id 找索引。
func runbookFindIndex(coll map[string]any, id string) map[string]any {
	for _, item := range runbookAnyList(coll["indexes"]) {
		m, ok := item.(map[string]any)
		if ok && runbookGotStr(m, "id") == id {
			return m
		}
	}
	return nil
}

// runbookAttrStatus 归一属性生命周期状态：protojson 省略 active（proto
// optional 缺省），故无 status 键 = active；nil（不存在）返回空串。
func runbookAttrStatus(attr map[string]any) string {
	if attr == nil {
		return ""
	}
	if s, _ := attr["status"].(string); s != "" {
		return s
	}
	return "active"
}

// ---- create/delete 同构骨架（§2.3 表的机械部分） ----

// ensureCreate 覆盖 create 类动词的同构形状：不存在 → 建；存在且白名单相等 →
// skip；存在但不等 → fail 带 diff；执行撞 AlreadyExists（TOCTOU：先读后建非
// 原子）→ 重读比较收敛（相等 skip / 不等 fail）。
func (r *runbookReconciler) ensureCreate(
	resource string,
	read func() (map[string]any, error),
	rpc string,
	req map[string]any,
	compare func(got map[string]any) []runbookFieldDiff,
) (string, error) {
	got, err := read()
	if err != nil {
		return "", err
	}
	if got == nil {
		if _, err := r.write(rpc, req); err != nil {
			if !isRunbookCode(err, runbookCodeAlreadyExists) {
				return "", err
			}
			// TOCTOU 收敛：并发方（双跑或手工）刚建了同名资源。
			if got, err = read(); err != nil {
				return "", err
			}
			if got == nil {
				return "", err // 撞了又消失的罕见竞态，如实上抛原始错误
			}
		} else {
			return r.done("created"), nil
		}
	}
	if diffs := compare(got); len(diffs) > 0 {
		return "", &runbookDriftError{resource: resource, diffs: diffs}
	}
	return "skipped", nil
}

// ensureDelete 覆盖 delete 类动词的同构形状：不存在 → skip；存在 → 删；执行撞
// NotFound 一律按成功（§2.3：服务端 Delete 前存在性校验与并发删除都可能产生）。
func (r *runbookReconciler) ensureDelete(
	read func() (map[string]any, error),
	rpc string,
	req map[string]any,
) (string, error) {
	got, err := read()
	if err != nil {
		return "", err
	}
	if got == nil {
		return "skipped", nil
	}
	if _, err := r.write(rpc, req); err != nil {
		if isRunbookCode(err, runbookCodeNotFound) {
			return "skipped", nil
		}
		return "", err
	}
	return r.done("deleted"), nil
}

// ---- database ----

func (r *runbookReconciler) createDatabase(body map[string]any) (runbookActionOutcome, error) {
	id := runbookWantStr(body, "id")
	result, err := r.ensureCreate("database "+id,
		func() (map[string]any, error) { return r.getDatabase(id) },
		runbookVerbSpecs["create_database"].rpc, body,
		func(got map[string]any) []runbookFieldDiff { return compareRunbookDatabase(body, got) })
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("create_database", id, result), nil
}

func (r *runbookReconciler) deleteDatabase(body map[string]any) (runbookActionOutcome, error) {
	id := runbookWantStr(body, "id")
	result, err := r.ensureDelete(
		func() (map[string]any, error) { return r.getDatabase(id) },
		runbookVerbSpecs["delete_database"].rpc, body)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("delete_database", id, result), nil
}

// ---- collection ----

// createCollection 是唯一复合动词（D5）：CreateCollection → 逐 CreateAttribute →
// 逐 CreateIndex。声明 = 全集（§2.3 特例）：线上缺的逐个补建（半应用重入），
// 线上多出或配置不等 → fail。
func (r *runbookReconciler) createCollection(body map[string]any) (runbookActionOutcome, error) {
	databaseID := runbookWantStr(body, "database_id")
	collectionID := runbookWantStr(body, "id")
	target := databaseID + "." + collectionID
	resource := "collection " + target

	// RPC 体剥离引擎复合字段，其余直传（D5）。
	createReq := make(map[string]any, len(body))
	for k, v := range body {
		createReq[k] = v
	}
	delete(createReq, "attributes")
	delete(createReq, "indexes")

	result, err := r.ensureCreate(resource,
		func() (map[string]any, error) { return r.getCollection(databaseID, collectionID) },
		runbookVerbSpecs["create_collection"].rpc, createReq,
		func(got map[string]any) []runbookFieldDiff { return compareRunbookCollection(body, got) })
	if err != nil {
		return runbookActionOutcome{}, err
	}
	events := []ActionReport{runbookReport("create_collection", target, result)}

	coll, err := r.getCollection(databaseID, collectionID)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	if coll == nil {
		// dry-run：集合创建被计划未执行，后续声明按空线上集收敛（全部 planned）。
		coll = map[string]any{}
	}
	declaredAttrs := runbookBodyList(body, "attributes")
	declaredIdxs := runbookBodyList(body, "indexes")
	if err := rejectRunbookUndeclaredAttributes(coll, declaredAttrs, resource); err != nil {
		return runbookActionOutcome{}, err
	}
	if err := rejectRunbookUndeclaredIndexes(coll, declaredIdxs, resource); err != nil {
		return runbookActionOutcome{}, err
	}
	for _, item := range declaredAttrs {
		attr, _ := item.(map[string]any)
		key := runbookWantStr(attr, "key")
		res, err := r.ensureAttribute(databaseID, collectionID, attr, true)
		if err != nil {
			return runbookActionOutcome{}, fmt.Errorf("attribute %s.%s: %w", target, key, err)
		}
		events = append(events, runbookReport("create_attribute", target+"."+key, res))
	}
	for _, item := range declaredIdxs {
		idx, _ := item.(map[string]any)
		id := runbookWantStr(idx, "id")
		res, err := r.ensureIndex(databaseID, collectionID, idx, true)
		if err != nil {
			return runbookActionOutcome{}, fmt.Errorf("index %s.%s: %w", target, id, err)
		}
		events = append(events, runbookReport("create_index", target+"."+id, res))
	}
	return runbookActionOutcome{Report: runbookReport("create_collection", target, result), Events: events}, nil
}

func (r *runbookReconciler) updateCollection(body map[string]any) (runbookActionOutcome, error) {
	target := runbookWantStr(body, "database_id") + "." + runbookWantStr(body, "collection_id")
	// update 天然幂等（设置同值无害），reconcile 直接执行不做比较（§2.4）；
	// YAML 键存在性即 proto3 optional presence，体直传。
	if _, err := r.write(runbookVerbSpecs["update_collection"].rpc, body); err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("update_collection", target, r.done("updated")), nil
}

func (r *runbookReconciler) deleteCollection(body map[string]any) (runbookActionOutcome, error) {
	target := runbookWantStr(body, "database_id") + "." + runbookWantStr(body, "collection_id")
	result, err := r.ensureDelete(
		func() (map[string]any, error) {
			return r.getCollection(runbookWantStr(body, "database_id"), runbookWantStr(body, "collection_id"))
		},
		runbookVerbSpecs["delete_collection"].rpc, body)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("delete_collection", target, result), nil
}

// ---- attribute（生命周期状态参与 reconcile，D11） ----

// ensureAttribute 处理单条属性声明：absent/retired → 建（retired 物理列已删，
// 视为不存在）；deprecated 且配置相等 → RestoreAttribute（复活，直接 Create
// 撞 key）；active → 配置比较；migrating → fail（中间态不猜）。Create 撞
// AlreadyExists → 重读重判（TOCTOU，单次重试）。
func (r *runbookReconciler) ensureAttribute(databaseID, collectionID string, attr map[string]any, assumeCollection bool) (string, error) {
	key := runbookWantStr(attr, "key")
	resource := "attribute " + databaseID + "." + collectionID + "." + key
	for attempt := 0; ; attempt++ {
		var coll map[string]any
		var err error
		if assumeCollection {
			// create_collection 复合路径：集合刚由本动作建（或 dry-run 计划建）。
			if coll, err = r.getCollection(databaseID, collectionID); err != nil {
				return "", err
			}
			if coll == nil {
				if r.dryRun {
					coll = map[string]any{}
				} else {
					return "", fmt.Errorf("collection %s.%s disappeared during reconcile", databaseID, collectionID)
				}
			}
		} else if coll, err = r.requireCollection(databaseID, collectionID); err != nil {
			return "", err
		}

		online := runbookFindAttribute(coll, key)
		switch status := runbookAttrStatus(online); status {
		case "", "retired": // 不存在 / 物理列已删：正常建
			req := runbookInjectScope(databaseID, collectionID, attr)
			if _, err := r.write(runbookVerbSpecs["create_attribute"].rpc, req); err != nil {
				if isRunbookCode(err, runbookCodeAlreadyExists) && attempt == 0 {
					continue // TOCTOU：重读现状再判
				}
				return "", err
			}
			return r.done("created"), nil
		case "migrating":
			return "", fmt.Errorf("attribute %q is migrating; re-run `runbook up` after the migration completes", key)
		case "deprecated":
			// 复活语义：配置相等才复活（D11），不等仍按 D6 fail 带 diff。
			if diffs := compareRunbookAttribute(attr, online); len(diffs) > 0 {
				return "", &runbookDriftError{resource: resource, diffs: diffs}
			}
			if _, err := r.write(runbookVerbSpecs["restore_attribute"].rpc, map[string]any{
				"database_id": databaseID, "collection_id": collectionID, "key": key,
			}); err != nil {
				return "", err
			}
			return r.done("restored"), nil
		default: // active
			if diffs := compareRunbookAttribute(attr, online); len(diffs) > 0 {
				return "", &runbookDriftError{resource: resource, diffs: diffs}
			}
			return "skipped", nil
		}
	}
}

func (r *runbookReconciler) createAttribute(body map[string]any) (runbookActionOutcome, error) {
	target := runbookWantStr(body, "database_id") + "." + runbookWantStr(body, "collection_id") + "." + runbookWantStr(body, "key")
	result, err := r.ensureAttribute(runbookWantStr(body, "database_id"), runbookWantStr(body, "collection_id"), body, false)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("create_attribute", target, result), nil
}

func (r *runbookReconciler) deleteAttribute(body map[string]any) (runbookActionOutcome, error) {
	databaseID := runbookWantStr(body, "database_id")
	collectionID := runbookWantStr(body, "collection_id")
	key := runbookWantStr(body, "key")
	target := databaseID + "." + collectionID + "." + key
	result, err := r.ensureDelete(
		func() (map[string]any, error) {
			coll, err := r.getCollection(databaseID, collectionID)
			if err != nil || coll == nil {
				// 整个集合不在 ⇒ 属性必然不在 ⇒ skip；其余错误如实上抛。
				return nil, err
			}
			attr := runbookFindAttribute(coll, key)
			if attr == nil {
				return nil, nil
			}
			// deprecated/retired 已是（或已过）软删目标态：无需再删。
			switch runbookAttrStatus(attr) {
			case "deprecated", "retired":
				return nil, nil
			}
			return attr, nil
		},
		runbookVerbSpecs["delete_attribute"].rpc, body)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("delete_attribute", target, result), nil
}

func (r *runbookReconciler) restoreAttribute(body map[string]any) (runbookActionOutcome, error) {
	databaseID := runbookWantStr(body, "database_id")
	collectionID := runbookWantStr(body, "collection_id")
	key := runbookWantStr(body, "key")
	target := databaseID + "." + collectionID + "." + key
	coll, err := r.requireCollection(databaseID, collectionID)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	attr := runbookFindAttribute(coll, key)
	if attr == nil {
		return runbookActionOutcome{}, fmt.Errorf("attribute %s not found: nothing to restore", target)
	}
	switch runbookAttrStatus(attr) {
	case "active":
		return runbookSimpleOutcome("restore_attribute", target, "skipped"), nil
	case "retired":
		return runbookActionOutcome{}, fmt.Errorf("attribute %s is retired (physical column dropped): RestoreAttribute cannot revive it — declare create_attribute instead (retired counts as absent)", target)
	}
	// deprecated / migrating → Restore（migrating 时 Restore 中止迁移并恢复 active）。
	if _, err := r.write(runbookVerbSpecs["restore_attribute"].rpc, body); err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("restore_attribute", target, r.done("restored")), nil
}

// ---- index ----

// ensureIndex 处理单条索引声明：不存在 → 建（AlreadyExists → TOCTOU 单次重读
// 重判）；存在 → 白名单比较（相等 skip / 不等 fail）。索引无生命周期状态。
func (r *runbookReconciler) ensureIndex(databaseID, collectionID string, idx map[string]any, assumeCollection bool) (string, error) {
	id := runbookWantStr(idx, "id")
	resource := "index " + databaseID + "." + collectionID + "." + id
	for attempt := 0; ; attempt++ {
		var coll map[string]any
		var err error
		if assumeCollection {
			if coll, err = r.getCollection(databaseID, collectionID); err != nil {
				return "", err
			}
			if coll == nil {
				if r.dryRun {
					coll = map[string]any{}
				} else {
					return "", fmt.Errorf("collection %s.%s disappeared during reconcile", databaseID, collectionID)
				}
			}
		} else if coll, err = r.requireCollection(databaseID, collectionID); err != nil {
			return "", err
		}
		if online := runbookFindIndex(coll, id); online != nil {
			if diffs := compareRunbookIndex(idx, online); len(diffs) > 0 {
				return "", &runbookDriftError{resource: resource, diffs: diffs}
			}
			return "skipped", nil
		}
		req := runbookInjectScope(databaseID, collectionID, idx)
		if _, err := r.write(runbookVerbSpecs["create_index"].rpc, req); err != nil {
			if isRunbookCode(err, runbookCodeAlreadyExists) && attempt == 0 {
				continue // TOCTOU：重读现状再判
			}
			return "", err
		}
		return r.done("created"), nil
	}
}

func (r *runbookReconciler) createIndex(body map[string]any) (runbookActionOutcome, error) {
	target := runbookWantStr(body, "database_id") + "." + runbookWantStr(body, "collection_id") + "." + runbookWantStr(body, "id")
	result, err := r.ensureIndex(runbookWantStr(body, "database_id"), runbookWantStr(body, "collection_id"), body, false)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("create_index", target, result), nil
}

func (r *runbookReconciler) deleteIndex(body map[string]any) (runbookActionOutcome, error) {
	databaseID := runbookWantStr(body, "database_id")
	collectionID := runbookWantStr(body, "collection_id")
	indexID := runbookWantStr(body, "index_id")
	target := databaseID + "." + collectionID + "." + indexID
	result, err := r.ensureDelete(
		func() (map[string]any, error) {
			coll, err := r.getCollection(databaseID, collectionID)
			if err != nil || coll == nil {
				return nil, err
			}
			return runbookFindIndex(coll, indexID), nil
		},
		runbookVerbSpecs["delete_index"].rpc, body)
	if err != nil {
		return runbookActionOutcome{}, err
	}
	return runbookSimpleOutcome("delete_index", target, result), nil
}

// runbookInjectScope 把 database_id/collection_id 注入内嵌声明体（standalone
// 动词体自带这两个键，注入同名键无副作用），返回副本。
func runbookInjectScope(databaseID, collectionID string, body map[string]any) map[string]any {
	req := make(map[string]any, len(body)+2)
	for k, v := range body {
		req[k] = v
	}
	req["database_id"] = databaseID
	req["collection_id"] = collectionID
	return req
}

// rejectRunbookUndeclaredAttributes 落实"声明 = 全集"：线上存在（active/
// deprecated/migrating）却未声明的属性 → fail；retired 忽略（物理列已删）。
func rejectRunbookUndeclaredAttributes(coll map[string]any, declared []any, resource string) error {
	declaredKeys := make(map[string]bool, len(declared))
	for _, item := range declared {
		if m, ok := item.(map[string]any); ok {
			declaredKeys[runbookWantStr(m, "key")] = true
		}
	}
	var extra []string
	for _, item := range runbookAnyList(coll["attributes"]) {
		m, ok := item.(map[string]any)
		if !ok || runbookAttrStatus(m) == "retired" {
			continue
		}
		if key := runbookGotStr(m, "key"); !declaredKeys[key] {
			extra = append(extra, key)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("%s: online attributes not declared (declarations are the full set): %s — remove them via a new step or add them to the declaration", resource, strings.Join(extra, ", "))
	}
	return nil
}

// rejectRunbookUndeclaredIndexes 同上（索引无生命周期，一概计入）。
func rejectRunbookUndeclaredIndexes(coll map[string]any, declared []any, resource string) error {
	declaredIDs := make(map[string]bool, len(declared))
	for _, item := range declared {
		if m, ok := item.(map[string]any); ok {
			declaredIDs[runbookWantStr(m, "id")] = true
		}
	}
	var extra []string
	for _, item := range runbookAnyList(coll["indexes"]) {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id := runbookGotStr(m, "id"); !declaredIDs[id] {
			extra = append(extra, id)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("%s: online indexes not declared (declarations are the full set): %s — remove them via a new step or add them to the declaration", resource, strings.Join(extra, ", "))
	}
	return nil
}
