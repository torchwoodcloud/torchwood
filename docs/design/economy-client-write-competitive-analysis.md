# 客户端写经济与广告奖励：BaaS 竞品分析

> 状态：调研存档（2026-09-09）；支撑 `docs/design/client-self-consume-assets.md`（方案 A）与方案 B（函数客户端调用面）的拍板
> 方法：官方文档为主（firebase.google.com / learn.microsoft.com/playfab / docs.unity.com / heroiclabs.com / developers.weixin.qq.com / docs.cloudbase.net / docs.leancloud.cn 等），社区文章交叉印证；来源见文末
> 结论时效：2026-09；各平台能力以官方文档现行表述为准

---

## TL;DR

1. **前提修正（最重要）**：微信激励视频广告**有官方可选的服务端验证（SSV）**——「激励广告服务端验证」（流量主后台配置，AES 加密回调 + transaction_id），基础库 ≥ v3.10.3。「无服务端回调、只有 isEnded」的说法已不完全成立；但 SSV 是可选项，官方自己的建议策略仍是「前端先发奖 + 服务端回调验证兜底」。
2. 行业对「客户端写经济」的共识分野：**「花」（购买/兑换/扣款）一律服务端权威，行业一致**；**「发」的隔离多为软隔离**（PlayFab v1 甚至有 Client/AddUserVirtualCurrency；Unity 默认玩家可写 Economy，要靠 Access Control 显式封禁），硬隔离（Nakama/GameSparks/Parse 惯例/微信云开发惯例）是少数派。torchwood 的 D6 是最严格的硬隔离。
3. **方案 A 有直接的产品化先例：PlayFab Rewarded Ads**——客户端自报观看（`Client/RewardAdActivity`）+ 平台侧校验 + 平台强制每日限额（官方建议 3–4 次/日、00:00 UTC 重置、可按小时粒度）。与 A 稿设计几乎逐项同构。
4. **方案 B 是行业通用骨干**（CloudScript / Cloud Code / RPC / Cloud Code / 云函数），且**身份自动注入是全行业标配**（currentPlayerId / context.playerId / request.user / openid / auth token），没有任何一家让开发者往函数里手工塞长期 API key——torchwood 现状（函数内零注入、variables 明文存 key）是行业之外的落后态，execution principal 正规化是做 B 的前提，也是补齐现状短板。
5. 广告奖励的三个层级：**SSV（AdMob / ironSource / 微信，可验证）＞ 自报+平台限额（PlayFab Rewarded Ads）＞ 纯客户端自管**。行业最佳实践链路：广告平台 SSV 回调 → 开发者服务端（通常就是可调用函数的 HTTP 端点）→ 验签去重 → 服务端权威发放，限额作成本兜底。
6. 竞争视角：微信云开发（openid 注入的云函数 = B 模式）是小游戏生态的事实标准；「货币/库存只能云函数写」是其社区惯例。

---

## 一、前提修正：微信激励视频的 SSV

官方文档：《激励广告服务端验证接入指引》（developers.weixin.qq.com/minigame/dev/guide/security/antiadcheat.html）：

> 「服务端验证是一种**可选**的验证方式……为奖励下发提供额外的保护机制，以规避客户端的作弊行为」；设计初衷「帮助开发者应对可能存在的外挂作弊情况，如**直接跳过广告获取奖励**」；「若开发者判断游戏当前外挂作弊风险较小，则无需接入」。

机制要点（均原文）：

- 配置入口：流量主后台 → 广告管理 → 激励广告 →「服务端奖励回调入口」，填回调 URL（http/https 域名，不能 IP）+ Token + EncodingAESKey；
- 两步握手：GET URL 校验（Token+timestamp+nonce 字典序 sha256 验签）；真实回调 `encrypt` = AES-256-CBC（pkcs7），解密 JSON 含 **transaction_id / user_id / reward_item / reward_amount / custom_data**；
- 客户端经 `setServerSideVerificationData`（userId/rewardItem/customData）透传身份，**基础库最低 v3.10.3**，须在 `show()` 前调用；
- 回包 `is_valid`；回调超时 1s、重试 3 次（15s/22.5s/33.75s）→ **服务端必须按 transaction_id 去重**；
- 官方策略建议：「建议开发者在收到前端回调时下发奖励，再在收到服务端回调时进行验证，作为保障手段」——**前端先发、SSV 兜底对账**，而非严格 SSV-first；
- `isEnded` 官方口径仅为中性字段描述（「视频是否是在用户完整观看的情况下被关闭的」）；未明说不可信，不可信定性从反作弊文档反推。另有平台级「每个用户每天可观看激励式视频广告的次数有限」（数值不公开）。

**含义**：本仓库此前讨论（含 A 稿 Background）基于「微信无服务端回调」的前提，需要修正为「有可选 SSV；不接 SSV 时才退化为 isEnded + 限额」。

## 二、行业全景速查表

| 平台 | 客户端直写经济数据 | 客户端可调用服务端代码 | 函数身份注入 | 函数凭证 | 广告奖励设施 | 反作弊 |
|---|---|---|---|---|---|---|
| PlayFab | 花可直发（PurchaseItem/RedeemCoupon 服务端权威）；v1 连「发」都可达（Client/AddUserVirtualCurrency，legacy）；v2 玩家 token 可达 Add/SubtractInventoryItems（软隔离） | CloudScript / CloudScript using Azure Functions | currentPlayerId，官方：「server-controlled and safe」；args「zero trust」 | 内置 `server` 对象全 Server API；AF 版需手工配 Title Secret Key | **Rewarded Ads 内置**（自报 + 平台限额，无 SSV） | per-entity throttling；无设备证明 |
| Unity UGS | Economy 端点玩家 token 默认可读写（含 Add/Subtract），需 Access Control 策略显式封禁（官方示例策略 deny-economy-write-access） | Cloud Code（JS Scripts / C# Modules） | context.playerId + accessToken（可代表玩家调服务）；跨玩家走 service account | 自动注入，无需手工 key | 无内置；Unity LevelPlay/ironSource **有 S2S 回调**（MD5 签名 + eventId 去重 + IP 白名单） | Access Control + 600 req/min/player；无设备证明 |
| Nakama | **无任何经济写 API**；Wallet「cannot be modified by clients」；Storage 仅 owner 写、无公共写 | RPC（Go/Lua/TS 运行时） | ctx 注入 user_id（server-to-server 无 user id） | 运行时即服务端（nk.*） | 无 | wallet ledger 账本；限流需自实现 |
| GameSparks（已停服） | 无（客户端只能发事件） | 云代码（Spark），Spark.getPlayer() 注入 | 平台内置能力 | 无 | 不详（文档已下线） | — |
| Firebase | 可直写（Security Rules 门禁） | callable functions（httpsCallable） | auth token 自动携带，函数读 request.auth | Admin SDK（平台服务身份，绕 Rules） | **AdMob SSV 官方闭环**（ECDSA 验签 + Functions） | **App Check 一等公民**（Play Integrity 等，可对 callable 强制，官方直指 billing fraud） |
| Supabase | 可直写（RLS） | Edge Functions | verify_jwt 默认校验；withSupabase 客户端按调用者 RLS | service_role key 绕 RLS（env 注入） | 无 | 无 |
| Appwrite | 可直写（资源权限数组 + Row Security） | Functions，**Execute 权限可授 users 角色** | x-appwrite-user-jwt 头 | API key（scope 收权） | 无 | 无 |
| Parse | 可，生态惯例锁死（CLP locked mode + ACL） | Cloud Code（Parse.Cloud.run） | request.user 自动注入（masterKey 调用时无） | masterKey 显式传 + masterKeyIps | 无 | 内建 rateLimit 配置 |
| Amplify/AppSync | 可直写（@auth/.authorization()，默认 deny 但脚手架带全局 public） | 自定义 query/mutation → Lambda | $ctx.identity（sub/claims） | allow.resource()（默认管理员级） | 无 | 无 |
| 微信云开发 | 可直写（四预设权限 + 安全规则）；**惯例：库存/货币「仅管理端可写」** | 云函数 wx.cloud.callFunction | **openid/unionid 自动注入**，「开发者无需校验 openid 的正确性」 | 云函数=管理端权限（绕安全规则），事务仅云函数可用 | 微信 SSV（可选）+ 平台每日观看上限 | 环境配额闸门（并发/月调用），异常滥用封禁 |
| LeanCloud | 可直写（ACL/CLP）；惯例：敏感 Class 关写权限、云引擎中转 | 云引擎云函数 | 注入用户身份 | Master Key（服务端） | 无 | API 请求数日限额（429） |
| **torchwood 现状** | **不可（D6 硬隔离）** | **无 client 面** | **无注入** | **开发者自塞 API key（明文 variables）** | **无** | 全局限流（fail-open）+ 全局 run 信号量 16 |

## 三、模式一：客户端可调用的服务端代码（方案 B 的行业形态）

所有主流平台都有这一层，形态各异但三个共性雷打不动：

1. **身份自动注入且不可伪造**：PlayFab「currentPlayerId is server-controlled and safe」、UGS context.playerId、Parse request.user、Nakama ctx user_id、微信云函数 openid（「微信已经完成了这部分鉴权」）、Firebase auth token 自动携带。同时**对客户端传参一律零信任**（PlayFab args「zero trust」是代表性表述）。
2. **平台能力经特权通道获得，不经手工 API key**：Admin SDK / service_role / masterKey / nk.* / 云函数管理端权限 / 内置 server 对象。唯一反例是 PlayFab CloudScript AF 的 Title Secret Key 手工配置（老形态）与 Appwrite 的 API key env（scope 收权）。torchwood 现状「开发者往 variables 塞长期 API key」无行业同类。
3. **限流普遍存在**：UGS 600 req/min/player（最明确）、PlayFab per-entity throttling（固定窗口，key=调用/目标实体）、Parse rateLimit、微信云开发环境级配额（单函数并发 20、20 万次/月、超限即闸）。Nakama 是例外（需运行时自实现）。

对 B 的含义：**execution principal（平台注入执行身份）+ per-user 限流**不是可选项，是行业入场券。torchwood 若做 B，注入身份应携带 {project, function_id, 触发用户, 声明 scope}，函数发起的经济操作在账本 operator 记录函数+触发用户（对标 CloudScript 的 currentPlayerId 审计语义）。

## 四、模式二：声明式权限直写——没有一家把它用于货币

Firestore Rules / Supabase RLS / Appwrite 权限数组 / Amplify @auth / 云开发安全规则都是「客户端直写普通数据」的第一公民，但各家的货币/库存惯例无一例外收敛到服务端：

- Supabase：余额逻辑放 **security definer 函数/触发器**（客户端只能调用它改变余额，不能 UPDATE 余额行）；service_role「绕过 RLS，保留在服务端」。
- 微信云开发：预设权限「仅管理端可写」就是为「商品信息」类数据准备的；**事务（防超卖的原子扣减）只在云函数可用**；安全规则示例把订单删除设为「仅云函数端」。
- LeanCloud：敏感 Class「写权限完全关闭，客户端所有请求都通过云引擎中转」——文档背书「与自己搭建后端具有同样的数据安全性保障」。
- Parse：CLP locked mode + Cloud Code 全中转（五家里最「服务端优先」）。
- 值得注意：没有任何一家有「金融/货币数据禁用声明式规则直写」的**明文**禁令——指引全是间接的（预设场景 + 事务边界 + 惯例）。

torchwood 的资产子系统（D1：静态表 + 行锁 + 唯一约束 + 追加流水，不走动态文档层）与这个行业惯例完全同向，且走得更远（专用服务而非 RLS/规则）。**声明式直写路线在本仓库没有对标物，也无需对标。**

## 五、模式三：专用经济服务与「只能花不能发」（方案 A 的行业形态）

### 5.1 「花」的服务端权威是行业一致形态

- PlayFab Client/PurchaseItem：按目录价格扣虚拟货币，余额不足返回 `InsufficientFunds`——客户端发起、服务端校验扣款；Economy v2 `PurchaseInventoryItems` 价格「must match a value configured in the Catalog or specified Store」+ `IdempotencyId`（保留约 14 天防重放）。
- PlayFab Client/RedeemCoupon：客户端可调的兑换码核销，服务端权威发放——「客户端可发起的服务端权威核销动词」的直接先例。
- Unity Economy 虚拟购买：价格由 Economy 配置权威校验。
- Nakama / GameSparks：连「花」都不提供客户端端点，逻辑写在服务端代码里。

### 5.2 PlayFab Rewarded Ads = 方案 A 的产品化先例（逐项对照）

| PlayFab Rewarded Ads | torchwood 方案 A（client-self-consume-assets.md） |
|---|---|
| Game Manager 配置 Ad Placement + Reward（可按 Segment 覆盖） | asset def 声明 `client_consumable` + `self_consume_quota` |
| Client/GetAdPlacements 拉取配置 | client 面 AssetDef 投影暴露策略字段 |
| 「ad SDK reports completed → call Client/RewardAdActivity」客户端自报 | POST /v1/assets:self-consume |
| 「It first checks that the reward is valid, then processes it」 | use-case 校验策略声明 + 配额 + 余额 |
| 每日限额：「limited this to 4 times per day, per player」官方建议「common to set a limit of 3 times per day」 | `self_consume_quota` 通道配额 |
| 「daily resets on this counter occur at **00:00 UTC**」，可按 daily/hourly/every-two-hours 重置 | A 稿开放问题 Q1（UTC 自然日）——**行业先例直接支持 UTC** |
| 无 SSV，观看真实性不验证，靠限额兜底损失 | 同（威胁模型声明：损失上限 = K/日/账号） |

### 5.3 「发」的隔离：软隔离是多数，硬隔离是少数——且软隔离有代价

- 软隔离：PlayFab v1 的 `Client/AddUserVirtualCurrency`（客户端 token 可给自己加钱，仅标 legacy）；v2 `AddInventoryItems` 同样玩家 token 可达；Unity 默认玩家可写 Economy，官方靠 Access Control 策略（示例策略名就叫 deny-economy-write-access）+ Cloud Code 引导收敛。**「客户端不能发」在这些平台是要配置出来的目标态，不是出厂状态。**
- 硬隔离：Nakama（「wallet cannot be modified by clients」）、GameSparks（客户端协议里不存在经济写端点）、Parse/云开发/LeanCloud 的生态惯例。
- 软隔离的代价：客户端 token 一旦泄漏即可直接铸币，平台层无止损。torchwood 的 D6（API 面收口 + use-case 二次断言）属于最严格的硬隔离；**A 稿的 D6-v2（受控消费例外）恰好把 torchwood 带到 PlayFab 的产品化位置，而不牺牲硬隔离骨架**。

## 六、广告奖励的三个层级与反作弊

| 层级 | 代表 | 验证强度 | 奖励发放链路 |
|---|---|---|---|
| L1 SSV | AdMob SSV（ECDSA 验签）、ironSource/LevelPlay S2S（MD5 签名 + eventId 去重 + IP 白名单）、**微信激励广告服务端验证**（AES + transaction_id） | 可验证观看完成 | 回调 → 开发者服务端（通常是可调用函数的 HTTP 端点）→ 验签去重 → 服务端权威发放；客户端事件仅作即时 UX（ironSource 官方：「the recommended best practice is to use the server-side event to trigger the user reward」） |
| L2 自报 + 平台限额 | PlayFab Rewarded Ads | 不可验证 | 客户端自报 + 平台强制每日限额（3–4 次/日，UTC 重置） |
| L3 纯客户端自管 | 无后端的小游戏 | 无 | 本地计数（作弊=无限复活） |

反作弊设施：设备证明（Play Integrity / App Check / App Attest）只有 Firebase 做成一等公民且官方把价值直指「计费欺诈」；游戏平台（PlayFab/UGS/Nakama）均无内置。通用兜底 = per-user 限流 + 事务账本/幂等键 + 封禁。

## 七、对 torchwood 决策的映射

1. **方案 A（self-consume）**：有 PlayFab Rewarded Ads 这个成熟产品先例，设计逐项同构（含 UTC 窗口）；D6-v2 修订把 torchwood 从「比 PlayFab 更严」带到「与 PlayFab 产品化位置对齐、保持硬隔离骨架」。在不接 SSV 或 SSV 不可用（基础库 < v3.10.3）的场景，这是行业验证过的正确形态。
2. **方案 B（客户端可调用函数）**：行业通用骨干；微信云函数就是本生态（微信小游戏）的事实标准。做 B 的前置 = execution principal + per-user/per-function 限流（行业入场券，见 §三）；现状「函数内零注入 + variables 明文塞 key」是行业之外的状态，无论是否开 client 面，都应先补 execution principal（同时修复已有凭证风险，对标 PC-6）。
3. **新出现的方案 A'（广告奖励回调接收器）**：若用户游戏可接微信 SSV，黄金链路是「SSV 回调 → torchwood 验签（复用 payments webhook 的 serverhttp/D7 模式）去重 → 服务端发放」。先例充分（Firebase AdMob SSV + Functions 官方闭环、uni-ad 的 uniAdCallback 云函数、ironSource S2S → Cloud Code）。代价：按广告平台逐一做适配器，平台绑定性强；且回调 URL 需要平台提供公网端点（等价于 B 的「HTTP 触发器」设施，即评审 04-platform-capabilities.md PC-5 的触发器模块）。
4. **竞争格局**：微信云开发（B 模式 + openid 注入）是小游戏后端的事实标准。torchwood 若只有 A，简单场景体验可打平甚至更优（声明式 def 策略 vs 写函数）；若长期没有 B，复杂场景（埋点、成绩校验、任何自定义服务端逻辑）接入成本高于云开发。
5. **建议的决策顺序**：
   - 先核实（用户侧）：目标游戏基础库 ≥ v3.10.3？流量主后台该广告位有无「服务端奖励回调入口」？是否愿意接 SSV？
   - 接 SSV → 优先 A'（或 B 的 HTTP 触发器 + 函数内验签），isEnded 仅作 UX 即时反馈 + SSV 对账兜底（微信官方自己建议的策略）；self-consume 降级为「SSV 前即时暂发 + SSV 后核销」的可选增强。
   - 不接 SSV → 方案 A 成立（PlayFab 同款），A 稿照评审推进；B 的第一步（execution principal）并行排期。
   - 无论哪条路：execution principal + 触发器模块是共同地基（PC-5/PC-6），先做不亏。

## 来源清单

**PlayFab**：learn.microsoft.com/en-us/rest/api/playfab/client/player-item-management/{purchase-item,redeem-coupon,add-user-virtual-currency}；/en-us/gaming/playfab/features/economy-v2/inventory；/en-us/gaming/playfab/features/automation/cloudscript/writing-custom-cloudscript；/en-us/xbox/playfab/{live-service-management/service-gateway/{automation/cloudscript-af/quickstart,throttling/what-is-throttling},economy-monetization/rewarded-ads/quickstart,rewarded-ads/}；/en-us/rest/api/playfab/server/account-management/ban-users

**Unity UGS**：docs.unity.com/en-us/services/access-control；/en-us/cloud-code/{server-access-control,scripts/how-to-guides/unity-services-integration,scripts/reference/limits}；/en-us/economy；/en-us/grow/levelplay/platform/settings/event-handlers；discussions.unity.com/t/921973

**Nakama**：heroiclabs.com/docs/nakama/concepts/{storage/permissions,wallet}；/docs/nakama/server-framework/{introduction,runtime-context,guarding-apis}

**Firebase/AdMob**：firebase.google.com/docs/{firestore/security/get-started,firestore/security/rules-conditions,functions/callable,app-check,remote-config}；developers.google.com/admob/android/ssv；support.google.com/admob/answer/9603226

**Supabase**：supabase.com/docs/guides/database/postgres/row-level-security；/guides/functions/{auth,limits}

**Appwrite**：appwrite.io/docs/advanced/security/permissions；/docs/products/functions/execute；/docs/references/cloud/server-deno/functions；/docs/products/databases/documents

**Parse**：docs.parseplatform.org/cloudcode/guide/；/rest/guide/#security；back4app.com/docs/security/parse-security；github.com/parse-community/parse-server

**Amplify**：docs.amplify.aws（customize-authz、custom-business-logic、grant-lambda-function-access-to-api、set-up-function）；docs.aws.amazon.com/appsync/latest/devguide/resolver-context-reference.html

**微信生态**：developers.weixin.qq.com/minigame/dev/guide/security/antiadcheat.html（激励广告服务端验证）；/minigame/dev/guide/open-ability/ad/rewarded-video-ad.html；/minigame/dev/wxcloud/guide/{functions,database/permission,database/legacy-permission,database/security-rules,database/transaction}.html；/minigame/dev/wxcloud/reference/quota.html；/minigame/dev/wxcloud/billing/price.html；ad.weixin.qq.com/guide/1195；uniapp.dcloud.net.cn/uni-ad/ad-rewarded-video.html；docs.cloudbase.net（authentication/method/anonymous、database/security-rules、cloud-function/security-rules、database/transaction、error-code/basic）；docs.leancloud.cn（sdk/engine/faq、sdk/storage/guide/acl、sdk/engine/functions/sdk）；leancloud.cn/pricing；juejin.cn/post/6844903831256432654；cloud.tencent.com.cn/developer/article/1638120
