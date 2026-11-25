-- 014: 3X-UI 面板对接 —— 控制台直连外部 3X-UI 面板的 API 实现统一管理。
--
-- 3X-UI（github.com/MHSanaei/3x-ui）的 API 契约（官方 README 与 Postman 集合核对）：
--
--   POST /login                          form 表单 {username,password}
--                                        成功返回 {"success":true,...} + Set-Cookie
--                                        cookie 名：新版 "3x-ui"，旧版 "session"
--   GET  /panel/api/inbounds/list        所有入站（含客户端配置与流量统计）
--   GET  /panel/api/inbounds/onlines     当前在线客户端 email 列表
--   POST /panel/api/inbounds/addClient   添加客户端
--   POST /panel/api/inbounds/:id/delClient/:clientId   删除客户端
--   POST /panel/api/inbounds/:id/resetClientTraffic/:email   重置客户端流量
--
-- 后续所有请求都靠登录后的 session cookie 鉴权（没有独立 API token），
-- 所以 username/password 就是 3X-UI 面板的管理员凭据，与 hysteria 节点的
-- api_key 同级：AES-256-GCM 信封加密后存 BYTEA，任何接口都不回显，
-- 只回 has_basic（沿用既有字段语义：是否有可用凭据）。

CREATE TABLE IF NOT EXISTS xui_panels (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    base_url        TEXT NOT NULL,
    username        TEXT NOT NULL,
    password        BYTEA NOT NULL,
    last_status     TEXT NOT NULL DEFAULT 'unknown',
    last_checked_at TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT '',
    inbounds_count  INTEGER NOT NULL DEFAULT 0,
    clients_count   INTEGER NOT NULL DEFAULT 0,
    online_count    INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 同一个地址只登记一次；软删语义本项目暂不用于这张表（3X-UI 面板数量少，
-- 直接删行即可，历史数据落在审计日志里）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_xui_panels_base_url
    ON xui_panels (base_url);

COMMENT ON TABLE xui_panels IS
    '外部 3X-UI 面板登记。凭据等价于对方面板管理员，绝不回显。';
COMMENT ON COLUMN xui_panels.password IS
    '3X-UI 管理员密码，AES-256-GCM 信封加密。与 username 组成登录会话凭据。';
COMMENT ON COLUMN xui_panels.last_status IS
    '最近一次探测/调用的结果状态：ok / error / unknown。';
