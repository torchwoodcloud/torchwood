-- P0 执行身份（docs/design/functions-execution-identity-and-triggers.md §1/§2）：
-- functions.declared_scopes 是函数执行 principal 的平台访问声明（默认空 =
-- 无平台访问权限，fail-closed）；function_variables.kind 区分普通文本与
-- 第三方密钥（密钥缝 PC-6）。占位符 {{schema}} 由 Apply 替换为 quoteIdent。

ALTER TABLE {{schema}}.functions
    ADD COLUMN declared_scopes TEXT[] NOT NULL DEFAULT '{}';

ALTER TABLE {{schema}}.function_variables
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'text'
    CONSTRAINT function_variables_kind_check CHECK (kind IN ('text', 'secret'));
