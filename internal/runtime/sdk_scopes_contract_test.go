package runtime

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
)

// TestSDKScopesContract_AlignedWithVocabulary internal 侧契约锁：解析
// sdk/go/server/scopes.go 的 const 块（源码文本断言，SDK 是独立 Go module
// 不 import internal），断言其常量值集合与 domainauth.AllScopeResources
// 派生的词表（全部资源 × {裸名, .read, .write} ∪ {*, all}）完全对齐——
// 资源清单演进而未重新生成 SDK 常量时此处变红。
func TestSDKScopesContract_AlignedWithVocabulary(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(filepath.Join("..", "..", "sdk", "go", "server", "scopes.go"))
	require.NoError(t, err, "sdk/go/server/scopes.go 不存在")

	// 提取 const 块（首个 const ( ... ) 段），跳过注释行。
	block := sdkScopesConstBlock(t, string(src))
	lineRe := regexp.MustCompile(`^\s*(Scope\w+)\s*=\s*"([^"]+)"`)
	got := map[string]string{} // value → const 名
	for _, line := range strings.Split(block, "\n") {
		m := lineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		require.False(t, strings.HasPrefix(strings.TrimSpace(line), "//"), "注释行误解析: %s", line)
		prev, dup := got[m[2]]
		require.False(t, dup, "scope 值 %q 重复（%s 与 %s）", m[2], prev, m[1])
		got[m[2]] = m[1]
	}
	require.NotEmpty(t, got, "const 块解析为空：scopes.go 结构变更后需同步本测试")

	// 期望值集合：全部资源 × {裸名, .read, .write} ∪ {*, all}。
	want := map[string]bool{"*": true, "all": true}
	for _, r := range domainauth.AllScopeResources {
		want[string(r)] = true
		want[string(r)+"."+string(domainauth.ScopeRead)] = true
		want[string(r)+"."+string(domainauth.ScopeWrite)] = true
	}

	require.Len(t, got, len(want), "SDK scope 常量数量与词表不符（got=%d want=%d）", len(got), len(want))
	var missing, extra []string
	for v := range want {
		if _, ok := got[v]; !ok {
			missing = append(missing, v)
		}
	}
	for v := range got {
		if !want[v] {
			extra = append(extra, v+" (const "+got[v]+")")
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	require.Empty(t, missing, "SDK 缺少词表 scope 常量: %v（请重新生成 scopes.go）", missing)
	require.Empty(t, extra, "SDK 存在词表外 scope 常量（死 scope 残留）: %v", extra)
}

// sdkScopesConstBlock 提取首个 `const (` ... `)` 块文本。
func sdkScopesConstBlock(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "const (")
	require.GreaterOrEqual(t, start, 0, "scopes.go 缺少 const 块")
	end := strings.Index(src[start:], "\n)")
	require.GreaterOrEqual(t, end, 0, "const 块未闭合")
	return src[start : start+end]
}
