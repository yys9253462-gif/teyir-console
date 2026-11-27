# Teyir 控制台（teyir-console）

> 本仓库为开源脱敏发行版：已移除生产部署拓扑、私有域名、公网节点地址、密钥凭据、真实客户/订单样本与内部运维文档，所有外部联动（面板跳转域名、商城续费地址、宿主机封禁接口、节点一键安装脚本与门户更新源）均已改为按环境变量或界面设置配置。部署前请按需填写 `.env` 中的必填密钥与可选外部配置；本项目采用 GNU AGPL-3.0 协议开源（见根目录 `LICENSE`）。

VPS 与 Hysteria2 节点运维控制台。Go API + PostgreSQL + React 前端，指标与节点状态通过 SSH 或节点门户接口采集，任务由执行器下发。

## 架构

```
浏览器 ── HTTPS ── Caddy ─┬─ /api/* ─→ Go API（容器内 8080，仅监听回环）─→ PostgreSQL
                          └─ /*     ─→ 前端静态资源（dist/，SPA 回落 index.html）
                                  │
                          SSH（每主机每周期 1 条连接）─→ 被管主机
```

- 前端：React 18 + TypeScript + Vite。
- 后端：Go 1.26，标准库 `net/http`，`pgx/v5` 访问 PostgreSQL 16。
- 采集与执行：`golang.org/x/crypto/ssh`，在目标主机上运行只读命令取指标、用 `ss -lun` 判定 HY2 端口监听、从节点侧统计文件读取累计用量。
- 生产编排：`docker-compose.yml` 起 db / migrate / api / web 四个服务，其中 `web` 是 Caddy，负责托管前端并把 `/api` 转发给 `api`。前端**同源**访问 API，因此生产环境不涉及 CORS。

## 环境变量

见 `.env.example`。全部必填项都没有默认值，缺失时进程直接拒绝启动：

| 变量 | 说明 |
| --- | --- |
| `POSTGRES_PASSWORD` | 数据库密码 |
| `JWT_SECRET` | ≥32 字符，签发会话令牌（可选 `JWT_TTL_MINUTES` 调整有效期，默认 720 分钟） |
| `SEED_ADMIN_EMAIL` / `SEED_ADMIN_PASSWORD` | 首次启动的种子管理员（密码 ≥12 字符） |
| `CREDENTIAL_KEY` | 标准 base64(32 字节)，SSH 私钥与节点密钥的 AES-256-GCM 主密钥。**有凭据入库后不可更改** |
| `ENABLE_COLLECTOR` | `true` 才启动周期采集，否则面板只有存量数据（可选 `COLLECTOR_INTERVAL_SECONDS` 默认 30） |
| `EXECUTOR_INTERVAL_SECONDS` | 任务执行器轮询间隔（默认 20 秒；另有 `SPEEDTEST_INTERVAL_MINUTES` 默认 15，设 0 禁用周期测速） |
| `SITE_ADDRESS` | 留空（注释掉该行）监听 `:80`；填域名则自动申领 Let's Encrypt 证书并监听 `:443` |
| `API_BIND` / `API_PORT` | API 映射到宿主机的地址与端口，默认 `127.0.0.1:8080` |
| `WEB_BIND` / `WEB_PORT` / `WEB_TLS_PORT` | web 服务映射到宿主机的地址与端口（默认仅绑定 `127.0.0.1`，公网直出请显式设 `0.0.0.0`） |
| `PANEL_PUBLIC_URL` | 可选，控制台外部访问地址（用于 Telegram 告警与菜单跳转按钮） |
| `SUBSCRIPTION_PUBLIC_BASE_URL` / `SUBSCRIPTION_BACKUP_BASE_URLS` | 可选，独立订阅/交付链接主域名与逗号分隔备用域名（留空则沿用面板访问域名） |
| `SUBSCRIPTION_DEFAULT_NAME` | 可选，客户端导入后的默认订阅分组名（默认 `Teyir`，也可在系统设置页覆盖） |
| `STORE_RENEW_BASE_URL` | 可选，外部商城续费基础地址（留空则交付页不展示外部续费跳转链接） |
| `TURNSTILE_SITE_KEY` / `TURNSTILE_SECRET` | 可选，Cloudflare Turnstile 登录人机验证（两项同时配置才启用） |
| `LOGIN_SERVICE_KEY` / `LOGIN_SERVICE_EMAIL` | 可选，自动化登录豁免密钥（≥32 字符）与允许豁免的账号邮箱（留空默认仅限种子管理员） |
| `BAN_WEBHOOK_URL` / `BAN_WEBHOOK_TOKEN` | 可选，宿主机封禁联动接口地址与令牌（供 Telegram 封禁管理调用） |
| `HY2_INSTALL_SCRIPT_URL` / `HY2_INSTALLER_REPO` | 可选，一键安装节点脚本地址（HTTPS）与门户批量更新仓库（格式 `owner/repo`） |
| `TRUSTED_PROXY_CIDRS` / `VIEWER_RESTRICT_SUBSCRIPTIONS` | 可选，可信反代网段（逗号分隔 CIDR）与是否限制只读角色查看订阅凭证（默认 `false`） |
| `PORTAL_TLS_PINS` | 可选，自签节点门户证书 SHA256 指纹固定映射（JSON 对象） |
| `GEO_*` / `IP_PURITY_*` | 可选，离线/在线地区归属与 IP 纯净度检测配置（含 `IP_PURITY_IPQS_KEY`，详见 `.env.example`） |

## 部署

### 方式一：一条命令交互菜单式安装（推荐）

在**目标 Linux 服务器**上直接执行（支持 Debian / Ubuntu / CentOS / RHEL / Rocky / Alma / Alpine，自动检测并安装 `git` / `docker` / `docker compose`，自动避让冲突端口，自动识别已有 Caddy 或输出 Nginx 反代配置）：

**Root 用户（终端提示符为 `#`）**：

```bash
curl -fsSL https://raw.githubusercontent.com/yys9253462-gif/teyir-console/main/install.sh -o ~/teyir-install.sh && bash ~/teyir-install.sh
```

**普通用户（终端提示符为 `$`）**：

```bash
curl -fsSL https://raw.githubusercontent.com/yys9253462-gif/teyir-console/main/install.sh -o ~/teyir-install.sh && sudo bash ~/teyir-install.sh
```

> 若服务器访问 `raw.githubusercontent.com` 不畅，可改用备用源（`cdn.jsdelivr.net`，普通用户把 `bash` 改为 `sudo bash`）：
> ```bash
> curl -fsSL https://cdn.jsdelivr.net/gh/yys9253462-gif/teyir-console@main/install.sh -o ~/teyir-install.sh && bash ~/teyir-install.sh
> ```
> 安装完成后脚本会自动备份到安装目录并注册全局快捷命令 `teyir`（普通用户用 `sudo teyir`），日后随时输入 `teyir` 即可再次打开交互管理菜单。

交互菜单内置以下功能：
1. **一键安装 / 重新配置部署**：仅需回答 2–3 个问题（访问方式与域名、初始管理员账号、可选高级项），其余密钥与端口自动安全生成；重配时自动保留既有 `CREDENTIAL_KEY`、`POSTGRES_PASSWORD` 与你在 `.env` 中手工追加的自定义环境变量，并自动避让已有监听端口、Docker 端口范围与 NAT 端口重定向区间。
2. **一键升级更新**：自动备份配置与数据库 → 拉取最新源码（同时支持 Git 仓库与压缩包安装形态平滑升级）→ 重建并执行 `migrate` 迁移 → 重建 `api` / `web` → 容器内部健康验收。
3. **运行状态总览**：查看容器状态、容器内部健康探针、公网 HTTPS 连通性与证书到期时间（只读操作不强制 root）。
4. **修改配置子菜单**：单独调整域名/访问方式、端口、默认订阅名、周期采集开关或外部联动地址，改共享 Caddyfile 前自动备份、校验、热重载，失败自动回滚。
5. **服务运维子菜单**：启停、重启、查看 `api`/`web`/`db` 日志、立即备份、手动触发数据库迁移。
6. **检查远端更新** / **安全卸载**（默认保留数据库卷与安装目录，彻底清除需二次确认或 `--purge`）。

全自动与预演模式（适合脚本化或批量部署）：

```bash
bash ~/teyir-install.sh --dry-run --install                                    # 仅预演不改系统
bash ~/teyir-install.sh --yes                                                  # 全自动默认安装
bash ~/teyir-install.sh --yes --domain panel.example.com                       # 全自动绑域名配 HTTPS
bash ~/teyir-install.sh --status | --upgrade | --check-update | --uninstall    # 直接调用维护命令
```

### 方式二：手工 Docker Compose 部署

```bash
cp .env.example .env
# 生成 URL 安全的数据库密码（避免含 + / = 破坏连接串解析）
DB_PASS="$(openssl rand -base64 36 | tr '+/' '-_' | tr -d '=\n')"
sed -i "s|replace-with-a-long-random-value|${DB_PASS}|" .env
# ……其余必填项同理（注意 CREDENTIAL_KEY 须使用标准 openssl rand -base64 32），或手工用编辑器填

docker compose up -d --build
docker compose logs -f api          # 观察启动
```

启动顺序由 `depends_on` 保证：`db` 健康 → `migrate` 迁移成功退出 → `api` 健康就绪 → `web` 启动。
默认仅绑定 `127.0.0.1:80` 供外层反代转发；若需单机直接对外开放，请在 `.env` 中显式设置 `WEB_BIND=0.0.0.0` 后从 `http://<主机>:80` 打开面板。

带域名自动 HTTPS：

```bash
# .env 里设置
WEB_BIND=0.0.0.0
SITE_ADDRESS=panel.example.com
```

前置条件：域名已解析到本机，且 80/443 对公网可达（ACME 校验需要）。Caddy 会自动申领并续期证书，无需额外操作。

**升级流程**：用仓库自带的 `deploy.sh`（服务器上拷一份到部署目录根即可，它会转发到
`deploy/update.sh`，逻辑只有一份，不会随更新分叉）：

```bash
./deploy.sh                        # 沿用当前分支并拉取 origin 对应分支最新提交
./deploy.sh <branch>               # 显式切换并更新到 origin 上存在的指定分支
DEPLOY_NO_BACKUP=1 ./deploy.sh     # 跳过代码打包备份
DEPLOY_NO_DB_BACKUP=1 ./deploy.sh  # 跳过迁移前数据库转储（慎用）
```

它会依次做完：打包备份（保留最近 5 份）→ `git fetch` + `reset --hard` → 等待 `db` 就绪并备份数据库 → **重建 `migrate`
镜像** → 跑迁移 → 重建 api/web → 轮询 `/api/health` 直到 200；失败时直接把 api 最近 60
行日志打到 stderr。

⚠️ 不要图省事只跑 `docker compose up -d --build`。`migrate` 服务使用独立的精简二进制阶段，
每次发版先执行 `docker compose build migrate && docker compose run --rm migrate` 可确保迁移器代码与最新迁移文件同步生效，
避免旧镜像静默跳过新增迁移或逻辑变更。这也是 `deploy.sh` 统一封装发布顺序的原因。

健康检查端口从 `.env` 的 `API_PORT` 读（默认 8080）。曾经在脚本里写死过旧测试端口，
于是每次部署都以「健康检查超时」收场，而服务其实早就起来了——别再写死它。

### 方式三：宿主机直装（systemd）

不想用 Docker 时，把 Go 二进制和前端静态文件直接放宿主机：

```bash
# 1) 构建（同时编译 api 与 migrate，目标机无需安装 Go 环境）
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/api ./server/cmd/api
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/migrate ./server/cmd/migrate
npm ci --no-audit --no-fund && npm run build

# 2) 安装
sudo useradd --system --home /opt/hy2-ops-console --shell /usr/sbin/nologin nodeops
sudo mkdir -p /opt/hy2-ops-console/bin /etc/hy2-ops-console /srv/hy2-ops-console
sudo cp bin/api bin/migrate /opt/hy2-ops-console/bin/
sudo cp -r dist/* /srv/hy2-ops-console/
sudo cp -r server/migrations /opt/hy2-ops-console/    # 迁移器需要
sudo chown -R nodeops:nodeops /opt/hy2-ops-console

# 3) 配置与 unit
sudo install -m 600 -o nodeops -g nodeops deploy/api.env.example /etc/hy2-ops-console/api.env
sudo editor /etc/hy2-ops-console/api.env              # 填真实值
sudo install -m 644 deploy/hy2-ops-console-api.service /etc/systemd/system/

# 4) 迁移 + 启动
sudo -u nodeops env DATABASE_URL='...' MIGRATIONS_DIR=/opt/hy2-ops-console/migrations \
  /opt/hy2-ops-console/bin/migrate
sudo systemctl daemon-reload && sudo systemctl enable --now hy2-ops-console-api
journalctl -u hy2-ops-console-api -f
```

前端与反代：把 `deploy/Caddyfile.host` 并进宿主机 Caddy 配置，改掉域名与静态目录路径即可（它同时托管 `dist/` 和反代 `/api`）。

### 数据库迁移

迁移由 `server/cmd/migrate` 负责，逻辑是**幂等**的：

- 建立 `schema_migrations` 台账表，记录已应用的迁移文件。
- 按文件名数字前缀排序，跳过已记录的，只执行新增的。
- 每个迁移与它的台账记录在**同一事务**里提交，失败不留半截记录。
- 识别「老库」：如果库里已有业务表（`users`）但台账为空，说明是本 Runner 出现之前建的库，此时把现有文件登记为 baseline 而**不是**重放（重放会在第一个 `CREATE TABLE` 就报错）。
- 迁移连接**使用 simple protocol**：迁移文件是多语句 SQL（`001_init.sql` 一个文件建十几张表），而 pgx 默认的扩展协议一次只接受一条语句，会把整份文件当成语法错误拒绝。

`docker compose up` 里的 `migrate` 服务每次都会跑，因此升级只需 `docker compose up -d --build`。

手工补跑某个文件（不推荐，仅排障）：

```bash
psql "$DATABASE_URL" -f server/migrations/008_xxx.sql
```

> 注意：`docker-entrypoint-initdb.d` 只在**数据卷为空**时执行一次。靠它加迁移在已有部署上是无效的——这正是引入 Runner 的原因。

## 本地开发

```bash
npm install
npm run dev            # Vite，默认把 /api 代理到 127.0.0.1:8080
go run ./server/cmd/api
```

如果本地 Go 进程不在 8080，用环境变量覆盖代理目标：

```bash
VITE_API_PROXY=http://127.0.0.1:18080 npm run dev
```

## API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/health` | 含数据库连通性 |
| POST | `/api/login` | bcrypt 校验，签发 HS256 JWT |
| GET/POST | `/api/servers` | 主机清单 / 登记主机 |
| PATCH/DELETE | `/api/servers/{id}` | 改主机名、地址、SSH 端口与用户（改端点会重置主机密钥指纹）/ 软删除 |
| POST | `/api/servers/{id}/credential` | 上传 SSH 私钥或密码，加密后入库 |
| GET | `/api/servers/{id}/credential` | 仅返回元数据，永不回显私钥 |
| DELETE | `/api/servers/{id}/credential` | 删除凭据（需 `X-Confirm-Dangerous: yes`） |
| POST | `/api/servers/{id}/write-ops` | 开关写操作并设置 unit 白名单 |
| GET | `/api/servers/{id}/services` | **只读**读取该主机白名单内各 unit 的实时 systemd 状态（active / enabled / 状态变更时间）。同步返回，不经任务队列 |
| GET | `/api/servers/{id}/metrics` | 24h 指标时序（单主机，原始采样点） |
| GET | `/api/metrics/recent` | 全部主机的 24h 指标时序，按 10 分钟降采样，按 server_id 分组。总览页用它一次拿全量，避免每台主机各发一个请求 |
| GET/POST | `/api/nodes` | HY2 节点 / 登记节点（可带 `api_url` + `api_key` 一步绑定） |
| PATCH/DELETE | `/api/nodes/{id}` | 编辑节点（名称、所属主机、端口、版本、域名）/ 注销登记（软删除，同时忘掉 portal key） |
| POST | `/api/nodes/preview` | 只读探测节点侧 portal 的 `/api/v1/node/meta`，返回端口/域名/公网 IP/跳段/用户数，不落库 |
| POST/GET/DELETE | `/api/nodes/{id}/api` | 绑定 / 重读 / 解绑节点侧 portal API（解绑需确认头） |
| GET | `/api/nodes/{id}/overview` | **只读**节点概览：聚合 `node/meta`、`users/list`、`proxy-services/list`，并在服务端算出用户数/有效数/用量与配额合计。不返回 `auth_password` |
| GET/POST | `/api/nodes/{id}/users` | 读取 / 新建节点用户（新建返回口令与订阅链接，只回一次） |
| POST | `/api/nodes/{id}/users/{uid}/renew` | 续期（延长天数 / 追加流量） |
| DELETE | `/api/nodes/{id}/users/{uid}` | 删除用户（需 `X-Confirm-Dangerous: yes`） |
| GET/POST | `/api/nodes/{id}/proxy-services` | 读取 / 新建 gost 代理服务（`socks5` / `http` / `https`） |
| DELETE | `/api/nodes/{id}/proxy-services/{pid}` | 删除代理服务（需 `X-Confirm-Dangerous: yes`） |
| GET | `/api/subscription` | 合并订阅的当前链接、包含/排除的节点清单（首次调用即生成令牌） |
| GET | `/api/subscription/preview?format=clash\|singbox\|uri` | 认证态下预览订阅内容，不必把链接交给客户端 |
| POST | `/api/subscription/rotate` | 换新订阅链接，旧链接立即失效（管理员，需确认头） |
| PATCH | `/api/subscription` | 启用 / 停用订阅、改标签 |
| GET | `/sub/{token}` | **公开**订阅 feed。客户端导入的就是这个地址，`/api/sub/` 是其等价别名 |
| GET/POST | `/api/tasks` | 任务 / 下发任务。`operation_type` 仅接受执行器真正实现的操作（见下表），其余返回 422 |
| GET | `/api/tasks/{id}/logs` | 任务逐行日志 |
| POST | `/api/tasks/{id}/cancel` | 取消排队中的任务 |
| POST | `/api/tasks/{id}/retry` | 按原参数重新入队（危险操作需确认头） |
| GET | `/api/alerts` | 只读返回告警；`?include=resolved` 带已解决归档。告警的产生与自动解决由服务端定时同步完成，不在本接口内写库 |
| POST | `/api/alerts/{id}/ack` | 逐条确认告警 |
| GET/POST | `/api/users` | 用户清单 / 新建账号（管理员） |
| PATCH/DELETE | `/api/users/{id}` | 调整角色 / 删除账号（管理员，不能对自己操作） |
| POST | `/api/users/{id}/password` | 重置他人密码（管理员） |
| GET | `/api/audit`、`/api/runtime` | 审计日志、运行时状态 |

除 `health`、`login` 与 `/sub/{token}` 外都需要 `Authorization: Bearer`。
`/sub/{token}` 是唯一免登录的路由——订阅客户端不可能带 JWT，所以路径里那把随机令牌就是凭据。

## 服务控制

面板对 VPS 上 systemd 服务的控制能力，全部经由任务队列（可审计、可限流、可重试）。

### 支持的操作

| `operation_type` | 说明 | 需要写操作 |
| --- | --- | --- |
| `health_check` | SSH 握手 + 负载与 unit 状态 | 否 |
| `logs` | `journalctl -u <unit>` 最近 50 行 | 否 |
| `restart` | `systemctl restart` | 是 |
| `start` | `systemctl start` | 是 |
| `stop` | `systemctl stop` | 是 |
| `reload` | `systemctl reload`（重载配置，不中断连接） | 是 |
| `enable` / `disable` | 开机自启开关 | 是 |

三个闸门同时生效，缺一不可：`allow_write_ops`、unit 白名单、`X-Confirm-Dangerous: yes`。

### 为什么 API 会拒绝某些操作

`operation_tasks` 表的 CHECK 约束里还留着 `deploy` / `config_apply` / `upgrade` 三个值，但**执行器没有实现它们**。
早先版本会照单接收、入队，然后在执行时失败——API 报了成功，实际永远做不成。
现在 `POST /api/tasks` 用一张显式的支持矩阵挡住它们，返回 `422`，前端也不再提供对应按钮。

保留 CHECK 约束而不收窄，是为了让历史任务行和将来真做实现时都不需要迁移。

### 状态读取为什么不走队列

`GET /api/servers/{id}/services` 是**同步**的：SSH 连上去跑一次 `systemctl show`，直接返回。
走队列的话，用户点开抽屉得等一个采集周期（默认 20 秒）才能看到状态，而这只是个只读查询。
代价是它占用一个 HTTP 连接约 1~2 秒，所以前端只在打开详情和手动点「刷新状态」时调用，不做定时轮询。

unit 列表复用写操作白名单——那份清单本身就是「这台主机允许面板谈论哪些服务」的声明，不必再引入第二份会漂移的注册表。

## 节点控制

面板对 Hysteria2 节点的控制，走的是节点上 `hysteria2-installer` 自带 portal 的 REST 接口，
用的就是「绑定节点 API」时存下的那把 Bearer `api_key`。**不需要任何额外凭据。**

### 能做什么

**节点的 Bearer 密钥只能到达 portal 的三条读接口。** `portal.py` 的 `do_GET` 在 `/api/v1/` 下**只**服务：

| 接口 | 返回 |
| --- | --- |
| `node/meta` | 该节点的 `client_meta.json`（端口、域名、公网 IP、证书类型、跳段、订阅端口、混淆开关） |
| `users/list` | 动态用户列表（**故意不含 password**） |
| `proxy-services/list` | gost 代理服务列表 |

其余一律 404 `API endpoint not found`。所以面板节点控制的范围就是下表这些：

| 能力 | 对应 portal 接口 | 说明 |
| --- | --- | --- |
| 节点概览 | `node/meta`、`users/list`、`proxy-services/list` | 在服务端汇总用户数/有效数/用量与配额合计。**`auth_password` 与 `obfs_password` 不下发浏览器**，只给布尔值表示「已启用混淆」 |
| 用户管理 | `users/list`、`users/create`、`users/renew`、`users/delete` | 新建时返回口令与 hysteria2 / Reality / Clash / sing-box 四种订阅产物 |
| 代理服务 | `proxy-services/list`、`add`、`delete` | gost 的 socks5 / http / https，凭据由节点生成并透传 |

这里的「Reality 订阅产物」指的是 `users/create` **返回值里**那一项（portal 自己拼的），
**不是**面板能读节点的 Reality 配置——后者在 `reality-status`，走网页会话鉴权，见下。

这些接口都是**同步**的：portal 是节点上的本地 HTTP 服务，往返在亚秒级，而且新建用户要立刻拿到口令与订阅链接
交给客户——丢进任务队列只会让操作员去轮询一个「只该看一次」的值。所有变更操作都写审计日志，
但**口令与完整订阅 URI 不写审计**：审计表管理员可读，而一条订阅 URI 就是一份可直接使用的凭据。

### 不做什么，以及为什么

节点侧还有一批动作——重启机器、升级 core/portal、切换 BBR、重置 Reality 密钥、安装 gost/xray/warp、
改 WARP 分流规则，以及**读取**版本 / Reality / BBR / WARP 状态——它们在 portal 上是**网页会话鉴权**
（`/{token}/` 前缀 + 登录态/Basic），**不是** Bearer 鉴权。面板只持有 `api_key`，够不到这些端点。

面板**不提供这些入口**。要控制或查看这些，请直接登录节点上的 portal 页面。
这和「堵谎报」是同一条原则：宁可不做，也不放一个必然失败的按钮。

> 早期版本曾把这些动作挂在节点详情里，但执行时必然失败——API 报成功、实际做不成。
> 更早的一版概览还曾去探 `check-version` / `reality-status` / `bbr-status` / `warp-status`，
> 它们全部 404，界面上只是一排「不可用」空卡。两处都已移除，而不是留着好看。

### 为什么不在浏览器里直连节点

节点 API 的地址是**用户可填**的，所以它按不可信输入处理：面板服务端用一个拒连非公网地址的 dialer 去请求它，
避免 API 服务器被拿来当跳板访问内网或云元数据端点，也让 `api_key` 只在服务端解密、不出浏览器。

## 合并订阅

一条链接带走全部节点：客户端导入一次，就拿到整支舰队。入口在「HY2 节点」页右上角的**合并订阅**。

### 凭据模型（决定了它的安全语义）

每个节点贡献它自己的**主口令**（portal 的 `auth_password`），由面板实时从该节点的 `/api/v1/node/meta` 读取。
选它的理由是最小改动：不新增任何存储、不向节点写入任何东西、每个已绑定 portal API 的节点立刻可用。

两个后果是刻意的、并且写进了界面而不是藏起来：

1. **链接即主口令**。谁拿到这条链接，谁就拿到全部节点的通行证。所以它是随机生成（32 字节）、
   一键可换新（换新后旧链接立即失效）、可整体停用，并且按令牌限流。
2. **节点侧记账会合并**。导入同一条链接的客户端，在每台节点上都认证成同一个账号（portal 的 base user），
   因此该节点上「按用户」的流量统计无法区分它们。个人自用无影响；要按客户独立记账，请用 portal 上给每个客户单独开户。

### 为什么有些节点不进来

只有**绑定了节点侧 API** 的节点能被纳入——口令只能来自那里的 `node/meta`。
`users/list` 故意不返回密码，所以「拿已有客户的密码去拼订阅」这条路是不通的。
被排除的节点会连同原因列在面板上（例如「未绑定节点侧 API，拿不到该节点的口令」），不会静默消失。

### 三种格式与自动协商

| 格式 | `?format=` | 说明 |
| --- | --- | --- |
| Clash / mihomo | `clash` | 含 `PROXY`（select，手动切）与 `AUTO`（url-test，自动选最快）两个组，`rules: MATCH,PROXY` |
| sing-box | `singbox` | `outbounds`：每节点一个 hysteria2 + `selector` + `urltest` + `direct` |
| 通用 URI 列表 | `uri` | base64 换行拼接的 `hysteria2://` 列表，v2rayN / v2rayNG 等认这个 |

不带 `?format=` 时按 **User-Agent** 判断（clash / mihomo / stash / surfboard → Clash；
sing-box / SFA / SFI / NekoBox → sing-box；v2rayNG / hysteria / Shadowrocket → URI 列表），
这与所有订阅服务的做法一致，因为客户端没法用 Accept 头表达格式偏好。未知 UA 默认给 Clash。

输出是 JSON——它同时是合法 YAML，portal 自己的 `clash.yaml` 也是这么做的；写真正的 YAML
要多一个编码器和一套引号规则，而 Clash 系客户端本来就能解析这个。

### 两个不太显眼但会咬人的细节

- **口令里的空格必须编码成 `%20`，不能是 `+`**。URI 的 userinfo 段里 `+` 是字面加号，
  所以代码用自己实现的百分号编码而不是 `url.QueryEscape`——后者会把空格变成 `+`，客户端于是用错口令、连不上却没有任何报错。
- **代理名必须唯一**。Clash / sing-box 以名字为主键，重名会让客户端**静默丢掉一个节点**。
  同名时按端口加后缀，再冲突就继续加，且过程幂等（重新导入不会让顺序漂移）。

### 缓存与限流

解析一次要给每台节点发一个 portal 请求，所以结果带 60 秒缓存，缓存键是「各节点 id + `updated_at`」——
增删节点或重绑 API 会立即失效，只有节点侧改了口令才需要等 TTL。并发探测限制为 6 路，避免一次打开几十个连接。
订阅 feed 按令牌限流；拉取只更新 `last_accessed_at` / `access_count`，**不写审计日志**——客户端是定时轮询的，
每次拉取都写一行只会把审计表淹掉。

## 安全模型

- **凭据加密存储**：私钥用 `CREDENTIAL_KEY` 做 AES-256-GCM 后存 `server_credentials.ciphertext`，接口只回 `auth_type` / `key_version` / 时间。
- **主机密钥**：首次连接 TOFU 锁定指纹并写入 `servers.host_fingerprint`，之后指纹不符即拒绝连接。
- **写操作双重闸门**：`allow_write_ops` 按主机开启，且必须同时给出 unit 白名单；执行器再校验目标 unit 是否在白名单内，不在则拒绝且不执行任何命令。HTTP 侧还要求 `X-Confirm-Dangerous: yes`。整套 systemd 操作族（start/stop/restart/reload/enable/disable）共用同一个实现与同一套闸门，避免新增动词时漏掉某道校验。
- **只读操作也受白名单约束**：`GET /api/servers/{id}/services` 与 `logs` 虽不改变主机状态，但仍只允许访问白名单内的 unit——白名单定义的是这台主机愿意谈论的范围，不只是允许修改的范围。
- **限流与审计**：登录按来源 IP 限流，写接口按用户限流；成功写入进 `audit_logs`，登录失败（`login_failed`）与越权被拒（`forbidden_admin_only`）同样留痕，以便发现暴力破解与提权尝试。
- **SSH 认证**：支持私钥与密码两种方式，凭据统一 AES-256-GCM 加密入库；带口令的私钥直接拒绝，需先转成无口令密钥或改用密码方式。详情抽屉可轮换或删除凭据。
- **节点 API 密钥同级对待**：portal 的 `api_key` 能读出节点的 obfs/auth 口令，因此与 SSH 私钥同样做 AES-256-GCM 后存 `hysteria_nodes.api_key`，接口只回 `has_api` 与 URL，永不回显。
- **节点地址按不可信输入处理**：`api_url` 由用户填写，所以面板服务端用一个拒连非公网地址（私网 / 链路本地 / 环回）的 dialer 去请求，且拒绝重定向、限制响应体大小。这样即使有人填了内网地址或云元数据端点，API 服务器也不会被当成跳板。
- **节点侧只有 REST 才做**：节点 portal 的 `/<token>/` 网页端点走会话鉴权，Bearer 密钥够不到，面板因此不提供入口（见「节点控制」一节）。宁可功能少，也不放必然失败的按钮。
- **一次性凭据不进审计**：新建节点用户返回的口令与订阅 URI 只在响应里出现，不落库、不写审计——审计表管理员可读，而一条订阅 URI 就是可直接使用的凭据。
- **概览接口不返回主口令**：`/api/nodes/{id}/overview` 与 `/api/subscription` 都会把 portal 主口令脱敏掉（只给 `has_obfs` / `has_pin` 这类布尔），并有测试锁死——连字段名都不许出现，否则一个 `null` 字段就等于宣告它的存在。
- **订阅链接是可撤销的 bearer 能力**：`/sub/{token}` 是唯一免登录路由。令牌 32 字节随机、换新即失效、可整体停用、按令牌限流；未知与已停用的令牌返回**同样**的 404，不给出可用来试探的差异。

## 踩过的坑（重要）

不要用「裸 TCP 连 22 端口再断开」做存活探测。这类连接在 sshd 日志里是
`Connection closed by <ip>`，会被 fail2ban 计入失败次数；默认 sshd jail 常见配置是
`maxretry=3`，于是采集器会把自己的出口 IP 封掉，表现为监控静默中断、任务报
`connection refused`。本项目的做法是每个周期只建立一条完整认证的 SSH 连接，
节点探测与用量读取复用同一条连接，延迟取该次握手耗时（含密钥交换与认证，
因此大于纯网络 RTT）。

同理，`df -B1P` 会被 GNU df 解析成「块大小 1P」，要写 `df -P -B1`；Debian 默认
`mawk` 的 `printf "%d"` 会把大整数截到 `INT32_MAX`，采集脚本因此只输出原始字段文本，
换算交给 Go 侧完成。

## 接入 hysteria 的上/下行速率

在目标机的 `config.yaml` 末尾加一段（`listen` 只绑回环，密钥放独立文件并设 0600），然后重启服务：

```yaml
trafficStats:
  listen: 127.0.0.1:29862
  secret: <随机 48 位 hex>
```

再到面板「VPS 主机 → 编辑」里填 `http://127.0.0.1:29862` 与密钥文件路径（如 `/etc/hysteria/traffic_stats_secret`）。
接口地址只接受回环 http，密钥路径只接受普通绝对路径——两者都会被拼进目标机上执行的命令，
所以后端用正则白名单卡死，前端也先校验；密钥本身不进数据库。

## 地区 / VPS 商家自动识别

新增 hy2 节点、或登记 3X-UI 面板时，控制台会按它的**公网 IP** 自动判断
「在哪个地区、是哪家 VPS 商家」，并据此命名（如 `日本-东京`，商家信息单独写入 `provider` 字段），
免去每加一台机器都要自己回忆「这台是法兰克福的 Hetzner 还是赫尔辛基的」。

三条行为约定（实现见 `server/internal/httpapi/geoapply.go` 与 `server/internal/geo/geo.go`）：

1. **命名格式是 `国家-城市`**；只识别出一边时就只用那一边，不会产出
   `日本-` 或 `-东京` 这种带孤儿连字符的名字。
2. **已经手工填过的字段永远不覆盖。** 你打的 `HK-主力` 不会被识别结果冲掉。
   靠数据库里的 `name_source` / `region_source` / `provider_source`
   三个标记区分「自动填的（auto，可被更新）」与「人工填的（manual，永不覆盖）」。
3. **识别不出来时什么都不改**，让你手填。识别是加分项，不是前置条件 ——
   没配离线库、没网、IP 是内网，都不影响加节点/加面板本身。

### 识别数据从哪来：离线库优先，公网 API 兜底

| | 离线库（推荐） | 公网 API（兜底） |
|---|---|---|
| 配置 | `GEO_OFFLINE_DB` + 生成一个库文件 | 默认就开（`GEO_ENABLE_API=true`） |
| 优点 | 不联网、不泄漏机器 IP、无限速 | 零配置、总有数据 |
| 缺点 | 需要自己生成并更新 | 有速率限制、把机器 IP 发给第三方 |

生成离线库（在**能上网**的机器上跑，产物传到服务器）：

```bash
# 从 IP 清单生成（每行一个 IP，支持 # 注释）
python3 tools/gen_offline_db.py --ips ips.txt --out geo-offline.json

# 把产物放到宿主 ./geo-data/ 下，compose 已把该目录挂到容器 /data
mkdir -p geo-data && cp geo-offline.json geo-data/
docker compose restart api
```

工具默认把每个 IP **放宽成它所在的 /24 网段**——这样收录一台机器，
同网段新买的机器也能认出来。想要精确到单 IP 就加 `--exact-prefix`。

> ⚠️ `ip-api.com` 的**免费版仅允许非商业用途**。商业运营请购买其付费端点
> （用 `GEO_API_BASE_URL` 指过去），或改用自备数据源生成离线库。

### 什么时候识别不了（不是 bug）

- **3X-UI 面板地址填的是域名**（如 `panel.example.com`）——不查。
  反解域名得到的是 CDN 边缘节点、不是这台机器真实所在，那比不识别更误导。
  要自动命名就用 `IP:端口` 形式的地址（面板通常正是这么访问的）。
- **IP 是内网地址**——不查。既没意义，也不该把内网拓扑发给第三方。
- **节点没上报公网 IP**——退回用节点自报的 `server_name` 命名。

## 已知边界

- 上/下行流量来自 hysteria 自带的 `trafficStats` 接口：在主机表单填「速率采集接口 + 密钥文件路径」后，采集器每周期在目标机本地读 `/traffic?clear=true`（取走即清零，天然是速率），按分钟入库。未开启的主机退回 portal 记账值并在界面标注口径。
- 主机列表里两个用量字段口径不同、刻意不合并：`measured_bytes_24h` 是 hysteria 速率在 24h 内的求和（实测用量），`portal_bytes_24h` 是 portal 的配额记账变化量。早先版本用 `GREATEST()` 把两者压成一个 `used_bytes_24h`，会让两种口径互相冒充，现已拆开。
- `traffic_samples` 按分钟分桶，而采集间隔（默认 30s）短于一分钟，因此同一分钟内的多次采样是**累加**写入的——这正是让该分钟的字节数完整所必需的。
- portal 的 `used_bytes` 只在客户端重新认证时前进，因此「累计用量」可能长时间不变；实时用量看 24h 那一列。
- 采集器与执行器都是每周期并发处理各主机/任务（每批一个 goroutine），但每个周期整体是同步等待的，不会重叠到下一个周期。执行器单实例、无分布式抢占；任务认领使用 `FOR UPDATE SKIP LOCKED`。
- 告警状态同步（`ReconcileAlerts`）由独立定时器驱动，与 `ENABLE_COLLECTOR` 无关；`GET /api/alerts` 是纯只读的。
- 修改主机的地址 / SSH 端口 / SSH 用户会清空已锁定的 `host_fingerprint`，下一次连接按 TOFU 重新锁定；修改节点端口或所属主机会把节点状态重置为未知，等下一轮真实探测。
- 服务重启依赖目标机具备 `sudo`（或以 root 连接时直接执行）。
- 节点侧控制只覆盖 portal 的 REST（`/api/v1/*`，Bearer 鉴权）：用户管理与代理服务可读写，其余状态只读。节点级高危动作在 portal 上是网页会话鉴权，面板够不到，因此不提供入口（见「节点控制」）。
- 节点用户的 `traffic_used_bytes` / `traffic_limit_bytes` 是 portal 的配额记账，只在客户端重新认证时前进，**不是**实时速率；界面已按此标注，不要当速率读。
- 合并订阅只纳入绑定了节点侧 API 的节点，且用各自的主口令；被排除的节点会列出原因。订阅内容有 60 秒缓存，节点侧改了主口令后最多 60 秒生效。
- 合并订阅不提供 `Subscription-Userinfo` 头（流量/到期信息）。节点侧那是 portal 的按用户记账，与「一条链接多节点」的聚合口径对不上，硬凑一个数字等于编造。
- `/sub/*` 需要反代到 API，不能落到 SPA 兜底。仓库内的 `Caddyfile` 与 `deploy/Caddyfile.host` 都已加好这条规则；若你自己写反代，记得加。

## 部署相关边界

- **迁移不与 API 启动耦合**：API 进程不自己跑迁移，迁移由独立的 `migrate` 服务/命令完成。这样做的好处是 API 可以多副本横向扩展而不会并发跑迁移，代价是升级时必须先跑迁移（compose 已用 `service_completed_successfully` 约束顺序）。
- **`web` 服务默认把 80/443 绑定到 `127.0.0.1`**：避免未配置证书时直接暴露明文管理面，也便于接入宿主机已有外层反代（如外层 Caddy 占用 80/443 时可顺手调高 `WEB_PORT`）。如需单机直接对外开放 80/443，请在 `.env` 中显式设置 `WEB_BIND=0.0.0.0`。
- **`SITE_ADDRESS` 留空时不启 HTTPS**：这是刻意的——本地和内网部署通常没有公网可达的 80/443，强行申领证书只会失败刷日志。要 HTTPS 就填域名，并确保 ACME 校验可达。
- **Caddy 会自动处理 HTTP→HTTPS 跳转与证书续期**：配了域名后不要再手工加跳转规则，否则容易和 Caddy 内建行为打架。
- **API 默认只绑回环**：前端经 `web` 同源转发，正常部署无需把 API 暴露到公网。只有在前后端分离部署时才需要调整。

## 开源许可证

本项目基于 **GNU Affero General Public License v3.0 (AGPL-3.0)** 协议发布，完整协议正文见 [LICENSE](LICENSE)。
