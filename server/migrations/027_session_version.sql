-- 部署前必须核对现役schema_migrations与文件最高编号，重编号后再交给既有迁移器。
-- 此快照未包含原server/migrations；不得用该目录替换生产完整迁移目录。
ALTER TABLE users ADD COLUMN IF NOT EXISTS token_version bigint NOT NULL DEFAULT 0;
COMMENT ON COLUMN users.token_version IS '管理JWT撤销版本；旧token缺ver按0，密码变更/角色变更原子递增';
