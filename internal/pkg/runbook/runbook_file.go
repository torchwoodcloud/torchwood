package runbook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// runbook 文件层（docs/design/runbook.md §2.1 文件契约 + §2.3「共同前置」）：
// 加载 runbooks/ 目录下的版本化迁移文件并完成结构与版本链校验。本层只认
// 结构——动作动词的词表与字段级校验在引擎层（阶段 C），动作体原样保留为
// map 供引擎消费。

// DefaultDir 是 --dir 的缺省值（项目仓库内的 runbooks/ 目录）。
const DefaultDir = "runbooks"

// runbookFileNameRe 约束迁移文件名：NNNNNN_name.yaml——6 位零填充序号 +
// 小写 name，仅认 .yaml 后缀。
var runbookFileNameRe = regexp.MustCompile(`^([0-9]{6})_([a-z0-9_]{1,64})\.yaml$`)

// runbookNameRe 约束 name 段（`runbook new <name>` 参数同用此规则）。
var runbookNameRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// utf8BOM 是 checksum 归一化要剥掉的 UTF-8 BOM（D9）。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// runbookAction 是一条动作：恰好一个动词键 + 请求体（D5：扁平动词、请求体
// 字段直传 protojson）。Body 已归一为 map[string]any（含嵌套），本阶段不
// 做动词词表与字段级校验。
type runbookAction struct {
	Verb string
	Body map[string]any
}

// runbookFile 是单个迁移文件加载校验后的形态。
type runbookFile struct {
	Version  int64           // 文件名序号
	Name     string          // 文件名 name 段
	FileName string          // 基础文件名（错误定位用）
	Path     string          // 完整路径
	Up       []runbookAction // up 段动作（声明顺序）
	Down     []runbookAction // down 段动作；空 = 不可逆（D4：空段即语义）
	Checksum string          // sha256(归一化后文件字节) 的 hex（D9）
}

// parseRunbookFileName 解析迁移文件名；不匹配规则返回 ok=false。
func parseRunbookFileName(fileName string) (version int64, name string, ok bool) {
	m := runbookFileNameRe.FindStringSubmatch(fileName)
	if m == nil {
		return 0, "", false
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, "", false // 6 位数字不会溢出 int64，防御性兜底
	}
	return v, m[2], true
}

// runbookDoc 是迁移文件的 YAML 顶层模型：仅 up/down 两个列表键（D4 单文件
// 双段）。配合 KnownFields 严格解析——拼错的顶层键直接报错而非静默忽略
// （config.go 的 loadConfigFile 同策略）。
type runbookDoc struct {
	Up   []map[string]any `yaml:"up"`
	Down []map[string]any `yaml:"down"`
}

// parseRunbookContent 严格解析文件内容：未知顶层键报错；up/down 各为动作
// 列表。down 缺失或空列表合法（D4：空 down 段 = 不可逆，不引入 irreversible
// 字段）；一个文件只允许一个 YAML 文档。
func parseRunbookContent(data []byte, fileName string) ([]runbookAction, []runbookAction, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc runbookDoc
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, fmt.Errorf("%s: empty runbook file (need up:/down: sections)", fileName)
		}
		return nil, nil, fmt.Errorf("parse runbook %s: %w", fileName, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, nil, fmt.Errorf("parse runbook %s: expected a single YAML document", fileName)
		}
		return nil, nil, fmt.Errorf("parse runbook %s: %w", fileName, err)
	}
	up, err := parseRunbookActions("up", doc.Up, fileName)
	if err != nil {
		return nil, nil, err
	}
	down, err := parseRunbookActions("down", doc.Down, fileName)
	if err != nil {
		return nil, nil, err
	}
	return up, down, nil
}

// parseRunbookActions 把 up/down 列表的原始 map 解析为动作：一个动作 = 恰好
// 一个动词键 + map 体。0 个或 ≥2 个动词键、动词体为 null 或非映射均为解析
// 期错误（D 复查补充：null 不是合法的 presence 表达，protojson 侧同理）。
func parseRunbookActions(section string, raw []map[string]any, fileName string) ([]runbookAction, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	actions := make([]runbookAction, 0, len(raw))
	for i, item := range raw {
		where := fmt.Sprintf("%s: %s action #%d", fileName, section, i+1)
		if len(item) != 1 {
			if len(item) == 0 {
				return nil, fmt.Errorf(`%s has no verb key (one action = exactly one verb key plus its request body, e.g. "- create_collection: {...}")`, where)
			}
			verbs := make([]string, 0, len(item))
			for k := range item {
				verbs = append(verbs, k)
			}
			sort.Strings(verbs)
			return nil, fmt.Errorf("%s has %d verb keys (%s); one action carries exactly one verb - split it into separate list items", where, len(item), strings.Join(verbs, ", "))
		}
		var verb string
		var bodyRaw any
		for k, v := range item {
			verb, bodyRaw = k, v
		}
		bodyAny, err := normalizeRunbookValue(bodyRaw)
		if err != nil {
			return nil, fmt.Errorf("%s: verb %q: %w", where, verb, err)
		}
		if bodyAny == nil {
			return nil, fmt.Errorf(`%s: verb %q body is null (write an explicit mapping, "{}" if the verb takes no fields)`, where, verb)
		}
		body, ok := bodyAny.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: verb %q body must be a mapping of request fields", where, verb)
		}
		actions = append(actions, runbookAction{Verb: verb, Body: body})
	}
	return actions, nil
}

// normalizeRunbookValue 递归归一动作体：yaml.v3 把全字符串键的映射解码为
// map[string]any、含非字符串键的映射解码为 map[any]any，这里统一收敛为
// map[string]any（请求体最终直传 protojson，键必须是 proto 字段名）；
// 非字符串键就地报错（阶段 C 的引擎拿到的形态因此是确定的）。
func normalizeRunbookValue(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			nv, err := normalizeRunbookValue(vv)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = nv
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("mapping key %v is not a string (keys are proto field names)", k)
			}
			nv, err := normalizeRunbookValue(vv)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ks, err)
			}
			out[ks] = nv
		}
		return out, nil
	case []any:
		for i := range t {
			nv, err := normalizeRunbookValue(t[i])
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			t[i] = nv
		}
		return t, nil
	default:
		return v, nil
	}
}

// runbookChecksum 计算 sha256 hex。计算前归一化：去 UTF-8 BOM + CRLF→LF
// （D9：Windows 编辑器 + git autocrlf 下原始字节哈希会跨环境误报；引擎不
// 依赖 .gitattributes 建议，归一化自身保证等价）。归一化只用于 checksum，
// YAML 解析仍用原始字节。
func runbookChecksum(data []byte) string {
	data = bytes.TrimPrefix(data, utf8BOM)
	sum := sha256.Sum256(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// loadRunbookDir 加载并校验迁移目录（§2.3 共同前置）：
//   - 目录不存在 / 无合法迁移文件 → 错误（D13：打错路径的静默成功比失败危险）
//   - 带 .yaml/.yml 后缀却不匹配 NNNNNN_name.yaml 的文件按错误处理——用户
//     以为已纳入的文件被静默跳过就是环境漂移之源；其余文件（README 等）忽略
//   - 版本链从 1 起、连续、无重号
//
// 返回按版本升序的文件列表。
func loadRunbookDir(dir string) ([]runbookFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("runbook directory %s does not exist", dir)
		}
		return nil, fmt.Errorf("read runbook directory %s: %w", dir, err)
	}
	var files []runbookFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		version, rbName, ok := parseRunbookFileName(name)
		if !ok {
			if ext := filepath.Ext(name); strings.EqualFold(ext, ".yaml") || strings.EqualFold(ext, ".yml") {
				return nil, fmt.Errorf("%s: invalid runbook file name (want NNNNNN_name.yaml: 6-digit zero-padded version + name of [a-z0-9_]{1,64})", name)
			}
			continue
		}
		f, err := loadRunbookFile(filepath.Join(dir, name), version, rbName)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("runbook directory %s contains no migration files (want NNNNNN_name.yaml)", dir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Version < files[j].Version })
	if err := validateRunbookChain(files); err != nil {
		return nil, err
	}
	return files, nil
}

// loadRunbookFile 读取并解析单个迁移文件（文件名已通过规则校验）。
func loadRunbookFile(path string, version int64, name string) (*runbookFile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path 由 --dir 目录扫描拼接
	if err != nil {
		return nil, fmt.Errorf("read runbook %s: %w", filepath.Base(path), err)
	}
	up, down, err := parseRunbookContent(data, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	return &runbookFile{
		Version:  version,
		Name:     name,
		FileName: filepath.Base(path),
		Path:     path,
		Up:       up,
		Down:     down,
		Checksum: runbookChecksum(data),
	}, nil
}

// validateRunbookChain 校验版本链（列表已按版本升序）：从 1 起、连续、无
// 重号——跳号 = 丢文件，重号 = 二义性；错误信息带文件名定位。
func validateRunbookChain(files []runbookFile) error {
	for i := range files {
		f := &files[i]
		want := int64(i + 1)
		if f.Version == want {
			continue
		}
		if i > 0 && f.Version == files[i-1].Version {
			return fmt.Errorf("duplicate runbook version %06d: %s and %s (version numbers must be unique)", f.Version, files[i-1].FileName, f.FileName)
		}
		return fmt.Errorf("broken runbook version chain at %s: expected version %06d, got %06d (versions start at 1 and must be contiguous)", f.FileName, want, f.Version)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 骨架生成（`runbook new`；CLI 命令层只做旗标与输出）
// ---------------------------------------------------------------------------

// RunbookMaxVersion 返回目录内最大序号：只看文件名、不解析内容、不校验链
// （new 只需要下一号）；目录不存在返回 0。
func RunbookMaxVersion(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read runbook directory %s: %w", dir, err)
	}
	var max int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if v, _, ok := parseRunbookFileName(entry.Name()); ok && v > max {
			max = v
		}
	}
	return max, nil
}

// Scaffold 生成下一序号迁移骨架 NNNNNN_name.yaml（O_EXCL 不覆盖已有文件），
// 返回（路径, 版本号）。目录不存在则创建——new 承担引导空目录，D13 的「目录
// 必须有合法文件」只约束 up/down/status 的加载路径。纯本地操作，无 RPC。
func Scaffold(dir, name string) (string, int64, error) {
	if !runbookNameRe.MatchString(name) {
		return "", 0, fmt.Errorf("invalid runbook name %q (lowercase letters, digits and '_', 1-64 chars)", name)
	}
	version, err := RunbookMaxVersion(dir)
	if err != nil {
		return "", 0, err
	}
	if version >= 999999 {
		return "", 0, fmt.Errorf("runbook version exhausted: the sequence is 6-digit and cannot go past 999999")
	}
	version++
	path := filepath.Join(dir, fmt.Sprintf("%06d_%s.yaml", version, name))
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // 用户源文件目录（类 sql migrations），0755 有意
		return "", 0, fmt.Errorf("create runbook directory %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // 用户源文件，0644 有意
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", 0, fmt.Errorf("runbook file already exists: %s", path)
		}
		return "", 0, fmt.Errorf("write runbook %s: %w", path, err)
	}
	if _, err := f.WriteString(runbookSkeleton(version, name)); err != nil {
		_ = f.Close()
		return "", 0, fmt.Errorf("write runbook %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", 0, fmt.Errorf("write runbook %s: %w", path, err)
	}
	return path, version, nil
}

// runbookSkeleton 是 `runbook new` 落盘的骨架：注释给出动作写法模板与
// 「down 段为空 = 不可逆」提示（D4/D17）。骨架必须能通过本文件层的严格
// 解析与引擎层的动词 schema 校验——自己生成的文件过不了自己的校验就是
// 自摆乌龙（create_collection 的集合 ID 字段按 CreateCollectionRequest 的
// proto 字段名写 `id`；delete/update 才是 `collection_id`）。
func runbookSkeleton(version int64, name string) string {
	return fmt.Sprintf(`# Runbook step %06d_%s - scaffolded by "torchwood runbook new"; edit below.
# Naming: NNNNNN_name.yaml; versions start at 1 and must stay contiguous.
# One action = exactly one verb key + its request body; body fields mirror the
# matching *Request proto message (camelCase), e.g. create_collection uses "id"
# (CreateCollectionRequest.id) while delete_collection uses "collection_id".
# Example:
#
#   - create_collection:
#       database_id: app
#       id: configs
#       name: configs
#       document_security: true
#       permissions: ['read:users']
#       attributes:
#         - { key: key, type: string, size: 64, required: true }
#       indexes:
#         - { id: key, type: unique, attributes: [key] }
up: []
down: [] # empty down = this step is IRREVERSIBLE; add reverse actions (e.g. delete_collection) to allow rollback
`, version, name)
}
