-- 015: 3X-UI 面板补上地区与商家，并记住「名字是谁定的」。
--
-- 背景
-- ----
-- servers 表早就有 region / provider 两列（001_init.sql），但 xui_panels
-- 建表（014）时漏了 —— 于是同一个控制台里，hy2 节点能标出「这台是东京的
-- AWS」，3X-UI 面板只能叫「日本面板1」，全靠名字里自己写。
--
-- 这次补上，并让新增面板时与节点导入走**同一套**自动识别规则
-- （见 server/internal/httpapi/geoapply.go）：
--   命名＝地区-商家（如 Tokyo-AWS），已手工填过的一律不覆盖。
--
-- 为什么还要 name_source 这一列
-- -----------------------------
-- 「已手工填过的不覆盖」这条规则要能真的生效，就必须**区分得出**
-- 当前这个名字是人打的还是系统填的。只靠「name 非空」判断是不够的：
-- 自动填完之后 name 也非空了，第二次识别就会被误判成「有手工值」而跳过；
-- 反过来若不加区分地覆盖，又会把运维者精心起的业务代号（"HK-主力"）
-- 冲掉 —— 那是更难接受的错误。
--
-- 所以显式记来源：
--   auto   由 geo 识别自动生成，**允许**被后续识别更新
--   manual 人工填写/修改过，**永不**被自动识别覆盖
--
-- ⚠️ 关键：**加列时的默认值必须是 manual，不是 auto**。
--
-- 这两张表在加这一版迁移之前**根本没有任何自动识别**，所以库里所有现存行
-- 的 name/region/provider 100% 是人手打的。若默认 auto，就意味着"这些
-- 精确的人工命名随时可能被一次自动识别冲掉" —— 正好踩中用户明确否掉的那条。
-- 所以：加列 → 存量行全部标记为 manual → 再把列默认值改成 auto（给新行用）。
-- 顺序不能反，否则存量行的"曾是人工填写"这个事实就永久丢失了。
--
-- 同理，region/provider 三列都各自记来源：运维者手填 region=Tokyo、但没填
-- provider 时，应当只补 provider、不动 region。

ALTER TABLE xui_panels
    ADD COLUMN IF NOT EXISTS region TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS provider TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS name_source TEXT NOT NULL DEFAULT 'manual',
    ADD COLUMN IF NOT EXISTS region_source TEXT NOT NULL DEFAULT 'manual',
    ADD COLUMN IF NOT EXISTS provider_source TEXT NOT NULL DEFAULT 'manual';

-- 存量行已标为 manual（上面的 DEFAULT 生效）。现在把默认值切回 auto，
-- 让**以后新插入**的行默认走自动识别。
ALTER TABLE xui_panels ALTER COLUMN name_source SET DEFAULT 'auto';
ALTER TABLE xui_panels ALTER COLUMN region_source SET DEFAULT 'auto';
ALTER TABLE xui_panels ALTER COLUMN provider_source SET DEFAULT 'auto';

-- 来源只允许这两个值。用 CHECK 而不是 ENUM：本项目其余部分都用 TEXT +
-- 应用层校验，引入 ENUM 会让后续加值需要 ALTER TYPE（还得小心锁表）。
-- 但这里值得有一条约束 —— 一旦写进脏值，「是否覆盖」的判断就会静默走错分支，
-- 而那正是本功能最容易出错、又最难发现的地方。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'xui_panels_source_values'
    ) THEN
        ALTER TABLE xui_panels ADD CONSTRAINT xui_panels_source_values CHECK (
            name_source IN ('auto', 'manual') AND
            region_source IN ('auto', 'manual') AND
            provider_source IN ('auto', 'manual')
        );
    END IF;
END $$;

COMMENT ON COLUMN xui_panels.region IS
    '地区短名（如 Tokyo），新增面板时由 IP 自动识别填充，可手工覆盖。';
COMMENT ON COLUMN xui_panels.provider IS
    'VPS 商家短名（如 AWS），新增面板时由 IP 自动识别填充，可手工覆盖。';
COMMENT ON COLUMN xui_panels.name_source IS
    '名字来源：auto=自动识别生成（可被更新），manual=人工填写（永不被覆盖）。';
COMMENT ON COLUMN xui_panels.region_source IS
    'region 来源：auto/manual，语义同上。';
COMMENT ON COLUMN xui_panels.provider_source IS
    'provider 来源：auto/manual，语义同上。';

-- servers 表同样处理，顺序与理由完全一致（存量行全为人工命名）。
ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS name_source TEXT NOT NULL DEFAULT 'manual',
    ADD COLUMN IF NOT EXISTS region_source TEXT NOT NULL DEFAULT 'manual',
    ADD COLUMN IF NOT EXISTS provider_source TEXT NOT NULL DEFAULT 'manual';

ALTER TABLE servers ALTER COLUMN name_source SET DEFAULT 'auto';
ALTER TABLE servers ALTER COLUMN region_source SET DEFAULT 'auto';
ALTER TABLE servers ALTER COLUMN provider_source SET DEFAULT 'auto';

-- ⚠️ 上面那条「存量全部标 manual」有一条必须回捞的例外。
--
-- 真机数据（2026-10-01 在 se 上实测）暴露了它：升级前所有由
-- ensureHostForImportedNode 自动登记的主机，形态都是
--     name=<域名>  region=''  provider='API 节点'
-- 也就是说 provider 那列里写的是**旧代码的硬编码兜底串**，
-- 不是任何人手打的商家名。
--
-- 如果照「存量全 manual」一刀切，这些机器的 provider 就永远停在
-- "API 节点" 上 —— 而这个功能的全部意义就是"加节点时自动认出商家"。
-- 用户会看到"我升级了，但它还是没认出 AWS"，然后合理地认为功能坏了。
--
-- 所以只回捞这一种**可判定的系统兜底值**：provider 恰好等于
-- 'API 节点' 的行，把它标成 auto，允许下次识别更新。
--
-- 为什么可以安全地这样判定：'API 节点' 是代码里的字面量常量
-- （见 nodeapi.go 的历史实现），真实商家名不可能恰好是它。
-- 这是一次有明确依据的数据修正，不是"猜哪行是自动生成的"。
--
-- region 同理：旧代码写死空串，空串在 fillable() 里本来就算"可填"，
-- 所以不需要额外处理。
UPDATE servers
   SET provider_source = 'auto'
 WHERE provider = 'API 节点'
   AND provider_source = 'manual';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'servers_source_values'
    ) THEN
        ALTER TABLE servers ADD CONSTRAINT servers_source_values CHECK (
            name_source IN ('auto', 'manual') AND
            region_source IN ('auto', 'manual') AND
            provider_source IN ('auto', 'manual')
        );
    END IF;
END $$;

COMMENT ON COLUMN servers.name_source IS
    '名字来源：auto=自动识别生成（可被更新），manual=人工填写（永不被覆盖）。';
COMMENT ON COLUMN servers.region_source IS
    'region 来源：auto/manual，语义同上。';
COMMENT ON COLUMN servers.provider_source IS
    'provider 来源：auto/manual，语义同上。';
