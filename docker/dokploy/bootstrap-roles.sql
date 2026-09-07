-- tw_authenticator 授权补齐（docs/developer/13-operations.md §4.5 ②③④④'）。
-- 前置：迁移已执行到 000004（tw_owner/tw_app/tw_system 三角色已存在），
--       tw_authenticator 已由 initdb/01-authenticator.sh 创建。
-- 幂等性：GRANT 重复执行为 no-op；新表补授由 DO 块逐表 GRANT（重复 GRANT 同权亦为 no-op）。
-- 执行身份：owner/引导账号（compose 的 db-grants 一次性作业）。

-- ② 000004 授权面：三角色 membership（每请求 SET LOCAL ROLE 的变色龙源头）
GRANT tw_owner, tw_app, tw_system TO tw_authenticator;

-- ③ 库级权限：CONNECT + CREATE（项目 schema 供给）+ public schema USAGE
GRANT CONNECT, CREATE ON DATABASE :"dbname" TO tw_authenticator;
GRANT USAGE ON SCHEMA public TO tw_authenticator;

-- ④ 控制面静态表 DML（边界邻居面）：public 全表排除 catalog 两表（仅经角色可达）
--    与 tw_secrets（运行账号对密钥表零权限，永不授予）
DO $do$ DECLARE t text; BEGIN
    FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'public'
        AND tablename NOT IN ('catalog_databases', 'catalog_collections', 'tw_secrets')
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO tw_authenticator', t);
    END LOOP;
END $do$;

-- ④' projectschema 静态迁移的 FK 面（REFERENCES public.projects）
GRANT REFERENCES ON public.projects TO tw_authenticator;

-- 后续新迁移新增 public 表时需以 owner 身份补授；可预建默认授权：
-- ALTER DEFAULT PRIVILEGES FOR ROLE <owner 引导账号> IN SCHEMA public
--   GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO tw_authenticator;
