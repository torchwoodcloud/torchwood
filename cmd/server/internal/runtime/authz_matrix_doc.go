package runtime

import (
	"bytes"
	"fmt"
	"strings"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// 本文件是授权矩阵文档的渲染层（机制重设计 Phase B）：从 domainauth.PolicySet
// 生成 docs/developer/authz-matrix.md。策略唯一声明在 proto；文档、矩阵测试、
// well-known 下发、SDK 常量全部是 PolicySet 的派生物，互不手写。文档漂移由
// authz_matrix_doc_test.go 字节级锁定（重渲染 ≠ 磁盘即红）。

// authzMatrixHeader 是生成文档的文件头（注明生成来源与再生成方式）。
const authzMatrixHeader = `# 授权矩阵（Authorization Matrix）

> **本文件由策略注册表生成（` + "`task gen:authz-matrix`" + `），勿手改。**
>
> - 声明源：` + "`proto/shared/v1/authz.proto`" + ` 的 ` + "`method_auth`/`service_auth`" + ` 注解 →
>   ` + "`cmd/server/internal/runtime.BuildMethodPolicies`" + `（启动期经 ` + "`ProvideMethodPolicies`" + ` 注入执行点）。
> - 策略变更后重新生成：` + "`task gen:authz-matrix`" + `；漂移由
>   ` + "`cmd/server/internal/runtime/authz_matrix_doc_test.go`" + ` 字节级锁定（重渲染 ≠ 磁盘即红）。
> - 执行器消费同一策略的行为一致性证明见 ` + "`cmd/server/internal/runtime/authz_matrix_test.go`" + `
>   （全方法 × 凭证档过真实拦截器，与独立推导全量比对）。
> - Access 语义：PUBLIC 匿名可调；END_USER 端用户会话专属（client 面）；
>   SERVER admin 会话（admin_roles）或 API key（scope）；PERMISSION admin 会话
>   专属（permissions），key 一律拒绝；SYSTEM 预留禁用。
`

// authzMatrixThreatModelAppendix 是威胁模型附录（静态文本）：罗列从 PolicySet
// 派生不了的已知取舍（评审裁决，不在策略注册表的表达范围内）。重生成时随
// 渲染器输出，任何措辞变更走代码评审。
const authzMatrixThreatModelAppendix = `
## 附录：威胁模型已知取舍（静态文本）

以下条目无法从策略注册表（PolicySet）派生，属安全评审确认接受的已知取舍；
本节为渲染器内置静态文本，任何一项变更需重新过安全评审：

- **限流与熔断 fail-open**：限流器基础设施（Redis）故障进入熔断放行态时，
  请求跳过限流判定直接放行（可用性优先于精确限流）。攻击者打挂 Redis 可
  换取限流旁路窗口；鉴权链路本身不受影响（fail-closed）。
- **匿名公开读（guests 显式授予）**：PUBLIC 级方法与文档 ACL 的 guests 角色
  属"公开读"语义，匿名流量按显式授权放行，是产品设计而非漏洞；集合/文档级
  收紧由 ACL 声明负责。
- **file token 上限 1 小时、颁发后不可撤销**：存储下载/预览 token 为无状态
  HMAC 签名（服务端不保存颁发记录），有效期上限 1 小时（默认 15 分钟）；
  泄露窗口上限即剩余有效期，无吊销通道，依赖短有效期兜底。
- **单 jwt.secret**：端用户会话、console 会话与 file token 的签名密钥均从
  唯一 ` + "`security.jwt.secret`" + ` 派生（tw_roles 的 HMAC 亦同源）；该密钥泄露
  等于全部凭证面同时失守，轮换窗口内旧 token 不失效。
`

// RenderAuthzMatrix 从策略注册表渲染授权矩阵 Markdown（确定性输出：方法与
// 服务按序遍历，列值全部来自 MethodPolicy 派生）。
func RenderAuthzMatrix(set *domainauth.PolicySet) ([]byte, error) {
	if set == nil {
		return nil, fmt.Errorf("策略注册表为 nil")
	}
	methods := set.Methods()
	if len(methods) == 0 {
		return nil, fmt.Errorf("策略注册表为空")
	}

	var buf bytes.Buffer
	buf.WriteString(authzMatrixHeader)

	// 统计行（按 access 计数）。
	counts := map[domainauth.AccessLevel]int{}
	for _, p := range methods {
		counts[p.Access]++
	}
	fmt.Fprintf(&buf, "\n方法总数 %d：PUBLIC %d · END_USER %d · SERVER %d · PERMISSION %d · SYSTEM %d。\n",
		len(methods), counts[domainauth.AccessPublic], counts[domainauth.AccessEndUser],
		counts[domainauth.AccessServer], counts[domainauth.AccessPermission], counts[domainauth.AccessSystem])

	// 按服务分节（Methods 已按方法名排序，服务自然成组且顺序稳定）。
	current := ""
	for _, p := range methods {
		if p.Service != current {
			current = p.Service
			fmt.Fprintf(&buf, "\n## %s\n\n", p.Service)
			buf.WriteString("| 方法 | Access | Admin 角色档 | Key Scope | 档位 | 说明 |\n")
			buf.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		}
		row, err := authzMatrixRow(p)
		if err != nil {
			return nil, err
		}
		buf.WriteString(row)
	}

	buf.WriteString(authzMatrixThreatModelAppendix)
	return buf.Bytes(), nil
}

// RenderAuthzMatrixFromProto 从真实 proto descriptor 清单构建注册表并渲染
// （genauthzmatrix 命令与 CI 锁测试共用，保持与启动期同源）。
func RenderAuthzMatrixFromProto() ([]byte, error) {
	set, err := BuildMethodPolicies(authzFileDescriptors()...)
	if err != nil {
		return nil, err
	}
	return RenderAuthzMatrix(set)
}

// authzMatrixRow 渲染单方法行：方法 / access / admin 角色档 / key scope /
// 档位 / 说明（留空，供人工补充评审注记——生成器不填，避免第二策略源）。
// "Admin 角色档"列是 admin 会话角色门的统一投影：SERVER 面取 admin_roles
// （空 = 不限角色），PERMISSION 面取 permissions（owner/admin/console）。
func authzMatrixRow(p domainauth.MethodPolicy) (string, error) {
	roles := "—"
	switch p.Access {
	case domainauth.AccessServer:
		if len(p.AdminRoles) == 0 {
			roles = "不限角色"
		} else {
			roles = strings.Join(domainauth.RoleStrings(p.AdminRoles), ", ")
		}
	case domainauth.AccessPermission:
		if len(p.Permissions) > 0 {
			roles = strings.Join(p.Permissions, ", ")
		}
	}
	scope := "—"
	if p.Scope != nil {
		scope = string(p.Scope.Resource) + "." + string(p.Scope.Op)
	}
	tier := "—"
	switch {
	case p.Access == domainauth.AccessServer:
		t, err := domainauth.ClassifyTier(p)
		if err != nil {
			return "", err
		}
		tier = string(t)
		// 档位仅定义在 SERVER 面（console 面 PERMISSION 的 permissions 值域含
		// console 标签，不在 ClassifyTier 值域内——与 AssertPolicy 口径一致）。
	case p.Access == domainauth.AccessPermission && strings.HasPrefix(p.Service, "/torchwood.server.v1."):
		t, err := domainauth.ClassifyTier(p)
		if err != nil {
			return "", err
		}
		tier = string(t)
	}
	return fmt.Sprintf("| `%s` | %s | %s | %s | %s | |\n",
		p.Method, authzAccessName(p.Access), roles, scope, tier), nil
}

// authzAccessName 将 access 级别渲染为文档用名（与 proto enum 名对齐）。
func authzAccessName(l domainauth.AccessLevel) string {
	switch l {
	case domainauth.AccessPublic:
		return "PUBLIC"
	case domainauth.AccessEndUser:
		return "END_USER"
	case domainauth.AccessServer:
		return "SERVER"
	case domainauth.AccessPermission:
		return "PERMISSION"
	case domainauth.AccessSystem:
		return "SYSTEM"
	default:
		return fmt.Sprintf("UNSPECIFIED(%d)", int(l))
	}
}
