package clients

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/torchwoodcloud/torchwood/pkg/uow"
	"github.com/uptrace/bun"
)

var _ uow.Runner = (*Database)(nil)

type txContextKey struct{}

// WithTx stores a bun transaction in context for repository adapters.
func WithTx(ctx context.Context, tx bun.Tx) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

// Conn returns the active transaction when present, otherwise the root database.
func (d *Database) Conn(ctx context.Context) bun.IDB {
	if tx, ok := ctx.Value(txContextKey{}).(bun.Tx); ok {
		return tx
	}
	return d.DB
}

// InTx reports whether ctx carries an active transaction (see WithTx).
func InTx(ctx context.Context) bool {
	_, ok := ctx.Value(txContextKey{}).(bun.Tx)
	return ok
}

// ---------------------------------------------------------------------------
// 执行身份（redesign §3.2 工程纪律 / A1，阶段③包 B）
// ---------------------------------------------------------------------------

// DB 角色分层（§4.3）：单一变色龙 authenticator（现有 DSN 用户，成员含三角色）
// + 事务首条 SET LOCAL ROLE 切换，替代多连接池（PostgREST authenticator 模式）。
const (
	// RoleApp 是运行时角色（非 owner 无 BYPASSRLS）——文档 CRUD/查询事务用；
	// 对 c_* 表的 DML 由建表路径按列授予（阶段③包 C）。
	RoleApp = "tw_app"
	// RoleOwner 是 DDL/迁移专用角色（catalog DML、CREATE ON DATABASE），
	// 不跑业务查询。
	RoleOwner = "tw_owner"
	// RoleSystem 是内部旁路角色（BYPASSRLS）——SystemPrincipal / PlatformAdmin。
	RoleSystem = "tw_system"
)

// rolesSeparator 是 app.roles GUC 的元素分隔符（\x1f 控制字符，不出现在
// 角色字符串中；SQL 侧 string_to_array(..., chr(31)) 对应解包）。
const rolesSeparator = "\x1f"

// ExecIdentity 是事务的执行身份：SET LOCAL ROLE 的目标角色 + app.roles GUC
// 的展开角色集（ExpandPermissionRoles 产物）。漏注入 roles → GUC 缺省 NULL →
// RLS policy 恒 false（fail-closed）；SET LOCAL 事务结束自动失效零残留。
type ExecIdentity struct {
	Role  string
	Roles []string
	// Tenant 是 sig 消息覆盖的租户（projects.internal_id，R16 ①）：tw_app
	// 身份必填（documentdb 入口经 resolveInternalID 解析后填入）；tw_owner/
	// tw_system 不验签、不消费。sig 消息 = tenant|roles|exp，tw_set_document_acl
	// 强制 p_tenant = 验签 tenant——跨租户/跨项目在签名层死锁。
	Tenant int64
}

type execIdentityKey struct{}

// WithExecIdentity 把执行身份挂入 ctx（documentdb 的 withDocumentTx 入口构造）。
func WithExecIdentity(ctx context.Context, id ExecIdentity) context.Context {
	return context.WithValue(ctx, execIdentityKey{}, id)
}

// ExecIdentityFrom 读取 ctx 的执行身份；未注入时 ok=false。
func ExecIdentityFrom(ctx context.Context) (ExecIdentity, bool) {
	id, ok := ctx.Value(execIdentityKey{}).(ExecIdentity)
	return id, ok
}

// SameExecIdentity 报告两个身份是否等价（角色、展开角色集与租户一致）。
func SameExecIdentity(a, b ExecIdentity) bool {
	if a.Role != b.Role || a.Tenant != b.Tenant || len(a.Roles) != len(b.Roles) {
		return false
	}
	for i := range a.Roles {
		if a.Roles[i] != b.Roles[i] {
			return false
		}
	}
	return true
}

// InjectExecIdentity 在当前事务连接上注入执行身份（SET LOCAL ROLE + set_config）。
// 供两类调用方使用：RunInTx 的 BEGIN 后首注，与事务中段的身份切换（如尾随
// 读回 SystemPrincipal）；中段切换的调用方负责退出前恢复（ResetExecIdentity
// 或回注外层身份），保证 ctx 身份与连接角色在边界一致。
//
// roles_sig（阶段③-b 包 C，A2 简化版 + R16 ①）：tw_app 身份同时注入
// app.tenant 与 app.roles_sig = HMAC-SHA256(密钥, tenant|roles|exp)
// （"<exp>|<hexmac>"，60s 窗口），供 tw_roles()/tw_tenant() 验签——app.roles
// 与 app.tenant GUC 本身可被任何持 SQL 会话者 set_config 伪造，验签后伪造
// 通道封死（密钥仅存在于 Go 进程与 tw_secrets 表，tw_app 不可读）。
// 密钥未初始化时不注入 sig → 验签失败 → 零角色/NULL tenant fail-closed
// （与漏注入同语义）。
func InjectExecIdentity(ctx context.Context, idb bun.IDB, id ExecIdentity) error {
	switch id.Role {
	case RoleApp, RoleOwner, RoleSystem:
	default:
		// 角色名拼入 SQL：仅接受固定白名单，拒绝任意输入。
		return fmt.Errorf("invalid exec role %q", id.Role)
	}
	// pgdriver 全程 simple protocol（客户端插参）：注入语句合并为单次往返
	//（A1 原型验证结论）。
	roles := strings.Join(id.Roles, rolesSeparator)
	stmt, args := injectIdentitySQL(id.Role, id.Tenant, roles)
	_, err := idb.ExecContext(ctx, fmt.Sprintf(stmt, id.Role), args...)
	if err != nil {
		return fmt.Errorf("inject exec identity %s: %w", id.Role, err)
	}
	return nil
}

// injectIdentitySQL 返回（带一个 %s 角色位占位的）注入语句与绑定参数：
// tw_app 且密钥已初始化时附带 app.tenant 与 roles_sig 签名（三个 set_config
// 同语句），其余角色仅注入 roles。
func injectIdentitySQL(role string, tenant int64, roles string) (string, []any) {
	if role == RoleApp {
		if keyHex, ok := RolesSigKeyHex(); ok {
			return `SET LOCAL ROLE %s; SELECT set_config('app.roles', ?, true), set_config('app.roles_sig', ?, true), set_config('app.tenant', ?, true)`,
				[]any{roles, SignRolesSig(keyHex, tenant, roles, time.Now()), strconv.FormatInt(tenant, 10)}
		}
	}
	return `SET LOCAL ROLE %s; SELECT set_config('app.roles', ?, true)`, []any{roles}
}

// ResetExecIdentity 把连接恢复为 authenticator 本身份（RESET ROLE）并清空
// app.roles/app.roles_sig/app.tenant——用于无外层身份的事务中段切换退出
// （复合 uow 事务回到 DSN 用户）。
func ResetExecIdentity(ctx context.Context, idb bun.IDB) error {
	if _, err := idb.ExecContext(ctx,
		`RESET ROLE; SELECT set_config('app.roles', '', true), set_config('app.roles_sig', '', true), set_config('app.tenant', '', true)`,
	); err != nil {
		return fmt.Errorf("reset exec identity: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// roles_sig 密钥派生与签名（阶段③-b 包 C，A2；page-token 同模式：
// pkg/crud pagination.go 的 purpose 派生 + 进程级 atomic Init）
// ---------------------------------------------------------------------------

// RolesSigPurpose 是 roles GUC 签名的密钥派生域（HMAC-SHA256(master, purpose)）。
const RolesSigPurpose = "tw-roles-guc-v1"

// rolesSigTTL 是签名有效期窗口（注入时刻起 60s：文档事务为短事务，窗口
// 同时是 DB 时钟偏差容差）。
const rolesSigTTL = 60 * time.Second

var rolesSigKeyHex atomic.Value // string

// InitRolesSigKey 以主密钥（security.jwt.secret）进程内派生 roles 签名密钥
// （hex）。组合根启动期调用（server/worker 各自一次）；落库 public.tw_secrets
// 供 tw_roles() 验签由部署期 owner 一次性作业完成（`torchwood admin
// sync-roles-sig` → SyncRolesSigKey，转出 POC 门禁 B15——运行 DSN 对
// tw_secrets 零权限，启动钩子已退役）。
func InitRolesSigKey(master string) error {
	if strings.TrimSpace(master) == "" {
		return fmt.Errorf("roles sig signing requires a non-empty master secret")
	}
	mac := hmac.New(sha256.New, []byte(master))
	_, _ = mac.Write([]byte(RolesSigPurpose))
	rolesSigKeyHex.Store(hex.EncodeToString(mac.Sum(nil)))
	return nil
}

// RolesSigKeyHex 返回进程内派生的 roles 签名密钥（hex）；未初始化时 ok=false
// （此时 tw_app 注入不带 sig，DB 侧验签 fail-closed）。
func RolesSigKeyHex() (string, bool) {
	v, ok := rolesSigKeyHex.Load().(string)
	return v, ok && v != ""
}

// SignRolesSig 生成 app.roles_sig 的值："<exp_unix>|<hexmac>"，mac =
// HMAC-SHA256(keyHex, tenant + "|" + roles + "|" + exp)（R16 ①：消息覆盖
// tenant，跨租户伪造在验签层死锁）。exp 由 now+TTL 起算。keyHex 取进程内
// 派生钥（RolesSigKeyHex），经 SyncRolesSigKey 落库为 tw_secrets 的 current
// 位——签名恒用 current，验证侧（tw_sig_match）再兼容 previous。
func SignRolesSig(keyHex string, tenant int64, roles string, now time.Time) string {
	exp := now.Add(rolesSigTTL).Unix()
	mac := hmac.New(sha256.New, []byte(keyHex))
	_, _ = mac.Write([]byte(strconv.FormatInt(tenant, 10) + "|" + roles + "|" + strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "|" + hex.EncodeToString(mac.Sum(nil))
}

// SyncRolesSigKey 把进程内派生的 roles 签名密钥落进 public.tw_secrets
// （迁移 000004，双钥槽位），供 tw_roles() 验签——部署期 owner 一次
// 性作业（`torchwood admin sync-roles-sig`，转出 POC 门禁 B15）以引导/owner
// 身份调用；运行 DSN 对 tw_secrets 零权限（迁移 000004 收口），启动钩子已
// 退役，server/worker 不再落库（进程内派生钥仅供注入签名，无需读库）。
//
// 双钥轮换（转出 POC 门禁 A4）：新钥落 current 位，旧 current 降级 previous
// （而非覆盖删除），previous 至多保留紧邻上一把（third 条直接删）。滚动重启
// 换钥期间，旧进程签发的 sig 在 60s TTL 窗口内经 previous 验签通过、窗口外
// （exp 过期）依旧拒绝——消除单钥覆盖式换钥的权限面降级窗口。同钥重启幂等
// （current 已是目标钥时整体 no-op）；回滚场景（目标钥躺在 previous 位）把
// 该钥提回 current。四条语句经 simple protocol 单次往返执行，构成隐式事务
// （全或无）。
func SyncRolesSigKey(ctx context.Context, db *Database) error {
	keyHex, ok := RolesSigKeyHex()
	if !ok {
		return errors.New("roles sig key not initialized (call InitRolesSigKey first)")
	}
	p := RolesSigPurpose
	// ① 目标钥若躺在 previous 位（回滚场景）先移除——将被重插为 current；
	// ② 旧 current（异钥）降级 previous；
	// ③ 新钥落 current 位（同钥重启幂等跳过）；
	// ④ third 条直接删：previous 仅保留最新一条（updated_at 降序 + key_hex
	//    决胜，确定性保留），行数不变量 ≤2。
	if _, err := db.Conn(ctx).ExecContext(ctx, `
DELETE FROM public.tw_secrets WHERE purpose = ? AND NOT is_current AND key_hex = ?;
UPDATE public.tw_secrets SET is_current = FALSE, updated_at = NOW()
 WHERE purpose = ? AND is_current AND key_hex <> ?;
INSERT INTO public.tw_secrets (purpose, key_hex, is_current)
 SELECT ?, ?, TRUE
 WHERE NOT EXISTS (SELECT 1 FROM public.tw_secrets
                   WHERE purpose = ? AND key_hex = ? AND is_current);
DELETE FROM public.tw_secrets
 WHERE purpose = ? AND NOT is_current
   AND key_hex <> COALESCE((SELECT key_hex FROM public.tw_secrets
                            WHERE purpose = ? AND NOT is_current
                            ORDER BY updated_at DESC, key_hex DESC LIMIT 1), '');
`, p, keyHex, p, keyHex, p, keyHex, p, keyHex, p, p); err != nil {
		return fmt.Errorf("sync roles sig key: %w", err)
	}
	return nil
}

// Run 实现 uow.Runner：已在工作单元内则加入，否则开启新事务。
// 委托 RunInTx，嵌套行为不变。
func (d *Database) Run(ctx context.Context, fn func(ctx context.Context) error) error {
	return d.RunInTx(ctx, fn)
}

// RunInTx runs fn inside a database transaction. When ctx already carries an
// active transaction (see WithTx), fn runs on that transaction instead of
// opening a nested one — nested transactions on separate connections would
// deadlock on DDL locks held by the outer transaction.
// ctx 带 ExecIdentity 时（阶段③包 B），BEGIN 后立即注入 SET LOCAL ROLE +
// app.roles（每请求一事务，A1）；未带身份的事务保持 authenticator 本身份
// （静态系统表面 = 边界邻居，独立应用层授权，redesign 预决策 9）。
func (d *Database) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if InTx(ctx) {
		return fn(ctx)
	}
	return d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if id, ok := ExecIdentityFrom(ctx); ok {
			if err := InjectExecIdentity(ctx, tx, id); err != nil {
				return err
			}
		}
		return fn(WithTx(ctx, tx))
	})
}

// RunInNewTx runs fn in a fresh transaction even when ctx already carries one.
// 用于「子操作必须先于外层事务提交」的路径（如订单 + provider index 在渠道
// 下单之前 COMMIT，见设计 §9.2）：外层失败回滚自己的部分，子事务保持已提交。
// 调用方自行确保两张事务不触碰会互相加锁的行。
// 与 RunInTx 同规则注入 ExecIdentity（未带身份则保持 authenticator）。
func (d *Database) RunInNewTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if id, ok := ExecIdentityFrom(ctx); ok {
			if err := InjectExecIdentity(ctx, tx, id); err != nil {
				return err
			}
		}
		return fn(WithTx(ctx, tx))
	})
}
