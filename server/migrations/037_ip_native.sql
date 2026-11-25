-- 037: 原生 IP 判定（2026-10-15）。
--
-- reg_country：IP 段的注册国家（RDAP，ISO 两位大写），空串 = 未查到。
-- geo_country：IP 的实际所在国家（面板 geo 解析），空串 = 未查到。
-- is_native：两者都有时才判定（相同 = 原生，不同 = 广播）；缺任一为 NULL（未判定）。
-- 与其他判定字段一样：检测失败不覆盖上次成功的值。
ALTER TABLE node_ip_purity
    ADD COLUMN IF NOT EXISTS reg_country TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reg_registry TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS geo_country TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS is_native BOOLEAN;
