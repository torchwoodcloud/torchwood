// S-1（数据保真）：PG 数组字面量带引号元素内 `\` 是转义符，必须先双写
// （`\` → `\\`）再做引号翻倍（`"` → `""`）——顺序勿反。覆盖 pgTextArray
// 单元形态与端到端往返（写入/containsAny 匹配/append）。
package documentdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

// TestPgTextArray_Escaping（S-1 单元）：反斜杠双写、引号以 `\"` 转义；
// 混合值两个规则叠加。字面量形态以 PG 数组输入解析实证为准（PG 16 psql：
// `{"a""c"}` → 22P02；`{"a\"c"}` → `a"c`；`{"a\\b\"c"}` → `a\b"c`）。
func TestPgTextArray_Escaping(t *testing.T) {
	t.Parallel()
	require.Equal(t, `{"a\\b"}`, pgTextArray([]string{`a\b`}), "反斜杠必须双写（带引号元素内 \\ 是转义符）")
	require.Equal(t, `{"c\"d"}`, pgTextArray([]string{`c"d`}), "引号以反斜杠转义（PG 数组不支持 \"\" 双写）")
	require.Equal(t, `{}`, pgTextArray(nil))
	require.Equal(t, `{"x"}`, pgTextArray([]string{"x"}))
	// 混合：`a\b"c` → 双写反斜杠 + 反斜杠转义引号。
	require.Equal(t, `{"a\\b\"c"}`, pgTextArray([]string{`a\b"c`}))
	// 尾反斜杠与连续反斜杠。
	require.Equal(t, `{"tail\\"}`, pgTextArray([]string{`tail\`}))
	require.Equal(t, `{"two\\\\x"}`, pgTextArray([]string{`two\\x`}))
}

// TestArrayColumns_BackslashQuoteFidelity（S-1 端到端）：含 `\` / `"` /
// 混合值的数组经 Create 往返必须保真（修复前 `\` 被 PG 去转义——数据失真）；
// containsAny 对含 `\` 值正常匹配；append 含 `\` 值不失真。
func TestArrayColumns_BackslashQuoteFidelity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupArrayCollection(ctx, t)
	principal := databases.Principal{Roles: []string{"any"}}

	tags := []any{`C:\Users\tw`, `say "hi"`, `a\b"c`, `tail\`}
	created, err := docDB.CreateDocument(ctx, projectID, "app", "items", databases.Document{
		ID:   "s1",
		Data: map[string]any{"title": "s1", "tags": tags},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)

	// 写读往返保真（含 Get 与 Update 返回两条路径）。
	got, err := docDB.GetDocument(ctx, projectID, "app", "items", created.ID, principal)
	require.NoError(t, err)
	require.Equal(t, tags, got.Data["tags"], "含 \\ / \" 的数组值必须原样往返")

	// containsAny 对含 `\` 值匹配（修复前两侧同错相抵不可靠——以保真为前提）。
	list, err := docDB.ListDocuments(ctx, projectID, "app", "items",
		databases.Query{AST: &query.Query{Filter: query.ContainsAny("tags", `C:\Users\tw`)}}, principal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1, "containsAny 必须命中含 \\ 的存储值")
	require.Equal(t, "s1", list.Documents[0].ID)

	// 引号值同样可查。
	list, err = docDB.ListDocuments(ctx, projectID, "app", "items",
		databases.Query{AST: &query.Query{Filter: query.ContainsAny("tags", `say "hi"`)}}, principal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)

	// append 含 `\` 值：追加后整体仍保真。
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "items", databases.DocumentUpdate{
		Document:        databases.Document{ID: "s1"},
		ArrayUpdates:    map[string]databases.ArrayUpdate{"tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{`new\path`}}},
		ExpectedVersion: got.Version,
	}, principal)
	require.NoError(t, err)
	require.Equal(t, append(tags, `new\path`), updated.Data["tags"])
}
