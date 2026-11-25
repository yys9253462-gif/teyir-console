-- 036: 按修正后的口径重算已存的「双 ISP」（2026-10-15）。
--
-- 双 ISP = ipapi.is 的 asn.type 与 company.type 两个独立口径都是 isp。
-- 原实现口径错误（见 provider_ipapiis.go 的注释）。raw 里存着当时的原始
-- 分类，直接据此重算，不必重新消耗第三方配额。
-- 只处理 source='api' 且两个类型都在的行；其余保持原值。
UPDATE node_ip_purity
SET is_dual_isp = (lower(raw->'asn'->>'type') = 'isp' AND lower(raw->'company'->>'type') = 'isp'),
    updated_at = now()
WHERE source = 'api'
  AND coalesce(raw->'asn'->>'type', '') <> ''
  AND coalesce(raw->'company'->>'type', '') <> '';
