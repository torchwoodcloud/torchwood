# functions-runtime-node：Torchwood 契约基础镜像（BYO 函数镜像基座）

Torchwood functions 的 BYO（bring your own image）镜像源（三期，设计
`docs/design/functions-runtimes-and-sources.md` §3）要求函数镜像实现
**Runner 协议**——协议契约全文见
`docs/developer/08-functions.md` §3.2「Runner 协议（契约镜像规范）」。
本目录交付随仓的**契约基础镜像**：用户 `FROM` 本镜像后 COPY 自己的代码，
即得到符合协议的函数镜像，无需自行实现协议。

- 预置的 `/app/.tw-runner.js` 是协议的 node 参考实现（源码
  `internal/infra/functions/runner/runner.js`，与 Go 平台 bootstrap 共享
  单一 `RunnerTemplateVersion` 同版本纪律）；
- runner 启动即同步加载用户模块（约定 `/app/index.js`，导出 `main` 或
  `fetch`，优先级 fetch > main），加载完成前 `GET /_tw/health` 返回
  not-ready；
- 监听容器内 `:18080`（env `TW_RUNNER_PORT` 可配），仅 per-project 网络
  可达；`TW_MAX_REQUESTS` 自回收、SIGTERM drain（`TW_DRAIN_TIMEOUT_MS`
  兜底）。

## 构建

构建上下文 = **仓库根**（runner.js 源在 `internal/` 下）：

```bash
docker build -f docker/functions-runtime-node/Dockerfile -t <registry>/functions-runtime-node:1 .
```

推送属 ops 动作（不阻塞代码交付）：

```bash
docker push <registry>/functions-runtime-node:1
```

## 用户侧用法（BYO）

最小示例见 [`example/`](example/)（`exports.main = (data, ctx) => ...`，
不参与本镜像自身的构建）。用户 Dockerfile 只需两行：

```dockerfile
FROM <registry>/functions-runtime-node:1
COPY index.js /app/index.js
```

推送自己的镜像后即可走镜像源部署（runtime=image 的函数）：

```bash
./bin/torchwood functions create --id greet --name greet --runtime image
./bin/torchwood functions deployments create-from-image greet \
  --image <registry>/my-function:1
```

## 部署前本地自测（Lambda RIE 等效物）

契约镜像可直接 `docker run` 起本地对照，推送前即可自测 `:18080` 契约
符合性：

```bash
docker run --rm -p 18080:18080 my-function:dev
curl http://127.0.0.1:18080/_tw/health
curl -X POST -d '{"hello":"world"}' http://127.0.0.1:18080/
```

平台参考实现：node `internal/infra/functions/runner/runner.js`、Go
`internal/infra/functions/runner/gorunner/`（渲染进 twmain 的
`assets/runtime.go.tmpl`）。
