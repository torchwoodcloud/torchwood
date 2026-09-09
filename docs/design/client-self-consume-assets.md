# 客户端可消费资产（Self-Consume）设计草案

> 状态：**草案（2026-09-09，待 owner 评审拍板；Open Questions 清零前不得实施）**
> 动机：纯客户端游戏（无自建服务器）需要服务端权威的奖励核销通道（激励视频复活奖励等）
> 前置：`docs/design/v3-payments-economy.md`（本稿包含对红线 D6 的修订，随本稿一并拍板）
> 执行计划：本稿 §Rollout 的 PR 切片，批准后按仓库惯例派发

---

## Overview

为 asset def 新增**客户端可消费**策略（`client_consumable` + `self_consume_quota`），并在 Client API（ACCESS_END_USER 面）新增一个受配额约束的消费端点 `POST /v1/assets:self-consume`。终端用户仍然**只能「花」、不能「发」**：资产流入（grant/transfer_in）只能由服务端发起；自服务消费由服务端在事务内权威执行限额、幂等与记账。

核心不变量（对 v3 设计的增量）：

1. **资产流入只能服务端发起**（原 D6 精神收窄为「流入」而非「一切写」）；
2. 自服务消费是 def 级显式 opt-in（存量项目零影响、fail-closed）；
3. 每次自服务消费都有账本条目，`ledger 重放 = holdings 快照` 的对账不变量（D11）不受影响；
4. `client_consumable` 的 def 强制不可转让（防止跨账号汇集额度）。

```
微信小游戏（纯客户端）            torchwood
┌──────────────────┐   ①isEnded（不可验证）  ┌─────────────────────────┐
│  激励视频广告 ────┼───────────────────────▶│ POST /v1/assets:self-consume
│                  │                        │  ├ def.client_consumable? ──否→ PermissionDenied
│  无自建服务器     │   ②复活次数 +1          │  ├ 当日通道配额 used<K?   ──否→ ResourceExhausted
│                  │◀───────────────────────│  ├ 惰性 grant 当日额度（幂等键系统派生）
└──────────────────┘                        │  └ consume（FEFO/OCC/账本）同事务
                                            └─────────────────────────┘
```

## Background & Motivation

### 场景

微信小游戏的激励式视频广告没有强制的服务端回调——观看完成信号只有客户端 `isEnded`；官方另有**可选**的「激励广告服务端验证」（SSV，AES 回调 + transaction_id，基础库 ≥ v3.10.3），不接则退化为 isEnded + 限额（见 `economy-client-write-competitive-analysis.md` §一）。纯客户端游戏（无自建游戏服务器）接 torchwood 做账号/云存档/奖励时，「每日最多看 N 次广告换 N 次复活（N 可运营配置）」这条链路的服务端权威管理在当前能力下**没有任何路径**：

1. 客户端资产面只读三方法（`proto/client/v1/assets.proto:69-81`）；grant/consume 在 Server 面，需 `assets:write` scope 的 API key 或 admin 会话（`proto/server/v1/assets.proto:108-151`）。
2. Client proto 无 functions 服务，函数通道不存在；函数执行器也不自动注入 API key。
3. 唯一内置自动发放是支付履约（`internal/app/assets/fulfiller.go`，幂等键 `fulfill:{orderID}`），不适用广告场景。

不解决的后果：限额只能落在客户端，作弊者可无限复活；torchwood 作为「无自建服务器 BaaS 游戏后端」的适用面受限。

### 红线 D6 的原始动机与本稿的修订

D6 出处：`docs/design/v3-payments-economy.md`（2026-08-19 批准），Key Decisions 表：「D6 终端用户无资产写 API，use-case 层二次断言——经济系统红线」。散文表述（§2.7）：「终端用户没有任何直接写资产的 API——资产变动只能由服务端发起。」动机论证散布在两处：

- D1 论证（`:62-69`）：「**资产永远不允许用户直写**——动态文档层的 `_perms` 读写模型在这里不是便利而是攻击面」，即防用户改变自己的余额/给自己发资产；
- Security #3（`:416`）：纵深防御（API 面收口 + use-case 二次断言）。

2026-08-22 独立评审（`docs/review/saas-baas-design-2026/05-economy.md`）以代码为真相复核后**维持红线**（`:45`「这条红线是对的」），但 `:334` 明确点名了消费场景缺口：「终端用户不能自己扣自己的币——对防作弊是对的；对 SaaS 用户消耗 included credits 则必须走 builder 的 Server API」。可见红线保护的实质是**资产流入的服务端权威**（用户不得改变余额来源），而非禁止用户发起受控扣减请求——受控扣减由服务端执行记账，恰恰是防作弊的强化而非削弱。

**修订表述（建议，D6-v2，待拍板）**：

> 终端用户无资产**流入/调整**写 API：Grant/Transfer/Mutate/Expire/Adjust 仅 system/admin-key 发起（use-case 二次断言不变）。唯一例外：def 显式声明 `client_consumable` 时，终端用户可发起 Consume，由服务端权威执行配额、幂等与记账；能扣减的前提是余额存在，余额来源依旧只有服务端。

## Goals & Non-Goals

**Goals**

- 终端用户在 def 显式 opt-in 下可发起受配额约束的消费，服务端权威执行；
- 每日配额可运营配置（def 级），修改立即对新区间生效；
- 幂等重放、账本可审计、对账不变量全部沿用既有机制；
- 存量项目/存量 def 零影响（未声明即 PermissionDenied，fail-closed）。

**Non-Goals**

- 不验证「真的看了广告」——`isEnded` 不可验证，本机制把不可验证信号转化为**限额 + 幂等 + 频控 + 账本可审计**的受限资源（损失上限 = 每日配额 K/账号，见 Security）。若目标游戏接入微信 SSV，发放链路应优先走回调验证，本稿退化为「SSV 前即时暂发 + 对账核销」的可选增强（见 Alternatives 方案 A'）；
- 不做函数的客户端调用面（方案 B，独立演进，见 §Alternatives）;
- 不做自服务 Transfer/Grant（D13 用户间交易仍后置）；
- 一期不做冷却窗口（cooldown，见 Open Questions）；
- 不做滚动窗口配额（一期自然日窗口，见 Open Questions）。

## Proposed Design

### 1. Asset def 策略字段

Server 面 `proto/server/v1/assets.proto`：

```proto
message AssetDef {
  // … 现有 1-15 不动 …
  // 终端用户自服务消费开关（D6-v2 唯一例外通道）；开启时强制 tradable=false。
  bool client_consumable = 16;
  // 每自然日（UTC）每用户自服务消费上限；client_consumable=true 时必须 >=1。
  int32 self_consume_quota = 17;
}
message CreateAssetDefRequest {
  // … 现有 1-10 不动 …
  bool client_consumable = 11;
  optional int32 self_consume_quota = 12;
}
message UpdateAssetDefRequest {
  // … 现有 1-10 不动 …
  optional bool client_consumable = 11;
  optional int32 self_consume_quota = 12;
}
```

Client 面投影（`proto/client/v1/assets.proto`）同步暴露只读字段，客户端启动时可据此决定是否展示「看广告复活」入口：

```proto
message AssetDef {
  // … 现有 1-11 不动 …
  bool client_consumable = 12;
  int32 self_consume_quota = 13;
}
```

**def 级不变量（app 用例层校验，跨字段规则不入 protovalidate）**：

- `client_consumable = true` ⇒ `tradable` 必须为 `false`（Create 拒绝、Update 拒绝改回 true）；`entitlement` class 本就禁转让，不变量对四类 class 统一适用；
- `client_consumable = true` ⇒ `self_consume_quota >= 1`；
- 配额调低**不追溯**：当窗口已用次数不回滚，新窗口按新值生效（计数以账本为准，天然如此）。

### 2. Client RPC：`POST /v1/assets:self-consume`

`proto/client/v1/assets.proto`（服务级 `service_auth default_access: ACCESS_END_USER` 继承，无需方法级注解，与只读三方法一致）：

```proto
rpc SelfConsume(SelfConsumeRequest) returns (SelfConsumeResponse) {
  option (google.api.http) = { post: "/v1/assets:self-consume", body: "*" };
}

message SelfConsumeRequest {
  string def_code = 1 [(buf.validate.field).required = true];
  // 项目级全局幂等键，语义与资产侧一致（Stripe 语义）：永久去重、
  // 重放返回首次条目、不同动词不可复用同一键。
  string idempotency_key = 2 [(buf.validate.field) = {
    required: true, string: {max_len: 128}   // 与 domain MaxIdempotencyKey 对齐
  }];
  // 可选业务引用（如广告会话 ID），进账本 ref_type/ref_id，可审计。
  string ref_type = 3 [(buf.validate.field).string.max_len = 64];
  string ref_id   = 4 [(buf.validate.field).string.max_len = 128];
}

message SelfConsumeResponse {
  // 该次操作产生的全部账本条目（当日首次调用含惰性 grant 条目 + consume 条目）。
  repeated AssetLedgerEntry entries = 1;
  bool idempotent_replay = 2;
  // 本窗口已用/上限（客户端 UI 直用，免再查账本）。
  int32 quota_used = 3;
  int32 quota_limit = 4;
}
```

一期**不收**业务 metadata（`google.protobuf.Struct` 入账本需定大小上限与脱敏策略，见 Open Questions；`ref_type/ref_id` 已覆盖「广告流水 ID」级审计需求）。

### 3. 额度模型：M2 惰性 grant 余额型 + 通道配额（M1 落选）

两个候选：

- **M1 计数器型**（非余额）：consume 即记一条、不计 holdings，额度 = 当日条目数。
- **M2 余额型 + 通道配额**（本稿采用）：额度以资产余额表达，当日首次 self-consume 时惰性 grant 当日配额，再扣减；另以「该 origin 的当日 consume 条目数 ≤ quota」做通道上限。

**M1 落选理由**：破坏 v3 统一资产模型的基石——「无持有扣减」要么让 ledger 出现不落地 holdings 的条目（`Reconcile` 流水重放 ≠ 快照，破坏 D11 对账不变量），要么允许 holdings 数量为 0/负（动 `quantity > 0` CHECK 与全链 FEFO/OCC 语义）。为省一条 grant 条目动摇统一模型不值。

**M2 细节**：

- **惰性 grant**：当日窗口首次 self-consume 时，以系统派生幂等键 `selfserve:{def_id}:{owner_id}:{YYYYMMDD}` grant `self_consume_quota` 个，`expires_at = 窗口结束时刻`（UTC 次日 00:00）。键的所有成分（def_id/owner_id/日期）均由服务端从 Principal 与时钟派生，**用户不可操纵任何成分**——这是「流入只能服务端发起」不破的关键。expires_at 使「当日额度不结转」精确落地，且免费复用既有 expire 扫描与 FEFO。
- **通道配额**：账本按 origin 计数。`asset_ledger_entries` 新增 `origin TEXT NOT NULL DEFAULT 'server'`（取值 `server` | `self_consume`；支付履约等后续可逐步标注，存量行默认 `server` 语义不变）。self-consume 的 use-case 在同一事务内 count `(owner, def, origin='self_consume', kind='consume', 当日) >= quota` 则拒绝。纯自服务场景下惰性 grant 量 = 通道配额，两个约束重合；同 def 另有服务端 grant 时（运营活动奖励），通道配额保证「广告通道严格每日 K 次」，多发的余额留给正常消费——语义各自清晰。
- **结转语义**：运营想让额度结转（攒着用）→ def 不设 `expires_in` 且接受惰性 grant 的窗口期 expires_at？不行——惰性 grant 固定带窗口结束 expires_at，即**自服务通道的额度一律当日有效**。要结转的场景应由服务端 grant（运营活动）而非自服务通道发放，边界清晰（如需自服务结转型额度，见 Open Questions）。
- **与 server 面 Consume 的关系**：同一 def 可被服务端正常 consume（`origin='server'`），互不干扰；self-consume 复用领域层 `Consume`（FEFO、OCC、outbox 事件），仅 origin 标注不同。

### 4. use-case 伪代码

位置：`internal/app/assets/`（新文件 `selfconsume.go`；**不经** `requireAssetWrite`——那是 D6 的执行点，self-consume 有自己的断言）：

```go
// SelfConsume 终端用户自服务消费（红线 D6-v2 唯一例外通道）。
func (a *Assets) SelfConsume(ctx context.Context, cmd SelfConsumeCommand) (*SelfConsumeResult, error) {
    // 纵深防御：仅 end_user principal（API key / console 会话在拦截器已被
    // END_USER permission 门拒绝；此处二次断言，与 requireAssetWrite 同惯例）。
    p := RequireEndUser(ctx)                       // → {projectID, userID}
    def, err := a.repos.Defs.GetByCodeForShare(ctx, p.ProjectID, cmd.DefCode)
    // 映射错误：NotFound / archived → NotFound；未声明 client_consumable → PermissionDenied
    if !def.ClientConsumable || def.SelfConsumeQuota < 1 { return nil, ErrNotClientConsumable }

    return a.repos.DB.RunInTx(ctx, func(tx) error {
        // ① 幂等重放优先（沿用 Stripe 语义，键空间项目级、不区分动词）。
        if replay := tx.Ledger.GetByIdempotencyKey(p.ProjectID, cmd.Key); replay != nil {
            return respondReplay(replay)            // 附 quota_used 计算
        }
        // ② 通道配额：当日 origin=self_consume 的 consume 条目数。
        used := tx.Ledger.CountConsume(p.ProjectID, p.UserID, def.ID,
                                        origin="self_consume", window=utcDate(now))
        if used >= def.SelfConsumeQuota {
            return QuotaExceeded{RetryAfter: nextUtcMidnight(now)}  // ResourceExhausted
        }
        // ③ 惰性 grant（当日一次；键/量/过期时刻全部系统派生，重放无害）。
        grantKey := "selfserve:" + def.ID + ":" + p.UserID + ":" + utcDate(now)
        tx.Grant(GrantCommand{
            OwnerID: p.UserID, DefCode: def.Code, Quantity: int64(def.SelfConsumeQuota),
            IdempotencyKey: grantKey, ExpiresAt: &nextUtcMidnight(now),
            RefType: "self_consume", Origin: "server",   // 系统发放，origin 仍 server
        })                                              // InsertIfAbsent 双检，重放静默跳过
        // ④ 扣减：复用领域 Consume（FEFO + holdings FOR UPDATE + OCC + outbox）。
        //    同用户资产写在 holdings 行锁上天然串行 → ② 的计数判定无并发竞态。
        entries := tx.Consume(ConsumeCommand{
            OwnerID: p.UserID, DefCode: def.Code, Quantity: 1,
            IdempotencyKey: cmd.Key, RefType: cmd.RefType, RefID: cmd.RefID,
            Origin: "self_consume",
        })
        return respond(entries, quotaUsed: used+1, quotaLimit: def.SelfConsumeQuota)
    })
}
```

并发正确性：惰性 grant 与 consume 均走领域写路径的 `ListForUpdate`（holdings 行 FOR UPDATE），同一用户的资产写串行化，步骤②的 count→insert 竞态被行锁消除；跨用户无共享状态。领域 `Consume` 余额不足返回 `ErrInsufficient`（正常不会发生，除非同 def 被服务端消费花掉——如实透出）。

### 5. 错误语义

| 情形 | gRPC code | 结构化 Reason（ErrorInfo） | 客户端语义 |
|------|-----------|---------------------------|-----------|
| def 未声明 `client_consumable` / quota<1 | PermissionDenied | — | 入口不该出现（fail-closed，存量项目全量此态） |
| def 不存在 / archived | NotFound | — | — |
| 幂等键缺失/超长 | InvalidArgument | — | protovalidate + 领域校验双拦 |
| 当日通道配额用尽 | ResourceExhausted | `ASSETS.SELF_CONSUME_QUOTA_EXCEEDED` + RetryInfo（下窗口） | 正常业务态：UI「明天再来」 |
| 余额不足（同 def 被其他消费花掉） | FailedPrecondition | — | 异常态：沿用 server 面 consume 同款映射（`mapWriteError`） |

配额超额与余额不足刻意分码：前者是**预期内业务终态**（与限流同族，ResourceExhausted），后者是**预期外状态不一致**（FailedPrecondition，沿用现状，`internal/app/assets/assets.go mapWriteError` 不改）。`ASSETS.SELF_CONSUME_QUOTA_EXCEEDED` 走既有 `errdetails.ErrorInfo.Reason` 惯例（同 `RATE_LIMIT.EXCEEDED` / `EVENTS.RESUME_EXPIRED`）。

### 6. Authz（method_auth / authz-matrix / 纵深防御）

- proto：client 面服务级 `service_auth = { default_access: ACCESS_END_USER }` 已声明，`SelfConsume` 继承，permissions 归一 `["users"]`（`internal/runtime/authz_policy.go`）；**无新 scope、无新权限档位**。
- 拦截器：API key 凭证在 END_USER permission 门方法上一律拒绝（`internal/api/interceptor/jwt.go`），console 会话同拒——self-consume 只能由登录态（含匿名会话）JWT 调用。
- use-case：`RequireEndUser` 二次断言（纵深防御惯例，同 `requireAssetWrite`）。
- 文档：`task gen:authz-matrix` 重生成 `docs/developer/authz-matrix.md`（字节锁测试强制）。

## Data Model Changes

`internal/infra/projectschema/migrations/000013_asset_self_consume.{up,down}.sql`（编号以实施时为准）：

```sql
ALTER TABLE {{schema}}.asset_defs
  ADD COLUMN client_consumable BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN self_consume_quota INTEGER NOT NULL DEFAULT 0;
ALTER TABLE {{schema}}.asset_defs
  ADD CONSTRAINT asset_defs_self_consume_shape CHECK (
    client_consumable = FALSE OR (self_consume_quota >= 1));

ALTER TABLE {{schema}}.asset_ledger_entries
  ADD COLUMN origin TEXT NOT NULL DEFAULT 'server';
-- 通道配额计数索引（部分索引，仅 self_consume 行）
CREATE INDEX asset_ledger_self_consume_count
  ON {{schema}}.asset_ledger_entries (project_id, owner_id, def_id, created_at)
  WHERE origin = 'self_consume' AND kind = 'consume';
```

- 存量 def 全部落 `FALSE/0` ⇒ 存量项目 fail-closed，零行为变化；存量账本行 `origin='server'`，语义不变。
- `origin` 不参与流水重放与 `Reconcile`（重放只依赖 kind/delta），对账不变量不动。
- 迁移机制沿用 projectschema 每项目 schema 应用管道；down 迁移对称 DROP。
- bun 侧：`internal/infra/bun/model/assets.go` 补两模型字段（`asset_defs` 两列 + ledger `origin`），**UpdateAssetDef 列白名单显式登记** `client_consumable, self_consume_quota`（`internal/infra/bun/bunrepo/assets_repo.go:115-118`；漏登记 = "改不动"，update_guard 护栏会红）。
- 领域层：`internal/domain/assets` 补 `Def.ClientConsumable/SelfConsumeQuota`、`LedgerEntry.Origin`、命令结构体 `Origin` 字段与哨兵错误 `ErrNotClientConsumable`/`ErrSelfConsumeQuota`。

## Security & Privacy Considerations

1. **威胁模型（诚实声明）**：`isEnded` 可伪造，本机制不验证观看真实性。攻击者伪造信号的收益上限 = 每日 K 次/账号，与正常玩家相同——损失上限是运营可显式定价的常量。这是无服务端回调平台上的行业标准做法（PlayFire/Epic 等激励视频经济同理）。反滥用增强（设备指纹、多账号归一）超出范围。
2. **防汇集**：`client_consumable` 强制 `tradable=false`（def 不变量 + 既有 Transfer `ErrNotTradable` 检查自然拦截），额度无法跨账号转移。多账号各自 K 次为账号粒度限额的既定语义。
3. **流入不可操纵**：惰性 grant 的键、数量、过期时刻全部服务端派生；用户输入仅 `def_code/idempotency_key/ref_type/ref_id`，其中 idempotency_key 只影响**自己的**去重与重放，无法影响他人或额外获取额度。
4. **配额判定 fail-closed**：配额判定在 DB 事务内（账本计数），不依赖 Redis（既有 per-user 限流为 fail-open，不构成经济保障；且 per-user 1000/min 全局限流与每日 K 次配额相互独立、层次不同）。
5. **重放与并发**：幂等键项目级 UNIQUE 永久去重（`asset_ledger_idempotency` 约束 + `InsertIfAbsent` 双检）；同用户 holdings 行锁串行化，配额无超发窗口。
6. **用户可控数据入账本**：一期仅 `ref_type/ref_id`（定长字符串，protovalidate 上限），不收自由 JSON；账本对外投影（client 面 ledger 查询）本来只回本人数据。
7. **审计**：每次 self-consume 即一条 consume 账本条目（origin 标注）——assets 子系统的审计模型就是账本（D11），不另发 audit 事件；def 策略变更（开启 client_consumable）的审计随既有 server 面 def CRUD 的审计行为走（核对点见 Open Questions）。

## Observability

- 复用 `doWrite` 的 Prometheus 写指标（`internal/app/assets/write.go`）；
- 新增 `torchwood_assets_self_consume_total{project, def, result=ok|quota_exceeded|insufficient|denied}` 计数器，运营侧观测配额命中率与滥用迹象；
- 惰性 grant 走既有 grant 指标，不重复计数口径。

## Rollout Plan / PR 切片

| PR | 内容 | 验收锚点 |
|----|------|---------|
| PR-1 数据层 | 迁移 000013 + bun 模型/白名单 + 领域字段/哨兵错误 + server 面 def CRUD 策略字段（app 层不变量校验：tradable 冲突、quota>=1）+ Console def 表单字段 | `task test`；存量项目回归（新列默认值零影响）；update_guard 绿 |
| PR-2 用例层 | `origin` 列贯通（领域 Consume/Grant 命令 + repo 映射）+ `SelfConsume` use-case（惰性 grant、通道配额、幂等、错误映射）+ 指标 | use-case 单测矩阵（见下）+ 并发集成测试（同用户并发不超 K）+ `Reconcile` zero_drift |
| PR-3 API/SDK | client proto `SelfConsume` + `task generate:proto` + clientgrpc handler + gateway 注册 + `task gen:authz-matrix` + Go/TS SDK 方法 + 契约测试登记 + `docs/developer` 资产文档 | `task generate:all`、`task wire:all`、authz-matrix 字节锁绿、TS contract.test 绿 |

每 PR 沿用派发稿红线自查（`docs/prompts/implement-v3.md` 模板）：金额/数量无 float、终端用户无**流入**写入口、每笔变动可追 ledger entry。

## Key Decisions（建议，待 owner 拍板）

| # | 决策 | 理由 |
|---|------|------|
| K1 | 采用方案 A（def 级策略 + 单端点），方案 B（函数客户端调用面）作为独立演进后置 | B 工程量大一个量级且引入新的凭证/配额面；A 是 B 无法替代的最小闭环 |
| K2 | D6 修订为 D6-v2（§Background 表述）：流入/调整仍全禁，consume 为 def 显式 opt-in 例外 | 红线实质（流入服务端权威、防作弊、纵深防御）全保留，字面收窄 |
| K3 | 额度模型 M2（惰性 grant 余额型 + origin 通道配额），M1 落选 | M1 破坏 D8 统一五动词与 D11 对账不变量；M2 免费获得过期/余额可见/adjust/对账 |
| K4 | `client_consumable=true` 强制 `tradable=false`（def 不变量） | 防跨账号汇集额度 |
| K5 | 惰性 grant 键系统派生 `selfserve:{def_id}:{owner_id}:{YYYYMMDD}`，expires_at=窗口结束 | 流入不可操纵；当日不结转精确落地 |
| K6 | 配额窗口 = 自然日 UTC（一期）；错误码语义 §5 | 见 Open Questions Q1 |
| K7 | 一期不收业务 metadata、不做 cooldown | 最小面；账本 ref 字段够一期审计 |

## Open Questions（需拍板）

1. **窗口时区**：自然日按 UTC（本稿倾向，实现最简、无歧义）还是项目级/def 级可配时区（对微信游戏 UTC=北京 8:00 重置，运营常预期本地 0:00）？若配置化，配置放 def 还是项目？行业先例：PlayFab Rewarded Ads 同样按 00:00 UTC 重置，另提供 hourly/two-hours 粒度（竞品分析 §5.2）。
2. **惰性 grant 形态**：满额预发（本稿：账本每日一条 `+K` 条目，用户可见「今日额度」）vs 按需补发（每 consume 补 1，账本更「瘦」但键/过期逻辑复杂）？
3. **cooldown**：是否一期就要「两次消费最小间隔」？账本可查最近条目时间，实现不难但加面；本稿建议后置。
4. **业务载荷**：`context`/metadata（Struct）是否开放？上限与脱敏策略？一期仅 ref_type/ref_id 是否够（如需传关卡 ID/广告位 ID，ref 字段可承载）。
5. **def 策略变更审计**：既有 server 面 `Create/UpdateAssetDef` 是否已进 `audit_logs`？若否，开启 `client_consumable` 这类经济敏感配置变更是否要求审计事件？
6. **client 账本投影**：client 面 `AssetLedgerEntry` 是否暴露 `origin`（终端用户可区分系统发放/自服务消费）？本稿倾向一期不加（最小面，response 已带 quota_used）。
7. **结转型自服务额度**：若运营要「自服务额度可结转」，是放开惰性 grant 的 expires_at 策略（def 级第三开关）还是引导走服务端 grant？本稿倾向后者。
8. **方案 B 排期**：函数客户端调用面（埋点摄入、成绩校验等场景）是否进 roadmap 独立立项？

## Alternatives Considered

- **方案 B：函数客户端调用面**（per-function 开关 + 执行身份注入 + 调用配额）：行业通用骨干（CloudScript / Cloud Code / Nakama RPC / 微信云函数同款），更通用（顺带解决埋点、成绩回放校验）；前置是 execution principal 与 per-user/per-function 限流（行业入场券），工程量约为本稿的 2–3 倍。A 的 def 策略模型可平滑映射为未来 B 的函数注解，两者不冲突；B 立项时 A 无返工。
- **方案 A'：广告奖励回调接收器（SSV 链路）**：微信激励广告有官方可选服务端验证；若目标游戏可接入，黄金链路是「SSV 回调 → torchwood serverhttp 验签（复用 payments webhook D7 模式）→ 按 transaction_id 去重 → 服务端发放」。代价是按广告平台逐一做适配器，且依赖 B 的「公开 HTTP 触发器」类设施（评审 04-platform-capabilities.md PC-5）。
- **方案 C：游戏自建无状态中转服务持 server key**：不改 torchwood，但违背「无自建游戏服务器」接入初衷，且增加域名/运维/密钥面，仅作兜底对照。
- **配额独立子系统**（「每日任务计数」不走资产）：语义最贴，但新建一套计数/查询/审计设施，与「账本可审计」诉求重复建设；K3 落选 M1 的同一逻辑。
- **竞品先例**：PlayFab Rewarded Ads 与本稿逐项同构（客户端自报 `Client/RewardAdActivity` + 平台校验 + 平台强制每日限额 3–4 次、00:00 UTC 重置、无 SSV 靠限额兜底），是本路线的成熟产品化证据；「花服务端权威 / 发软硬隔离」的行业分野与 torchwood 的定位见 `economy-client-write-competitive-analysis.md` §五。

## References

- `docs/design/economy-client-write-competitive-analysis.md`（竞品分析：PlayFab/Unity/Nakama/Firebase/云开发等对客户端写经济与广告奖励的处理）
- `docs/design/v3-payments-economy.md`（D1-D19；§2.7 红线原文；本稿修订 D6）
- `docs/review/saas-baas-design-2026/05-economy.md:45,334`（红线再背书与消费缺口点名）
- `docs/developer/06-databases.md` §0（模块边界，本稿不涉数据面）
- `docs/developer/17-update-write-guard.md`（bun 更新写护栏）
- 代码锚点：`internal/app/assets/authz.go`（requireAssetWrite）、`internal/domain/assets/write.go`（领域写路径）、`internal/infra/bun/bunrepo/assets_repo.go`（幂等 InsertIfAbsent）、`internal/infra/projectschema/migrations/000005_assets.up.sql`（表结构）
