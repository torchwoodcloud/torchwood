-- RuntimeVars：项目级运行时配置下发（docs/design/runtime-vars.md §2.2，D1）。
-- 放 public 控制面而非项目数据面：runtimeVars 是项目级配置元数据，三个访问
-- 者（Console / Server API / Client SDK）都不走文档 RLS；项目删除经
-- FK CASCADE 自动传导（与 catalog_databases 同构，同样不进
-- DeleteProjectControlPlaneRows）。
--
-- 版本链（D10/D11）：heads.revision 是集合级严格单调版本号（= etag 成分、
-- = 版本链寻址号）；runtime_var_versions 存全列快照（非 diff），回滚 = 整替。

CREATE TABLE runtime_var_sets (
    project_id  TEXT NOT NULL REFERENCES public.projects(id) ON DELETE CASCADE,
    var_set_id  TEXT NOT NULL,             -- ^[a-z_][a-z0-9_]*$ ≤40，创建后不可改
    visibility  TEXT NOT NULL DEFAULT 'public',   -- 'public' | 'private'
    epoch       TEXT NOT NULL,             -- 创建时随机（8 字节 hex）：对外 etag
                                           -- 的防碰撞因子（D2：同名删除重建
                                           -- revision 归零，epoch 使旧 etag 必失效）
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, var_set_id)
);

CREATE TABLE runtime_vars (
    project_id  TEXT NOT NULL,
    var_set_id  TEXT NOT NULL,
    key         TEXT NOT NULL,             -- ^[a-z_][a-z0-9_]*$ ≤64
    value_type  TEXT NOT NULL,             -- 'string'|'integer'|'float'|'boolean'|'json'
    value       JSONB NOT NULL,            -- 标准化 JSON 值（标量即 JSON 标量）；
                                           -- value_type 为类型锚点列（D3）
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, var_set_id, key),
    FOREIGN KEY (project_id, var_set_id)
        REFERENCES runtime_var_sets (project_id, var_set_id) ON DELETE CASCADE
);

-- heads 行与集合同生共死（CreateVarSet 同事务建立，删除集合经 CASCADE 连带），
-- 行不存在视同 revision 0。写事务先 SELECT ... FOR UPDATE 锁此行，串行化同
-- 集合并发写（§2.5）。
CREATE TABLE runtime_var_heads (
    project_id TEXT NOT NULL,
    var_set_id TEXT NOT NULL,
    revision   BIGINT NOT NULL DEFAULT 0,  -- 集合级严格单调；= 版本链寻址号
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, var_set_id),
    FOREIGN KEY (project_id, var_set_id)
        REFERENCES runtime_var_sets (project_id, var_set_id) ON DELETE CASCADE
);

-- 版本行：revision = 快照时点的 heads.revision（一鱼两吃）；vars 为全量快照
-- {key: {t, v, d, c}}（D11 复查修正：全列四元组，回滚重插时 description/
-- created_at 取快照值，元数据不丢失）。保留窗口 50 版/集合由写路径淘汰，
-- 迁移层不设约束。
CREATE TABLE runtime_var_versions (
    project_id TEXT NOT NULL,
    var_set_id TEXT NOT NULL,
    revision   BIGINT NOT NULL,            -- = 快照时点的 heads.revision，一鱼两吃
    vars       JSONB NOT NULL,             -- 全量快照 {key: {t, v, d, c}}
    action     TEXT NOT NULL,              -- 'create'|'update'|'delete'|'rollback'
    summary    TEXT NOT NULL DEFAULT '',   -- 如 "update: a, b" / "rollback to 42"
    actor      TEXT NOT NULL,              -- admin:<id> / apikey:<id> / function:<id>
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, var_set_id, revision),
    FOREIGN KEY (project_id, var_set_id)
        REFERENCES runtime_var_sets (project_id, var_set_id) ON DELETE CASCADE
);
