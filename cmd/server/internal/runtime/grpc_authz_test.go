package runtime

import (
	"testing"

	"github.com/lynx-go/grpcapi/authz"
	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// TestAssertSemantic_RealProtoRegistry：以真实 proto registry（与
// ProvideMethodPolicies 同源）复现启动期语义断言——完备性/死 scope/档位/
// client·console 值域/项目寻址白名单/streaming。策略违例在测试期直接变红，
// 而不是等 main 启动失败。
func TestAssertSemantic_RealProtoRegistry(t *testing.T) {
	t.Parallel()

	set, err := ProvideMethodPolicies()
	require.NoError(t, err)
	require.NotEmpty(t, set.Methods())

	// 抽查裁决决策已落地（锚点子集；完整锚点矩阵由 Phase B 生成）。
	checks := []struct {
		method string
		verify func(domainauth.MethodPolicy) bool
		desc   string
	}{
		{"/torchwood.server.v1.UsersService/UpdateUserPassword", func(p domainauth.MethodPolicy) bool {
			return p.Access == domainauth.AccessServer && p.Scope != nil &&
				string(p.Scope.Resource) == "users" && p.Scope.Op == domainauth.ScopeWrite &&
				len(p.AdminRoles) == 3
		}, "users 六写方法归一业务写档"},
		{"/torchwood.server.v1.ProjectsService/CreateProject", func(p domainauth.MethodPolicy) bool {
			return p.Access == domainauth.AccessPermission
		}, "CreateProject 为 PERMISSION 平台专属"},
		{"/torchwood.server.v1.APIKeysService/CreateAPIKey", func(p domainauth.MethodPolicy) bool {
			return p.Access == domainauth.AccessPermission && p.Scope == nil
		}, "APIKeys 全服务 PERMISSION（无 key 通道）"},
		{"/torchwood.server.v1.OutboxService/ListDeadLetters", func(p domainauth.MethodPolicy) bool {
			return p.Access == domainauth.AccessServer && len(p.AdminRoles) == 2 && p.Scope != nil && p.Scope.Op == domainauth.ScopeRead
		}, "死信读面限 owner/admin + outbox.read"},
	}
	for _, c := range checks {
		p, ok := set.Get(c.method)
		require.True(t, ok, "%s 必须在策略注册表", c.method)
		require.True(t, c.verify(p), "%s：%s（实际 Access=%v AdminRoles=%v Scope=%v）", c.method, c.desc, p.Access, p.AdminRoles, p.Scope)
	}

	// apikeys/economy 资源已退役；assets 在词表。T2 起 <service>.<name>
	// 自定义语法会有意放行 "apikeys.write" 这类串（作为不透明标签存储、
	// 供外部系统消费），因此"资源退役"的安全不变量断言在词表面：内建资源
	// 词表不再含 apikeys/economy → 任何 TW 方法都不可能被这些串匹配
	// （匹配面 fail-closed 由 TestCustomScope_NeverGrantsTWAccess 锁定）。
	vocab := domainauth.VocabularyFromPolicies(set)
	require.NotContains(t, vocab.Resources(), domainauth.ScopeResource("apikeys"), "apikeys scope 资源应退役")
	require.NotContains(t, vocab.Resources(), domainauth.ScopeResource("economy"), "economy 应更名 assets")
	require.True(t, vocab.Valid("assets.read"), "assets 词表项存在")
	require.False(t, setAllowsAnyMethod(set, "apikeys.write"), "退役资源串不得匹配任何 TW 方法")
	require.False(t, setAllowsAnyMethod(set, "economy.read"), "退役资源串不得匹配任何 TW 方法")
}

// setAllowsAnyMethod 报告 scope 串能否匹配注册表内任一 SERVER 面方法
// （退役资源串的匹配面断言）。
func setAllowsAnyMethod(set *domainauth.PolicySet, scope string) bool {
	for _, p := range set.Methods() {
		if domainauth.AllowsAPIKeyTargets(set, p.Method, []string{scope}, domainauth.ScopeTargets{}) {
			return true
		}
	}
	return false
}

// TestAssertSemantic_DetectsViolations：构造违例策略，断言语义断言真的会
// 抓漏（防上一测试空洞通过）。
func TestAssertSemantic_DetectsViolations(t *testing.T) {
	t.Parallel()

	base := func(mutate func(*domainauth.MethodPolicy)) []domainauth.MethodPolicy {
		p := domainauth.MethodPolicy{
			Method:     "/torchwood.server.v1.TestService/DoThing",
			Service:    "/torchwood.server.v1.TestService",
			Access:     domainauth.AccessServer,
			AdminRoles: []string{string(domainauth.AdminRoleMember), string(domainauth.AdminRoleAdmin), string(domainauth.AdminRoleOwner)},
			Scope:      &domainauth.ScopeRule{Resource: string(domainauth.ScopeUsers), Op: domainauth.ScopeWrite},
		}
		if mutate != nil {
			mutate(&p)
		}
		return []domainauth.MethodPolicy{p}
	}

	_, err := authz.NewPolicySet(base(nil)...)
	require.NoError(t, err)
	require.NoError(t, domainauth.AssertPolicy(base(nil)[0]), "合法业务写档应通过（单方法断言）")

	for name, mutate := range map[string]func(*domainauth.MethodPolicy){
		"SERVER 缺 scope": func(p *domainauth.MethodPolicy) { p.Scope = nil },
		"write+空角色（无档位）": func(p *domainauth.MethodPolicy) { p.AdminRoles = nil },
		"角色含 viewer":     func(p *domainauth.MethodPolicy) { p.AdminRoles = []string{string(domainauth.AdminRoleViewer)} },
		"PERMISSION+member": func(p *domainauth.MethodPolicy) {
			p.Access = domainauth.AccessPermission
			p.Permissions = []string{"member"}
		},
		"项目寻址违例": func(p *domainauth.MethodPolicy) {
			p.RequestFields = []string{"project_id"}
		},
	} {
		_, err := authz.NewPolicySet(base(mutate)...)
		require.NoError(t, err)
		require.Error(t, domainauth.AssertPolicy(base(mutate)[0]), "违例 [%s] 必须被语义断言拒绝", name)
	}
}
