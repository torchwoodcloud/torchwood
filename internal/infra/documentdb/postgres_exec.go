// 执行身份包装（redesign §3.2 工程纪律 / A1，阶段③包 B）：文档面所有入口
// 经 withDocumentTx/withOwnerTx 包进带身份的显式事务——每请求一事务（含读，
// autocommit 退役），事务首条 SET LOCAL ROLE + set_config('app.roles') 注入
// （漏注入 = RLS policy 恒 false，fail-closed；policy 本身在包 C 落地）。
package documentdb

import (
	"context"
	"fmt"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

// execIdentityFor 把文档主体映射为执行身份（§3.2 #8：系统/平台管理员旁路走
// 独立 BYPASSRLS 连接角色，绝不编码进 GUC 或 policy 白名单）：
// BypassesDocumentACL（SystemPrincipal / PlatformAdmin）→ tw_system；
// 其余 → tw_app。app.roles 注入 ExpandPermissionRoles 展开角色（与
// AllowsDocumentAccess 判定同源输入）。
func execIdentityFor(principal databases.Principal) clients.ExecIdentity {
	expanded := databases.ExpandPermissionRoles(principal.Roles)
	if principal.BypassesDocumentACL() {
		return clients.ExecIdentity{Role: clients.RoleSystem, Roles: expanded}
	}
	return clients.ExecIdentity{Role: clients.RoleApp, Roles: expanded}
}

// execIdentity 是文档面入口的身份构建器（R16 ①）：在开启事务前解析项目
// internal_id（resolveInternalID 有进程缓存）并填入 Tenant——sig 消息覆盖
// tenant（tenant|roles|exp），tw_set_document_acl 强制 p_tenant = 验签 tenant，
// 跨租户/跨项目在签名层死锁。解析失败时 Tenant=0：sig 仍自洽注入，但不会
// 命中任何真实行的 _tenant（identity 序列自 1 起），且事务体内的
// resolvePhysicalTable 会以真实错误先行返回——fail-closed。
// 接线选择说明：未采用"把身份构建挪进事务体"（withDocumentTx 的 BEGIN 后
// 首注依赖 ctx 身份先于 fn 存在，挪入需改全部入口的事务结构）；本包装在
// 入口处多一次缓存读（首访一次 projects 点查），侵入最小。
func (p *postgresDocumentDB) execIdentity(ctx context.Context, projectID string, principal databases.Principal) clients.ExecIdentity {
	id := execIdentityFor(principal)
	if id.Role != clients.RoleApp {
		return id
	}
	if internalID, err := p.resolveInternalID(ctx, projectID); err == nil {
		id.Tenant = internalID
	}
	return id
}

// setDocumentACL 替换文档 _acl（R16 收口后语义）：
//   - tw_app 身份：经 tw_set_document_acl（000004，SECURITY DEFINER
//     owner=tw_system，BYPASSRLS 绕开 UPDATE 修改 SELECT policy 引用列的新行
//     复检）。函数内两道强制校验：p_tenant = tw_tenant()（验签 tenant，跨
//     租户/跨项目在签名层死锁）与目标行 tw_visible 可见性（堵项目内改他人
//     ACL 提权读）；任一不满足 → 返回 0（= 无行可改）。合法路径不受影响：
//     "可写即可读"使 update/upsert/bulk 的既有权限面 ⊆ tw_visible 面。
//   - tw_system 身份（SystemPrincipal/PlatformAdmin 的 _acl 替换）：信任根
//     直写 UPDATE——tw_system 无函数 EXECUTE（R16 ④ 收紧后仅 tw_app），且
//     表级 ALL 不受列授权限制；BYPASSRLS 语义等价、无提权面（已持全权）。
//
// p_table 在函数内经 catalog physical_name 白名单校验（防注入）。
func (p *postgresDocumentDB) setDocumentACL(ctx context.Context, schema, physical string, tenant int64, docID string, perms []databases.Permission) error {
	if id, ok := clients.ExecIdentityFrom(ctx); ok && id.Role == clients.RoleSystem {
		res, err := p.conn(ctx).ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET %s = ?::text[] WHERE _id = ? AND _tenant = ?`, tableName(schema, physical), quoteIdent("_acl")),
			aclParam(perms), docID, tenant)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("set document acl: expected 1 row, got %d", n)
		}
		return nil
	}
	var n int64
	if err := p.conn(ctx).QueryRowContext(ctx,
		`SELECT public.tw_set_document_acl(?, ?, ?, ?, ?::text[])`,
		schema, physical, tenant, docID, aclParam(perms),
	).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("set document acl: expected 1 row, got %d", n)
	}
	return nil
}

// withDocumentTx 把文档面操作包进带执行身份的事务（读写同构，A1）。
// 已在外层事务中：外层身份一致时直接执行（零额外语句）；不一致（如尾随读回
// SystemPrincipal）则事务中段切换并在退出前恢复外层身份——ctx 身份与连接
// 角色在边界保持一致，中途切换不向调用方泄漏；外层无身份（复合 uow 事务）
// 则切换后 RESET ROLE 回 authenticator。
func (p *postgresDocumentDB) withDocumentTx(ctx context.Context, id clients.ExecIdentity, fn func(ctx context.Context) error) error {
	if clients.InTx(ctx) {
		cur, hasCur := clients.ExecIdentityFrom(ctx)
		if hasCur && clients.SameExecIdentity(cur, id) {
			return fn(ctx)
		}
		if err := clients.InjectExecIdentity(ctx, p.conn(ctx), id); err != nil {
			return err
		}
		err := fn(clients.WithExecIdentity(ctx, id))
		var restoreErr error
		if hasCur {
			restoreErr = clients.InjectExecIdentity(ctx, p.conn(ctx), cur)
		} else {
			restoreErr = clients.ResetExecIdentity(ctx, p.conn(ctx))
		}
		if err != nil {
			return err
		}
		return restoreErr
	}
	return p.db.RunInTx(clients.WithExecIdentity(ctx, id), fn)
}

// withOwnerTx 把 DDL 面操作包进 tw_owner 事务（catalog DML + CREATE ON
// DATABASE）。嵌套语义与 withDocumentTx 相同（中段切换 + 退出恢复）。
func (p *postgresDocumentDB) withOwnerTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return p.withDocumentTx(ctx, clients.ExecIdentity{Role: clients.RoleOwner}, fn)
}
