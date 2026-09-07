#!/bin/bash
# 数据卷首次 initdb 时由 postgres 入口脚本执行（docker-entrypoint-initdb.d）：
# 创建运行态非 superuser 登录账号 tw_authenticator（docs/developer/13-operations.md §4.5 ①）。
# 三角色 membership 等授权依赖迁移 000004 创建的角色，由 db-grants 作业（bootstrap-roles.sql）在迁移后补齐。
# 注意：仅在 postgres_data 卷为空、首次建库时执行；已初始化的卷不会再跑。
set -euo pipefail
: "${TORCHWOOD_AUTH_PASSWORD:?TORCHWOOD_AUTH_PASSWORD 未设置}"
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
CREATE ROLE tw_authenticator LOGIN PASSWORD '$TORCHWOOD_AUTH_PASSWORD'
    NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION;
EOSQL
