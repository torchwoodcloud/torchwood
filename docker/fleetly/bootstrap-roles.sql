-- tw_authenticator 授权补齐（docs/developer/13-operations.md §4.5 ②③④④'）。
-- **本文件是 docker/dokploy/bootstrap-roles.sql 的 fleetly 形态副本（IMPL-T2-4）**：
-- 上传为平台 Config 资源（fleetly configs set torchwood bootstrap-roles.sql
-- --from-file docker/fleetly/bootstrap-roles.sql），由 db-bootstrap init job 执行
-- （psql -v dbname=<托管库名> -f）。与 docker/dokploy/ 侧保持同内容：变更两处同改
-- （dokploy 栈退役后本文件为唯一真源）。托管实例无 initdb 钩子创建 tw_authenticator，
-- 该步由同目录 bootstrap-runtime.sql（db-bootstrap 作业的同一份 configs 挂载）承接。
-- 前置：迁移已执行到 000004（tw_owner/tw_app/tw_system 三角色已存在），
--       tw_authenticator 已由 bootstrap-runtime.sql 创建。
-- 幂等性：GRANT 重复执行为 no-op；新表补授由 DO 块逐表 GRANT（重复 GRANT 同权亦为 no-op）。
-- 执行身份：owner/引导账号（compose 的 db-bootstrap 一次性作业）。

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
