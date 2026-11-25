-- 节点整机流量计量与限额（2026-10-14）。
--
-- 数据来源是节点 portal 的 GET /api/v1/node/traffic：默认路由网卡开机以来的
-- 累计收发字节 + boot_id。控制台按差值累加到本周期用量，boot_id 变了说明
-- 节点重启、计数器归零，此时新计数器本身就是重启后的增量。
--
-- 计费口径：上传 + 下载双向合计；每月 traffic_reset_day 号（北京时间 0 点）重置。
-- 超过 traffic_limit_bytes 时调 POST /api/v1/node/suspend 停代理服务，
-- 新周期开始或限额调高后自动恢复。
ALTER TABLE hysteria_nodes
  ADD COLUMN IF NOT EXISTS traffic_limit_bytes bigint
    CHECK (traffic_limit_bytes IS NULL OR traffic_limit_bytes > 0),
  ADD COLUMN IF NOT EXISTS traffic_reset_day smallint NOT NULL DEFAULT 1
    CHECK (traffic_reset_day BETWEEN 1 AND 31),
  ADD COLUMN IF NOT EXISTS traffic_cycle_start timestamptz,
  ADD COLUMN IF NOT EXISTS traffic_cycle_bytes bigint NOT NULL DEFAULT 0
    CHECK (traffic_cycle_bytes >= 0),
  ADD COLUMN IF NOT EXISTS traffic_counter_rx bigint,
  ADD COLUMN IF NOT EXISTS traffic_counter_tx bigint,
  ADD COLUMN IF NOT EXISTS traffic_boot_id text,
  ADD COLUMN IF NOT EXISTS traffic_sampled_at timestamptz,
  ADD COLUMN IF NOT EXISTS traffic_error text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS traffic_suspended boolean NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS traffic_suspended_at timestamptz,
  ADD COLUMN IF NOT EXISTS version_checked_at timestamptz;
