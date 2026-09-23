package auth

import (
	"strings"
	"testing"

	"github.com/lynx-go/grpcapi/authz"
	"github.com/stretchr/testify/require"
)

// TestParseCustomScope 语法门逐格验证（T2：scope := <service>.<name>，
// service := [a-z][a-z0-9-]{1,31}，name := [a-z0-9_.-]{1,40}，总长 ≤64，
// 全小写，无空段，内建域保护）。
func TestParseCustomScope(t *testing.T) {
	longService := strings.Repeat("a", 32)                // service 段最长合法
	overService := strings.Repeat("a", 33)                // service 段超长
	longName := strings.Repeat("n", 40)                   // name 段最长合法
	overName := strings.Repeat("n", 41)                   // name 段超长
	fit64 := longService + "." + strings.Repeat("n", 31)  // 恰 32+1+31 = 64
	over64 := longService + "." + strings.Repeat("n", 32) // 65：分段合法但总长超限

	valid := map[string][2]string{
		// 常规形态。
		"messageloop.session.act":     {"messageloop", "session.act"},
		"mlbridge.history.read":       {"mlbridge", "history.read"},
		"ab.c":                        {"ab", "c"},
		"a-b.c-d":                     {"a-b", "c-d"},
		"svc.name_with.dot-and_123":   {"svc", "name_with.dot-and_123"},
		fit64:                         {longService, strings.Repeat("n", 31)},
		"analytics-team.reports.read": {"analytics-team", "reports.read"},
	}
	for in, want := range valid {
		svc, name, ok := ParseCustomScope(in)
		require.True(t, ok, "ParseCustomScope(%q) 应合法", in)
		require.Equal(t, want[0], svc, "ParseCustomScope(%q) service", in)
		require.Equal(t, want[1], name, "ParseCustomScope(%q) name", in)
	}

	invalid := []string{
		"",                           // 空
		"noservice",                  // 缺 service 前缀（无点）
		".lead",                      // 空 service
		"svc.",                       // 空 name（尾点）
		"svc..x",                     // 双点（空段）
		"svc.x..y",                   // 中部双点
		"svc..",                      // 尾部双点
		".svc.x",                     // 前导点
		"Messageloop.x",              // 大写 service
		"svc.Name",                   // 大写 name
		"svc.NAME",                   // 全大写 name
		"1svc.x",                     // service 数字开头
		"-svc.x",                     // service 连字符开头
		"a.x",                        // service 单字符（≥2）
		overService + ".x",           // service 超长（33）
		"svc." + overName,            // name 超长（41）
		longService + "." + longName, // 分段各自合法但总长 73 > 64
		over64,                       // 总长 65
		"svc.x$y",                    // 非法字符 $
		"svc.x y",                    // 空格
		"svc.x/y",                    // 斜杠
		"my_svc.x",                   // service 下划线非法
		"svc:x.y",                    // 冒号（实例限定语法专用）
		// 内建保护：service 段命中内建资源词表 / 保留别名。
		"users.anything",
		"databases.custom",
		"storage.x",
		"projects.x",
		"functions.x",
		"audit_logs.x", // 词表形态含下划线，service 语法本就非法，双保险
		"all.x",        // 保留别名
	}
	for _, in := range invalid {
		_, _, ok := ParseCustomScope(in)
		require.False(t, ok, "ParseCustomScope(%q) 应非法", in)
	}
}

// testVocabAllResources 构造覆盖全部资源词表（读+写+admin 方向）的词表，
// 供 Valid 交互断言（内建形态不受自定义语法影响）。
func testVocabAllResources(t *testing.T) *ScopeVocabulary {
	t.Helper()
	pols := make([]MethodPolicy, 0, len(AllScopeResources)*3)
	for _, res := range AllScopeResources {
		for _, op := range []ScopeOp{ScopeRead, ScopeWrite, ScopeAdmin} {
			pols = append(pols, MethodPolicy{
				Method: "/t/" + string(res) + "." + string(op), Service: "/t",
				Access: AccessServer,
				Scope:  &ScopeRule{Resource: string(res), Op: op},
			})
		}
	}
	set, err := authz.NewPolicySet(pols...)
	require.NoError(t, err)
	return VocabularyFromPolicies(set)
}

// TestScopeVocabularyValid_CustomInterplay：Valid 三类形态互不干扰——
// 内建精确/实例限定行为不变（回归），自定义形态按语法接受，内建域串拒绝。
func TestScopeVocabularyValid_CustomInterplay(t *testing.T) {
	v := testVocabAllResources(t)

	accepted := []string{
		// 内建精确形态（回归）。
		"*", "all", "users", "users.read", "databases.write", "leaderboards.admin",
		// 内建实例限定形态（回归）。
		"databases:blog", "databases:blog.read", "storage:bkt_01.write",
		// 自定义形态。
		"messageloop.session.act",
		"mlbridge.history.read",
	}
	for _, s := range accepted {
		require.True(t, v.Valid(s), "Valid(%q) 应接受", s)
	}

	rejected := []string{
		// 内建词表外的伪资源（回归：既非内建也非合法自定义——service 撞
		// 词表内建域或语法非法）。
		"databases.delete", // 内建 service + 非法方向
		"databases.read.x", // 内建 service + 多段（service=databases 命中内建域）
		"users.anything",   // 内建域不可占用
		"databases:Blog",   // 实例 ID 非法
		"nosuchsvc:blog",   // 非法实例限定（nosuchsvc:blog 非自定义语法，冒号不合法）
		// 自定义语法非法。
		"", "Messageloop.x", "svc..x", "a.x", "svc.", ".svc.x",
		strings.Repeat("a", 65), // 总长 65
	}
	for _, s := range rejected {
		require.False(t, v.Valid(s), "Valid(%q) 应拒绝", s)
	}
}

// TestCustomScope_NeverGrantsTWAccess 锁定"自定义 scope 对 TW 方法永不匹配"
// （fail-closed）：任意语法合法的自定义串经 AllowsAPIKeyTargets 一律拒绝，
// mlbridge 等外部标签不得意外获得 TW 权限。
func TestCustomScope_NeverGrantsTWAccess(t *testing.T) {
	set, err := authz.NewPolicySet([]MethodPolicy{{
		Method: "/torchwood.server.v1.UsersService/ListUsers", Service: "/torchwood.server.v1.UsersService",
		Access: AccessServer,
		Scope:  &ScopeRule{Resource: string(ScopeUsers), Op: ScopeRead},
	}}...)
	require.NoError(t, err)
	for _, s := range []string{
		"messageloop.session.act",
		"mlbridge.users.read", // 即使 name 段伪装成内建形态也不放行
		"messageloop.all",     // 无点 → 语法非法，更不放行
		"x.users",             // service 单字符 → 语法非法
	} {
		require.False(t, AllowsAPIKeyTargets(set, "/torchwood.server.v1.UsersService/ListUsers", []string{s}, ScopeTargets{}),
			"自定义 scope %q 不得匹配 TW 方法", s)
	}
	// 内建对照：users.read 照常放行（匹配语义不受影响）。
	require.True(t, AllowsAPIKeyTargets(set, "/torchwood.server.v1.UsersService/ListUsers", []string{"users.read"}, ScopeTargets{}))
}
