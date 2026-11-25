-- 012: 让 alert_events 真正成为告警的时间线。
--
-- 背景（本轮实测发现）：001 建了 alert_events 表，event_type 还带
-- 'triggered' / 'repeated' / 'acknowledged' / 'resolved' 四态，但翻开
-- 全项目没有一处 INSERT —— 它是一张死表。
--
-- 为什么这件事对小白用户是缺陷而不只是「少了个日志」：
--   1. 面板点开一条告警，只能看到「第一次出现/最近一次出现」两个时间点。
--      用户无法回答最关心的那个问题：**这条告警中间好过吗？是不是一直没恢复？**
--      对运维面板来说，「反复抖动」和「持续故障」是完全不同的两件事，处理方式
--      也不同（前者查网络，后者查进程）。
--   2. 一条告警被 ack 之后，`last_seen_at` 会被 reconcile 继续刷新，于是
--      「刚才处理过」和「一小时前处理过」在界面上长得一样 —— ack 的意义丢失。
--   3. 告警自己 resolve 掉之后，除了 status 变化，没有任何痕迹说明它是什么
--      时候恢复的、恢复了多久。而 resolved_at 会被下一次复现覆盖掉。
--
-- 修法：ReconcileAlerts 在「新开一条告警」和「恢复一条告警」时各写一条
-- alert_events；acknowledgeAlert 也写一条。历史记录只增不改，是纯粹的时间线。
--
-- 只动约束、不动已有列，因此对既有部署是安全的：表本来就是空的。

-- event_type 增加 'reopened'：一条已经 resolve 的告警，如果同一个条件再次
-- 成立，reconcile 会新开一条记录（key 里含 status<>'resolved'），这在时间线上
-- 表现为「解了又犯」。单独标记出来，用户一眼能看出是反复性问题。
ALTER TABLE alert_events DROP CONSTRAINT IF EXISTS alert_events_event_type_check;
ALTER TABLE alert_events
    ADD CONSTRAINT alert_events_event_type_check
    CHECK (event_type IN ('triggered', 'repeated', 'reopened', 'acknowledged', 'resolved'));

-- 时间线永远是「按告警查、按时间倒序」，没有索引就是全表扫。
CREATE INDEX IF NOT EXISTS alert_events_alert_id_created_at_idx
    ON alert_events (alert_id, created_at DESC);

-- 一条告警被删（目前不会，ON DELETE RESTRICT）时的行为保持不变；
-- 这里只补一个「按时间看最近发生了什么」的全局索引，告警中心首页会用到。
CREATE INDEX IF NOT EXISTS alert_events_created_at_idx
    ON alert_events (created_at DESC);
