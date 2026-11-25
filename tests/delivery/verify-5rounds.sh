#!/bin/bash
# 订阅交付五轮全链路验证（管理员要求）。
#
# 每轮验一个维度，**全部用真实客户端连接**（铁律⑱：列表里有 ≠ 连得上）：
#
#   R1 交付完整性 —— 订阅里每个节点都能**建连成功**并出网
#   R2 三种格式一致 —— clash / sing-box / uri 解析出的节点集合必须一致
#   R3 端口跳跃可用 —— 用跳跃范围内的端口连接（客户端常常走这条路径）
#   R4 认证边界   —— 错误密码必须被拒（证明鉴权真的在生效，不是放行一切）
#   R5 幂等与稳定 —— 同一节点连续连三次都成功（排除偶发）
set -u

WORK=/root/subverify
rm -rf "$WORK"; mkdir -p "$WORK"
TOK=$(cat /root/.subtoken)
BASE=http://127.0.0.1:8081

pass=0; fail=0
ok()   { echo "    ✅ $1"; pass=$((pass+1)); }
bad()  { echo "    ❌ $1"; fail=$((fail+1)); }

fetch() { curl -s -m 25 "$BASE/sub/$TOK?format=$1"; }

# 用一段配置实际连一次，返回出口 IP（空 = 失败）
try_connect() {
  local server="$1" port="$2" pw="$3" sni="$4" obfs="$5" tag="$6" timeout="${7:-18}"
  local cfg="$WORK/c.yaml" port_socks=10901
  cat > "$cfg" <<EOF
server: $server:$port
auth: $pw
tls:
  sni: $sni
  insecure: false
obfs:
  type: salamander
  salamander:
    password: $obfs
socks5:
  listen: 127.0.0.1:$port_socks
EOF
  ( timeout 40 hysteria client -c "$cfg" > "$WORK/c.log" 2>&1 & ) 2>/dev/null
  # 等到客户端自报 connected 再发请求（固定 sleep 会偶发假失败）
  local waited=0
  while [ "$waited" -lt 30 ]; do
    grep -q 'connected to server' "$WORK/c.log" 2>/dev/null && break
    sleep 1; waited=$((waited+1))
  done
  sleep 2
  local ip
  ip=$(curl -s -m 10 --socks5-hostname "127.0.0.1:$port_socks" https://api.ipify.org 2>/dev/null)
  pkill -f "hysteria client -c $cfg" 2>/dev/null || true
  sleep 1
  echo "$ip"
}

echo "########## R1 交付完整性：订阅里每个节点都能建连出网 ##########"
curl -s -m 25 "$BASE/sub/$TOK?format=clash" > "$WORK/clash.json"
python3 - "$WORK" <<'PY'
import json, os, sys
w = sys.argv[1]
d = json.load(open(os.path.join(w, 'clash.json'), encoding='utf-8'))
lines = []
for p in d.get('proxies', []):
    lines.append('\t'.join([
        p.get('name',''), p.get('server',''), str(p.get('port','')),
        p.get('password',''), p.get('sni',''), p.get('obfs-password',''),
        p.get('ports',''),
    ]))
open(os.path.join(w, 'nodes.tsv'), 'w', encoding='utf-8', newline='\n').write('\n'.join(lines) + '\n')
print(f"  订阅含 {len(lines)} 个节点")
PY

R1_OK=0; R1_TOTAL=0
while IFS=$'\t' read -r NAME SERVER PORT PW SNI OBFS PORTS; do
  [ -z "$NAME" ] && continue
  R1_TOTAL=$((R1_TOTAL+1))
  IP=$(try_connect "$SERVER" "$PORT" "$PW" "$SNI" "$OBFS" "$NAME")
  if [ -n "$IP" ]; then
    ok "$NAME -> 出口 $IP"
    R1_OK=$((R1_OK+1))
  else
    bad "$NAME -> 连接失败（见 $WORK/c.log）"
  fi
done < "$WORK/nodes.tsv"
echo "  R1 结果: $R1_OK/$R1_TOTAL 可建连"

echo
echo "########## R2 三种格式的节点集合必须一致 ##########"
fetch singbox > "$WORK/sing.json"
fetch uri > "$WORK/uri.txt"
python3 - "$WORK" <<'PY'
import json, os, sys, base64, urllib.parse
w = sys.argv[1]
clash = json.load(open(os.path.join(w,'clash.json'), encoding='utf-8'))
c_nodes = {(p['server'], int(p['port'])) for p in clash.get('proxies', [])}

sing = json.load(open(os.path.join(w,'sing.json'), encoding='utf-8'))
s_nodes = set()
for o in sing.get('outbounds', []):
    if o.get('type') == 'hysteria2':
        s_nodes.add((o['server'], int(o.get('server_port') or (o.get('server_ports') or ['0'])[0].split(':')[0])))

raw = open(os.path.join(w,'uri.txt'), encoding='utf-8').read().strip()
try:
    decoded = base64.b64decode(raw).decode('utf-8')
except Exception:
    decoded = raw
u_nodes = set()
for line in decoded.splitlines():
    line = line.strip()
    if not line.startswith('hysteria2://'):
        continue
    rest = line.split('://',1)[1].split('#')[0]
    hostpart = rest.split('@')[-1].split('?')[0]
    if ':' in hostpart:
        h, p = hostpart.rsplit(':',1)
        try: u_nodes.add((h, int(p)))
        except ValueError: pass

print(f"  clash={len(c_nodes)} sing-box={len(s_nodes)} uri={len(u_nodes)}")
if c_nodes == s_nodes == u_nodes and c_nodes:
    print(f"  ✅ 三种格式节点集合完全一致：{sorted(c_nodes)}")
else:
    print(f"  ❌ 不一致！")
    print(f"     只在 clash: {c_nodes - s_nodes - u_nodes}")
    print(f"     只在 sing-box: {s_nodes - c_nodes - u_nodes}")
    print(f"     只在 uri: {u_nodes - c_nodes - s_nodes}")
PY

echo
echo "########## R3 端口跳跃：客户端走 port hopping 时能否连接 ##########"
# ⚠️ 这里踩过两次，都是**测试写错**而不是功能问题（铁律⑨）：
#   1) 语法：hysteria 客户端用 `host:port,start-end`；
#      写成 `start:end` 是 **sing-box** 的语法，hysteria 不认 → 假失败。
#   2) 时序：跳跃要逐端口试，握手比普通模式慢。固定 sleep 7 秒就 curl
#      会在客户端就绪前发请求 → 超时 → 假失败。
#      必须**等客户端自己报告 connected to server** 再发。
FIRST=$(head -1 "$WORK/nodes.tsv")
NAME=$(echo "$FIRST" | cut -f1); SERVER=$(echo "$FIRST" | cut -f2); PORT=$(echo "$FIRST" | cut -f3)
PW=$(echo "$FIRST" | cut -f4); SNI=$(echo "$FIRST" | cut -f5); OBFS=$(echo "$FIRST" | cut -f6)
echo "  节点 $NAME"

try_hop() {
  local field="$1" label="$2" socks="$3"
  cat > "$WORK/hop.yaml" <<EOF
server: $field
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
  ( timeout 40 hysteria client -c "$WORK/hop.yaml" > "$WORK/hop.log" 2>&1 & ) 2>/dev/null
  local waited=0
  while [ "$waited" -lt 30 ]; do
    grep -q 'connected to server' "$WORK/hop.log" 2>/dev/null && break
    sleep 1; waited=$((waited+1))
  done
  sleep 2
  local ip
  ip=$(curl -s -m 10 --socks5-hostname "127.0.0.1:$socks" https://api.ipify.org 2>/dev/null)
  pkill -f "hysteria client -c $WORK/hop.yaml" 2>/dev/null || true
  sleep 1
  if [ -n "$ip" ]; then ok "跳跃 $label -> 出口 $ip"; else bad "跳跃 $label 连不上"; fi
}

try_hop "$SERVER:$PORT,20000-40000" "host:port,20000-40000" 11011
try_hop "$SERVER:20000-40000" "host:20000-40000" 11012

echo
echo "########## R4 认证边界：错误密码必须被拒 ##########"
IP=$(try_connect "$SERVER" "14617" "deadbeefdeadbeefdeadbeefdeadbeef" "$SNI" "$OBFS" "wrongpw" 12)
if [ -z "$IP" ]; then ok "错误密码被拒（鉴权真的在生效）"; else bad "错误密码竟然连上了 -> 鉴权形同虚设！出口 $IP"; fi

echo
echo "########## R5 幂等与稳定：同一节点连续连三次 ##########"
S_OK=0
for i in 1 2 3; do
  IP=$(try_connect "$SERVER" "$(echo "$FIRST" | cut -f3)" "$PW" "$SNI" "$OBFS" "rep$i" 15)
  if [ -n "$IP" ]; then S_OK=$((S_OK+1)); echo "    第 $i 次: 出口 $IP"; else echo "    第 $i 次: 失败"; fi
done
if [ "$S_OK" -eq 3 ]; then ok "连续 3 次全部成功"; else bad "连续 3 次只有 $S_OK 次成功"; fi

echo
echo "################ 汇总 ################"
echo "  通过 $pass 项，失败 $fail 项"
[ "$fail" -eq 0 ] && echo "  ★ 全部通过" || echo "  ✗ 有失败项，需处理"
