# fleetly API 契约（vendored）

本目录是 fleetly 平台 API 契约在 torchwood 仓的 **verbatim 副本**，供
dispatcher 作为 fleetly Tasks/build API 客户端消费（IMPL-T2-3，DT-5/DT-6）。

## 来源与同步纪律

| 文件 | 来源 | 来源 sha256 |
|---|---|---|
| `proto/fleetly/server/v1/tasks.proto` | `fleetlyrun/fleetly` @ `1b60e146d20ea346b58bab99e370ad5ae97977b7`（2026-09-28） | `52e6b7d54df6e1564f7a0a7c96050d1a5d42c8180389d931a554b6a6cc7e528f` |
| `proto/fleetly/server/v1/builds.proto` | 同上 | `d0847fc6e2d3c7ab4a5c0f6f394c58ce2b1933e85d35578451f5291d00381650` |
| `proto/fleetly/shared/v1/error.proto` | 同上 | `6844418d7a70a341b6be596d60abcb3d4a2f95fddaa95cd6b81909a091a74868` |

**同步流程**（fleetly 侧契约变更后）：从 fleetly 仓复制上述文件（逐字，不改
内容）→ 更新本表 sha256 与 commit → `cd third_party/fleetly && buf generate`
→ `go build ./...`（genproto 模块）→ 跑 torchwood 全量测试。

**为什么 vendored 而不是 Go module 依赖**（审查裁决，见实施方案 §4
IMPL-T2-3）：fleetly 的 `sdk/go` 与 `genproto` 均为未发布的独立 Go module
（`github.com/fleetlyrun/fleetly/...`），私有仓 + 未发布形态在 torchwood CI
（公开 GitHub Actions，无 fleetly 凭据）不可拉取；SDK 代码拷贝同病（import
的 genproto 仍不可得）。vendored proto + 本仓生成 stubs 是唯一自足、CI 友好
的消费形态；契约漂移由「来源 sha256 台账 + 同步流程」显式管理（不做网络
drift 检查——CI 无 fleetly 访问权）。

## 生成

- 配置：本目录 `buf.yaml`（独立 module，与 torchwood 主 proto 模块隔离）
  + `buf.gen.yaml`（仅 protobuf-go 与 grpc-go 两插件——dispatcher 是纯客户端，
  不生成 gateway/swagger）。
- 产物：`genproto/fleetly/{server,shared}/v1/*.pb.go`（提交入库）。
- 命令：`mise run generate:fleetly-proto`（= `cd third_party/fleetly && buf generate`）。

## 消费面

- `fleetly.server.v1.TasksService`：EnsureTaskNetwork / CreateTask / GetTask /
  ListTasks / StopTask / DeleteTask（scope 引用 + 平台加固，无 attach 入参）。
- `fleetly.server.v1.BuildsService.BuildFromUpload`：client-streaming 上传构建
  （gRPC-only）。
- `fleetly.shared.v1.ErrorResponse`：错误信封 detail（稳定错误码提取，
  配额/镜像缺失的分类判据）。
