-- 000013 对称回滚：先删带 CHECK 约束的 kind 列，再删 declared_scopes。

ALTER TABLE {{schema}}.function_variables
    DROP CONSTRAINT IF EXISTS function_variables_kind_check;

ALTER TABLE {{schema}}.function_variables
    DROP COLUMN IF EXISTS kind;

ALTER TABLE {{schema}}.functions
    DROP COLUMN IF EXISTS declared_scopes;
