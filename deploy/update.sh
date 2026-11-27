#!/usr/bin/env bash
# 幂等部署：备份 -> 拉最新代码 -> 重建 migrate -> 跑迁移 -> 重建 api/web -> 健康检查
#
# 用法:
#   ./deploy.sh                        # 更新到 origin 上的当前分支
#   ./deploy.sh <branch>               # 更新到 origin 上的指定分支
#   DEPLOY_NO_BACKUP=1 ./deploy.sh     # 跳过打包备份（日常小改动可用）
#
# 位置自适应：仓库里是 deploy/update.sh，拷到服务器根目录当 deploy.sh 也一样能跑。
#
# 顺序不能换：
#   1. git fetch + reset --hard        —— 只改磁盘，不影响在跑的容器
#   2. docker compose build migrate    —— 迁移器与迁移文件同步重建
#   3. docker compose run --rm migrate
#   4. docker compose up -d --build api
#      docker compose up -d --build web
#
# 本文件受 .gitattributes 保护为 LF；若在 Windows 上编辑过，先确认没有 CR，
# 否则 Linux 上会报 bash: $'\r': command not found。
set -euo pipefail
# 2026-11-04：备份（含 .env 的代码包、整库 pg_dump）一律只给 root 读。原来默认 0644 写进 /opt，
# 同机任何用户或容器都能读到 CREDENTIAL_KEY / JWT_SECRET 和加密的节点凭据。
umask 077

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ "$(basename "$SCRIPT_DIR")" = "deploy" ]; then
  ROOT="$(dirname "$SCRIPT_DIR")"
else
  ROOT="$SCRIPT_DIR"
fi
cd "$ROOT"

STAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="/opt"

# REF 在下面「拉代码」一节里才真正确定：默认沿用当前分支，而不是写死 main。
# 原因见该节注释（生产分支与 main 分叉，写死 main 会丢掉生产上的提交）。

log()  { echo "==> $*"; }
warn() { echo "!! $*" >&2; }

DUMP_ERR=""
DUMP_PARTIAL=""
cleanup_tmp() {
  [ -n "$DUMP_ERR" ] && rm -f "$DUMP_ERR" 2>/dev/null || true
  [ -n "$DUMP_PARTIAL" ] && rm -f "$DUMP_PARTIAL" 2>/dev/null || true
}
trap cleanup_tmp EXIT

# 清理旧备份（保留最新 keep 份；不依赖 xargs -r，且在 pipefail 下零匹配安全）
prune_backups() {
  local keep="$1" dir="$2" prefix="$3" suffix="$4"
  local -a files=()
  local old_nullglob=0
  shopt -q nullglob && old_nullglob=1
  shopt -s nullglob
  files=("$dir/$prefix"*"$suffix")
  if [ "$old_nullglob" -eq 0 ]; then shopt -u nullglob; fi
  [ "${#files[@]}" -gt "$keep" ] || return 0
  local f idx=0
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    idx=$((idx + 1))
    if [ "$idx" -gt "$keep" ]; then
      rm -f "$f" 2>/dev/null || true
    fi
  done < <(ls -1t -- "${files[@]}" 2>/dev/null || true)
}

# ------------------------------------------------------------------ 预检
[ -d .git ] || { warn "当前目录不是 git 仓库：$ROOT"; exit 1; }
git rev-parse --git-dir >/dev/null 2>&1 || { warn "git 仓库不可用"; exit 1; }
command -v docker >/dev/null 2>&1 || { warn "找不到 docker"; exit 1; }
docker compose version >/dev/null 2>&1 || { warn "docker compose 插件不可用"; exit 1; }
[ -f .env ] || warn ".env 不存在 —— compose 会因缺少必填变量直接失败（.env 不在 git 里，换机器要手工搬）"

# 护栏：/api 不允许被 bind mount 覆盖。
#
# 2026-10-30 实测：生产 compose 里有一行
#     - ./releases/<某次构建>/api:/api:ro
# 于是 `docker compose build --no-cache` 建出来的新二进制**根本没被用上** ——
# 容器跑的是挂进去的那个文件。构建成功、镜像时间新、容器 healthy，
# 而代码是旧的，比构建缓存更隐蔽。
# 发现挂载就直接停：让操作者先删掉那行，而不是静默部署一个旧版本。
if grep -Eq '^[[:space:]]*-[[:space:]]*[^#]*:/api(:ro|:rw)?[[:space:]]*$' docker-compose*.yml 2>/dev/null; then
  warn "docker-compose 里把 /api 目录/文件挂载进了容器：镜像里的二进制会被覆盖，部署等于没生效"
  warn "请删除该挂载（二进制只应来自 Dockerfile.api），再重新运行本脚本"
  grep -En '^[[:space:]]*-[[:space:]]*[^#]*:/api(:ro|:rw)?[[:space:]]*$' docker-compose*.yml >&2 || true
  exit 1
fi

# 从 .env 读取配置项：兼容 export 前缀、前后空白、单双引号（含闭合后行内注释）与无引号行内注释，并去除 CR 回车符。
read_env() {
  local raw
  raw="$(sed -n "s/^[[:space:]]*\(export[[:space:]]\+\)\?$1=//p" .env 2>/dev/null | tail -1 | tr -d '\r' || true)"
  raw="$(printf '%s' "$raw" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  case "$raw" in
    \"*\"*)
      raw="${raw#\"}"
      raw="${raw%%\"*}"
      ;;
    \'*\'*)
      raw="${raw#\'}"
      raw="${raw%%\'*}"
      ;;
    *)
      # 剥离未加引号时的行内注释（空白后的 # 开头部分）
      raw="${raw%%[[:space:]]#*}"
      raw="$(printf '%s' "$raw" | sed -e 's/[[:space:]]*$//')"
      ;;
  esac
  printf '%s' "$raw"
}

# 将监听地址规范化为健康检查可直连的主机地址（兼容通配地址与 IPv6 字面量）。
resolve_health_host() {
  local bind="$1"
  case "$bind" in
    ""|"0.0.0.0"|"::"|"[::]"|"*")
      printf '127.0.0.1'
      ;;
    \[*\])
      printf '%s' "$bind"
      ;;
    *:*)
      printf '[%s]' "$bind"
      ;;
    *)
      printf '%s' "$bind"
      ;;
  esac
}

PROJECT="$(read_env COMPOSE_PROJECT_NAME)"; PROJECT="${PROJECT:-$(basename "$ROOT")}"
API_PORT="$(read_env API_PORT)"; API_PORT="${API_PORT:-8080}"
API_BIND="$(read_env API_BIND)"; API_BIND="${API_BIND:-127.0.0.1}"
HEALTH_HOST="$(resolve_health_host "$API_BIND")"

BEFORE="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
log "当前版本: $BEFORE"

# ------------------------------------------------------------------ 备份
# reset --hard 会丢弃工作区里对已跟踪文件的改动，先把整目录打包留底。
# 只保留最近 5 份，免得 /opt 被备份塞满。
if [ "${DEPLOY_NO_BACKUP:-0}" = "1" ]; then
  log "跳过备份（DEPLOY_NO_BACKUP=1）"
else
  BK="$BACKUP_DIR/$PROJECT-backup-$STAMP.tar.gz"
  log "备份到 $BK"
  tar --exclude='./.git' --exclude='./node_modules' --exclude='./dist' -czf "$BK" .
  prune_backups 5 "$BACKUP_DIR" "${PROJECT}-backup-" ".tar.gz"
  log "保留备份数: $(ls -1 "$BACKUP_DIR/$PROJECT-backup-"*.tar.gz 2>/dev/null | wc -l | tr -d ' ')"
fi

# 有本地改动就摊开说清楚，别让人以为是自己的改动凭空消失了
DIRTY="$(git status --porcelain --untracked-files=no || true)"
if [ -n "$DIRTY" ]; then
  warn "工作区对已跟踪文件有改动，reset --hard 会丢弃它们（已打包进上面的备份）："
  echo "$DIRTY" | sed 's/^/    /' >&2
fi

# ------------------------------------------------------------------ 拉代码
# ⚠️ 2026-10-24 审计：REF 的默认值必须跟着**当前部署的分支**走，不能写死 main。
#
# 为什么必须沿用当前分支：
#   若部署环境运行在独立发布分支且与 main 存在分叉，写死 REF="${1:-main}"
#   会在执行日常更新时强行切回 main 并丢失发布分支上的提交。
#
# 现在的规则：
#   1. 不传参数时，沿用**当前工作区所在分支**（生产上就是那个真实在跑的分支）；
#   2. 传入的 ref 必须存在于 origin，否则直接中止 —— 不允许"悄悄"退化成 main；
#   3. 切换前先确认目标 ref 在本地是否存在，避免 checkout 失败后 reset 到错误的对象。
if [ -n "${1:-}" ]; then
  REF="$1"
  case "$REF" in
    -*|*..*|*//*)
      warn "非法分支参数 '$REF'（禁止以连字符开头或包含 .. / //）"
      exit 1
      ;;
  esac
else
  CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo '')"
  if [ -z "$CURRENT_BRANCH" ] || [ "$CURRENT_BRANCH" = "HEAD" ]; then
    warn "当前处于游离 HEAD，无法推断部署分支；请显式指定 ref，例如 ./deploy.sh <branch>"
    exit 1
  fi
  REF="$CURRENT_BRANCH"
  log "未指定 ref，沿用当前分支：$REF"
fi

log "拉取 origin/$REF"
git fetch --prune origin
if ! git rev-parse --verify --quiet "refs/remotes/origin/$REF^{commit}" >/dev/null; then
  warn "origin 上不存在分支 '$REF'，中止部署"
  exit 1
fi
if git show-ref --verify --quiet "refs/heads/$REF"; then
  git checkout -q "$REF"
else
  git checkout -q -b "$REF" --track "origin/$REF"
fi
# 护栏：本地比 origin/$REF 多出提交时，reset --hard 会丢掉它们。
# rev-list 失败（取不到数字）一律视为有风险；DEPLOY_ALLOW_DISCARD=1 才放行。
AHEAD="$(git rev-list --count "origin/$REF..HEAD" 2>/dev/null)" || AHEAD=""
case "$AHEAD" in
  ""|*[!0-9]*) AHEAD_RISK="无法判断(rev-list 失败)" ;;
  0) AHEAD_RISK="" ;;
  *) AHEAD_RISK="本地比 origin/$REF 多 $AHEAD 个提交" ;;
esac
if [ -n "$AHEAD_RISK" ] && [ "${DEPLOY_ALLOW_DISCARD:-0}" != "1" ]; then
  warn "拒绝执行 reset --hard：$AHEAD_RISK，将被丢弃。确认要丢弃请设置 DEPLOY_ALLOW_DISCARD=1"
  exit 1
fi
git reset --hard "origin/$REF"
AFTER="$(git rev-parse --short HEAD)"
log "目标版本: $AFTER"

# 换行符兜底：.gitattributes 只在「重新检出」时才作用于工作区。若某次拷贝把 CRLF
# 带了进来，git 会因 index 一致而不去碰它，结果就是 compose / Caddy 读到带 CR 的配置。
has_cr() {
  [ -f "$1" ] && [ "$(tr -cd '\r' < "$1" | wc -c)" -gt 0 ]
}
for f in docker-compose.yml .gitattributes Caddyfile deploy/update.sh; do
  if has_cr "$f"; then
    warn "$f 含 CRLF，强制重新检出以套用 .gitattributes"
    rm -f .git/index
    git reset --hard HEAD >/dev/null
    break
  fi
done
if has_cr ".env"; then
  warn "检测到 .env 含 CRLF 回车符，建议转换为 LF 以避免环境变量解析异常"
fi

# ------------------------------------------------------------------ 迁移
migration_count() {
  docker compose exec -T db psql -U nodeops -d nodeops -t -A \
    -c "SELECT count(*) FROM schema_migrations" 2>/dev/null | tr -d '[:space:]' || echo "?"
}

log "确保 db 在跑"
docker compose up -d db >/dev/null
# 2026-11-04：等库真的能连再往下。db 容器被重建（镜像标签变了）时，紧接着的 psql 会失败，
# 迁移计数拿到 "?"，后面的算术在 set -e 下直接让脚本退出 —— 迁移已跑、api 却没重建。
db_ready=0
for _ in $(seq 1 30); do
  if docker compose exec -T db pg_isready -U nodeops -d nodeops >/dev/null 2>&1; then
    db_ready=1
    break
  fi
  sleep 2
done
if [ "$db_ready" -ne 1 ]; then
  warn "等待数据库就绪超时（60s），中止迁移与发布。最近 db 日志："
  docker compose logs --tail=40 db >&2 || true
  exit 1
fi

MIG_BEFORE="$(migration_count)"
log "已应用迁移数: $MIG_BEFORE"

# ⚠️ 2026-10-31：跑迁移之前先备份**数据库**。
#
# 原脚本只备份代码目录（tar .），而迁移改的是库。迁移文件虽然是"逐文件事务 +
# 台账"，但一旦某条迁移把表改坏（或磁盘写满导致中途失败），没有库快照就只能靠
# 手工回滚。用 DEPLOY_NO_DB_BACKUP=1 可跳过（明确知情时的快速迭代）。
if [ "${DEPLOY_NO_DB_BACKUP:-0}" = "1" ]; then
  log "跳过数据库备份（DEPLOY_NO_DB_BACKUP=1）"
else
  DUMP="$BACKUP_DIR/$PROJECT-db-$STAMP.dump"
  DUMP_ERR="$(mktemp)"
  DUMP_PARTIAL="$DUMP"
  log "备份数据库到 $DUMP"
  if docker compose exec -T db pg_dump -U nodeops -d nodeops > "$DUMP" 2>"$DUMP_ERR" && [ -s "$DUMP" ]; then
    DUMP_PARTIAL=""
    rm -f "$DUMP_ERR"
    DUMP_ERR=""
    log "数据库备份完成: $(du -h "$DUMP" | cut -f1)"
    prune_backups 3 "$BACKUP_DIR" "${PROJECT}-db-" ".dump"
  else
    rm -f "$DUMP"
    DUMP_PARTIAL=""
    warn "数据库备份失败；为避免「改坏了没有退路」，本次不继续迁移"
    if [ -s "$DUMP_ERR" ]; then
      sed 's/^/    /' "$DUMP_ERR" >&2 || true
    fi
    rm -f "$DUMP_ERR"
    DUMP_ERR=""
    warn "确认可以冒险时用 DEPLOY_NO_DB_BACKUP=1 重跑"
    exit 1
  fi
fi

log "重建 migrate（带上新迁移文件）"
docker compose build migrate

log "应用迁移"
docker compose run --rm migrate

MIG_AFTER="$(migration_count)"
case "$MIG_BEFORE$MIG_AFTER" in
  *[!0-9]*) log "已应用迁移数: ${MIG_AFTER}（迁移前计数未知，无法算出新增数）" ;;
  *)
    if [ "$MIG_BEFORE" != "$MIG_AFTER" ]; then
      log "已应用迁移数: $MIG_AFTER（本次新增 $((MIG_AFTER - MIG_BEFORE))）"
    else
      log "已应用迁移数: $MIG_AFTER（无变化）"
    fi
    ;;
esac

# ------------------------------------------------------------------ 重建服务
log "重建并重启 api"
docker compose up -d --build api

# ⚠️ 2026-10-31：web 也必须重建。
# 原来这里只有 api，web 用旧镜像启动会导致前端静态资源改动无法上线。
log "重建并重启 web（前端静态资源）"
docker compose up -d --build web

# 外部商城服务属另一个编排项目；external 网络必须已存在，不在此处启动。
# web 已在上面 `up -d --build web` 重建，这里不再重复操作。

log "等待健康 ($HEALTH_HOST:$API_PORT/api/health)"
for _ in $(seq 1 45); do
  code="$(curl --noproxy '*' --max-time 5 -s -o /dev/null -w '%{http_code}' "http://$HEALTH_HOST:$API_PORT/api/health" || echo 000)"
  if [ "$code" = "200" ]; then
    log "健康 OK"
    log "部署完成: $BEFORE -> $AFTER"
    docker compose ps --format 'table {{.Name}}	{{.Status}}'
    exit 0
  fi
  sleep 2
done

warn "健康检查超时（90s）。最近 api 日志："
docker compose logs --tail=60 api >&2 || true
exit 1
