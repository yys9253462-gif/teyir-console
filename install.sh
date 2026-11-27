#!/usr/bin/env bash
# ==============================================================================
# Teyir 控制台（teyir-console）· 一条命令交互菜单式安装与运维脚本
# 仓库地址：https://github.com/yys9253462-gif/teyir-console
# 开源协议：GNU AGPL-3.0
#
# 使用方式：
#   交互菜单（默认）：  bash install.sh
#   全自动安装：        bash install.sh --yes
#   预览执行计划：      bash install.sh --dry-run --install
#   运维快捷命令：      bash install.sh --status | --upgrade | --check-update | --uninstall | --help
#
# 支持系统：Debian / Ubuntu（apt）、CentOS / RHEL / Rocky / Alma（dnf / yum）、Alpine（apk）
# ==============================================================================

set -Eeuo pipefail
umask 077

SCRIPT_VERSION="1.0.0"
APP_NAME="teyir-console"
APP_LABEL="Teyir 控制台"
DEFAULT_REPO="yys9253462-gif/teyir-console"
DEFAULT_REF="main"
DEFAULT_DIR="/opt/teyir-console"
DEFAULT_PROJECT="teyir-console"
DEFAULT_SUB_NAME="Teyir"
DEFAULT_ADMIN_EMAIL="admin@teyir.local"
DEFAULT_DIRECT_PORT="8088"
DEFAULT_LOCAL_WEB_PORT="18080"
DEFAULT_LOCAL_TLS_PORT="18443"
DEFAULT_API_PORT="8080"
SHORTCUT_PATH="/usr/local/bin/teyir"

MARK_BEGIN="# >>> teyir-console managed block —— 由 install.sh 维护，请勿手改 >>>"
MARK_END="# <<< teyir-console managed block <<<"

# ── 脚本自身定位（兼容管道运行、裸相对名调用与软链接快捷命令）──────────────────
SELF="$(basename "${0:-}")"
SELF_PATH=""
INSTALL_DIR_SELF=""

resolve_realpath() {
  local p="$1"
  if command -v readlink >/dev/null 2>&1; then
    readlink -f "$p" 2>/dev/null && return 0
  fi
  local dir base
  dir="$(cd "$(dirname "$p")" 2>/dev/null && pwd -P)" || return 1
  base="$(basename "$p")"
  printf '%s/%s\n' "$dir" "$base"
}

if [ -n "${0:-}" ] && [ -f "${0:-}" ]; then
  SELF_PATH="$(resolve_realpath "$0" 2>/dev/null || printf '%s' "$0")"
fi
case "$SELF" in
  bash|sh|dash|ash|zsh|ksh|"") SELF="install.sh"; SELF_PATH="" ;;
esac

if [ -n "$SELF_PATH" ] && [ -f "$SELF_PATH" ]; then
  _self_dir="$(cd "$(dirname "$SELF_PATH")" 2>/dev/null && pwd -P || true)"
  if [ -n "$_self_dir" ]; then
    if [ -f "$_self_dir/install.conf" ] || { [ -f "$_self_dir/.env" ] && [ -f "$_self_dir/docker-compose.yml" ]; }; then
      INSTALL_DIR_SELF="$_self_dir"
    fi
  fi
fi

# ── 终端色彩（仅输出到 stderr 时启用，避免污染任何命令替换返回值）────────────────
C_RED=""; C_GRN=""; C_YEL=""; C_BLU=""; C_CYN=""; C_DIM=""; C_BLD=""; C_OFF=""
if [ -t 2 ]; then
  C_RED=$'\033[31m'; C_GRN=$'\033[32m'; C_YEL=$'\033[33m'
  C_BLU=$'\033[34m'; C_CYN=$'\033[36m'; C_DIM=$'\033[2m'
  C_BLD=$'\033[1m'; C_OFF=$'\033[0m'
fi

# ── 统一输出函数（全部走 stderr，stdout 只留给函数返回值与 --help/--version）────
hr()   { printf '%s────────────────────────────────────────────────────────────%s\n' "$C_DIM" "$C_OFF" >&2; }
say()  { printf '%s\n' "$*" >&2; }
info() { printf '  %s\n' "$*" >&2; }
ok()   { printf '  %s✓%s %s\n' "$C_GRN" "$C_OFF" "$*" >&2; }
warn() { printf '  %s!%s %s\n' "$C_YEL" "$C_OFF" "$*" >&2; }
err()  { printf '  %s×%s %s\n' "$C_RED" "$C_OFF" "$*" >&2; }
step() { printf '\n%s▸ %s%s\n' "$C_BLD$C_CYN" "$*" "$C_OFF" >&2; }
dim()  { printf '  %s%s%s\n' "$C_DIM" "$*" "$C_OFF" >&2; }

# ── 运行期配置与状态变量 ───────────────────────────────────────────────────────
INSTALL_DIR="${INSTALL_DIR_SELF:-$DEFAULT_DIR}"
APP_REPO="$DEFAULT_REPO"
APP_REF="$DEFAULT_REF"
PROJECT_NAME="$DEFAULT_PROJECT"

ACCESS_MODE=""          # auto | self | host-caddy | docker-caddy | direct | local
DOMAIN=""
NO_DOMAIN=0
WEB_BIND=""
WEB_PORT=""
WEB_TLS_PORT=""
API_BIND="127.0.0.1"
API_PORT=""
SUB_NAME=""
ENABLE_COLLECTOR=""     # true | false
ADMIN_EMAIL=""
ADMIN_PASSWORD=""
STORE_RENEW_BASE_URL=""
HY2_INSTALL_SCRIPT_URL=""
HY2_INSTALLER_REPO=""
BUILD_NETWORK="auto"    # auto | default | host

# 显式命令行传参标记（用于区分「用户显式指定」与「状态文件回填」）
FLAG_DIR=0
FLAG_REPO=0
FLAG_REF=0
FLAG_PROJECT=0
FLAG_ACCESS_MODE=0
FLAG_DOMAIN=0
FLAG_WEB_BIND=0
FLAG_WEB_PORT=0
FLAG_WEB_TLS_PORT=0
FLAG_API_BIND=0
FLAG_API_PORT=0
FLAG_SUB_NAME=0
FLAG_COLLECTOR=0
FLAG_ADMIN_EMAIL=0
FLAG_ADMIN_PASSWORD=0
FLAG_ADV_GIVEN=0

# 动作开关与行为选项
ACTION=""               # "" (菜单) | install | upgrade | status | check-update | uninstall | logs | start | stop | restart | backup | help | version
LOGS_SERVICE="all"
ASSUME_YES=0
DRY_RUN=0
SKIP_DNS_CHECK=0
NO_DEPS=0
NO_SHORTCUT=0
PURGE_DATA=0
RECONFIG=0

# 探测与回滚状态
DETECTED_PROXY_TYPE="none"   # none | host-caddy | docker-caddy | nginx | other
DETECTED_PROXY_PROC=""
CADDY_MODE="none"            # none | self | host-caddy | docker-caddy
CADDY_CONTAINER=""
CADDY_FILE=""
CADDY_INNER=""
CADDY_NET=""
UPSTREAM_HOST="127.0.0.1"

CADDY_BACKED_UP=0
CADDY_BACKUP_FILE=""
FRESH_INSTALL=0
COMPOSE_STARTED=0
STATE_SAVED=0
USED_PORTS_CACHE=""
PUBLIC_IP_CACHE=""
LAST_FAIL_KIND=""
NON_TTY_NOTICED=0
IS_NEW_ADMIN_SEEDED=0
EXISTING_ENV_DETECTED=0

# 从状态文件读取的旧值（始终按白名单加载，严禁 source）
SAVED_LOADED=0
SAVED_PROJECT_NAME=""
SAVED_REPO=""
SAVED_BRANCH=""
SAVED_ACCESS_MODE=""
SAVED_DOMAIN=""
SAVED_WEB_BIND=""
SAVED_WEB_PORT=""
SAVED_WEB_TLS_PORT=""
SAVED_API_BIND=""
SAVED_API_PORT=""
SAVED_SUB_NAME=""
SAVED_ENABLE_COLLECTOR=""
SAVED_CADDY_MODE=""
SAVED_CADDY_CONTAINER=""
SAVED_CADDY_FILE=""
SAVED_CADDY_INNER=""
SAVED_CADDY_NET=""
SAVED_UPSTREAM_HOST=""
SAVED_SCRIPT_VERSION=""
SAVED_INSTALLED_AT=""

# ── 中断与失败自动回滚保护 ─────────────────────────────────────────────────────
prune_caddy_backups() {
  local target="${1:-$CADDY_FILE}"
  [ -n "$target" ] || return 0
  local dir base
  dir="$(dirname "$target")"
  base="$(basename "$target")"
  ls -1t "$dir/$base".bak-*-preTeyir 2>/dev/null \
    | tail -n +4 \
    | while IFS= read -r f; do
        if [ -n "$f" ]; then
          rm -f "$f" 2>/dev/null || true
        fi
      done
}

restore_caddy() {
  if [ "$CADDY_BACKED_UP" != "1" ]; then return 0; fi
  if [ -z "$CADDY_BACKUP_FILE" ] || [ ! -f "$CADDY_BACKUP_FILE" ]; then return 0; fi
  if [ -z "$CADDY_FILE" ]; then return 0; fi
  CADDY_BACKED_UP=0
  warn "正在还原共享反代配置文件：$CADDY_FILE"
  if cp "$CADDY_BACKUP_FILE" "$CADDY_FILE" 2>/dev/null; then
    if validate_external_caddy >/dev/null 2>&1 && reload_external_caddy >/dev/null 2>&1; then
      ok "已还原共享反代配置并完成热重载"
    else
      err "共享反代配置已还原，但自动重载未通过，请手工检查：$CADDY_FILE"
    fi
  else
    err "还原反代配置失败，请手工从备份恢复：$CADDY_BACKUP_FILE"
  fi
}

rollback_fresh_containers() {
  if [ "$FRESH_INSTALL" != "1" ] || [ "$COMPOSE_STARTED" != "1" ] || [ "$STATE_SAVED" != "0" ]; then
    return 0
  fi
  if [ "$DRY_RUN" = "1" ] || [ ! -f "$INSTALL_DIR/docker-compose.yml" ]; then
    return 0
  fi
  COMPOSE_STARTED=0
  warn "检测到首次安装尚未完成，正在停止本次启动的容器以释放端口（保留目录与已下载源码）..."
  COMPOSE_PROJECT_NAME="$PROJECT_NAME" docker compose \
    -f "$INSTALL_DIR/docker-compose.yml" \
    --env-file "$INSTALL_DIR/.env" \
    down --remove-orphans >/dev/null 2>&1 || true
}

on_signal() {
  trap - ERR EXIT INT TERM
  printf '\n' >&2
  warn "收到中断信号，正在执行安全收尾..."
  restore_caddy
  rollback_fresh_containers
  dim "说明：共享反代配置（若本次有改动）已还原；若刚才已拉取源码或生成配置，重跑脚本可继续接上。"
  exit 130
}

on_fail() {
  local rc=$?
  if [ "$rc" -eq 0 ]; then
    return 0
  fi
  trap - ERR EXIT INT TERM
  restore_caddy
  rollback_fresh_containers
  return "$rc"
}

trap on_signal INT TERM
trap on_fail ERR EXIT

die() {
  err "$*"
  exit 1
}

run() {
  if [ "$DRY_RUN" = "1" ]; then
    printf '  %s[预演]%s %s\n' "$C_DIM" "$C_OFF" "$*" >&2
    return 0
  fi
  "$@"
}

# ── 交互终端读取工具（兼容管道但有控制终端、中文回答与 EOF 安全语义）───────────
can_prompt() {
  if [ "$ASSUME_YES" = "1" ]; then
    return 1
  fi
  if [ -t 0 ]; then
    return 0
  fi
  (exec 3</dev/tty) 2>/dev/null
}

tty_read() {
  local __var="$1"
  local __silent="${2:-0}"
  local __val=""
  if [ -t 0 ]; then
    if [ "$__silent" = "1" ]; then
      IFS= read -rs __val || return 1
    else
      IFS= read -r __val || return 1
    fi
  elif (exec 3</dev/tty) 2>/dev/null; then
    if [ "$__silent" = "1" ]; then
      IFS= read -rs __val </dev/tty || return 1
    else
      IFS= read -r __val </dev/tty || return 1
    fi
  else
    return 1
  fi
  __val="${__val%$'\r'}"
  printf -v "$__var" '%s' "$__val"
}

notify_non_interactive_once() {
  if [ "$ASSUME_YES" != "1" ] && [ "$NON_TTY_NOTICED" = "0" ]; then
    NON_TTY_NOTICED=1
    warn "当前不是交互终端，所有问题将自动采用默认值。"
  fi
}

ask() {
  local prompt="$1" def="$2" ans=""
  if ! can_prompt; then
    notify_non_interactive_once
    printf '%s' "$def"
    return 0
  fi
  printf '  %s [%s]: ' "$prompt" "$def" >&2
  if ! tty_read ans 0; then
    printf '\n' >&2
    dim "（读不到输入，自动采用默认值：$def）"
    printf '%s' "$def"
    return 0
  fi
  printf '%s' "${ans:-$def}"
}

ask_yn() {
  local prompt="$1" def="$2" ans=""
  if ! can_prompt; then
    notify_non_interactive_once
    if [ "$def" = "y" ]; then return 0; else return 1; fi
  fi
  while :; do
    printf '  %s [%s]: ' "$prompt" "$def" >&2
    if ! tty_read ans 0; then
      printf '\n' >&2
      dim "（读不到输入，为安全起见按「否」处理）"
      return 1
    fi
    case "$ans" in
      "")
        if [ "$def" = "y" ]; then return 0; else return 1; fi
        ;;
      y|Y|yes|YES|Yes|true|1|是|是的|好|好的|对|要|嗯|可以|行|确定) return 0 ;;
      n|N|no|NO|No|false|0|否|不|不要|不用|不是|取消) return 1 ;;
      *) warn "没看懂「$ans」—— 请回答 y（是）或 n（否），直接回车使用默认值 $def" ;;
    esac
  done
}

ask_port() {
  local prompt="$1" def="$2" ans=""
  if ! can_prompt; then
    notify_non_interactive_once
    printf '%s' "$def"
    return 0
  fi
  while :; do
    ans="$(ask "$prompt" "$def")"
    if is_valid_port "$ans"; then
      printf '%s' "$ans"
      return 0
    fi
    warn "端口必须是 1 到 65535 之间的整数，请重新输入（直接回车使用 $def）"
  done
}

ask_password() {
  local prompt="$1" auto_pass="$2" ans=""
  if ! can_prompt; then
    notify_non_interactive_once
    printf '%s' "$auto_pass"
    return 0
  fi
  while :; do
    printf '  %s [直接回车自动生成 16 位强密码]: ' "$prompt" >&2
    if ! tty_read ans 1; then
      printf '\n' >&2
      dim "（读不到输入，自动生成强密码）"
      printf '%s' "$auto_pass"
      return 0
    fi
    printf '\n' >&2
    if [ -z "$ans" ]; then
      printf '%s' "$auto_pass"
      return 0
    fi
    if [ "${#ans}" -lt 12 ]; then
      warn "管理员密码至少需要 12 位字符（当前 ${#ans} 位），请重新输入或直接回车自动生成"
      continue
    fi
    if printf '%s' "$ans" | grep -q '[[:cntrl:]]'; then
      warn "密码不能包含控制字符，请重新输入"
      continue
    fi
    printf '%s' "$ans"
    return 0
  done
}

choose() {
  local prompt="$1" def="$2"
  shift 2
  local opts=("$@")
  local i=1 ans=""
  local o
  for o in "${opts[@]}"; do
    printf '    %s%d)%s %s\n' "$C_CYN" "$i" "$C_OFF" "$o" >&2
    i=$((i + 1))
  done
  if ! can_prompt; then
    notify_non_interactive_once
    printf '%s' "$def"
    return 0
  fi
  while :; do
    printf '  %s [%s]: ' "$prompt" "$def" >&2
    if ! tty_read ans 0; then
      printf '\n' >&2
      dim "（读不到输入，采用默认选项 $def）"
      printf '%s' "$def"
      return 0
    fi
    if [ -z "$ans" ]; then
      printf '%s' "$def"
      return 0
    fi
    if printf '%s' "$ans" | grep -qE '^[0-9]+$' && [ "$ans" -ge 1 ] && [ "$ans" -le "${#opts[@]}" ]; then
      printf '%s' "$ans"
      return 0
    fi
    warn "请输入 1 到 ${#opts[@]} 之间的数字（直接回车选默认 $def）"
  done
}

pause_enter() {
  if ! can_prompt; then
    return 0
  fi
  local _dummy=""
  printf '\n  %s按回车返回主菜单...%s' "$C_DIM" "$C_OFF" >&2
  tty_read _dummy 0 || true
}

# ── 基础校验与随机密钥生成 ─────────────────────────────────────────────────────
is_valid_port() {
  printf '%s' "${1:-}" | grep -qE '^[0-9]+$' && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

clean_domain() {
  local d="$1"
  d="$(printf '%s' "$d" | tr -d '[:space:]' | tr '[:upper:]' '[:lower:]')"
  d="${d#https://}"
  d="${d#http://}"
  d="${d%%/*}"
  d="${d%%\?*}"
  d="${d%%#*}"
  d="${d%%:*}"
  d="${d%.}"
  printf '%s' "$d"
}

is_valid_domain() {
  local d="$1"
  [ -n "$d" ] || return 1
  [ "${#d}" -le 253 ] || return 1
  printf '%s' "$d" | grep -qE '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$'
}

is_valid_email() {
  local e="$1"
  [ -n "$e" ] || return 1
  printf '%s' "$e" | grep -qE '^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$'
}

is_valid_project_name() {
  printf '%s' "${1:-}" | grep -qE '^[a-z0-9][a-z0-9_-]{0,48}$'
}

is_valid_bind_ip() {
  local b="$1"
  case "$b" in
    127.0.0.1|0.0.0.0|::1|::) return 0 ;;
  esac
  printf '%s' "$b" | grep -qE '^([0-9]{1,3}\.){3}[0-9]{1,3}$'
}

is_safe_install_dir() {
  local d="$1"
  case "$d" in
    ""|"/"|"/bin"|"/boot"|"/dev"|"/etc"|"/home"|"/lib"|"/lib64"|"/opt"|"/proc"|"/root"|"/run"|"/sbin"|"/srv"|"/sys"|"/tmp"|"/usr"|"/var")
      return 1 ;;
    /*)
      ! printf '%s' "$d" | grep -qE '[[:space:][:cntrl:]"'\''`$;|&<>]' ;;
    *)
      return 1 ;;
  esac
}

gen_urlsafe_secret() {
  local len="${1:-48}"
  local out=""
  if command -v openssl >/dev/null 2>&1; then
    out="$(openssl rand -base64 $((len + 16)) 2>/dev/null | tr '+/' '-_' | tr -dc 'A-Za-z0-9_-' | head -c "$len" || true)"
  fi
  if [ "${#out}" -lt "$len" ] && command -v python3 >/dev/null 2>&1; then
    out="$(python3 -c "import secrets; print(secrets.token_urlsafe($len)[:$len])" 2>/dev/null || true)"
  fi
  if [ "${#out}" -lt "$len" ]; then
    out="$(head -c 512 /dev/urandom 2>/dev/null | LC_ALL=C tr -dc 'A-Za-z0-9_-' | head -c "$len" || true)"
  fi
  [ "${#out}" -ge 16 ] || die "无法生成安全随机字符串（请检查 openssl 或 /dev/urandom）"
  printf '%s' "$out"
}

gen_alnum_password() {
  local len="${1:-16}"
  local out=""
  if command -v openssl >/dev/null 2>&1; then
    out="$(openssl rand -base64 $((len * 4)) 2>/dev/null | tr -dc 'A-HJ-NP-Za-km-z2-9' | head -c "$len" || true)"
  fi
  if [ "${#out}" -lt "$len" ] && command -v python3 >/dev/null 2>&1; then
    out="$(python3 -c "import secrets; alphabet='ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789'; print(''.join(secrets.choice(alphabet) for _ in range($len)))" 2>/dev/null || true)"
  fi
  if [ "${#out}" -lt "$len" ]; then
    out="$(head -c 512 /dev/urandom 2>/dev/null | LC_ALL=C tr -dc 'A-HJ-NP-Za-km-z2-9' | head -c "$len" || true)"
  fi
  [ "${#out}" -ge 12 ] || die "无法生成管理员随机密码"
  printf '%s' "$out"
}

# CREDENTIAL_KEY 必须是标准 base64（Go base64.StdEncoding，32 字节随机数编码后固定 44 字符）
gen_credential_key() {
  local out=""
  if command -v openssl >/dev/null 2>&1; then
    out="$(openssl rand -base64 32 2>/dev/null | tr -d '\r\n' || true)"
  elif command -v python3 >/dev/null 2>&1; then
    out="$(python3 -c "import os, base64; print(base64.b64encode(os.urandom(32)).decode())" 2>/dev/null || true)"
  elif command -v base64 >/dev/null 2>&1; then
    out="$(head -c 32 /dev/urandom 2>/dev/null | base64 | tr -d '\r\n' || true)"
  fi
  [ "${#out}" -eq 44 ] || die "无法生成标准 32 字节 Base64 凭据主密钥（CREDENTIAL_KEY）"
  printf '%s' "$out"
}

# ── 安全键值读写（严禁 source 配置文件，防命令注入）───────────────────────────
read_kv_file() {
  local file="$1" key="$2"
  if [ ! -f "$file" ] || [ ! -r "$file" ]; then
    return 0
  fi
  local line k v found=""
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    case "$line" in
      ""|\#*) continue ;;
      *=*) ;;
      *) continue ;;
    esac
    k="${line%%=*}"
    k="${k#"${k%%[![:space:]]*}"}"
    k="${k%"${k##*[![:space:]]}"}"
    if [ "$k" != "$key" ]; then
      continue
    fi
    v="${line#*=}"
    v="${v#"${v%%[![:space:]]*}"}"
    v="${v%"${v##*[![:space:]]}"}"
    case "$v" in
      \"*\") v="${v#\"}"; v="${v%\"}" ;;
      \'*\') v="${v#\'}"; v="${v%\'}" ;;
    esac
    found="$v"
  done < "$file"
  printf '%s' "$found"
}

reset_saved_vars() {
  SAVED_LOADED=0
  SAVED_PROJECT_NAME=""
  SAVED_REPO=""
  SAVED_BRANCH=""
  SAVED_ACCESS_MODE=""
  SAVED_DOMAIN=""
  SAVED_WEB_BIND=""
  SAVED_WEB_PORT=""
  SAVED_WEB_TLS_PORT=""
  SAVED_API_BIND=""
  SAVED_API_PORT=""
  SAVED_SUB_NAME=""
  SAVED_ENABLE_COLLECTOR=""
  SAVED_CADDY_MODE=""
  SAVED_CADDY_CONTAINER=""
  SAVED_CADDY_FILE=""
  SAVED_CADDY_INNER=""
  SAVED_CADDY_NET=""
  SAVED_UPSTREAM_HOST=""
  SAVED_SCRIPT_VERSION=""
  SAVED_INSTALLED_AT=""
}

load_saved_state() {
  local conf="$INSTALL_DIR/install.conf"
  reset_saved_vars
  if [ ! -f "$conf" ] || [ ! -r "$conf" ]; then
    return 0
  fi
  local line k v valid_keys=0 non_empty=0
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    case "$line" in
      ""|\#*) continue ;;
    esac
    non_empty=$((non_empty + 1))
    case "$line" in
      *=*) ;;
      *) continue ;;
    esac
    k="${line%%=*}"
    v="${line#*=}"
    case "$k" in
      PROJECT_NAME|REPO|BRANCH|ACCESS_MODE|DOMAIN|WEB_BIND|WEB_PORT|WEB_TLS_PORT|API_BIND|API_PORT|SUB_NAME|ENABLE_COLLECTOR|CADDY_MODE|CADDY_CONTAINER|CADDY_FILE|CADDY_INNER|CADDY_NET|UPSTREAM_HOST|SCRIPT_VERSION|INSTALLED_AT)
        printf -v "SAVED_${k}" '%s' "$v"
        valid_keys=$((valid_keys + 1))
        ;;
    esac
  done < "$conf"
  if [ "$non_empty" -gt 0 ] && [ "$valid_keys" -eq 0 ]; then
    warn "检测到状态文件 $conf 可能已损坏（未解析出有效配置项），将采用默认配置"
    return 0
  fi
  if [ "$valid_keys" -gt 0 ]; then
    SAVED_LOADED=1
  fi
}

prefill_from_saved() {
  if [ "$SAVED_LOADED" != "1" ]; then
    return 0
  fi
  if [ "$FLAG_PROJECT" = "0" ] && [ -n "$SAVED_PROJECT_NAME" ]; then PROJECT_NAME="$SAVED_PROJECT_NAME"; fi
  if [ "$FLAG_REPO" = "0" ] && [ -n "$SAVED_REPO" ]; then APP_REPO="$SAVED_REPO"; fi
  if [ "$FLAG_REF" = "0" ] && [ -n "$SAVED_BRANCH" ]; then APP_REF="$SAVED_BRANCH"; fi
  if [ "$FLAG_ACCESS_MODE" = "0" ] && [ -n "$SAVED_ACCESS_MODE" ] && [ "$NO_DOMAIN" != "1" ]; then ACCESS_MODE="$SAVED_ACCESS_MODE"; fi
  if [ "$NO_DOMAIN" = "1" ]; then
    DOMAIN=""
  elif [ "$FLAG_DOMAIN" = "0" ] && [ -n "$SAVED_DOMAIN" ]; then
    DOMAIN="$SAVED_DOMAIN"
  fi
  if [ "$FLAG_WEB_BIND" = "0" ] && [ -n "$SAVED_WEB_BIND" ]; then WEB_BIND="$SAVED_WEB_BIND"; fi
  if [ "$FLAG_WEB_PORT" = "0" ] && [ -n "$SAVED_WEB_PORT" ]; then WEB_PORT="$SAVED_WEB_PORT"; fi
  if [ "$FLAG_WEB_TLS_PORT" = "0" ] && [ -n "$SAVED_WEB_TLS_PORT" ]; then WEB_TLS_PORT="$SAVED_WEB_TLS_PORT"; fi
  if [ "$FLAG_API_BIND" = "0" ] && [ -n "$SAVED_API_BIND" ]; then API_BIND="$SAVED_API_BIND"; fi
  if [ "$FLAG_API_PORT" = "0" ] && [ -n "$SAVED_API_PORT" ]; then API_PORT="$SAVED_API_PORT"; fi
  if [ "$FLAG_SUB_NAME" = "0" ] && [ -n "$SAVED_SUB_NAME" ]; then SUB_NAME="$SAVED_SUB_NAME"; fi
  if [ "$FLAG_COLLECTOR" = "0" ] && [ -n "$SAVED_ENABLE_COLLECTOR" ]; then ENABLE_COLLECTOR="$SAVED_ENABLE_COLLECTOR"; fi
}

save_state() {
  local conf="$INSTALL_DIR/install.conf"
  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将写入状态文件 $conf（权限 600）并备份脚本到 $INSTALL_DIR/install.sh"
    return 0
  fi
  local now
  now="$(date '+%Y-%m-%d %H:%M:%S')"
  local tmp="${conf}.tmp.$$"
  cat > "$tmp" <<EOF
# Teyir 控制台安装状态文件（由 install.sh 自动维护，请勿手工执行）
SCRIPT_VERSION=${SCRIPT_VERSION}
INSTALLED_AT=${now}
PROJECT_NAME=${PROJECT_NAME}
REPO=${APP_REPO}
BRANCH=${APP_REF}
ACCESS_MODE=${ACCESS_MODE}
DOMAIN=${DOMAIN}
WEB_BIND=${WEB_BIND}
WEB_PORT=${WEB_PORT}
WEB_TLS_PORT=${WEB_TLS_PORT}
API_BIND=${API_BIND}
API_PORT=${API_PORT}
SUB_NAME=${SUB_NAME}
ENABLE_COLLECTOR=${ENABLE_COLLECTOR}
CADDY_MODE=${CADDY_MODE}
CADDY_CONTAINER=${CADDY_CONTAINER}
CADDY_FILE=${CADDY_FILE}
CADDY_INNER=${CADDY_INNER}
CADDY_NET=${CADDY_NET}
UPSTREAM_HOST=${UPSTREAM_HOST}
EOF
  chmod 600 "$tmp"
  mv -f "$tmp" "$conf"

  # 保存脚本副本到安装目录，保证即使管道安装后也能随时执行维护命令
  if [ -n "$SELF_PATH" ] && [ -f "$SELF_PATH" ]; then
    local dst="$INSTALL_DIR/install.sh"
    local real_dst
    real_dst="$(resolve_realpath "$dst" 2>/dev/null || printf '%s' "$dst")"
    if [ "$SELF_PATH" != "$real_dst" ]; then
      if ! cmp -s "$SELF_PATH" "$dst" 2>/dev/null; then
        cp -f "$SELF_PATH" "$dst"
      fi
    fi
    chmod 755 "$dst"
  elif [ -f "$INSTALL_DIR/install.sh" ]; then
    chmod 755 "$INSTALL_DIR/install.sh"
  fi

  # 安装全局快捷命令 teyir（方便用户随时输入 teyir 打开交互菜单）
  if [ "$NO_SHORTCUT" != "1" ] && [ -f "$INSTALL_DIR/install.sh" ] && [ "$(id -u)" -eq 0 ]; then
    if [ ! -e "$SHORTCUT_PATH" ] || [ -L "$SHORTCUT_PATH" ]; then
      mkdir -p "$(dirname "$SHORTCUT_PATH")"
      ln -sfn "$INSTALL_DIR/install.sh" "$SHORTCUT_PATH"
    fi
  fi
  STATE_SAVED=1
}

need_state() {
  local conf="$INSTALL_DIR/install.conf"
  if [ ! -e "$INSTALL_DIR" ]; then
    err "未检测到安装目录：$INSTALL_DIR（这台机器上还没安装过 $APP_LABEL）"
    info "如需全新安装，请运行：$(render_priv_cmd --install)"
    return 1
  fi
  if [ ! -r "$INSTALL_DIR" ]; then
    err "没有权限读取安装目录：$INSTALL_DIR"
    info "请使用管理员权限重试：$(render_priv_cmd --status)"
    return 1
  fi
  if [ ! -f "$conf" ] && [ ! -f "$INSTALL_DIR/.env" ]; then
    err "目录 $INSTALL_DIR 存在，但缺少 install.conf 与 .env（多半是上次安装中途中断）"
    info "直接重跑安装即可继续：$(render_priv_cmd --install --dir "$INSTALL_DIR")"
    return 1
  fi
  load_saved_state
  prefill_from_saved
  return 0
}

render_self_cmd() {
  local base=""
  if [ -L "$SHORTCUT_PATH" ] && [ -x "$SHORTCUT_PATH" ]; then
    base="teyir"
  elif [ -f "$INSTALL_DIR/install.sh" ]; then
    base="bash $INSTALL_DIR/install.sh"
  else
    base="bash $SELF"
  fi
  if [ $# -gt 0 ]; then
    printf '%s %s' "$base" "$*"
  else
    printf '%s' "$base"
  fi
}

render_priv_cmd() {
  local cmd
  cmd="$(render_self_cmd "$@")"
  if [ "$(id -u)" -eq 0 ]; then
    printf '%s' "$cmd"
  elif command -v sudo >/dev/null 2>&1; then
    printf 'sudo %s' "$cmd"
  else
    printf 'su -c "%s"' "$cmd"
  fi
}

# ── 参数拼写纠错（编辑距离）────────────────────────────────────────────────────
_levdist() {
  local s="$1" t="$2"
  local m=${#s} n=${#t}
  local -a prev cur
  local i j cost a b c
  for ((j = 0; j <= n; j++)); do prev[$j]=$j; done
  for ((i = 1; i <= m; i++)); do
    cur[0]=$i
    for ((j = 1; j <= n; j++)); do
      if [ "${s:i-1:1}" = "${t:j-1:1}" ]; then cost=0; else cost=1; fi
      a=$((prev[j] + 1))
      b=$((cur[j-1] + 1))
      c=$((prev[j-1] + cost))
      if [ "$a" -le "$b" ] && [ "$a" -le "$c" ]; then
        cur[$j]=$a
      elif [ "$b" -le "$c" ]; then
        cur[$j]=$b
      else
        cur[$j]=$c
      fi
    done
    for ((j = 0; j <= n; j++)); do prev[$j]=${cur[$j]}; done
  done
  printf '%s' "${prev[$n]}"
}

KNOWN_FLAGS=(
  --install --upgrade --status --check-update --uninstall --logs --start --stop --restart --backup
  --yes --dry-run --help --version --no-domain --skip-dns-check --no-deps --no-shortcut --purge
  --dir --repo --branch --project-name --access-mode --domain
  --web-bind --web-port --web-tls-port --api-bind --api-port
  --admin-email --admin-password --sub-name --collector --build-network
)

is_known_flag() {
  local x="$1" f
  case "$x" in
    -y|-h|-V) return 0 ;;
  esac
  for f in "${KNOWN_FLAGS[@]}"; do
    if [ "$x" = "$f" ]; then return 0; fi
  done
  return 1
}

suggest_arg() {
  local raw="$1"
  local had_dash=0
  case "$raw" in -*) had_dash=1 ;; esac
  local norm
  norm="$(printf '%s' "$raw" | sed 's/^-*//' | tr '[:upper:]' '[:lower:]')"
  local nlen=${#norm}
  local maxd=5
  if [ "$nlen" -le 4 ]; then maxd=2
  elif [ "$nlen" -le 7 ]; then maxd=4
  fi
  local best="" best_d=999 f fnorm d
  for f in "${KNOWN_FLAGS[@]}"; do
    fnorm="${f#--}"
    d="$(_levdist "$norm" "$fnorm")"
    if [ "$d" -eq 0 ] && [ "$had_dash" -eq 0 ]; then
      printf '%s' "$f"; return 0
    fi
    if [ "$d" -lt "$best_d" ] && [ "$d" -le "$maxd" ]; then
      best_d=$d; best="$f"
    fi
  done
  printf '%s' "$best"
}

# ── 帮助与版本信息 ─────────────────────────────────────────────────────────────
show_help() {
  cat <<EOF
${APP_LABEL}（${APP_NAME}）一键安装与交互管理脚本 v${SCRIPT_VERSION}

用法：
  bash ${SELF}                         打开交互式主菜单（推荐）
  bash ${SELF} --yes                   全自动按默认值安装（无需交互）
  bash ${SELF} --dry-run --install     预演安装计划，不对系统做任何修改
  bash ${SELF} --status                查看服务运行状态与访问信息（无需管理员权限）
  bash ${SELF} --upgrade               备份数据、拉取最新代码、迁移数据库并重建服务
  bash ${SELF} --check-update          检查远端仓库是否有新提交
  bash ${SELF} --logs [api|web|db|all] 查看容器最近日志
  bash ${SELF} --start | --stop | --restart   启停或重启服务
  bash ${SELF} --backup                立即备份配置与 PostgreSQL 数据库
  bash ${SELF} --uninstall [--purge]   卸载服务（默认保留数据卷与目录；--purge 彻底清理）

配置参数（均支持 --参数 值 与 --参数=值 两种写法）：
  --dir <绝对路径>            安装目录（默认：${DEFAULT_DIR}）
  --project-name <名称>       容器编排项目名（默认：${DEFAULT_PROJECT}）
  --repo <owner/repo>         源码仓库（默认：${DEFAULT_REPO}）
  --branch <分支名>           源码分支（默认：${DEFAULT_REF}）
  --domain <域名>             绑定域名并自动配置 HTTPS（自动识别 80/443 或复用已有 Caddy）
  --no-domain                 显式取消域名绑定并摘除本脚本写入的共享反代站点块
  --access-mode <模式>        访问模式：auto | self | host-caddy | docker-caddy | direct | local
  --web-bind <IP>             前端监听地址（如 127.0.0.1 或 0.0.0.0）
  --web-port <端口|auto>      前端 HTTP 端口（默认按访问模式自动挑选空闲端口）
  --web-tls-port <端口|auto>  前端 HTTPS 端口（默认按访问模式自动挑选空闲端口）
  --api-bind <IP>             后端 API 宿主机监听地址（默认：127.0.0.1）
  --api-port <端口|auto>      后端 API 宿主机端口（默认：${DEFAULT_API_PORT}，冲突自动避让）
  --admin-email <邮箱>        首次安装初始管理员邮箱（默认：${DEFAULT_ADMIN_EMAIL}）
  --admin-password <密码>     首次安装初始管理员密码（至少 12 位，留空自动生成）
  --sub-name <名称>           客户端导入订阅后的默认分组名（默认：${DEFAULT_SUB_NAME}）
  --collector <on|off>        是否启用周期采集器（默认：on）
  --build-network <模式>      镜像构建网络模式：auto | default | host（默认：auto）
  --skip-dns-check            跳过域名解析与公网 IP 一致性预检（CDN 回源场景可用）
  --no-deps                   不自动安装缺失依赖，仅提示安装命令
  --no-shortcut               不写入全局快捷命令 ${SHORTCUT_PATH}
  -y, --yes                   非交互模式，全部采用默认值或命令行参数
  -V, --version               显示脚本版本号
  -h, --help                  显示本帮助说明

一条命令安装示例（在目标 Linux 服务器上执行）：
  # Root 用户（提示符为 #）：
  curl -fsSL https://raw.githubusercontent.com/${DEFAULT_REPO}/${DEFAULT_REF}/install.sh -o ~/teyir-install.sh && bash ~/teyir-install.sh

  # 普通用户（提示符为 \$）：
  curl -fsSL https://raw.githubusercontent.com/${DEFAULT_REPO}/${DEFAULT_REF}/install.sh -o ~/teyir-install.sh && sudo bash ~/teyir-install.sh
EOF
}

# ── 参数解析 ───────────────────────────────────────────────────────────────────
set_action() {
  local next="$1"
  if [ -n "$ACTION" ] && [ "$ACTION" != "$next" ]; then
    die "不能同时指定多个冲突动作：--${ACTION} 与 --${next}"
  fi
  ACTION="$next"
}

require_value() {
  local opt="$1" argc="$2" next_val="${3:-}"
  [ "$argc" -ge 2 ] || die "参数 ${opt} 需要一个值（例如：${opt} <值>）"
  if is_known_flag "$next_val"; then
    die "参数 ${opt} 需要一个值，但紧跟的是另一个选项 '${next_val}' —— 是不是漏写了 ${opt} 的值？"
  fi
}

parse_args() {
  local opt val
  while [ $# -gt 0 ]; do
    opt="$1"
    val=""
    case "$opt" in
      --*=*)
        val="${opt#*=}"
        opt="${opt%%=*}"
        case "$opt" in
          --dir|--repo|--branch|--project-name|--access-mode|--domain|--web-bind|--web-port|--web-tls-port|--api-bind|--api-port|--admin-email|--admin-password|--sub-name|--collector|--build-network|--logs)
            [ -n "$val" ] || die "参数 ${opt}= 后面不能为空"
            ;;
          *)
            die "参数 ${opt} 不接受等号传值"
            ;;
        esac
        set -- "$opt" "$val" "${@:2}"
        ;;
    esac

    case "$1" in
      --dir|--repo|--branch|--project-name|--access-mode|--domain|--web-bind|--web-port|--web-tls-port|--api-bind|--api-port|--admin-email|--admin-password|--sub-name|--collector|--build-network)
        require_value "$1" "$#" "${2:-}"
        ;;
    esac

    case "$1" in
      --install)      set_action "install"; shift ;;
      --upgrade)      set_action "upgrade"; shift ;;
      --status)       set_action "status"; shift ;;
      --check-update) set_action "check-update"; shift ;;
      --uninstall)    set_action "uninstall"; shift ;;
      --start)        set_action "start"; shift ;;
      --stop)         set_action "stop"; shift ;;
      --restart)      set_action "restart"; shift ;;
      --backup)       set_action "backup"; shift ;;
      --logs)
        set_action "logs"
        if [ $# -ge 2 ] && ! is_known_flag "$2"; then
          case "$2" in
            api|web|db|migrate|all) LOGS_SERVICE="$2"; shift 2 ;;
            *) die "--logs 仅支持 api | web | db | migrate | all，收到：$2" ;;
          esac
        else
          shift
        fi
        ;;
      -y|--yes)          ASSUME_YES=1; shift ;;
      --dry-run)         DRY_RUN=1; shift ;;
      --no-domain)       NO_DOMAIN=1; DOMAIN=""; FLAG_DOMAIN=1; shift ;;
      --skip-dns-check)  SKIP_DNS_CHECK=1; shift ;;
      --no-deps)         NO_DEPS=1; shift ;;
      --no-shortcut)     NO_SHORTCUT=1; shift ;;
      --purge)           PURGE_DATA=1; shift ;;
      --dir)             INSTALL_DIR="${2%/}"; FLAG_DIR=1; FLAG_ADV_GIVEN=1; shift 2 ;;
      --repo)            APP_REPO="$2"; FLAG_REPO=1; FLAG_ADV_GIVEN=1; shift 2 ;;
      --branch)          APP_REF="$2"; FLAG_REF=1; FLAG_ADV_GIVEN=1; shift 2 ;;
      --project-name)    PROJECT_NAME="$2"; FLAG_PROJECT=1; FLAG_ADV_GIVEN=1; shift 2 ;;
      --access-mode)
        case "$2" in
          auto|self|host-caddy|docker-caddy|direct|local) ACCESS_MODE="$2"; FLAG_ACCESS_MODE=1 ;;
          *) die "--access-mode 非法值 '$2'，可选：auto | self | host-caddy | docker-caddy | direct | local" ;;
        esac
        shift 2
        ;;
      --domain)
        DOMAIN="$(clean_domain "$2")"
        NO_DOMAIN=0
        FLAG_DOMAIN=1
        shift 2
        ;;
      --web-bind)        WEB_BIND="$2"; FLAG_WEB_BIND=1; FLAG_ADV_GIVEN=1; shift 2 ;;
      --web-port)        WEB_PORT="$2"; FLAG_WEB_PORT=1; FLAG_ADV_GIVEN=1; shift 2 ;;
      --web-tls-port)    WEB_TLS_PORT="$2"; FLAG_WEB_TLS_PORT=1; FLAG_ADV_GIVEN=1; shift 2 ;;
      --api-bind)        API_BIND="$2"; FLAG_API_BIND=1; FLAG_ADV_GIVEN=1; shift 2 ;;
      --api-port)        API_PORT="$2"; FLAG_API_PORT=1; FLAG_ADV_GIVEN=1; shift 2 ;;
      --admin-email)     ADMIN_EMAIL="$2"; FLAG_ADMIN_EMAIL=1; shift 2 ;;
      --admin-password)  ADMIN_PASSWORD="$2"; FLAG_ADMIN_PASSWORD=1; shift 2 ;;
      --sub-name)        SUB_NAME="$2"; FLAG_SUB_NAME=1; FLAG_ADV_GIVEN=1; shift 2 ;;
      --collector)
        case "$2" in
          on|true|1|yes|是)  ENABLE_COLLECTOR="true"; FLAG_COLLECTOR=1; FLAG_ADV_GIVEN=1 ;;
          off|false|0|no|否) ENABLE_COLLECTOR="false"; FLAG_COLLECTOR=1; FLAG_ADV_GIVEN=1 ;;
          *) die "--collector 仅支持 on | off（或 true | false），收到：$2" ;;
        esac
        shift 2
        ;;
      --build-network)
        case "$2" in
          auto|default|host) BUILD_NETWORK="$2" ;;
          *) die "--build-network 仅支持 auto | default | host，收到：$2" ;;
        esac
        shift 2
        ;;
      -h|--help)    ACTION="help"; shift ;;
      -V|--version) ACTION="version"; shift ;;
      *)
        local sug
        sug="$(suggest_arg "$1")"
        if [ -n "$sug" ]; then
          die "不认识的参数 '$1' —— 你是不是想输入 '${sug}'？（运行 bash ${SELF} --help 查看完整用法）"
        fi
        die "不认识的参数 '$1'（运行 bash ${SELF} --help 查看完整用法）"
        ;;
    esac
  done

  if [ "$NO_DOMAIN" = "1" ] && [ -n "$DOMAIN" ]; then
    die "不能同时指定 --domain 与 --no-domain"
  fi
  if [ "$NO_DOMAIN" = "1" ] && [ "$ACCESS_MODE" = "self" ]; then
    die "--no-domain 不能与 --access-mode self 同时使用"
  fi
}

validate_inputs() {
  is_safe_install_dir "$INSTALL_DIR" || die "安装目录不合法或不安全（须为非系统根目录的绝对路径）：$INSTALL_DIR"
  is_valid_project_name "$PROJECT_NAME" || die "项目名仅支持小写字母、数字、连字符与下划线，且须以字母或数字开头：$PROJECT_NAME"
  printf '%s' "$APP_REPO" | grep -qE '^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$' || die "仓库地址格式应为 owner/repo，收到：$APP_REPO"
  printf '%s' "$APP_REF" | grep -qE '^[A-Za-z0-9._/-]+$' || die "分支名包含非法字符：$APP_REF"
  if [ -n "$DOMAIN" ]; then
    is_valid_domain "$DOMAIN" || die "域名格式非法：$DOMAIN（示例：panel.example.com，无需带 http:// 或路径）"
  fi
  if [ -n "$WEB_BIND" ]; then
    is_valid_bind_ip "$WEB_BIND" || die "前端监听地址非法：$WEB_BIND"
  fi
  if [ -n "$API_BIND" ]; then
    is_valid_bind_ip "$API_BIND" || die "后端监听地址非法：$API_BIND"
  fi
  if [ -n "$WEB_PORT" ] && [ "$WEB_PORT" != "auto" ]; then
    is_valid_port "$WEB_PORT" || die "前端 HTTP 端口非法（1–65535 或 auto）：$WEB_PORT"
  fi
  if [ -n "$WEB_TLS_PORT" ] && [ "$WEB_TLS_PORT" != "auto" ]; then
    is_valid_port "$WEB_TLS_PORT" || die "前端 HTTPS 端口非法（1–65535 或 auto）：$WEB_TLS_PORT"
  fi
  if [ -n "$API_PORT" ] && [ "$API_PORT" != "auto" ]; then
    is_valid_port "$API_PORT" || die "后端 API 端口非法（1–65535 或 auto）：$API_PORT"
  fi
  if [ -n "$ADMIN_EMAIL" ]; then
    is_valid_email "$ADMIN_EMAIL" || die "管理员邮箱格式非法：$ADMIN_EMAIL"
  fi
  if [ -n "$ADMIN_PASSWORD" ]; then
    [ "${#ADMIN_PASSWORD}" -ge 12 ] || die "管理员密码至少需要 12 位字符（当前 ${#ADMIN_PASSWORD} 位）"
    ! printf '%s' "$ADMIN_PASSWORD" | grep -q '[[:cntrl:]]' || die "管理员密码不能包含控制字符"
  fi
}

# ── 权限与系统依赖探测 / 自动安装 ──────────────────────────────────────────────
require_root() {
  if [ "$DRY_RUN" = "1" ]; then
    return 0
  fi
  if [ "$(id -u)" -ne 0 ]; then
    err "该操作需要管理员权限（root）以管理容器、服务目录或反代配置。"
    if command -v sudo >/dev/null 2>&1; then
      info "请改用 sudo 运行：sudo bash ${SELF} $*"
    else
      info "当前机器未安装 sudo，请先执行 su - 切换到 root 后再运行本脚本。"
    fi
    return 1
  fi
}

detect_pkg_mgr() {
  if command -v apt-get >/dev/null 2>&1; then printf 'apt'
  elif command -v dnf >/dev/null 2>&1; then printf 'dnf'
  elif command -v yum >/dev/null 2>&1; then printf 'yum'
  elif command -v apk >/dev/null 2>&1; then printf 'apk'
  else printf 'unknown'
  fi
}

pkg_install() {
  local mgr
  mgr="$(detect_pkg_mgr)"
  case "$mgr" in
    apt)
      DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null 2>&1 || true
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@"
      ;;
    dnf) dnf install -y -q "$@" ;;
    yum) yum install -y -q "$@" ;;
    apk) apk add --no-cache -q "$@" ;;
    *) return 1 ;;
  esac
}

docker_bin_ok() {
  command -v docker >/dev/null 2>&1
}

docker_daemon_ok() {
  docker info >/dev/null 2>&1
}

compose_ok() {
  docker compose version >/dev/null 2>&1
}

ensure_docker_service_running() {
  if docker_daemon_ok; then
    return 0
  fi
  if command -v systemctl >/dev/null 2>&1; then
    systemctl enable --now docker >/dev/null 2>&1 || systemctl start docker >/dev/null 2>&1 || true
  elif command -v service >/dev/null 2>&1; then
    service docker start >/dev/null 2>&1 || true
  elif command -v rc-service >/dev/null 2>&1; then
    rc-service docker start >/dev/null 2>&1 || true
  fi
  local i
  for i in 1 2 3 4 5 6; do
    if docker_daemon_ok; then return 0; fi
    sleep 2
  done
  return 1
}

install_docker_auto() {
  local mgr
  mgr="$(detect_pkg_mgr)"
  local tmp_script log_file
  tmp_script="$(mktemp /tmp/teyir-get-docker.XXXXXX.sh)"
  log_file="$(mktemp /tmp/teyir-docker-install.XXXXXX.log)"

  # 方式 1：官方一键脚本（下载落地 + 语法校验后再执行）
  info "正在尝试方式 1/4：Docker 官方安装脚本..."
  if curl -fsSL --proto '=https' --tlsv1.2 --max-time 30 https://get.docker.com -o "$tmp_script" 2>"$log_file" \
    && [ -s "$tmp_script" ] && sh -n "$tmp_script" 2>>"$log_file"; then
    sh "$tmp_script" >"$log_file" 2>&1 || true
  fi

  # 方式 2：官方脚本 + 国内镜像兜底
  if ! docker_bin_ok; then
    info "正在尝试方式 2/4：Docker 安装脚本（阿里云镜像）..."
    if [ -s "$tmp_script" ] && sh -n "$tmp_script" 2>/dev/null; then
      sh "$tmp_script" --mirror Aliyun >"$log_file" 2>&1 || true
    fi
  fi

  # 方式 3：发行版自带软件源
  if ! docker_bin_ok; then
    info "正在尝试方式 3/4：系统软件源安装 Docker..."
    case "$mgr" in
      apt) pkg_install docker.io docker-compose-v2 >"$log_file" 2>&1 || pkg_install docker.io >"$log_file" 2>&1 || true ;;
      dnf|yum) pkg_install docker >"$log_file" 2>&1 || true ;;
      apk) pkg_install docker docker-cli-compose >"$log_file" 2>&1 || true ;;
    esac
  fi

  # 方式 4：包元数据已装但二进制缺失时强制重装
  if ! docker_bin_ok && [ "$mgr" = "apt" ]; then
    info "正在尝试方式 4/4：修复重装 Docker 软件包..."
    DEBIAN_FRONTEND=noninteractive apt-get install --reinstall -y -qq docker-ce docker-ce-cli containerd.io >"$log_file" 2>&1 \
      || DEBIAN_FRONTEND=noninteractive apt-get install --reinstall -y -qq docker.io >"$log_file" 2>&1 || true
  fi

  rm -f "$tmp_script"
  if ! docker_bin_ok; then
    err "自动安装 Docker 失败，最近安装日志如下："
    tail -n 20 "$log_file" | sed 's/^/    /' >&2 || true
    rm -f "$log_file"
    die "请先手工安装 Docker 后再重试（官方命令：curl -fsSL https://get.docker.com | sh）"
  fi
  rm -f "$log_file"
}

install_compose_plugin_auto() {
  if compose_ok; then
    return 0
  fi
  local mgr
  mgr="$(detect_pkg_mgr)"
  info "正在安装 Docker Compose v2 插件..."
  case "$mgr" in
    apt) pkg_install docker-compose-plugin >/dev/null 2>&1 || pkg_install docker-compose-v2 >/dev/null 2>&1 || true ;;
    dnf|yum) pkg_install docker-compose-plugin >/dev/null 2>&1 || true ;;
    apk) pkg_install docker-cli-compose >/dev/null 2>&1 || true ;;
  esac
  if compose_ok; then
    return 0
  fi

  local arch url_arch plugin_dir plugin_bin tmp_bin
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) url_arch="x86_64" ;;
    aarch64|arm64) url_arch="aarch64" ;;
    armv7l) url_arch="armv7" ;;
    *) die "未找到 Docker Compose v2 插件且当前架构 ($arch) 不支持自动下载，请手工安装 docker-compose-plugin" ;;
  esac
  plugin_dir="/usr/local/lib/docker/cli-plugins"
  plugin_bin="${plugin_dir}/docker-compose"
  tmp_bin="$(mktemp /tmp/teyir-compose.XXXXXX)"
  local ver
  ver="$(curl -fsSL --max-time 15 https://api.github.com/repos/docker/compose/releases/latest 2>/dev/null \
    | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1 || true)"
  ver="${ver:-v2.32.4}"
  mkdir -p "$plugin_dir"
  if curl -fsSL --proto '=https' --tlsv1.2 --max-time 60 \
    "https://github.com/docker/compose/releases/download/${ver}/docker-compose-linux-${url_arch}" \
    -o "$tmp_bin"; then
    chmod 755 "$tmp_bin"
    mv -f "$tmp_bin" "$plugin_bin"
  else
    rm -f "$tmp_bin"
  fi
  compose_ok || die "自动安装 Docker Compose v2 插件失败，请手工安装后重试"
}

check_system_resources() {
  local mem_kb mem_mb disk_kb disk_gb
  if [ -r /proc/meminfo ]; then
    mem_kb="$(awk '/^MemTotal:/{print $2}' /proc/meminfo 2>/dev/null || true)"
    if printf '%s' "$mem_kb" | grep -qE '^[0-9]+$'; then
      mem_mb=$((mem_kb / 1024))
      if [ "$mem_mb" -lt 1536 ]; then
        warn "检测到本机内存较小（约 ${mem_mb} MiB），首次编译前后端镜像可能因内存不足失败。"
        dim "建议补充 2G 虚拟内存（Swap）命令："
        dim "  fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile"
      else
        ok "内存检测：约 ${mem_mb} MiB"
      fi
    fi
  fi
  local check_dir="$INSTALL_DIR"
  while [ ! -d "$check_dir" ] && [ "$check_dir" != "/" ]; do
    check_dir="$(dirname "$check_dir")"
  done
  disk_kb="$(df -P -k "$check_dir" 2>/dev/null | awk 'NR==2{print $4}' || true)"
  if printf '%s' "$disk_kb" | grep -qE '^[0-9]+$'; then
    disk_gb=$((disk_kb / 1024 / 1024))
    if [ "$disk_gb" -lt 5 ]; then
      warn "目标磁盘可用空间偏低（约 ${disk_gb} GiB），首次构建镜像建议预留至少 5 GiB 空间。"
    else
      ok "磁盘可用空间：约 ${disk_gb} GiB"
    fi
  fi
}

ensure_dependencies() {
  step "检查运行环境与必要依赖"
  check_system_resources

  if [ "$DRY_RUN" = "1" ]; then
    ok "[预演] 将检查并确保 curl / git / openssl / docker / docker compose 就绪"
    return 0
  fi

  local missing_pkgs=()
  if ! command -v curl >/dev/null 2>&1; then missing_pkgs+=(curl); fi
  if ! command -v git >/dev/null 2>&1; then missing_pkgs+=(git); fi
  if ! command -v openssl >/dev/null 2>&1; then missing_pkgs+=(openssl); fi

  if [ "${#missing_pkgs[@]}" -gt 0 ]; then
    if [ "$NO_DEPS" = "1" ]; then
      die "缺少必要工具：${missing_pkgs[*]}（已指定 --no-deps，请先用系统包管理器安装后再重试）"
    fi
    info "正在自动安装基础工具：${missing_pkgs[*]}..."
    pkg_install "${missing_pkgs[@]}" >/dev/null 2>&1 || warn "部分基础工具自动安装未成功，将在后续步骤按需检查"
  fi

  command -v curl >/dev/null 2>&1 || die "缺少 curl，请先安装 curl 后重试"

  if ! docker_bin_ok; then
    if [ "$NO_DEPS" = "1" ]; then
      err "这台机器尚未安装 Docker（已启用 --no-deps，未自动安装）。"
      info "可直接执行以下命令安装后重试：curl -fsSL https://get.docker.com | sh"
      exit 1
    fi
    install_docker_auto
  fi
  ok "Docker 已就绪（$(docker --version 2>/dev/null | head -1)）"

  if ! ensure_docker_service_running; then
    err "Docker 守护进程未运行且自动拉起失败。"
    info "请检查服务日志：systemctl status docker --no-pager"
    exit 1
  fi

  if ! compose_ok; then
    if [ "$NO_DEPS" = "1" ]; then
      die "缺少 Docker Compose v2 插件（已启用 --no-deps，请安装 docker-compose-plugin 后重试）"
    fi
    install_compose_plugin_auto
  fi
  ok "Docker Compose 已就绪（$(docker compose version 2>/dev/null | head -1)）"
}

# ── 端口扫描、自动避让与外层反代探测 ───────────────────────────────────────────
load_used_ports() {
  if [ -n "$USED_PORTS_CACHE" ]; then
    return 0
  fi
  USED_PORTS_CACHE="$({
    if command -v ss >/dev/null 2>&1; then
      ss -ltnH 2>/dev/null | awk '{print $4}' | sed 's/.*://'
    elif command -v netstat >/dev/null 2>&1; then
      netstat -ltn 2>/dev/null | awk 'NR>2{print $4}' | sed 's/.*://'
    fi
    if docker_bin_ok && docker_daemon_ok; then
      docker ps --format '{{.Ports}}' 2>/dev/null | tr ',' '\n' \
        | sed -n 's/.*:\([0-9][0-9]*\)->.*/\1/p'
    fi
  } | grep -E '^[0-9]+$' | sort -nu || true)"
}

own_project_holds_port() {
  local p="$1"
  if ! docker_bin_ok || ! docker_daemon_ok; then
    return 1
  fi
  docker ps --filter "label=com.docker.compose.project=${PROJECT_NAME}" --format '{{.Ports}}' 2>/dev/null \
    | tr ',' '\n' | sed -n 's/.*:\([0-9][0-9]*\)->.*/\1/p' | grep -qxF -e "$p"
}

is_port_used() {
  local p="$1"
  local extra_exclude="${2:-}"
  if [ -n "$extra_exclude" ]; then
    case ",${extra_exclude}," in
      *",${p},"*) return 0 ;;
    esac
  fi
  if [ -n "$SAVED_WEB_PORT" ] && [ "$p" = "$SAVED_WEB_PORT" ] && own_project_holds_port "$p"; then
    return 1
  fi
  if [ -n "$SAVED_WEB_TLS_PORT" ] && [ "$p" = "$SAVED_WEB_TLS_PORT" ] && own_project_holds_port "$p"; then
    return 1
  fi
  if [ -n "$SAVED_API_PORT" ] && [ "$p" = "$SAVED_API_PORT" ] && own_project_holds_port "$p"; then
    return 1
  fi
  load_used_ports
  printf '%s\n' "$USED_PORTS_CACHE" | grep -qxF -e "$p"
}

pick_free_port() {
  local start="$1"
  local exclude="${2:-}"
  local p="$start"
  local tries=0
  while [ "$tries" -lt 200 ] && [ "$p" -le 65000 ]; do
    if ! is_port_used "$p" "$exclude"; then
      printf '%s' "$p"
      return 0
    fi
    p=$((p + 1))
    tries=$((tries + 1))
  done
  die "从端口 $start 起连续 200 个端口均被占用，请手工通过参数指定可用端口"
}

detect_public_ip() {
  if [ -n "$PUBLIC_IP_CACHE" ]; then
    printf '%s' "$PUBLIC_IP_CACHE"
    return 0
  fi
  local u ip=""
  for u in https://api.ipify.org https://ifconfig.me/ip https://ipv4.icanhazip.com; do
    ip="$(curl -fsS --max-time 6 "$u" 2>/dev/null | tr -d '[:space:]' || true)"
    if printf '%s' "$ip" | grep -qE '^([0-9]{1,3}\.){3}[0-9]{1,3}$'; then
      PUBLIC_IP_CACHE="$ip"
      printf '%s' "$ip"
      return 0
    fi
  done
  printf ''
}

detect_host_caddyfile() {
  local f=""
  if command -v systemctl >/dev/null 2>&1; then
    f="$(systemctl cat caddy 2>/dev/null | sed -n 's/.*--config[= ]\([^[:space:]]*\).*/\1/p' | head -1 || true)"
  fi
  if [ -n "$f" ] && [ -f "$f" ]; then
    printf '%s' "$f"; return 0
  fi
  for f in /etc/caddy/Caddyfile /usr/local/etc/caddy/Caddyfile; do
    if [ -f "$f" ]; then
      printf '%s' "$f"; return 0
    fi
  done
  printf ''
}

detect_existing_proxy() {
  DETECTED_PROXY_TYPE="none"
  DETECTED_PROXY_PROC=""
  CADDY_CONTAINER=""
  CADDY_FILE=""
  CADDY_INNER=""
  CADDY_NET=""
  UPSTREAM_HOST="127.0.0.1"

  # 1) 检查是否存在已占用 80/443 的容器（排除本实例自己的 web 容器）
  if docker_bin_ok && docker_daemon_ok; then
    local cname cimg cproj cports
    while IFS='|' read -r cname cimg cproj cports; do
      if [ -z "$cname" ]; then continue; fi
      if [ "$cproj" = "$PROJECT_NAME" ]; then continue; fi
      if printf '%s' "$cports" | grep -qE ':(80|443)->'; then
        if printf '%s %s' "$cimg" "$cname" | grep -qi 'caddy'; then
          DETECTED_PROXY_TYPE="docker-caddy"
          CADDY_CONTAINER="$cname"
          CADDY_FILE="$(docker inspect "$cname" --format '{{range .Mounts}}{{if eq .Destination "/etc/caddy/Caddyfile"}}{{.Source}}{{end}}{{end}}' 2>/dev/null || true)"
          CADDY_INNER="/etc/caddy/Caddyfile"
          if [ -z "$CADDY_FILE" ]; then
            local mdir
            mdir="$(docker inspect "$cname" --format '{{range .Mounts}}{{if eq .Destination "/etc/caddy"}}{{.Source}}{{end}}{{end}}' 2>/dev/null || true)"
            if [ -n "$mdir" ] && [ -f "$mdir/Caddyfile" ]; then
              CADDY_FILE="$mdir/Caddyfile"
            fi
          fi
          CADDY_NET="$(docker inspect "$cname" --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>/dev/null | awk '{print $1}' || true)"
          if [ -n "$CADDY_NET" ]; then
            UPSTREAM_HOST="$(docker network inspect "$CADDY_NET" --format '{{range .IPAM.Config}}{{.Gateway}}{{end}}' 2>/dev/null | awk '{print $1}' || true)"
          fi
          if [ -z "$UPSTREAM_HOST" ]; then
            UPSTREAM_HOST="172.17.0.1"
          fi
          return 0
        elif printf '%s %s' "$cimg" "$cname" | grep -qi 'nginx'; then
          DETECTED_PROXY_TYPE="nginx"
          DETECTED_PROXY_PROC="容器 $cname"
          return 0
        else
          DETECTED_PROXY_TYPE="other"
          DETECTED_PROXY_PROC="容器 $cname ($cimg)"
          return 0
        fi
      fi
    done < <(docker ps --format '{{.Names}}|{{.Image}}|{{.Label "com.docker.compose.project"}}|{{.Ports}}' 2>/dev/null || true)
  fi

  # 2) 检查宿主机 80/443 监听进程
  if command -v ss >/dev/null 2>&1; then
    local listen_lines
    listen_lines="$(ss -ltnpH '( sport = :80 or sport = :443 )' 2>/dev/null || true)"
    if [ -n "$listen_lines" ]; then
      if ! own_project_holds_port 80 && ! own_project_holds_port 443; then
        if printf '%s' "$listen_lines" | grep -qi 'caddy'; then
          DETECTED_PROXY_TYPE="host-caddy"
          CADDY_FILE="$(detect_host_caddyfile)"
          CADDY_INNER="$CADDY_FILE"
          UPSTREAM_HOST="127.0.0.1"
          return 0
        elif printf '%s' "$listen_lines" | grep -qi 'nginx'; then
          DETECTED_PROXY_TYPE="nginx"
          DETECTED_PROXY_PROC="宿主机 Nginx"
          return 0
        else
          DETECTED_PROXY_TYPE="other"
          DETECTED_PROXY_PROC="$(printf '%s\n' "$listen_lines" | sed -n 's/.*users:(("\([^"]*\)".*/\1/p' | head -1 || true)"
          if [ -z "$DETECTED_PROXY_PROC" ]; then
            DETECTED_PROXY_PROC="未知进程"
          fi
          return 0
        fi
      fi
    fi
  fi
}

# ── 共享 Caddyfile 安全管理（冲突检测 → 备份 → 标记块更新 → 校验 → 重载）───────
strip_managed_block_to_stdout() {
  local file="$1"
  awk -v b="$MARK_BEGIN" -v e="$MARK_END" '
    $0 == b { skip=1; next }
    $0 == e { skip=0; next }
    !skip { print }
  ' "$file"
}

caddyfile_has_domain_conflict() {
  local file="$1" dom="$2"
  [ -f "$file" ] || return 1
  local dom_re="${dom//./\\.}"
  local cleaned
  cleaned="$(strip_managed_block_to_stdout "$file")"
  printf '%s\n' "$cleaned" | grep -Eq "^[[:space:]]*(https?://)?${dom_re}([[:space:],:{]|$)"
}

validate_external_caddy() {
  case "$CADDY_MODE" in
    host-caddy)
      command -v caddy >/dev/null 2>&1 || return 1
      caddy validate --config "$CADDY_FILE" --adapter caddyfile
      ;;
    docker-caddy)
      docker exec "$CADDY_CONTAINER" caddy validate --config "$CADDY_INNER" --adapter caddyfile
      ;;
    *) return 0 ;;
  esac
}

reload_external_caddy() {
  case "$CADDY_MODE" in
    host-caddy)
      if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet caddy 2>/dev/null; then
        systemctl reload caddy
      else
        caddy reload --config "$CADDY_FILE" --adapter caddyfile
      fi
      ;;
    docker-caddy)
      docker exec "$CADDY_CONTAINER" caddy reload --config "$CADDY_INNER" --adapter caddyfile
      ;;
    *) return 0 ;;
  esac
}

gen_external_caddy_block() {
  local dom="$1" upstream="$2"
  local now
  now="$(date '+%Y-%m-%d %H:%M:%S')"
  cat <<EOF

${MARK_BEGIN}
# 生成时间：${now} | 项目：${PROJECT_NAME} | 上游：${upstream}
${dom} {
	encode zstd gzip
	log {
		output stdout
		format filter {
			wrap console
			fields {
				request>uri replace "[redacted-request-uri]"
				request>headers>Referer delete
			}
		}
	}
	reverse_proxy ${upstream} {
		transport http {
			read_timeout 60s
		}
		header_up X-Forwarded-For {http.request.remote.host}
		header_up X-Forwarded-Proto {http.request.scheme}
	}
}
${MARK_END}
EOF
}

apply_external_caddy() {
  if [ "$CADDY_MODE" != "host-caddy" ] && [ "$CADDY_MODE" != "docker-caddy" ]; then
    return 0
  fi
  [ -n "$CADDY_FILE" ] && [ -f "$CADDY_FILE" ] || die "找不到外层 Caddy 配置文件：$CADDY_FILE"

  if caddyfile_has_domain_conflict "$CADDY_FILE" "$DOMAIN"; then
    die "外层反代配置 $CADDY_FILE 中已存在域名 $DOMAIN 的其他站点块，为避免冲突已中止（请先清理或更换域名）"
  fi

  local upstream="${UPSTREAM_HOST}:${WEB_PORT}"
  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将备份 $CADDY_FILE，写入域名 $DOMAIN -> $upstream 站点块，执行 caddy validate 与热重载"
    return 0
  fi

  step "配置共享 Caddy 反代（${CADDY_MODE}：${DOMAIN} -> ${upstream}）"
  CADDY_BACKUP_FILE="${CADDY_FILE}.bak-$(date +%Y%m%d-%H%M%S)-preTeyir"
  cp "$CADDY_FILE" "$CADDY_BACKUP_FILE"
  CADDY_BACKED_UP=1
  ok "已备份原配置：$CADDY_BACKUP_FILE"

  local tmp
  tmp="$(mktemp /tmp/teyir-caddyfile.XXXXXX)"
  strip_managed_block_to_stdout "$CADDY_FILE" > "$tmp"
  gen_external_caddy_block "$DOMAIN" "$upstream" >> "$tmp"
  cat "$tmp" > "$CADDY_FILE"
  rm -f "$tmp"

  if ! validate_external_caddy >/dev/null 2>&1; then
    err "Caddy 配置语法校验失败，详情如下："
    validate_external_caddy >&2 || true
    restore_caddy
    die "共享反代配置校验未通过，已自动回滚到修改前状态"
  fi
  ok "Caddy 配置校验通过"

  if ! reload_external_caddy >/dev/null 2>&1; then
    err "Caddy 热重载失败，正在自动回滚..."
    restore_caddy
    die "共享反代热重载失败，已自动回滚到修改前状态"
  fi
  CADDY_BACKED_UP=0
  prune_caddy_backups "$CADDY_FILE"
  ok "共享 Caddy 已热重载生效（未中断现有站点）"
}

remove_external_caddy_block() {
  local target_mode="${1:-$SAVED_CADDY_MODE}"
  local target_file="${2:-$SAVED_CADDY_FILE}"
  local target_container="${3:-$SAVED_CADDY_CONTAINER}"
  local target_inner="${4:-$SAVED_CADDY_INNER}"

  if [ "$target_mode" != "host-caddy" ] && [ "$target_mode" != "docker-caddy" ]; then
    return 0
  fi
  if [ -z "$target_file" ] || [ ! -f "$target_file" ]; then
    return 0
  fi
  if ! grep -qF -e "$MARK_BEGIN" "$target_file" 2>/dev/null; then
    return 0
  fi

  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将从 $target_file 摘除本脚本维护的反代站点块并重载 Caddy"
    return 0
  fi

  local old_mode="$CADDY_MODE" old_file="$CADDY_FILE" old_container="$CADDY_CONTAINER" old_inner="$CADDY_INNER"
  CADDY_MODE="$target_mode"
  CADDY_FILE="$target_file"
  CADDY_CONTAINER="$target_container"
  CADDY_INNER="$target_inner"

  CADDY_BACKUP_FILE="${CADDY_FILE}.bak-$(date +%Y%m%d-%H%M%S)-preTeyir"
  cp "$CADDY_FILE" "$CADDY_BACKUP_FILE"
  CADDY_BACKED_UP=1

  local tmp
  tmp="$(mktemp /tmp/teyir-caddyfile-rm.XXXXXX)"
  strip_managed_block_to_stdout "$CADDY_FILE" > "$tmp"
  cat "$tmp" > "$CADDY_FILE"
  rm -f "$tmp"

  if validate_external_caddy >/dev/null 2>&1 && reload_external_caddy >/dev/null 2>&1; then
    CADDY_BACKED_UP=0
    prune_caddy_backups "$CADDY_FILE"
    ok "已从共享反代 $CADDY_FILE 摘除站点块并热重载"
  else
    restore_caddy
    warn "摘除共享反代站点块后重载失败，已自动还原原配置，请手工检查 $CADDY_FILE"
  fi

  CADDY_MODE="$old_mode"
  CADDY_FILE="$old_file"
  CADDY_CONTAINER="$old_container"
  CADDY_INNER="$old_inner"
}

check_dns_before_ssl() {
  if [ -z "$DOMAIN" ]; then return 0; fi
  if [ "$CADDY_MODE" = "none" ]; then return 0; fi
  if [ "$SKIP_DNS_CHECK" = "1" ]; then
    warn "已按 --skip-dns-check 跳过域名解析预检"
    return 0
  fi
  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将核对 $DOMAIN 解析结果与本机公网 IP"
    return 0
  fi

  local pub_ip resolved=""
  pub_ip="$(detect_public_ip)"
  if command -v getent >/dev/null 2>&1; then
    resolved="$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ' | sed 's/[[:space:]]*$//' || true)"
  elif command -v dig >/dev/null 2>&1; then
    resolved="$(dig +short A "$DOMAIN" 2>/dev/null | grep -E '^[0-9.]+$' | tr '\n' ' ' | sed 's/[[:space:]]*$//' || true)"
  fi

  if [ -z "$resolved" ]; then
    warn "尚未查到域名 $DOMAIN 的 A 记录（自动申请 HTTPS 证书需要域名指向本机公网 IP：${pub_ip:-未知}）"
    if [ "$ASSUME_YES" = "1" ]; then
      die "非交互模式下域名未解析，已中止。如使用 CDN 回源或稍后再配置解析，请加 --skip-dns-check 重试"
    fi
    if ! ask_yn "域名尚未解析，仍要继续安装吗？（可稍后配好解析再自动签发）" "n"; then
      die "已中止安装，请先在域名服务商处添加 A 记录指向 ${pub_ip:-本机公网 IP} 后重试"
    fi
    return 0
  fi

  if [ -n "$pub_ip" ] && ! printf ' %s ' "$resolved" | grep -qF -e " $pub_ip "; then
    warn "域名 $DOMAIN 当前解析为 [$resolved]，与本机公网 IP [$pub_ip] 不一致（若开启了 Cloudflare 代理/CDN 属正常现象）"
    if [ "$ASSUME_YES" = "1" ]; then
      warn "非交互模式继续执行（如证书申请失败，请确认回源与 80/443 放行，或用 --skip-dns-check）"
    elif ! ask_yn "确认域名解析配置正确并继续安装吗？" "y"; then
      die "已中止安装，请核对域名解析后重试"
    fi
  else
    ok "域名解析核对通过：$DOMAIN -> ${resolved}"
  fi
}

print_nginx_snippet() {
  local dom="${DOMAIN:-panel.example.com}"
  say ""
  warn "检测到 80/443 已被 Nginx 占用。为避免误改您现有生产站点，本脚本不会自动修改 Nginx 配置。"
  info "Teyir 控制台已监听在本机回环 127.0.0.1:${WEB_PORT}，您只需将下方片段加入 Nginx 站点配置即可："
  cat >&2 <<EOF

  server {
      listen 80;
      server_name ${dom};
      client_max_body_size 20m;

      location / {
          proxy_pass http://127.0.0.1:${WEB_PORT};
          proxy_http_version 1.1;
          proxy_set_header Host \$host;
          proxy_set_header X-Real-IP \$remote_addr;
          proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
          proxy_set_header X-Forwarded-Proto \$scheme;
          proxy_set_header Upgrade \$http_upgrade;
          proxy_set_header Connection "upgrade";
          proxy_buffering off;
          proxy_read_timeout 300s;
      }
  }

  # 申请免费 HTTPS 证书命令示例：
  #   sudo certbot --nginx -d ${dom}
EOF
}

# ── 源码拉取（统一使用 APP_REF，支持现有仓库复用 + Git 克隆 + Tarball 兜底）─────
prepare_source_dir() {
  step "准备源码目录：$INSTALL_DIR（分支：${APP_REF}）"
  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将复用或从 https://github.com/${APP_REPO} (分支 ${APP_REF}) 准备源码至 $INSTALL_DIR"
    return 0
  fi

  mkdir -p "$(dirname "$INSTALL_DIR")"

  if [ -f "$INSTALL_DIR/docker-compose.yml" ] && [ -f "$INSTALL_DIR/Dockerfile.api" ]; then
    ok "检测到已有源码目录，直接复用：$INSTALL_DIR"
    return 0
  fi

  if [ -d "$INSTALL_DIR" ] && [ -n "$(ls -A "$INSTALL_DIR" 2>/dev/null || true)" ]; then
    die "目标目录 $INSTALL_DIR 非空且不是有效的 teyir-console 源码目录，请通过 --dir 更换空目录"
  fi

  if command -v git >/dev/null 2>&1; then
    info "正在从 GitHub 克隆源码（分支：${APP_REF}）..."
    if git clone --depth 1 --branch "$APP_REF" "https://github.com/${APP_REPO}.git" "$INSTALL_DIR" >/dev/null 2>&1; then
      ok "源码克隆完成（版本：$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)）"
      return 0
    fi
    warn "Git 克隆失败，自动切换压缩包兜底下载..."
    rm -rf "$INSTALL_DIR"
  fi

  local tar_url="https://codeload.github.com/${APP_REPO}/tar.gz/refs/heads/${APP_REF}"
  local tmp_tar tmp_extract top_dir
  tmp_tar="$(mktemp /tmp/teyir-src.XXXXXX.tar.gz)"
  tmp_extract="$(mktemp -d /tmp/teyir-extract.XXXXXX)"
  if curl -fsSL --proto '=https' --tlsv1.2 --max-time 60 "$tar_url" -o "$tmp_tar" && [ -s "$tmp_tar" ]; then
    tar -xzf "$tmp_tar" -C "$tmp_extract"
    top_dir="$(find "$tmp_extract" -maxdepth 1 -mindepth 1 -type d | head -1 || true)"
    if [ -n "$top_dir" ] && [ -f "$top_dir/docker-compose.yml" ]; then
      mkdir -p "$INSTALL_DIR"
      (cd "$top_dir" && shopt -s dotglob nullglob && mv -- * "$INSTALL_DIR/")
      rm -rf "$tmp_tar" "$tmp_extract"
      ok "源码压缩包下载并解包完成"
      return 0
    fi
  fi
  rm -rf "$tmp_tar" "$tmp_extract"
  die "无法从 GitHub 下载 ${APP_REPO}（分支 ${APP_REF}），请检查服务器网络连通性后重试"
}

# ── 生成与安全保留 .env 配置（绝不覆盖既有核心密钥与数据库密码）────────────────
write_env_file() {
  local env_file="$INSTALL_DIR/.env"
  local pg_pass="" jwt_sec="" cred_key="" seed_email="" seed_pass=""
  local old_sub_backup="" old_turnstile_site="" old_turnstile_sec="" old_login_key="" old_login_email=""
  local old_ban_url="" old_ban_tok="" old_geo_api="" old_geo_url="" old_purity_key="" old_purity_ipqs=""
  local old_portal_pins="" old_viewer_restrict="" old_speedtest="" old_collector_sec="" old_executor_sec=""
  local old_jwt_ttl="" old_store_renew="" old_hy2_script="" old_hy2_repo=""

  EXISTING_ENV_DETECTED=0
  IS_NEW_ADMIN_SEEDED=0

  if [ -f "$env_file" ]; then
    EXISTING_ENV_DETECTED=1
    pg_pass="$(read_kv_file "$env_file" POSTGRES_PASSWORD)"
    jwt_sec="$(read_kv_file "$env_file" JWT_SECRET)"
    cred_key="$(read_kv_file "$env_file" CREDENTIAL_KEY)"
    seed_email="$(read_kv_file "$env_file" SEED_ADMIN_EMAIL)"
    seed_pass="$(read_kv_file "$env_file" SEED_ADMIN_PASSWORD)"
    old_sub_backup="$(read_kv_file "$env_file" SUBSCRIPTION_BACKUP_BASE_URLS)"
    old_turnstile_site="$(read_kv_file "$env_file" TURNSTILE_SITE_KEY)"
    old_turnstile_sec="$(read_kv_file "$env_file" TURNSTILE_SECRET)"
    old_login_key="$(read_kv_file "$env_file" LOGIN_SERVICE_KEY)"
    old_login_email="$(read_kv_file "$env_file" LOGIN_SERVICE_EMAIL)"
    old_ban_url="$(read_kv_file "$env_file" BAN_WEBHOOK_URL)"
    old_ban_tok="$(read_kv_file "$env_file" BAN_WEBHOOK_TOKEN)"
    old_geo_api="$(read_kv_file "$env_file" GEO_ENABLE_API)"
    old_geo_url="$(read_kv_file "$env_file" GEO_API_BASE_URL)"
    old_purity_key="$(read_kv_file "$env_file" IP_PURITY_API_KEY)"
    old_purity_ipqs="$(read_kv_file "$env_file" IP_PURITY_IPQS_KEY)"
    old_portal_pins="$(read_kv_file "$env_file" PORTAL_TLS_PINS)"
    old_viewer_restrict="$(read_kv_file "$env_file" VIEWER_RESTRICT_SUBSCRIPTIONS)"
    old_speedtest="$(read_kv_file "$env_file" SPEEDTEST_INTERVAL_MINUTES)"
    old_collector_sec="$(read_kv_file "$env_file" COLLECTOR_INTERVAL_SECONDS)"
    old_executor_sec="$(read_kv_file "$env_file" EXECUTOR_INTERVAL_SECONDS)"
    old_jwt_ttl="$(read_kv_file "$env_file" JWT_TTL_MINUTES)"
    old_store_renew="$(read_kv_file "$env_file" STORE_RENEW_BASE_URL)"
    old_hy2_script="$(read_kv_file "$env_file" HY2_INSTALL_SCRIPT_URL)"
    old_hy2_repo="$(read_kv_file "$env_file" HY2_INSTALLER_REPO)"
  fi

  if [ -z "$pg_pass" ] || [ "$pg_pass" = "replace-with-a-long-random-value" ]; then
    pg_pass="$(gen_urlsafe_secret 40)"
  fi
  if [ -z "$jwt_sec" ] || [ "${#jwt_sec}" -lt 32 ] || [ "$jwt_sec" = "replace-with-a-random-secret-of-at-least-32-characters" ]; then
    jwt_sec="$(gen_urlsafe_secret 56)"
  fi
  if [ -z "$cred_key" ] || [ "$cred_key" = "base64-of-32-random-bytes" ]; then
    cred_key="$(gen_credential_key)"
  fi

  if [ -n "$ADMIN_EMAIL" ]; then
    seed_email="$ADMIN_EMAIL"
  elif [ -z "$seed_email" ] || [ "$seed_email" = "admin@example.invalid" ]; then
    seed_email="$DEFAULT_ADMIN_EMAIL"
  fi
  if [ -n "$ADMIN_PASSWORD" ]; then
    seed_pass="$ADMIN_PASSWORD"
    IS_NEW_ADMIN_SEEDED=1
  elif [ -z "$seed_pass" ] || [ "${#seed_pass}" -lt 12 ] || [ "$seed_pass" = "replace-with-a-unique-long-password" ]; then
    seed_pass="$(gen_alnum_password 16)"
    IS_NEW_ADMIN_SEEDED=1
  fi
  ADMIN_EMAIL="$seed_email"
  ADMIN_PASSWORD="$seed_pass"

  local renew_val="${STORE_RENEW_BASE_URL:-$old_store_renew}"
  local hy2_script_val="${HY2_INSTALL_SCRIPT_URL:-$old_hy2_script}"
  local hy2_repo_val="${HY2_INSTALLER_REPO:-$old_hy2_repo}"

  local panel_public_url="" sub_public_url="" allowed_origin="http://localhost:5173"
  local site_line="# SITE_ADDRESS="
  local trusted_cidrs="127.0.0.1/32,::1/128,172.16.0.0/12,192.168.0.0/16,10.0.0.0/8"

  if [ -n "$DOMAIN" ]; then
    panel_public_url="https://${DOMAIN}"
    sub_public_url="https://${DOMAIN}"
    allowed_origin="https://${DOMAIN}"
    if [ "$ACCESS_MODE" = "self" ]; then
      site_line="SITE_ADDRESS=${DOMAIN}"
    fi
  fi

  local ingress_net="${PROJECT_NAME}-internal"

  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将生成 $env_file（权限 600）："
    dim "  ACCESS_MODE=${ACCESS_MODE} WEB_BIND=${WEB_BIND} WEB_PORT=${WEB_PORT} WEB_TLS_PORT=${WEB_TLS_PORT} API_BIND=${API_BIND} API_PORT=${API_PORT}"
    dim "  ${site_line} SUBSCRIPTION_DEFAULT_NAME=${SUB_NAME} ENABLE_COLLECTOR=${ENABLE_COLLECTOR}"
    return 0
  fi

  if [ -f "$env_file" ]; then
    local env_bak="${env_file}.bak-$(date +%Y%m%d-%H%M%S)"
    cp "$env_file" "$env_bak"
    chmod 600 "$env_bak"
    ls -1t "${env_file}.bak-"* 2>/dev/null | tail -n +4 | while IFS= read -r f; do
      if [ -n "$f" ]; then
        rm -f "$f" 2>/dev/null || true
      fi
    done
  fi

  local tmp="${env_file}.tmp.$$"
  cat > "$tmp" <<EOF
# ==============================================================================
# Teyir 控制台运行配置（由 install.sh 自动生成并维护，权限 600）
# ⚠️ 切勿修改已入库实例的 CREDENTIAL_KEY 与 POSTGRES_PASSWORD
# ==============================================================================
COMPOSE_PROJECT_NAME=${PROJECT_NAME}
WEB_INGRESS_NETWORK=${ingress_net}

# 数据库与核心密钥
POSTGRES_PASSWORD=${pg_pass}
JWT_SECRET=${jwt_sec}
JWT_TTL_MINUTES=${old_jwt_ttl:-720}
SEED_ADMIN_EMAIL=${seed_email}
SEED_ADMIN_PASSWORD=${seed_pass}
CREDENTIAL_KEY=${cred_key}

# 访问来源与监听端口
ALLOWED_ORIGIN=${allowed_origin}
TRUSTED_PROXY_CIDRS=${trusted_cidrs}
API_BIND=${API_BIND}
API_PORT=${API_PORT}
WEB_BIND=${WEB_BIND}
WEB_PORT=${WEB_PORT}
WEB_TLS_PORT=${WEB_TLS_PORT}

# 站点与 TLS（仅内置 Caddy 自动 HTTPS 时取消注释；留空必须保持注释态）
${site_line}

# 采集、执行与测速周期
ENABLE_COLLECTOR=${ENABLE_COLLECTOR}
COLLECTOR_INTERVAL_SECONDS=${old_collector_sec:-30}
EXECUTOR_INTERVAL_SECONDS=${old_executor_sec:-20}
SPEEDTEST_INTERVAL_MINUTES=${old_speedtest:-15}

# 地区识别与 IP 纯净度
GEO_OFFLINE_DB=/data/geo-offline.json
GEO_ENABLE_API=${old_geo_api:-true}
GEO_API_BASE_URL=${old_geo_url}
IP_PURITY_OFFLINE_DB=
IP_PURITY_API_KEY=${old_purity_key}
IP_PURITY_IPQS_KEY=${old_purity_ipqs}
IP_PURITY_API_BASE_URL=
PORTAL_TLS_PINS=${old_portal_pins}
VIEWER_RESTRICT_SUBSCRIPTIONS=${old_viewer_restrict:-false}

# 外部地址、订阅分组与可选联动
PANEL_PUBLIC_URL=${panel_public_url}
SUBSCRIPTION_PUBLIC_BASE_URL=${sub_public_url}
SUBSCRIPTION_BACKUP_BASE_URLS=${old_sub_backup}
SUBSCRIPTION_DEFAULT_NAME=${SUB_NAME}
STORE_RENEW_BASE_URL=${renew_val}
TURNSTILE_SITE_KEY=${old_turnstile_site}
TURNSTILE_SECRET=${old_turnstile_sec}
LOGIN_SERVICE_KEY=${old_login_key}
LOGIN_SERVICE_EMAIL=${old_login_email}
BAN_WEBHOOK_URL=${old_ban_url}
BAN_WEBHOOK_TOKEN=${old_ban_tok}
HY2_INSTALL_SCRIPT_URL=${hy2_script_val}
HY2_INSTALLER_REPO=${hy2_repo_val}
EOF
  chmod 600 "$tmp"
  mv -f "$tmp" "$env_file"
  mkdir -p "$INSTALL_DIR/geo-data"
  ok "已写入运行配置：$env_file（保留既有密钥并设置 600 权限）"
}

# ── Docker Compose 操作与 ip_forward=0 兼容构建 ────────────────────────────────
dc() {
  COMPOSE_PROJECT_NAME="$PROJECT_NAME" docker compose \
    -f "$INSTALL_DIR/docker-compose.yml" \
    --env-file "$INSTALL_DIR/.env" "$@" </dev/null
}

should_use_host_build_network() {
  case "$BUILD_NETWORK" in
    host) return 0 ;;
    default) return 1 ;;
    auto)
      if [ -r /proc/sys/net/ipv4/ip_forward ] && [ "$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null || echo 1)" = "0" ]; then
        return 0
      fi
      return 1
      ;;
  esac
}

dc_build() {
  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] docker compose build $*"
    return 0
  fi
  if should_use_host_build_network; then
    local ov
    ov="$(mktemp /tmp/teyir-build-net.XXXXXX.yml)"
    cat > "$ov" <<'EOF'
services:
  migrate:
    build:
      network: host
  api:
    build:
      network: host
  web:
    build:
      network: host
EOF
    local rc=0
    COMPOSE_PROJECT_NAME="$PROJECT_NAME" docker compose \
      -f "$INSTALL_DIR/docker-compose.yml" \
      -f "$ov" \
      --env-file "$INSTALL_DIR/.env" \
      build "$@" || rc=$?
    rm -f "$ov"
    return "$rc"
  fi
  dc build "$@"
}

resolve_health_host() {
  local bind="$1"
  case "$bind" in
    ""|"0.0.0.0"|"::"|"[::]"|"*") printf '127.0.0.1' ;;
    \[*\]) printf '%s' "$bind" ;;
    *:*) printf '[%s]' "$bind" ;;
    *) printf '%s' "$bind" ;;
  esac
}

classify_compose_error() {
  local out="$1"
  case "$out" in
    *"address already in use"*|*"port is already allocated"*|*"Bind for"*)
      LAST_FAIL_KIND="port" ;;
    *"is already in use"*|*"Conflict. The container name"*)
      LAST_FAIL_KIND="name" ;;
    *"failed to solve"*|*"executor failed"*|*"Dockerfile"*)
      LAST_FAIL_KIND="build" ;;
    *)
      LAST_FAIL_KIND="other" ;;
  esac
}

start_and_verify_stack() {
  local need_build="${1:-1}"
  step "构建镜像、执行数据库迁移并启动服务"
  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 启动 db → 执行 migrate → 启动 api 与 web → 容器内部与端口健康验收"
    return 0
  fi

  local out=""
  if [ "$need_build" = "1" ]; then
    if should_use_host_build_network; then
      info "检测到宿主机未开启 IPv4 转发（ip_forward=0），构建阶段自动使用宿主机网络模式..."
    fi
    info "正在构建 migrate / api / web 镜像（首次需下载 Go/Node 依赖并编译，约 2–5 分钟，请耐心等待）..."
    if ! dc_build migrate api web; then
      LAST_FAIL_KIND="build"
      die "镜像构建失败，请查看上方编译输出（若是小内存主机可先增加 2G Swap 后重试）"
    fi
    ok "镜像构建完成"
  fi

  info "启动数据库并等待 PostgreSQL 就绪..."
  if ! out="$(dc up -d db 2>&1)"; then
    printf '%s\n' "$out" | tail -n 15 | sed 's/^/    /' >&2
    classify_compose_error "$out"
    return 1
  fi
  COMPOSE_STARTED=1

  local db_ready=0 i
  for i in $(seq 1 30); do
    if dc exec -T db pg_isready -U nodeops -d nodeops >/dev/null 2>&1; then
      db_ready=1
      break
    fi
    sleep 2
  done
  if [ "$db_ready" -ne 1 ]; then
    err "等待数据库就绪超时（60 秒），最近日志："
    dc logs --tail=30 db >&2 || true
    LAST_FAIL_KIND="health"
    return 1
  fi
  ok "PostgreSQL 数据库已就绪"

  info "执行幂等数据库迁移..."
  if ! dc run --rm migrate; then
    err "数据库迁移执行失败"
    LAST_FAIL_KIND="other"
    return 1
  fi
  local mig_cnt
  mig_cnt="$(dc exec -T db psql -U nodeops -d nodeops -t -A -c 'SELECT count(*) FROM schema_migrations' 2>/dev/null | tr -d '[:space:]' || echo '?')"
  ok "数据库迁移完成（当前已应用 ${mig_cnt} 份迁移脚本）"

  info "启动后端 api 与前端 web 服务..."
  if ! out="$(dc up -d --no-build api web 2>&1)"; then
    printf '%s\n' "$out" | tail -n 15 | sed 's/^/    /' >&2
    classify_compose_error "$out"
    return 1
  fi

  # 容器内部真实验证（避免 docker-proxy 造成端口监听假通过）
  info "正在从容器内部验收 api 与 web 健康状态..."
  local api_ok=0 web_ok=0
  local api_host web_host
  api_host="$(resolve_health_host "$API_BIND")"
  web_host="$(resolve_health_host "$WEB_BIND")"

  for i in $(seq 1 45); do
    if dc exec -T api /api --healthcheck >/dev/null 2>&1; then
      api_ok=1
      break
    fi
    sleep 2
  done
  if [ "$api_ok" -ne 1 ]; then
    err "后端 api 容器健康检查超时，最近 40 行日志："
    dc logs --tail=40 api >&2 || true
    LAST_FAIL_KIND="health"
    return 1
  fi
  ok "后端 api 容器内部探活通过"

  for i in $(seq 1 20); do
    if dc exec -T web wget -qO- http://127.0.0.1:8081/healthz 2>/dev/null | grep -q 'ok'; then
      web_ok=1
      break
    fi
    sleep 2
  done
  if [ "$web_ok" -ne 1 ]; then
    err "前端 web 容器内部探活失败，最近 40 行日志："
    dc logs --tail=40 web >&2 || true
    LAST_FAIL_KIND="health"
    return 1
  fi
  ok "前端 web 容器内部探活通过（127.0.0.1:8081/healthz -> ok）"

  local api_code
  api_code="$(curl --noproxy '*' --max-time 5 -s -o /dev/null -w '%{http_code}' "http://${api_host}:${API_PORT}/api/health" || echo 000)"
  if [ "$api_code" = "200" ]; then
    ok "后端接口可达性核验通过（http://${api_host}:${API_PORT}/api/health -> 200）"
  else
    warn "后端宿主机端口返回 ${api_code}，请确认防火墙或监听地址 ${API_BIND}:${API_PORT}"
  fi

  if [ "$ACCESS_MODE" != "self" ]; then
    local web_api_code
    web_api_code="$(curl --noproxy '*' --max-time 5 -s -o /dev/null -w '%{http_code}' "http://${web_host}:${WEB_PORT}/api/health" || echo 000)"
    if [ "$web_api_code" = "200" ]; then
      ok "前端反代转发核验通过（http://${web_host}:${WEB_PORT}/api/health -> 200）"
    else
      warn "前端 HTTP 入口暂返回 ${web_api_code}（地址 http://${web_host}:${WEB_PORT}）"
    fi
  fi

  return 0
}

# ── 规划访问方式与自动分配端口 ─────────────────────────────────────────────────
plan_access_and_ports() {
  detect_existing_proxy

  if [ -z "$ACCESS_MODE" ] || [ "$ACCESS_MODE" = "auto" ]; then
    if [ -n "$DOMAIN" ]; then
      case "$DETECTED_PROXY_TYPE" in
        none)         ACCESS_MODE="self" ;;
        host-caddy)   ACCESS_MODE="host-caddy" ;;
        docker-caddy) ACCESS_MODE="docker-caddy" ;;
        nginx)        ACCESS_MODE="local" ;;
        other)
          die "80/443 已被 ${DETECTED_PROXY_PROC} 占用，无法自动绑定域名 ${DOMAIN}。您可以：① 停掉占用进程后重试；② 使用 --no-domain --access-mode local 仅监听回环并自行反代；③ 使用 --access-mode direct 走高位端口。"
          ;;
      esac
    else
      ACCESS_MODE="direct"
    fi
  fi

  case "$ACCESS_MODE" in
    self)
      [ -n "$DOMAIN" ] || die "--access-mode self 必须配合 --domain <域名> 使用"
      if [ "$DETECTED_PROXY_TYPE" != "none" ]; then
        die "80/443 已被 ${DETECTED_PROXY_TYPE} (${DETECTED_PROXY_PROC:-Caddy}) 占用，无法启用内置自动 HTTPS（self）；请改用 --access-mode auto 自动复用或 --access-mode local"
      fi
      CADDY_MODE="self"
      CADDY_CONTAINER=""
      CADDY_FILE=""
      CADDY_INNER=""
      CADDY_NET=""
      UPSTREAM_HOST="127.0.0.1"
      ;;
    host-caddy)
      [ -n "$DOMAIN" ] || die "--access-mode host-caddy 必须配合 --domain <域名> 使用"
      [ "$DETECTED_PROXY_TYPE" = "host-caddy" ] || die "未检测到宿主机 Caddy 进程，无法使用 --access-mode host-caddy"
      [ -n "$CADDY_FILE" ] && [ -f "$CADDY_FILE" ] || die "未找到宿主机 Caddyfile，请确认 Caddy 配置路径"
      CADDY_MODE="host-caddy"
      ;;
    docker-caddy)
      [ -n "$DOMAIN" ] || die "--access-mode docker-caddy 必须配合 --domain <域名> 使用"
      [ "$DETECTED_PROXY_TYPE" = "docker-caddy" ] || die "未检测到占用 80/443 的 Caddy 容器，无法使用 --access-mode docker-caddy"
      [ -n "$CADDY_FILE" ] && [ -f "$CADDY_FILE" ] || die "未找到外层 Caddy 容器挂载的宿主机 Caddyfile，请改用 --access-mode local 并手动配置反代"
      CADDY_MODE="docker-caddy"
      ;;
    direct|local)
      CADDY_MODE="none"
      CADDY_CONTAINER=""
      CADDY_FILE=""
      CADDY_INNER=""
      CADDY_NET=""
      UPSTREAM_HOST="127.0.0.1"
      ;;
  esac

  USED_PORTS_CACHE=""
  local used_here=""

  case "$ACCESS_MODE" in
    self)
      if [ "$FLAG_WEB_BIND" = "0" ]; then WEB_BIND="0.0.0.0"; fi
      if [ -z "$WEB_PORT" ] || [ "$WEB_PORT" = "auto" ]; then WEB_PORT="80"; fi
      if [ -z "$WEB_TLS_PORT" ] || [ "$WEB_TLS_PORT" = "auto" ]; then WEB_TLS_PORT="443"; fi
      ;;
    host-caddy)
      if [ "$FLAG_WEB_BIND" = "0" ]; then WEB_BIND="127.0.0.1"; fi
      if [ -z "$WEB_PORT" ] || [ "$WEB_PORT" = "auto" ]; then
        WEB_PORT="$(pick_free_port "$DEFAULT_LOCAL_WEB_PORT")"
      fi
      used_here="$WEB_PORT"
      if [ -z "$WEB_TLS_PORT" ] || [ "$WEB_TLS_PORT" = "auto" ]; then
        WEB_TLS_PORT="$(pick_free_port "$DEFAULT_LOCAL_TLS_PORT" "$used_here")"
      fi
      ;;
    docker-caddy)
      if [ "$FLAG_WEB_BIND" = "0" ]; then WEB_BIND="$UPSTREAM_HOST"; fi
      if [ -z "$WEB_PORT" ] || [ "$WEB_PORT" = "auto" ]; then
        WEB_PORT="$(pick_free_port "$DEFAULT_LOCAL_WEB_PORT")"
      fi
      used_here="$WEB_PORT"
      if [ -z "$WEB_TLS_PORT" ] || [ "$WEB_TLS_PORT" = "auto" ]; then
        WEB_TLS_PORT="$(pick_free_port "$DEFAULT_LOCAL_TLS_PORT" "$used_here")"
      fi
      ;;
    direct)
      if [ "$FLAG_WEB_BIND" = "0" ]; then WEB_BIND="0.0.0.0"; fi
      if [ -z "$WEB_PORT" ] || [ "$WEB_PORT" = "auto" ]; then
        WEB_PORT="$(pick_free_port "$DEFAULT_DIRECT_PORT")"
      fi
      used_here="$WEB_PORT"
      if [ -z "$WEB_TLS_PORT" ] || [ "$WEB_TLS_PORT" = "auto" ]; then
        WEB_TLS_PORT="$(pick_free_port "$DEFAULT_LOCAL_TLS_PORT" "$used_here")"
      fi
      ;;
    local)
      if [ "$FLAG_WEB_BIND" = "0" ]; then WEB_BIND="127.0.0.1"; fi
      if [ -z "$WEB_PORT" ] || [ "$WEB_PORT" = "auto" ]; then
        WEB_PORT="$(pick_free_port "$DEFAULT_LOCAL_WEB_PORT")"
      fi
      used_here="$WEB_PORT"
      if [ -z "$WEB_TLS_PORT" ] || [ "$WEB_TLS_PORT" = "auto" ]; then
        WEB_TLS_PORT="$(pick_free_port "$DEFAULT_LOCAL_TLS_PORT" "$used_here")"
      fi
      ;;
  esac

  used_here="${WEB_PORT},${WEB_TLS_PORT}"
  if [ -z "$API_PORT" ] || [ "$API_PORT" = "auto" ]; then
    API_PORT="$(pick_free_port "$DEFAULT_API_PORT" "$used_here")"
  fi

  if [ "$WEB_PORT" = "$WEB_TLS_PORT" ] || [ "$WEB_PORT" = "$API_PORT" ] || [ "$WEB_TLS_PORT" = "$API_PORT" ]; then
    die "端口冲突：WEB_PORT($WEB_PORT)、WEB_TLS_PORT($WEB_TLS_PORT)、API_PORT($API_PORT) 必须互不相同"
  fi
  if [ "$FLAG_WEB_PORT" = "1" ] && [ "$WEB_PORT" != "auto" ] && is_port_used "$WEB_PORT"; then
    die "指定的前端端口 ${WEB_PORT} 已被其他进程或容器占用，请换一个端口或使用 --web-port auto"
  fi
  if [ "$FLAG_WEB_TLS_PORT" = "1" ] && [ "$WEB_TLS_PORT" != "auto" ] && is_port_used "$WEB_TLS_PORT"; then
    die "指定的 HTTPS 端口 ${WEB_TLS_PORT} 已被其他进程或容器占用，请换一个端口或使用 --web-tls-port auto"
  fi
  if [ "$FLAG_API_PORT" = "1" ] && [ "$API_PORT" != "auto" ] && is_port_used "$API_PORT"; then
    die "指定的 API 端口 ${API_PORT} 已被其他进程或容器占用，请换一个端口或使用 --api-port auto"
  fi

  if [ -z "$SUB_NAME" ]; then SUB_NAME="$DEFAULT_SUB_NAME"; fi
  if [ -z "$ENABLE_COLLECTOR" ]; then ENABLE_COLLECTOR="true"; fi
}

access_mode_label() {
  case "${1:-}" in
    self)         printf '绑定域名 + 内置 Caddy 自动 HTTPS' ;;
    host-caddy)   printf '绑定域名 + 复用宿主机 Caddy 反代' ;;
    docker-caddy) printf '绑定域名 + 复用容器化 Caddy 反代' ;;
    direct)       printf '无域名 + 服务器端口直连（HTTP）' ;;
    local)        printf '仅本机回环（SSH 隧道或自配 Nginx 反代）' ;;
    *)            printf '%s' "${1:-未配置}" ;;
  esac
}

# ── 访问方式互动子流程（安装向导与修改配置共用）────────────────────────────────
prompt_access_mode_interactive() {
  detect_existing_proxy
  local def_choice="1"
  if [ "$SAVED_LOADED" = "1" ]; then
    case "$SAVED_ACCESS_MODE" in
      self|host-caddy|docker-caddy) def_choice="1" ;;
      direct) def_choice="2" ;;
      local)  def_choice="3" ;;
    esac
  elif [ "$DETECTED_PROXY_TYPE" = "nginx" ] || [ "$DETECTED_PROXY_TYPE" = "other" ]; then
    def_choice="3"
  fi

  if [ "$DETECTED_PROXY_TYPE" = "host-caddy" ] || [ "$DETECTED_PROXY_TYPE" = "docker-caddy" ]; then
    ok "检测到本机已有 Caddy（${DETECTED_PROXY_TYPE}），选 1 可自动复用配 HTTPS，不影响现有网站"
  elif [ "$DETECTED_PROXY_TYPE" = "nginx" ]; then
    warn "检测到本机 80/443 正由 Nginx 占用，选 3 将提供可直接粘贴的 Nginx 反代配置片段"
  fi

  local c
  c="$(choose "请选择访问方式" "$def_choice" \
    "绑定域名并自动配置 HTTPS 证书（推荐，需域名已解析到本机）" \
    "不绑域名，通过服务器端口直连 HTTP（适合内网或快速体验）" \
    "仅监听本机回环（配合现有 Nginx 自行反代，或用 SSH 隧道访问）")"

  case "$c" in
    1)
      ACCESS_MODE="auto"
      local def_dom="${DOMAIN:-$SAVED_DOMAIN}"
      while :; do
        local raw_dom=""
        if [ -n "$def_dom" ]; then
          raw_dom="$(ask "请输入要绑定的域名（如 panel.example.com）" "$def_dom")"
        else
          printf '  请输入要绑定的域名（如 panel.example.com，直接回车改走端口直连）: ' >&2
          tty_read raw_dom 0 || raw_dom=""
        fi
        if [ -z "$raw_dom" ]; then
          DOMAIN=""
          ACCESS_MODE="direct"
          if [ -n "$SAVED_DOMAIN" ]; then NO_DOMAIN=1; fi
          warn "未填写域名，已自动切换为「端口直连 HTTP」模式"
          break
        fi
        raw_dom="$(clean_domain "$raw_dom")"
        if is_valid_domain "$raw_dom"; then
          DOMAIN="$raw_dom"
          NO_DOMAIN=0
          break
        fi
        warn "「$raw_dom」不是合法域名格式（示例：panel.example.com），请重新输入"
      done
      ;;
    2)
      ACCESS_MODE="direct"
      if [ -n "$SAVED_DOMAIN" ]; then NO_DOMAIN=1; fi
      DOMAIN=""
      ;;
    3)
      ACCESS_MODE="local"
      if [ -n "$SAVED_DOMAIN" ]; then NO_DOMAIN=1; fi
      DOMAIN=""
      ;;
  esac
}

# ── 安装向导（默认仅问 2–3 个关键问题，拿不准一律回车）──────────────────────────
run_install_wizard() {
  if ! can_prompt; then
    return 0
  fi

  say ""
  say "${C_BLD}${C_CYN}════════════════════════════════════════════════════════════${C_OFF}"
  say "${C_BLD}  欢迎使用 ${APP_LABEL} 一键安装向导（v${SCRIPT_VERSION}）${C_OFF}"
  say "${C_BLD}${C_CYN}════════════════════════════════════════════════════════════${C_OFF}"
  info "装好后您可以通过浏览器统一管理多台 VPS 主机、Hysteria2 节点、订阅交付与告警。"
  info "首次安装需要构建镜像，预计 2–5 分钟；下面只会问 2–3 个问题，拿不准直接按回车即可。"
  hr

  # 问题 1：访问方式
  if [ "$FLAG_ACCESS_MODE" = "0" ] && [ "$FLAG_DOMAIN" = "0" ] && [ "$NO_DOMAIN" != "1" ]; then
    step "第 1 步：选择访问方式"
    prompt_access_mode_interactive
  fi

  # 问题 2：管理员账号（仅首次安装或无 .env 时询问）
  if [ ! -f "$INSTALL_DIR/.env" ]; then
    step "第 2 步：设置初始管理员账号"
    if [ "$FLAG_ADMIN_EMAIL" = "0" ]; then
      while :; do
        local em
        em="$(ask "管理员登录邮箱" "${ADMIN_EMAIL:-$DEFAULT_ADMIN_EMAIL}")"
        if is_valid_email "$em"; then
          ADMIN_EMAIL="$em"
          break
        fi
        warn "邮箱格式不正确，请重新输入（例如 admin@teyir.local）"
      done
    fi
    if [ "$FLAG_ADMIN_PASSWORD" = "0" ]; then
      local auto_pw
      auto_pw="$(gen_alnum_password 16)"
      ADMIN_PASSWORD="$(ask_password "管理员登录密码（至少 12 位）" "$auto_pw")"
    fi
  else
    ok "检测到已有 .env 配置，将自动保留原管理员账号、数据库密码与节点加密主密钥"
  fi

  # 问题 3：高级选项门禁
  step "第 3 步：高级选项（一般直接回车跳过）"
  local want_adv=0
  if ask_yn "需要修改高级选项（安装目录 / 端口 / 默认订阅名 / 周期采集开关 / 联动地址）吗？" "n"; then
    want_adv=1
  fi

  if [ "$want_adv" = "1" ]; then
    while :; do
      local d_in
      d_in="$(ask "安装目录（绝对路径）" "$INSTALL_DIR")"
      d_in="${d_in%/}"
      if is_safe_install_dir "$d_in"; then
        if [ "$d_in" != "$INSTALL_DIR" ]; then
          INSTALL_DIR="$d_in"
          load_saved_state
        fi
        break
      fi
      warn "安装目录必须是非系统根目录的绝对路径（例如 /opt/teyir-console）"
    done

    SUB_NAME="$(ask "客户端导入订阅后的默认分组名" "${SUB_NAME:-$DEFAULT_SUB_NAME}")"
    if ask_yn "是否启用后台周期采集器（定期刷新 VPS 主机与节点监控指标）？" "y"; then
      ENABLE_COLLECTOR="true"
    else
      ENABLE_COLLECTOR="false"
    fi

    if [ "$ACCESS_MODE" != "self" ]; then
      local def_wp="${WEB_PORT:-$DEFAULT_DIRECT_PORT}"
      if [ "$ACCESS_MODE" != "direct" ]; then
        def_wp="${WEB_PORT:-$DEFAULT_LOCAL_WEB_PORT}"
      fi
      WEB_PORT="$(ask_port "前端访问端口" "$def_wp")"
      FLAG_WEB_PORT=1
    fi
    API_PORT="$(ask_port "后端本机 API 端口" "${API_PORT:-$DEFAULT_API_PORT}")"
    FLAG_API_PORT=1

    local cur_renew cur_hy2_url cur_hy2_repo
    cur_renew="$(read_kv_file "$INSTALL_DIR/.env" STORE_RENEW_BASE_URL)"
    cur_hy2_url="$(read_kv_file "$INSTALL_DIR/.env" HY2_INSTALL_SCRIPT_URL)"
    cur_hy2_repo="$(read_kv_file "$INSTALL_DIR/.env" HY2_INSTALLER_REPO)"
    STORE_RENEW_BASE_URL="$(ask "外部商城续费基础地址（可选，留空跳过）" "$cur_renew")"
    HY2_INSTALL_SCRIPT_URL="$(ask "节点一键安装脚本 HTTPS 地址（可选，留空跳过）" "$cur_hy2_url")"
    HY2_INSTALLER_REPO="$(ask "节点门户更新来源仓库 owner/repo（可选，留空跳过）" "$cur_hy2_repo")"
  fi
}

print_install_summary() {
  local pub_ip
  pub_ip="$(detect_public_ip)"
  local access_url=""
  case "$ACCESS_MODE" in
    self|host-caddy|docker-caddy) access_url="https://${DOMAIN}" ;;
    direct) access_url="http://${pub_ip:-<服务器IP>}:${WEB_PORT}" ;;
    local)
      if [ -n "$DOMAIN" ] && [ "$DETECTED_PROXY_TYPE" = "nginx" ]; then
        access_url="http://127.0.0.1:${WEB_PORT}（配置下方 Nginx 反代后访问 https://${DOMAIN}）"
      else
        access_url="http://127.0.0.1:${WEB_PORT}（经 SSH 隧道或外层 Nginx 访问）"
      fi
      ;;
  esac

  say ""
  say "${C_BLD}${C_GRN}════════════════════════════════════════════════════════════${C_OFF}"
  say "${C_BLD}  ${APP_LABEL} 已部署完成！${C_OFF}"
  say "${C_BLD}${C_GRN}════════════════════════════════════════════════════════════${C_OFF}"
  say ""
  say "${C_BLD}  下一步：用浏览器打开控制台并登录管理员账号${C_OFF}"
  info "面板地址：${C_CYN}${access_url}${C_OFF}"
  info "登录邮箱：${C_BLD}${ADMIN_EMAIL}${C_OFF}"
  if [ "$IS_NEW_ADMIN_SEEDED" = "1" ]; then
    info "初始密码：${C_YEL}${ADMIN_PASSWORD}${C_OFF}  （请立刻妥善保存，后续可在面板修改）"
  else
    info "管理员密码：沿用原有配置（位于 $INSTALL_DIR/.env 中的 SEED_ADMIN_PASSWORD）"
  fi
  if [ "$ACCESS_MODE" = "direct" ]; then
    warn "安全提示：当前为纯 HTTP 端口直连，请在云服务商安全组放行 TCP ${WEB_PORT}，生产环境建议绑定域名启用 HTTPS。"
  elif [ "$ACCESS_MODE" = "self" ] || [ "$ACCESS_MODE" = "host-caddy" ] || [ "$ACCESS_MODE" = "docker-caddy" ]; then
    info "证书提示：首次访问时 Caddy 会自动申请 Let's Encrypt 证书，请确保 80/443 端口已在安全组放行。"
  elif [ "$ACCESS_MODE" = "local" ]; then
    local ssh_user="${SUDO_USER:-$(whoami 2>/dev/null || echo root)}"
    say ""
    info "本机免暴露端口安全访问（在您自己的电脑终端执行）："
    info "  ssh -N -L ${WEB_PORT}:127.0.0.1:${WEB_PORT} ${ssh_user}@${pub_ip:-<服务器IP>}"
    dim "说明：输完密码后没有任何输出、看着像卡住是正常的（正在转发端口），不要关掉该窗口；"
    dim "若平时使用 SSH 别名/密钥登录，请在原 ssh 命令后追加 -N -L ${WEB_PORT}:127.0.0.1:${WEB_PORT}。"
    if [ "$DETECTED_PROXY_TYPE" = "nginx" ]; then
      print_nginx_snippet
    fi
  fi

  say ""
  info "登录后推荐操作顺序："
  info "  1) 进入「系统设置」核对默认订阅名与管理员密码"
  info "  2) 进入「VPS 主机」添加主机凭据并绑定 Hysteria2 节点"
  info "  3) 按需开启「订阅中心」或配置 Telegram 运维机器人"

  say ""
  dim "────────── 常用运维命令（以后需要再查） ──────────"
  if [ "$NO_SHORTCUT" != "1" ] && [ -L "$SHORTCUT_PATH" ]; then
    if [ "$(id -u)" -eq 0 ]; then
      info "打开交互管理菜单：teyir"
    else
      info "打开交互管理菜单：sudo teyir"
    fi
  else
    info "打开交互管理菜单：$(render_priv_cmd)"
  fi
  info "查看运行状态：    $(render_self_cmd --status)"
  info "一键升级更新：    $(render_priv_cmd --upgrade)"
  info "安全卸载服务：    $(render_priv_cmd --uninstall)"
  info "安装目录：        ${INSTALL_DIR}"
  say ""
}

# ── 核心业务动作：安装 / 重配 ──────────────────────────────────────────────────
do_install() {
  require_root --install || return 1
  load_saved_state
  prefill_from_saved

  if [ ! -f "$INSTALL_DIR/install.conf" ] && [ ! -f "$INSTALL_DIR/.env" ]; then
    FRESH_INSTALL=1
  else
    FRESH_INSTALL=0
  fi

  run_install_wizard
  validate_inputs
  ensure_dependencies
  plan_access_and_ports

  say ""
  step "部署方案确认"
  info "安装目录：      ${INSTALL_DIR}"
  info "项目标识：      ${PROJECT_NAME}"
  info "源码仓库：      ${APP_REPO}（分支：${APP_REF}）"
  info "访问方式：      $(access_mode_label "$ACCESS_MODE")"
  if [ -n "$DOMAIN" ]; then
    info "绑定域名：      ${DOMAIN}"
  fi
  info "前端监听：      ${WEB_BIND}:${WEB_PORT}（TLS 映射：${WEB_TLS_PORT}）"
  info "后端 API：      ${API_BIND}:${API_PORT}"
  info "默认订阅分组：  ${SUB_NAME}"
  if [ "$ENABLE_COLLECTOR" = "true" ]; then
    info "周期采集器：    开启"
  else
    info "周期采集器：    关闭"
  fi
  if [ "$CADDY_MODE" = "host-caddy" ] || [ "$CADDY_MODE" = "docker-caddy" ]; then
    info "共享反代文件：  ${CADDY_FILE}（改前自动备份，失败自动回滚）"
  fi
  hr

  if [ "$DRY_RUN" = "1" ]; then
    prepare_source_dir
    write_env_file
    if [ "$NO_DOMAIN" = "1" ] || { [ -n "$SAVED_CADDY_FILE" ] && [ "$CADDY_MODE" = "none" ]; }; then
      remove_external_caddy_block
    fi
    apply_external_caddy
    start_and_verify_stack 1
    save_state
    ok "预演完成：未对系统做任何实际修改。"
    return 0
  fi

  if can_prompt; then
    if ! ask_yn "确认按以上方案开始部署吗？" "y"; then
      warn "已取消本次部署。"
      return 0
    fi
  fi

  if [ "$NO_DOMAIN" = "1" ] || { [ -n "$SAVED_CADDY_FILE" ] && [ "$CADDY_MODE" != "$SAVED_CADDY_MODE" ]; }; then
    remove_external_caddy_block
  fi

  if [ -n "$DOMAIN" ]; then
    if [ "$CADDY_MODE" = "host-caddy" ] || [ "$CADDY_MODE" = "docker-caddy" ]; then
      if caddyfile_has_domain_conflict "$CADDY_FILE" "$DOMAIN"; then
        die "外层反代配置 $CADDY_FILE 中已存在域名 $DOMAIN 的其他站点块，已提前中止"
      fi
    fi
    check_dns_before_ssl
  fi

  prepare_source_dir

  local attempt=1
  while [ "$attempt" -le 3 ]; do
    write_env_file
    if start_and_verify_stack 1; then
      break
    fi
    if [ "$LAST_FAIL_KIND" = "port" ] && [ "$ACCESS_MODE" != "self" ] && [ "$attempt" -lt 3 ]; then
      warn "检测到端口绑定冲突（第 ${attempt}/3 次），正在自动避让并换新端口重试..."
      USED_PORTS_CACHE=""
      WEB_PORT="$(pick_free_port $((WEB_PORT + 1)))"
      WEB_TLS_PORT="$(pick_free_port $((WEB_TLS_PORT + 1)) "$WEB_PORT")"
      API_PORT="$(pick_free_port $((API_PORT + 1)) "${WEB_PORT},${WEB_TLS_PORT}")"
      attempt=$((attempt + 1))
      continue
    fi
    if [ "$LAST_FAIL_KIND" = "name" ]; then
      die "容器名冲突，请使用 --project-name <新名称> 指定独立项目名后重试"
    fi
    die "服务启动或健康验收失败（失败类型：${LAST_FAIL_KIND:-unknown}），请根据上方日志排查"
  done

  apply_external_caddy
  save_state
  print_install_summary
}

# ── 核心业务动作：检查更新 & 备份 & 一键升级 ───────────────────────────────────
do_check_update() {
  need_state || return 1
  if [ ! -d "$INSTALL_DIR/.git" ]; then
    warn "当前安装目录不是 Git 仓库（可能通过压缩包安装），无法对比远端提交。"
    return 0
  fi
  step "检查远端更新（${APP_REPO} · 分支 ${APP_REF}）"
  local local_sha local_msg remote_sha behind
  local_sha="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  local_msg="$(git -C "$INSTALL_DIR" log -1 --pretty='%s' 2>/dev/null || true)"
  if git -C "$INSTALL_DIR" fetch --quiet origin "$APP_REF" 2>/dev/null; then
    remote_sha="$(git -C "$INSTALL_DIR" rev-parse --short "origin/${APP_REF}" 2>/dev/null || echo unknown)"
    behind="$(git -C "$INSTALL_DIR" rev-list --count "HEAD..origin/${APP_REF}" 2>/dev/null || echo 0)"
    info "当前版本：${local_sha}（${local_msg}）"
    info "远端最新：${remote_sha}（分支 ${APP_REF}）"
    if [ "$behind" = "0" ]; then
      ok "已是最新版本，无需升级。"
    else
      warn "远端领先 ${behind} 个提交！可执行升级命令：$(render_priv_cmd --upgrade)"
    fi
  else
    warn "无法连接远端仓库检查更新，请检查网络连通性。"
  fi
}

do_backup() {
  require_root --backup || return 1
  need_state || return 1
  step "备份配置与 PostgreSQL 数据库"
  local stamp backup_root="/opt"
  stamp="$(date +%Y%m%d-%H%M%S)"
  local code_bk="${backup_root}/${PROJECT_NAME}-backup-${stamp}.tar.gz"
  local db_bk="${backup_root}/${PROJECT_NAME}-db-${stamp}.dump"

  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将备份源码与配置到 $code_bk，并转储数据库到 $db_bk"
    return 0
  fi

  tar -C "$INSTALL_DIR" --exclude='./.git' --exclude='./node_modules' --exclude='./dist' -czf "$code_bk" .
  chmod 600 "$code_bk"
  ls -1t "${backup_root}/${PROJECT_NAME}-backup-"*.tar.gz 2>/dev/null | tail -n +6 | xargs -r rm -f
  ok "源码与配置备份完成：$code_bk"

  local err_tmp
  err_tmp="$(mktemp /tmp/teyir-pgdump.XXXXXX)"
  if dc exec -T db pg_dump -U nodeops -d nodeops > "$db_bk" 2>"$err_tmp" && [ -s "$db_bk" ]; then
    chmod 600 "$db_bk"
    rm -f "$err_tmp"
    ls -1t "${backup_root}/${PROJECT_NAME}-db-"*.dump 2>/dev/null | tail -n +4 | xargs -r rm -f
    ok "数据库快照备份完成：$db_bk（$(du -h "$db_bk" | cut -f1)）"
  else
    rm -f "$db_bk"
    err "数据库备份失败："
    if [ -s "$err_tmp" ]; then
      sed 's/^/    /' "$err_tmp" >&2 || true
    fi
    rm -f "$err_tmp"
    return 1
  fi
}

do_upgrade() {
  require_root --upgrade || return 1
  need_state || return 1
  ensure_dependencies

  step "开始升级 ${APP_LABEL}（目录：${INSTALL_DIR}）"
  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将依次执行：备份源码与数据库 → 拉取 origin/${APP_REF} → 重建 migrate 并迁移 → 重建 api/web → 容器内健康验收"
    return 0
  fi

  [ -d "$INSTALL_DIR/.git" ] || die "目录 $INSTALL_DIR 不是 Git 仓库，无法自动拉取更新（请手工同步源码后重跑重新部署）"

  local before_sha after_sha
  before_sha="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  info "升级前版本：$before_sha"

  dc up -d db >/dev/null || die "启动数据库失败，已中止升级"
  do_backup || die "升级前备份失败，为避免无退路已中止升级"

  info "拉取远端分支 origin/${APP_REF}..."
  git -C "$INSTALL_DIR" fetch --prune origin || die "拉取远端仓库失败，请检查网络连接"
  git -C "$INSTALL_DIR" rev-parse --verify --quiet "refs/remotes/origin/${APP_REF}^{commit}" >/dev/null \
    || die "远端仓库不存在分支 ${APP_REF}"

  if git -C "$INSTALL_DIR" show-ref --verify --quiet "refs/heads/${APP_REF}"; then
    git -C "$INSTALL_DIR" checkout -q "$APP_REF" || die "切换本地分支 ${APP_REF} 失败"
  else
    git -C "$INSTALL_DIR" checkout -q -b "$APP_REF" --track "origin/${APP_REF}" || die "创建跟踪分支 ${APP_REF} 失败"
  fi

  local ahead
  ahead="$(git -C "$INSTALL_DIR" rev-list --count "origin/${APP_REF}..HEAD" 2>/dev/null || echo '?')"
  if [ "$ahead" != "0" ] && [ "${DEPLOY_ALLOW_DISCARD:-0}" != "1" ]; then
    die "本地存在未推送提交（ahead=${ahead}），为防止丢失已拒绝 reset --hard；确认丢弃请设置 DEPLOY_ALLOW_DISCARD=1"
  fi
  git -C "$INSTALL_DIR" reset --hard "origin/${APP_REF}" || die "重置工作区到 origin/${APP_REF} 失败"
  after_sha="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  ok "源码已更新：${before_sha} -> ${after_sha}"

  start_and_verify_stack 1 || die "升级后健康验收未通过，请查看上方日志；备份已保存在 /opt/${PROJECT_NAME}-*"
  save_state
  ok "升级完成：${before_sha} -> ${after_sha}，全部服务运行健康！"
}

# ── 核心业务动作：查看状态（只读，不强制 root）────────────────────────────────
do_status() {
  need_state || return 1

  say ""
  say "${C_BLD}${C_CYN}════════════════════════════════════════════════════════════${C_OFF}"
  say "${C_BLD}  ${APP_LABEL} 运行状态总览${C_OFF}"
  say "${C_BLD}${C_CYN}════════════════════════════════════════════════════════════${C_OFF}"

  local git_ver="非 Git 安装"
  if [ -d "$INSTALL_DIR/.git" ]; then
    git_ver="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)（分支：$(git -C "$INSTALL_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "$APP_REF")）"
  fi
  local env_email
  env_email="$(read_kv_file "$INSTALL_DIR/.env" SEED_ADMIN_EMAIL)"
  local pub_ip=""
  if [ "$ACCESS_MODE" = "direct" ] || [ "$ACCESS_MODE" = "local" ]; then
    pub_ip="$(detect_public_ip)"
  fi

  info "安装目录：    ${INSTALL_DIR}"
  info "项目标识：    ${PROJECT_NAME}"
  info "源码版本：    ${git_ver}"
  info "安装器版本：  ${SAVED_SCRIPT_VERSION:-$SCRIPT_VERSION}"
  info "访问模式：    $(access_mode_label "$ACCESS_MODE")"
  if [ -n "$DOMAIN" ]; then
    info "访问地址：    https://${DOMAIN}"
  elif [ "$ACCESS_MODE" = "direct" ]; then
    info "访问地址：    http://${pub_ip:-<服务器IP>}:${WEB_PORT}"
  else
    info "访问地址：    http://127.0.0.1:${WEB_PORT}（本机回环 / SSH 隧道）"
  fi
  info "监听端口：    Web=${WEB_BIND}:${WEB_PORT} (TLS:${WEB_TLS_PORT}) | API=${API_BIND}:${API_PORT}"
  info "默认订阅名：  ${SUB_NAME:-$DEFAULT_SUB_NAME}"
  info "周期采集器：  ${ENABLE_COLLECTOR:-true}"
  if [ -n "$env_email" ]; then
    info "种子管理员：  ${env_email}"
  fi
  if [ -n "$SAVED_CADDY_FILE" ]; then
    info "共享反代：    ${SAVED_CADDY_MODE}（${SAVED_CADDY_FILE}）"
  fi

  hr
  if ! docker_bin_ok; then
    warn "本机未检测到 docker 命令"
    return 0
  fi
  if ! docker_daemon_ok; then
    warn "当前用户无法连接 Docker 守护进程（如需查看容器实时状态，请用管理员权限运行：$(render_priv_cmd --status)）"
    return 0
  fi

  step "容器运行状态"
  dc ps --format 'table {{.Name}}\t{{.Service}}\t{{.Status}}\t{{.Ports}}' >&2 || true

  step "服务内部健康探针"
  if dc exec -T api /api --healthcheck >/dev/null 2>&1; then
    ok "api 容器内部探活正常"
  else
    err "api 容器内部探活异常"
  fi
  if dc exec -T web wget -qO- http://127.0.0.1:8081/healthz 2>/dev/null | grep -q 'ok'; then
    ok "web 容器内部探活正常（127.0.0.1:8081/healthz -> ok）"
  else
    err "web 容器内部探活异常"
  fi

  if [ -n "$DOMAIN" ] && command -v openssl >/dev/null 2>&1; then
    step "域名 HTTPS 与证书核验"
    local https_code cert_end
    https_code="$(curl --max-time 8 -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/api/health" 2>/dev/null || echo 000)"
    if [ "$https_code" = "200" ]; then
      ok "公网 HTTPS 接口正常（https://${DOMAIN}/api/health -> 200）"
    else
      warn "公网 HTTPS 接口返回：${https_code}（如刚绑定域名，证书签发可能需要 10–30 秒）"
    fi
    cert_end="$(echo | openssl s_client -connect "${DOMAIN}:443" -servername "$DOMAIN" 2>/dev/null \
      | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2- || true)"
    if [ -n "$cert_end" ]; then
      info "证书到期时间：${cert_end}"
    fi
  fi
  say ""
}

# ── 核心业务动作：启停、日志与卸载 ─────────────────────────────────────────────
do_lifecycle() {
  local op="$1"
  require_root "--${op}" || return 1
  need_state || return 1
  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将在 $INSTALL_DIR 执行服务操作：$op"
    return 0
  fi
  case "$op" in
    start)
      step "启动 ${APP_LABEL} 服务"
      dc up -d --no-build db api web
      ok "服务已启动"
      ;;
    stop)
      step "停止 ${APP_LABEL} 服务"
      dc stop api web db
      ok "服务已停止"
      ;;
    restart)
      step "重启 ${APP_LABEL} 服务"
      dc restart api web
      ok "api 与 web 已重启"
      ;;
  esac
}

do_logs() {
  need_state || return 1
  docker_daemon_ok || die "当前无权连接 Docker，请改用：$(render_priv_cmd --logs "$LOGS_SERVICE")"
  step "查看最近 80 行日志（对象：${LOGS_SERVICE}）"
  if [ "$LOGS_SERVICE" = "all" ]; then
    dc logs --tail=80 api web db >&2
  else
    dc logs --tail=80 "$LOGS_SERVICE" >&2
  fi
}

do_uninstall() {
  require_root --uninstall || return 1

  step "卸载 ${APP_LABEL}"
  if [ ! -e "$INSTALL_DIR" ]; then
    ok "安装目录 $INSTALL_DIR 不存在，无需卸载（环境已是干净状态）。"
    return 0
  fi

  load_saved_state
  prefill_from_saved

  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将停止并移除项目 ${PROJECT_NAME} 容器"
    if [ -n "$SAVED_CADDY_FILE" ]; then
      dim "[预演] 将从 ${SAVED_CADDY_FILE} 摘除托管站点块并热重载"
    fi
    if [ "$PURGE_DATA" = "1" ]; then
      dim "[预演] 将删除数据卷与安装目录 ${INSTALL_DIR}"
    else
      dim "[预演] 默认保留数据库数据卷与目录 ${INSTALL_DIR}（加 --purge 才会彻底删除）"
    fi
    return 0
  fi

  local purge="$PURGE_DATA"
  if can_prompt; then
    warn "即将停止并移除 ${APP_LABEL} 的容器（目录：${INSTALL_DIR}）。"
    if ! ask_yn "确认开始卸载吗？" "n"; then
      info "已取消卸载。"
      return 0
    fi
    if [ "$purge" != "1" ]; then
      if ask_yn "是否同时永久删除数据库数据卷与整个安装目录？（危险，默认保留数据）" "n"; then
        purge=1
      fi
    fi
  fi

  if docker_bin_ok && docker_daemon_ok && [ -f "$INSTALL_DIR/docker-compose.yml" ]; then
    info "正在停止并移除容器..."
    if [ "$purge" = "1" ]; then
      COMPOSE_PROJECT_NAME="$PROJECT_NAME" docker compose \
        -f "$INSTALL_DIR/docker-compose.yml" \
        down -v --remove-orphans >/dev/null 2>&1 || true
      ok "已移除容器及关联数据卷"
    else
      COMPOSE_PROJECT_NAME="$PROJECT_NAME" docker compose \
        -f "$INSTALL_DIR/docker-compose.yml" \
        down --remove-orphans >/dev/null 2>&1 || true
      ok "已停止并移除容器（PostgreSQL 数据卷已安全保留）"
    fi
  fi

  if [ -n "$SAVED_CADDY_FILE" ]; then
    remove_external_caddy_block "$SAVED_CADDY_MODE" "$SAVED_CADDY_FILE" "$SAVED_CADDY_CONTAINER" "$SAVED_CADDY_INNER"
  fi

  if [ -L "$SHORTCUT_PATH" ]; then
    local target_link expected_self
    target_link="$(resolve_realpath "$SHORTCUT_PATH" 2>/dev/null || true)"
    expected_self="$(resolve_realpath "$INSTALL_DIR/install.sh" 2>/dev/null || printf '%s/install.sh' "$INSTALL_DIR")"
    if [ "$target_link" = "$expected_self" ]; then
      rm -f "$SHORTCUT_PATH"
      ok "已移除快捷命令：$SHORTCUT_PATH"
    fi
  fi

  if [ "$purge" = "1" ]; then
    is_safe_install_dir "$INSTALL_DIR" || die "拒绝删除不安全路径：$INSTALL_DIR"
    rm -rf "$INSTALL_DIR"
    ok "已彻底删除安装目录与全部数据：$INSTALL_DIR"
  else
    ok "卸载完成！配置与数据已保留在 $INSTALL_DIR（重装时可无损恢复数据与凭据）。"
  fi
}

# ── 交互子菜单：修改配置 & 服务运维 ────────────────────────────────────────────
menu_configure() {
  require_root || return 1
  need_state || return 0
  while :; do
    say ""
    step "修改配置（当前目录：${INSTALL_DIR}）"
    info "1) 修改访问方式与域名（当前：$(access_mode_label "$ACCESS_MODE") ${DOMAIN:+[$DOMAIN]}）"
    info "2) 修改监听端口（当前：Web=${WEB_PORT} / API=${API_PORT}）"
    info "3) 修改默认订阅分组名（当前：${SUB_NAME:-$DEFAULT_SUB_NAME}）"
    info "4) 切换后台周期采集器（当前：${ENABLE_COLLECTOR:-true}）"
    info "5) 配置外部联动地址（商城续费 / 节点安装脚本 / 门户更新源）"
    info "0) 返回主菜单"
    local ch=""
    printf '  请选择操作 [0]: ' >&2
    tty_read ch 0 || ch="0"
    case "${ch:-0}" in
      0) return 0 ;;
      1)
        FLAG_ACCESS_MODE=0; FLAG_DOMAIN=0; FLAG_WEB_BIND=0; FLAG_WEB_PORT=0; FLAG_WEB_TLS_PORT=0; NO_DOMAIN=0
        WEB_PORT=""; WEB_TLS_PORT=""; WEB_BIND=""
        prompt_access_mode_interactive
        validate_inputs
        plan_access_and_ports
        if [ "$NO_DOMAIN" = "1" ] || { [ -n "$SAVED_CADDY_FILE" ] && [ "$CADDY_MODE" != "$SAVED_CADDY_MODE" ]; }; then
          remove_external_caddy_block
        fi
        if [ -n "$DOMAIN" ]; then
          check_dns_before_ssl
        fi
        write_env_file
        start_and_verify_stack 0
        apply_external_caddy
        save_state
        ok "访问方式与域名配置已生效！"
        ;;
      2)
        WEB_PORT="$(ask_port "新的前端访问端口" "${WEB_PORT:-$DEFAULT_LOCAL_WEB_PORT}")"; FLAG_WEB_PORT=1
        API_PORT="$(ask_port "新的后端 API 端口" "${API_PORT:-$DEFAULT_API_PORT}")"; FLAG_API_PORT=1
        validate_inputs
        plan_access_and_ports
        write_env_file
        start_and_verify_stack 0
        apply_external_caddy
        save_state
        ok "端口修改已生效！"
        ;;
      3)
        SUB_NAME="$(ask "新的客户端默认订阅分组名" "${SUB_NAME:-$DEFAULT_SUB_NAME}")"
        if [ -z "$SUB_NAME" ]; then SUB_NAME="$DEFAULT_SUB_NAME"; fi
        write_env_file
        start_and_verify_stack 0
        save_state
        ok "默认订阅分组名已更新为：${SUB_NAME}"
        ;;
      4)
        if [ "${ENABLE_COLLECTOR:-true}" = "true" ]; then
          ENABLE_COLLECTOR="false"
        else
          ENABLE_COLLECTOR="true"
        fi
        write_env_file
        start_and_verify_stack 0
        save_state
        ok "周期采集器已切换为：${ENABLE_COLLECTOR}"
        ;;
      5)
        local cur_renew cur_hy2_url cur_hy2_repo
        cur_renew="$(read_kv_file "$INSTALL_DIR/.env" STORE_RENEW_BASE_URL)"
        cur_hy2_url="$(read_kv_file "$INSTALL_DIR/.env" HY2_INSTALL_SCRIPT_URL)"
        cur_hy2_repo="$(read_kv_file "$INSTALL_DIR/.env" HY2_INSTALLER_REPO)"
        STORE_RENEW_BASE_URL="$(ask "外部商城续费基础地址（留空关闭）" "$cur_renew")"
        HY2_INSTALL_SCRIPT_URL="$(ask "节点一键安装脚本 HTTPS 地址（留空关闭）" "$cur_hy2_url")"
        HY2_INSTALLER_REPO="$(ask "节点门户更新来源仓库 owner/repo（留空关闭）" "$cur_hy2_repo")"
        write_env_file
        start_and_verify_stack 0
        save_state
        ok "外部联动配置已更新并生效！"
        ;;
      *) warn "请输入 0 到 5 之间的数字" ;;
    esac
  done
}

menu_ops() {
  need_state || return 0
  while :; do
    say ""
    step "服务运维管理（项目：${PROJECT_NAME}）"
    info "1) 启动服务"
    info "2) 停止服务"
    info "3) 重启 api 与 web"
    info "4) 查看后端 api 最近日志"
    info "5) 查看前端 web 最近日志"
    info "6) 查看数据库 db 最近日志"
    info "7) 立即备份源码配置与数据库"
    info "8) 仅执行一次数据库迁移检查"
    info "0) 返回主菜单"
    local ch=""
    printf '  请选择操作 [0]: ' >&2
    tty_read ch 0 || ch="0"
    case "${ch:-0}" in
      0) return 0 ;;
      1) do_lifecycle start ;;
      2) do_lifecycle stop ;;
      3) do_lifecycle restart ;;
      4) LOGS_SERVICE="api"; do_logs ;;
      5) LOGS_SERVICE="web"; do_logs ;;
      6) LOGS_SERVICE="db"; do_logs ;;
      7) do_backup ;;
      8)
        require_root || continue
        step "执行数据库迁移"
        dc run --rm migrate
        ok "数据库迁移检查完成"
        ;;
      *) warn "请输入 0 到 8 之间的数字" ;;
    esac
  done
}

# ── 主交互菜单（菜单项直接调用对应函数，绝不依赖已检查过的旧标记）──────────────
render_menu_header() {
  load_saved_state
  prefill_from_saved

  local install_state="未安装"
  local running_info="—"
  local access_info="尚未部署"
  local ver_info="—"

  if [ -f "$INSTALL_DIR/install.conf" ] || [ -f "$INSTALL_DIR/.env" ]; then
    install_state="已安装"
    if [ -d "$INSTALL_DIR/.git" ]; then
      ver_info="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown) (${APP_REF})"
    fi
    if [ -n "$DOMAIN" ]; then
      access_info="https://${DOMAIN}"
    elif [ -n "$WEB_PORT" ]; then
      access_info="${WEB_BIND:-127.0.0.1}:${WEB_PORT}"
    fi
    if docker_bin_ok && docker_daemon_ok && [ -f "$INSTALL_DIR/docker-compose.yml" ]; then
      local up_cnt
      up_cnt="$(dc ps --status running --services 2>/dev/null | wc -l | tr -d ' ' || echo 0)"
      if [ "$up_cnt" -ge 3 ]; then
        running_info="${C_GRN}运行中（${up_cnt} 个服务在线）${C_OFF}"
      elif [ "$up_cnt" -gt 0 ]; then
        running_info="${C_YEL}部分运行（${up_cnt} 个服务在线）${C_OFF}"
      else
        running_info="${C_RED}已停止${C_OFF}"
      fi
    else
      running_info="${C_DIM}需管理员权限读取容器状态${C_OFF}"
    fi
  elif [ -d "$INSTALL_DIR" ] && [ -f "$INSTALL_DIR/docker-compose.yml" ]; then
    install_state="目录已就绪（尚未完成配置）"
  fi

  say ""
  say "${C_BLD}${C_CYN}╔══════════════════════════════════════════════════════════╗${C_OFF}"
  say "${C_BLD}${C_CYN}║${C_OFF}       ${C_BLD}${APP_LABEL} · 一键安装与交互管理菜单 v${SCRIPT_VERSION}${C_OFF}       ${C_BLD}${C_CYN}║${C_OFF}"
  say "${C_BLD}${C_CYN}╚══════════════════════════════════════════════════════════╝${C_OFF}"
  info "安装目录：${INSTALL_DIR}  |  状态：${C_BLD}${install_state}${C_OFF}"
  info "服务运行：${running_info}  |  版本：${ver_info}"
  info "当前入口：${access_info}"
  hr
}

run_interactive_menu() {
  if ! can_prompt; then
    notify_non_interactive_once
    do_install
    return $?
  fi

  while :; do
    render_menu_header
    local is_installed=0
    if [ -f "$INSTALL_DIR/install.conf" ] || [ -f "$INSTALL_DIR/.env" ]; then
      is_installed=1
    fi

    if [ "$is_installed" = "1" ]; then
      say "  ${C_CYN}1)${C_OFF} 重新配置并部署 ${APP_LABEL}"
    else
      say "  ${C_CYN}1)${C_OFF} 一键安装 ${APP_LABEL}（推荐）"
    fi
    say "  ${C_CYN}2)${C_OFF} 升级更新到最新版本（备份 + 拉码 + 迁移 + 重建）"
    say "  ${C_CYN}3)${C_OFF} 查看运行状态、健康探针与证书信息"
    say "  ${C_CYN}4)${C_OFF} 修改配置（域名 / 端口 / 订阅名 / 采集开关 / 联动地址）"
    say "  ${C_CYN}5)${C_OFF} 服务运维管理（启停 / 重启 / 日志 / 备份 / 迁移）"
    say "  ${C_CYN}6)${C_OFF} 检查远端是否有新版本"
    say "  ${C_CYN}7)${C_OFF} 卸载 ${APP_LABEL}"
    say "  ${C_CYN}0)${C_OFF} 退出菜单"
    hr

    local choice=""
    printf '  请输入数字选择操作 [1]: ' >&2
    if ! tty_read choice 0; then
      printf '\n' >&2
      info "输入结束，退出菜单。"
      return 0
    fi
    choice="${choice:-1}"

    case "$choice" in
      1)
        RECONFIG="$is_installed"
        do_install || true
        pause_enter
        ;;
      2)
        do_upgrade || true
        pause_enter
        ;;
      3)
        do_status || true
        pause_enter
        ;;
      4)
        menu_configure || true
        ;;
      5)
        menu_ops || true
        ;;
      6)
        do_check_update || true
        pause_enter
        ;;
      7)
        do_uninstall || true
        if [ "$is_installed" = "1" ] && [ ! -d "$INSTALL_DIR" ]; then
          return 0
        fi
        pause_enter
        ;;
      0)
        info "已退出 ${APP_LABEL} 管理菜单。"
        return 0
        ;;
      *)
        warn "请输入 0 到 7 之间的数字。"
        ;;
    esac
  done
}

# ── 主入口 ─────────────────────────────────────────────────────────────────────
main() {
  parse_args "$@"

  case "$ACTION" in
    help)    show_help; return 0 ;;
    version) printf '%s %s\n' "$APP_NAME" "$SCRIPT_VERSION"; return 0 ;;
  esac

  case "$ACTION" in
    status|check-update|logs|uninstall) NO_DEPS=1 ;;
  esac

  validate_inputs

  if [ -n "$ACTION" ]; then
    case "$ACTION" in
      install)      do_install ;;
      upgrade)      do_upgrade ;;
      status)       do_status ;;
      check-update) do_check_update ;;
      uninstall)    do_uninstall ;;
      logs)         do_logs ;;
      start)        do_lifecycle start ;;
      stop)         do_lifecycle stop ;;
      restart)      do_lifecycle restart ;;
      backup)       do_backup ;;
    esac
    return $?
  fi

  if [ "$ASSUME_YES" = "1" ] || [ "$DRY_RUN" = "1" ]; then
    do_install
    return $?
  fi

  run_interactive_menu
}

main "$@"
