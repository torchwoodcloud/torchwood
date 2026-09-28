# IMPL-T2-3 本地 dind fleetly 端到端（守卫①）

真实 fleetlyd + 真实 swarm 下跑通 dispatcher 的执行底座路径：

```
fleetly-dind.sh（宿主）
  ├─ 交叉编译 fleetlyd/fleetly（FLEETLY_REPO）与 dispatcher e2e 测试二进制
  ├─ 起特权 dind（发布 127.0.0.1:18420 → dind 8420 供宿主注册/铸令牌）
  ├─ in-e2e-boot.sh：swarm init + fleetlyd 起服（REST/gRPC 0.0.0.0）
  ├─ 宿主注册 founder + 铸机具令牌（scope tasks,build；只落 run 目录）
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

## 运行

```sh
# Git Bash，torchwood 仓库根
FLEETLY_REPO=/d/Codes/qiulin/fleetly sh dispatcher/testdata/e2e/fleetly-dind.sh
```

产物落 `artifacts/<run-id>/`（summary/boot/inner 日志与任务日志；`token.txt`
是机具令牌，勿外传）。退出码 0 = `E2E-RESULT: PASS`。

## 边界（如实登记）

- 覆盖：网络 ensure、任务 spawn（CreateTask + 平台收敛）、DNS 名寻址、
  health 握手、分发封套、TW_MAX_REQUESTS 自退、平台回收、显式 Stop/Delete。
- 不覆盖（由单测/T2-2 真机探针承接）：`BuildImage → BuildFromUpload` 真实
  构建腿（本 e2e 用 dind 内 `docker build` 的本地镜像 + 映射种子）、
  internal 变体出网封死（T2-1 探针）、跨节点调度（staging 待验）。
- staging 真机版（对真实 fleetly 控制面 + 多节点）见 T2-4 割接窗口。
