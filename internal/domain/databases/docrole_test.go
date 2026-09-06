package databases

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/groups"
)

// TestDocRole_ConstructorParseRoundTrip：构造器产物必须能被解析回同一
// (kind, id, extra)，且解析结果可重建同一字符串（词表主权的基本闭环）。
func TestDocRole_ConstructorParseRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role  string
		kind  string
		id    string
		extra string
	}{
		{RoleUsers, RoleKindUsers, "", ""},
		{RoleAny, RoleKindAny, "", ""},
		{RoleGuests, RoleKindGuests, "", ""},
		{RolePrivate, RoleKindPrivate, "", ""},
		{RoleSystem, RoleKindSystem, "", ""},
		{RoleUser("u-1"), RoleKindUser, "u-1", ""},
		{RoleUserVerified("u-1"), RoleKindUser, "u-1", RoleVerifiedTag},
		{RoleGroup("g-1"), RoleKindGroup, "g-1", ""},
		{RoleGroupRole("g-1", groups.RoleOwner), RoleKindGroup, "g-1", groups.RoleOwner},
		{RoleGroupRole("g-1", groups.RoleMember), RoleKindGroup, "g-1", groups.RoleMember},
		{RoleKey("k-1"), RoleKindKey, "k-1", ""},
		{RoleMembership("m-1"), RoleKindMember, "m-1", ""},
		{RoleLabel("beta"), RoleKindLabel, "beta", ""},
	}
	for _, tc := range cases {
		kind, id, extra, ok := ParseDocRole(tc.role)
		require.True(t, ok, "ParseDocRole(%q) 应在词表内", tc.role)
		require.Equal(t, tc.kind, kind, "ParseDocRole(%q) kind", tc.role)
		require.Equal(t, tc.id, id, "ParseDocRole(%q) id", tc.role)
		require.Equal(t, tc.extra, extra, "ParseDocRole(%q) extra", tc.role)
		require.True(t, IsNamespacedRole(tc.role) == isNamespacedKind(kind),
			"IsNamespacedRole(%q) 应与 kind 一致", tc.role)
	}

	// 模板占位符可被前缀识别（ExpandPermissionTemplates 依赖该形态）。
	require.True(t, IsNamespacedRole(RoleUserTemplate))
	require.True(t, IsNamespacedRole(RoleGroupTemplate))
	kind, _, _, ok := ParseDocRole(RoleUserTemplate)
	require.True(t, ok)
	require.Equal(t, RoleKindUser, kind)
	kind, _, _, ok = ParseDocRole(RoleGroupTemplate)
	require.True(t, ok)
	require.Equal(t, RoleKindGroup, kind)
}

func isNamespacedKind(kind string) bool {
	switch kind {
	case RoleKindUser, RoleKindGroup, RoleKindKey, RoleKindMember, RoleKindLabel:
		return true
	default:
		return false
	}
}

// TestDocRole_RejectsOutOfVocabulary：裸串（组域裸角色、console 角色）与
// 病态形态不在词表内。
func TestDocRole_RejectsOutOfVocabulary(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"", "owner", "admin", "member", "viewer", "console",
		"user:", "group:", "key:", "member:", "label:",
		"user:/verified",              // 空 id
		"user:u-1/other",              // user 域仅认 verified 后缀
		"group:g-1/",                  // 空 group role
		"users/", "any:x", "__root__", // 撞形探测
	} {
		_, _, _, ok := ParseDocRole(s)
		require.False(t, ok, "ParseDocRole(%q) 应拒绝", s)
		require.False(t, IsNamespacedRole(s), "IsNamespacedRole(%q) 应拒绝", s)
	}
}

// TestDocRole_GroupRolesOnlyCompositeFormat（机制 v8 B1 不变量）：groups 域的
// RoleOwner/Admin/Member 是组域职务词表（membership.roles），不是文档角色。
// 它们不等于任何裸 DocRole 词表项；IsGroupRoleFormat 对其一律 false——组职务
// 只有以 group:<gid>/<role> 复合格式才可进入文档角色集（M6.3 投影互斥）。
func TestDocRole_GroupRolesOnlyCompositeFormat(t *testing.T) {
	t.Parallel()
	bareVocab := []string{RoleUsers, RoleAny, RoleGuests, RoleKeys, RolePrivate, RoleSystem}
	for _, groupRole := range []string{groups.RoleOwner, groups.RoleAdmin, groups.RoleMember} {
		for _, v := range bareVocab {
			require.NotEqual(t, v, groupRole,
				"组域裸角色 %q 不得与 DocRole 裸词表项撞形", groupRole)
		}
		_, _, _, ok := ParseDocRole(groupRole)
		require.False(t, ok, "组域裸角色 %q 不在 DocRole 词表内", groupRole)
		require.False(t, IsGroupRoleFormat(groupRole),
			"组域裸角色 %q 不是 group:<gid>/<role> 复合格式", groupRole)
		require.False(t, IsNamespacedRole(groupRole),
			"组域裸角色 %q 不是命名空间角色", groupRole)
	}

	// 组职务只有经 RoleGroupRole 复合后才满足组角色文档格式。
	require.True(t, IsGroupRoleFormat(RoleGroupRole("g-1", groups.RoleOwner)))
	require.True(t, IsGroupRoleFormat(RoleGroupRole("g-1", groups.RoleAdmin)))
	require.True(t, IsGroupRoleFormat(RoleGroupRole("g-1", groups.RoleMember)))
	// group:<gid>（组成员身份，无职务段）不是组职务格式。
	require.False(t, IsGroupRoleFormat(RoleGroup("g-1")))
}

// TestDocRole_NoBareStringLiterals（M6.1 收编守门）：对已收编的生产文件做
// 源码扫描，禁止 DocRole 裸串形态（构造/比较/字面量），词表唯一出口是
// docrole.go 常量与构造器。注释行豁免；扫描范围保守限定在本批次收编文件，
// 避免全仓误报。
func TestDocRole_NoBareStringLiterals(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	scanned := []string{
		filepath.Join(root, "internal", "app", "client", "user_roles.go"),
		filepath.Join(root, "internal", "infra", "auth", "validator.go"),
		filepath.Join(root, "internal", "app", "storage", "storage.go"),
		filepath.Join(root, "internal", "app", "storage", "uploads.go"),
		filepath.Join(root, "internal", "domain", "databases", "permissions.go"),
		filepath.Join(root, "internal", "domain", "databases", "access.go"),
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`Role:\s*"`),                            // Permission 字面量裸角色
		regexp.MustCompile(`"(user|group|key|member|label):`),      // 裸命名空间前缀（含 Sprintf 拼接）
		regexp.MustCompile(`HasRole\("(users|keys|guests|any)"\)`), // 裸词表 HasRole
		regexp.MustCompile(`[!=]= "(users|keys|guests|any)"`),      // 裸词表等值比较
	}
	for _, file := range scanned {
		data, err := os.ReadFile(file)
		require.NoError(t, err, "读取 %s", file)
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // 注释豁免
			}
			for _, p := range patterns {
				if p.MatchString(line) {
					t.Errorf("%s:%d: 命中 DocRole 裸串形态 /%s/，应改用 docrole.go 词表常量或构造器: %s",
						file, i+1, p.String(), trimmed)
				}
			}
		}
	}
}

// repoRoot 从测试工作目录向上定位仓库根（go.mod 所在目录），避免依赖
// go test 的 cwd 约定。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到仓库根（go.mod）")
		}
		dir = parent
	}
}
