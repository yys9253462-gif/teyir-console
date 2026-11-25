-- 010: 订阅 ↔ 节点侧独立账号的绑定。
--
-- 背景（P0）：在此之前，订阅下发给买家的口令全部取自节点侧
-- `meta.auth_password`，也就是 portal.json 里那个**全局共享口令**。
-- 后果有二：
--   1. 到期收不干净。回收时只删了 subscription_tokens 那一行，节点侧的
--      账号还在，口令对应的账号（admin_master）有效期到 2036 年 ——
--      买家只要在到期前导入过配置或抄下口令，到期后照样能连。
--   2. ip_limit 失真。所有买家共用同一个节点侧账号，设备额度是记在共享
--      账号上的，多设备/多人可以互相占满额度。
--
-- 修法：下单时在每个在册节点上为这个买家单独开户
-- （POST /api/v1/users/create），把「节点 → 该买家的节点侧账号」记在这里；
-- 下发配置时用该买家的独立口令；回收时按这份映射调 users/delete 销号。
--
-- 为什么用 JSONB 而不是新开一张关联表：
--   映射是「一条订阅的整体属性」，永远整体读写，从不跨订阅查询某一个
--   节点账号。单列内联读一次就够，省一次 join，也和 active_ips 的既有
--   风格一致（同一张表里已经是 JSONB 存列表）。
--
-- 结构：
--   {
--     "<hysteria_nodes.id>": {
--       "user_id":  "iso_xxxxxxxxxxxx",   -- 节点侧账号 ID
--       "password": "…32位hex…",          -- 该买家的独立口令
--       "created_at": 1790739003           -- unix 秒，便于排查
--     }, …
--   }
--
-- 兼容：既有订阅这一列为 '{}'，读取时视为「未开户」，仍走旧的共享口令
-- 路径。这保证了迁移期间新旧订单能共存（用户明确要求：只保新订单，
-- 旧的不动）。

ALTER TABLE subscription_tokens
    ADD COLUMN IF NOT EXISTS node_accounts JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN subscription_tokens.node_accounts IS
    '节点侧独立账号映射 {node_id: {user_id, password, created_at}}。'
    '为空 {} 表示这是迁移前的旧订阅，仍使用节点全局共享口令（到期收不干净）。';
