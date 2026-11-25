-- 005: 软删除的主机不应继续占用 (host, ssh_port) 唯一位。
-- 之前删掉一台主机后，再用同样的地址+端口登记会被判成「server already exists」，
-- 因为表级 UNIQUE 把 deleted_at 非空的行也算进去了。改成只约束未删除的行。
ALTER TABLE servers DROP CONSTRAINT IF EXISTS servers_host_ssh_port_key;
CREATE UNIQUE INDEX IF NOT EXISTS servers_host_port_active ON servers (host, ssh_port) WHERE deleted_at IS NULL;
