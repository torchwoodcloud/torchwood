-- Functions 多机执行面·构建亲和（docs/design/functions-runtimes-and-sources.md
-- §4 M5，四期 4a-1）：function_deployments 新增 build_node 列。
--   build_node     首个构建落成的 dispatcher 节点 ID（节点注册表
--                  torchwood:fnnodes:* 的 node_id）；local 路由模式下执行/
--                  补构建固定路由该节点（4a-2 消费）。存量行回填空串 =
--                  无亲和（单机时代产物，任意本机 dispatcher 等价）。
-- 与 source 快照五列（迁移 000023）不同：build_node 是构建后写入的**操作
-- 列**（成功路径经 UpdateDeployment 落库，列白名单已登记），非 INSERT 期
-- 不可变快照。节点死 + 构建亲和丢失的降级语义（重部署）见设计 §4 M5。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

ALTER TABLE {{schema}}.function_deployments
    ADD COLUMN build_node TEXT NOT NULL DEFAULT '';
