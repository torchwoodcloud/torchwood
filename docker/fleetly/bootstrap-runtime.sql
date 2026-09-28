-- fleetly 托管实例引导（IMPL-T2-4）：承接 docker/dokploy/initdb/01-authenticator.sh
-- 的「运行态登录账号」职责，并附加托管模板缺失 initdb 编码参数时的连接编码兜底。
--
-- 执行形态：平台 Config 资源 + db-bootstrap init job 的 psql 调用，psql 变量
-- 注入（-v dbname=... -v auth_password=..."$TORCHWOOD_AUTH_PASSWORD"）——
-- 口令不落本文件/仓库/日志。幂等：可随每次发布重跑。
--
-- ① 连接编码兜底（**重要，本地实证见 cutover-runbook.md 附录 A**）：
--    fleetly 的 percona-postgresql-18 模板未携带 POSTGRES_INITDB_ARGS，percona
--    镜像 locale 为 POSIX → 实例初始化即 server_encoding=SQL_ASCII，连接缺省
--    client_encoding 随之为 SQL_ASCII；torchwood 运行时的 pgdriver 客户端在
--    握手期强制要求 client_encoding=UTF8（standard_conforming_strings=on），
--    否则连接直接失败（roles-sig/同步作业亦同）。
--    数据库级缺省设置使**新连接**的 client_encoding=UTF8（现存连接不受影响）。
--    边界诚实声明：server_encoding 仍是 SQL_ASCII——PG 对 SQL_ASCII 库不做
--    字符集转换，UTF-8 字节流原样存取（与现役 dokploy `--locale=C --encoding=UTF8`
--    的字节序表象一致，但 length()/upper() 等字符函数按字节计）；彻底修复 =
--    平台模板补 `POSTGRES_INITDB_ARGS=--locale=C --encoding=UTF8` 并重建实例
--    （fleetly 侧改动，本仓无法承载，建议见 cutover-runbook.md §9）。
ALTER DATABASE :"dbname" SET client_encoding TO 'UTF8';

-- ② 运行态非 superuser 登录账号（属性集与 initdb/01-authenticator.sh 逐字一致：
--    NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION；三角色
--    membership 与库表授权由 bootstrap-roles.sql 在迁移后补齐）。
--    口令每次重写 = 与平台 env TORCHWOOD_AUTH_PASSWORD 对齐（轮换路径：
--    改平台 env + 重新部署，本作业即重写数据库侧口令）。
DO $do$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tw_authenticator') THEN
    CREATE ROLE tw_authenticator LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION;
  END IF;
END
$do$;
ALTER ROLE tw_authenticator WITH LOGIN PASSWORD :'auth_password';
