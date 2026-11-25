-- 025: 订阅用量落库 —— 让「订阅中心」列表能直接显示各订阅用了多少流量。
--
-- 背景（2026-10-02 管理员要求）
-- --------------------------
-- 用量数据**只存在于节点侧**：portal 的 users/list 里每个账号带
-- traffic_used_bytes。而订阅中心页面要显示"这条订阅用了多少"，
-- 就得跨节点聚合 —— 一条订阅在多个节点上各有账号（node_accounts 是
-- {节点id: {user_id, ...}}），逐个实时查要走 N 次节点 API。
--
-- 管理员选的是「采集器定期落库」：采集器本来每轮就在连各节点，
-- 顺手把各账号的 used_bytes 记下来，列表直接读本地表 —— 打开即见，不用等。
-- 代价是最长一轮采集的延迟（默认 30 秒），这对"看用量"完全够用。
--
-- 为什么按 (subscription_id, node_id) 而不是只按 subscription_id：
--   - 一个订阅在多个节点上各有账号，分开存才能看出"哪个节点吃掉了流量"
--     （这对排查"某台机器跑满带宽"是直接有用的信息）
--   - 聚合到订阅级只是 SUM，随时能算；反过来拆不开
--
-- 为什么不存 ip_limit / 到期时间这些：那些是**订阅自身**的属性，
-- 已经在 subscription_tokens 里，重复存一份必然分叉。

CREATE TABLE IF NOT EXISTS subscription_usage (
    subscription_id UUID NOT NULL REFERENCES subscription_tokens(id) ON DELETE CASCADE,
    node_id         UUID NOT NULL,

    -- 节点侧 portal 的账号标识。采集时用它去 users/list 里找对应记录。
    -- 存一份是为了：即使 node_accounts 后来被改，也能看出"上次采的是谁"。
    user_id         TEXT NOT NULL DEFAULT '',

    -- 用量。**两个都给**：
    --   used_bytes      —— portal 的记账值（只在客户端重新认证时前进，
    --                      可能是陈旧的，但它是"官方口径"）
    --   limit_bytes     —— 配额上限，0 表示不限
    -- 分开存是因为它们的更新节奏完全不同，混在一起会让人误判。
    used_bytes      BIGINT NOT NULL DEFAULT 0,
    limit_bytes     BIGINT NOT NULL DEFAULT 0,

    -- 这次采集是否成功。
    -- ⚠️ 这一列是必须的：节点不可达时我们**采不到**用量，
    -- 但绝不能把"采不到"写成 used_bytes=0 —— 那会让用户在界面上
    -- 看到"这条订阅用量 0"，进而以为客户没在用（而实际是没采到）。
    -- 所以采失败时保留上一次的数值、把 ok 置 false，让前端能标注"数据陈旧"。
    ok              BOOLEAN NOT NULL DEFAULT false,
    error           TEXT NOT NULL DEFAULT '',

    sampled_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (subscription_id, node_id)
);

CREATE INDEX IF NOT EXISTS idx_subscription_usage_node
    ON subscription_usage (node_id);

COMMENT ON TABLE subscription_usage IS
    '订阅在各节点上的用量（采集器定期落库）。用量只存在于节点侧，本表是本地投影，最长延迟一轮采集。';
COMMENT ON COLUMN subscription_usage.ok IS
    '本次采集是否成功。false 表示这个数值是上一次的（节点不可达），前端应标注"数据陈旧"而不是当成 0。';
COMMENT ON COLUMN subscription_usage.used_bytes IS
    'portal 记账的累计用量（字节）。注意它只在客户端重新认证时前进，可能长时间不动。';
