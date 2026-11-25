-- 026: 节点分类 —— 让节点能按用途分组，并让订阅/商城按分类圈定节点范围。
--
-- 背景（2026-10-03 管理员需求）
-- --------------------------
-- 现在节点只有一个布尔 `in_store`（商城/自用），买家拿的是**全部已上架节点**。
-- 管理员要按用途分类（AI 专用 / 跨境专用 / 普通自用 …），并且：
--   * 商城商品选「AI 专用」→ 买家只拿到 AI 专用的节点
--   * 商城商品选「跨境专用」→ 买家只拿到跨境专用的节点
--   * 标签本身在面板后台可增删改，节点的标签可随时切换
--
-- 也就是说：**分类标签同时是「分组」和「上架范围」**。
--
-- 表设计取舍
-- ----------
-- 1) 分类单独建表（而不是在节点上存一个 text 字段）：
--    需要在后台增删改、要显示描述与排序、商城要按稳定标识引用。
--    存文本的话，改名就等于改所有引用它的地方。
--
-- 2) 节点↔分类用**多对多**（而不是节点上一个 category_id 外键）：
--    一个节点完全可能既算「AI 专用」又算「跨境专用」——
--    这两个用途并不互斥，用单选会被迫二选一，之后必然要改。
--
-- 3) slug 而不是 id 作为商城侧的引用键：
--    商城库里存的是文本列（product_node_plans.node_category），
--    存 uuid 也能用，但 slug 可读、排障时一眼能看出这个商品卖的是哪类，
--    而且商城侧不需要先查一次面板拿 id。slug 必须唯一且不轻易改。
--
-- 4) 订阅的 category_id **可空**：
--    NULL = 不按分类圈定 = 全部分类（= 现在的行为）。
--    这样现有订阅与现有订单**行为完全不变** —— 迁移零风险。

CREATE TABLE IF NOT EXISTS node_categories (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- slug 是商城侧的引用键，所以必须唯一且稳定。
    -- 限制字符集：商城那边是文本传递，避免空格/斜杠引起 URL 与 JSON 的歧义。
    slug        TEXT NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9_-]{0,31}$'),
    name        TEXT NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 40),
    description TEXT NOT NULL DEFAULT '',
    -- 展示顺序。同值时按 name 排，保证顺序稳定（不会因插入顺序不同而跳）。
    sort_order  INTEGER NOT NULL DEFAULT 0,
    -- 停用而不是删除：已有订阅可能引用它，
    -- 硬删会让那些订阅的 category_id 变成悬空外键。
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_node_categories_order
    ON node_categories (enabled, sort_order, name);

COMMENT ON TABLE node_categories IS
    '节点分类（AI 专用 / 跨境专用 / 普通自用…）。既是分组，也是商城圈定节点范围的依据。';
COMMENT ON COLUMN node_categories.slug IS
    '商城侧的引用键（product_node_plans.node_category）。改了会让商城的配置失配，慎改。';
COMMENT ON COLUMN node_categories.enabled IS
    '停用而非删除：已有订阅可能引用它，硬删会产生悬空引用。';

CREATE TABLE IF NOT EXISTS node_category_members (
    node_id     UUID NOT NULL REFERENCES hysteria_nodes(id) ON DELETE CASCADE,
    category_id UUID NOT NULL REFERENCES node_categories(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, category_id)
);

-- 反查「这个分类下有哪些节点」是最高频的查询（每次生成订阅都要用）。
CREATE INDEX IF NOT EXISTS idx_node_category_members_cat
    ON node_category_members (category_id, node_id);

COMMENT ON TABLE node_category_members IS
    '节点与分类的多对多关系。用多对多而非单选：一个节点可以既是 AI 专用又是跨境专用。';

-- 订阅挂分类。NULL = 不圈定（= 全部分类 = 现有行为）。
ALTER TABLE subscription_tokens
    ADD COLUMN IF NOT EXISTS category_id UUID REFERENCES node_categories(id);

CREATE INDEX IF NOT EXISTS idx_subscription_tokens_category
    ON subscription_tokens (category_id) WHERE deleted_at IS NULL;

COMMENT ON COLUMN subscription_tokens.category_id IS
    '该订阅只包含此分类下的节点。NULL = 不按分类圈定（全部分类，与加分类前的行为一致）。';

-- ---------------------------------------------------------------------------
-- 种子数据：把管理员点名的三类先建好，并把现有节点做个合理归位。
--
-- ⚠️ 现有 5 个节点**不预设任何分类**（保持 category_id 为 NULL 的语义），
--    由管理员在后台自己勾。这里只建分类本身 ——
--    替用户猜"哪台是 AI 专用"很容易猜错，宁可他点一下。
-- ---------------------------------------------------------------------------
INSERT INTO node_categories (slug, name, description, sort_order)
VALUES
    ('ai',      'AI 专用',   '面向 ChatGPT / Claude 等 AI 服务的节点，通常需要原生 IP', 10),
    ('cross',   '跨境专用',   '面向跨境访问、外贸办公等场景的节点',                      20),
    ('general', '普通自用',   '日常自用的通用节点',                                      30)
ON CONFLICT (slug) DO NOTHING;
