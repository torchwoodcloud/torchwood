-- 逆序对称回滚：函数随 policy 体系退役（各集合表的 policy/RLS 由
-- DeleteCollection 的 DROP TABLE 与 reconcile 路径自行清理，本迁移只撤函数）；
-- tw_secrets 随表回收；pgcrypto 无其他依赖对象（失败即暴露）。
-- 集群角色保留约定（A6/A8）：tw_owner/tw_app/tw_system 是集群级对象、被同集群
-- 多个库共享，本 down 只回滚"本库作用域"——对象所有权回 authenticator →
-- 撤销三角色在本库的权限 → 撤 authenticator membership。不执行 DROP ROLE：
-- 角色依赖跨库不可见，多库/并行场景（go test -p N 的并行测试库中 tw_owner
-- 名下有表/函数）DROP ROLE 会撞 SQLSTATE 2BP01；且角色保留不影响 down→up
-- 重放（创建幂等）。角色生命周期归集群供给方（DBA/部署）处置。
-- 逐角色分支容错：部分角色缺失（异常手工态）时其余仍可清理。

DROP FUNCTION IF EXISTS public.tw_set_document_acl(text, text, bigint, text, text[]);
DROP FUNCTION IF EXISTS public.tw_tenant();
DROP FUNCTION IF EXISTS public.tw_sig_match();
DROP FUNCTION IF EXISTS public.tw_visible(text[], text[], boolean, boolean, boolean, boolean);
DROP FUNCTION IF EXISTS public.tw_coll_allows(jsonb, text[], text);
DROP FUNCTION IF EXISTS public.tw_can(text[], text[], text, boolean);
DROP FUNCTION IF EXISTS public.tw_roles();
REVOKE CREATE ON SCHEMA public FROM tw_system;

DROP TABLE IF EXISTS public.tw_secrets;
DROP EXTENSION IF EXISTS pgcrypto;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tw_owner') THEN
        EXECUTE format('REASSIGN OWNED BY tw_owner TO %I', current_user);
        EXECUTE 'DROP OWNED BY tw_owner';
        EXECUTE 'REVOKE tw_owner FROM CURRENT_USER';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tw_app') THEN
        EXECUTE format('REASSIGN OWNED BY tw_app TO %I', current_user);
        EXECUTE 'DROP OWNED BY tw_app';
        EXECUTE 'REVOKE tw_app FROM CURRENT_USER';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tw_system') THEN
        EXECUTE format('REASSIGN OWNED BY tw_system TO %I', current_user);
        EXECUTE 'DROP OWNED BY tw_system';
        EXECUTE 'REVOKE tw_system FROM CURRENT_USER';
    END IF;
END
$$;
