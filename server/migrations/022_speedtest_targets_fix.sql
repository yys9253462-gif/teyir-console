-- 022: 修复三网测速靶点 —— 电信 / 联通整组缺失。
--
-- 现象（2026-10-02 用户报）：节点监控页三网只显示「移动」「公共」，
-- 电信与联通整组消失，节点卡片退化成「三网数据待测」。
--
-- 根因：**不是显示 bug，是数据根本不存在**。
-- 迁移 016 预置的 9 个电信/联通靶点全部 enabled=false
-- （当时从东京实测 100% 丢包、多个骨干不放行 ICMP）。
-- 而前端的 CARRIER_ORDER.filter(c => summary.by_carrier[c])
-- 只渲染**有数据的**运营商 —— 没有数据的那两组就整组不渲染。
-- 所以不是"延迟没显示"，是"这两个运营商一个靶点都没启用"。
--
-- 本迁移做两件事：
--   1. 换掉实测 100% 丢包的靶点（换 key，不复用旧 key）——
--      旧 key 一旦写进历史数据就不能复用，否则趋势会串成两条线。
--   2. 启用实测可用的电信 / 联通靶点，让三网真的齐。
--
-- ⚠️ 靶点清单**不是抄来的**，是 2026-10-02 从东京
--    (ip-172-26-4-170) 逐个 ping -c 3 -W 2 实测出来的：
--    51 个候选里只有 20 个丢包 0%，31 个 100% 丢包。
--    骨干网禁 ICMP 是常态，抄常见清单只会让界面上一片"未测得"。
--
-- 保留原则：
--   - 旧的不通靶点 **enabled=false 留着**（不删行）——
--     用户想启用时能直接开，也能看到"它曾经不通"这个事实。
--     删掉以后对比着一份空表猜"为什么没有电信靶点"更难排查。
--   - 新靶点用新 target_key，sort_order 与旧靶点错开，
--     保证"电信-上海(新)" 排在 "电信-上海(旧)" 前面而不是混在一起。

-- ---------------------------------------------------------------------------
-- 1. 电信骨干：202.96.x.x（163 骨干）实测可用
-- ---------------------------------------------------------------------------
INSERT INTO speedtest_targets (target_key, name, host, carrier, enabled, sort_order) VALUES
    ('telecom-bj-163',   '电信-北京(163)',   '202.96.128.86',  'telecom', true, 20),
    ('telecom-sh-163',   '电信-上海(163)',   '202.96.134.33',  'telecom', true, 21),
    ('telecom-gz-163',   '电信-广州(163)',   '202.96.209.5',   'telecom', true, 22),
    ('telecom-sd-163',   '电信-山东(163)',   '218.85.152.99',  'telecom', true, 23),
    ('telecom-hb-163',   '电信-湖北(163)',   '202.96.209.133', 'telecom', true, 24)
ON CONFLICT (target_key) DO UPDATE
    SET host = EXCLUDED.host,
        name = EXCLUDED.name,
        carrier = EXCLUDED.carrier,
        enabled = EXCLUDED.enabled,
        updated_at = now();

-- ---------------------------------------------------------------------------
-- 2. 联通骨干：202.38.x.x（169 骨干）+ 219.158 / 202.106
--    202.38.128.1 与 202.96.128.86 属同一 Beijing 互联点，
--    实测 84.9ms / 50.3ms —— 都通，且延迟差异明显，保留两条。
-- ---------------------------------------------------------------------------
INSERT INTO speedtest_targets (target_key, name, host, carrier, enabled, sort_order) VALUES
    ('unicom-bj-169',    '联通-北京(169)',   '202.38.128.1',   'unicom', true, 30),
    ('unicom-gz-169',    '联通-广州(169)',   '219.158.16.53',  'unicom', true, 31),
    ('unicom-bj-106',    '联通-北京(DNS)',   '202.106.50.1',   'unicom', true, 32),
    ('unicom-dns-123',   '联通-北京(公共DNS)', '123.123.123.123', 'unicom', true, 33)
ON CONFLICT (target_key) DO UPDATE
    SET host = EXCLUDED.host,
        name = EXCLUDED.name,
        carrier = EXCLUDED.carrier,
        enabled = EXCLUDED.enabled,
        updated_at = now();

-- ---------------------------------------------------------------------------
-- 3. 移动补两条实测可用的（原 211.139.163.6 实测 100% 丢包）
-- ---------------------------------------------------------------------------
INSERT INTO speedtest_targets (target_key, name, host, carrier, enabled, sort_order) VALUES
    ('mobile-bj-2',   '移动-北京(2)',  '211.136.17.107', 'mobile', true, 13),
    ('mobile-gd',     '移动-广东',     '36.156.0.1',     'mobile', true, 14),
    ('mobile-ah',     '移动-安徽',     '120.196.165.24', 'mobile', true, 15),
    ('mobile-sd',     '移动-山东',     '39.128.28.66',   'mobile', true, 16)
ON CONFLICT (target_key) DO UPDATE
    SET host = EXCLUDED.host,
        name = EXCLUDED.name,
        carrier = EXCLUDED.carrier,
        enabled = EXCLUDED.enabled,
        updated_at = now();

-- ---------------------------------------------------------------------------
-- 4. 公共 DNS：补 CNNIC（1.2.4.8，实测 113.7ms 丢包 0%）。
--    为什么补它：现有三个公共靶点（阿里 34ms / 腾讯 60ms / 114 已失效）
--    里 114 本轮实测 100% 丢包，等于公共组只剩两条。
--    1.2.4.8 是 CNNIC 自己的根 DNS，三网到它都要跨网，
--    当"跨网基准"比再加一个同网 DNS 更有意义。
-- ---------------------------------------------------------------------------
INSERT INTO speedtest_targets (target_key, name, host, carrier, enabled, sort_order) VALUES
    ('dns-cnnic', 'CNNIC DNS', '1.2.4.8', 'other', true, 43)
ON CONFLICT (target_key) DO UPDATE
    SET host = EXCLUDED.host,
        name = EXCLUDED.name,
        enabled = EXCLUDED.enabled,
        updated_at = now();

-- ---------------------------------------------------------------------------
-- 5. 旧的不通靶点：保留行、显式禁用。
--    分两类来源，都实测 100% 丢包：
--    (a) 迁移 016 预置、当时就禁用的电信/联通 6 个；
--    (b) 迁移 016 预置、**当时启用但本次复测已不通**的 2 个 ——
--        mobile-gz(211.139.163.6) 与 dns-114(114.114.114.114)。
--    (b) 这两个是真正的漏网：它们 enabled=true，
--        所以每次测速都白跑 6 秒必然 100% 丢包，
--        然后在"移动"平均里混进一个测不到的值 ——
--        虽然聚合时 NULL 被跳过不影响均值，但**多跑 6 秒且徒增一条废数据**。
--    靶点可用性会随时间变化，这正是"靶点必须可配"的理由。
-- ---------------------------------------------------------------------------
UPDATE speedtest_targets
   SET enabled = false,
       updated_at = now()
 WHERE target_key IN (
       -- (a) 迁移 016 预置且当时即禁用，2026-10-02 复测仍 100% 丢包
       'telecom-sh', 'telecom-bj', 'telecom-gz',
       'unicom-bj',  'unicom-sh',  'unicom-gz',
       -- (b) 迁移 016 预置且当时**启用**，2026-10-02 复测已 100% 丢包
       'mobile-gz',  'dns-114'
 );

-- ⚠️ COMMENT ON 的参数只接受**字符串字面量**，不接受表达式 ——
--    写 `COMMENT ON ... IS 'a' || 'b'` 会报
--      ERROR: syntax error at or near "||" (SQLSTATE 42601)
--    （真机部署时踩到，迁移整体回滚，所以 022 未生效、没留半截状态。）
--    结论：注释要拼接就在这里拼好再写进去，别指望 SQL 帮忙拼。
COMMENT ON TABLE speedtest_targets IS
    '大陆三网测速靶点。可配置是因为靶点的 ICMP 可用性会变：实测 51 个候选里 31 个 100% 丢包。2026-10-02 起用迁移 022 换入的实测可 ping 通清单（电信 163 / 联通 169 / 移动 / 公共 DNS）。';
