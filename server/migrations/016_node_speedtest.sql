-- 016: 节点监控 —— 大陆三网测速 + 真实网络延迟。
--
-- 背景：为什么需要单独一张表
-- --------------------------
-- 现有的 servers.latency_ms 名字像"网络延迟"，实际是 **SSH 握手耗时**
-- （collector.go 里从 dialStarted 计时到 SSH 建连完成）。它对诊断很有价值
-- （能看出 SSH 慢是因为网络还是因为对方主机负载高），但把它当"节点延迟"
-- 展示给用户是错的 —— 一个 SSH 端口被封的节点会显示"离线/无延迟"，
-- 而它的代理服务可能好得很。
--
-- 所以本次新增两类数据，与 latency_ms 并存、语义各自独立：
--
--   1. servers.net_latency_ms —— 面板到该主机的**真实 TCP 建连耗时**
--      （拨号 host:ssh_port，不含 SSH 握手）。比 ICMP 可靠：很多机器禁 ping。
--
--   2. node_speedtests —— 大陆三网测速结果。**在节点本机执行**测速
--      （面板 SSH 过去跑探针脚本），因为只有节点自己才知道它回国的线路质量。
--      面板所在宿主机直接测速测的是「面板↔目标」，
--      与你家连这台节点的体验无关。
--
-- 为什么按 (node/server, 靶点) 存多行而不是一行塞多列
-- ----------------------------------------------------
-- 三网靶点是**可配置、可增减**的（电信从 3 个靶点变 5 个是常态）。
-- 若做成一行的 province_telecom_ms / province_unicom_ms / ... 列，
-- 每加一个靶点都要改表结构 + 改查询 + 改前端。用行存储则加靶点只是多插几行。
--
-- 代价是查询要 pivot（见 API 层的聚合），但这个代价是一次性的，
-- 而列方案的代价是每加一个靶点付一次。

-- ---------------------------------------------------------------------------
-- 1. servers 增加真实网络延迟
-- ---------------------------------------------------------------------------
ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS net_latency_ms INTEGER,
    ADD COLUMN IF NOT EXISTS net_latency_checked_at TIMESTAMPTZ;

COMMENT ON COLUMN servers.net_latency_ms IS
    '面板到该主机的真实 TCP 建连耗时（毫秒）。与 latency_ms 不同：后者是 SSH 握手耗时。';
COMMENT ON COLUMN servers.net_latency_checked_at IS
    'net_latency_ms 的最后一次测量时间。';

-- ---------------------------------------------------------------------------
-- 2. 三网测速结果表
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS node_speedtests (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 测速在**哪个节点**上执行。软删的节点保留历史（加 deleted_at 过滤查询）。
    node_id       UUID NOT NULL REFERENCES hysteria_nodes(id) ON DELETE CASCADE,
    -- 冗余 server_id：面板上"按主机看三网质量"是常见需求（一个主机多个节点时
    -- 各节点的线路可能不同）。冗余一份避免每次都要 join hysteria_nodes。
    server_id     UUID NOT NULL REFERENCES servers(id) ON DELETE CASCADE,

    -- 靶点标识。target_key 是稳定主键（如 "telecom-sh"），name 是展示名。
    -- 两者分开：展示名以后可能改（"电信-上海" → "上海电信"），
    -- 而 target_key 一旦用进历史数据就不能变，否则趋势图会断成两条线。
    target_key    TEXT NOT NULL,
    target_name   TEXT NOT NULL,
    target_host   TEXT NOT NULL,
    -- 运营商分类：telecom / unicom / mobile / other。
    -- 用 TEXT 而非 ENUM，与项目其他部分一致；但用 CHECK 约束防脏值 ——
    -- 前端按这个字段分组展示，脏值会让那个靶点凭空消失。
    carrier       TEXT NOT NULL,

    -- 测量结果。**全部允许 NULL**：靶点可能禁 ICMP（很多骨干不放行），
    -- 也可能只测得通 TCP 测不到带宽。用 NULL 表示"这项没测到"，
    -- 与 0 区分开 —— 0ms 延迟和"没测到"是完全不同的意思。
    icmp_avg_ms   DOUBLE PRECISION,
    icmp_loss_pct DOUBLE PRECISION,
    tcp_connect_ms DOUBLE PRECISION,
    -- 带宽（Mbps）。实测中很少能拿到：需要节点侧有可下载的测速文件。
    -- 留着字段是因为"以后接上"比"以后改表"便宜，且 NULL 语义清晰。
    download_mbps DOUBLE PRECISION,
    upload_mbps   DOUBLE PRECISION,

    -- probe_ok 表示这次探针**整体**是否成功执行（脚本跑通、有输出）。
    -- 与"某个指标为 NULL"不同：probe_ok=false 说明连脚本都没跑起来
    -- （比如节点没装 ping、或 SSH 失败），那和"靶点不通"是两类问题，
    -- 排查方向完全不同，所以分开记。
    probe_ok      BOOLEAN NOT NULL DEFAULT false,
    probe_error   TEXT NOT NULL DEFAULT '',

    measured_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 查"某节点最近一次测速"是本表最高频的查询（页面加载、趋势图都走它）。
CREATE INDEX IF NOT EXISTS idx_node_speedtests_node_time
    ON node_speedtests (node_id, measured_at DESC);

-- 按主机聚合（总览/节点监控页按 server 分组时用）。
CREATE INDEX IF NOT EXISTS idx_node_speedtests_server_time
    ON node_speedtests (server_id, measured_at DESC);

-- 同一个 (节点, 靶点, 测量秒) 只应有一条：探针重复上报时靠它幂等。
--
-- ⚠️ 这里连踩两个坑，都记下来，因为这个错误在真机上才暴露：
--
--   第 1 版 `date_trunc('second', measured_at)` →
--     ERROR: functions in index expression must be marked IMMUTABLE
--   第 2 版 `floor(extract(epoch FROM measured_at))::bigint` → 同样报错
--
-- 根因：`timestamptz` 上的这两个函数是 **STABLE 而非 IMMUTABLE** ——
-- 它们的结果依赖会话时区（同一个绝对时刻，在不同 TimeZone 设置下
-- date_trunc 出不同的值），而索引表达式要求结果对同一行永远一致。
--
-- 查 pg_proc 证实：`date_trunc` 与 `extract` 各有 IMMUTABLE 与 STABLE
-- **两个重载**，取决于参数类型 —— 传 timestamp 走 immutable，传
-- timestamptz 走 stable。所以修法是先把 timestamptz 转成 timestamp：
--
--   `measured_at AT TIME ZONE 'UTC'` 得到 timestamp（绝对时刻，无时区歧义）
--   → 再 date_trunc 就走 immutable 重载了。
--
-- 实测确认（2026-10-02，真 Postgres 16）：
--   ❌ date_trunc('second', measured_at)
--   ❌ floor(extract(epoch FROM measured_at))::bigint
--   ✅ date_trunc('second', measured_at AT TIME ZONE 'UTC')
--
-- 注意 AT TIME ZONE 'UTC' 在这里**不是**"把时间转成 UTC 时区"，
-- 而是"取出这个绝对时刻在 UTC 下的墙上时间"，语义上等价于截到秒，
-- 且与服务器 TimeZone 设置无关。
--
-- 还要注意：这个索引失败时**整条迁移会从这里中断**，后面的
-- speedtest_targets 建表与预置数据全都不会执行 —— 表现是"表建了一半"。
CREATE UNIQUE INDEX IF NOT EXISTS idx_node_speedtests_unique_round
    ON node_speedtests (node_id, target_key, date_trunc('second', measured_at AT TIME ZONE 'UTC'));

COMMENT ON TABLE node_speedtests IS
    '大陆三网测速结果。探针在**节点本机**执行（面板 SSH 过去跑），因为只有节点自己知道它回国的线路质量。';
COMMENT ON COLUMN node_speedtests.carrier IS
    '运营商分类：telecom / unicom / mobile / other。前端按此分组。';
COMMENT ON COLUMN node_speedtests.probe_ok IS
    '探针整体是否成功执行。false 表示脚本没跑起来（如节点缺 ping），与"靶点不通"是两类问题。';
COMMENT ON COLUMN node_speedtests.download_mbps IS
    '下载带宽 Mbps。需要节点侧有测速文件，未接入时为 NULL（与 0 区分）。';

-- ---------------------------------------------------------------------------
-- 3. 测速靶点表（可配置，不写死在代码里）
-- ---------------------------------------------------------------------------
-- 为什么靶点要可配：三网靶点的可用性是**会变的** —— 实测发现
-- 101.227.255.16（电信-上海）与 123.125.99.1（联通-北京）都 100% 丢包
-- （它们不放行 ICMP），而 211.136.25.153（移动-北京）正常。
-- 若写死在代码里，用户遇到不通只能改代码重新部署；做成表则可以在界面上
-- 禁用某个靶点、或补一个新的，立刻生效。
CREATE TABLE IF NOT EXISTS speedtest_targets (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    target_key  TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    host        TEXT NOT NULL,
    carrier     TEXT NOT NULL,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    -- 排序权重：让"电信-上海"排在"电信-北京"前面这种偏好可配。
    sort_order  INTEGER NOT NULL DEFAULT 100,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT speedtest_targets_carrier_check
        CHECK (carrier IN ('telecom', 'unicom', 'mobile', 'other'))
);

COMMENT ON TABLE speedtest_targets IS
    '大陆三网测速靶点。可配置是因为靶点的 ICMP 可用性会变（实测多个骨干不放行 ping）。';

-- 预置靶点。选择依据是 **2026-10-02 从东京实测的可用性**，
-- 不是随便抄的 IP 列表 —— 原始那份常见靶点清单里，电信上海与联通北京
-- 都是 100% 丢包，直接写进去只会让用户看到一片"未测得"。
--
-- 取舍说明：这里只放实测**可 ping 通**的靶点。会丢包的靶点不是删掉，
-- 而是 enabled=false 留着 —— 用户想启用时能直接开，且能看到"它曾经不通"
-- 这个事实（比对着一份空表猜"为什么没有电信靶点"强）。
INSERT INTO speedtest_targets (target_key, name, host, carrier, enabled, sort_order) VALUES
    -- 实测通（2026-10-02 从东京，54.3ms 0% 丢包）
    ('mobile-bj',  '移动-北京', '211.136.25.153',  'mobile',  true,  10),
    ('mobile-sh',  '移动-上海', '211.136.112.200', 'mobile',  true,  11),
    ('mobile-gz',  '移动-广州', '211.139.163.6',   'mobile',  true,  12),
    -- 实测不通（不放行 ICMP）——默认禁用但保留，用户可自行启用验证
    ('telecom-sh', '电信-上海', '101.227.255.16',  'telecom', false, 20),
    ('telecom-bj', '电信-北京', '106.37.177.2',    'telecom', false, 21),
    ('telecom-gz', '电信-广州', '113.108.209.202', 'telecom', false, 22),
    ('unicom-bj',  '联通-北京', '123.125.99.1',    'unicom',  false, 30),
    ('unicom-sh',  '联通-上海', '210.22.97.1',     'unicom',  false, 31),
    ('unicom-gz',  '联通-广州', '221.5.88.1',      'unicom',  false, 32),
    -- 公共 DNS：三网都通、且延迟稳定，适合当"基准靶点"判断节点出网质量。
    -- 它们不严格属于某一家运营商，所以归 other。
    ('dns-ali',    '阿里 DNS',  '223.5.5.5',       'other',   true,  40),
    ('dns-tencent','腾讯 DNS',  '119.29.29.29',    'other',   true,  41),
    ('dns-114',    '114 DNS',   '114.114.114.114', 'other',   true,  42)
ON CONFLICT (target_key) DO NOTHING;
