package auth

import (
	"testing"

	"github.com/lynx-go/grpcapi/authz"
	"github.com/stretchr/testify/require"
)

// testPolicySet 构造含 databases/storage 读写规则的最小策略集。
func testPolicySet(t *testing.T) *PolicySet {
	t.Helper()
	set, err := authz.NewPolicySet([]MethodPolicy{
		{Method: "/test/DBRead", Access: AccessServer, Scope: &ScopeRule{Resource: string(ScopeDatabases), Op: ScopeRead}},
		{Method: "/test/DBWrite", Access: AccessServer, Scope: &ScopeRule{Resource: string(ScopeDatabases), Op: ScopeWrite}},
		{Method: "/test/StRead", Access: AccessServer, Scope: &ScopeRule{Resource: string(ScopeStorage), Op: ScopeRead}},
		{Method: "/test/StWrite", Access: AccessServer, Scope: &ScopeRule{Resource: string(ScopeStorage), Op: ScopeWrite}},
	}...)
	require.NoError(t, err)
	return set
}

// TestParseScopeToken：语法矩阵——合法/非法形态与字段拆解。
func TestParseScopeToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in     string
		want   scopeToken
		wantOK bool
	}{
		{in: "*", want: scopeToken{}, wantOK: false}, // 通配符不是 token 形态（在 Allows 层短路）
		{in: "databases", want: scopeToken{Resource: ScopeDatabases}, wantOK: true},
		{in: "databases.read", want: scopeToken{Resource: ScopeDatabases, Op: ScopeRead}, wantOK: true},
		{in: "databases.write", want: scopeToken{Resource: ScopeDatabases, Op: ScopeWrite}, wantOK: true},
		{in: "leaderboards.admin", want: scopeToken{Resource: ScopeLeaderboards, Op: ScopeAdmin}, wantOK: true},
		{in: "databases:blog", want: scopeToken{Resource: ScopeDatabases, TargetID: "blog"}, wantOK: true},
		{in: "databases:blog.read", want: scopeToken{Resource: ScopeDatabases, Op: ScopeRead, TargetID: "blog"}, wantOK: true},
		{in: "storage:media.write", want: scopeToken{Resource: ScopeStorage, Op: ScopeWrite, TargetID: "media"}, wantOK: true},
		{in: "storage:My_Bucket-01", want: scopeToken{Resource: ScopeStorage, TargetID: "My_Bucket-01"}, wantOK: true},
		// 非法：未知资源 / 未知方向 / 不可寻址资源带 ':' / 非法实例 ID。
		{in: "nosuch", wantOK: false},
		{in: "databases.read.write", wantOK: false},
		{in: "users:u123", wantOK: false},
		{in: "databases:Blog", wantOK: false},
		{in: "databases:", wantOK: false},
		{in: "databases:a.b", wantOK: false},
		{in: "", wantOK: false},
	}
	for _, tc := range cases {
		got, ok := ParseScopeToken(tc.in)
		if tc.wantOK {
			require.True(t, ok, "ParseScopeToken(%q) 应合法", tc.in)
			require.Equal(t, tc.want, got, "ParseScopeToken(%q)", tc.in)
		} else {
			require.False(t, ok, "ParseScopeToken(%q) 应非法", tc.in)
		}
	}
}

// TestScopeVocabulary_Valid_ScopedForms：词表校验接受可寻址资源的实例限定，
// 拒绝不可寻址资源与未声明方向的形态。
func TestScopeVocabulary_Valid_ScopedForms(t *testing.T) {
	t.Parallel()
	v := VocabularyFromPolicies(testPolicySet(t))

	require.True(t, v.Valid("databases:blog"))
	require.True(t, v.Valid("databases:blog.read"))
	require.True(t, v.Valid("storage:media"))
	require.True(t, v.Valid("storage:media.write"))
	// 既有形态不受影响。
	require.True(t, v.Valid("databases"))
	require.True(t, v.Valid("databases.read"))
	require.True(t, v.Valid("*"))
	// storage 只声明了 read+write；这里都有 → 实例限定皆可。
	// users 未被任何方法引用（词表外）→ 实例限定拒绝。
	require.False(t, v.Valid("users:u1"))
	// 不可寻址资源（词表内若有）也拒绝；目标 ID 非法拒绝。
	require.False(t, v.Valid("databases:Blog"))
	require.False(t, v.Valid("databases:blog.readwrite"))
}

// TestAllowsAPIKeyTargets_Scoped（T-02 验收）：databases:blog 放行寻址 blog
// 的读写；其他库/无目标方法/其他资源 403；.read 单向限定生效。
func TestAllowsAPIKeyTargets_Scoped(t *testing.T) {
	t.Parallel()
	set := testPolicySet(t)
	blog := ScopeTargets{DatabaseID: "blog"}

	// 读写皆放行（blog）。
	require.True(t, AllowsAPIKeyTargets(set, "/test/DBRead", []string{"databases:blog"}, blog))
	require.True(t, AllowsAPIKeyTargets(set, "/test/DBWrite", []string{"databases:blog"}, blog))
	// 其他库 → 拒绝。
	require.False(t, AllowsAPIKeyTargets(set, "/test/DBWrite", []string{"databases:blog"}, ScopeTargets{DatabaseID: "cms"}))
	// 无目标方法（List 类）→ 拒绝。
	require.False(t, AllowsAPIKeyTargets(set, "/test/DBWrite", []string{"databases:blog"}, ScopeTargets{}))
	// .read 单向限定：读放行、写拒绝。
	require.True(t, AllowsAPIKeyTargets(set, "/test/DBRead", []string{"databases:blog.read"}, blog))
	require.False(t, AllowsAPIKeyTargets(set, "/test/DBWrite", []string{"databases:blog.read"}, blog))
	// 跨资源族不混淆：storage scope 不放行 databases 方法。
	require.False(t, AllowsAPIKeyTargets(set, "/test/DBRead", []string{"storage:blog"}, blog))
}

// TestAllowsAPIKeyTargets_BackwardCompat（T-02 回归不变量）：无资源限定的
// 既有 scope 形态行为与目标完全无关。
func TestAllowsAPIKeyTargets_BackwardCompat(t *testing.T) {
	t.Parallel()
	set := testPolicySet(t)
	anyTarget := ScopeTargets{DatabaseID: "whatever", BucketID: "b"}

	require.True(t, AllowsAPIKeyTargets(set, "/test/DBRead", []string{"*"}, anyTarget))
	require.True(t, AllowsAPIKeyTargets(set, "/test/DBWrite", []string{"all"}, anyTarget))
	require.True(t, AllowsAPIKeyTargets(set, "/test/DBRead", []string{"databases"}, anyTarget))
	require.True(t, AllowsAPIKeyTargets(set, "/test/DBWrite", []string{"databases"}, anyTarget))
	require.True(t, AllowsAPIKeyTargets(set, "/test/DBRead", []string{"databases.read"}, anyTarget))
	require.False(t, AllowsAPIKeyTargets(set, "/test/DBWrite", []string{"databases.read"}, anyTarget))
	require.True(t, AllowsAPIKeyTargets(set, "/test/DBWrite", []string{"databases.write"}, anyTarget))
	// 零目标时既有形态同样放行（与旧 AllowsAPIKey 一致）。
	require.True(t, AllowsAPIKeyTargets(set, "/test/DBRead", []string{"databases"}, ScopeTargets{}))
	// 未声明 scope 的方法一律拒绝。
	require.False(t, AllowsAPIKeyTargets(set, "/test/Nope", []string{"*"}, anyTarget))
	// 旧 AllowsAPIKey 与零目标新判定等价。
	require.Equal(t,
		AllowsAPIKeyTargets(set, "/test/DBWrite", []string{"databases.read", "storage"}, ScopeTargets{}),
		AllowsAPIKeyTargets(set, "/test/DBWrite", []string{"databases.read", "storage"}, ScopeTargets{}),
	)
}

// TestAllowsAPIKeyTargets_StorageBucket：storage:media 只放行该桶。
func TestAllowsAPIKeyTargets_StorageBucket(t *testing.T) {
	t.Parallel()
	set := testPolicySet(t)
	media := ScopeTargets{BucketID: "media"}

	require.True(t, AllowsAPIKeyTargets(set, "/test/StWrite", []string{"storage:media"}, media))
	require.False(t, AllowsAPIKeyTargets(set, "/test/StWrite", []string{"storage:media"}, ScopeTargets{BucketID: "other"}))
	require.False(t, AllowsAPIKeyTargets(set, "/test/StWrite", []string{"storage:media"}, ScopeTargets{}))
	require.True(t, AllowsAPIKeyTargets(set, "/test/StRead", []string{"storage:media.read"}, media))
}

// TestValidateScopeTargetID：实例 ID 格式校验（与物理命名规则同源）。
func TestValidateScopeTargetID(t *testing.T) {
	t.Parallel()
	require.NoError(t, ValidateScopeTargetID(ScopeDatabases, "blog"))
	require.NoError(t, ValidateScopeTargetID(ScopeDatabases, "a"))
	require.Error(t, ValidateScopeTargetID(ScopeDatabases, "Blog"))
	require.Error(t, ValidateScopeTargetID(ScopeDatabases, "blog_id"))
	require.Error(t, ValidateScopeTargetID(ScopeDatabases, ""))
	require.Error(t, ValidateScopeTargetID(ScopeDatabases, string(make([]byte, 0))+pad("ab", 30)))
	require.NoError(t, ValidateScopeTargetID(ScopeStorage, "My_Bucket-01"))
	require.Error(t, ValidateScopeTargetID(ScopeStorage, "bad bucket"))
	require.Error(t, ValidateScopeTargetID(ScopeUsers, "x"))
}

// testLeaderboardsPolicySet 构造 leaderboards 三方向的最小策略集。
func testLeaderboardsPolicySet(t *testing.T) *PolicySet {
	t.Helper()
	set, err := authz.NewPolicySet([]MethodPolicy{
		{Method: "/test/LBRead", Access: AccessServer, Scope: &ScopeRule{Resource: string(ScopeLeaderboards), Op: ScopeRead}},
		{Method: "/test/LBWrite", Access: AccessServer, Scope: &ScopeRule{Resource: string(ScopeLeaderboards), Op: ScopeWrite}},
		{Method: "/test/LBAdmin", Access: AccessServer, Scope: &ScopeRule{Resource: string(ScopeLeaderboards), Op: ScopeAdmin}},
	}...)
	require.NoError(t, err)
	return set
}

// TestScopeAdminOp（2026-09-13 leaderboards.admin 裁决的匹配语义）：admin 是
// 配置面方向——提交钥（write）不得命中 admin 门方法，admin 钥也不得命中
// write/read 门方法（各方向独立授予）；裸资源与通配符仍放行全方向（既有
// 行为不变量）。
func TestScopeAdminOp(t *testing.T) {
	t.Parallel()
	set := testLeaderboardsPolicySet(t)

	// admin 门：admin 钥/裸资源/通配符放行；write、read 钥拒绝。
	require.True(t, AllowsAPIKeyTargets(set, "/test/LBAdmin", []string{"leaderboards.admin"}, ScopeTargets{}))
	require.True(t, AllowsAPIKeyTargets(set, "/test/LBAdmin", []string{"leaderboards"}, ScopeTargets{}))
	require.True(t, AllowsAPIKeyTargets(set, "/test/LBAdmin", []string{"all"}, ScopeTargets{}))
	require.False(t, AllowsAPIKeyTargets(set, "/test/LBAdmin", []string{"leaderboards.write"}, ScopeTargets{}))
	require.False(t, AllowsAPIKeyTargets(set, "/test/LBAdmin", []string{"leaderboards.read"}, ScopeTargets{}))
	// write 门：admin 钥拒绝（管理权不放大提交权）。
	require.True(t, AllowsAPIKeyTargets(set, "/test/LBWrite", []string{"leaderboards.write"}, ScopeTargets{}))
	require.False(t, AllowsAPIKeyTargets(set, "/test/LBWrite", []string{"leaderboards.admin"}, ScopeTargets{}))
	// read 门：admin 钥拒绝（读单独授予——管理钥如需读配置须并列携带 read）。
	require.True(t, AllowsAPIKeyTargets(set, "/test/LBRead", []string{"leaderboards.read"}, ScopeTargets{}))
	require.False(t, AllowsAPIKeyTargets(set, "/test/LBRead", []string{"leaderboards.admin"}, ScopeTargets{}))
}

// TestScopeAdmin_Vocabulary：词表派生自动收录 admin 方向；不可寻址资源
// 的实例限定形态仍拒绝。
func TestScopeAdmin_Vocabulary(t *testing.T) {
	t.Parallel()
	set := testLeaderboardsPolicySet(t)
	v := VocabularyFromPolicies(set)

	require.True(t, v.Valid("leaderboards.admin"))
	require.True(t, v.HasOp(ScopeLeaderboards, ScopeAdmin))
	require.False(t, v.Valid("leaderboards.readwrite"))
	require.False(t, v.Valid("leaderboards:b1.admin"))
}

func pad(s string, n int) string {
	for len(s) < n {
		s += "x"
	}
	return s
}
