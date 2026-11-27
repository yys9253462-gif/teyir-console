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

SCRIPT_VERSION="1.1.0"
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
SELF_IS_SHORTCUT=0
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

if [ "${0:-}" = "$SHORTCUT_PATH" ] || [ "$SELF" = "$(basename "$SHORTCUT_PATH")" ]; then
  SELF_IS_SHORTCUT=1
fi
if [ -n "${0:-}" ] && [ -f "${0:-}" ]; then
  SELF_PATH="$(resolve_realpath "$0" 2>/dev/null || printf '%s' "$0")"
fi
case "$SELF" in
  bash|sh|dash|ash|zsh|ksh|"") SELF="install.sh"; SELF_PATH=""; SELF_IS_SHORTCUT=0 ;;
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
SOURCE_COMMIT=""

# 显式命令行/交互传参标记（用于区分「显式指定」「显式清空」与「状态文件回填」）
FLAG_REPO=0
FLAG_REF=0
FLAG_PROJECT=0
FLAG_ACCESS_MODE=0
FLAG_DOMAIN=0
FLAG_DOMAIN_SET=0
FLAG_NO_DOMAIN_SET=0
FLAG_WEB_BIND=0
FLAG_WEB_PORT=0
FLAG_WEB_TLS_PORT=0
FLAG_API_BIND=0
FLAG_API_PORT=0
FLAG_SUB_NAME=0
FLAG_COLLECTOR=0
FLAG_ADMIN_EMAIL=0
FLAG_ADMIN_PASSWORD=0
FLAG_STORE_RENEW=0
FLAG_HY2_SCRIPT=0
FLAG_HY2_REPO=0

# 动作开关与行为选项
ACTION=""               # "" (菜单) | install | upgrade | status | check-update | uninstall | logs | start | stop | restart | backup | help | version
LOGS_SERVICE="all"
ASSUME_YES=0
DRY_RUN=0
SKIP_DNS_CHECK=0
NO_DEPS=0
NO_SHORTCUT=0
PURGE_DATA=0

# 探测、回滚与菜单桥接状态
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
RESERVED_TCP_RANGES=""
RESERVED_UDP_RANGES=""
PUBLIC_IP_CACHE=""
LAST_FAIL_KIND=""
NON_TTY_NOTICED=0
IS_NEW_ADMIN_SEEDED=0
EXISTING_ENV_DETECTED=0
LAST_MENU_RC=0
MENU_DIR_BRIDGE=""
TMP_FILES=()

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
SAVED_SOURCE_COMMIT=""

# ── 临时文件追踪、备份轮转与中断/失败自动回滚保护 ──────────────────────────────
register_tmp() {
  TMP_FILES+=("$1")
}

cleanup_tmp_files() {
  if [ "${#TMP_FILES[@]}" -eq 0 ]; then
    return 0
  fi
  local f
  for f in "${TMP_FILES[@]}"; do
    if [ -n "$f" ] && [ -e "$f" ]; then
      rm -rf "$f" 2>/dev/null || true
    fi
  done
  TMP_FILES=()
  return 0
}

# 安全保留最新 keep 份备份：零匹配不触发 pipefail，不依赖 xargs -r，兼容路径含空格
prune_backups() {
  local keep="$1" dir="$2" prefix="$3" suffix="$4"
  [ -d "$dir" ] || return 0
  local -a files=()
  local old_nullglob=0
  shopt -q nullglob && old_nullglob=1
  shopt -s nullglob
  files=("${dir}/${prefix}"*"${suffix}")
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

prune_caddy_backups() {
  local target="${1:-$CADDY_FILE}"
  [ -n "$target" ] || return 0
  prune_backups 3 "$(dirname "$target")" "$(basename "$target").bak-" "-preTeyir"
}

restore_caddy() {
  if [ "$CADDY_BACKED_UP" != "1" ]; then return 0; fi
  if [ -z "$CADDY_BACKUP_FILE" ] || [ ! -f "$CADDY_BACKUP_FILE" ]; then return 0; fi
  if [ -z "$CADDY_FILE" ]; then return 0; fi
  CADDY_BACKED_UP=0
  warn "正在还原共享反代配置文件：$CADDY_FILE"
  if cat "$CADDY_BACKUP_FILE" > "$CADDY_FILE" 2>/dev/null; then
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
  if [ -f "$INSTALL_DIR/.env" ]; then
    COMPOSE_PROJECT_NAME="$PROJECT_NAME" docker compose \
      -f "$INSTALL_DIR/docker-compose.yml" \
      --env-file "$INSTALL_DIR/.env" \
      down --remove-orphans >/dev/null 2>&1 || true
  fi
}

on_signal() {
  trap - EXIT INT TERM
  printf '\n' >&2
  warn "收到中断信号，正在执行安全收尾..."
  restore_caddy
  rollback_fresh_containers
  cleanup_tmp_files
  dim "说明：共享反代配置（若本次有改动）已还原；若刚才已拉取源码或生成配置，重跑脚本可继续接上。"
  exit 130
}

on_fail() {
  local rc=$?
  trap - EXIT INT TERM
  if [ "$rc" -ne 0 ]; then
    restore_caddy
    rollback_fresh_containers
  fi
  cleanup_tmp_files
  return "$rc"
}

trap on_signal INT TERM
trap on_fail EXIT

die() {
  err "$*"
  exit 1
}

# ── 交互终端读取工具（兼容管道但有控制终端、中文回答与 EOF 安全语义）───────────
can_prompt() {
  if [ "$ASSUME_YES" = "1" ]; then
    return 1
  fi
  if [ "${TEYIR_FORCE_STDIN_TTY:-0}" = "1" ]; then
    return 0
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
  if [ "${TEYIR_FORCE_STDIN_TTY:-0}" = "1" ] || [ -t 0 ]; then
    if [ "$__silent" = "1" ] && [ -t 0 ]; then
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

# 可选配置读取：已有值时直接回车保留、输入 - 显式清空
ask_optional() {
  local prompt="$1" cur="$2" ans=""
  if ! can_prompt; then
    notify_non_interactive_once
    printf '%s' "$cur"
    return 0
  fi
  if [ -n "$cur" ]; then
    printf '  %s [当前: %s | 回车保留，输入 - 清空]: ' "$prompt" "$cur" >&2
  else
    printf '  %s [直接回车留空跳过]: ' "$prompt" >&2
  fi
  if ! tty_read ans 0; then
    printf '\n' >&2
    dim "（读不到输入，保持当前配置）"
    printf '%s' "$cur"
    return 0
  fi
  if [ "$ans" = "-" ]; then
    printf ''
    return 0
  fi
  printf '%s' "${ans:-$cur}"
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
  is_valid_port "$def" || def="$DEFAULT_LOCAL_WEB_PORT"
  if ! can_prompt; then
    notify_non_interactive_once
    printf '%s' "$def"
    return 0
  fi
  while :; do
    printf '  %s [%s]: ' "$prompt" "$def" >&2
    if ! tty_read ans 0; then
      printf '\n' >&2
      dim "（读不到输入，自动采用默认端口：$def）"
      printf '%s' "$def"
      return 0
    fi
    ans="${ans:-$def}"
    if is_valid_port "$ans"; then
      printf '%s' "$ans"
      return 0
    fi
    warn "端口必须是 1 到 65535 之间且不含前导零的整数，请重新输入（直接回车使用 $def）"
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
  local i=1 ans="" o
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
    if printf '%s' "$ans" | grep -qE '^[1-9][0-9]*$' && [ "$ans" -ge 1 ] && [ "$ans" -le "${#opts[@]}" ]; then
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

# ── 基础校验与随机密钥生成（纯 Bash 内置判定，零外部进程开销）────────────────────
is_valid_port() {
  local p="${1:-}"
  [[ "$p" =~ ^[1-9][0-9]{0,4}$ ]] && [ "$p" -ge 1 ] && [ "$p" -le 65535 ]
}

clean_domain() {
  local d="$1"
  d="${d//[[:space:]]/}"
  d="${d,,}"
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
  local LC_ALL=C d="$1"
  [ -n "$d" ] && [ "${#d}" -le 253 ] || return 1
  local re='^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+([a-z]{2,63}|xn--[a-z0-9-]{2,59}[a-z0-9])$'
  [[ "$d" =~ $re ]]
}

is_valid_email() {
  local e="$1"
  [ -n "$e" ] || return 1
  local re_ctrl='[[:cntrl:]]'
  [[ ! "$e" =~ $re_ctrl ]] || return 1
  local re='^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$'
  [[ "$e" =~ $re ]]
}

is_valid_project_name() {
  local LC_ALL=C re='^[a-z0-9][a-z0-9_-]{0,48}$'
  [[ "${1:-}" =~ $re ]]
}

is_valid_ref() {
  local LC_ALL=C r="${1:-}"
  [ -n "$r" ] || return 1
  case "$r" in
    -*|/*|*/|*..*|*//*|*.lock|*@\{*) return 1 ;;
  esac
  local re='^[A-Za-z0-9._/-]+$'
  [[ "$r" =~ $re ]]
}

is_valid_ipv4() {
  local LC_ALL=C ip="$1" re='^([0-9]{1,3}\.){3}[0-9]{1,3}$'
  [[ "$ip" =~ $re ]] || return 1
  local IFS='.' oct
  local -a parts
  read -r -a parts <<< "$ip"
  [ "${#parts[@]}" -eq 4 ] || return 1
  for oct in "${parts[@]}"; do
    case "$oct" in
      0) ;;
      0*) return 1 ;;
    esac
    [ "$oct" -ge 0 ] && [ "$oct" -le 255 ] || return 1
  done
  return 0
}

is_valid_bind_ip() {
  local LC_ALL=C b="$1"
  case "$b" in
    127.0.0.1|0.0.0.0|::1|::) return 0 ;;
  esac
  if is_valid_ipv4 "$b"; then
    return 0
  fi
  # 兼容标准 IPv6 字面量（至少含 2 个冒号、仅十六进制与冒号、无三连冒号）
  case "$b" in
    *:::*) return 1 ;;
    *:*:*) local re='^[0-9A-Fa-f:]+$'; [[ "$b" =~ $re ]] ;;
    *) return 1 ;;
  esac
}

is_safe_install_dir() {
  local d="$1"
  case "$d" in
    ""|"/"|"/bin"|"/boot"|"/dev"|"/etc"|"/home"|"/lib"|"/lib64"|"/opt"|"/proc"|"/root"|"/run"|"/sbin"|"/srv"|"/sys"|"/tmp"|"/usr"|"/var")
      return 1 ;;
    /*)
      case "${d}/" in
        */../*|*/./*|*//*) return 1 ;;
      esac
      local re_bad='[[:space:][:cntrl:]"'\''`$;|&<>]'
      [[ ! "$d" =~ $re_bad ]]
      ;;
    *)
      return 1 ;;
  esac
}

is_valid_http_url() {
  local re='^https?://[^[:space:][:cntrl:]]+$'
  [[ "${1:-}" =~ $re ]]
}

is_valid_https_url() {
  local re='^https://[^[:space:][:cntrl:]]+$'
  [[ "${1:-}" =~ $re ]]
}

is_valid_repo_slug() {
  local LC_ALL=C re='^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$'
  [[ "${1:-}" =~ $re ]]
}

# CREDENTIAL_KEY 必须为 32 字节标准 Base64（Go base64.StdEncoding 固定 44 字符且末位为 =）
is_valid_credential_key() {
  local LC_ALL=C k="${1:-}" re='^[A-Za-z0-9+/]{42}[AEIMQUYcgkosw048]=$'
  [[ "$k" =~ $re ]] || return 1
  if command -v base64 >/dev/null 2>&1; then
    local n
    n="$(printf '%s' "$k" | base64 -d 2>/dev/null | wc -c | tr -d '[:space:]' || echo 0)"
    [ "$n" = "32" ] || return 1
  fi
  return 0
}

gen_urlsafe_secret() {
  local len="${1:-48}" out=""
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
  local len="${1:-16}" out=""
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

gen_credential_key() {
  local out=""
  if command -v openssl >/dev/null 2>&1; then
    out="$(openssl rand -base64 32 2>/dev/null | tr -d '\r\n' || true)"
  fi
  if ! is_valid_credential_key "$out" && command -v python3 >/dev/null 2>&1; then
    out="$(python3 -c "import os, base64; print(base64.b64encode(os.urandom(32)).decode())" 2>/dev/null | tr -d '\r\n' || true)"
  fi
  if ! is_valid_credential_key "$out" && command -v base64 >/dev/null 2>&1; then
    out="$(head -c 32 /dev/urandom 2>/dev/null | base64 | tr -d '\r\n' || true)"
  fi
  is_valid_credential_key "$out" || die "无法生成标准 32 字节 Base64 凭据主密钥（CREDENTIAL_KEY）"
  printf '%s' "$out"
}

# ── 安全键值读写（严禁 source 配置文件，防命令注入，零子进程单遍解析）───────────
parse_kv_value_into() {
  local __target="$1" raw="$2"
  raw="${raw#"${raw%%[![:space:]]*}"}"
  raw="${raw%"${raw##*[![:space:]]}"}"
  if [ -z "$raw" ]; then
    printf -v "$__target" '%s' ""
    return 0
  fi

  local first="${raw:0:1}"
  if [ "$first" = "'" ]; then
    # 单引号字面量：取到下一个单引号为止（闭合后的行内注释自动忽略）
    local rest="${raw:1}"
    if [[ "$rest" == *"'"* ]]; then
      printf -v "$__target" '%s' "${rest%%\'*}"
    else
      printf -v "$__target" '%s' "$rest"
    fi
    return 0
  fi

  if [ "$first" = '"' ]; then
    # 双引号值：解析 \"、\\、\$ 转义直到未转义的闭合双引号
    local i=1 len="${#raw}" ch next out=""
    while [ "$i" -lt "$len" ]; do
      ch="${raw:i:1}"
      if [ "$ch" = '\' ] && [ $((i + 1)) -lt "$len" ]; then
        next="${raw:i+1:1}"
        case "$next" in
          '"'|'\'|'$') out+="$next"; i=$((i + 2)); continue ;;
        esac
      fi
      if [ "$ch" = '"' ]; then
        break
      fi
      out+="$ch"
      i=$((i + 1))
    done
    printf -v "$__target" '%s' "$out"
    return 0
  fi

  # 无引号值：仅剥离「空白 + #」形式的行内注释，保留值内紧贴的 #（如 Pass#123）
  raw="${raw%%[[:space:]]#*}"
  raw="${raw%"${raw##*[![:space:]]}"}"
  printf -v "$__target" '%s' "$raw"
}

parse_kv_value() {
  local _v=""
  parse_kv_value_into _v "$1"
  printf '%s' "$_v"
}

# 将任意值编码为 Docker Compose .env 安全格式（纯 Bash 零子进程；含 $ / 空格 / # / 引号 / 反斜杠均无损往返）
format_env_value() {
  local LC_ALL=C v="$1"
  v="${v//$'\r'/}"
  v="${v//$'\n'/}"
  if [ -z "$v" ]; then
    return 0
  fi
  local re='^[A-Za-z0-9_./:@,+=-]+$'
  if [[ "$v" =~ $re ]]; then
    printf '%s' "$v"
    return 0
  fi
  if [[ "$v" != *"'"* ]]; then
    printf "'%s'" "$v"
    return 0
  fi
  # 同时包含单引号时退回双引号并转义反斜杠、双引号与美元符
  local esc="$v"
  esc="${esc//\\/\\\\}"
  esc="${esc//\"/\\\"}"
  esc="${esc//\$/\\\$}"
  printf '"%s"' "$esc"
}

emit_env_kv() {
  printf '%s=' "$1"
  format_env_value "$2"
  printf '\n'
}

emit_conf_kv() {
  local v="$2"
  v="${v//$'\r'/}"
  v="${v//$'\n'/}"
  printf '%s=%s\n' "$1" "$v"
}

read_kv_file() {
  local file="$1" key="$2"
  if [ ! -f "$file" ] || [ ! -r "$file" ]; then
    return 0
  fi
  local line trimmed k found=""
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    trimmed="${line#"${line%%[![:space:]]*}"}"
    case "$trimmed" in
      ""|\#*) continue ;;
      export[[:space:]]*)
        trimmed="${trimmed#export}"
        trimmed="${trimmed#"${trimmed%%[![:space:]]*}"}"
        ;;
    esac
    case "$trimmed" in
      *=*) ;;
      *) continue ;;
    esac
    k="${trimmed%%=*}"
    k="${k%"${k##*[![:space:]]}"}"
    if [ "$k" != "$key" ]; then
      continue
    fi
    parse_kv_value_into found "${trimmed#*=}"
  done < "$file"
  printf '%s' "$found"
}

is_managed_env_key() {
  case "$1" in
    COMPOSE_PROJECT_NAME|WEB_INGRESS_NETWORK|POSTGRES_PASSWORD|JWT_SECRET|JWT_TTL_MINUTES|\
    SEED_ADMIN_EMAIL|SEED_ADMIN_PASSWORD|CREDENTIAL_KEY|ALLOWED_ORIGIN|TRUSTED_PROXY_CIDRS|\
    API_BIND|API_PORT|WEB_BIND|WEB_PORT|WEB_TLS_PORT|SITE_ADDRESS|\
    ENABLE_COLLECTOR|COLLECTOR_INTERVAL_SECONDS|EXECUTOR_INTERVAL_SECONDS|SPEEDTEST_INTERVAL_MINUTES|\
    MIGRATIONS_DIR|MIGRATE_ASSUME_BASELINE|\
    GEO_OFFLINE_DB|GEO_ENABLE_API|GEO_API_BASE_URL|\
    IP_PURITY_OFFLINE_DB|IP_PURITY_API_KEY|IP_PURITY_IPQS_KEY|IP_PURITY_API_BASE_URL|\
    PORTAL_TLS_PINS|VIEWER_RESTRICT_SUBSCRIPTIONS|\
    PANEL_PUBLIC_URL|SUBSCRIPTION_PUBLIC_BASE_URL|SUBSCRIPTION_BACKUP_BASE_URLS|SUBSCRIPTION_DEFAULT_NAME|\
    STORE_RENEW_BASE_URL|TURNSTILE_SITE_KEY|TURNSTILE_SECRET|LOGIN_SERVICE_KEY|LOGIN_SERVICE_EMAIL|\
    BAN_WEBHOOK_URL|BAN_WEBHOOK_TOKEN|HY2_INSTALL_SCRIPT_URL|HY2_INSTALLER_REPO)
      return 0 ;;
    *) return 1 ;;
  esac
}

# 单遍读取 .env：同时将受管变量填入 OLD_ENV_<KEY> 并将非托管自定义行收集到 CUSTOM_ENV_LINES
CUSTOM_ENV_LINES=()
load_env_snapshot() {
  local file="$1"
  local LC_ALL=C re_key='^[A-Za-z_][A-Za-z0-9_]*$'
  local k
  for k in COMPOSE_PROJECT_NAME WEB_INGRESS_NETWORK POSTGRES_PASSWORD JWT_SECRET JWT_TTL_MINUTES CREDENTIAL_KEY \
    SEED_ADMIN_EMAIL SEED_ADMIN_PASSWORD ALLOWED_ORIGIN TRUSTED_PROXY_CIDRS \
    API_BIND API_PORT WEB_BIND WEB_PORT WEB_TLS_PORT SITE_ADDRESS \
    ENABLE_COLLECTOR COLLECTOR_INTERVAL_SECONDS EXECUTOR_INTERVAL_SECONDS SPEEDTEST_INTERVAL_MINUTES \
    MIGRATIONS_DIR MIGRATE_ASSUME_BASELINE \
    GEO_OFFLINE_DB GEO_ENABLE_API GEO_API_BASE_URL \
    IP_PURITY_OFFLINE_DB IP_PURITY_API_KEY IP_PURITY_IPQS_KEY IP_PURITY_API_BASE_URL \
    PORTAL_TLS_PINS VIEWER_RESTRICT_SUBSCRIPTIONS \
    PANEL_PUBLIC_URL SUBSCRIPTION_PUBLIC_BASE_URL SUBSCRIPTION_BACKUP_BASE_URLS SUBSCRIPTION_DEFAULT_NAME \
    STORE_RENEW_BASE_URL TURNSTILE_SITE_KEY TURNSTILE_SECRET LOGIN_SERVICE_KEY LOGIN_SERVICE_EMAIL \
    BAN_WEBHOOK_URL BAN_WEBHOOK_TOKEN HY2_INSTALL_SCRIPT_URL HY2_INSTALLER_REPO; do
    printf -v "OLD_ENV_${k}" '%s' ""
  done
  CUSTOM_ENV_LINES=()
  [ -f "$file" ] && [ -r "$file" ] || return 0

  local line trimmed raw_k val i found_idx
  local -a c_keys=()
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    trimmed="${line#"${line%%[![:space:]]*}"}"
    case "$trimmed" in
      ""|\#*) continue ;;
      export[[:space:]]*)
        trimmed="${trimmed#export}"
        trimmed="${trimmed#"${trimmed%%[![:space:]]*}"}"
        ;;
    esac
    case "$trimmed" in
      *=*) ;;
      *) continue ;;
    esac
    raw_k="${trimmed%%=*}"
    raw_k="${raw_k%"${raw_k##*[![:space:]]}"}"
    [[ "$raw_k" =~ $re_key ]] || continue
    if is_managed_env_key "$raw_k"; then
      parse_kv_value_into val "${trimmed#*=}"
      printf -v "OLD_ENV_${raw_k}" '%s' "$val"
    else
      found_idx=-1
      for i in "${!c_keys[@]}"; do
        if [ "${c_keys[$i]}" = "$raw_k" ]; then
          found_idx="$i"
          break
        fi
      done
      if [ "$found_idx" -ge 0 ]; then
        CUSTOM_ENV_LINES[$found_idx]="$line"
      else
        c_keys+=("$raw_k")
        CUSTOM_ENV_LINES+=("$line")
      fi
    fi
  done < "$file"
  return 0
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
  SAVED_SOURCE_COMMIT=""
}

load_saved_state() {
  local conf="$INSTALL_DIR/install.conf"
  local env_file="$INSTALL_DIR/.env"
  reset_saved_vars

  if [ -f "$conf" ] && [ -r "$conf" ]; then
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
      k="${k#"${k%%[![:space:]]*}"}"
      k="${k%"${k##*[![:space:]]}"}"
      parse_kv_value_into v "${line#*=}"
      case "$k" in
        PROJECT_NAME|REPO|BRANCH|ACCESS_MODE|DOMAIN|WEB_BIND|WEB_PORT|WEB_TLS_PORT|API_BIND|API_PORT|SUB_NAME|ENABLE_COLLECTOR|CADDY_MODE|CADDY_CONTAINER|CADDY_FILE|CADDY_INNER|CADDY_NET|UPSTREAM_HOST|SCRIPT_VERSION|INSTALLED_AT|SOURCE_COMMIT)
          printf -v "SAVED_${k}" '%s' "$v"
          valid_keys=$((valid_keys + 1))
          ;;
      esac
    done < "$conf"
    if [ "$non_empty" -gt 0 ] && [ "$valid_keys" -eq 0 ]; then
      warn "检测到状态文件 $conf 可能已损坏（未解析出有效配置项），将采用默认配置"
    elif [ "$valid_keys" -gt 0 ]; then
      SAVED_LOADED=1
      return 0
    fi
  fi

  # 兼容手工 Docker Compose 部署（仅有 .env 尚无 install.conf）：单遍读取 .env 推断基础状态
  if [ -f "$env_file" ] && [ -r "$env_file" ]; then
    load_env_snapshot "$env_file"
    SAVED_PROJECT_NAME="${OLD_ENV_COMPOSE_PROJECT_NAME:-$DEFAULT_PROJECT}"
    SAVED_WEB_BIND="${OLD_ENV_WEB_BIND:-127.0.0.1}"
    SAVED_WEB_PORT="${OLD_ENV_WEB_PORT:-80}"
    SAVED_WEB_TLS_PORT="${OLD_ENV_WEB_TLS_PORT:-443}"
    SAVED_API_BIND="${OLD_ENV_API_BIND:-127.0.0.1}"
    SAVED_API_PORT="${OLD_ENV_API_PORT:-$DEFAULT_API_PORT}"
    SAVED_SUB_NAME="${OLD_ENV_SUBSCRIPTION_DEFAULT_NAME:-$DEFAULT_SUB_NAME}"
    SAVED_ENABLE_COLLECTOR="${OLD_ENV_ENABLE_COLLECTOR:-false}"
    if [ -n "$OLD_ENV_SITE_ADDRESS" ]; then
      SAVED_DOMAIN="$(clean_domain "$OLD_ENV_SITE_ADDRESS")"
      SAVED_ACCESS_MODE="self"
    elif [ -n "$OLD_ENV_PANEL_PUBLIC_URL" ]; then
      SAVED_DOMAIN="$(clean_domain "$OLD_ENV_PANEL_PUBLIC_URL")"
      SAVED_ACCESS_MODE="local"
    elif [ "$SAVED_WEB_BIND" = "0.0.0.0" ] || [ "$SAVED_WEB_BIND" = "::" ]; then
      SAVED_ACCESS_MODE="direct"
    else
      SAVED_ACCESS_MODE="local"
    fi
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
  # 显式指定 --no-domain 或切换到无域名直连模式（--access-mode direct）时不回填旧域名
  if [ "$NO_DOMAIN" = "1" ] || { [ "$FLAG_ACCESS_MODE" = "1" ] && [ "$ACCESS_MODE" = "direct" ] && [ "$FLAG_DOMAIN" = "0" ]; }; then
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
  if [ -z "$SOURCE_COMMIT" ] && [ -n "$SAVED_SOURCE_COMMIT" ]; then SOURCE_COMMIT="$SAVED_SOURCE_COMMIT"; fi
}

save_state() {
  local conf="$INSTALL_DIR/install.conf"
  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将写入状态文件 $conf（权限 600）并维护脚本与快捷命令"
    return 0
  fi
  local now commit_val="$SOURCE_COMMIT"
  now="$(date '+%Y-%m-%d %H:%M:%S')"
  if [ -d "$INSTALL_DIR/.git" ]; then
    commit_val="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || printf '%s' "$commit_val")"
  fi
  SOURCE_COMMIT="$commit_val"

  local tmp="${conf}.tmp.$$"
  register_tmp "$tmp"
  {
    printf '%s\n' "# Teyir 控制台安装状态文件（由 install.sh 自动维护，请勿手工执行）"
    emit_conf_kv SCRIPT_VERSION "$SCRIPT_VERSION"
    emit_conf_kv INSTALLED_AT "$now"
    emit_conf_kv PROJECT_NAME "$PROJECT_NAME"
    emit_conf_kv REPO "$APP_REPO"
    emit_conf_kv BRANCH "$APP_REF"
    emit_conf_kv SOURCE_COMMIT "$SOURCE_COMMIT"
    emit_conf_kv ACCESS_MODE "$ACCESS_MODE"
    emit_conf_kv DOMAIN "$DOMAIN"
    emit_conf_kv WEB_BIND "$WEB_BIND"
    emit_conf_kv WEB_PORT "$WEB_PORT"
    emit_conf_kv WEB_TLS_PORT "$WEB_TLS_PORT"
    emit_conf_kv API_BIND "$API_BIND"
    emit_conf_kv API_PORT "$API_PORT"
    emit_env_kv  SUB_NAME "$SUB_NAME"
    emit_conf_kv ENABLE_COLLECTOR "$ENABLE_COLLECTOR"
    emit_conf_kv CADDY_MODE "$CADDY_MODE"
    emit_conf_kv CADDY_CONTAINER "$CADDY_CONTAINER"
    emit_env_kv  CADDY_FILE "$CADDY_FILE"
    emit_env_kv  CADDY_INNER "$CADDY_INNER"
    emit_conf_kv CADDY_NET "$CADDY_NET"
    emit_conf_kv UPSTREAM_HOST "$UPSTREAM_HOST"
  } > "$tmp"
  chmod 600 "$tmp"
  mv -f "$tmp" "$conf"

  # 仅当安装目录尚无 install.sh 时才补存副本，绝不用外部旧副本覆写刚拉取的仓库新版脚本
  local dst="$INSTALL_DIR/install.sh"
  if [ ! -f "$dst" ] && [ -n "$SELF_PATH" ] && [ -f "$SELF_PATH" ]; then
    cp -f "$SELF_PATH" "$dst"
  fi
  if [ -f "$dst" ]; then
    chmod 755 "$dst"
  fi

  # 安装全局快捷命令 teyir（方便用户随时输入 teyir 打开交互菜单）
  if [ "$NO_SHORTCUT" != "1" ] && [ -f "$dst" ] && [ "$(id -u)" -eq 0 ]; then
    if [ ! -e "$SHORTCUT_PATH" ] || [ -L "$SHORTCUT_PATH" ]; then
      mkdir -p "$(dirname "$SHORTCUT_PATH")"
      ln -sfn "$dst" "$SHORTCUT_PATH"
    fi
  fi
  STATE_SAVED=1
  load_saved_state
  if [ -n "$MENU_DIR_BRIDGE" ]; then
    printf '%s\n' "$INSTALL_DIR" > "$MENU_DIR_BRIDGE" 2>/dev/null || true
  fi
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
  if [ "$SELF_IS_SHORTCUT" = "1" ] || { [ -L "$SHORTCUT_PATH" ] && [ -x "$SHORTCUT_PATH" ]; }; then
    base="$(basename "$SHORTCUT_PATH")"
  elif [ -f "$INSTALL_DIR/install.sh" ]; then
    base="bash $INSTALL_DIR/install.sh"
  elif [ -n "$SELF_PATH" ] && [ -f "$SELF_PATH" ]; then
    base="bash $SELF_PATH"
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
  [ "${#raw}" -le 40 ] || return 0
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
  local cmd_prefix="bash ${SELF}"
  if [ "$SELF_IS_SHORTCUT" = "1" ]; then
    cmd_prefix="$(basename "$SHORTCUT_PATH")"
  fi
  cat <<EOF
${APP_LABEL}（${APP_NAME}）一键安装与交互管理脚本 v${SCRIPT_VERSION}

用法：
  ${cmd_prefix}                         打开交互式主菜单（推荐）
  ${cmd_prefix} --yes                   全自动按默认值安装（无需交互）
  ${cmd_prefix} --dry-run --install     预演安装计划，不对系统做任何修改
  ${cmd_prefix} --status                查看服务运行状态与访问信息（无需管理员权限）
  ${cmd_prefix} --upgrade               备份数据、拉取最新代码、迁移数据库并重建服务
  ${cmd_prefix} --check-update          检查远端仓库是否有新提交
  ${cmd_prefix} --logs [api|web|db|all] 查看容器最近日志
  ${cmd_prefix} --start | --stop | --restart   启停或重启服务
  ${cmd_prefix} --backup                立即备份配置与 PostgreSQL 数据库
  ${cmd_prefix} --uninstall [--purge]   卸载服务（默认保留数据卷与目录；--purge 彻底清理）

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
  local opt="$1" argc="$2" next_val="${3:-}" from_eq="${4:-0}"
  [ "$argc" -ge 2 ] || die "参数 ${opt} 需要一个值（例如：${opt} <值>）"
  if [ "$from_eq" != "1" ] && is_known_flag "$next_val"; then
    die "参数 ${opt} 需要一个值，但紧跟的是另一个选项 '${next_val}' —— 是不是漏写了 ${opt} 的值？"
  fi
}

parse_args() {
  local opt val from_eq
  while [ $# -gt 0 ]; do
    opt="$1"
    val=""
    from_eq=0
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
        from_eq=1
        set -- "$opt" "$val" "${@:2}"
        ;;
    esac

    case "$1" in
      --dir|--repo|--branch|--project-name|--access-mode|--domain|--web-bind|--web-port|--web-tls-port|--api-bind|--api-port|--admin-email|--admin-password|--sub-name|--collector|--build-network)
        require_value "$1" "$#" "${2:-}" "$from_eq"
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
        if [ $# -ge 2 ] && { [ "$from_eq" = "1" ] || ! is_known_flag "$2"; }; then
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
      --no-domain)       NO_DOMAIN=1; DOMAIN=""; FLAG_DOMAIN=1; FLAG_NO_DOMAIN_SET=1; shift ;;
      --skip-dns-check)  SKIP_DNS_CHECK=1; shift ;;
      --no-deps)         NO_DEPS=1; shift ;;
      --no-shortcut)     NO_SHORTCUT=1; shift ;;
      --purge)           PURGE_DATA=1; shift ;;
      --dir)
        if [ "$2" = "/" ]; then INSTALL_DIR="/"; else INSTALL_DIR="${2%/}"; fi
        shift 2
        ;;
      --repo)            APP_REPO="$2"; FLAG_REPO=1; shift 2 ;;
      --branch)          APP_REF="$2"; FLAG_REF=1; shift 2 ;;
      --project-name)    PROJECT_NAME="$2"; FLAG_PROJECT=1; shift 2 ;;
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
        FLAG_DOMAIN_SET=1
        shift 2
        ;;
      --web-bind)        WEB_BIND="$2"; FLAG_WEB_BIND=1; shift 2 ;;
      --web-port)        WEB_PORT="$2"; FLAG_WEB_PORT=1; shift 2 ;;
      --web-tls-port)    WEB_TLS_PORT="$2"; FLAG_WEB_TLS_PORT=1; shift 2 ;;
      --api-bind)        API_BIND="$2"; FLAG_API_BIND=1; shift 2 ;;
      --api-port)        API_PORT="$2"; FLAG_API_PORT=1; shift 2 ;;
      --admin-email)     ADMIN_EMAIL="$2"; FLAG_ADMIN_EMAIL=1; shift 2 ;;
      --admin-password)  ADMIN_PASSWORD="$2"; FLAG_ADMIN_PASSWORD=1; shift 2 ;;
      --sub-name)        SUB_NAME="$2"; FLAG_SUB_NAME=1; shift 2 ;;
      --collector)
        case "$2" in
          on|true|1|yes|是)  ENABLE_COLLECTOR="true"; FLAG_COLLECTOR=1 ;;
          off|false|0|no|否) ENABLE_COLLECTOR="false"; FLAG_COLLECTOR=1 ;;
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
          die "不认识的参数 '$1' —— 你是不是想输入 '${sug}'？（运行 $(render_self_cmd --help) 查看完整用法）"
        fi
        die "不认识的参数 '$1'（运行 $(render_self_cmd --help) 查看完整用法）"
        ;;
    esac
  done

  if [ "$FLAG_DOMAIN_SET" = "1" ] && [ "$FLAG_NO_DOMAIN_SET" = "1" ]; then
    die "不能同时指定 --domain 与 --no-domain"
  fi
  if [ "$NO_DOMAIN" = "1" ]; then
    case "$ACCESS_MODE" in
      self|host-caddy|docker-caddy)
        die "--no-domain 不能与 --access-mode ${ACCESS_MODE} 同时使用"
        ;;
    esac
  fi
  if [ "$FLAG_DOMAIN_SET" = "1" ] && [ "$ACCESS_MODE" = "direct" ]; then
    die "--access-mode direct 为无域名端口直连模式，不能同时指定 --domain（绑定域名请使用 --access-mode auto 或 local）"
  fi
}

validate_inputs() {
  is_safe_install_dir "$INSTALL_DIR" || die "安装目录不合法或不安全（须为非系统根目录、不含 .. 的绝对路径）：$INSTALL_DIR"
  is_valid_project_name "$PROJECT_NAME" || die "项目名仅支持小写字母、数字、连字符与下划线，且须以字母或数字开头：$PROJECT_NAME"
  is_valid_repo_slug "$APP_REPO" || die "仓库地址格式应为 owner/repo，收到：$APP_REPO"
  is_valid_ref "$APP_REF" || die "分支或标签名不合法（禁止连字符开头或含 .. / //）：$APP_REF"
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
    is_valid_port "$WEB_PORT" || die "前端 HTTP 端口非法（1–65535 且无前导零，或 auto）：$WEB_PORT"
  fi
  if [ -n "$WEB_TLS_PORT" ] && [ "$WEB_TLS_PORT" != "auto" ]; then
    is_valid_port "$WEB_TLS_PORT" || die "前端 HTTPS 端口非法（1–65535 且无前导零，或 auto）：$WEB_TLS_PORT"
  fi
  if [ -n "$API_PORT" ] && [ "$API_PORT" != "auto" ]; then
    is_valid_port "$API_PORT" || die "后端 API 端口非法（1–65535 且无前导零，或 auto）：$API_PORT"
  fi
  if [ -n "$ADMIN_EMAIL" ]; then
    is_valid_email "$ADMIN_EMAIL" || die "管理员邮箱格式非法：$ADMIN_EMAIL"
  fi
  if [ -n "$ADMIN_PASSWORD" ]; then
    [ "${#ADMIN_PASSWORD}" -ge 12 ] || die "管理员密码至少需要 12 位字符（当前 ${#ADMIN_PASSWORD} 位）"
    ! printf '%s' "$ADMIN_PASSWORD" | grep -q '[[:cntrl:]]' || die "管理员密码不能包含控制字符"
  fi
  if [ -n "$SUB_NAME" ]; then
    ! printf '%s' "$SUB_NAME" | grep -q '[[:cntrl:]]' || die "默认订阅分组名不能包含控制字符"
  fi
  if [ -n "$STORE_RENEW_BASE_URL" ]; then
    is_valid_http_url "$STORE_RENEW_BASE_URL" || die "外部商城续费地址须以 http:// 或 https:// 开头：$STORE_RENEW_BASE_URL"
  fi
  if [ -n "$HY2_INSTALL_SCRIPT_URL" ]; then
    is_valid_https_url "$HY2_INSTALL_SCRIPT_URL" || die "节点一键安装脚本地址须为 https:// 链接：$HY2_INSTALL_SCRIPT_URL"
  fi
  if [ -n "$HY2_INSTALLER_REPO" ]; then
    is_valid_repo_slug "$HY2_INSTALLER_REPO" || die "节点门户更新来源仓库格式应为 owner/repo，收到：$HY2_INSTALLER_REPO"
  fi
}

# ── 权限与系统依赖探测 / 自动安装 ──────────────────────────────────────────────
require_root() {
  if [ "$DRY_RUN" = "1" ]; then
    return 0
  fi
  if [ "$(id -u)" -ne 0 ]; then
    err "该操作需要管理员权限（root）以管理容器、服务目录或反代配置。"
    info "请使用管理员权限重试：$(render_priv_cmd "$@")"
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

docker_bin_ok()    { command -v docker >/dev/null 2>&1; }
docker_daemon_ok() { docker info >/dev/null 2>&1; }
compose_ok()       { docker compose version >/dev/null 2>&1; }

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
  local mgr tmp_script log_file
  mgr="$(detect_pkg_mgr)"
  tmp_script="$(mktemp /tmp/teyir-get-docker.XXXXXX.sh)"
  log_file="$(mktemp /tmp/teyir-docker-install.XXXXXX.log)"
  register_tmp "$tmp_script"
  register_tmp "$log_file"

  # 方式 1：官方一键脚本（下载落地 + 语法校验后再执行）
  info "正在尝试方式 1/4：Docker 官方安装脚本..."
  if curl -fsSL --proto '=https' --tlsv1.2 --max-time 30 https://get.docker.com -o "$tmp_script" 2>"$log_file" \
    && [ -s "$tmp_script" ] && sh -n "$tmp_script" 2>>"$log_file"; then
    sh "$tmp_script" >"$log_file" 2>&1 || true
  fi

  # 方式 2：官方脚本 + 国内镜像兜底（若方式 1 未拉下脚本，再尝试备用镜像地址下载）
  if ! docker_bin_ok; then
    info "正在尝试方式 2/4：Docker 安装脚本（阿里云镜像）..."
    if [ ! -s "$tmp_script" ] || ! sh -n "$tmp_script" 2>/dev/null; then
      curl -fsSL --proto '=https' --tlsv1.2 --max-time 30 https://mirrors.aliyun.com/docker-ce/linux/docker-install.sh -o "$tmp_script" 2>>"$log_file" || true
    fi
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
  if ! docker_bin_ok; then
    info "正在尝试方式 4/4：修复重装 Docker 软件包..."
    case "$mgr" in
      apt)
        DEBIAN_FRONTEND=noninteractive apt-get install --reinstall -y -qq docker-ce docker-ce-cli containerd.io >"$log_file" 2>&1 \
          || DEBIAN_FRONTEND=noninteractive apt-get install --reinstall -y -qq docker.io >"$log_file" 2>&1 || true
        ;;
      dnf|yum)
        "$mgr" reinstall -y -q docker-ce docker-ce-cli >"$log_file" 2>&1 \
          || "$mgr" reinstall -y -q docker >"$log_file" 2>&1 || true
        ;;
    esac
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

  local arch url_arch plugin_dir plugin_bin tmp_bin ver dl_url final_url
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
  register_tmp "$tmp_bin"

  ver="$(curl -fsSL --max-time 15 https://api.github.com/repos/docker/compose/releases/latest 2>/dev/null \
    | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1 || true)"
  ver="${ver:-v2.32.4}"
  dl_url="https://github.com/docker/compose/releases/download/${ver}/docker-compose-linux-${url_arch}"

  final_url="$(curl -fsSL --proto '=https' --tlsv1.2 --max-time 60 -w '%{url_effective}' "$dl_url" -o "$tmp_bin" 2>/dev/null || true)"
  case "$final_url" in
    https://github.com/*|https://objects.githubusercontent.com/*|https://release-assets.githubusercontent.com/*)
      chmod 755 "$tmp_bin"
      if [ -s "$tmp_bin" ] && "$tmp_bin" version >/dev/null 2>&1; then
        mkdir -p "$plugin_dir"
        mv -f "$tmp_bin" "$plugin_bin"
      else
        warn "下载到的 Docker Compose 插件未能通过可执行校验，已丢弃"
        rm -f "$tmp_bin"
      fi
      ;;
    *)
      [ -n "$final_url" ] && warn "Docker Compose 下载跳转来源异常：$final_url"
      rm -f "$tmp_bin"
      ;;
  esac
  compose_ok || die "自动安装 Docker Compose v2 插件失败，请手工安装后重试"
}

check_system_resources() {
  local mem_kb mem_mb disk_kb disk_gb
  if [ -r /proc/meminfo ]; then
    mem_kb="$(awk '/^MemTotal:/{print $2}' /proc/meminfo 2>/dev/null || true)"
    if printf '%s' "$mem_kb" | grep -qE '^[0-9]+$'; then
      mem_mb=$((10#$mem_kb / 1024))
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
    disk_gb=$((10#$disk_kb / 1024 / 1024))
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

# ── 端口扫描、NAT 端口跳跃区间探测与外层反代识别 ──────────────────────────────
# 解析 docker ps {{.Ports}} 中的单端口与端口范围（如 0.0.0.0:8000-8002->... 展开为 8000 8001 8002）
extract_docker_published_ports() {
  awk -F',' '
    {
      for (i = 1; i <= NF; i++) {
        seg = $i
        arrow = index(seg, "->")
        if (arrow == 0) continue
        left = substr(seg, 1, arrow - 1)
        sub(/^.*:/, "", left)
        gsub(/[[:space:]]/, "", left)
        if (left ~ /^[0-9]+-[0-9]+$/) {
          split(left, r, "-")
          s = r[1] + 0; e = r[2] + 0
          if (e >= s && (e - s) <= 2000) {
            for (p = s; p <= e; p++) print p
          } else if (s > 0) {
            print s; print e
          }
        } else if (left ~ /^[0-9]+$/) {
          print left
        }
      }
    }
  '
}

load_nat_reserved_ranges() {
  RESERVED_TCP_RANGES=""
  RESERVED_UDP_RANGES=""
  local rules=""
  if command -v iptables >/dev/null 2>&1; then
    rules="$(iptables -t nat -S PREROUTING 2>/dev/null || true)"
  fi
  if [ -n "$rules" ]; then
    RESERVED_TCP_RANGES="$(printf '%s\n' "$rules" | awk '
      /-j (REDIRECT|DNAT)/ && /-p tcp/ {
        for (i=1; i<=NF; i++) {
          if ($i == "--dport" && (i+1) <= NF) {
            v = $(i+1)
            if (v ~ /^[0-9]+:[0-9]+$/) { gsub(":", "-", v); print v }
            else if (v ~ /^[0-9]+$/) { print v "-" v }
          }
        }
      }
    ' | sort -u || true)"
    RESERVED_UDP_RANGES="$(printf '%s\n' "$rules" | awk '
      /-j (REDIRECT|DNAT)/ && /-p udp/ {
        for (i=1; i<=NF; i++) {
          if ($i == "--dport" && (i+1) <= NF) {
            v = $(i+1)
            if (v ~ /^[0-9]+:[0-9]+$/) { gsub(":", "-", v); print v }
            else if (v ~ /^[0-9]+$/) { print v "-" v }
          }
        }
      }
    ' | sort -u || true)"
  fi
}

port_in_ranges() {
  local p="$1" ranges="$2" line s e
  [ -n "$ranges" ] || return 1
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    s="${line%%-*}"
    e="${line#*-}"
    if printf '%s' "$s" | grep -qE '^[0-9]+$' && printf '%s' "$e" | grep -qE '^[0-9]+$'; then
      if [ "$p" -ge "$s" ] && [ "$p" -le "$e" ]; then
        printf '%s' "$line"
        return 0
      fi
    fi
  done <<< "$ranges"
  return 1
}

load_used_ports() {
  if [ -n "$USED_PORTS_CACHE" ]; then
    return 0
  fi
  load_nat_reserved_ranges
  USED_PORTS_CACHE="$({
    if command -v ss >/dev/null 2>&1; then
      if ss -ltnH >/dev/null 2>&1; then
        ss -ltnH 2>/dev/null | awk '{print $4}' | sed 's/.*://'
      else
        ss -ltn 2>/dev/null | tail -n +2 | awk '{print $4}' | sed 's/.*://'
      fi
    elif command -v netstat >/dev/null 2>&1; then
      netstat -ltn 2>/dev/null | awk 'NR>2{print $4}' | sed 's/.*://'
    fi
    if docker_bin_ok && docker_daemon_ok; then
      docker ps --format '{{.Ports}}' 2>/dev/null | extract_docker_published_ports
    fi
  } | grep -E '^[1-9][0-9]{0,4}$' | sort -nu || true)"
}

own_project_holds_port() {
  local p="$1"
  if ! docker_bin_ok || ! docker_daemon_ok; then
    return 1
  fi
  docker ps --filter "label=com.docker.compose.project=${PROJECT_NAME}" --format '{{.Ports}}' 2>/dev/null \
    | extract_docker_published_ports | grep -qxF -e "$p"
}

other_container_holds_port() {
  local p="$1"
  if ! docker_bin_ok || ! docker_daemon_ok; then
    return 1
  fi
  local cname cproj cports
  while IFS='|' read -r cname cproj cports; do
    [ -n "$cname" ] || continue
    [ "$cproj" = "$PROJECT_NAME" ] && continue
    if printf '%s\n' "$cports" | extract_docker_published_ports | grep -qxF -e "$p"; then
      return 0
    fi
  done < <(docker ps --format '{{.Names}}|{{.Label "com.docker.compose.project"}}|{{.Ports}}' 2>/dev/null || true)
  return 1
}

is_port_used() {
  local p="$1"
  local extra_exclude="${2:-}"
  if [ -n "$extra_exclude" ]; then
    case ",${extra_exclude}," in
      *",${p},"*) return 0 ;;
    esac
  fi
  load_used_ports
  if port_in_ranges "$p" "$RESERVED_TCP_RANGES" >/dev/null; then
    return 0
  fi
  if own_project_holds_port "$p" && ! other_container_holds_port "$p"; then
    return 1
  fi
  printf '%s\n' "$USED_PORTS_CACHE" | grep -qxF -e "$p"
}

warn_if_udp_hop_overlap() {
  local p="$1" label="$2" hit=""
  load_used_ports
  hit="$(port_in_ranges "$p" "$RESERVED_UDP_RANGES" || true)"
  if [ -n "$hit" ]; then
    warn "${label}端口 ${p} 落在宿主机 NAT UDP 端口跳跃区间（${hit}）内；TCP 通常不受影响，但若启用 HTTP/3 QUIC 或排查网络请留意。"
  fi
}

pick_free_port() {
  local start="$1"
  local exclude="${2:-}"
  local p="$start"
  if ! is_valid_port "$p" || [ "$p" -gt 65000 ]; then
    p="$DEFAULT_LOCAL_WEB_PORT"
  fi
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
    if is_valid_ipv4 "$ip"; then
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

# 解析容器化 Caddy 的容器内配置路径、宿主机真实路径与容器访问宿主机的上游地址
inspect_docker_caddy_details() {
  local cname="$1"
  CADDY_CONTAINER="$cname"
  CADDY_INNER="/etc/caddy/Caddyfile"
  CADDY_FILE=""
  CADDY_NET=""
  UPSTREAM_HOST="127.0.0.1"

  # 1) 优先从容器命令参数读取自定义 --config 路径
  local args_line custom_cfg=""
  args_line="$(docker inspect "$cname" --format '{{range .Args}}{{.}} {{end}}' 2>/dev/null || true)"
  custom_cfg="$(printf '%s' "$args_line" | sed -n 's/.*--config[= ]\([^[:space:]]*\).*/\1/p' | head -1 || true)"
  if [ -n "$custom_cfg" ]; then
    CADDY_INNER="$custom_cfg"
  fi

  # 2) 按精确文件挂载或父目录挂载映射宿主机路径
  local m_src m_dst rel
  while IFS='|' read -r m_dst m_src; do
    [ -n "$m_dst" ] && [ -n "$m_src" ] || continue
    if [ "$m_dst" = "$CADDY_INNER" ] && [ -f "$m_src" ]; then
      CADDY_FILE="$m_src"
      break
    fi
    case "$CADDY_INNER" in
      "${m_dst%/}/"*)
        rel="${CADDY_INNER#"${m_dst%/}/"}"
        if [ -f "${m_src%/}/${rel}" ]; then
          CADDY_FILE="${m_src%/}/${rel}"
          break
        fi
        ;;
    esac
  done < <(docker inspect "$cname" --format '{{range .Mounts}}{{.Destination}}|{{.Source}}{{"\n"}}{{end}}' 2>/dev/null || true)

  # 3) 解析容器网络模式与 IPv4 网桥网关（兼容 host 网络、自定义桥与双栈 IPv4/IPv6 网络）
  local net_mode=""
  net_mode="$(docker inspect "$cname" --format '{{.HostConfig.NetworkMode}}' 2>/dev/null | tr -d '[:space:]' || true)"
  CADDY_NET="$(docker inspect "$cname" --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>/dev/null | awk '{print $1}' || true)"

  if [ "$net_mode" = "host" ] || [ "$CADDY_NET" = "host" ]; then
    CADDY_NET="host"
    UPSTREAM_HOST="127.0.0.1"
    return 0
  fi

  local gw_candidates="" cand
  # 优先读取容器实时网络网关（多数自动分配子网的自定义网桥仅在此处有 Gateway）
  gw_candidates="$(docker inspect "$cname" --format '{{range $k,$v := .NetworkSettings.Networks}}{{if $v.Gateway}}{{$v.Gateway}} {{end}}{{end}}' 2>/dev/null || true)"
  if [ -n "$CADDY_NET" ]; then
    gw_candidates="${gw_candidates} $(docker network inspect "$CADDY_NET" --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}' 2>/dev/null || true)"
  fi
  UPSTREAM_HOST=""
  for cand in $gw_candidates; do
    if is_valid_ipv4 "$cand"; then
      UPSTREAM_HOST="$cand"
      break
    fi
  done
  if [ -z "$UPSTREAM_HOST" ]; then
    UPSTREAM_HOST="172.17.0.1"
  fi
}

host_80_443_listeners() {
  if command -v ss >/dev/null 2>&1; then
    if ss -ltnpH '( sport = :80 or sport = :443 )' >/dev/null 2>&1; then
      ss -ltnpH '( sport = :80 or sport = :443 )' 2>/dev/null || true
    else
      ss -ltnp 2>/dev/null | awk 'NR>1 && ($4 ~ /:80$/ || $4 ~ /:443$/)' || true
    fi
  elif command -v netstat >/dev/null 2>&1; then
    netstat -ltnp 2>/dev/null | awk 'NR>2 && ($4 ~ /:80$/ || $4 ~ /:443$/)' || true
  fi
}

detect_existing_proxy() {
  DETECTED_PROXY_TYPE="none"
  DETECTED_PROXY_PROC=""
  CADDY_CONTAINER=""
  CADDY_FILE=""
  CADDY_INNER=""
  CADDY_NET=""
  UPSTREAM_HOST="127.0.0.1"

  local host_caddy_container=""
  # 1) 检查是否存在已占用 80/443 的其他容器（排除本实例自己的 web 容器）
  if docker_bin_ok && docker_daemon_ok; then
    local cname cimg cproj cports net_mode
    while IFS='|' read -r cname cimg cproj cports; do
      [ -n "$cname" ] || continue
      [ "$cproj" = "$PROJECT_NAME" ] && continue
      if printf '%s\n' "$cports" | extract_docker_published_ports | grep -qE '^(80|443)$'; then
        if printf '%s %s' "$cimg" "$cname" | grep -qi 'caddy'; then
          DETECTED_PROXY_TYPE="docker-caddy"
          inspect_docker_caddy_details "$cname"
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
      elif printf '%s %s' "$cimg" "$cname" | grep -qi 'caddy'; then
        net_mode="$(docker inspect "$cname" --format '{{.HostConfig.NetworkMode}}' 2>/dev/null | tr -d '[:space:]' || true)"
        if [ "$net_mode" = "host" ]; then
          host_caddy_container="$cname"
        fi
      fi
    done < <(docker ps --format '{{.Names}}|{{.Image}}|{{.Label "com.docker.compose.project"}}|{{.Ports}}' 2>/dev/null || true)
  fi

  # 2) 检查宿主机 80/443 监听进程（逐端口排除仅由本项目占用的端口）
  local listen_lines external_lines="" line lport
  listen_lines="$(host_80_443_listeners)"
  if [ -n "$listen_lines" ]; then
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      lport="$(printf '%s' "$line" | awk '{print $4}' | sed 's/.*://')"
      if [ "$lport" = "80" ] && own_project_holds_port 80 && ! other_container_holds_port 80; then
        continue
      fi
      if [ "$lport" = "443" ] && own_project_holds_port 443 && ! other_container_holds_port 443; then
        continue
      fi
      external_lines="${external_lines}${line}"$'\n'
    done <<< "$listen_lines"
  fi

  if [ -n "$external_lines" ]; then
    if [ -n "$host_caddy_container" ] && { printf '%s' "$external_lines" | grep -qi 'caddy' || ! command -v caddy >/dev/null 2>&1; }; then
      DETECTED_PROXY_TYPE="docker-caddy"
      inspect_docker_caddy_details "$host_caddy_container"
      return 0
    fi
    if printf '%s' "$external_lines" | grep -qi 'caddy'; then
      DETECTED_PROXY_TYPE="host-caddy"
      CADDY_FILE="$(detect_host_caddyfile)"
      CADDY_INNER="$CADDY_FILE"
      UPSTREAM_HOST="127.0.0.1"
      return 0
    elif printf '%s' "$external_lines" | grep -qi 'nginx'; then
      DETECTED_PROXY_TYPE="nginx"
      DETECTED_PROXY_PROC="宿主机 Nginx"
      return 0
    else
      # 非 root 运行 ss 时进程名列可能不可见，辅以服务/进程名兜底识别
      if { command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet caddy 2>/dev/null; } || pgrep -x caddy >/dev/null 2>&1; then
        DETECTED_PROXY_TYPE="host-caddy"
        CADDY_FILE="$(detect_host_caddyfile)"
        CADDY_INNER="$CADDY_FILE"
        UPSTREAM_HOST="127.0.0.1"
        return 0
      elif { command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet nginx 2>/dev/null; } || pgrep -x nginx >/dev/null 2>&1; then
        DETECTED_PROXY_TYPE="nginx"
        DETECTED_PROXY_PROC="宿主机 Nginx"
        return 0
      fi
      DETECTED_PROXY_TYPE="other"
      DETECTED_PROXY_PROC="$(printf '%s\n' "$external_lines" | sed -n 's/.*users:(("\([^"]*\)".*/\1/p' | head -1 || true)"
      if [ -z "$DETECTED_PROXY_PROC" ]; then
        DETECTED_PROXY_PROC="未知进程"
      fi
      return 0
    fi
  fi
}

# ── 共享 Caddyfile 安全管理（顶层词法冲突检测 → 备份 → 标记块更新 → 校验 → 重载）
# 摘除托管块并同步收起插入块时附带的前导空行，实现反复增删字节级幂等
strip_managed_block_to_stdout() {
  local file="$1"
  awk -v b="$MARK_BEGIN" -v e="$MARK_END" '
    {
      lines[NR] = $0
    }
    END {
      skip = 0
      n_out = 0
      for (i = 1; i <= NR; i++) {
        if (lines[i] == b) {
          if (n_out > 0 && out[n_out] == "") {
            n_out--
          }
          skip = 1
          continue
        }
        if (lines[i] == e) {
          skip = 0
          continue
        }
        if (!skip) {
          n_out++
          out[n_out] = lines[i]
        }
      }
      for (i = 1; i <= n_out; i++) {
        print out[i]
      }
    }
  ' "$file"
}

# 顶层站点地址冲突检测：追踪大括号深度，仅解析 depth==0 的站点头（支持多域名同行、大小写、协议与端口，块内零误报）
caddyfile_has_domain_conflict() {
  local file="$1" dom="$2"
  [ -f "$file" ] || return 1
  local target_dom
  target_dom="$(clean_domain "$dom")"
  [ -n "$target_dom" ] || return 1

  strip_managed_block_to_stdout "$file" | awk -v target="$target_dom" '
    BEGIN { depth = 0; hit = 0 }
    {
      line = $0
      sub(/\r$/, "", line)
      # 逐字符剥离注释并提取本行位于 depth==0 时的站点头片段，同时准确维护大括号深度
      in_quote = 0
      esc = 0
      header = ""
      n = length(line)
      for (i = 1; i <= n; i++) {
        c = substr(line, i, 1)
        if (esc) {
          if (depth == 0) header = header c
          esc = 0
          continue
        }
        if (c == "\\" && in_quote) {
          if (depth == 0) header = header c
          esc = 1
          continue
        }
        if (c == "\"") {
          in_quote = !in_quote
          if (depth == 0) header = header c
          continue
        }
        if (!in_quote && c == "#") {
          break
        }
        if (!in_quote && c == "{") {
          depth++
          continue
        }
        if (!in_quote && c == "}") {
          if (depth > 0) depth--
          continue
        }
        if (depth == 0) {
          header = header c
        }
      }

      gsub(/^[[:space:]]+|[[:space:]]+$/, "", header)
      if (header == "" || header ~ /^\(/ || header ~ /^import[[:space:]]/) {
        next
      }
      # 逗号与空白均可分隔同站点的多个地址标签
      gsub(/,/, " ", header)
      cnt = split(header, toks, /[[:space:]]+/)
      for (t = 1; t <= cnt; t++) {
        tok = tolower(toks[t])
        sub(/^https?:\/\//, "", tok)
        sub(/\/.*$/, "", tok)
        sub(/:[0-9]+$/, "", tok)
        sub(/\.$/, "", tok)
        if (tok == target) {
          hit = 1
          exit 0
        }
      }
    }
    END { exit (hit ? 0 : 1) }
  '
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
  local dom="$1" upstream="$2" now
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
  register_tmp "$tmp"
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
  register_tmp "$tmp"
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
  if [ -z "$DOMAIN" ] || [ "$CADDY_MODE" = "none" ]; then return 0; fi
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

# ── 源码拉取与压缩包同步（统一引用 APP_REF，同时兼容分支与 Tag）──────────────────
fetch_remote_head_sha() {
  local sha=""
  if command -v curl >/dev/null 2>&1; then
    sha="$(curl -fsSL --max-time 8 -H 'Accept: application/vnd.github+json' \
      "https://api.github.com/repos/${APP_REPO}/commits/${APP_REF}" 2>/dev/null \
      | grep -oE '"sha"[[:space:]]*:[[:space:]]*"[0-9a-f]{7,40}"' | head -1 \
      | sed -E 's/.*"([0-9a-f]{7,40})"/\1/' | cut -c1-7 || true)"
  fi
  printf '%s' "$sha"
}

download_source_tarball_to() {
  local dest_dir="$1"
  command -v tar >/dev/null 2>&1 || die "缺少 tar 命令，无法解压源码压缩包"
  # codeload.github.com/<owner>/<repo>/tar.gz/<ref> 同时兼容分支名、标签与提交哈希
  local tar_url="https://codeload.github.com/${APP_REPO}/tar.gz/${APP_REF}"
  local tmp_tar tmp_extract top_dir
  tmp_tar="$(mktemp /tmp/teyir-src.XXXXXX.tar.gz)"
  tmp_extract="$(mktemp -d /tmp/teyir-extract.XXXXXX)"
  register_tmp "$tmp_tar"
  register_tmp "$tmp_extract"

  if curl -fsSL --proto '=https' --tlsv1.2 --max-time 60 "$tar_url" -o "$tmp_tar" \
    && [ -s "$tmp_tar" ] \
    && tar -xzf "$tmp_tar" -C "$tmp_extract" 2>/dev/null; then
    top_dir="$(find "$tmp_extract" -maxdepth 1 -mindepth 1 -type d | head -1 || true)"
    if [ -n "$top_dir" ] && [ -f "$top_dir/docker-compose.yml" ] && [ -f "$top_dir/Dockerfile.api" ]; then
      mkdir -p "$dest_dir"
      (
        cd "$top_dir"
        shopt -s dotglob nullglob
        # 保留目标目录已有的运行时文件（.env / install.conf / geo-data）
        local entry base
        for entry in *; do
          base="$(basename "$entry")"
          case "$base" in
            .env|.env.*|install.conf|install.conf.*|geo-data) continue ;;
          esac
          rm -rf "${dest_dir:?}/${base}"
          mv -- "$entry" "$dest_dir/"
        done
      )
      rm -rf "$tmp_tar" "$tmp_extract"
      SOURCE_COMMIT="$(fetch_remote_head_sha)"
      return 0
    fi
  fi
  rm -rf "$tmp_tar" "$tmp_extract"
  return 1
}

prepare_source_dir() {
  step "准备源码目录：$INSTALL_DIR（分支/版本：${APP_REF}）"
  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将复用或从 https://github.com/${APP_REPO} (引用 ${APP_REF}) 准备源码至 $INSTALL_DIR"
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
    info "正在从 GitHub 克隆源码（分支/版本：${APP_REF}）..."
    if git clone --depth 1 --branch "$APP_REF" "https://github.com/${APP_REPO}.git" "$INSTALL_DIR" >/dev/null 2>&1; then
      SOURCE_COMMIT="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
      ok "源码克隆完成（版本：${SOURCE_COMMIT}）"
      return 0
    fi
    warn "Git 克隆失败，自动切换压缩包兜底下载..."
    rm -rf "$INSTALL_DIR"
  fi

  if download_source_tarball_to "$INSTALL_DIR"; then
    ok "源码压缩包下载并解包完成${SOURCE_COMMIT:+（提交：${SOURCE_COMMIT}）}"
    return 0
  fi
  die "无法从 GitHub 下载 ${APP_REPO}（分支/版本 ${APP_REF}），请检查服务器网络连通性后重试"
}

# ── 生成与安全保留 .env 配置（单遍读取零子壳写盘，保留核心密钥与自定义项）───────
write_env_file() {
  local env_file="$INSTALL_DIR/.env"
  EXISTING_ENV_DETECTED=0
  IS_NEW_ADMIN_SEEDED=0

  load_env_snapshot "$env_file"
  if [ -f "$env_file" ]; then
    EXISTING_ENV_DETECTED=1
  fi

  local pg_pass="$OLD_ENV_POSTGRES_PASSWORD"
  local jwt_sec="$OLD_ENV_JWT_SECRET"
  local cred_key="$OLD_ENV_CREDENTIAL_KEY"
  local seed_email="$OLD_ENV_SEED_ADMIN_EMAIL"
  local seed_pass="$OLD_ENV_SEED_ADMIN_PASSWORD"

  if [ -z "$pg_pass" ] || [ "$pg_pass" = "replace-with-a-long-random-value" ]; then
    pg_pass="$(gen_urlsafe_secret 40)"
  fi
  if [ -z "$jwt_sec" ] || [ "${#jwt_sec}" -lt 32 ] || [ "$jwt_sec" = "replace-with-a-random-secret-of-at-least-32-characters" ]; then
    jwt_sec="$(gen_urlsafe_secret 56)"
  fi
  if [ -z "$cred_key" ] || [ "$cred_key" = "base64-of-32-random-bytes" ]; then
    cred_key="$(gen_credential_key)"
  elif ! is_valid_credential_key "$cred_key"; then
    die "检测到已有 $env_file 中的 CREDENTIAL_KEY 格式非法（须为 32 字节标准 Base64，固定 44 字符且末位为 =）。为防止误覆盖已入库凭据的加密主密钥，已拒绝自动覆写，请先核查修复后重试。"
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

  local renew_val="$OLD_ENV_STORE_RENEW_BASE_URL"
  local hy2_script_val="$OLD_ENV_HY2_INSTALL_SCRIPT_URL"
  local hy2_repo_val="$OLD_ENV_HY2_INSTALLER_REPO"
  if [ "$FLAG_STORE_RENEW" = "1" ] || [ -n "$STORE_RENEW_BASE_URL" ]; then renew_val="$STORE_RENEW_BASE_URL"; fi
  if [ "$FLAG_HY2_SCRIPT" = "1" ] || [ -n "$HY2_INSTALL_SCRIPT_URL" ]; then hy2_script_val="$HY2_INSTALL_SCRIPT_URL"; fi
  if [ "$FLAG_HY2_REPO" = "1" ] || [ -n "$HY2_INSTALLER_REPO" ]; then hy2_repo_val="$HY2_INSTALLER_REPO"; fi

  local old_dom_url=""
  if [ -n "$SAVED_DOMAIN" ]; then old_dom_url="https://${SAVED_DOMAIN}"; fi

  local panel_public_url="" sub_public_url="" allowed_origin="http://localhost:5173"
  local site_line="# SITE_ADDRESS="
  local trusted_cidrs="${OLD_ENV_TRUSTED_PROXY_CIDRS:-127.0.0.1/32,::1/128,172.16.0.0/12,192.168.0.0/16,10.0.0.0/8}"

  if [ -n "$DOMAIN" ]; then
    panel_public_url="https://${DOMAIN}"
    # 若用户之前手工配置了独立订阅域名（不同于旧面板域名），则保留之
    if [ -n "$OLD_ENV_SUBSCRIPTION_PUBLIC_BASE_URL" ] && [ "$OLD_ENV_SUBSCRIPTION_PUBLIC_BASE_URL" != "$old_dom_url" ]; then
      sub_public_url="$OLD_ENV_SUBSCRIPTION_PUBLIC_BASE_URL"
    else
      sub_public_url="https://${DOMAIN}"
    fi
    # 若用户手工配置了多来源跨域列表，仅在其未含新域名时追加，不粗暴冲掉
    if [ -n "$OLD_ENV_ALLOWED_ORIGIN" ] && [[ "$OLD_ENV_ALLOWED_ORIGIN" == *","* ]]; then
      local compact_origins="${OLD_ENV_ALLOWED_ORIGIN//[[:space:]]/}"
      if [[ ",${compact_origins}," == *",https://${DOMAIN},"* ]]; then
        allowed_origin="$OLD_ENV_ALLOWED_ORIGIN"
      else
        allowed_origin="https://${DOMAIN},${OLD_ENV_ALLOWED_ORIGIN}"
      fi
    else
      allowed_origin="https://${DOMAIN}"
    fi
    if [ "$ACCESS_MODE" = "self" ]; then
      site_line="SITE_ADDRESS=${DOMAIN}"
    fi
  else
    if [ "$NO_DOMAIN" != "1" ] && [ -n "$OLD_ENV_PANEL_PUBLIC_URL" ] && [ "$OLD_ENV_PANEL_PUBLIC_URL" != "$old_dom_url" ]; then
      panel_public_url="$OLD_ENV_PANEL_PUBLIC_URL"
    fi
    if [ "$NO_DOMAIN" != "1" ] && [ -n "$OLD_ENV_SUBSCRIPTION_PUBLIC_BASE_URL" ] && [ "$OLD_ENV_SUBSCRIPTION_PUBLIC_BASE_URL" != "$old_dom_url" ]; then
      sub_public_url="$OLD_ENV_SUBSCRIPTION_PUBLIC_BASE_URL"
    fi
    if [ -n "$OLD_ENV_ALLOWED_ORIGIN" ] && [ "$OLD_ENV_ALLOWED_ORIGIN" != "$old_dom_url" ]; then
      allowed_origin="$OLD_ENV_ALLOWED_ORIGIN"
    fi
  fi

  local ingress_net="${OLD_ENV_WEB_INGRESS_NETWORK:-${PROJECT_NAME}-internal}"

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
    prune_backups 3 "$INSTALL_DIR" ".env.bak-" ""
  fi

  local tmp="${env_file}.tmp.$$" cline
  register_tmp "$tmp"
  {
    printf '%s\n' "# =============================================================================="
    printf '%s\n' "# Teyir 控制台运行配置（由 install.sh 自动生成并维护，权限 600）"
    printf '%s\n' "# ⚠️ 切勿修改已入库实例的 CREDENTIAL_KEY 与 POSTGRES_PASSWORD"
    printf '%s\n' "# =============================================================================="
    emit_env_kv COMPOSE_PROJECT_NAME "$PROJECT_NAME"
    emit_env_kv WEB_INGRESS_NETWORK "$ingress_net"
    printf '\n# 数据库与核心密钥\n'
    emit_env_kv POSTGRES_PASSWORD "$pg_pass"
    emit_env_kv JWT_SECRET "$jwt_sec"
    emit_env_kv JWT_TTL_MINUTES "${OLD_ENV_JWT_TTL_MINUTES:-720}"
    emit_env_kv SEED_ADMIN_EMAIL "$seed_email"
    emit_env_kv SEED_ADMIN_PASSWORD "$seed_pass"
    emit_env_kv CREDENTIAL_KEY "$cred_key"
    printf '\n# 访问来源与监听端口\n'
    emit_env_kv ALLOWED_ORIGIN "$allowed_origin"
    emit_env_kv TRUSTED_PROXY_CIDRS "$trusted_cidrs"
    emit_env_kv API_BIND "$API_BIND"
    emit_env_kv API_PORT "$API_PORT"
    emit_env_kv WEB_BIND "$WEB_BIND"
    emit_env_kv WEB_PORT "$WEB_PORT"
    emit_env_kv WEB_TLS_PORT "$WEB_TLS_PORT"
    printf '\n# 站点与 TLS（仅内置 Caddy 自动 HTTPS 时取消注释；留空必须保持注释态）\n%s\n' "$site_line"
    printf '\n# 采集、执行与测速周期\n'
    emit_env_kv ENABLE_COLLECTOR "$ENABLE_COLLECTOR"
    emit_env_kv COLLECTOR_INTERVAL_SECONDS "${OLD_ENV_COLLECTOR_INTERVAL_SECONDS:-30}"
    emit_env_kv EXECUTOR_INTERVAL_SECONDS "${OLD_ENV_EXECUTOR_INTERVAL_SECONDS:-20}"
    emit_env_kv SPEEDTEST_INTERVAL_MINUTES "${OLD_ENV_SPEEDTEST_INTERVAL_MINUTES:-15}"
    printf '\n# 地区识别与 IP 纯净度\n'
    emit_env_kv GEO_OFFLINE_DB "${OLD_ENV_GEO_OFFLINE_DB:-/data/geo-offline.json}"
    emit_env_kv GEO_ENABLE_API "${OLD_ENV_GEO_ENABLE_API:-true}"
    emit_env_kv GEO_API_BASE_URL "$OLD_ENV_GEO_API_BASE_URL"
    emit_env_kv IP_PURITY_OFFLINE_DB "$OLD_ENV_IP_PURITY_OFFLINE_DB"
    emit_env_kv IP_PURITY_API_KEY "$OLD_ENV_IP_PURITY_API_KEY"
    emit_env_kv IP_PURITY_IPQS_KEY "$OLD_ENV_IP_PURITY_IPQS_KEY"
    emit_env_kv IP_PURITY_API_BASE_URL "$OLD_ENV_IP_PURITY_API_BASE_URL"
    emit_env_kv PORTAL_TLS_PINS "$OLD_ENV_PORTAL_TLS_PINS"
    emit_env_kv VIEWER_RESTRICT_SUBSCRIPTIONS "${OLD_ENV_VIEWER_RESTRICT_SUBSCRIPTIONS:-false}"
    printf '\n# 外部地址、订阅分组与可选联动\n'
    emit_env_kv PANEL_PUBLIC_URL "$panel_public_url"
    emit_env_kv SUBSCRIPTION_PUBLIC_BASE_URL "$sub_public_url"
    emit_env_kv SUBSCRIPTION_BACKUP_BASE_URLS "$OLD_ENV_SUBSCRIPTION_BACKUP_BASE_URLS"
    emit_env_kv SUBSCRIPTION_DEFAULT_NAME "$SUB_NAME"
    emit_env_kv STORE_RENEW_BASE_URL "$renew_val"
    emit_env_kv TURNSTILE_SITE_KEY "$OLD_ENV_TURNSTILE_SITE_KEY"
    emit_env_kv TURNSTILE_SECRET "$OLD_ENV_TURNSTILE_SECRET"
    emit_env_kv LOGIN_SERVICE_KEY "$OLD_ENV_LOGIN_SERVICE_KEY"
    emit_env_kv LOGIN_SERVICE_EMAIL "$OLD_ENV_LOGIN_SERVICE_EMAIL"
    emit_env_kv BAN_WEBHOOK_URL "$OLD_ENV_BAN_WEBHOOK_URL"
    emit_env_kv BAN_WEBHOOK_TOKEN "$OLD_ENV_BAN_WEBHOOK_TOKEN"
    emit_env_kv HY2_INSTALL_SCRIPT_URL "$hy2_script_val"
    emit_env_kv HY2_INSTALLER_REPO "$hy2_repo_val"
    [ -n "$OLD_ENV_MIGRATIONS_DIR" ] && emit_env_kv MIGRATIONS_DIR "$OLD_ENV_MIGRATIONS_DIR"
    [ -n "$OLD_ENV_MIGRATE_ASSUME_BASELINE" ] && emit_env_kv MIGRATE_ASSUME_BASELINE "$OLD_ENV_MIGRATE_ASSUME_BASELINE"
    if [ "${#CUSTOM_ENV_LINES[@]}" -gt 0 ]; then
      printf '\n# 保留的用户自定义环境变量\n'
      for cline in "${CUSTOM_ENV_LINES[@]}"; do
        printf '%s\n' "$cline"
      done
    fi
  } > "$tmp"

  chmod 600 "$tmp"
  mv -f "$tmp" "$env_file"
  mkdir -p "$INSTALL_DIR/geo-data"
  ok "已写入运行配置：$env_file（保留既有密钥与自定义项并设置 600 权限）"
}

# ── Docker Compose 操作、护栏与 ip_forward=0 兼容构建 ──────────────────────────
dc() {
  COMPOSE_PROJECT_NAME="$PROJECT_NAME" docker compose \
    -f "$INSTALL_DIR/docker-compose.yml" \
    --env-file "$INSTALL_DIR/.env" "$@" </dev/null
}

check_compose_api_mount_guard() {
  if grep -Eq '^[[:space:]]*-[[:space:]]*[^#]*:/api(:ro|:rw)?[[:space:]]*$' "$INSTALL_DIR"/docker-compose*.yml 2>/dev/null; then
    err "检测到 $INSTALL_DIR/docker-compose*.yml 将宿主机文件挂载覆盖了容器 /api，会导致镜像更新不生效！"
    grep -En '^[[:space:]]*-[[:space:]]*[^#]*:/api(:ro|:rw)?[[:space:]]*$' "$INSTALL_DIR"/docker-compose*.yml >&2 || true
    die "请先删除该 /api 挂载后重试"
  fi
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
    local ov rc=0
    ov="$(mktemp /tmp/teyir-build-net.XXXXXX.yml)"
    register_tmp "$ov"
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
    COMPOSE_PROJECT_NAME="$PROJECT_NAME" docker compose \
      -f "$INSTALL_DIR/docker-compose.yml" \
      -f "$ov" \
      --env-file "$INSTALL_DIR/.env" \
      build "$@" </dev/null || rc=$?
    rm -f "$ov"
    return "$rc"
  fi
  dc build "$@"
}

wait_db_ready() {
  local max_tries="${1:-30}" i
  for i in $(seq 1 "$max_tries"); do
    if dc exec -T db pg_isready -U nodeops -d nodeops >/dev/null 2>&1 \
      && dc exec -T db psql -U nodeops -d nodeops -t -A -c 'SELECT 1' >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
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

  check_compose_api_mount_guard

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

  if ! wait_db_ready 30; then
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
  local api_ok=0 web_ok=0 i
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
      if [ "$FLAG_WEB_PORT" = "1" ] && [ "$WEB_PORT" != "auto" ] && [ "$WEB_PORT" != "80" ]; then
        die "内置自动 HTTPS（self）模式要求前端 HTTP 端口固定为 80（收到：${WEB_PORT}）"
      fi
      if [ "$FLAG_WEB_TLS_PORT" = "1" ] && [ "$WEB_TLS_PORT" != "auto" ] && [ "$WEB_TLS_PORT" != "443" ]; then
        die "内置自动 HTTPS（self）模式要求前端 HTTPS 端口固定为 443（收到：${WEB_TLS_PORT}）"
      fi
      CADDY_MODE="self"
      CADDY_CONTAINER=""; CADDY_FILE=""; CADDY_INNER=""; CADDY_NET=""; UPSTREAM_HOST="127.0.0.1"
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
      CADDY_CONTAINER=""; CADDY_FILE=""; CADDY_INNER=""; CADDY_NET=""; UPSTREAM_HOST="127.0.0.1"
      if [ "$ACCESS_MODE" = "direct" ]; then
        DOMAIN=""
      fi
      ;;
  esac

  USED_PORTS_CACHE=""
  local default_web_start="$DEFAULT_LOCAL_WEB_PORT"
  case "$ACCESS_MODE" in
    self)
      [ "$FLAG_WEB_BIND" = "0" ] && WEB_BIND="0.0.0.0"
      WEB_PORT="80"
      WEB_TLS_PORT="443"
      if is_port_used 80; then die "内置自动 HTTPS（self）所需的 80 端口已被占用或被 NAT 重定向，请释放后再试"; fi
      if is_port_used 443; then die "内置自动 HTTPS（self）所需的 443 端口已被占用或被 NAT 重定向，请释放后再试"; fi
      ;;
    *)
      case "$ACCESS_MODE" in
        host-caddy|local) [ "$FLAG_WEB_BIND" = "0" ] && WEB_BIND="127.0.0.1" ;;
        docker-caddy)     [ "$FLAG_WEB_BIND" = "0" ] && WEB_BIND="$UPSTREAM_HOST" ;;
        direct)           [ "$FLAG_WEB_BIND" = "0" ] && WEB_BIND="0.0.0.0"; default_web_start="$DEFAULT_DIRECT_PORT" ;;
      esac
      if [ -z "$WEB_PORT" ] || [ "$WEB_PORT" = "auto" ]; then
        WEB_PORT="$(pick_free_port "$default_web_start")"
      elif [ "$FLAG_WEB_PORT" = "0" ] && is_port_used "$WEB_PORT"; then
        local new_wp
        new_wp="$(pick_free_port "$default_web_start")"
        warn "上次记录的前端端口 ${WEB_PORT} 已被其他进程占用，已自动避让为 ${new_wp}"
        WEB_PORT="$new_wp"
      fi
      if [ -z "$WEB_TLS_PORT" ] || [ "$WEB_TLS_PORT" = "auto" ]; then
        WEB_TLS_PORT="$(pick_free_port "$DEFAULT_LOCAL_TLS_PORT" "$WEB_PORT")"
      elif [ "$FLAG_WEB_TLS_PORT" = "0" ] && is_port_used "$WEB_TLS_PORT" "$WEB_PORT"; then
        local new_wtp
        new_wtp="$(pick_free_port "$DEFAULT_LOCAL_TLS_PORT" "$WEB_PORT")"
        warn "上次记录的 HTTPS 映射端口 ${WEB_TLS_PORT} 已被占用，已自动避让为 ${new_wtp}"
        WEB_TLS_PORT="$new_wtp"
      fi
      ;;
  esac

  local used_here="${WEB_PORT},${WEB_TLS_PORT}"
  if [ -z "$API_PORT" ] || [ "$API_PORT" = "auto" ]; then
    API_PORT="$(pick_free_port "$DEFAULT_API_PORT" "$used_here")"
  elif [ "$FLAG_API_PORT" = "0" ] && is_port_used "$API_PORT" "$used_here"; then
    local new_ap
    new_ap="$(pick_free_port "$DEFAULT_API_PORT" "$used_here")"
    warn "上次记录的 API 端口 ${API_PORT} 已被占用，已自动避让为 ${new_ap}"
    API_PORT="$new_ap"
  fi

  if [ "$WEB_PORT" = "$WEB_TLS_PORT" ] || [ "$WEB_PORT" = "$API_PORT" ] || [ "$WEB_TLS_PORT" = "$API_PORT" ]; then
    die "端口冲突：WEB_PORT($WEB_PORT)、WEB_TLS_PORT($WEB_TLS_PORT)、API_PORT($API_PORT) 必须互不相同"
  fi
  if [ "$FLAG_WEB_PORT" = "1" ] && [ "$WEB_PORT" != "auto" ] && is_port_used "$WEB_PORT"; then
    die "指定的前端端口 ${WEB_PORT} 已被其他进程/容器占用或落在 NAT TCP 重定向区间，请换一个端口或使用 --web-port auto"
  fi
  if [ "$FLAG_WEB_TLS_PORT" = "1" ] && [ "$WEB_TLS_PORT" != "auto" ] && is_port_used "$WEB_TLS_PORT"; then
    die "指定的 HTTPS 端口 ${WEB_TLS_PORT} 已被其他进程/容器占用或落在 NAT TCP 重定向区间，请换一个端口或使用 --web-tls-port auto"
  fi
  if [ "$FLAG_API_PORT" = "1" ] && [ "$API_PORT" != "auto" ] && is_port_used "$API_PORT"; then
    die "指定的 API 端口 ${API_PORT} 已被其他进程/容器占用或落在 NAT TCP 重定向区间，请换一个端口或使用 --api-port auto"
  fi

  warn_if_udp_hop_overlap "$WEB_PORT" "前端 HTTP "
  if [ "$ACCESS_MODE" = "self" ]; then
    warn_if_udp_hop_overlap "$WEB_TLS_PORT" "前端 HTTPS "
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
      if [ -n "$def_dom" ] && ! is_valid_domain "$def_dom"; then
        def_dom=""
      fi
      while :; do
        local raw_dom=""
        if [ -n "$def_dom" ]; then
          printf '  请输入要绑定的域名（如 panel.example.com） [%s]: ' "$def_dom" >&2
          if ! tty_read raw_dom 0; then
            printf '\n' >&2
            raw_dom="$def_dom"
          fi
          raw_dom="${raw_dom:-$def_dom}"
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

prompt_admin_account_if_needed() {
  if [ -f "$INSTALL_DIR/.env" ]; then
    ok "检测到已有 .env 配置，将自动保留原管理员账号、数据库密码与节点加密主密钥"
    return 0
  fi
  if [ "$FLAG_ADMIN_EMAIL" = "0" ]; then
    local def_em="${ADMIN_EMAIL:-$DEFAULT_ADMIN_EMAIL}"
    is_valid_email "$def_em" || def_em="$DEFAULT_ADMIN_EMAIL"
    while :; do
      local em=""
      printf '  管理员登录邮箱 [%s]: ' "$def_em" >&2
      if ! tty_read em 0; then
        printf '\n' >&2
        ADMIN_EMAIL="$def_em"
        break
      fi
      em="${em:-$def_em}"
      if is_valid_email "$em"; then
        ADMIN_EMAIL="$em"
        break
      fi
      warn "邮箱格式不正确，请重新输入（例如 admin@teyir.local）"
    done
  fi
  if [ "$FLAG_ADMIN_PASSWORD" = "0" ] && [ -z "$ADMIN_PASSWORD" ]; then
    local auto_pw
    auto_pw="$(gen_alnum_password 16)"
    ADMIN_PASSWORD="$(ask_password "管理员登录密码（至少 12 位）" "$auto_pw")"
  fi
}

prompt_optional_integrations() {
  local cur_renew cur_hy2_url cur_hy2_repo v
  cur_renew="$(read_kv_file "$INSTALL_DIR/.env" STORE_RENEW_BASE_URL)"
  cur_hy2_url="$(read_kv_file "$INSTALL_DIR/.env" HY2_INSTALL_SCRIPT_URL)"
  cur_hy2_repo="$(read_kv_file "$INSTALL_DIR/.env" HY2_INSTALLER_REPO)"

  while :; do
    v="$(ask_optional "外部商城续费基础地址（http/https）" "$cur_renew")"
    if [ -z "$v" ] || is_valid_http_url "$v"; then
      STORE_RENEW_BASE_URL="$v"; FLAG_STORE_RENEW=1; break
    fi
    warn "地址格式非法，须以 http:// 或 https:// 开头（或输入 - 清空）"
  done

  while :; do
    v="$(ask_optional "节点一键安装脚本地址（https）" "$cur_hy2_url")"
    if [ -z "$v" ] || is_valid_https_url "$v"; then
      HY2_INSTALL_SCRIPT_URL="$v"; FLAG_HY2_SCRIPT=1; break
    fi
    warn "地址格式非法，须以 https:// 开头（或输入 - 清空）"
  done

  while :; do
    v="$(ask_optional "节点门户更新来源仓库（owner/repo）" "$cur_hy2_repo")"
    if [ -z "$v" ] || is_valid_repo_slug "$v"; then
      HY2_INSTALLER_REPO="$v"; FLAG_HY2_REPO=1; break
    fi
    warn "仓库格式非法，须为 owner/repo（或输入 - 清空）"
  done
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
  step "第 2 步：初始管理员账号"
  prompt_admin_account_if_needed

  # 问题 3：高级选项门禁
  step "第 3 步：高级选项（一般直接回车跳过）"
  if ask_yn "需要修改高级选项（安装目录 / 端口 / 默认订阅名 / 周期采集开关 / 联动地址）吗？" "n"; then
    local def_dir="$INSTALL_DIR"
    is_safe_install_dir "$def_dir" || def_dir="$DEFAULT_DIR"
    while :; do
      local d_in=""
      printf '  安装目录（绝对路径） [%s]: ' "$def_dir" >&2
      if ! tty_read d_in 0; then
        printf '\n' >&2
        d_in="$def_dir"
      fi
      d_in="${d_in:-$def_dir}"
      if [ "$d_in" != "/" ]; then d_in="${d_in%/}"; fi
      if is_safe_install_dir "$d_in"; then
        if [ "$d_in" != "$INSTALL_DIR" ]; then
          INSTALL_DIR="$d_in"
          if [ ! -f "$INSTALL_DIR/install.conf" ] && [ ! -f "$INSTALL_DIR/.env" ]; then
            FRESH_INSTALL=1
          else
            FRESH_INSTALL=0
          fi
          load_saved_state
          prefill_from_saved
          # 若切换到尚无 .env 的全新目录且未配置初始管理员，及时补问
          if [ ! -f "$INSTALL_DIR/.env" ] && [ -z "$ADMIN_PASSWORD" ]; then
            prompt_admin_account_if_needed
          fi
        fi
        break
      fi
      warn "安装目录必须是非系统根目录、不含 .. 的绝对路径（例如 /opt/teyir-console）"
    done

    SUB_NAME="$(ask "客户端导入订阅后的默认分组名" "${SUB_NAME:-$DEFAULT_SUB_NAME}")"
    local col_def="y"
    if [ "$ENABLE_COLLECTOR" = "false" ]; then col_def="n"; fi
    if ask_yn "是否启用后台周期采集器（定期刷新 VPS 主机与节点监控指标）？" "$col_def"; then
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

    prompt_optional_integrations
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
    if [ "$ACCESS_MODE" = "docker-caddy" ] && [ "$UPSTREAM_HOST" != "127.0.0.1" ]; then
      dim "提示：若宿主机启用了 UFW/firewalld，请确认允许 Docker 网桥访问本机 ${UPSTREAM_HOST}:${WEB_PORT}。"
    fi
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
should_remove_old_caddy() {
  [ -n "$SAVED_CADDY_FILE" ] || return 1
  if [ "$NO_DOMAIN" = "1" ] || [ "$CADDY_MODE" = "none" ]; then
    return 0
  fi
  if [ "$CADDY_MODE" != "$SAVED_CADDY_MODE" ] || [ "$CADDY_FILE" != "$SAVED_CADDY_FILE" ]; then
    return 0
  fi
  return 1
}

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
  info "源码仓库：      ${APP_REPO}（分支/版本：${APP_REF}）"
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
    if should_remove_old_caddy; then
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

  if should_remove_old_caddy; then
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

  local attempt=1 need_build=1 next_start
  while [ "$attempt" -le 3 ]; do
    write_env_file
    if start_and_verify_stack "$need_build"; then
      break
    fi
    need_build=0
    if [ "$LAST_FAIL_KIND" = "port" ] && [ "$ACCESS_MODE" != "self" ] && [ "$attempt" -lt 3 ]; then
      warn "检测到端口绑定冲突（第 ${attempt}/3 次），正在自动避让并换新端口重试..."
      USED_PORTS_CACHE=""
      next_start=$((WEB_PORT + 1)); [ "$next_start" -gt 65000 ] && next_start="$DEFAULT_LOCAL_WEB_PORT"
      WEB_PORT="$(pick_free_port "$next_start")"
      next_start=$((WEB_TLS_PORT + 1)); [ "$next_start" -gt 65000 ] && next_start="$DEFAULT_LOCAL_TLS_PORT"
      WEB_TLS_PORT="$(pick_free_port "$next_start" "$WEB_PORT")"
      next_start=$((API_PORT + 1)); [ "$next_start" -gt 65000 ] && next_start="$DEFAULT_API_PORT"
      API_PORT="$(pick_free_port "$next_start" "${WEB_PORT},${WEB_TLS_PORT}")"
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
resolve_backup_root() {
  case "$INSTALL_DIR" in
    /opt/*) printf '/opt' ;;
    *)      dirname "$INSTALL_DIR" ;;
  esac
}

do_check_update() {
  need_state || return 1
  step "检查远端更新（${APP_REPO} · 引用 ${APP_REF}）"

  if [ -d "$INSTALL_DIR/.git" ] && command -v git >/dev/null 2>&1; then
    local local_sha local_msg remote_sha behind
    local_sha="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
    local_msg="$(git -C "$INSTALL_DIR" log -1 --pretty='%s' 2>/dev/null || true)"
    if git -C "$INSTALL_DIR" fetch --quiet origin "+refs/heads/${APP_REF}:refs/remotes/origin/${APP_REF}" 2>/dev/null \
      || git -C "$INSTALL_DIR" fetch --quiet origin "$APP_REF" 2>/dev/null; then
      remote_sha="$(git -C "$INSTALL_DIR" rev-parse --short "origin/${APP_REF}" 2>/dev/null || git -C "$INSTALL_DIR" rev-parse --short FETCH_HEAD 2>/dev/null || echo unknown)"
      behind="$(git -C "$INSTALL_DIR" rev-list --count "HEAD..${remote_sha}" 2>/dev/null || echo 0)"
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
    return 0
  fi

  # 压缩包安装形态：通过 GitHub Commits API 对比已记录的 SOURCE_COMMIT
  local remote_sha
  remote_sha="$(fetch_remote_head_sha)"
  if [ -z "$remote_sha" ]; then
    warn "当前目录通过压缩包安装，且暂时无法访问 GitHub API 查询最新提交（请检查网络连通性）。"
    return 0
  fi
  info "当前记录版本：${SOURCE_COMMIT:-未知（压缩包安装）}"
  info "远端最新提交：${remote_sha}（引用 ${APP_REF}）"
  if [ -n "$SOURCE_COMMIT" ] && [ "$SOURCE_COMMIT" = "$remote_sha" ]; then
    ok "已是最新版本，无需升级。"
  else
    warn "检测到远端可能有更新！可直接执行平滑升级：$(render_priv_cmd --upgrade)"
  fi
}

do_backup() {
  require_root --backup || return 1
  need_state || return 1
  step "备份配置与 PostgreSQL 数据库"
  local stamp backup_root
  stamp="$(date +%Y%m%d-%H%M%S)"
  backup_root="$(resolve_backup_root)"
  local code_bk="${backup_root}/${PROJECT_NAME}-backup-${stamp}.tar.gz"
  local db_bk="${backup_root}/${PROJECT_NAME}-db-${stamp}.dump"

  if [ "$DRY_RUN" = "1" ]; then
    dim "[预演] 将备份源码与配置到 $code_bk，并转储数据库到 $db_bk"
    return 0
  fi

  mkdir -p "$backup_root"
  if ! tar -C "$INSTALL_DIR" --exclude='./.git' --exclude='./node_modules' --exclude='./dist' -czf "$code_bk" .; then
    rm -f "$code_bk" 2>/dev/null || true
    err "打包源码与配置失败（请检查磁盘空间与权限）：$code_bk"
    return 1
  fi
  chmod 600 "$code_bk"
  prune_backups 5 "$backup_root" "${PROJECT_NAME}-backup-" ".tar.gz"
  ok "源码与配置备份完成：$code_bk"

  if ! wait_db_ready 5; then
    warn "数据库容器当前未运行，正在尝试启动 db 以便执行快照转储..."
    dc up -d db >/dev/null 2>&1 || true
    if ! wait_db_ready 25; then
      err "数据库未能就绪，无法执行 pg_dump 备份"
      return 1
    fi
  fi

  local err_tmp
  err_tmp="$(mktemp /tmp/teyir-pgdump.XXXXXX)"
  register_tmp "$err_tmp"
  if dc exec -T db pg_dump -U nodeops -d nodeops > "$db_bk" 2>"$err_tmp" && [ -s "$db_bk" ]; then
    chmod 600 "$db_bk"
    rm -f "$err_tmp"
    prune_backups 3 "$backup_root" "${PROJECT_NAME}-db-" ".dump"
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
    dim "[预演] 将依次执行：等待 db 就绪并备份源码与数据库 → 拉取 ${APP_REPO}@${APP_REF} → 重建 migrate 并迁移 → 重建 api/web → 容器内健康验收"
    return 0
  fi

  local backup_root before_sha after_sha
  backup_root="$(resolve_backup_root)"

  dc up -d db >/dev/null || die "启动数据库失败，已中止升级"
  wait_db_ready 30 || die "等待数据库就绪超时，已中止升级"
  do_backup || die "升级前备份失败，为避免无退路已中止升级"

  if [ -d "$INSTALL_DIR/.git" ] && command -v git >/dev/null 2>&1; then
    before_sha="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
    info "升级前版本：$before_sha"
    info "拉取远端分支 origin/${APP_REF}..."
    git -C "$INSTALL_DIR" fetch --prune origin "+refs/heads/${APP_REF}:refs/remotes/origin/${APP_REF}" \
      || git -C "$INSTALL_DIR" fetch --prune origin \
      || die "拉取远端仓库失败，请检查网络连接"
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
    SOURCE_COMMIT="$after_sha"
  else
    before_sha="${SOURCE_COMMIT:-压缩包}"
    info "当前目录为压缩包安装形态，将通过最新源码压缩包执行平滑升级（完整保留 .env、install.conf 与 geo-data）..."
    download_source_tarball_to "$INSTALL_DIR" || die "下载最新源码压缩包失败，原运行容器未受影响；备份已保存在 ${backup_root}/${PROJECT_NAME}-*"
    after_sha="${SOURCE_COMMIT:-latest}"
  fi
  ok "源码已更新：${before_sha} -> ${after_sha}"

  start_and_verify_stack 1 || die "升级后健康验收未通过，请查看上方日志；备份已保存在 ${backup_root}/${PROJECT_NAME}-*"
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

  local git_ver="压缩包安装${SAVED_SOURCE_COMMIT:+（提交：${SAVED_SOURCE_COMMIT}）}"
  if [ -d "$INSTALL_DIR/.git" ] && command -v git >/dev/null 2>&1; then
    git_ver="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)（分支：$(git -C "$INSTALL_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "$APP_REF")）"
  fi
  local env_email pub_ip=""
  env_email="$(read_kv_file "$INSTALL_DIR/.env" SEED_ADMIN_EMAIL)"
  if [ "$ACCESS_MODE" = "direct" ] || [ "$ACCESS_MODE" = "local" ]; then
    pub_ip="$(detect_public_ip)"
  fi

  info "安装目录：    ${INSTALL_DIR}"
  info "项目标识：    ${PROJECT_NAME}"
  info "源码版本：    ${git_ver}"
  info "安装器版本：  ${SAVED_SCRIPT_VERSION:-$SCRIPT_VERSION}${SAVED_INSTALLED_AT:+（更新时间：${SAVED_INSTALLED_AT}）}"
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
    local https_code cert_end=""
    https_code="$(curl --max-time 8 -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/api/health" 2>/dev/null || echo 000)"
    if [ "$https_code" = "200" ]; then
      ok "公网 HTTPS 接口正常（https://${DOMAIN}/api/health -> 200）"
    else
      warn "公网 HTTPS 接口返回：${https_code}（如刚绑定域名，证书签发可能需要 10–30 秒）"
    fi
    if command -v timeout >/dev/null 2>&1; then
      cert_end="$(echo | timeout 8 openssl s_client -connect "${DOMAIN}:443" -servername "$DOMAIN" 2>/dev/null \
        | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2- || true)"
    elif [ "$https_code" != "000" ]; then
      cert_end="$(echo | openssl s_client -connect "${DOMAIN}:443" -servername "$DOMAIN" 2>/dev/null \
        | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2- || true)"
    fi
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
      dc up -d --no-build db api web || { err "启动服务失败"; return 1; }
      ok "服务已启动"
      ;;
    stop)
      step "停止 ${APP_LABEL} 服务"
      dc stop api web db || { err "停止服务失败"; return 1; }
      ok "服务已停止"
      ;;
    restart)
      step "重启 ${APP_LABEL} 服务"
      dc restart api web || { err "重启服务失败"; return 1; }
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

cleanup_project_containers_fallback() {
  local purge="$1"
  local cids nets vols
  cids="$(docker ps -aq --filter "label=com.docker.compose.project=${PROJECT_NAME}" 2>/dev/null || true)"
  if [ -n "$cids" ]; then
    docker rm -f $cids >/dev/null 2>&1 || return 1
  fi
  nets="$(docker network ls -q --filter "label=com.docker.compose.project=${PROJECT_NAME}" 2>/dev/null || true)"
  if [ -n "$nets" ]; then
    docker network rm $nets >/dev/null 2>&1 || true
  fi
  if [ "$purge" = "1" ]; then
    vols="$(docker volume ls -q --filter "label=com.docker.compose.project=${PROJECT_NAME}" 2>/dev/null || true)"
    if [ -n "$vols" ]; then
      docker volume rm $vols >/dev/null 2>&1 || true
    fi
  fi
  return 0
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

  if docker_bin_ok && docker_daemon_ok; then
    info "正在停止并移除容器..."
    local down_args=(down --remove-orphans)
    if [ "$purge" = "1" ]; then down_args=(down -v --remove-orphans); fi

    local down_ok=0
    if [ -f "$INSTALL_DIR/docker-compose.yml" ] && [ -f "$INSTALL_DIR/.env" ]; then
      if COMPOSE_PROJECT_NAME="$PROJECT_NAME" docker compose \
        -f "$INSTALL_DIR/docker-compose.yml" \
        --env-file "$INSTALL_DIR/.env" \
        "${down_args[@]}" </dev/null >/dev/null 2>&1; then
        down_ok=1
      fi
    fi
    if [ "$down_ok" != "1" ]; then
      if cleanup_project_containers_fallback "$purge"; then
        down_ok=1
      fi
    fi
    if [ "$down_ok" = "1" ]; then
      if [ "$purge" = "1" ]; then
        ok "已移除容器及关联数据卷"
      else
        ok "已停止并移除容器（PostgreSQL 数据卷已安全保留）"
      fi
    else
      warn "部分容器移除失败，请手工执行 docker ps -a 检查项目 ${PROJECT_NAME} 残留"
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

# ── 交互子壳隔离执行器（保留 set -Eeuo pipefail 与回滚钩子，绝不因 die 炸退菜单）──
run_menu_action() {
  local _rc=0
  set +e
  (
    set -Eeuo pipefail
    trap on_signal INT TERM
    trap on_fail EXIT
    "$@"
  )
  _rc=$?
  set -e
  LAST_MENU_RC="$_rc"
  return 0
}

sync_menu_runtime_state() {
  ACCESS_MODE=""; DOMAIN=""; NO_DOMAIN=0
  WEB_BIND=""; WEB_PORT=""; WEB_TLS_PORT=""
  API_BIND="127.0.0.1"; API_PORT=""
  SUB_NAME=""; ENABLE_COLLECTOR=""
  ADMIN_EMAIL=""; ADMIN_PASSWORD=""
  STORE_RENEW_BASE_URL=""; HY2_INSTALL_SCRIPT_URL=""; HY2_INSTALLER_REPO=""
  SOURCE_COMMIT=""
  FLAG_ACCESS_MODE=0; FLAG_DOMAIN=0; FLAG_DOMAIN_SET=0; FLAG_NO_DOMAIN_SET=0
  FLAG_WEB_BIND=0; FLAG_WEB_PORT=0; FLAG_WEB_TLS_PORT=0
  FLAG_API_BIND=0; FLAG_API_PORT=0; FLAG_SUB_NAME=0; FLAG_COLLECTOR=0
  FLAG_ADMIN_EMAIL=0; FLAG_ADMIN_PASSWORD=0
  FLAG_STORE_RENEW=0; FLAG_HY2_SCRIPT=0; FLAG_HY2_REPO=0
  load_saved_state
  prefill_from_saved
}

# ── 交互子菜单：修改配置 & 服务运维 ────────────────────────────────────────────
_cfg_change_access() {
  FLAG_ACCESS_MODE=0; FLAG_DOMAIN=0; FLAG_WEB_BIND=0; FLAG_WEB_PORT=0; FLAG_WEB_TLS_PORT=0; NO_DOMAIN=0
  WEB_PORT=""; WEB_TLS_PORT=""; WEB_BIND=""
  prompt_access_mode_interactive
  validate_inputs
  plan_access_and_ports
  if [ -n "$DOMAIN" ] && { [ "$CADDY_MODE" = "host-caddy" ] || [ "$CADDY_MODE" = "docker-caddy" ]; }; then
    if caddyfile_has_domain_conflict "$CADDY_FILE" "$DOMAIN"; then
      die "外层反代配置 $CADDY_FILE 中已存在域名 $DOMAIN 的其他站点块，已中止"
    fi
  fi
  if should_remove_old_caddy; then
    remove_external_caddy_block
  fi
  if [ -n "$DOMAIN" ]; then
    check_dns_before_ssl
  fi
  write_env_file
  start_and_verify_stack 0 || die "修改访问方式后容器健康验收未通过，请排查上方日志"
  apply_external_caddy
  save_state
  ok "访问方式与域名配置已生效！"
}

_cfg_change_ports() {
  if [ "$ACCESS_MODE" = "self" ]; then
    info "当前为内置自动 HTTPS（self）模式，前端端口固定使用 80/443，仅调整后端 API 端口。"
  else
    WEB_PORT="$(ask_port "新的前端访问端口" "${WEB_PORT:-$DEFAULT_LOCAL_WEB_PORT}")"
    FLAG_WEB_PORT=1
  fi
  API_PORT="$(ask_port "新的后端 API 端口" "${API_PORT:-$DEFAULT_API_PORT}")"
  FLAG_API_PORT=1
  if [ "$WEB_PORT" = "$WEB_TLS_PORT" ]; then
    WEB_TLS_PORT="auto"
    FLAG_WEB_TLS_PORT=0
  fi
  validate_inputs
  plan_access_and_ports
  write_env_file
  start_and_verify_stack 0 || die "修改端口后容器健康验收未通过，请排查上方日志"
  apply_external_caddy
  save_state
  ok "端口修改已生效！"
}

_cfg_change_sub_name() {
  SUB_NAME="$(ask "新的客户端默认订阅分组名" "${SUB_NAME:-$DEFAULT_SUB_NAME}")"
  if [ -z "$SUB_NAME" ]; then SUB_NAME="$DEFAULT_SUB_NAME"; fi
  validate_inputs
  write_env_file
  start_and_verify_stack 0 || die "更新订阅分组名后服务验收未通过"
  save_state
  ok "默认订阅分组名已更新为：${SUB_NAME}"
}

_cfg_toggle_collector() {
  if [ "${ENABLE_COLLECTOR:-true}" = "true" ]; then
    ENABLE_COLLECTOR="false"
  else
    ENABLE_COLLECTOR="true"
  fi
  write_env_file
  start_and_verify_stack 0 || die "切换周期采集器后服务验收未通过"
  save_state
  ok "周期采集器已切换为：${ENABLE_COLLECTOR}"
}

_cfg_change_integrations() {
  prompt_optional_integrations
  validate_inputs
  write_env_file
  start_and_verify_stack 0 || die "更新外部联动配置后服务验收未通过"
  save_state
  ok "外部联动配置已更新并生效！"
}

menu_configure() {
  require_root || return 0
  sync_menu_runtime_state
  need_state || return 0
  while :; do
    sync_menu_runtime_state
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
      1) run_menu_action _cfg_change_access ;;
      2) run_menu_action _cfg_change_ports ;;
      3) run_menu_action _cfg_change_sub_name ;;
      4) run_menu_action _cfg_toggle_collector ;;
      5) run_menu_action _cfg_change_integrations ;;
      *) warn "请输入 0 到 5 之间的数字" ;;
    esac
  done
}

_ops_run_migrate() {
  require_root || return 1
  step "执行数据库迁移"
  dc run --rm migrate || die "数据库迁移执行失败"
  ok "数据库迁移检查完成"
}

menu_ops() {
  sync_menu_runtime_state
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
      1) run_menu_action do_lifecycle start ;;
      2) run_menu_action do_lifecycle stop ;;
      3) run_menu_action do_lifecycle restart ;;
      4) LOGS_SERVICE="api"; run_menu_action do_logs ;;
      5) LOGS_SERVICE="web"; run_menu_action do_logs ;;
      6) LOGS_SERVICE="db"; run_menu_action do_logs ;;
      7) run_menu_action do_backup ;;
      8) run_menu_action _ops_run_migrate ;;
      *) warn "请输入 0 到 8 之间的数字" ;;
    esac
  done
}

# ── 主交互菜单（每项独立子壳执行，绝不抑制 errexit 且状态实时回读）─────────────
render_menu_header() {
  sync_menu_runtime_state

  local install_state="未安装"
  local running_info="—"
  local access_info="尚未部署"
  local ver_info="—"

  if [ -f "$INSTALL_DIR/install.conf" ] || [ -f "$INSTALL_DIR/.env" ]; then
    install_state="已安装"
    if [ -d "$INSTALL_DIR/.git" ] && command -v git >/dev/null 2>&1; then
      ver_info="$(git -C "$INSTALL_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown) (${APP_REF})"
    elif [ -n "$SAVED_SOURCE_COMMIT" ]; then
      ver_info="${SAVED_SOURCE_COMMIT} (tarball)"
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
        MENU_DIR_BRIDGE="$(mktemp /tmp/teyir-dir-bridge.XXXXXX)"
        register_tmp "$MENU_DIR_BRIDGE"
        run_menu_action do_install
        if [ -s "$MENU_DIR_BRIDGE" ]; then
          local new_dir
          new_dir="$(head -1 "$MENU_DIR_BRIDGE" | tr -d '\r\n')"
          if is_safe_install_dir "$new_dir"; then
            INSTALL_DIR="$new_dir"
          fi
        fi
        rm -f "$MENU_DIR_BRIDGE" 2>/dev/null || true
        MENU_DIR_BRIDGE=""
        pause_enter
        ;;
      2)
        run_menu_action do_upgrade
        pause_enter
        ;;
      3)
        run_menu_action do_status
        pause_enter
        ;;
      4)
        menu_configure
        ;;
      5)
        menu_ops
        ;;
      6)
        run_menu_action do_check_update
        pause_enter
        ;;
      7)
        run_menu_action do_uninstall
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
