-- Functions 部署源多元化（docs/design/functions-runtimes-and-sources.md
-- §0/§2，二期阶段 1/4）：function_deployments 源快照五列。
--   source_type   部署源类型，词表 zip|git|image（image 三期启用）；
--   source_url    git 仓库 url（zip 恒空串）；
--   source_ref    git 钉死 commit SHA（zip 恒空串）；
--   source_dir    git 构建上下文子目录（zip 恒空串）；
--   context_sha256 物化 zip sha256（zip/git 共用的可复现性锚，D9）。
-- 四件 source_url + source_ref + source_dir + context_sha256 = 不可变快照锚。
-- 存量行全部为 zip 源：五列带 DEFAULT，ALTER 即回填，无需数据回填。
-- source 列 INSERT 期写全、之后不可变：不登记进 UpdateDeployment 列白名单
-- （bun 更新写规范：不可变列漏登记正是期望行为）。
-- 凭证（GitSource.username/token）一次性内联，任何列不落库（D8）。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

ALTER TABLE {{schema}}.function_deployments
    ADD COLUMN source_type TEXT NOT NULL DEFAULT 'zip'
        CONSTRAINT function_deployments_source_type_check CHECK (source_type IN ('zip', 'git', 'image')),
    ADD COLUMN source_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_dir TEXT NOT NULL DEFAULT '',
    ADD COLUMN context_sha256 TEXT NOT NULL DEFAULT '';
