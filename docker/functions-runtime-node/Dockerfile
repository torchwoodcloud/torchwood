# Torchwood 契约基础镜像（functions runtime：node）
#
# 用途：BYO（bring your own image）契约镜像基座——用户 Dockerfile `FROM`
# 本镜像后 COPY 自己的代码即得到符合 Runner 协议的函数镜像（协议契约全文：
# docs/developer/08-functions.md §3.2「Runner 协议（契约镜像规范）」；本镜像
# 预置的 .tw-runner.js 即协议的 node 参考实现，源码
# internal/infra/functions/runner/runner.js）。
#
# 构建上下文 = 仓库根（runner.js 源在 internal/ 下，必须在仓库根执行）：
#   docker build -f docker/functions-runtime-node/Dockerfile -t <registry>/functions-runtime-node:1 .
# 推送属 ops 动作（不阻塞代码交付）：
#   docker push <registry>/functions-runtime-node:1
FROM node:18-alpine

WORKDIR /app

# 预置平台 runner（仅 COPY 单文件——example/ 等上下文内其他文件不进镜像）。
# 启动即同步 require 用户模块（约定 /app/index.js，导出 main 或 fetch），
# 加载完成前 GET /_tw/health 返回 not-ready。
COPY internal/infra/functions/runner/runner.js /app/.tw-runner.js

# Runner 协议监听端口（容器内 :18080，仅 per-project 网络可达）。
ENV TW_RUNNER_PORT=18080

USER node

CMD ["node", ".tw-runner.js"]
