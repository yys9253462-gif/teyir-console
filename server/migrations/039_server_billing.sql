-- 039: 主机到期与成本（2026-10-15）。
--
-- 卖节点要算账：每台 VPS 什么时候到期、每月多少钱。到期前没续费，
-- 节点直接消失，买家先发现 —— 所以到期前 7 天要在告警中心提醒。
--
-- 单独一张 1:1 表，不往 servers 加列：servers 的列表查询已经很重，
-- 这些字段只在主机页与告警巡检里用。
CREATE TABLE IF NOT EXISTS server_billing (
    server_id UUID PRIMARY KEY REFERENCES servers(id) ON DELETE CASCADE,
    -- 到期日（按北京时间理解的日期）。NULL = 未填写，不提醒。
    expires_on DATE,
    -- 月费；按年付费的填折算后的月均值。NULL = 未填写。
    monthly_cost NUMERIC(10, 2) CHECK (monthly_cost IS NULL OR monthly_cost >= 0),
    currency TEXT NOT NULL DEFAULT 'USD' CHECK (currency IN ('USD', 'CNY', 'EUR', 'HKD', 'JPY')),
    -- 是否自动续费：自动续费的主机到期不告警。
    auto_renew BOOLEAN NOT NULL DEFAULT false,
    note TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
