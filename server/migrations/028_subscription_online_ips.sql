-- 028: 订阅真实在线 IP —— 让「订阅中心」显示"此刻哪些 IP 正在这条订阅上跑"。
--
-- 背景（2026-10-08 管理员要求「显示这个订阅目前那些 IP 在线」）
-- ----------------------------------------------------------------
-- 面板原本已有一个 active_ips 列，但它记的是**谁拉取过订阅链接**
-- （面板自己在 /sub/{token} 上看到的客户端地址，24 小时滑窗）。
-- 运维真正要问的是另一个问题：
--   「客户现在到底在用哪个 IP 跑流量？」
-- 这件事只有节点侧知道 —— 节点 portal 在**鉴权回调**里维护
-- 内存态 ip_tracker，窗口 180 秒，与限流判定用的是同一个常量。
--
-- 为什么这张表只存"当前快照"而不存历史：
--   在线 IP 是**状态**，不是**事件**。存历史需要持续写入每次变化，
--   而面板只想回答"此刻谁在线"。采集器每轮把该(订阅,节点)当前集合
--   整份替换掉即可 —— 不做增量 UPSERT，避免"漏删"导致的幽灵 IP。
--
-- 为什么不复用 subscription_usage 加一列：
--   那张表的主键是 (subscription_id, node_id)，一行一个账号，
--   而一个账号可以同时有多个在线 IP。塞不进单列，塞成 jsonb
--   就没法按 IP 检索。对"这台机器上还有谁在连"这类查询也不友好。
--
-- 与 active_ips 的关系：两者都保留，语义不同 ——
--   subscription_online_ips —— 正在跑流量的 IP（节点侧权威，180 秒窗）
--   active_ips             —— 拉取过订阅链接的 IP（面板侧，24 小时窗）

CREATE TABLE IF NOT EXISTS subscription_online_ips (
    subscription_id UUID NOT NULL REFERENCES subscription_tokens(id) ON DELETE CASCADE,
    node_id         UUID NOT NULL,

    -- 节点侧 ip_tracker 里的客户端地址。
    ip              TEXT NOT NULL,

    -- 节点侧记录的该地址最后一次出现的时间（epoch 秒，取自节点）。
    -- ⚠️ 存**节点**的时间而不是采集器的时间：节点时钟可能与面板有偏差，
    -- 而这个字段的用途是"看谁在线"，采到就已经晚了，再加一层采集延迟
    -- 会让界面上的时间戳看起来比真实情况更旧。
    last_seen       BIGINT NOT NULL DEFAULT 0,

    collected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- 同一 (订阅,节点) 下 IP 唯一：整份替换时靠它决定谁该消失。
    -- ⚠️ 不能只按 IP 做主键 —— 同一个客户端地址完全可以同时连多个节点，
    -- 那样一台机器的"1 个在线 IP"会在多节点行里各占一条，
    -- 订阅级聚合时就得去重，索引也失去意义。
    PRIMARY KEY (subscription_id, node_id, ip)
);

-- 按节点反查"这台机器上现在有哪些订阅在用"，节点页要用。
CREATE INDEX IF NOT EXISTS idx_subscription_online_ips_node
    ON subscription_online_ips (node_id);

-- 按 IP 反查"这个地址正在用哪些订阅"，排查单个客户端时用。
CREATE INDEX IF NOT EXISTS idx_subscription_online_ips_ip
    ON subscription_online_ips (ip);

COMMENT ON TABLE subscription_online_ips IS
    '订阅在各节点上的真实在线 IP（采集器每轮整份替换）。来自节点 portal 鉴权时的 ip_tracker，窗口 180 秒。与 subscription_tokens.active_ips 不同：本表是"正在跑流量"，那是"拉取过订阅链接"。';
COMMENT ON COLUMN subscription_online_ips.last_seen IS
    '该 IP 在节点侧最后一次出现的时间（epoch 秒）。用于界面显示"最后在线"，以及判断数据新鲜度。';