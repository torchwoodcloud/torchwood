# webhook-example —— torchwood Webhooks 接收端示例

一个单文件 Go 接收端：验签（`X-Tw-Signature`）→ 记录投递 → 200。作为
fleetly app 部署后，把公网 Route URL 登记为 torchwood webhook target，
即可端到端验证「平台事件 → CloudEvents 投递 → 签名校验 → 接收方」全链
（docs/developer/08-functions.md §7）。

## 形态

| 面 | 语义 |
|---|---|
| `POST /hook` | torchwood webhook target；验签必经（失败 401 → 平台按重试梯度重投） |
| `GET /healthz` | 存活探针（平台健康门用，不走验签） |
| `GET /deliveries` | 最近 100 条投递的 JSON 视图（演示/验收用；生产接收方不要裸暴露） |

验签口径与 FDK（`sdk/go/functions`）逐字节一致；生产接收方建议直接复用
FDK 中间件，本示例为单文件零依赖而内联同款实现。

## 部署（fleetly）

```bash
# 1) 部署（首建 app；平台 dockerfile builder 构建并推内部仓）
fleetly apps create --project <PROJECT_ID> webhook-example
fleetly deploy --app webhook-example \
  --from-dir docker/fleetly/webhook-example \
  --port 8080 --http-probe /healthz --wait

# 2) 公网 Route（webhook url 强制 https，TLS 由平台边缘终结）
fleetly routes add --app webhook-example --process web --port 8080 \
  --protocol http --tls auto tw-hook.dev.fleetly.run   # host 按环境替换

# 3) torchwood 侧创建 webhook（secret 明文只在创建响应出现一次）
torchwood webhooks create \
  --url https://tw-hook.dev.fleetly.run/hook \
  --events auth.users.*,databases.messageloop.collections.messages.documents.created

# 4) 把 whsec_ 注入接收端并重部署（值变更 = 新 Revision）
FLEETLY_ADDR=... fleetly apps vars set --app webhook-example \
  --value 'whsec_...' WEBHOOK_SECRET
FLEETLY_ADDR=... fleetly deploy --app webhook-example \
  --image <内部仓 digest 引用> --port 8080 --http-probe /healthz --wait
```

## 验收

```bash
# 平台侧：投递台账应出现 delivered
torchwood webhooks deliveries list <wh-id>

# 接收端侧：最近投递（含验签通过后的 CloudEvents 载荷）
curl -s https://tw-hook.dev.fleetly.run/deliveries | jq '.[0]'
```

触发一条真实事件：向订阅串命中的资源写入（如向
`databases.messageloop.collections.messages` 写一条文档），接收端日志
`delivered: type=...` 与 `/deliveries` 视图即为准。
