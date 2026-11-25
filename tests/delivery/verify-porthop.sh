#!/bin/bash
# R3 修正版：用**正确的 hysteria 客户端语法**测端口跳跃。
#
# hysteria 的 server 字段语法：
#     server: host:port                         普通
#     server: host:port,start-end               端口跳跃
# 上一版脚本用 `sed 's/-/:/'` 转成 `20000:40000` —— 那是 **sing-box** 语法，
# hysteria 客户端不认，于是连基准都失败。是**测试写错**（铁律⑨）。
set -u
WORK=/root/subverify
mkdir -p "$WORK"
BASE=http://127.0.0.1:8081
TOK=$(cat /root/.subtoken)

# 每次重新拉订阅（保证密码是最新的）
curl -s -m 25 "$BASE/sub/$TOK?format=clash" > "$WORK/clash.json"
python3 - "$WORK" <<'PY'
import json, os, sys
w = sys.argv[1]
d = json.load(open(os.path.join(w,'clash.json'), encoding='utf-8'))
p = d['proxies'][0]
open(os.path.join(w,'one.env'), 'w', encoding='utf-8').write(
    f"SERVER={p['server']}\nPORT={p['port']}\nPW={p['password']}\nSNI={p['sni']}\nOBFS={p.get('obfs-password','')}\nPORTS={p.get('ports','')}\nNAME={p['name']}\n")
print("节点:", p['name'], p['server'], p['port'], "ports=", p.get('ports'))
PY
. "$WORK/one.env"

try() {
  local server_field="$1" label="$2" socks="$3"
  cat > "$WORK/t.yaml" <<EOF
server: $server_field
auth: $PW
tls:
  sni: $SNI
  insecure: false
obfs:
  type: salamander
  salamander:
    password: $OBFS
socks5:
  listen: 127.0.0.1:$socks
EOF
  ( timeout 40 hysteria client -c "$WORK/t.yaml" > "$WORK/t.log" 2>&1 & ) 2>/dev/null
  # ⚠️ 必须**等到客户端自己报告 connected** 再发请求。
  # 上一版固定 sleep 7 秒就 curl —— 端口跳跃要逐端口试，握手比普通模式慢，
  # 结果 curl 在客户端还没就绪时发出、超时，被误判成"跳跃不可用"。
  # 这是**测试时序问题**，不是功能问题（客户端日志里明明写着 connected）。
  local waited=0
  while [ "$waited" -lt 30 ]; do
    if grep -q 'connected to server' "$WORK/t.log" 2>/dev/null; then break; fi
    sleep 1; waited=$((waited+1))
  done
  sleep 2
  local ip
  ip=$(curl -s -m 10 --socks5-hostname "127.0.0.1:$socks" https://api.ipify.org 2>/dev/null)
  pkill -f "hysteria client -c $WORK/t.yaml" 2>/dev/null || true
  sleep 1
  if [ -n "$ip" ]; then
    printf '  ✅ %-44s 出口 %s（握手等待 %ss）\n' "$label" "$ip" "$waited"
    return 0
  else
    printf '  ❌ %-44s 失败：%s\n' "$label" "$(grep -iE 'FATAL|error' "$WORK/t.log" | head -1 | cut -c1-80)"
    return 1
  fi
}

echo
echo "=== A. 基准：不带跳跃 ==="
try "$SERVER:$PORT" "host:port（普通）" 11001

echo
echo "=== B. 带跳跃（hysteria 语法 host:port,start-end）==="
try "$SERVER:$PORT,20000-40000" "host:port,20000-40000" 11002

echo
echo "=== C. 只给范围 ==="
try "$SERVER:20000-40000" "host:20000-40000" 11003
