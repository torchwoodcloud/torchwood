package documentdb

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/pkg/ident"
)

// ErrDuplicateKey re-exports the domain duplicate-key error.
var ErrDuplicateKey = databases.ErrDuplicateKey

// ErrPermissionDenied re-exports the domain permission error.
var ErrPermissionDenied = databases.ErrPermissionDenied

var safeNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
var docIDRe = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,64}$`)

// systemCollectionsWriteProtected 是禁止非系统主体直接写入的系统集合（纵深防御，
// 正常情况下客户端 API 已在用例层拦截）。仅对项目数据面 sentinel 生效，
// 业务库（含 default）中的同名集合不受影响。
var systemCollectionsWriteProtected = map[string]struct{}{
	"users":      {},
	"sessions":   {},
	"identities": {},
}

func isWriteProtectedSystemCollection(databaseID, collectionID string) bool {
	if databaseID != ident.ProjectDataPlaneID {
		return false
	}
	_, ok := systemCollectionsWriteProtected[collectionID]
	return ok
}

const maxQueryLimit = 100

// A2 输入上限：equal/in 多值个数（DSL 串长度/条数上限随双栈退役移除——
// AST 叶数由 pkg/query.MaxQueries 封顶）。
const maxFilterValues = 1000

// maxTotalFilterParams 是跨 filter 绑定参数的累计上限（buildAppwriteQuery 出口
// 检查），封死 100 条 query × 1000 值 = 10 万参数超出 PG 65535 语句参数上限的风险。
const maxTotalFilterParams = 2000

// maxEfSearch 是 vector_search 查询级 ef_search 的防滥用上限（B7；pgvector
// 缺省 40，pgvector 文档调参建议量级 ≤ 数百）。下限 1 由 validateVectorSearch
// 校验（pgvector 要求 ≥1）。
const maxEfSearch = 500

// maxIdentifierBytes 对齐 PG NAMEDATALEN-1：超长标识符被 PG 静默截断，两个仅
// 超长部分不同的名字会映射到同一物理对象。app 层已按 collectionID ≤40 /
// attr key ≤63 / 索引 ID ≤40 校验；此处是直调 adapter 的第二道防线。
const maxIdentifierBytes = 63

// validatePhysicalNameLen 拒绝超长物理标识符（表名/列名）。第二道防线，见
// maxIdentifierBytes。
func validatePhysicalNameLen(kind, name string) error {
	if len(name) > maxIdentifierBytes {
		return status.Errorf(codes.InvalidArgument, "%s %q exceeds %d-byte identifier limit", kind, name, maxIdentifierBytes)
	}
	return nil
}

// validateIndexNameLen 拒绝拼接超长的物理索引名 idx_<coll>_<suffix>（suffix 为
// 索引 ID 或固定后缀 tenant_created）。静态段上限封不死组合长度，必须在有
// collectionID 上下文的 DDL 入口叠加本校验。
func validateIndexNameLen(collectionID, suffix string) error {
	if n := 4 + len(collectionID) + 1 + len(suffix); n > maxIdentifierBytes {
		return status.Errorf(codes.InvalidArgument,
			"index name idx_%s_%s is %d bytes, exceeds %d-byte identifier limit", collectionID, suffix, n, maxIdentifierBytes)
	}
	return nil
}

type postgresDocumentDB struct {
	db  *clients.Database
	pub shared.EventPublisher // nil 视为 nop（单测）；写路径同事务写入 outbox

	// in-process caches keyed by projectID; safe for concurrent use.
	internalIDCache sync.Map // projectID -> int64
	// versionColumns 记录已确认 _version 为 bigint 的 "schema.collection" 键，
	// 避免每次写/读重复查 information_schema；**只缓存已提交的列**。
	versionColumns sync.Map // "schema.collection" -> struct{}
	// versionAlterTx 记录"本事务内新建 _version 列"的键（txid|schema.collection）。
	// CREATE TABLE 带列或 ALTER ADD 都会打标，避免 versionColumnReady 把未提交列写入 cache。
	// DDL / catalog reconcile 才写此标记，不在文档写热路径。
	versionAlterTx sync.Map // "txid|schema.collection" -> struct{}
	// vectorColumns 缓存集合的 vector 列清单（"schema.physical" -> attr key
	// -> dims，会话 #10）。写路径的字面量编码/维度校验与读回投影覆盖都消费；
	// DDL 路径维护，miss 时点查 catalog attrs。
	vectorColumns sync.Map // "schema.physical" -> map[string]int
	// physicalNameCache 缓存业务库集合的逻辑 ID → 物理表名（B13c，转出 POC：
	// 阶段②预决策 4 挂账落地——点查占业务查询往返占比实测 ~26% ≥ 5% 判据）。
	// 失效面 = catalog_collections 的全部删除路径（DeleteCollection /
	// DeleteDatabase / import 清位）；CreateCollection 写穿覆盖（同名逻辑 ID
	// 重建必得新物理名）。跨实例陈旧语义 fail-loud：他实例删建后陈旧名指向
	// 已 DROP 的表 → PG 42P01 显式报错，无静默错写。
	physicalNameCache sync.Map // "project\x1fdatabase\x1fcollection" -> physical string
}

func NewPostgresDocumentDB(db *clients.Database, pub shared.EventPublisher) databases.DocumentDB {
	return &postgresDocumentDB{db: db, pub: pub}
}

var _ databases.DocumentDB = (*postgresDocumentDB)(nil)

func (p *postgresDocumentDB) conn(ctx context.Context) bun.IDB {
	return p.db.Conn(ctx)
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func mapIdentError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ident.ErrInvalidSchemaResourceID) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return err
}

// pgTextArray 把字符串列表编码为 PG 数组字面量（配 "?::text[]" 绑定）。
// S-1（数据保真）：PG 数组字面量的带引号元素内 `\` 是转义符——必须双写
// （`\` → `\\`），引号以反斜杠转义（`"` → `\"`）。**先双写 `\` 再转义 `"`**
// （顺序勿反：`\"` 的 `\` 不能再被双写）。实证（PG 16 psql）：`{"a""c"}` →
// 22P02 "Incorrectly quoted array element"（`""` 双写是 CSV 规则、PG 数组
// 不支持）；`{"a\"c"}` → `a"c`；裸 `\` 形态 `{"C:\Users\tw"}` → `\t` 被解为
// tab、`\U` 被去转义（数据失真）。读回走 to_jsonb（PG 服务端解码）无需
// 对称处理。
func pgTextArray(items []string) string {
	var parts []string
	for _, item := range items {
		item = strings.ReplaceAll(item, `\`, `\\`)
		item = strings.ReplaceAll(item, `"`, `\"`)
		parts = append(parts, `"`+item+`"`)
	}
	return `{` + strings.Join(parts, ",") + `}`
}

// pgArrayLiteral 把 data 通道（JSON 反序列化产物）中的数组值编码为 PG 数组
// 字面量（阶段③-b）：元素引用与 pgTextArray 同源——PG 数组字面量解析先剥引号
// 再按目标列元素类型转换，`{"1","2"}` 对 bigint[] 同样合法。标量类型（nil/
// bool/int64/float64/string）按各自文本形态渲染。返回 false 表示 v 不是数组
// （按原值绑定，PG 按目标列推断）。
func pgArrayLiteral(v any) (string, bool) {
	switch vv := v.(type) {
	case []string:
		return pgTextArray(vv), true
	case []any:
		parts := make([]string, 0, len(vv))
		for _, e := range vv {
			switch ev := e.(type) {
			case string:
				// S-1：与 pgTextArray 同转义序——先双写 `\` 再以 `\"` 转义引号。
				ev = strings.ReplaceAll(ev, `\`, `\\`)
				ev = strings.ReplaceAll(ev, `"`, `\"`)
				parts = append(parts, `"`+ev+`"`)
			case bool:
				if ev {
					parts = append(parts, "t")
				} else {
					parts = append(parts, "f")
				}
			case int64:
				parts = append(parts, strconv.FormatInt(ev, 10))
			case int:
				parts = append(parts, strconv.Itoa(ev))
			case float64:
				parts = append(parts, strconv.FormatFloat(ev, 'f', -1, 64))
			case nil:
				parts = append(parts, "NULL")
			default:
				return "", false
			}
		}
		return `{` + strings.Join(parts, ",") + `}`, true
	default:
		return "", false
	}
}
