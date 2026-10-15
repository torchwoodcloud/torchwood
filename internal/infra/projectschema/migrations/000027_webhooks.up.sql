-- Webhooks 一等公民子系统（docs/design/functions-v4.md §6，D5 拍板）：
--   webhook_subscriptions  订阅实体：URL（https 强制，应用层 + protovalidate
--                          双重校验）+ 订阅串集（TEXT[]，eventmatch 语法，
--                          1..64 条 × ≤200B，应用层校验）+ 状态
--                          （enabled/paused）+ per-project 签名密钥加密存储
--                          （secretbox AES-256-GCM，enc:v1: 前缀，密钥材料 =
--                          security.encryption_key 回退 jwt.secret——与
--                          OAuth client secret / TOTP secret 同一 KDF 派生
--                          入口）。previous_secret_enc + secret_rotated_at
--                          承载双密钥轮换窗口（v2 签名；新钥即刻生效，旧钥
--                          保留一个宽限窗）。明文不落库，API 面只出指纹。
--   webhook_deliveries     投递行（重投事实源 + DLQ）：payload JSONB 存
--                          CloudEvents 1.0 structured mode 信封整体（重试
--                          期间载荷不变）；status ∈ {pending, delivered,
--                          failed}（failed 即 DLQ）；attempts 计数 +
--                          next_attempt_at 承载重投调度（领取即 CAS 推进，
--                          先 CAS 后投递，与 cron ClaimDueCron 同款红线）。
-- 占位符 {{schema}} 由 Apply 替换为 quoteIdent。

CREATE TABLE {{schema}}.webhook_subscriptions (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    -- 投递终点（https:// 强制；出站 SSRF 守卫在拨号点，不在此约束）。
    url TEXT NOT NULL,
    -- 订阅串集（eventmatch 语法：databases.{db}.collections.{col}.documents.{op}
    -- 或系统行为事件目录形态）；应用层逐条校验后整体替换写。
    events TEXT[] NOT NULL,
    -- enabled = 投递中；paused = 暂停（快照扫描跳过，不投递不删行）。
    status TEXT NOT NULL DEFAULT 'enabled'
        CONSTRAINT webhook_subscriptions_status_check CHECK (status IN ('enabled', 'paused')),
    -- 签名密钥（secretbox 密文，enc:v1: 前缀）。当前钥。
    secret_enc TEXT NOT NULL,
    -- 轮换窗口内的旧钥密文（NULL = 无轮换或窗口已过由读取侧按时刻判定）。
    previous_secret_enc TEXT,
    -- 最近一次轮换时刻（读取侧据此判定旧钥是否仍在宽限窗内）。
    secret_rotated_at TIMESTAMPTZ,
    -- 密钥指纹（whsec_ + SHA-256 前 8 hex；管理面唯一可见形态）。
    secret_fingerprint TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 项目内列表（管理面 ListWebhooks）。
CREATE INDEX webhook_subscriptions_project
    ON {{schema}}.webhook_subscriptions (project_id, created_at DESC);

-- 订阅匹配器快照扫描（worker webhookConsumer，15s）：按项目加载启用订阅。
-- partial 索引与 function_triggers_event_scan 同款风格（status 由 partial
-- 条件保证，不重复进键）。
CREATE INDEX webhook_subscriptions_enabled_scan
    ON {{schema}}.webhook_subscriptions (project_id)
    WHERE status = 'enabled';

CREATE TABLE {{schema}}.webhook_deliveries (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    webhook_id TEXT NOT NULL REFERENCES {{schema}}.webhook_subscriptions(id) ON DELETE CASCADE,
    -- 事件幂等键（outbox event_id）：at-least-once 消费与停机补投的重复
    -- 由 (webhook_id, event_id) 唯一索引吸收——重复 INSERT 静默跳过。
    event_id TEXT NOT NULL,
    -- 事件 type（CloudEvents type = 事件目录名 / databases.documents.{op}）。
    event_type TEXT NOT NULL,
    -- outbox 全局序（事件产生序；0 = 未知序）。
    seq BIGINT NOT NULL DEFAULT 0,
    -- CloudEvents 1.0 structured mode 信封整体（重投期间载荷不变）。
    payload JSONB NOT NULL,
    -- pending = 待投/重投中；delivered = 送达；failed = 终态失败（DLQ）。
    status TEXT NOT NULL DEFAULT 'pending'
        CONSTRAINT webhook_deliveries_status_check CHECK (status IN ('pending', 'delivered', 'failed')),
    -- 已执行的投递尝试次数（含首投；领取即 +1，CAS 防多实例双投）。
    attempts INT NOT NULL DEFAULT 0,
    -- 下一次尝试时刻（领取即按重试梯度推进——投递中崩溃行退避后自愈，
    -- 不热循环）；终态语义 = 最后一次尝试时刻。
    next_attempt_at TIMESTAMPTZ NOT NULL,
    -- 最近一次尝试的响应码（2xx = 送达；0 = 未达 HTTP 层）。
    last_response_code INT,
    -- 最近一次失败的错误摘要（截断留档；成功后清空）。
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 到期扫描（worker 到期投递 loop，1min）：partial 收窄 pending——
-- delivered/failed 恒不被扫描。
CREATE INDEX webhook_deliveries_due
    ON {{schema}}.webhook_deliveries (next_attempt_at)
    WHERE status = 'pending';

-- 投递行浏览（管理面 ListDeliveries：webhook 维度按时间倒序分页）。
CREATE INDEX webhook_deliveries_webhook_created
    ON {{schema}}.webhook_deliveries (webhook_id, created_at DESC);

-- 事件幂等（at-least-once 去重吸收面；重复插入 DO NOTHING）。
CREATE UNIQUE INDEX webhook_deliveries_idem
    ON {{schema}}.webhook_deliveries (webhook_id, event_id);

-- 保留窗清理（终态行按 created_at 低频清理；pending 行由重试窗终态化）。
CREATE INDEX webhook_deliveries_terminal_created
    ON {{schema}}.webhook_deliveries (created_at)
    WHERE status IN ('delivered', 'failed');
