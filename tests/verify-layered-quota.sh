#!/usr/bin/env bash
# 验收「配额分层」这一轮改动：读桶与写桶必须互不侵占。
#
# 用法（在被测主机本机执行）：
#   cd /opt/hy2-ops-console && set -a && . ./.env && set +a && bash tests/verify-layered-quota.sh
#
# 全程只在 127.0.0.1 上打，且每个请求都停在「本地校验失败」那一步，
# 不会触及节点门户 —— 涉及生产的验证不该顺带扰动生产。
#
# ⚠️ 本脚本会短暂用尽写配额（10 秒），跑完自带冷却。

set -u

API=${API:-http://127.0.0.1:8080}
NODE=${NODE:-}
WINDOW=10
if [ -z "$NODE" ]; then
  echo "请通过环境变量 NODE 指定待测节点 UUID，例如：NODE=00000000-0000-0000-0000-000000000001 bash tests/verify-layered-quota.sh" >&2
  exit 1
fi

# ★ 选请求的讲究（第一版就在这里栽了）：
#
# portalNodeAction 的执行顺序是
#   ① JSON 解码 → 400        ② 动词查表 → 400        ③ 危险动词缺确认 → 428
#   ④ enforceWriteLimit  ←── 配额在这里才被消耗
#   ⑤ 参数校验 → 400（不碰节点）  ⑥ 解析凭据  ⑦ 能力门禁 → 409  ⑧ 真正下发
#
# 所以「{"verb":""}」——最顺手的那个畸形请求——在第 ② 步就返回了，
# 排在限流器**之前**，连打一百次也不会触发 429。用它去"打满写桶"，
# 测出来的永远是"配额没生效"。
#
# 能消耗配额又碰不到节点的请求只有一类：过了 ①②③、栽在第 ⑤ 步的。
# set_bbr 的 version 只允许 whitelist 里的值，传 "v9" 正好卡在这里。
BODY_WRITE='{"verb":"set_bbr","params":{"version":"v9"}}'
BODY_PRELIM='{"verb":""}'

pass=0
fail=0
ok()   { echo "  [PASS] $1"; pass=$((pass + 1)); }
bad()  { echo "  [FAIL] $1"; fail=$((fail + 1)); }
want() { if [ "$2" = "$3" ]; then ok "$1（$2）"; else bad "$1：期望 $3，实际 $2"; fi; }

action() {  # action <body> <extra-header...>  —— 打印状态码
  curl -s -o /tmp/ly_body.json -w '%{http_code}' -X POST \
    "$API/api/nodes/$NODE/portal-action" -H "$AUTH" -H 'Content-Type: application/json' \
    "$@" -d "$BODY_WRITE"
}
prelim() {
  curl -s -o /tmp/ly_body.json -w '%{http_code}' -X POST \
    "$API/api/nodes/$NODE/portal-action" -H "$AUTH" -H 'Content-Type: application/json' \
    -d "$BODY_PRELIM"
}

echo "== 0. 健康 =="
want "服务健康" "$(curl -s -o /dev/null -w '%{http_code}' "$API/api/health")" 200

echo "== 1. 登录 =="
if [ -z "${SEED_ADMIN_EMAIL:-}" ] || [ -z "${SEED_ADMIN_PASSWORD:-}" ]; then
  echo "  未设置 SEED_ADMIN_EMAIL / SEED_ADMIN_PASSWORD，先 source .env"; exit 1
fi
TOK=$(curl -s -X POST "$API/api/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$SEED_ADMIN_EMAIL\",\"password\":\"$SEED_ADMIN_PASSWORD\"}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin).get("token",""))')
if [ -n "$TOK" ]; then ok "取得 token"; else bad "登录失败"; exit 1; fi
AUTH="Authorization: Bearer $TOK"

echo "== 2. 顺序契约：前置校验不消耗配额 =="
prelim_codes=""
for _ in $(seq 1 25); do
  prelim_codes="$prelim_codes$(prelim)"
done
case "$prelim_codes" in
  *429*) bad "空动词连打 25 次出现了 429 —— 前置校验不该消耗配额" ;;
  *)     ok "空动词连打 25 次全部 400、无 429（未消耗写配额）" ;;
esac
case "$prelim_codes" in
  *400*) ok "空动词走的是 400 分支" ;;
  *)     bad "空动词未返回 400：$prelim_codes" ;;
esac

echo "== 3. 打满写桶（set_bbr 非法版本：过限流器、栽在参数校验）=="
echo "    先确认这个请求本身消耗配额：单发一次"
want "单发一次应当 400（说明已越过限流器）" "$(action)" 400

limited_at="" ; retry_after="" ; body=""
for i in $(seq 2 27); do
  code=$(action)
  if [ "$code" = "429" ]; then
    limited_at=$i ; body=$(cat /tmp/ly_body.json)
    retry_after=$(curl -s -D - -o /dev/null -X POST \
      "$API/api/nodes/$NODE/portal-action" -H "$AUTH" \
      -H 'Content-Type: application/json' -d "$BODY_WRITE" \
      | tr -d '\r' | awk 'tolower($1)=="retry-after:"{print $2}')
    break
  fi
done

if [ -z "$limited_at" ]; then
  bad "连打 26 次仍未触发 429（写配额没生效？）"
else
  ok "写桶在第 $limited_at 次触顶"
  if [ "$limited_at" -ge 18 ] && [ "$limited_at" -le 23 ]; then
    ok "触发点与配置（20 / 10s）一致"
  else
    bad "触发点偏离配置：第 $limited_at 次"
  fi
fi

echo "== 4. 429 响应体契约 =="
echo "     body       : $body"
echo "     Retry-After: ${retry_after:-（缺）}"
case "$body" in
  *'"limit_scope":"write"'*) ok "limit_scope=write" ;;
  *) bad "limit_scope 不是 write" ;;
esac
case "$body" in
  *'未下发'*) ok "文案说明操作未下发（否则操作者会以为开关已切过去）" ;;
  *) bad "文案没说明操作未下发" ;;
esac
case "$body" in
  *'"retry_after_seconds"'*) ok "带 retry_after_seconds" ;;
  *) bad "缺 retry_after_seconds" ;;
esac
if [ -n "$retry_after" ] && [ "$retry_after" -ge 1 ] 2>/dev/null; then
  ok "Retry-After 头存在且为正整数（$retry_after）"
else
  bad "Retry-After 头缺失或非法（${retry_after:-空}）"
fi

echo "== 5. ★ 写桶已满时，读桶必须毫发无损（本次改动的核心）=="
for path in /api/subscriptions /api/subscription /api/nodes /api/servers /api/runtime; do
  want "写桶满时 GET $path" "$(curl -s -o /dev/null -w '%{http_code}' "$API$path" -H "$AUTH")" 200
done

# 连打 25 次读。旧实现下读写共用一个 20 次桶，这里会全线 429。
seq_read=0
for _ in $(seq 1 25); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "$API/api/subscriptions" -H "$AUTH")" = "200" ] \
    && seq_read=$((seq_read + 1))
done
want "写桶满时连打 25 次只读，全部 200" "$seq_read" 25

echo "== 6. 冷却后写桶恢复 =="
echo "    … 等待 $((WINDOW + 2))s 让窗口滑过"
sleep $((WINDOW + 2))
want "窗口滑过后写桶恢复（仍是 400 参数校验，不是 429）" "$(action)" 400

echo
echo "==========  通过 $pass / 失败 $fail  =========="
[ "$fail" -eq 0 ]
