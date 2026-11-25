-- 040: 被墙检测、订阅收入（2026-10-15）。

-- 1) 被墙检测结果：国内探测点（一台在中国大陆的主机，经 SSH 执行）对每个节点
--    做 TCP 建连 + ICMP 探测。面板部署在海外能连上、国内探测点连不上，
--    就判定为疑似被墙。只存最新一次结果（1:1），历史看审计/告警时间线。
CREATE TABLE IF NOT EXISTS node_block_checks (
    node_id UUID PRIMARY KEY REFERENCES hysteria_nodes(id) ON DELETE CASCADE,
    probe_server_id UUID REFERENCES servers(id) ON DELETE SET NULL,
    target_ip TEXT NOT NULL DEFAULT '',
    tcp_port INTEGER,
    tcp_ok BOOLEAN,
    ping_ok BOOLEAN,
    -- blocked：tcp 与 ping 都不通 → true；任一通 → false；探测本身失败 → NULL。
    blocked BOOLEAN,
    detail TEXT NOT NULL DEFAULT '',
    checked_at TIMESTAMPTZ
);

-- 2) 订阅收入：商城下单/续费时可带 amount + currency（按订单时长折算成月收入），
--    也可在订阅中心手工填写。利润报表按各节点实测流量占比分摊。
ALTER TABLE subscription_tokens
    ADD COLUMN IF NOT EXISTS revenue_monthly NUMERIC(10, 2) CHECK (revenue_monthly IS NULL OR revenue_monthly >= 0),
    ADD COLUMN IF NOT EXISTS revenue_currency TEXT NOT NULL DEFAULT 'CNY'
        CHECK (revenue_currency IN ('USD', 'CNY', 'EUR', 'HKD', 'JPY'));
