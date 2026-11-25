-- 034: 节点 IP 纯净度检测（2026-10-15）。
--
-- 背景
-- ----
-- 卖的节点是「机场」，买家最在意的不是带宽而是**这条 IP 干不干净**：
-- 一查就是机房托管、被 Netflix/OpenAI/银行风控标记为代理、或是被滥用过
-- 的高风险段，那这个节点对目标用途就是废的。此前控制台只能人工拿 IP 去
-- 第三方网站逐个查，查完还不记得。
--
-- 这个功能把「这台节点的 IP 属于什么性质」沉淀成节点的一个属性：
--   - 连接类型：家庭宽带 / 机房托管 / 移动网络 / 未知
--   - 是否双 ISP（同一 IP 段由两家 ISP 共用 —— 常见于一些"看起来像家宽"
--     的机房，买家会问）
--   - 纯净度评分与风险标记（代理 / VPN / Tor / 滥用记录）
--   - 数据来源与检测时间（判断这条结论还新不新）
--
-- 为什么单独建表而不是给 hysteria_nodes 加一堆列
-- ---------------------------------------------
-- 1. 一次检测的产物是**一组会整体刷新**的字段 + 一份原始 JSON。挂在
--    hysteria_nodes 上会让那张已经很宽的表再胖一圈，而且每次刷新都要
--    UPDATE 主表行（与状态采集抢行锁）。
-- 2. 原始 JSON（raw）不能塞进主表：它是排障依据，可能有几 KB，
--    与"节点列表"这个高频查询的读放大完全不成比例。
-- 3. 1:1 关系（node_id 唯一），删节点时级联删除 —— 不留孤儿行。
--
-- 为什么 raw 存 JSONB 且允许为空
-- -----------------------------
-- 离线判定的结果可能没有完整原始响应（本地算出来的），此时 raw 为 '{}'。
-- 不设 NOT NULL 默认空对象是完全合法的形态，不应当视为"没检测过"——
-- 判据是 checked_at 是否为空，不是 raw 是否为空。
--
-- 为什么要有 source 列
-- -------------------
-- 与 geo 的 Source 同理：出问题时第一个要看的就是「这个结论从哪来」。
-- offline=本地判定 / api=第三方 API / manual=人工录入。
-- 用 CHECK 锁死取值，脏值会让"是否可信/是否要重测"的判断静默走错分支。

CREATE TABLE IF NOT EXISTS node_ip_purity (
    node_id UUID PRIMARY KEY REFERENCES hysteria_nodes(id) ON DELETE CASCADE,
    -- 被检测的 IP（节点自报 public_ip，或 servers.host）。存下来是为了
    -- 对比"现在节点 IP 还是不是当初检测的那个"——换了 IP 判定就作废，
    -- 界面据此提示"IP 已变更，建议重测"。
    ip TEXT NOT NULL DEFAULT '',
    -- connection_type：connection 的语义是"这条 IP 挂在什么网络里"。
    --   residential 家庭宽带 / datacenter 机房 / mobile 移动 / business 企业专线 / unknown
    connection_type TEXT NOT NULL DEFAULT 'unknown'
        CHECK (connection_type IN ('residential', 'datacenter', 'mobile', 'business', 'unknown')),
    -- is_dual_isp：是否双 ISP（同一段被两家 ISP 共用）。NULL 表示"未判定"，
    -- 与 false（明确判定不是）区分开——这点很重要，第三方 API 有一半不给
    -- 这个字段，把"没数据"当"不是双 ISP"是在骗用户。
    is_dual_isp BOOLEAN,
    -- fraud_score：0-100 的纯净度风险分（越大越脏）。NULL=未判定。
    -- 项目铁律「没有这一栏，状态码只是数字」的同一思路：宁可显示"未判定"，
    -- 也不给一个假的中性分。
    fraud_score INTEGER CHECK (fraud_score IS NULL OR (fraud_score BETWEEN 0 AND 100)),
    -- 三个布尔标记：明确 true/false 才写；未判定一律 NULL。
    is_proxy BOOLEAN,
    is_vpn BOOLEAN,
    is_abuser BOOLEAN,
    -- asn / asn_org：判定 connection_type 的主要依据（ASN 类型），
    -- 也方便运维一眼看出"这条 IP 挂在谁家"。
    asn TEXT NOT NULL DEFAULT '',
    asn_org TEXT NOT NULL DEFAULT '',
    -- raw：第三方返回的原始结构或本地判定明细，排障用。
    raw JSONB NOT NULL DEFAULT '{}'::jsonb,
    source TEXT NOT NULL DEFAULT ''
        CHECK (source IN ('', 'offline', 'api', 'manual')),
    -- error：最近一次检测失败的原因。非空表示"这次检测没成功"，
    -- 但 checked_at 仍是上一次成功的时间——两个字段各自表达一件事，
    -- 不互相覆盖。这样界面能同时说"上次成功是 3 天前 + 上次尝试失败因为超时"。
    error TEXT NOT NULL DEFAULT '',
    checked_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 列表页会按"脏不脏"排序/筛，给评分与检测时间各留一个索引。
CREATE INDEX IF NOT EXISTS idx_node_ip_purity_fraud
    ON node_ip_purity (fraud_score DESC NULLS LAST);
CREATE INDEX IF NOT EXISTS idx_node_ip_purity_checked
    ON node_ip_purity (checked_at DESC NULLS LAST);

COMMENT ON TABLE node_ip_purity IS
    '节点 IP 纯净度检测结果（1:1 挂 hysteria_nodes）。离线判定 + 第三方 API 兜底，结果缓存于此。';
COMMENT ON COLUMN node_ip_purity.connection_type IS
    '连接类型：residential 家宽 / datacenter 机房 / mobile 移动 / business 专线 / unknown 未知。';
COMMENT ON COLUMN node_ip_purity.is_dual_isp IS
    '是否双 ISP。NULL=未判定（第三方可能不提供该字段），false=明确判定不是。';
COMMENT ON COLUMN node_ip_purity.fraud_score IS
    '纯净度风险分 0-100，越大越脏。NULL=未判定。';
COMMENT ON COLUMN node_ip_purity.source IS
    '结论来源：offline 本地判定 / api 第三方 API / manual 人工录入。';
COMMENT ON COLUMN node_ip_purity.error IS
    '最近一次检测失败原因。与 checked_at（上次成功时间）独立，互不覆盖。';
