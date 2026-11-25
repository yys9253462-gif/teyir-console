# 商业层回归

针对「商城对接 → 订阅交付」这条链路的端到端测试。跑真实 HTTP 接口，不 mock。

## 跑法

```bash
# 从面板的「订阅中心 → 商城对接」拿到密钥（也可以用下面的办法直接从库里取）
KEY=$(ssh panel-host 'cd /opt/hy2-ops-console && docker compose exec -T db psql -U nodeops -d nodeops -t -A -c "SELECT value FROM system_settings WHERE key='"'"'store_api_key'"'"';"')

python tests/store-regression.py \
  --base https://panel.example.com \
  --store-key "$KEY" \
  --rounds 5 \
  --ssh-alias panel-host \
  --json-out report.json
```

退出码 0 表示全绿。`--only 3` 只跑第 3 轮，`--ssh-alias ""` 跳过需要登服务器的 IP 限额段。

## 为什么需要 `--ssh-alias`

IP 限额那一段必须**绕开 Caddy**，这不是为了省事，而是因为：

服务端只认 `X-Forwarded-For` 的**最后一跳**（防伪造的关键设计）。从公网打进来时，
最后一跳永远是 Caddy 追加的真实客户端地址，所以无论怎么伪造 XFF 都会被算成同一个
IP —— 那样验证的是 Caddy 的转发行为，不是限额逻辑。

所以 `ip-probe.py` 会 ssh 到目标机，直连 `127.0.0.1:18080`，让 XFF 原样送达。

## 覆盖的 80 条断言

| 组 | 测什么 |
|---|---|
| 下单校验 | 缺 `order_id`、无时长、超上限、无密钥各自的拒绝码 |
| 下单正常 | 字段回显、四种 URL 齐全 |
| 幂等 | 同 `order_id` 重放返回同一 token；**改参数重放也不改动已开订阅** |
| `auto_delete` | 显式 `false` 被尊重；缺省为 `true` |
| 密钥分离 | 公开视图零口令；`/sub/<token>` 才带 password |
| IP 限额 | 前 N 个放行、第 N+1 个 403、**已计过数的 IP 重访仍放行** |
| 到期 | 视图 200 但无 URL；`/sub` 403 `expired` |
| 优先级 | 已到期 + 超限时报「到期」而非「超限」 |
| 回收 | 宽限外 + `auto_delete` 才删；**两端都回 404**；`auto_delete=false` 不删 |
| 续费 | 未到期顺延；已到期重新起算；token 不变 |
| 撤销 | 403 `disabled`（可续费救回）；`delete=true` 才真删 |

## 三种「死法」必须能分开

这套测试最核心的断言是这一组。一条订阅失效可能有三条完全不同的出路，
如果回答都一样，客户和客服都无法判断该怎么办：

| 状态 | 响应 | 客户该做什么 |
|---|---|---|
| 到期 | `403 subscription expired` | 续费，链接不变 |
| 暂停（撤销） | `403 subscription disabled` | 联系客服或续费恢复 |
| 已回收 / 从未存在 | `404 subscription not found` | 重新购买 |

早期版本把「暂停」也答成 404，客户会以为链接作废而重新下单 —— 其实原样续费就能救回。

---

# 其余四个维度的回归

上面的 `store-regression.py` 回答「功能能不能用」。另外四个脚本各打一个维度，
每个都用同一套 `--json-out` 报告格式（`{round,total,passed,cases:[{label,pass,detail}]}`），
按轮次归档后可以直接对比历史。

```bash
python tests/format-negotiation.py --base https://panel.example.com \
  --token <有效token> --json-out r1-format.json          # 格式协商
python tests/concurrency.py --base https://panel.example.com \
  --store-key "$KEY" --ssh-alias panel-host --json-out r2-conc.json   # 并发与竞态
NODEOPS_ADMIN_EMAIL=... NODEOPS_ADMIN_PASSWORD=... \
  python tests/security-boundaries.py --base https://panel.example.com \
  --store-key "$KEY" --json-out r3-sec.json              # 越权与安全边界
python tests/boundaries.py --base https://panel.example.com \
  --store-key "$KEY" --ssh-alias panel-host --json-out r4-bound.json  # 状态机与边界值
node tests/delivery-ui.cjs --base https://panel.example.com \
  --store-key "$KEY" --json-out r5-ui.json               # 交付页真实渲染
```

## 格式协商（86 条）

`?format=` 的 11 个别名、按 UA 嗅探的三类客户端、无参数时的 Clash 兜底、
「别名压过 UA」的优先级、大小写与空白容错、以及三个格式各自的
`Content-Type` / `Content-Disposition` / `Cache-Control: no-store`。

一个已知的**诚实性问题（非功能缺陷）**：Clash 分支用 `json.MarshalIndent` 输出，
内容是 JSON 却声明 `text/yaml`。JSON 是 YAML 1.2 的子集，PyYAML 能解析、
`proxies`/`proxy-groups`/`rules` 三个键齐全，所以客户端不会出错；
但声明与内容不一致。测试里用的是**真解析一遍再查键**，
而不是找 `"proxies:"` 这种字符串 —— 后者对 JSON 载荷会全线误报。

## 并发与竞态（13 条）

8 路并发争抢、已知 IP 并发重访、8 路并发下单、5 路并发续费。

这轮揪出并修掉了三个真实竞态，都属于同一类「读-判断-写」：

| 症状 | 根因 | 修法 |
|---|---|---|
| `ip_limit=1` 时 8 路并发放行了 2 个 | 先读 `active_ips` 再判断再写回 | 单条 SQL，`SELECT … FOR UPDATE`，把判断和写入合并 |
| `ip_limit=2` 时 4 个已知 + 4 个新 IP 全部放行，账上只留 2 个 | 同上，并发覆盖写 | 同上 |
| 5 次 `+10 天` 并发续费只累加到 40 天 | 读出 `expires_at` 在 Go 里加天数再写回 | 算术搬进 SQL：`GREATEST(COALESCE(expires_at, now()), now()) + make_interval(days => $2)` |

`ssh_probe` 返回的是 `{ip: code}` 字典而不是按位置对应的数组 ——
并行子进程的完成顺序不确定，按位置读会串号（这个坑本身就让第一版测试报过假失败）。

## 越权与安全边界（75 条）

- **令牌边界 12**：空、过短、19 字符（下限下一字节）、129 字符（上限上）、
  尾部编码空格、含斜杠 —— 视图与订阅源两端都必须是 404。
- **存在性 oracle 1**：「格式合法但不存在」与「格式非法」的响应必须一致，
  否则可以拿来枚举 token 是否存在。
- **JWT 伪造 25**：`无 token` / `垃圾串` / `alg=none` 伪造 admin / `篡改签名`
  四种手法 × 6 个受保护端点（`/api/nodes`、`/api/subscriptions`、`/api/users`、
  `/api/audit`、`/api/servers`、`/api/subscription`）全部必须 401，
  外加一条「合法 token 能访问」的正向对照。
- **商城密钥 8**：完全不带、错误、前缀对尾部错、正确密钥多尾巴、空串全部 401；
  支持 `Authorization: Bearer` 形式；且商城密钥**不能**访问操作员端点。
- **泄漏面 13**：公开视图、`/api/health`、`/sub` 三个响应体里
  都不能出现 `obfs-password` / `auth_password` / `api_key` / `CREDENTIAL_KEY` /
  `JWT_SECRET` / `POSTGRES_PASSWORD`。
- **注入 8**：`..%2F` 路径穿越、编码穿越到受保护端点、`%00` 截断、CRLF 注入
  全部 404，且响应里不产生注入头。
- **失效订阅 8**：过期或撤销后，任何 `?format=` 组合、加 `&force=1`
  都不能把链接打开。

## 状态机与边界值（46 条）

这一轮问的是「极值处守不守规矩」，把几处原本只能从源码读出来的语义
变成线上实测结论：

| 项 | 有效区间 | 边界行为 |
|---|---|---|
| `ip_limit` | 0–1000 | 越界 400；**`0` 的语义是「不限设备」**，实测第 5 个不同 IP 仍 200 且账上记满 5 个 |
| `duration_days` | 0–3650 | 越界 400；`3650` 的 `days_left` 恰好 3650 |
| `expires_at` vs `duration_days` | — | 同时给时**以 `expires_at` 为准**：给 +100 天和 3 天，实得 `days_left=101` |
| `days_left` 哨兵 | — | `1 小时`→`1`（向上取整）；`过期 5 秒`→`0`；**永久才是 `-1`**，三者互不混淆 |
| 幂等重放 | — | 同 `order_id` 带 `ip_limit=1`/`duration_days=1` 重放，原单仍是 `1000`/`30 天` |

> 说明：下表为早期 7 天宽限期版本的历史边界记录；现行版本宽限已调整为 `0`（到期即收回），详见下文「到期立即回收（宽限 = 0）」一节。

宽限窗口（`now - 7d ± 2h`）的历史分界实测：

| 时间点 | `auto_delete` | 视图 | 订阅源 | 库里 |
|---|---|---|---|---|
| 宽限内 | 任意 | 200（过期未回收） | 403 | 还在 |
| 宽限外 | `true` | 404 | 404 | **确认真删了**（直接查库 `count=0`） |
| 宽限外 | `false` | 200 + `expired=true` | 403 | 还在 |

## 交付页真实渲染（30 条）

用 CDP 驱动 headless chrome，造出正常 / 暂停 / 到期三条订阅，
打开 `/user/<token>` 把 DOM 抓出来断言，截图留证。

两个硬要求：

1. **有效期内**必须给出可复制的订阅链接（非空输入框）、非空二维码、
   无空 `href` 的按钮。
2. **暂停与到期**都必须**一行链接都不渲染**：没有输入框、没有二维码、
   没有「复制」「下载」按钮。

第 2 条曾经是坏的：交付页只判断 `expired`，没判断 `enabled`。
已暂停的订阅（`enabled=false, expired=false`）会走进正常分支，
渲染出空二维码、两个空输入框、`href=undefined` 的按钮，
**以及完整的节点列表（含 `host:port`）** ——
页面上半部分写着「订阅已被暂停」，下半部分却在展示交付内容，
还把节点主机泄露给了一个已经失去访问权的客户。
修法是把判断改成 `view.expired || !view.enabled`。

> ⚠️ 这个缺陷接口层测不出来：`/api/sub/view` 在暂停时确实不下发任何 URL，
> 测试全绿。只有真的把页面渲染出来，才能看到前端拿 `undefined` 兜底成了什么。

## 换行符：三个会被误判成功能问题的坑

1. **`grep -c $'\r' file` 在 Git Bash 里不展开 `$'\r'`**，实际等价于
   `grep -c r`（数含字母 r 的行数），会报出巨大的假 CRLF 数。
   必须用 `python -c "print(open(f,'rb').read().count(b'\r'))"`。
2. **`subprocess.run(..., input=script, text=True)` 会在 Windows 上凭空注入 CR**：
   哪怕源文件 CR=0，文本模式也会把写入子进程 stdin 的每个 `\n` 变成 `\r\n`，
   经 `ssh host 'bash -s'` 喂给 Linux 就报 `$'\r': command not found`，
   且报错行号指向无关位置。**一律 `input=script.encode()`，不开 `text=True`。**
3. 只加 `.gitattributes` **不会**自动修好已有工作区。若仓库里的 blob 本来就是 LF，
   `git add --renormalize .` 会是空操作。真正生效需要重新检出：
   `git rm --cached -r . && git reset --hard HEAD`。

---

# 买家链路端到端验收

```bash
python tests/buyer-delivery-e2e.py --base https://panel.example.com \
  --store-key "$KEY" --ssh-alias panel-host --json-out buyer-e2e.json
```

⚠️ **`--base` 是给「运行脚本的本机」发请求用的，不是给远端测试机用的。** 这个脚本的 HTTP 调用
从运行它的那台机器直接发出，所以：

- 在**本机（Windows / 本地开发机）**跑 → `--base https://panel.example.com`（公网地址）。
  填 `127.0.0.1:8080` 会全部 `got=0`，看起来像产品挂了，其实是打到自己机器上没人听。
- 在**目标测试机本机**跑 → 才能用 `--base http://127.0.0.1:8080`。

只有 `--ssh-alias` 相关的操作（造到期数据、查库核对）才走 SSH 进目标测试机。

前五个脚本验证的是「系统的各个部件对不对」。这个脚本验证的是
**运营方对买家作出的承诺有没有兑现** —— 不查接口能返回什么，
只查买家从下单到到期这一路上拿到的东西对不对。45 条断言分四组：

| 组 | 承诺 | 怎么验 |
|---|---|---|
| P1 | 购买后发货页带订阅链接与二维码 | 四个格式地址齐全，且**每个都真的能拉到内容**（有地址取不到内容就是死链）；交付视图带 `uri_url`（二维码内容源） |
| P2 | 订阅包含**所有**已添加的 hy2 节点 | 拿库里 `deleted_at IS NULL` 的节点做基准逐个核对；`excluded` 必须为空 |
| P3 | 到期必须收回 | 库里 `1 → 0`，确认真删而不是只对外隐藏 |
| P4 | 收回后节点与链接都不能用 | 四格式 + 交付视图全 404；且失效响应体**不含任何节点主机名或口令** |

## 关于 `excluded` 必须为空

订阅里的节点是**实时向节点侧 `/node/meta` 拉取**的（见 `resolveSubscriptionNode`）。
拉不到就进 `excluded` —— 节点仍在册，但不会出现在买家的配置里。

所以「包含所有已添加的节点」这句话，真正的验收标准是 `excluded` 为空，
而不是 `node_count > 0`。常见进 `excluded` 的原因：

| reason | 含义 |
|---|---|
| 未绑定节点侧 API，拿不到该节点的口令 | 节点没填 `api_url` / `api_key` |
| 读取节点 meta 失败：… | 节点侧 portal 不可达（机器停了、端口不通） |
| 节点 meta 里没有 auth_password | 节点侧没生成口令 |

⚠️ 实测教训：`node2.example.com-19984` 曾长期挂在 `excluded` 里，因为它的域名
解析到**已退役的 GCP 机**、hy2 端口连不上。交付页会把它列在
「另有 N 个节点暂时不可用」，看起来像正常提示，实际是节点早就废了。
验收时一定要看 `excluded` 的 reason，不要只看有没有节点。

## 🔴 造「到期时间」必须用服务器时钟

```python
srv_now = ssh("SELECT EXTRACT(EPOCH FROM now())::bigint")
```

不能用本机 `time.time()`。实测本机与目标测试机时钟可能存在数秒偏差，于是用本机算的
「5 秒前」传到服务器眼里还是「4 秒后到期」，下单响应会**正确地**回
`expired=false`、并且照常下发订阅链接 —— 看起来像「到期不生效」这个
功能坏了，其实是测试把数据造错了。

同一个坑还有第二种表现：`P3.4`（确认库里真删了）如果在读取后立刻查库，
偶尔会读到还没提交的状态。回收是「读时懒删、同请求内完成」，
所以加几百毫秒重试即可，不必改产品代码。

## 到期立即回收（宽限 = 0）

`subscriptionExpiryGrace` 现在是 **0**，不是 7 天。这是运营方明确要求的产品规则：
**到期即收回，链接立即失效，不保留宽限**。

三处判断在 `grace=0` 时退化为同一个条件，改动时不要只改一处：

| 位置 | 条件 |
|---|---|
| `expired()` | `now >= expires_at` |
| 视图 / 订阅源的回收判断 | `now >= expires_at + grace` |
| 清扫器 SQL | `expires_at <= now - grace` |

两个端点的回收判断都用 `!now.Before(...)`（即 `>=`）而不是 `now.After(...)`（`>`）。
用 `After` 会在 `grace=0` 时留下一个瞬间：行已被判为过期却还没回收，
调用方拿到 403 紧接着行就消失了。

**后果**：过期客户看到的是 404（等同于「这个 token 从未存在」），
而不是「已到期，请续费」的 403 页面；也无法再靠订单号续费救回，
只能重新下单。这是有意为之，不是回归。

⚠️ 写测试时注意：**不存在「宽限内」这种中间态**。“已到期但仍可打开”
（视图 200 / `/sub` 403 expired）**只**出现在显式 `auto_delete=false` 的
订阅上。曾有三个套件拿「过期 2 天、7 天宽限内」做断言而假失败，
后来都改成显式传 `auto_delete`。

---

## 🔴 验收「收回」必须打到节点侧

**只验「我们这边删了没」是不够的，会漏掉真 bug。**

面板侧全绿 ≠ 买家的网断了。曾经的真实 P0：订阅下发的口令是**节点侧共享
口令**（对应 `admin_master`，2036 年到期），所以链接 404 了、库里删干净了，
买家手里那份配置**照样能上网**。45 条买家验收里没有一条能发现它 ——
因为它们测的都是面板侧承诺。

```bash
# 正确判据：拿买家实际拿到的口令直接问节点侧
curl -s -X POST http://127.0.0.1:29861/auth \
  -H 'Content-Type: application/json' \
  -d '{"addr":"203.0.113.20:12345","auth":"<买家口令>","reqAddr":"example.com:443","rx":0,"tx":0}'
# 已收回 → {"ok":false,"msg":"User not found"}
# 有效   → {"ok":true,"id":"sub_<订阅ID>"}
```

### 两个脚本的分工

| 脚本 | 断言 | 修复前 | 修复后 |
|---|---|---|---|
| `expiry-really-works.py` | 「共享口令**仍然可用**」= 证明 bug 存在 | 通过 | **必定失败**（可删） |
| `expiry-fixed-verify.py` | 「买家口令**被拒**、账号消失」= 证明已修 | 必定失败 | **9/9 通过** |

`expiry-fixed-verify.py` 用法：

```bash
python tests/expiry-fixed-verify.py \
  --base https://panel.example.com --store-key <商城密钥> \
  --ssh-alias panel-host --node-host <节点IP> --node-key <portal api_key> \
  --node-ssh-key "/path/to/node-ssh-private-key"
```

### ⚠️ 这个脚本自己踩过的三个坑（都会把 bug 伪装成通过）

1. **用 `psql -c "DELETE FROM subscription_tokens ..."` 触发回收** ——
   绕过了应用，证明的只是「我的 DELETE 生效了」。必须走真实商城路径
   `POST /api/store/revoke {delete:true}`。
2. **用 `GET /api/v1/users/list` 数节点账号** —— 当时该端点不存在
   （返回 404 `API endpoint not found`），恒得到 0，`0 -> 0` 看着很一致，
   正好掩盖真实孤儿。改为 SSH 读 `/etc/hysteria/portal.json` 数 `users`。

   > **2026-09-30 更新**：节点侧 portal 后来升级了，`/api/v1/users/list`
   > **现在是可用的**（实测返回完整用户数组）。但判断「这个信源能不能用」
   > 仍然不能只看 HTTP 200 —— 要检查返回体里真的有 `users` 数组。
   > `purge-node-users.py` 会把三个信源（面板接口 / portal 接口 / SSH 读
   > portal.json）都试一遍并**逐个标注可用性**，就是为这个。

3. **`curl -w '%{http_code}'`** —— 经 Windows bash → ssh → 远端 bash
   三层引号，花括号转义会丢，输出变成 `}`。改用 `-D -` 读状态行。

**教训**：脚本报「通过」时，先确认它真的在测被测对象。

---

## ⚠️ `--ssh-alias` 不传会让断言静默跳过

`SSH_ALIAS = None` 时所有依赖库查询/节点侧探测的断言**直接跳过**，
而且常常表现为一个莫名其妙的失败（例如 `P4 前置失败：没有已到期的 token`），
很容易被误读成功能问题。跑这些套件一律带上 `--ssh-alias panel-host`：

```bash
python tests/buyer-delivery-e2e.py --base https://panel.example.com \
  --store-key <key> --ssh-alias panel-host
```

---

## 清理测试残留要经应用，不要直接 DELETE

一轮套件跑下来会在节点侧开出几十个账号。清理时**必须**逐条走
`POST /api/store/revoke {delete:true}`：直接 `DELETE` 表会跳过吊销，
在节点上留下一堆仍然能连的孤儿账号（这正是本轮修的那个 bug 本身）。

```python
# 逐 order_id 吊销，节点账号随之收回
POST https://panel.example.com/api/store/revoke
X-Store-Key: <商城密钥>
{"order_id": "<oid>", "delete": true}
```

动手前先确认没有真孤儿（节点上有、库里没引用）：

```bash
comm -23 <(sort -u 节点账号列表) <(sort -u 库中 node_accounts 引用的 user_id)
```

### 自动化：`purge-node-users.py`

手工比对容易漏，所以把清点与清理做成了脚本：

```bash
# 只清点，不删（在能 ssh 到节点的机器上跑）
python tests/purge-node-users.py --ssh-alias panel-host

# 直接放在节点那台机器上跑，用空别名表示「就在本机」
python tests/purge-node-users.py --ssh-alias ""

# 真删；默认只删 sub_* 前缀，admin_master 永不删
python tests/purge-node-users.py --ssh-alias "" --apply
```

它做三件手工容易漏的事：

1. **交叉清点**。同时试面板接口、portal 的 `/api/v1/users/list`、SSH 读
   `portal.json`，并逐个打印「可用 / 不可用」。任何一个信源给出 0 都不会被
   直接采信 —— `0 -> 0` 恰恰是历史上掩盖过真实孤儿的那种假象。
2. **429 退避重试**。面板写接口按用户限流，一次连删上百个会大量 429。
   不退避时实测 110 个只成功 19 个（剩下的被误记为「失败」，其实等一会儿就能删）。
3. **默认保守**。不传 `--prefix` 时只动 `sub_*`；`admin_master` 永远不会被删，
   哪怕前缀白名单写成了它。

实测：一次跑完 91 个 `sub_*` 残留，成功率 91/91，节点侧从 92 个回落到 1 个。

> ⚠️ **面板管的是外部节点时，别用这个脚本。**
> 它假定「面板与被管主机同机」，靠 SSH 读本机 `portal.json` 作为可信信源。
> 若面板只管着一个用户导入的外部节点，那个信源读到的是**另一台机器**，
> 会得出完全错误的结论 —— 实测出现过「面板说有 134 个账号 /
> 本机 portal 说 1 个」这种三方不一致，差点把「清理干净了」当结论。
> 那种情况一律走面板接口：`GET /api/nodes/{id}/users` +
> `DELETE /api/nodes/{id}/users/{uid}`。

---

## 用面板自己的凭据登到它管的主机上

`with-panel-credential.py` 用面板存的（加密）凭据登录，不需要操作者
把密码贴进终端：

```bash
python tests/with-panel-credential.py --ssh-alias "" --probe
python tests/with-panel-credential.py --ssh-alias "" --run 'systemctl is-active hysteria-server'
```

它存在的意义：有些事**只能在被管主机上做**（改 `config.yaml`、重启 hy2、
开 `trafficStats`），而这些主机往往是用户通过面板导入的 —— 密码只在面板
库里加密存着。

### 两个格式细节（都踩过，写在脚本注释里了）

1. **`server_credentials.ciphertext` 是 `bytea`，不是 text。**
   psql 用 `\x` 前缀 + 十六进制输出。当成 base64 解会报
   `Incorrect padding` —— 因为压根不是同一种编码。
   49 字节密文 = nonce(12) + 密文 + tag(16)，明文是 21 字节的口令。
2. **`openssl enc` 不支持 AEAD。**
   实测 `openssl enc -aes-256-gcm` 报 `AEAD ciphers not supported`，
   GCM 必须走库：`apt-get install python3-cryptography`。

### 顺带验证了「面板能不能自己运维它管的节点」

用这套凭据做过一次完整操作：给外部节点开 `trafficStats`（改 `config.yaml`
+ 重启 hy2 + 回填面板的 `traffic_api_url`/`traffic_secret_file`）。
通路是通的，面板不需要额外能力就能自己做到。


---

# 面板五轮实测（以面板为被测对象）

`panel-5rounds.py` 把「小白打开面板看到什么」当判定口径，经公网域名
`https://panel.example.com` 走 Caddy 访问 —— **不要直连 `127.0.0.1:8080`**：
面板靠 `X-Forwarded-*` 拼订阅链接，回环口会拼出 `http://127.0.0.1:8080/sub/...`，
那是测试姿势错，不是产品缺陷（此坑已踩过一次，见脚本头部注释）。

```bash
PANEL_PASSWORD=<SEED_ADMIN_PASSWORD> python tests/panel-5rounds.py
```

## 真实故障注入演练原则（历史说明）

R1–R3、R5 都是读接口断言，唯独告警中心没法靠读来验证 —— 「当前 0 条告警」
既可能是真的没事，也可能是**告警永远不响**。开源版不内置直接停止目标节点服务的注入脚本，
如需在隔离测试机演练告警闭环，可参考以下方法手工或自编脚本停掉测试节点上的 `hysteria-server.service`，
盯完整条链路：
节点转 `offline` → 出现 `node_offline` 告警 → 时间线写 `triggered`/`reopened`
→ ack 转「已确认」→ 恢复后自动 `resolved`。跑完务必重启服务，不留副作用。

### 三个必须写进脚本注释的教训（都是模拟真实故障时踩的）

1. **要停的是"真正在 listen 的那个"服务。**
   第一版用 `grep -i hysteria | head -3` 取到 `hy2-iptables.service` ——
   它只是个 iptables 落盘服务，停掉它端口照旧在听。于是测试全程"没故障"，
   白跑 3 分钟还得出"告警不工作"的错误结论。
   正确做法：用 `ss -lunp 'sport = :PORT'` 反查端口归属的进程，停完再核实
   监听确实消失了（`wc -l` 只剩表头），否则换下一个候选。

2. **不要跟 30 秒的 reconcile 周期赛跑。**
   「节点状态」由采集器写、「告警状态」由 reconcile 写，两者最多差一个周期。
   曾一度在看到节点 `online` 时就断言，报出假失败；也曾因为 `acknowledged`
   不在 `open` 列表里，误判成"已归档"。**正确做法是直接盯目标告警的
   `status` 字段变成 `resolved` 为准**，而不是看衍生列表。

3. **`reopened` 不是 bug。**
   同一条告警此前 resolve 过、现在又犯，时间线写的是 `reopened` 而非
   `triggered` —— 这恰恰是设计意图（让"反复抖动"和"首次出现"可区分）。
   断言要接受二者之一。
