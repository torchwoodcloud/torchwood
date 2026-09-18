# functions-hello：镜像部署源的最小可运行示例
# （CI 自动构建发布：ghcr.io/torchwoodcloud/functions-hello）。
#
# 生产用户形态：FROM 官方契约基座 + COPY 自己的 index.js
# （基座预置 Runner 协议的 node 参考实现，契约全文：
# docs/developer/08-functions.md §3.2；基座 Dockerfile：
# docker/functions-runtime-node/Dockerfile）。
FROM ghcr.io/torchwoodcloud/functions-runtime-node:1

# 用户代码（runner 启动即 require /app/index.js，导出 main 或 fetch）
# 路径相对仓库根：CI/本地构建上下文均为仓库根（与基座同构）。
COPY docker/functions-hello/app/index.js /app/index.js
