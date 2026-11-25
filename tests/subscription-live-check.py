#!/usr/bin/env python3
"""订阅链接能不能真的订阅：拉链接 -> 解出节点 -> 拿口令去节点侧 /auth 认证。

「链接能拉到」和「客户能连上」是两件事。前者只需 HTTP 200，
后者要求订阅里的口令在节点侧确实被认可 —— 所以最后一步必须打到节点。
"""
import base64
import json
import sys
import urllib.error
import urllib.request
from urllib.parse import urlsplit, parse_qs, unquote

BASE = "https://panel.example.com"
EMAIL = "admin@nodeops.local"
PASSWORD = sys.argv[1]

TOKEN = ""


def call(method, path, body=None, headers=None):
    req = urllib.request.Request(
        BASE + path, method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Content-Type": "application/json", **(headers or {})},
    )
    if TOKEN:
        req.add_header("Authorization", f"Bearer {TOKEN}")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            t = r.read().decode()
            return r.status, (json.loads(t) if t.strip() else None)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def raw_get(url, ua):
    req = urllib.request.Request(url, headers={"User-Agent": ua})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.read().decode(), dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(), dict(e.headers)


TOKEN = call("POST", "/api/login", {"email": EMAIL, "password": PASSWORD})[1]["token"]
_, sub = call("GET", "/api/subscription")
token = sub["token"]
print(f"合并订阅 token={token}")
print(f"included={len(sub['included'])} excluded={len(sub['excluded'])}")
for x in sub["excluded"]:
    print(f"  ! 排除 {x['name']}: {x['reason']}")

print("\n" + "=" * 78)
print("【订阅链接拉取】")
UAS = [
    ("Clash", "clash-verge/v1.6.0 Clash/v1.18.0"),
    ("v2rayN", "v2rayN/6.40"),
    ("sing-box", "sing-box/1.11.0"),
    ("Shadowrocket", "Shadowrocket/2.2.5"),
    ("curl(兜底)", "curl/8.5.0"),
]
for label, ua in UAS:
    st, body, hdrs = raw_get(f"{BASE}/sub/{token}", ua)
    ctype = hdrs.get("Content-Type", "")
    print(f"  {label:14} -> {st}  Content-Type={ctype or '-':40} 长度={len(body)}")

print("\n  ?format= 显式指定：")
for f in ("clash", "singbox", "uri", "base64"):
    st, body, hdrs = raw_get(f"{BASE}/sub/{token}?format={f}", "curl/8.5.0")
    print(f"    format={f:8} -> {st}  {hdrs.get('Content-Type','-'):40} 长度={len(body)}")

print("\n" + "=" * 78)
print("【解出节点】")
st, body, _ = raw_get(f"{BASE}/sub/{token}?format=uri", "curl/8.5.0")
text = body.strip()
try:
    text = base64.b64decode(body.strip()).decode()
except Exception:
    pass
uris = [u for u in text.replace("\r", "").split("\n") if u.strip()]
print(f"  共 {len(uris)} 行：")
parsed = []
for u in uris:
    sp = urlsplit(u)
    q = parse_qs(sp.query)
    auth = unquote(sp.netloc.split("@")[0]) if "@" in sp.netloc else ""
    hostport = sp.netloc.split("@")[-1]
    info = {
        "scheme": sp.scheme,
        "host": hostport.rsplit(":", 1)[0],
        "port": hostport.rsplit(":", 1)[-1],
        "auth_len": len(auth),
        "sni": (q.get("sni") or ["-"])[0],
        "insecure": (q.get("insecure") or ["0"])[0],
        "obfs": "有" if q.get("obfs") else "无",
        "obfs_password_len": len((q.get("obfs-password") or [""])[0]),
        "pin": "有" if q.get("pinSHA256") else "无",
        "mport": (q.get("mport") or ["-"])[0],
        "name": unquote(sp.fragment or ""),
    }
    parsed.append((auth, info))
    print(f"    - {info['name'] or '(无名)':22} {info['scheme']}://***@{info['host']}:{info['port']} "
          f"sni={info['sni']} insecure={info['insecure']} obfs={info['obfs']}({info['obfs_password_len']}位) "
          f"mport={info['mport']}")

print("\n" + "=" * 78)
print("【交付页 /sub/view】")
st, view = call("GET", f"/api/sub/view/{token}")
print(f"  HTTP={st}")
if isinstance(view, dict):
    print(f"  label={view.get('label')} enabled={view.get('enabled')} expired={view.get('expired')} "
          f"days_left={view.get('days_left')}")
    print(f"  node_count={view.get('node_count')}  excluded={len(view.get('excluded') or [])}")
    for k in ("subscription_url", "clash_url", "singbox_url", "uri_url", "renew_url"):
        print(f"  {k:18}={view.get(k)}")

# 把口令交给下一步（节点侧 /auth 认证）
if parsed:
    with open("tests/.sub-credentials.json", "w") as f:
        json.dump([{"password": a, **i} for a, i in parsed], f, ensure_ascii=False, indent=2)
    print("\n口令已写入 tests/.sub-credentials.json，供节点侧 /auth 认证用")
