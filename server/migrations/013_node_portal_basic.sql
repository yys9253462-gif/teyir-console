-- 013: 让节点可以只凭 portal 凭据被完整管理，不再强依赖 SSH。
--
-- 背景（2026-10-01 源码审计 + 生产实测确认）：
--
-- portal.py 的鉴权是**双轨**的，两条轨道的可达范围差很多：
--
--   verify_api_key()   Authorization: Bearer <api_key>
--     → 只解锁 /api/v1/ 下的 9 个端点（node/meta、users/{list,create,renew,delete}、
--       proxy-services/{list,add,delete}、以及 404 兜底）
--
--   is_authenticated() Authorization: Basic base64(user:password)
--     → 解锁 /{token}/ 下的 21 个端点，也就是门户网页端的全部功能：
--       check-version / bbr-status / reality-status / warp-status / traffic-speed /
--       awg-state / proxy-services / user-config / do-upgrade / reboot-server /
--       manage-user / manage-proxy / manage-reality / manage-warp / manage-amneziawg /
--       set-bbr / install-gost / install-xray / install-warp / install-amneziawg /
--       awg-conf / awg-qr.svg
--
--   而 auth_hash = sha256(base64(username:password))，也就是说 Basic 的那对凭据
--   **等价于 portal 管理员密码**。
--
-- 面板此前只持久化了 api_key（迁移 006），把同一个 portal-access.json 里的
-- username/password 丢掉了，于是「重启 / 升级 / BBR / Reality / WARP / 组件安装」
-- 这些能力在面板上根本不可达 —— 之前被误判成 portal 不提供，实际上是没存凭据。
--
-- 因此本迁移把 Basic 凭据也纳进来，与 api_key 同等对待：
--   * 同样 AES-256-GCM 信封加密后存 BYTEA
--   * 任何接口都不回显，只回 has_basic 布尔
--   * 审计日志不记录
--
-- portal_path_token：门户的 {token} 段。它并非独立秘密（就嵌在 portal-access.json
-- 的 url 字段尾部），但没有它就无法拼出 /{token}/... 的 URL。本地存一份可省去
-- 每次调用都去解析 URL，也便于 UI 展示「门户地址」。
--
-- portal_caps：绑定成功时探测到的能力快照。取值是一个 JSON 对象，例如
--   {"basic": true, "version": "2.12.3", "portal_sha": "c585122", "update": true}
-- 只用于 UI 决定按钮是否可点，不参与鉴权判断（真正的判定永远在请求时发生）。

ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS portal_basic_user TEXT;
ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS portal_basic_password BYTEA;
ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS portal_path_token TEXT;
ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS portal_caps JSONB;
ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS portal_checked_at TIMESTAMPTZ;

-- 解绑（unbindNodeAPI）要一并清空这几列，故把「已绑定」的判定条件从
-- `api_url IS NOT NULL` 放宽语义即可 —— 无需改索引，但补一个部分索引能加速
-- 订阅开户那句 `WHERE api_url <> '' AND api_key IS NOT NULL` 的过滤。
CREATE INDEX IF NOT EXISTS idx_hysteria_nodes_provisionable
    ON hysteria_nodes (created_at)
    WHERE deleted_at IS NULL AND api_url IS NOT NULL AND api_key IS NOT NULL;

COMMENT ON COLUMN hysteria_nodes.portal_basic_user IS
    'portal-access.json 的 username。与 api_key 合起来才够得着 portal 的网页会话端点。';
COMMENT ON COLUMN hysteria_nodes.portal_basic_password IS
    'portal-access.json 的 password，AES-256-GCM 信封加密。等价于 portal 管理员密码，绝不回显。';
COMMENT ON COLUMN hysteria_nodes.portal_path_token IS
    '门户 URL 里的 {token} 段，用于拼 /{token}/ 前缀的端点。';
COMMENT ON COLUMN hysteria_nodes.portal_caps IS
    '绑定/刷新时探测到的能力快照 JSON，仅供 UI 决定按钮可用性，不参与鉴权。';
