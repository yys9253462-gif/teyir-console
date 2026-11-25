-- 023: 孤儿账号登记表 —— 让「删除节点时的回收失败」可补救。
--
-- 背景（2026-10-02 实测发现）
-- --------------------------
-- 删除节点时若某个买家账号没能从节点侧注销（节点不可达、凭据被拒、
-- 超时等），现在的行为是：
--   1. 保留 subscription_tokens.node_accounts 里的映射（对的，便于重试）
--   2. 但 hysteria_nodes 的 api_url / api_key **已被抹掉**（安全设计，也是对的）
--   3. 返回 207 并提示「需手动到节点清理」
--
-- 三条各自都合理，合起来却造成一个**死局**：
--   映射留着，但没有任何地方记着"该用哪把钥匙、连哪个地址去删"。
--   于是那条映射永远重试不了，买家账号永久留在节点上继续可用 ——
--   而控制台这边已经完全看不到这件事了（节点都删了）。
--
-- 这是「钱退了，货还在」的同一个病，只是发生在"下架节点"而非"退款"上。
-- 实测证据：清理订阅残留时发现已软删的「悉尼-AWS」上还挂着 sub_00000001，
-- 而该节点的 api_url/api_key 早已被抹成 NULL，无法自动补删。
--
-- 本表做什么
-- ----------
-- 在"节点删除 + 回收有失败"这一刻，把**补删所需的一切**落盘：
--   - 要删的 uid（每个失败的账号一行）
--   - 当时的 portal 地址与加密后的 api_key（凭据快照）
--   - 失败原因、重试次数、最后尝试时间
-- 这样运维（或后续的自动重试）在节点恢复可达后能真的把它删掉。
--
-- 安全说明：api_key 用与 hysteria_nodes.api_key 相同的加密（secrets.Box）
-- 存放，不落明文。表本身只增不改业务数据，读它需要管理员权限。

CREATE TABLE IF NOT EXISTS orphan_node_accounts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- 被删掉的节点（**不做外键**：节点的行还在，但语义上它已经"没了"，
    -- 我们不希望节点被硬删时把这批待办一起级联带走 —— 那等于再次丢掉线索）。
    node_id         UUID NOT NULL,
    -- 冗余节点名与主机地址：节点记录以后可能被改或被硬删，
    -- 而运维拿到这条待办时要能知道"这是哪台机器"。
    node_name       TEXT NOT NULL DEFAULT '',
    node_api_url    TEXT NOT NULL DEFAULT '',

    -- 要补删的账号。
    user_id         TEXT NOT NULL,
    -- 它属于哪条订阅（便于人工核对是谁的账号）。
    subscription_id UUID,

    -- 凭据快照：加了密的 portal api_key。
    -- 为空表示当时就没取到快照（那种情况基本无解，但也要留痕）。
    api_key_cipher  BYTEA,

    -- 失败与重试状态。
    reason          TEXT NOT NULL DEFAULT '',
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    -- resolved_at 非空表示已补删成功（保留行以便审计"曾经出过这个问题"）。
    resolved_at     TIMESTAMPTZ,
    resolved_note   TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 查"还有哪些待补删"是这张表唯一的高频查询。
CREATE INDEX IF NOT EXISTS idx_orphan_accounts_pending
    ON orphan_node_accounts (created_at DESC)
    WHERE resolved_at IS NULL;

-- 同一次删除里同一个 (节点, 账号) 只登记一条：重试删除节点时不该堆重复行。
CREATE UNIQUE INDEX IF NOT EXISTS idx_orphan_accounts_unique_pending
    ON orphan_node_accounts (node_id, user_id)
    WHERE resolved_at IS NULL;

COMMENT ON TABLE orphan_node_accounts IS
    '删除节点时未能从节点侧注销的买家账号。保留凭据快照，让节点恢复可达后能真正补删 —— 否则映射留着也没用（节点 api_url/api_key 已随删除抹掉）。';
COMMENT ON COLUMN orphan_node_accounts.api_key_cipher IS
    '当时的节点 portal api_key（secrets.Box 加密）。为空表示删除时就没取到快照，基本无法补删。';
COMMENT ON COLUMN orphan_node_accounts.resolved_at IS
    '非空表示已补删成功；保留行以便审计。';
