-- 019: servers 表也加 status_error，用于记录「SSH 凭据失效但节点在线」这类情况。
--
-- 为什么需要它（2026-10-02 生产实测暴露）
-- -------------------------------------
-- 生产上有一台 Stockholm-AWS：SSH 端口通、但**认证失败**
-- （`ssh: unable to authenticate`）。这台机器其实是活的，
-- 但面板采不到它的指标。
--
-- 修法是在 SSH 认证失败时退回 API 探测（见 collector.collectServer），
-- 但那样一来「节点状态」会显示 online，而用户**完全看不出**
-- 这台机器的指标为什么是空的。必须有个字段说明：
-- 「在线，但 SSH 凭据失效，所以没有 CPU/内存数据」。
--
-- 为什么不复用 018 给 hysteria_nodes 加的那一列：
-- 那是**节点**级的状态错误（某个节点门户 API 不通），
-- 而这是**主机**级的问题（SSH 凭据失效，影响这台机器上所有节点的指标采集）。
-- 两者的作用范围与处理动作都不同，混用一列会让前端无法区分该提示什么。
--
-- 为什么不改 018 而是新建 019：
-- **已经应用过的迁移不能再改**。改了会让"文件内容"与"台账记录"不符，
-- 以后排查迁移问题时，看到的是文件说的和实际跑的不是一回事 ——
-- 那比多一个迁移文件麻烦得多。这是迁移系统的铁律。

ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS status_error TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN servers.status_error IS
    '主机状态相关的错误说明。例如"SSH 凭据失效: unable to authenticate"——'
    '表示机器在线但采不到指标，需要换凭据。空表示无异常。';
