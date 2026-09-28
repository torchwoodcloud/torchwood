# IMPL-T2-3/T2-5 本地 dind 端到端（守卫①/②）

两形态执行底座的本地等价集成（IMPL-T2-5：fleetly 形态 + docker 直接执行形态）。

## fleetly 形态（fleetly-dind.sh）

真实 fleetlyd + 真实 swarm 下跑通 dispatcher 的执行底座路径：

```
fleetly-dind.sh（宿主）
  ├─ 交叉编译 fleetlyd/fleetly（FLEETLY_REPO）与 dispatcher e2e 测试二进制
  ├─ 起特权 dind（发布 127.0.0.1:18420 → dind 8420 供宿主注册/铸令牌）
  ├─ in-e2e-boot.sh：swarm init + fleetlyd 起服（REST/gRPC 0.0.0.0）
  ├─ 宿主注册 founder + 铸机具令牌（scope tasks,build,read；只落 run 目录——
  │    read 供编排后回收任务日志，tasks/build 为被测 dispatcher 的真实面）
  └─ in-e2e-run.sh：
       ├─ dind 内构建 driver 镜像（alpine + e2e.test；平台经 local inspect 解析）
       ├─ `fleetly tasks network ensure pe2e`
       ├─ `fleetly tasks run` 把编排器作为任务跑在任务网络内：
       │    TestE2EFleetlyTaskLifecycle（真实 fleetly 客户端 + 真实池）
       │      EnsureProjectNetwork → Dispatch 冷启动 → fleetly CreateTask
       │      （spawn）→ /_tw/health 握手 → 请求分发 ×2 → TW_MAX_REQUESTS
       │      自退 → 任务 stopped/exited（平台回收）→ reaper 幽灵清理 →
       │      Stop/Delete 幂等回收
       └─ 收集任务日志断言 `E2E PASS`
```

函数实例由同一镜像以缺省 ENTRYPOINT 拉起（`TestE2ERunnerServer`：runner
契约双，`/_tw/health` + 分发封套 + TW_MAX_REQUESTS 达阈 `os.Exit`）。

### 运行

```sh
# Git Bash，torchwood 仓库根
FLEETLY_REPO=/d/Codes/qiulin/fleetly sh dispatcher/testdata/e2e/fleetly-dind.sh
```

产物落 `artifacts/<run-id>/`（summary/boot/inner 日志与任务日志；`token.txt`
是机具令牌，勿外传）。退出码 0 = `E2E-RESULT: PASS`。

### 边界（如实登记）

- 覆盖：网络 ensure、任务 spawn（CreateTask + 平台收敛）、DNS 名寻址、
  health 握手、分发封套、TW_MAX_REQUESTS 自退、平台回收、显式 Stop/Delete。
- 不覆盖（由单测/T2-2 真机探针承接）：`BuildImage → BuildFromUpload` 真实
  构建腿（本 e2e 用 dind 内 `docker build` 的本地镜像 + 映射种子）、
  internal 变体出网封死（T2-1 探针）、跨节点调度（staging 待验）。
- staging 真机版（对真实 fleetly 控制面 + 多节点）见 T2-4 割接窗口。

## docker 底座形态（docker-driver-e2e.sh，IMPL-T2-5）

单特权 dind 直跑（零 fleetlyd/swarm/令牌——docker 底座直接对 dind 的
docker.sock 操作）：

```
docker-driver-e2e.sh（宿主）
  ├─ 交叉编译 dockerdriver 的 e2e 测试二进制（go test -c ./dispatcher/dockerdriver）
  ├─ 起特权 dind（无端口发布——全程容器内闭环）
  ├─ dind 内构建 driver 镜像（alpine + e2e.test；逻辑名即本地 tag，零映射种子）
  └─ `docker run` 把编排器作为容器跑起来（挂载 dind 的 docker.sock）：
       TestE2EDockerDriverLifecycle（真实 docker daemon 客户端 + 真实池）
         EnsureProjectNetwork（创建 tw-func-e2e + 自 attach）→ Dispatch 冷启动
         → SpawnInstance（容器 spawn + 加固）→ /_tw/health 握手 → 请求分发 ×2
         → TW_MAX_REQUESTS 自退 → 容器退出 → reaper 幽灵清理 → Stop/Remove 幂等
```

### 运行

```sh
# Git Bash，torchwood 仓库根
sh dispatcher/testdata/e2e/docker-driver-e2e.sh
```

产物落 `artifacts/docker-<run-id>/`（summary + orchestrator 日志）。
退出码 0 = 编排器输出含 `E2E PASS`。

### 边界（如实登记）

- 覆盖：项目网络创建/自 attach、容器 spawn（加固形态）、容器 IP 寻址、
  health 握手、分发封套、TW_MAX_REQUESTS 自退、reaper 幽灵清理、显式
  Stop/Remove 幂等（容器已退场视为成功）。
- 不覆盖：`BuildImage` 真实构建腿（需拉 node 基础镜像，重网络；构建上下文
  编排与 BuildKit 流解析由单测与 fleetly 形态共用面承接）、`ImportImage`
  真实 pull 腿（fake 驱动的编排单测覆盖）、callback 容器 attach（单测覆盖；
  e2e 编排器栈内无 server 容器可挂）。

