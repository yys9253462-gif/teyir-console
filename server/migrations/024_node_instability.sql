-- 024: 接通「节点不稳定」—— 让 warning 状态真的会产生。
--
-- 背景（2026-10-02 核查告警中心时发现）
-- ------------------------------------
-- 前端有 `warning: '不稳定'` 的状态文案与「不稳定」指标卡，
-- manage.go 有 `node_unstable` 告警规则（status='warning' → warning 级告警），
-- **但后端从来没有任何代码路径写 hysteria_nodes.status='warning'** ——
-- 节点状态只有 online / offline / unknown 三种。
--
-- 于是：界面预留了一个永远空着的状态，告警预留了一条永远不触发的规则。
-- 这类"看着有功能其实永不生效"的东西最坑 —— 用户以为"节点不稳定会告警"，
-- 实际上永远不会，而没有任何地方会告诉他这一点。
--
-- 什么算「不稳定」
-- ----------------
-- 不引入主观阈值（比如延迟多少算慢 —— 那个数在东京/洛杉矶之间能差 20 倍，
-- 定一个全局值必然误报）。用**可观测的客观信号**：
--
--   该节点**连续 N 轮探测失败**。
--
-- 节点是二元的（端口在听 / 不在听）：
--   - 一直失败 → 很快变成 offline（那是 critical 告警，已有规则）
--   - **偶尔失败、又偶尔成功** → 这才是"不稳定"，而现在它被静默吞掉
--     （失败那一轮只写 status_error 并保留 status 不动，见 collector.go）
--
-- 所以：失败累加、成功归零，累到阈值就置 warning。
-- 阈值取 3：连续三轮都失败说明不是偶发抖动；而超过阈值仍持续失败会
-- 自然滑向 offline（那由现有规则接管）。
--
-- 为什么用「连续」而不是「N 次里失败 M 次」：后者需要滑动窗口，
-- 而一张表上的滑动窗口要么加时间序列表、要么用数组列，成本高得多；
-- 「连续」用一个整数就能表达，且语义更直观（"它连着三轮没探通"）。

ALTER TABLE hysteria_nodes
    ADD COLUMN IF NOT EXISTS consecutive_probe_failures INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN hysteria_nodes.consecutive_probe_failures IS
    '连续探测失败轮数。成功探测归零；用于把「偶尔失败、偶尔成功」的节点标成 warning（不稳定）。';

-- 便于按"不稳定"筛选（前端指标卡与告警都要用）。
CREATE INDEX IF NOT EXISTS idx_hysteria_nodes_unstable
    ON hysteria_nodes (consecutive_probe_failures)
    WHERE deleted_at IS NULL AND consecutive_probe_failures > 0;
