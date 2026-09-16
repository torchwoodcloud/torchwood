-- RuntimeVars 回滚：DROP 四表（逆序，先删 FK 引用方）。
-- down 前若已有真实使用，先经 server API 导出 JSON 存档（设计文档 §回滚）。
DROP TABLE IF EXISTS runtime_var_versions;
DROP TABLE IF EXISTS runtime_var_heads;
DROP TABLE IF EXISTS runtime_vars;
DROP TABLE IF EXISTS runtime_var_sets;
