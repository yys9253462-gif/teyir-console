-- 008: 合并订阅令牌。
--
-- 订阅地址是要粘进 Clash / sing-box / v2rayN 的，客户端不会带 JWT，
-- 所以它自带一个不依赖登录态的随机令牌，等同于一次性 bearer 能力：
-- 拿到它就能取走「全部节点 + 各节点主口令」的客户端配置。
-- 因此令牌必须可即时撤销（rotate 即换即失效），并且只存哈希没有意义
-- （服务端还要拿明文拼进 URL 展示给操作员），所以按普通机密对待：
-- 不入审计、不写日志、只在管理接口里回显给已登录的管理员。
CREATE TABLE IF NOT EXISTS subscription_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token TEXT NOT NULL UNIQUE,
    label TEXT NOT NULL DEFAULT '全部节点',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_accessed_at TIMESTAMPTZ,
    access_count BIGINT NOT NULL DEFAULT 0
);

-- 单行语义：面板只维护「一条」合并订阅。用部分唯一索引把它钉住，
-- 避免并发请求各插一行导致出现多条互不相同、有效期不一致的订阅地址。
CREATE UNIQUE INDEX IF NOT EXISTS idx_subscription_tokens_singleton
    ON subscription_tokens ((TRUE));
