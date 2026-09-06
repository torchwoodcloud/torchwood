-- roles_sig 密钥面收口（转出 POC 门禁 B15，docs/developer/15-exit-poc.md）：
-- 运行 DSN（tw_authenticator）对 public.tw_secrets 零权限，密钥落库改部署期
-- owner 一次性作业（`torchwood admin sync-roles-sig` → clients.SyncRolesSigKey，
-- server/worker 启动钩子 RolesSigKeySyncHook 已退役）。
--
-- 威胁模型（B15 立条原文）：authenticator DSN（非 superuser、受 RLS）若泄漏，
-- 四权授予使其可读 tw_secrets 中的 roles_sig 密钥 → 伪造 app.roles /
-- app.roles_sig GUC 提权（受 RLS 的账号 → 任意提权）。REVOKE 后 DSN 泄漏
-- 读不到密钥、写不了槽位，伪造通道封死（sig 仍不可伪造，RLS 判定面不变）。
--
-- 载体选择：前向补丁而非 000029/000031 原地修订——遵 A5 迁移方案确立的规则
-- "已应用的迁移文件永不原地修订；语义修订一律新版本号前向补丁"。
--
-- 部署时序契约（B15）：迁移（本文件）→ `torchwood admin sync-roles-sig`
--（owner/引导身份，DSN 与运行态分别注入）→ server/worker 启动。服务在密钥
-- 未落库时启动，tw_roles() 验签 fail-closed（零角色）→ 文档查询不可见，属
-- 预期（首个业务查询暴露而非静默放行；既有 000029 fail-closed 语义）。
--
-- SECURITY DEFINER owner 链核对（000029/000031）：tw_sig_match / tw_roles /
-- tw_tenant 由迁移执行者（owner 引导账号）CREATE，未 ALTER OWNER——其 owner
-- 与 tw_secrets 表 owner 同为迁移执行者，SECURITY DEFINER 以函数 owner 身份
-- 读表走 owner 隐式全权，**无需任何显式 GRANT**；000029 建表时的
-- "REVOKE ALL ... FROM PUBLIC" + 从不 GRANT 运行时角色，使 base identity 的
-- 直接 GRANT（13-operations §4.5 引导 SQL ⑤）成为 authenticator 可达
-- tw_secrets 的唯一通道——本迁移撤掉它即完成收口。tw_set_document_acl 的
-- owner 是 tw_system（000029 ALTER），不直接读 tw_secrets（经 EXECUTE 授权
-- 的 tw_tenant → tw_sig_match definer 链，各按自身 owner 判定）。

-- 收口：撤销运行账号的全部表权限（含授权面 SQL 历史 ⑤ 授予的四权）。
-- DO 块守护：tw_authenticator 是运维手工创建的集群级角色（13-operations
-- §4.5 引导 SQL ①），未引导的库/测试库中不存在，跳过即保持零权限形态。
DO $do$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tw_authenticator') THEN
        REVOKE ALL ON public.tw_secrets FROM tw_authenticator;
    END IF;
END $do$;
