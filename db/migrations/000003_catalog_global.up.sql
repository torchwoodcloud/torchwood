-- 全局 catalog（基线重定 2026-09-06：000025/000032 合并；redesign §4.2 / C1 /
-- G1，阶段②包 A）。catalog 定位 cluster 内全局，POC 单集群即 public。
-- attrs/indexes/permissions 以 JSONB 列合一（预决策 1）：GetCollection 热路径
-- 从 3 查询收敛为 1，default_value 等全量属性契约以 catalog 为唯一源。

CREATE TABLE catalog_databases (
    project_id  TEXT NOT NULL REFERENCES public.projects(id) ON DELETE CASCADE,
    database_id TEXT NOT NULL,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, database_id),
    UNIQUE (project_id, name)
);

CREATE TABLE catalog_collections (
    project_id        TEXT NOT NULL,
    database_id       TEXT NOT NULL,
    collection_id     TEXT NOT NULL,
    name              TEXT NOT NULL,
    -- 物理名服务端分配（c_<base32(8)>，全局唯一）——内部实现细节，不出现在
    -- 任何 API 响应；sentinel 系统集合物理名 = 逻辑名（静态表不可改名）。
    physical_name     TEXT NOT NULL,
    document_security BOOLEAN NOT NULL DEFAULT TRUE,
    disabled          BOOLEAN NOT NULL DEFAULT FALSE,
    is_system         BOOLEAN NOT NULL DEFAULT FALSE,
    -- JSONB 合一（预决策 1）：attrs 含 key/type/size/required/array/default/options
    -- 全量契约；indexes 含 id/type/attributes/orders；permissions 为 type:role 对。
    permissions       JSONB NOT NULL DEFAULT '[]'::jsonb,
    attrs             JSONB NOT NULL DEFAULT '[]'::jsonb,
    indexes           JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- schema_version 仅立列，演进状态机语义挂账 redesign §4.6；
    -- ddl_seq 是元数据写路径的乐观锁（CAS 递增，redesign §4.4）。
    schema_version    BIGINT NOT NULL DEFAULT 1,
    ddl_seq           BIGINT NOT NULL DEFAULT 1,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, database_id, collection_id),
    UNIQUE (project_id, database_id, name),
    FOREIGN KEY (project_id, database_id)
        REFERENCES catalog_databases (project_id, database_id) ON DELETE CASCADE
);

-- 物理名唯一性：分配名（c_<base32>）全局唯一；sentinel 系统集合的物理名 =
-- 静态表名（tw_<p>.users，schema 内局部），跨项目必然同名，排除在约束外。
-- 部分唯一索引承载该语义（约束名即 23505 判别键）。
CREATE UNIQUE INDEX uq_catalog_collections_physical_name
    ON catalog_collections (physical_name)
    WHERE database_id <> '_';

-- ListCollections 按 (project_id, database_id) 过滤 + created_at DESC 排序。
CREATE INDEX idx_catalog_collections_db_created
    ON catalog_collections (project_id, database_id, created_at DESC);

-- catalog_migrations：schema 演进 copy 迁移任务账本（转出 POC 门禁 B4，redesign
-- §4.6 / 预决策 3）。改类型/收紧 = 新列（物理名带版本后缀）→ 异步批量回填
-- （批 500 行、限速、游标可恢复）→ 锁窗校验 → 原子 swap（RENAME 列）→ 旧列
-- deprecated。任务行承载：进度（cursor_id/rows_done）、阶段
--（backfilling|swapped|retired|failed）、swap 后旧列的物理名（old_physical
-- —— retired 时 DROP 的目标）。schema_version 在 swap commit 时递增。
CREATE TABLE catalog_migrations (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    database_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    attr_key TEXT NOT NULL,
    from_attr JSONB NOT NULL,
    to_attr JSONB NOT NULL,
    old_physical TEXT NOT NULL,
    new_physical TEXT NOT NULL,
    phase TEXT NOT NULL DEFAULT 'backfilling',
    cursor_id TEXT,
    rows_done BIGINT NOT NULL DEFAULT 0,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 进行中任务的寻址索引（MigrateAttribute 重入 / 运维观测按集合定位）。
CREATE INDEX idx_catalog_migrations_pending
    ON catalog_migrations (project_id, database_id, collection_id, attr_key)
    WHERE phase = 'backfilling';

-- 角色授权（tw_owner 写任务账本；tw_system 读任务行并执行回填数据语句——
-- 迁移期数据访问 = 运维面，BYPASSRLS 身份执行）随 000004 RBAC 一次性落位
--（三角色在 000004 才创建，本迁移只建结构）。
