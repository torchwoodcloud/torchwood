# v3 支付、订阅与虚拟经济系统

> 状态：**已批准（2026-08-19，Open Questions 全部拍板，并入 Key Decisions D12–D19）**  
> 执行计划：`docs/design/v3-execution-plan.md`；派发稿：`docs/prompts/implement-v3.md`  
> 前置：`docs/design/v2-events-realtime-transactions.md`（已实施，PR1–PR5 已合入，审查中）

---

## 已锁定决策（2026-08-19，owner 拍板）

| 议题 | 决定 |
|------|------|
| 支付渠道 | 先抽象 `PaymentProvider` 端口；设计必须覆盖 Stripe、微信支付、支付宝、iOS IAP 四类渠道的差异，不为单一厂商裁剪模型 |
| 资产归属 | 代币账本与物品/库存**都做平台一等资源**（不接受「用户用动态文档自建」） |
| 计费含义 | **两者都规划**：应用内购买计费（帮应用向终端用户收费）+ 平台用量计费（Torchwood 向项目 owner 计量/收费，roadmap §4.6） |
| 范围 | **全量规划，分步实施**：一份设计稿锁定全局，按 PR 切片落地 |
| 里程碑 | **P2.5**（插在内测前）；流程沿用 v2 三件套（批准 → 执行计划 → 派发稿） |

---

## Overview

四个子域，一份设计：

```
                ┌────────────────────────────────────────────┐
                │           PaymentProvider 端口              │
                │   Stripe │ 微信 │ 支付宝 │ iOS IAP（验票）   │
                └───────┬────────────────────┬───────────────┘
                        │ 下单/回调/退款      │ 验票/状态同步
                ┌───────▼────────────────────▼───────┐
                │   Orders（订单状态机 + 幂等）        │
                └───────┬────────────────────────────┘
                        │ paid → 履约（同一 PG 事务）
        ┌───────────────┴──────────────────┬─────────────────┐
┌───────▼───────────────────────┐  ┌───────▼────────┐ ┌──────▼──────┐
│ Assets 资产系统（代币/物品/权益）│  │ Subscriptions  │ │ Metering    │
│ defs + holdings + ledger       │◄─┤ 计划/周期/合同  │ │ 平台用量计费 │
│ Grant/Consume/Transfer/        │  │ （履约=发资产） │ │             │
│ Mutate/Expire                  │  └────────────────┘ └──────┬──────┘
└───────┬───────────────────────┘                           │
        └──────────────────────────┬────────────────────────┘
                                   │ 全部经 transactional outbox
                            ┌──────▼──────┐
                            │ 事件 → 实时推送 │（v2 脊柱复用）
                            └─────────────┘
```

设计主线：**钱进（支付）→ 账动（资产）→ 持续（订阅周期）→ 被看见（事件/Realtime）→ 平台自己也收钱（用量计费）**。

---

## Background & Motivation

### 复用 v2 的什么

- **事务性 outbox**（`public.document_events_outbox` → worker XADD → server Stream → Hub）：经济事件复用同一张表、同一条管道，只扩事件目录与频道。
- **单库事务 / `RunInTx`**：履约、资产变动、订单状态翻转都要求多表原子写，全部走同一个 `sql.Tx`。
- **OCC 习惯**：持有行带 `version`，所有变更先 `SELECT ... FOR UPDATE`。
- **Realtime**：资产变动、订单成交、订阅状态变更需要推到终端用户（游戏/应用的硬性体验要求）。

### 为什么不走动态文档层（关键决策 D1）

资产/订单/订阅**不放**租户 schema 的动态文档表，放**元数据库 `public` schema 静态表**（bun + golang-migrate，同 outbox 的先例）：

1. 资产永远不允许用户直写——动态文档层的 `_perms` 读写模型在这里不是便利而是攻击面。
2. 高频 `UPDATE quantity` + 追加式流水需要 `SELECT FOR UPDATE`、唯一约束、部分索引，静态表直接可用；动态文档的 JSONB `data` + 懒 ALTER 语义不适用。
3. 资产与订单/订阅/履约的跨表事务都在 `public` schema 内，天然单库 ACID。
4. 租户隔离用 `project_id` 列 + use-case 强制过滤（与 outbox 表同一模式）。

### 与「业务文档 + 资产」一单原子的关系（关键决策 D2）

典型诉求：「扣 100 金币 + 发 1 把剑 + 更新玩家文档」一次成功或整体回滚。

- v2 单库事务作用于**租户 schema** 的用户集合；资产在 `public` schema。同一 PG 实例内跨 schema 单事务技术上可行（同一连接同一 `sql.Tx`）。
- **一期不做**跨「动态文档 + 资产」的统一暂存事务（proto op 类型不扩）。一期：Functions / Server API 内调资产 API，资产内部自管单事务；业务文档写与资产写是两笔事务，用幂等键 + 事件对账兜底。
- 二期预留：资产端口提供 `WithTx(tx)` 变体，挂进 documentdb 事务 Commit 路径。表结构（`asset_ledger_entries.tx_id`）一期就留好列，避免二期迁移。

### 明确不在本期范围（已锁定）

- 多币种汇率换算（订单按 `amount + currency` 原样记录，不做换算）。
- 退款自动回收已发放资产（一期退款只翻转订单状态 + 事件，资产回收人工/二期；D12）。
- 用户间交易市场 / 挂单 / 反欺诈；终端用户间自由资产转让（D13）。
- 掉率 / 合成配方 / 升级规则 / 礼包开包逻辑（应用逻辑，跑在 Functions；平台只提供原子组合原语）。
- 有有效期的代币（见 §2.6 边界切割：按可堆叠物品建模；D10）。
- 发票 PDF、税务、财务报表（用量计费一期只出账单文档，不收款）。
- 平台侧向 owner 实际扣款（计量 + 账单先行，扣款复用 PaymentProvider，二期）。
- Realtime 消息数计量（D18）。
- PCI DSS 任何部分：不碰卡号，全部托管收银台 / 渠道 receipt。

---

## Goals & Non-Goals

### Goals

1. 四渠道归一：Stripe / 微信 / 支付宝 / iOS IAP 的差异收敛在各自 infra adapter，use-case 只见归一化 `PaymentProvider` 端口与订单状态机。
2. 订单幂等：客户端建单幂等键、渠道回调事件唯一约束，任何重试/重放不产生重复履约。
3. 履约与订单状态翻转同一 PG 事务：钱到账与发货不可分。
4. **代币 / 物品 / 权益共用一套资产系统**：统一目录（defs）、统一持有（holdings，物化视图）、统一流水（ledger，append-only 真相）、统一操作集（5 动词）。
5. 资产全操作幂等：Grant/Consume/Transfer/Mutate 必须携带幂等键。
6. 订阅双模：渠道托管（Stripe Billing / Apple 订阅，以渠道为事实源）与平台托管（worker 周期扣款）共用一个订阅状态机；订阅履约 = 调资产系统发放。
7. 经济事件全部经 outbox，Realtime 可订阅本人资产频道（单一 `accounts.{uid}`，D17）。
8. 平台用量计量：API 调用、存储字节、函数执行时长按项目/小时聚合落表，可出账单文档。

### Non-Goals

- 交易市场、挂单、拍卖、反欺诈评分。
- 多币种换算、税务引擎、发票 PDF。
- 掉率/配方/升级规则（Functions 的职责）。
- 完整 Messaging 通知（扣款失败先靠事件 + Realtime，邮件/SMS 等 P3 Messaging 再补）。

---

## Proposed Design

### 1. 支付（Payments）

#### 1.1 `PaymentProvider` 端口（domain）

```go
// internal/domain/payments/provider.go
type PaymentProvider interface {
    Name() string // "stripe" | "wechat" | "alipay" | "ios_iap"
    // CreatePayment 下单，返回客户端完成支付所需的载荷：
    // Stripe → checkout URL / client_secret；微信/支付宝 → 预下单参数/二维码串；
    // iOS IAP 不实现（返回 ErrUnsupported，走 VerifyReceipt 路径）。
    CreatePayment(ctx context.Context, in CreatePaymentInput) (*PaymentSession, error)
    // VerifyCallback 验签并归一化渠道异步通知。iOS IAP 对应
    // App Store Server Notifications V2（JWS）。
    VerifyCallback(ctx context.Context, headers http.Header, rawBody []byte) (*CallbackEvent, error)
    Refund(ctx context.Context, in RefundInput) (*RefundResult, error)
}

// iOS IAP 专用端口（客户端 receipt → 服务端验证）：
type ReceiptVerifier interface {
    VerifyReceipt(ctx context.Context, in VerifyReceiptInput) (*VerifiedPurchase, error)
}
```

归一化 `CallbackEvent`：`{Provider, ProviderEventID, ProviderOrderID, Type(paid|failed|refunded|subscription_*) , Amount, Currency, Raw}`。

#### 1.2 渠道差异矩阵（设计必须覆盖，非一期全实现）

| 维度 | Stripe | 微信支付 | 支付宝 | iOS IAP |
|------|--------|----------|--------|---------|
| 下单 | Checkout Session / PaymentIntent | JSAPI/Native/APP 预下单 | `alipay.trade.*` | **无下单**，客户端 StoreKit 直接买 |
| 服务端确认 | Webhook（签名头） | 回调报文解密（商户证书） | 异步 notify（公钥验签） | receipt / App Store Server API 验证 + ASN V2 (JWS) |
| 订阅 | Stripe Billing 托管 | 委托代扣（弱，按平台托管处理） | 周期扣款（弱，同上） | Apple 托管，状态以 ASN 为准 |
| 退款 | API | API（商户证书） | API | **不支持**（引导用户找 Apple） |
| 幂等锚点 | `event.id` | 回调报文流水号 | `notify_id`/`trade_no` | `transactionId` |

配置：`config.proto` 新增 `Payments { Stripe stripe = 1; WeChat wechat = 2; Alipay alipay = 3; IosIap ios_iap = 4; }`，各渠道 secret 走 `TORCHWOOD_` 环境变量，模板进 `configs/config.yaml.template`。

#### 1.3 订单模型（`public.payment_orders`，bun + migrate）

```
id              ULID
project_id      租户隔离列
user_id         付款终端用户（client JWT sub）
provider        stripe|wechat|alipay|ios_iap
idempotency_key 客户端建单幂等键，UNIQUE(project_id, idempotency_key)
provider_session_id / provider_order_id   UNIQUE(provider, provider_order_id) 可空
amount          bigint（最小货币单位，禁止浮点）
currency        char(3) ISO-4217 / 渠道约定
purpose_kind    topup | item_purchase | subscription
purpose         JSONB（{currency_code, amount} / {asset_code, quantity} / {plan_code}）
status          created → paying → paid | failed | closed → refunding → refunded
created_at / paid_at / expires_at
```

状态机（锁定）：

- `created`：建单落库，未向渠道下单（iOS 单直接 created→等验票）。
- `paying`：渠道 session 已建，等回调。
- 回调 `paid`：**同一事务**内 `SELECT FOR UPDATE` 订单 → 状态翻转 → 履约（§2/§3）→ outbox 事件 → COMMIT。
- `failed` / `closed`（超时未付，worker 关单）。
- `refunding → refunded`：一期只翻转状态 + 事件，不动已发放资产（D12）。
- 重放：`(provider, provider_event_id)` 落 `payment_callback_events` 唯一表，重复回调幂等 200，不重入状态机。

#### 1.4 回调入口（HTTP，不走 gRPC gateway）

`POST /v1/payments/callbacks/{provider}`，落在 `internal/api/serverhttp/`（需要**原始 body** 验签，gateway 的 JSON 反序列化会破坏签名）。验签失败 401，不落任何状态、不回执业务码。各渠道回执格式（XML/JSON/`success` 纯文本）在 adapter 内处理。

#### 1.5 履约（Fulfillment）

订单翻 `paid` 同事务内按 `purpose_kind` 分发：

- `topup` → 资产 Grant（currency 类，ref=order_id）
- `item_purchase` → 资产 Grant（stack/instance/entitlement 类）
- `subscription` → 订阅激活/续期（§3）

履约记录落 `payment_fulfillments`（order_id + 目标 ledger entry ref + 类型），与订单同事务。任何「钱到了货没发」视为数据事故——同事务从根上杜绝。

---

### 2. 资产系统（Assets）——代币 / 物品 / 权益统一模型

#### 2.1 概念切割：资产 vs 订阅（锁定 D9）

**订阅不是资产，是产生资产的合同。** 订阅是计费关系（计划、周期、渠道、状态机）；它的履约产物才是资产（代币入账、物品发放、权益刷新 `expires_at`）。切割原则：

- 资产系统**不知道订阅存在**；订阅把资产系统当履约工具调用。
- 「月卡道具」链路 = 购买 stack 物品 → Functions 内 Consume 该物品 + Grant 一条 entitlement。平台不设专门概念。

#### 2.2 属性正交分析（设计依据）

任一「持有」可分解为六元组：`(owner, 定义, 数量, 状态, 时间界, 来源)`。类别差异全部落在六元组的约束上，不需要六套系统。逐项属性的深入分析：

- **计量形态**（非布尔，三态+权益）：标量（代币，全局一个数）/ 堆叠（同定义同质，合并为数量）/ 实例（每个个体独立，携带私有数据）/ 权益（时间窗状态，无数量）。
- **有效期**：定义级只给默认 TTL（`expires_in`），持有级落**绝对 `expires_at`** 才是事实。
- **同有效期堆叠**：一旦允许过期，堆叠键必须包含 `expires_at`——同定义同到期时间才同桶，不同到期拆桶；消耗按 **FEFO**（先到期先扣）。这是堆叠 × 有效期的必然推论，不是可选项。
- **唯一性**：`unique_per_owner` 是叠加约束，不独立成类（绝版头像框 = 唯一实例；改名卡可持有多张 = 普通堆叠）。
- **可升级**：仅实例形态有意义（强化等级是个体属性）。双建模均支持：实例 `level` 列（平台机制）与 def-per-level（升级=Consume 旧 + Grant 新）。**升级规则（材料/成功率/配方）是应用逻辑**，跑在 Functions；平台提供「消耗材料 + 变更实例」一单原子组合。
- **可交易**：纯开关，叠加在任何形态上（权益除外，禁止转让）。

#### 2.3 四类别与属性合法性矩阵（锁定）

`class`：`currency` | `stack` | `instance` | `entitlement`

| 属性 | currency 代币 | stack 可堆叠 | instance 实例 | entitlement 权益 |
|------|--------------|--------------|---------------|------------------|
| 数量 | 标量余额 | quantity ≥ 0 | 恒 1 | 恒 1 |
| `expires_at` | **禁止**（D10） | 可选，同到期同桶、FEFO | 可选（限时皮肤） | **必填** |
| `level` | 无 | 无（同桶同质） | 可选（upgradeable） | 可选（tier） |
| 私有 `metadata` | 无 | 无（同质才可堆叠） | 有（roll/耐久） | 少量 |
| `unique_per_owner` | 天然（一币种一行） | 可选 | 可选 | 天然 |
| `tradable` | 可选（转账=Transfer） | 可选 | 可选 | **禁止** |

礼包 / 掉率 / 合成**不设类**：def 的 `metadata` 携带 contents，「开包」= Functions 组合 Consume + N×Grant。

付费钻 / 免费钻分桶：建模为**两种 currency 定义**（业界惯例），消耗顺序策略二期再定（D15）。

#### 2.4 物理模型（`public` schema，bun + migrate）

```
asset_defs            id, project_id, code, name, class, decimals(currency 展示用),
                      max_quantity(stack/currency 上限，可空),
                      expires_in(默认 TTL 秒，可空),
                      tradable, unique_per_owner, upgradeable,
                      metadata JSONB, status

asset_holdings        id, project_id,
                      owner_type 默认 'user'（一期只用 user；列保留 group/project 开放，D14）,
                      owner_id, def_id,
                      quantity bigint,
                      expires_at timestamptz 可空,
                      level int 默认 0,
                      metadata JSONB,
                      version bigint,
                      UNIQUE(owner_type, owner_id, def_id, expires_at)  -- 合并键
                      -- instance 类：每行一个个体，quantity=1 由 class 约束保证

asset_ledger_entries  id, project_id, holding_id 可空, owner_type, owner_id, def_id,
                      kind(grant|consume|transfer_out|transfer_in|mutate|expire|adjust),
                      delta bigint, quantity_after bigint,
                      ref_type(order|subscription|function|admin|transfer...), ref_id,
                      idempotency_key UNIQUE, tx_id(预留 D2),
                      operator JSONB（principal 快照）, created_at
```

约束与规则（锁定）：

- **holdings 是物化视图，ledger 是真相**（D11）：任一持有行的 `quantity` 必须等于其流水之和；`quantity_after` 冗余落列支持链路校验；worker 提供对账任务（一期手动触发）。
- `asset_ledger_entries` **append-only**，禁止 UPDATE/DELETE；纠错用反向 entry（`adjust`）。
- **消耗/过期不留尸体行**：holding 行删除，历史在流水；`locked` 状态为二期交易锁预留，一期不产生。
- currency 类：每个 (owner, def) 至多一行（`expires_at` 恒 NULL 使唯一索引天然成立）；非负约束在 use-case 校验。
- 每笔变动：`SELECT ... FOR UPDATE` holding → 校验（非负 / 上限 / unique_per_owner / class 规则）→ INSERT entry → UPDATE/INSERT/DELETE holding（`version+1`），同一事务。

#### 2.5 操作集（接口 5 动词，类别差异收进实现）

| 动词 | 语义 | 组合性 |
|------|------|--------|
| `Grant` | 发放（含充值 topup、购买发放、订阅履约、admin 补发） | 原语 |
| `Consume` | 消耗（扣代币、用物品、消耗实例） | 原语 |
| `Transfer` | 转让 = Consume(from) + Grant(to)，接口上原子暴露 | 组合，接口暴露 |
| `Mutate` | 实例属性变更（升级 level、改写 metadata） | 原语（仅 instance/entitlement） |
| `Expire` | 系统发起：worker 扫描到期 + 读路径懒过滤 | 系统内部 |

- 全部携带幂等键；Transfer 产生双侧 entry（`transfer_out`/`transfer_in`，共享 ref_id）。
- FEFO、分桶、class 约束校验全部在实现内，接口只见「动词 + def code + 数量 + 幂等键」。
- 礼包/合成/升级 = Functions 在一单事务内组合上述动词（如升级：Consume 材料 ×N + Mutate 目标 level+1）。

#### 2.6 有效期语义与边界切割（锁定 D10）

- **代币不允许有有效期**。有有效期的「代币」（积分过期场景）按 stack 类建模——余额分桶 + FEFO 正是 stack 已有语义；给最高频的标量余额行加分桶是把最热的行变成最复杂的模型。
- entitlement 必有 `expires_at`；stack/instance 的 `expires_at` 由 def 的 `expires_in` 在 Grant 时折算，也可由调用方显式指定（同到期自动并桶）。
- 到期处理双管齐下：读路径懒过滤（`expires_at > now`）+ worker 周期扫描产出 `expire` 流水并删行（批量，事件可聚合）。

#### 2.7 API 分层（红线不变）

| 面 | 能力 | 鉴权 |
|----|------|------|
| Client API | ListAssetDefs、ListMyAssets（含 ledger 查询）、读权益状态 | session JWT，只看自己 |
| Server API | Def CRUD、Grant/Consume/Transfer/Mutate/Adjust | API Key scope `economy.write` / console admin |
| Functions | 同 Server API（经济写操作的主要场景） | 函数执行身份 |

**终端用户没有任何直接写资产的 API**——资产变动只能由服务端发起。这是产品红线。转让（Transfer）一期仅 Server/Functions 可发起，终端用户间自由交易后置（D13）。

---

### 3. 订阅（Subscriptions）

#### 3.1 双模统一状态机

```
subscription_plans   id, project_id, code, name, amount, currency,
                     interval(month|year|custom_days),
                     grace_days（宽限期，plan 级配置，D16）,
                     benefits JSONB（资产发放清单，见 §3.2）,
                     provider_overrides JSONB（渠道侧 price_id 等映射）, status
subscriptions        id, project_id, user_id, plan_id, mode(hosted|platform),
                     provider, provider_sub_id, status, current_period_start/end,
                     cancel_at_period_end, grace_until, created_at
```

状态机（锁定）：`trialing → active ⇄ past_due → canceled | expired`。`past_due` 的宽限期由 plan 的 `grace_days` 折算为 `grace_until`，过期进 `expired`。

- **渠道托管（hosted）**：Stripe Billing / Apple。订阅的创建、扣款、状态以渠道为事实源；平台存镜像，靠 webhook/ASN 驱动状态迁移。不在平台侧发起扣款。
- **平台托管（platform）**：微信/支付宝（委托代扣能力弱，按自建周期处理）或**代币余额扣款**（Consume currency 类资产）。`cmd/worker` 注册 `subscription_biller` consumer：扫 `current_period_end <= now AND status=active` → 生成支付订单（或资产 Consume）→ 成功则续期 + 履约 benefits，失败进 `past_due` + 事件。

#### 3.2 权益（benefits）= 资产发放清单

`benefits` JSONB 快照：`{grants:[{asset_code, quantity, expires_in?}, ...], entitlements:[{asset_code, tier}]}`。激活/每期续费成功时，**调用资产系统 Grant**（同事务）完成履约——订阅系统自己不碰 holdings。

- entitlement 类资产承载「VIP/通行证」语义：续期 = Mutate 该持有的 `expires_at` 延长。
- 读侧由应用查订阅状态或 entitlement 持有判断权益，**不建独立 entitlement 服务**。

---

### 4. 平台用量计费（Metering & Billing）

> 面向项目 owner，roadmap §4.6 的落地。

#### 4.1 计量点

| 指标 | 采集点 |
|------|--------|
| API 调用次数 | grpc-gateway / gRPC 拦截器计数（按 project + method 类） |
| 存储字节 | 复用现有 `SumDocumentField` 聚合（Storage usage 已有） |
| 函数执行时长 | executions 已有 duration 字段 |

Realtime 消息数/连接时长**一期不计量**（D18）。

#### 4.2 管道

计数走 **Redis `INCRBY (project, metric, hour_bucket)`** → worker 定时（5min）扫上一完整小时 bucket 落 `usage_rollups(project_id, metric, period_start, value)` 幂等 upsert → 月聚合出 `billing_statements`（账单文档，JSONB 明细，状态 draft→final）。

- 一期交付：计量 + rollups + 账单文档 + Server/Console 查询。**不向 owner 收钱**。
- 二期：owner 账单支付复用 §1 的 `PaymentProvider`（对称设计，无需新抽象）。
- 配额/限流阈值挂钩**不在本设计**——依赖 §3.4 速率限制先行落地。

---

### 5. 事件与 Realtime 集成

#### 5.1 事件目录扩展（复用 outbox 同表同管道）

```
payments.orders.paid | failed | refunded
economy.assets.granted | consumed | transferred | mutated | expired
subscriptions.activated | renewed | past_due | canceled | expired
```

- outbox 写与业务写同一事务（资产变动、订单翻转、订阅迁移各自的 `sql.Tx` 内 INSERT outbox 行）。
- 信封扩字段：`channel` 直接落列（v2 是按信封现算频道，经济事件频道不挂在 database/collection 上，需要显式列）；payload 不含任何隐私字段（资产事件只带 def code / delta / quantity_after，不带他人信息）；事件用 `domain` 字段区分 economy/orders/subscriptions（配合 D17 单频道）。

#### 5.2 Realtime 频道扩展

新频道**合并为单一** `accounts.{userId}`（D17）：订单、资产、订阅事件都发这一条，客户端按事件 `domain`/名称分流。

- 现状 `internal/api/realtime/handler.go` 的 `parseChannel` 只认 `databases.*`——**设计预留 seam**：频道解析改派发表，`accounts.*` 频道校验规则 = 本人（JWT sub == userId）或 platform admin。
- 出站事件按频道天然过滤（只发本人），无需 `_perms` 快照；outbox `acl` 列对这类事件置空。
- 配额沿用 v2（每用户 4 连 / 32 订阅）。

---

### 6. API 变化（proto 草案）

| proto | 内容 | authz |
|-------|------|-------|
| `client/v1/payments.proto` | CreateOrder（幂等键 + purpose）、GetMyOrder、ListMyOrders、VerifyReceipt（iOS） | session |
| `client/v1/assets.proto` | ListAssetDefs、ListMyAssets、ListMyAssetLedger | session |
| `client/v1/subscriptions.proto` | ListPlans、Subscribe、GetMySubscription、Cancel（期末生效） | session |
| `server/v1/assets.proto` | Def CRUD、Grant/Consume/Transfer/Mutate/Adjust | scope `economy.write` |
| `server/v1/payments.proto` | ListOrders、GetOrder、Refund、ManualFulfill（兜底人工履约） | scope `payments.write` |
| `server/v1/subscriptions.proto` | Plan CRUD、ListSubscriptions、强制 Cancel/Expire | scope `subscriptions.write` |
| `server/v1/billing.proto` | GetUsage、ListRollups、ListStatements | scope `billing.read` / console admin |
| HTTP | `POST /v1/payments/callbacks/{provider}` | 渠道验签，无 JWT |

- 全部 gRPC 方法带 `method_auth` 注解；`adminRoleMethodRules` 登记全部写方法；swagger/`method_auth` 测试必须绿（v2 已立的规矩）。
- 更新类请求可选字段用 `proto3 optional`；时间字段一律 `google.protobuf.Timestamp`。

---

## Data Model Changes

### 静态（bun / golang-migrate，新 migrations 从 000013 起）

`payment_orders`、`payment_callback_events`、`payment_fulfillments`、`asset_defs`、`asset_holdings`、`asset_ledger_entries`、`subscription_plans`、`subscriptions`、`usage_rollups`、`billing_statements`。

### 动态（租户 schema）

**无**。本子域不动态建表（决策 D1）。

### 领域 port 增量

`PaymentProvider`、`ReceiptVerifier`、`OrderRepo`、`AssetRepo`（defs/holdings/ledger，含 `WithTx` 预留）、`SubscriptionRepo`、`UsageRepo`、`UsageCounter`（Redis）、事件目录常量扩展。

---

## Security & Privacy Considerations

1. **回调验签是唯一的信任根**：验签失败一律 401，不落库、不返回区分性错误（防探测）。原始 body 直读，禁止先进 JSON 中间件。
2. **金额/数量**：全链路 bigint 最小单位；`decimals` 仅展示用；任何 float 出现即 review 打回。
3. **资产写路径**：终端用户无写 API；use-case 层再断言一次 principal 是 system/admin-key（纵深防御）。
4. **幂等三锚点**：客户端幂等键（建单/资产操作）、渠道事件 id（回调）、履约 ref（发货）。任一环节重试都安全。
5. **iOS receipt**：`transactionId` 全局唯一防重放；一份 receipt 绑一个 user，跨用户领取拒绝。
6. **审计**：admin 的 Adjust / Refund / ManualFulfill 全部写审计日志——**依赖 §3.4 审计日志先行**。
7. **密钥**：渠道 secret 全走环境变量，不进 config.yaml、不进 proto 默认值。

---

## Observability

指标（前缀 `torchwood_`）：`payment_orders_total{provider,status}`、`payment_callback_verify_fail_total{provider}`、`payment_fulfillment_lag_seconds`、`asset_ops_total{kind,class,result}`、`asset_negative_quantity_blocked_total`、`asset_ledger_drift_total`（对账发现不一致）、`subscription_billing_cycle_total{result}`、`usage_rollup_lag_seconds`。

告警（一期看板即可，同 v2 定调）：回调验签失败突增、履约 lag > 1min、ledger 对账漂移 > 0、`past_due` 订阅堆积、rollup lag > 10min。

---

## Rollout Plan / PR 切片

> 详细切片与验收见 `docs/design/v3-execution-plan.md`。

依赖顺序：**PR0 前置 → PR1 支付骨架 → PR2 资产系统 → PR3 订阅 → PR6 Console/SDK**；PR4（渠道补齐）在 PR1 后可并行；PR5（用量计费）基础设施就绪后即可并行。

| PR | 内容 | 关键验收 |
|----|------|----------|
| PR0（前置，非本设计交付） | §3.4 速率限制 + 审计日志 | 内测/收钱前的硬性门槛 |
| PR1 支付骨架 | orders 表、PaymentProvider 端口、Stripe adapter、回调端点、幂等、outbox 事件 | Stripe webhook 重放幂等；paid 同事务翻转 |
| PR2 资产系统 | defs/holdings/ledger、5 动词、FEFO/分桶/唯一性约束、只读 Client API、事件、对账任务、topup/item_purchase 履约联通 | 流水重放 = holdings；负余额拒绝；幂等键重试安全；四类别矩阵约束测试 |
| PR3 订阅 | plans/subscriptions、Stripe Billing 托管镜像、平台托管周期 worker、benefits 履约（调资产系统） | 扣款失败 → past_due → 宽限 → expired；续期履约与续期同事务 |
| PR4 渠道补齐 | 微信/支付宝 adapter、iOS VerifyReceipt + ASN V2 | 四渠道回调归一化测试 |
| PR5 用量计费 | Redis 计量、rollups、statements、查询 API | 小时 bucket 幂等落表；账单与 rollup 对得上 |
| PR6 Console/SDK | 订单/资产/订阅 Console 页、TS/Go SDK 封装、demo | 同 v2 PR5 级别验收 |

共同约束沿用 v2 执行计划 §共同约束（vet/test 绿、不动 genproto、console-build 后再 build、中文 commit、一 PR 一件事）。

---

## Key Decisions（汇总）

| # | 决策 | 理由 |
|---|------|------|
| D1 | 资产/订单/订阅放 `public` 静态表，不走动态文档层 | 权限模型反而成攻击面；要行锁/唯一约束/追加流水 |
| D2 | 「文档+资产」一单原子二期，一期幂等+事件兜底；表列（`tx_id`）一期预留 | 避免一期动 v2 事务 proto 的复杂度 |
| D3 | 订单履约与状态翻转同一事务 | 根绝「钱到货没到」 |
| D4 | 订阅双模一状态机：渠道托管以渠道为事实源，平台托管 worker 周期扣款 | 微信/支付宝代扣弱，Apple/Stripe 不许自建扣款 |
| D5 | 用量计费一期只计量+账单，不收款；收款复用 PaymentProvider | 对称复用，先解决「看得见」 |
| D6 | 终端用户无资产写 API，use-case 层二次断言 | 经济系统红线 |
| D7 | 回调走 serverhttp 裸 body，不走 grpc-gateway | 验签需要原始报文 |
| D8 | 代币/物品/权益统一一套资产系统：defs + holdings + ledger，5 动词 | 统一落点是流水与操作集，不是物品模型；类别差异收进实现 |
| D9 | 订阅不是资产，是产生资产的合同；资产系统不知道订阅存在 | 切割后两边各自简单；月卡=物品+Functions 编排 |
| D10 | currency 禁止有效期；有期「代币」按 stack 建模 | 余额分桶+FEFO 复用 stack 语义；不给最热行加最复杂模型 |
| D11 | holdings 是物化视图、ledger 是真相；消耗/过期删行不留尸体 | 对账/审计/客服全部由流水回答 |
| D12 | 退款一期只翻转订单状态 + 事件，不动已发放资产 | owner 拍板；资产回收人工/二期 |
| D13 | Transfer 一期仅 Server/Functions 发起；用户间自由交易后置 | owner 拍板 |
| D14 | 资产 owner 一期只挂 user；`owner_type` 列入表保持 group/project 开放 | owner 拍板 |
| D15 | 付费/免费代币建模为两个 currency 定义；消耗顺序策略二期 | owner 拍板 |
| D16 | 订阅宽限期为 plan 级配置（`grace_days`） | owner 拍板 |
| D17 | Realtime 经济频道合并为单一 `accounts.{userId}` | owner 拍板；客户端按事件 domain 分流 |
| D18 | Realtime 消息数一期不计入用量计量 | owner 拍板 |
| D19 | 里程碑落位 P2.5（内测前）；沿用 v2「批准→执行计划→派发稿」流程 | owner 拍板 |

---

## Open Questions

已全部拍板（2026-08-19），结论并入 Key Decisions D12–D19。

---

## References

- `docs/design/v2-events-realtime-transactions.md`（outbox / Realtime / 事务，本设计复用）
- `docs/design/v3-execution-plan.md`（执行切片）、`docs/prompts/implement-v3.md`（派发稿）
- `docs/roadmap.md` §3.4（PR0 前置）、§4.6（用量计费原始条目）
- `internal/domain/events/envelope.go`（事件信封与 ClientPayload 先例）
- `internal/infra/realtime/subscriber.go`（Stream 消费循环）
- `internal/api/realtime/handler.go`（`parseChannel` 频道 seam）
- `internal/api/serverhttp/`（裸 HTTP handler 先例：Storage 上传下载）
- `internal/pkg/config/config.proto`（Payments 配置落点）
- `cmd/worker/worker.go`（consumer 注册点）
- PlayFab Economy / Steam Inventory Service（资产分类与 FEFO 的业界参照）
- Stripe / 微信支付 / 支付宝 / Apple App Store Server API 官方文档（adapter 实施时引用）
