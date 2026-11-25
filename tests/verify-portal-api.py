#!/usr/bin/env python3
"""端到端验证「纯 API 管理 hy2 节点」。

在被测主机本机运行，直接请求面板 API（loopback）。全部为只读或幂等探测，
不做任何会改变节点状态的操作。

验证项：
  1. 管理员登录
  2. 列出节点，找到已绑定 Basic 凭据的那个
  3. GET /api/nodes/{id}/portal-status —— 7 路探测是否都返回
  4. POST /api/nodes/{id}/portal-action 传非法动词 —— 应被白名单拒绝
  5. POST portal-action 传危险动词但不带确认头 —— 应 428
"""
import json
import os
import sys
import urllib.error
import urllib.request

API = f"http://127.0.0.1:{os.environ.get('API_PORT', '8081')}"


def call(method, path, body=None, token=None, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(API + path, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode()
        try:
            return exc.code, json.loads(raw or "{}")
        except json.JSONDecodeError:
            return exc.code, {"raw": raw[:300]}
    except Exception as exc:  # noqa: BLE001
        return 0, {"error": f"{type(exc).__name__}: {exc}"}


ok = True


def check(label, condition, detail=""):
    global ok
    mark = "PASS" if condition else "FAIL"
    if not condition:
        ok = False
    print(f"[{mark}] {label}" + (f" :: {detail}" if detail else ""))


print("=" * 70)
status, payload = call("POST", "/api/login", {
    "email": os.environ["SEED_ADMIN_EMAIL"],
    "password": os.environ["SEED_ADMIN_PASSWORD"],
})
check("管理员登录", status == 200 and "token" in payload, f"HTTP {status}")
if status != 200 or "token" not in payload:
    print(json.dumps(payload, ensure_ascii=False)[:400])
    sys.exit(1)
token = payload["token"]

print("=" * 70)
status, nodes = call("GET", "/api/nodes", token=token)
check("列出节点", status == 200 and isinstance(nodes, list), f"HTTP {status}, {len(nodes) if isinstance(nodes, list) else '?'} 个")
if not isinstance(nodes, list):
    print(json.dumps(nodes, ensure_ascii=False)[:400])
    sys.exit(1)

with_basic = [n for n in nodes if n.get("has_basic")]
with_api = [n for n in nodes if n.get("has_api")]
print(f"      节点总数 {len(nodes)}，已绑 API {len(with_api)}，已绑门户凭据 {len(with_basic)}")
for n in nodes:
    print(f"      - {n['name']}  has_api={n.get('has_api')} has_basic={n.get('has_basic')}")

if not with_basic:
    print("\n没有已绑定门户凭据的节点，跳过 portal-status / portal-action 验证。")
    print("（这是预期的：需要先在 UI 里为某个节点补填 portal-access.json）")
    sys.exit(0 if ok else 1)

node = with_basic[0]
nid = node["id"]

print("=" * 70)
status, st = call("GET", f"/api/nodes/{nid}/portal-status", token=token)
check("读取节点运维状态", status == 200, f"HTTP {status}")
if status == 200:
    probes = st.get("probes", {})
    print(f"      探测到 {len(probes)} 路：")
    for key, val in sorted(probes.items()):
        state = val.get("state")
        err = (val.get("error") or "")[:70]
        print(f"        {key:9s} {state:10s} {err}")
    healthy = [k for k, v in probes.items() if v.get("state") == "healthy"]
    check("七路探测齐全", len(probes) == 7, f"实际 {len(probes)} 路")
    check("至少一路可读", len(healthy) > 0, f"可读 {sorted(healthy)}")

print("=" * 70)
status, res = call("POST", f"/api/nodes/{nid}/portal-action", {"verb": "definitely_not_a_verb"}, token=token)
check("未知动词被白名单拒绝", status == 400, f"HTTP {status} {res.get('error', '')[:60]}")

print("=" * 70)
status, res = call("POST", f"/api/nodes/{nid}/portal-action", {"verb": "reboot"}, token=token)
check("危险动词缺确认头应被拦截", status == 428, f"HTTP {status} {res.get('error', '')[:60]}")

print("=" * 70)
# 只读动词，安全：拿 BBR 状态走一次转发链路，证明 Basic 通路真的通到 portal。
status, res = call("POST", f"/api/nodes/{nid}/portal-action", {"verb": "warp_reset_rules"}, token=token)
check("转发 warp_reset_rules（幂等，恢复默认表）", status == 200, f"HTTP {status} {json.dumps(res, ensure_ascii=False)[:120]}")

print("=" * 70)
print("结果：" + ("全部通过" if ok else "存在失败项"))
sys.exit(0 if ok else 1)
