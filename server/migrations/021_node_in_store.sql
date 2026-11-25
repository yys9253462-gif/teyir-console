-- 021_node_in_store.sql
-- 为 hysteria_nodes 增加 in_store 字段，标识该节点是否加入商城（向已购用户推送）。
-- 默认为 TRUE（存量节点默认均在商城中流通）。
-- 索引支持高效查询在商城中流通的节点。

ALTER TABLE hysteria_nodes
    ADD COLUMN IF NOT EXISTS in_store BOOLEAN NOT NULL DEFAULT TRUE;

CREATE INDEX IF NOT EXISTS idx_hysteria_nodes_in_store
    ON hysteria_nodes (in_store)
    WHERE deleted_at IS NULL;

COMMENT ON COLUMN hysteria_nodes.in_store IS '是否加入商城推送给购买用户；false 则仅限管理员自用，买家订阅不包含且会物理注销买家账号';
