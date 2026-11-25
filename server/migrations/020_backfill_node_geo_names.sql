-- 020_backfill_node_geo_names.sql
-- 将存量中仍然是域名形式（如 node1.example.com-15825）且所属主机已有规范命名（地区-商家）的节点，
-- 自动重命名回填为「主机名-端口」（如 东京-Alibaba Cloud-15825），保持全站命名一致。

UPDATE hysteria_nodes n
SET name = s.name || '-' || n.listen_port,
    updated_at = now()
FROM servers s
WHERE n.server_id = s.id
  AND n.deleted_at IS NULL
  AND s.deleted_at IS NULL
  AND n.name LIKE '%.%-%' -- 匹配形如 xxx.domain.com-port 的旧格式
  AND s.name NOT LIKE '%.%' -- 主机名已经是「地区-商家」格式
  AND n.name != s.name || '-' || n.listen_port;
