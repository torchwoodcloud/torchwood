-- RBAC 角色分层 + RLS 判定单源 + roles_sig 验签 + _acl 变更通道（基线重定
-- 2026-09-06：000026/000027/000029/000031/000033 终态合并；redesign §3.2/§4.3）。
--
--   tw_owner  —— DDL/迁移专用（catalog DML、CREATE ON DATABASE），不跑业务查询
--   tw_app    —— 运行时（非 owner 无 BYPASSRLS）；c_* 表 DML 由建表路径授予
--   tw_system —— BYPASSRLS 内部旁路（SystemPrincipal / PlatformAdmin）
-- 单一变色龙 authenticator（现有 DSN 用户）+ 三执行角色，事务首条 SET LOCAL
-- ROLE 完成切换（PostgREST authenticator 模式，替代多连接池）。角色经
-- SET LOCAL ROLE + app.roles GUC 注入（每请求一事务，clients.Database.RunInTx；
-- 漏注入 fail-closed）。依赖顺序：projectschema 的 schema 级 GRANT 依赖本迁移
-- 先建出三角色（public 迁移先于项目 schema Apply：服务启动与 testutil 均满足）。

-- 并行安全（A6）：三角色是集群级对象，多库（go test -p N 并行建库跑迁移）可能
-- 同时到达本迁移；"IF NOT EXISTS 检查 + CREATE"非原子（检查时另一库尚未提交），
-- 用 PL/pgSQL 异常容错实现原子幂等——duplicate 视为另一库已建成就绪，语义不变。
DO $$
BEGIN
    BEGIN
        CREATE ROLE tw_owner NOLOGIN;
    EXCEPTION WHEN duplicate_object THEN NULL;
    END;
    BEGIN
        CREATE ROLE tw_app NOLOGIN;
    EXCEPTION WHEN duplicate_object THEN NULL;
    END;
    BEGIN
        CREATE ROLE tw_system NOLOGIN BYPASSRLS;
    EXCEPTION WHEN duplicate_object THEN NULL;
    END;
END
$$;

-- authenticator membership：现有 DSN 用户成员含三角色。
GRANT tw_owner, tw_app, tw_system TO CURRENT_USER;

GRANT USAGE ON SCHEMA public TO tw_app, tw_owner, tw_system;

-- tw_app（运行时热路径）：租户解析（projects）+ catalog 只读 + outbox 追加
--（SELECT 供 bun INSERT ... RETURNING 回读 default 列）。
GRANT SELECT ON TABLE projects, catalog_databases, catalog_collections TO tw_app;
GRANT INSERT, SELECT ON TABLE document_events_outbox TO tw_app;

-- tw_owner（DDL 面）：catalog 全 DML + 建库建表（CREATE ON DATABASE）。
GRANT SELECT ON TABLE projects TO tw_owner;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE catalog_databases, catalog_collections TO tw_owner;

-- tw_system（内部旁路）：与 tw_app 同基线；c_* 表 ALL 由建表路径授予。
GRANT SELECT ON TABLE projects, catalog_databases, catalog_collections TO tw_system;
GRANT INSERT, SELECT ON TABLE document_events_outbox TO tw_system;

DO $$
BEGIN
    EXECUTE format('GRANT CREATE ON DATABASE %I TO tw_owner', current_database());
END
$$;

-- catalog_migrations 账本（表建于 000003）：tw_owner 写任务账本（迁移创建/
-- 推进）；tw_system 读任务行并执行回填数据语句（迁移期数据访问 = 运维面，
-- BYPASSRLS 身份执行——tw_owner 受 FORCE RLS 约束不可见业务行，A6 runbook 同源）。
GRANT SELECT, INSERT, UPDATE ON catalog_migrations TO tw_owner, tw_system;

-- pgcrypto：验签需要 hmac()（trusted extension，DB owner 可装）。
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- 签名密钥表：部署期 owner 一次性作业落库（`torchwood admin sync-roles-sig`
-- → clients.SyncRolesSigKey，B15：server/worker 启动钩子已退役）。双钥槽位：
-- is_current = TRUE 为 current（Go 进程派生钥的落位，签名信任面），FALSE 为
-- previous（紧邻上一把，换钥窗口内旧 sig 的命中面）。不授予任何运行时角色：
-- tw_app 不可读（否则可自签），仅验签函数经 SECURITY DEFINER 以 owner 读。
-- 部署时序契约（B15）：迁移（本文件）→ sync-roles-sig 作业（owner/引导身份，
-- DSN 与运行态分别注入）→ server/worker 启动；密钥未落库时 tw_roles() 验签
-- fail-closed（零角色）→ 文档查询不可见，属预期。
CREATE TABLE public.tw_secrets (
    purpose    TEXT NOT NULL,
    key_hex    TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    is_current BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (purpose, key_hex)
);
REVOKE ALL ON public.tw_secrets FROM PUBLIC;

-- 部分唯一索引保证每 purpose 至多一把 current（Go 侧平移逻辑的表级兜底约束）。
CREATE UNIQUE INDEX tw_secrets_single_current ON public.tw_secrets (purpose) WHERE is_current;

-- tw_sig_match：sig 验签单源（消息 = tenant|roles|exp）。任一钥命中即通过
-- ——current/previous 两行都进 EXISTS 面；过期判定先于钥匹配，previous
-- 命中无法给窗口外的 sig 续命。仅做密码学校验；"仅 tw_app 身份"的限定由
-- 调用方（tw_roles/tw_tenant）承载。密钥缺失/格式错/过期/mac 不符 → false
--（fail-closed）。search_path 锁定 pg_catalog。
CREATE FUNCTION public.tw_sig_match()
RETURNS boolean
LANGUAGE sql STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    SELECT COALESCE((
        SELECT sig.exp ~ '^[0-9]+$'
           AND (sig.exp)::bigint >= (EXTRACT(epoch FROM now()))::bigint
           AND EXISTS (
               SELECT 1 FROM public.tw_secrets k
               WHERE k.purpose = 'tw-roles-guc-v1'
                 AND encode(public.hmac(
                       COALESCE(current_setting('app.tenant', true), '') || '|' ||
                       current_setting('app.roles', true) || '|' || sig.exp,
                       k.key_hex, 'sha256'), 'hex') = sig.mac
           )
        FROM (SELECT split_part(current_setting('app.roles_sig', true), '|', 1) AS exp,
                     split_part(current_setting('app.roles_sig', true), '|', 2) AS mac) sig
    ), false)
$$;

-- tw_roles()：SECURITY DEFINER 验签函数。仅 current_setting('role') = 'tw_app'
-- 走验签（SECURITY DEFINER 只切 current_user，不切 role setting）；其余身份
-- 直接零角色。sig 缺失/格式错/过期/验签失败/密钥缺失 → 空数组 = 零角色
--（fail-closed，与漏注入同语义）。app.roles/app.tenant GUC 可被任何持
-- SQL 会话者 set_config 伪造，验签后伪造通道封死。sig 格式
-- "<exp_unix>|<hexmac>"，窗口 60s（DB 时钟偏差容差）。
CREATE OR REPLACE FUNCTION public.tw_roles()
RETURNS text[]
LANGUAGE sql STABLE PARALLEL SAFE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    SELECT CASE
        WHEN current_setting('role', true) <> 'tw_app' THEN ARRAY[]::text[]
        WHEN COALESCE(current_setting('app.roles', true), '') = '' THEN ARRAY[]::text[]
        WHEN public.tw_sig_match() THEN string_to_array(current_setting('app.roles', true), chr(31))
        ELSE ARRAY[]::text[]
    END
$$;

-- tw_tenant()：同一 sig 的租户解包——验签通过返回 app.tenant 的 bigint，
-- 否则 NULL（供 tw_set_document_acl 强制 p_tenant 绑定）。
CREATE FUNCTION public.tw_tenant()
RETURNS bigint
LANGUAGE sql STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    SELECT CASE
        WHEN current_setting('role', true) <> 'tw_app' THEN NULL
        WHEN current_setting('app.tenant', true) ~ '^-?[0-9]+$' AND public.tw_sig_match()
            THEN (current_setting('app.tenant', true))::bigint
        ELSE NULL
    END
$$;

-- RLS 判定单源函数（redesign §3.2/§3.3/§4.3）：装 public（集群级，随 catalog
-- 同库）；宿主是每集合表的 policy 与 golden 测试矩阵（CI 锁语义，禁止 Go 侧
-- 等价实现）。
--
-- 语义锚点（= AllowsDocumentAccess 用户集合分支，只换执行点不动模型）：
--   tw_can = ACE 命中（read 仅 'typ:role'；create/update/delete 同时匹配
--            'write:role'——matchTypes 的 write 展开）∨ 空 _acl 回退集合级（B1）
--   tw_visible = tw_can('read') ∨ tw_can('update') ∨ tw_can('delete')
--           （"可写即可读"产品语义，§3.2 已决）；docSec=false 纯集合级
--            （AllowsDocumentAccess 的 !DocumentSecurity 分支忠实保留）
--   roles 缺省（NULL/''）→ 零角色可匹配 → 恒 false（fail-closed）
CREATE OR REPLACE FUNCTION public.tw_can(acl text[], roles text[], typ text, coll_allows boolean)
RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE
AS $$
    SELECT COALESCE(EXISTS (
               SELECT 1
               FROM unnest(acl) ace
               CROSS JOIN LATERAL unnest(roles) r
               WHERE ace = typ || ':' || r
                  OR (typ <> 'read' AND ace = 'write:' || r)
           ), false)
        OR COALESCE(cardinality(acl) = 0 AND cardinality(roles) > 0 AND coll_allows, false)
$$;

-- tw_coll_allows 是集合级权限判定（catalog permissions JSONB，type:role 对）：
-- write 展开与 tw_can 同源（typ∈create/update/delete 同时命中 'write'）。
CREATE OR REPLACE FUNCTION public.tw_coll_allows(perms jsonb, roles text[], typ text)
RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE
AS $$
    SELECT COALESCE((
               SELECT bool_or(
                      ((p ->> 'type' = typ)
                          OR (typ <> 'read' AND p ->> 'type' = 'write'))
                      AND (p ->> 'role' = ANY (roles)))
               FROM jsonb_array_elements(perms) p
           ), false)
$$;

-- tw_visible 是 SELECT policy 的可见谓词（可写即可读）。docsec=false 时 ACE
-- 不参与（AllowsDocumentAccess 的集合级独占分支），集合级写权同样蕴含可见。
-- 空 _acl 快速路径先行短路（等价于三次 tw_can 的空回退析取），免每行函数
-- 调用——大集合全扫的相对基准由它压住（I1）。
CREATE OR REPLACE FUNCTION public.tw_visible(
    acl text[], roles text[], docsec boolean,
    coll_read boolean, coll_update boolean, coll_delete boolean)
RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE
AS $$
    SELECT CASE
        WHEN docsec THEN
               COALESCE(cardinality(acl) = 0 AND cardinality(roles) > 0
                   AND (coll_read OR coll_update OR coll_delete), false)
            OR public.tw_can(acl, roles, 'read', coll_read)
            OR public.tw_can(acl, roles, 'update', coll_update)
            OR public.tw_can(acl, roles, 'delete', coll_delete)
        ELSE coll_read OR coll_update OR coll_delete
    END
$$;

-- 默认 ACL 是 EXECUTE TO PUBLIC：SECURITY DEFINER 函数必须收紧。tw_app 是
-- policy 表达式的执行身份（tw_roles）；tw_system 是 tw_set_document_acl 的
-- definer（其函数体内调用 tw_sig_match/tw_tenant/tw_roles 按 definer 检查）。
-- tw_tenant 同授 tw_app（只读验签探针：返回验签 tenant 或 NULL，无信息泄露
-- ——调用方本就知道自己注入的 tenant）。tw_sig_match 不直接授 tw_app
--（经 tw_roles/tw_tenant 间接可达）。
REVOKE ALL ON FUNCTION public.tw_sig_match() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.tw_tenant() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.tw_roles() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.tw_roles() TO tw_app;
GRANT EXECUTE ON FUNCTION public.tw_tenant() TO tw_app;
GRANT EXECUTE ON FUNCTION public.tw_sig_match() TO tw_system;
GRANT EXECUTE ON FUNCTION public.tw_tenant() TO tw_system;
GRANT EXECUTE ON FUNCTION public.tw_roles() TO tw_system;

-- tw_set_document_acl：_acl 删改（UPDATE/REPLACE）唯一通道。owner 需为
-- BYPASSRLS 角色才能绕开 FORCE RLS 的 SELECT policy 新行复检（自锁规避）——
-- 建为迁移执行者后 ALTER OWNER TO tw_system（tw_system 无 CREATE ON public，
-- 迁移显式补授；tw_system 是 NOLOGIN 的信任根角色，该授权不扩大可达面）。
--   p_table 经 catalog physical_name 白名单校验（防注入）；
--   p_tenant 必须等于验签 tenant（跨租户/跨项目在签名层死锁，不满足 RETURN 0）；
--   目标行 tw_visible 可见性校验（堵项目内改他人 ACL 提权读；"可写即可读"
--   保证合法路径权限面 ⊆ tw_visible 面）；不可见/行不存在均 RETURN 0。
GRANT CREATE ON SCHEMA public TO tw_system;
CREATE FUNCTION public.tw_set_document_acl(
    p_schema text, p_table text, p_tenant bigint, p_doc text, p_acl text[]
) RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_docsec boolean;
    v_perms jsonb;
    v_acl text[];
    v_sigtenant bigint;
    n integer;
BEGIN
    v_sigtenant := public.tw_tenant();
    IF v_sigtenant IS NULL OR v_sigtenant <> p_tenant THEN
        RETURN 0;
    END IF;
    SELECT cc.document_security, cc.permissions INTO v_docsec, v_perms
    FROM public.catalog_collections cc WHERE cc.physical_name = p_table;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown collection table: %', p_table USING ERRCODE = '42704';
    END IF;
    EXECUTE format('SELECT "_acl" FROM %I.%I WHERE "_id" = $1 AND "_tenant" = $2', p_schema, p_table)
    INTO v_acl USING p_doc, p_tenant;
    IF v_acl IS NULL THEN
        RETURN 0;
    END IF;
    IF NOT public.tw_visible(v_acl, public.tw_roles(), v_docsec,
            public.tw_coll_allows(v_perms, public.tw_roles(), 'read'),
            public.tw_coll_allows(v_perms, public.tw_roles(), 'update'),
            public.tw_coll_allows(v_perms, public.tw_roles(), 'delete')) THEN
        RETURN 0;
    END IF;
    EXECUTE format(
        'UPDATE %I.%I SET "_acl" = $1 WHERE "_id" = $2 AND "_tenant" = $3',
        p_schema, p_table)
    USING p_acl, p_doc, p_tenant;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;
ALTER FUNCTION public.tw_set_document_acl(text, text, bigint, text, text[]) OWNER TO tw_system;
REVOKE ALL ON FUNCTION public.tw_set_document_acl(text, text, bigint, text, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.tw_set_document_acl(text, text, bigint, text, text[]) TO tw_app;

-- 密钥面收口（B15）：运行 DSN（tw_authenticator）对 public.tw_secrets 零权限
-- ——authenticator DSN（非 superuser、受 RLS）若泄漏，四权授予使其可读
-- roles_sig 密钥 → 伪造 GUC 提权；REVOKE 后 DSN 泄漏读不到密钥、写不了槽位，
-- 伪造通道封死。DO 块守护：tw_authenticator 是运维手工创建的集群级角色
--（13-operations §4.5 引导 SQL ①），未引导的库/测试库中不存在，跳过即保持
-- 零权限形态。
DO $do$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tw_authenticator') THEN
        REVOKE ALL ON public.tw_secrets FROM tw_authenticator;
    END IF;
END $do$;
