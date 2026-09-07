package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/api/interceptor"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// 授权矩阵测试（机制重设计 Phase B）：数据源 = 真实 proto 的 PolicySet
// （BuildMethodPolicies(authzFileDescriptors()...)），对全部方法按 access
// 级别裁剪凭证档，过真实 AuthInterceptor（stub validator 注入主体，无 DB
// 依赖），断言实际放行/拒绝与独立推导函数 expectedDecision 全量一致——
// 证明"执行器与期望一致消费同一策略"。评审裁决的语义锚点单列
// （TestAuthzMatrix_SemanticAnchors），防止锚点被静默改宽。

// matrixPolicySet 构造真实 proto 策略注册表（与 ProvideMethodPolicies 同源，
// 但不经 AssertSemantic——语义断言另由 grpc_authz_test.go 承担，本文件专注
// 执行行为与推导的一致性）。
func matrixPolicySet(t *testing.T) *domainauth.PolicySet {
	t.Helper()
	set, err := BuildMethodPolicies(authzFileDescriptors()...)
	require.NoError(t, err)
	return set
}

// ---------------------------------------------------------------------------
// 凭证档

type matrixCredKind int

const (
	matrixCredAnonymous matrixCredKind = iota // 无凭证元数据
	matrixCredEndUser                         // Bearer 端用户会话
	matrixCredAdmin                           // Session admin 会话
	matrixCredAPIKey                          // x-api-key
)

// matrixCredential 是单个凭证档：传输形态（metadata）与 stub validator
// 注入的主体一体定义。
type matrixCredential struct {
	name          string
	kind          matrixCredKind
	roles         []string // principal.Roles（admin 角色串 / 端用户角色）
	scopes        []string // principal.Permissions（API key scope 集）
	projectHeader string   // X-Torchwood-Project（admin 会话，单值）
}

var (
	matrixAnon     = matrixCredential{name: "匿名（无凭证元数据）", kind: matrixCredAnonymous}
	matrixEndUser  = matrixCredential{name: "端用户会话", kind: matrixCredEndUser, roles: []string{"users"}}
	matrixAPIKeyNo = matrixCredential{name: "API key（无 scope）", kind: matrixCredAPIKey}
)

// matrixAdmin 构造指定角色的 admin 会话档。
func matrixAdmin(role domainauth.AdminRole) matrixCredential {
	return matrixCredential{name: "admin 会话（" + string(role) + "）", kind: matrixCredAdmin, roles: []string{string(role)}}
}

// matrixAPIKey 构造携带给定 scope 集的 API key 档。
func matrixAPIKey(scopes ...string) matrixCredential {
	return matrixCredential{name: fmt.Sprintf("API key（scope=%v）", scopes), kind: matrixCredAPIKey, scopes: scopes}
}

// principal 构造 stub validator 返回的主体（字段形态对照
// internal/infra/auth 真实 validator 的投影约定）。
func (c matrixCredential) principal() *shared.Principal {
	switch c.kind {
	case matrixCredEndUser:
		return &shared.Principal{
			ActorKind:      shared.ActorKindEndUser,
			CredentialType: shared.CredentialTypeToken,
			UserID:         "user-matrix",
			ProjectID:      "proj-matrix",
			Roles:          c.roles,
		}
	case matrixCredAdmin:
		return &shared.Principal{
			ActorKind:       shared.ActorKindAdmin,
			CredentialType:  shared.CredentialTypeSession,
			AdminID:         "admin-matrix",
			ProjectID:       "proj-matrix",
			IsPlatformAdmin: true,
			Roles:           c.roles,
		}
	case matrixCredAPIKey:
		return &shared.Principal{
			ActorKind:      shared.ActorKindService,
			CredentialType: shared.CredentialTypeAPIKey,
			APIKeyID:       "key-matrix",
			ProjectID:      "proj-matrix",
			Roles:          []string{"keys"},
			Permissions:    c.scopes,
		}
	default:
		return nil
	}
}

// metadata 构造凭证档的传输形态（nil = 不携带任何凭证元数据）。
func (c matrixCredential) metadata() metadata.MD {
	switch c.kind {
	case matrixCredEndUser:
		return metadata.Pairs("authorization", "Bearer end-user-token")
	case matrixCredAdmin:
		if c.projectHeader != "" {
			return metadata.Pairs("authorization", "Session admin-token", "x-torchwood-project", c.projectHeader)
		}
		return metadata.Pairs("authorization", "Session admin-token")
	case matrixCredAPIKey:
		return metadata.Pairs("x-api-key", "matrix-key")
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// stub validator（本文件自有，不改 interceptor 包既有 helper 行为）

// matrixValidator 按凭证档返回主体，并经 ValidateAdminProjectAccess 捕获
// 拦截器处理后的主体（供 X-Torchwood-Project 覆盖断言）。
type matrixValidator struct {
	principal *shared.Principal
	captured  *shared.Principal
}

func (v *matrixValidator) Authenticate(_ context.Context, req shared.AuthnRequest) (*shared.Principal, error) {
	if _, _, err := shared.ParseAuthnRequest(req); err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return v.principal, nil
}

func (v *matrixValidator) ValidateToken(_ context.Context, _ string) (*shared.Principal, error) {
	return v.principal, nil
}

func (v *matrixValidator) ValidateCredential(_ context.Context, _ string, _ shared.CredentialType) (*shared.Principal, error) {
	return v.principal, nil
}

func (v *matrixValidator) ValidateAdminProjectAccess(_ context.Context, p *shared.Principal) error {
	v.captured = p
	return nil
}

// ---------------------------------------------------------------------------
// 独立推导

type matrixDecision int

const (
	matrixAllow matrixDecision = iota
	matrixUnauthenticated
	matrixPermissionDenied
)

func (d matrixDecision) String() string {
	switch d {
	case matrixAllow:
		return "放行"
	case matrixUnauthenticated:
		return "Unauthenticated"
	case matrixPermissionDenied:
		return "PermissionDenied"
	default:
		return fmt.Sprintf("matrixDecision(%d)", int(d))
	}
}

// expectedDecision 独立推导期望裁决：只读 MethodPolicy 字段与凭证档做
// if/else 推导，不触碰拦截器代码路径。语义逐条对照
// internal/api/interceptor/jwt.go 的 UnaryAuthMiddleware：
//  1. PUBLIC：凭证尽力注入、失败不阻断——无条件放行；
//  2. 非 PUBLIC：无凭证 → Unauthenticated；
//  3. SERVER 面凭证族门禁：仅 API key / admin 会话可入，端用户 Unauthenticated；
//     API key 过 scope 门（* / all / 裸资源 / <res>.<op>；nil = 不对 key 开放，
//     通配符不豁免）；
//  4. admin 会话角色门（任意面；AdminRoles 空 = 不限角色）；
//  5. PERMISSION/END_USER 面角色门；API key 一律 PermissionDenied
//     （apikey_permission_method_denied，通配符不豁免）。
func expectedDecision(p domainauth.MethodPolicy, cred matrixCredential) matrixDecision {
	if p.Access == domainauth.AccessPublic {
		return matrixAllow
	}
	if cred.kind == matrixCredAnonymous {
		return matrixUnauthenticated
	}
	if p.Access == domainauth.AccessServer {
		if cred.kind == matrixCredEndUser {
			return matrixUnauthenticated
		}
		if cred.kind == matrixCredAPIKey && !matrixScopeAllows(p.Scope, cred.scopes) {
			return matrixPermissionDenied
		}
	}
	if cred.kind == matrixCredAdmin && len(p.AdminRoles) > 0 &&
		!matrixHasAny(cred.roles, domainauth.RoleStrings(p.AdminRoles)) {
		return matrixPermissionDenied
	}
	if len(p.Permissions) > 0 {
		if cred.kind == matrixCredAPIKey {
			return matrixPermissionDenied
		}
		if !matrixHasAny(cred.roles, p.Permissions) {
			return matrixPermissionDenied
		}
	}
	return matrixAllow
}

// matrixScopeAllows 独立复刻 AllowsAPIKey 匹配语义（不调用生产代码路径）。
func matrixScopeAllows(rule *domainauth.ScopeRule, scopes []string) bool {
	if rule == nil {
		return false
	}
	for _, sc := range scopes {
		switch sc {
		case "*", "all":
			return true
		case string(rule.Resource):
			return true
		case string(rule.Resource) + "." + string(rule.Op):
			return true
		}
	}
	return false
}

func matrixHasAny(haystack, needles []string) bool {
	for _, n := range needles {
		for _, h := range haystack {
			if h == n {
				return true
			}
		}
	}
	return false
}

// compareMatrixCase 是矩阵比对的唯一入口（主矩阵与变异检出测试共用）。
func compareMatrixCase(p domainauth.MethodPolicy, cred matrixCredential, got matrixDecision) error {
	want := expectedDecision(p, cred)
	if want != got {
		return fmt.Errorf("方法 %s × 凭证档 %s：独立推导期望 %v，拦截器实际 %v（策略 Access=%v AdminRoles=%v Scope=%v Permissions=%v）",
			p.Method, cred.name, want, got, p.Access, p.AdminRoles, p.Scope, p.Permissions)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 凭证档裁剪

// matrixProfilesFor 按 access 级别裁剪凭证档（按面裁剪，非全笛卡尔积）。
func matrixProfilesFor(p domainauth.MethodPolicy, vocab *domainauth.ScopeVocabulary) []matrixCredential {
	switch p.Access {
	case domainauth.AccessPublic:
		// 公开方法：匿名放行即锁死公开语义。
		return []matrixCredential{matrixAnon}
	case domainauth.AccessEndUser:
		// client 面：端用户放行 / API key 拒绝 / 匿名拒绝。
		return []matrixCredential{matrixAnon, matrixEndUser, matrixAPIKey("*")}
	case domainauth.AccessServer:
		profiles := []matrixCredential{
			matrixAnon,
			matrixEndUser, // 凭证族门禁：端用户会话不得入 SERVER 面
			matrixAdmin(domainauth.AdminRoleViewer),
			matrixAdmin(domainauth.AdminRoleMember),
			matrixAdmin(domainauth.AdminRoleAdmin),
			matrixAdmin(domainauth.AdminRoleOwner),
			matrixAPIKeyNo,
			matrixAPIKey(matrixMismatchScope(p, vocab)), // 词表内不匹配 scope
		}
		if p.Scope != nil {
			profiles = append(profiles,
				matrixAPIKey(string(p.Scope.Resource)+"."+string(p.Scope.Op)), // 匹配方向
				matrixAPIKey(string(p.Scope.Resource)),                        // 裸资源名
			)
		}
		profiles = append(profiles, matrixAPIKey("*"), matrixAPIKey("all"))
		return profiles
	case domainauth.AccessPermission:
		return []matrixCredential{
			matrixAnon,
			matrixAPIKey("*"),
			matrixAPIKey("all"),
			matrixPermissionHit(p),
			matrixPermissionMiss(p),
			matrixEndUser, // 端用户对 PERMISSION 面（含 console 面）一律拒绝
		}
	default:
		// SYSTEM 级被 AssertSemantic 禁止；出现时至少验证匿名拒绝（fail-closed）。
		return []matrixCredential{matrixAnon}
	}
}

// matrixPermissionHit 构造命中 permissions 角色门的 admin 会话档。
func matrixPermissionHit(p domainauth.MethodPolicy) matrixCredential {
	if len(p.Permissions) == 0 {
		return matrixAdmin(domainauth.AdminRoleOwner)
	}
	return matrixCredential{name: "admin 会话（命中 " + p.Permissions[0] + "）", kind: matrixCredAdmin, roles: []string{p.Permissions[0]}}
}

// matrixPermissionMiss 构造必然未命中 permissions 角色门的 admin 会话档
// （permissions 值域 ⊆ {owner, admin, console}，viewer 恒未命中）。
func matrixPermissionMiss(p domainauth.MethodPolicy) matrixCredential {
	for _, r := range domainauth.AllAdminRoles {
		if !matrixHasAny([]string{string(r)}, p.Permissions) {
			return matrixAdmin(r)
		}
	}
	// 理论不可达（值域断言保证）；兜底仍给一个未命中档。
	return matrixCredential{name: "admin 会话（空角色）", kind: matrixCredAdmin, roles: []string{"__none__"}}
}

// matrixMismatchScope 从词表取一个与本方法 scope 规则必然不匹配的合法 scope
// （异资源），模拟"持他资源 scope 的 key"。
func matrixMismatchScope(p domainauth.MethodPolicy, vocab *domainauth.ScopeVocabulary) string {
	if p.Scope != nil {
		for _, res := range vocab.Resources() {
			if res == p.Scope.Resource {
				continue
			}
			for _, op := range []domainauth.ScopeOp{domainauth.ScopeRead, domainauth.ScopeWrite} {
				if vocab.HasOp(res, op) {
					return string(res) + "." + string(op)
				}
			}
		}
	}
	return string(domainauth.ScopeUsers) + ".read"
}

// ---------------------------------------------------------------------------
// 执行

// matrixRunCase 过真实拦截器执行单用例，返回实际裁决与捕获的主体。
func matrixRunCase(t *testing.T, set *domainauth.PolicySet, fullMethod string, cred matrixCredential) (matrixDecision, *shared.Principal) {
	t.Helper()
	v := &matrixValidator{principal: cred.principal()}
	ic, err := interceptor.NewAuthInterceptor(v, set)
	require.NoError(t, err)
	// 拒绝路径的留痕日志静音（矩阵大量拒绝用例属预期，避免刷屏）。
	ic.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	if md := cred.metadata(); md != nil {
		ctx = metadata.NewIncomingContext(ctx, md)
	}
	handlerRan := false
	_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{FullMethod: fullMethod}, func(context.Context, any) (any, error) {
		handlerRan = true
		return "ok", nil
	})
	if err == nil {
		require.True(t, handlerRan, "方法 %s × %s：裁决为放行但 handler 未执行", fullMethod, cred.name)
		return matrixAllow, v.captured
	}
	require.False(t, handlerRan, "方法 %s × %s：拦截器报错但 handler 仍执行", fullMethod, cred.name)
	st, ok := status.FromError(err)
	require.True(t, ok, "方法 %s × %s：非 gRPC status 错误: %v", fullMethod, cred.name, err)
	switch st.Code() {
	case codes.Unauthenticated:
		return matrixUnauthenticated, v.captured
	case codes.PermissionDenied:
		return matrixPermissionDenied, v.captured
	default:
		t.Fatalf("方法 %s × %s：意外裁决 %v（%s）", fullMethod, cred.name, st.Code(), st.Message())
		return 0, nil
	}
}

// TestAuthzMatrix_ExecutionMatchesIndependentDerivation 主矩阵：全部方法 ×
// 按面裁剪凭证档，实际裁决与独立推导全量比对。
func TestAuthzMatrix_ExecutionMatchesIndependentDerivation(t *testing.T) {
	t.Parallel()

	set := matrixPolicySet(t)
	vocab := domainauth.VocabularyFromPolicies(set)
	methods := set.Methods()
	require.NotEmpty(t, methods)

	byAccess := map[domainauth.AccessLevel]int{}
	cases, allow, denied := 0, 0, 0
	for _, p := range methods {
		byAccess[p.Access]++
		for _, cred := range matrixProfilesFor(p, vocab) {
			got, _ := matrixRunCase(t, set, p.Method, cred)
			cases++
			switch got {
			case matrixAllow:
				allow++
			default:
				denied++
			}
			if err := compareMatrixCase(p, cred, got); err != nil {
				t.Error(err)
			}
		}
	}
	// 规模下限（防数据源缩水成空矩阵）：方法数对齐 proto 注册面，组合数下限
	// 覆盖全部四档的裁剪设计。
	require.GreaterOrEqual(t, len(methods), 150, "策略注册表方法数异常缩水")
	require.GreaterOrEqual(t, cases, 700, "矩阵用例数异常缩水")
	require.Greater(t, allow, 0)
	require.Greater(t, denied, 0)
	t.Logf("矩阵规模：方法 %d（PUBLIC=%d END_USER=%d SERVER=%d PERMISSION=%d SYSTEM=%d），用例 %d（放行 %d / 拒绝 %d）",
		len(methods), byAccess[domainauth.AccessPublic], byAccess[domainauth.AccessEndUser],
		byAccess[domainauth.AccessServer], byAccess[domainauth.AccessPermission], byAccess[domainauth.AccessSystem],
		cases, allow, denied)
}

// TestAuthzMatrix_AdminProjectHeaderOverridesPrincipal SERVER 面方法同时验证：
// admin 带 X-Torchwood-Project 单值头时 principal.ProjectID 被覆盖为头值
// （经 ValidateAdminProjectAccess 捕获点断言）。
func TestAuthzMatrix_AdminProjectHeaderOverridesPrincipal(t *testing.T) {
	t.Parallel()

	set := matrixPolicySet(t)
	owner := matrixAdmin(domainauth.AdminRoleOwner)
	serverMethods := 0
	for _, p := range set.Methods() {
		if p.Access != domainauth.AccessServer {
			continue
		}
		serverMethods++
		v := &matrixValidator{principal: owner.principal()}
		ic, err := interceptor.NewAuthInterceptor(v, set)
		require.NoError(t, err)
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			"authorization", "Session admin-token",
			"x-torchwood-project", "proj-from-header",
		))
		_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{FullMethod: p.Method}, func(context.Context, any) (any, error) {
			return "ok", nil
		})
		require.NoError(t, err, "%s：owner admin + 单值项目头应放行", p.Method)
		require.NotNil(t, v.captured, "%s：ValidateAdminProjectAccess 未被调用（admin 主体必经）", p.Method)
		require.Equal(t, "proj-from-header", v.captured.ProjectID, "%s：X-Torchwood-Project 单值头应覆盖 principal.ProjectID", p.Method)
	}
	require.Positive(t, serverMethods)
	t.Logf("项目头覆盖验证覆盖 SERVER 面方法 %d 个", serverMethods)
}

// ---------------------------------------------------------------------------
// 语义锚点（手写，锁死评审裁决不被静默改宽）

// TestAuthzMatrix_SemanticAnchors 评审裁决锚点：策略声明（非执行行为）必须
// 精确等于锚点口径；任何"改宽"都在此处变红。
func TestAuthzMatrix_SemanticAnchors(t *testing.T) {
	t.Parallel()

	set := matrixPolicySet(t)
	anchor := func(method string) domainauth.MethodPolicy {
		p, ok := set.Get(method)
		require.True(t, ok, "锚点方法 %s 不在策略注册表", method)
		return p
	}

	// users 六写方法 = SERVER + users.write + {member,admin,owner}（业务写档）。
	for _, name := range []string{"CreateUser", "UpdateUser", "UpdateUserPassword", "DeleteUser", "DeleteUserSession", "CreateUserToken"} {
		method := "/torchwood.server.v1.UsersService/" + name
		p := anchor(method)
		require.Equal(t, domainauth.AccessServer, p.Access, "%s", method)
		require.Equal(t, &domainauth.ScopeRule{Resource: domainauth.ScopeUsers, Op: domainauth.ScopeWrite}, p.Scope, "%s", method)
		require.ElementsMatch(t, []domainauth.AdminRole{domainauth.AdminRoleMember, domainauth.AdminRoleAdmin, domainauth.AdminRoleOwner}, p.AdminRoles, "%s", method)
	}

	// 平台专属面（PERMISSION，无 key 通道）：项目建删 + API key 管理。
	for _, name := range []string{"CreateProject", "DeleteProject"} {
		p := anchor("/torchwood.server.v1.ProjectsService/" + name)
		require.Equal(t, domainauth.AccessPermission, p.Access, "%s", name)
		require.Equal(t, []string{"owner", "admin"}, p.Permissions, "%s", name)
		require.Nil(t, p.Scope, "%s", name)
	}
	for _, name := range []string{"CreateAPIKey", "DeleteAPIKey", "ListAPIKeys", "GetAPIKey"} {
		p := anchor("/torchwood.server.v1.APIKeysService/" + name)
		require.Equal(t, domainauth.AccessPermission, p.Access, "%s", name)
		require.Equal(t, []string{"owner", "admin"}, p.Permissions, "%s", name)
		require.Nil(t, p.Scope, "%s：API key 管理不对 key 开放（防自铸提权）", name)
	}

	// Outbox 两方法 = delegated_platform 档。
	for _, name := range []string{"ListDeadLetters", "ReplayDeadLetter"} {
		p := anchor("/torchwood.server.v1.OutboxService/" + name)
		require.Equal(t, domainauth.AccessServer, p.Access, "%s", name)
		tier, err := domainauth.ClassifyTier(p)
		require.NoError(t, err)
		require.Equal(t, domainauth.TierDelegatedPlatform, tier, "%s", name)
	}

	// UpdateProject = business_write 档（projects.write）。
	p := anchor("/torchwood.server.v1.ProjectsService/UpdateProject")
	tier, err := domainauth.ClassifyTier(p)
	require.NoError(t, err)
	require.Equal(t, domainauth.TierBusinessWrite, tier, "UpdateProject")
	require.Equal(t, &domainauth.ScopeRule{Resource: domainauth.ScopeProjects, Op: domainauth.ScopeWrite}, p.Scope, "UpdateProject")

	// databases DDL 12 方法 = delegated_platform 档。
	for _, name := range []string{
		"CreateDatabase", "DeleteDatabase",
		"CreateCollection", "DeleteCollection", "UpdateCollection",
		"CreateAttribute", "DeleteAttribute", "RestoreAttribute", "RetireAttribute", "MigrateAttribute",
		"CreateIndex", "DeleteIndex",
	} {
		p := anchor("/torchwood.server.v1.DatabasesService/" + name)
		require.Equal(t, domainauth.AccessServer, p.Access, "%s", name)
		tier, err := domainauth.ClassifyTier(p)
		require.NoError(t, err)
		require.Equal(t, domainauth.TierDelegatedPlatform, tier, "%s", name)
	}

	// CreateDocument = business_write 档（databases.write）。
	p = anchor("/torchwood.server.v1.DatabasesService/CreateDocument")
	tier, err = domainauth.ClassifyTier(p)
	require.NoError(t, err)
	require.Equal(t, domainauth.TierBusinessWrite, tier, "CreateDocument")

	// console 面权限口径：写三方法仅 owner；ListAdmins owner/admin；me 型 console 标签。
	for _, name := range []string{"CreateAdmin", "UpdateAdmin", "DeleteAdmin"} {
		p := anchor("/torchwood.console.v1.AdminsService/" + name)
		require.Equal(t, domainauth.AccessPermission, p.Access, "%s", name)
		require.Equal(t, []string{"owner"}, p.Permissions, "%s", name)
	}
	p = anchor("/torchwood.console.v1.AdminsService/ListAdmins")
	require.Equal(t, []string{"owner", "admin"}, p.Permissions, "ListAdmins")
	p = anchor("/torchwood.console.v1.AdminsService/GetCurrentAdmin")
	require.Equal(t, []string{"console"}, p.Permissions, "GetCurrentAdmin")

	// client 面：Me = END_USER ["users"]；SignUp = PUBLIC。
	p = anchor("/torchwood.client.v1.AccountService/Me")
	require.Equal(t, domainauth.AccessEndUser, p.Access, "Me")
	require.Equal(t, []string{domainauth.RoleEndUserTag}, p.Permissions, "Me")
	p = anchor("/torchwood.client.v1.AccountService/SignUp")
	require.Equal(t, domainauth.AccessPublic, p.Access, "SignUp")
}

// TestAuthzMatrix_MutationDetection 变异检出自检（防空洞通过）：取真实用例
// （viewer × delegated DDL = 拒绝），先证明实际裁决与独立推导一致，再手工
// 构造错误期望（放行），断言比对器真的会抓到不一致。
func TestAuthzMatrix_MutationDetection(t *testing.T) {
	t.Parallel()

	set := matrixPolicySet(t)
	method := "/torchwood.server.v1.DatabasesService/CreateDatabase"
	p, ok := set.Get(method)
	require.True(t, ok)
	cred := matrixAdmin(domainauth.AdminRoleViewer)

	got, _ := matrixRunCase(t, set, method, cred)
	require.Equal(t, matrixPermissionDenied, got, "viewer 调 DDL 应被拒")
	// 一致性链路成立：推导与实际一致。
	require.NoError(t, compareMatrixCase(p, cred, got))

	// 变异：把期望改成放行（等价于拦截器被改宽），比对器必须报错。
	mutated := matrixAllow
	if expectedDecision(p, cred) == matrixAllow {
		mutated = matrixPermissionDenied
	}
	err := compareMatrixCase(p, cred, mutated)
	require.Error(t, err, "错误期望（%v）必须被比对器检出", mutated)
	require.Contains(t, err.Error(), method)
}
