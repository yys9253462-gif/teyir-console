-- 041: 登录记录（2026-10-16）。
-- 每个账号每个来源 IP 一行：首次出现时间与最近一次登录时间。
-- 用来判断「新 IP 登录」并推送 Telegram，以及在账号安全页列出最近登录。
CREATE TABLE IF NOT EXISTS login_history (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ip INET NOT NULL,
    user_agent TEXT NOT NULL DEFAULT '',
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    login_count INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (user_id, ip)
);
CREATE INDEX IF NOT EXISTS idx_login_history_last ON login_history (user_id, last_seen_at DESC);
