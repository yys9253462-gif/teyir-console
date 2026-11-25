-- 038: 记录评分与风险标记的来源（2026-10-15）。
-- 'ipqs' = IPQualityScore 单 IP 欺诈分；'ipapi' = ipapi.is 的 ASN 滥用比例。
-- 两者含义不同，界面据此显示「欺诈分」或「ASN 滥用率」。
ALTER TABLE node_ip_purity ADD COLUMN IF NOT EXISTS score_source TEXT NOT NULL DEFAULT '';
UPDATE node_ip_purity SET score_source = 'ipapi' WHERE source = 'api' AND score_source = '';
