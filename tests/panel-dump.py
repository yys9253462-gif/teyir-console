#!/usr/bin/env python3
"""把关键接口的真实内容摊开看 —— 只看 200 不够，
真正的问题（节点进 excluded、服务状态读不到、流量为空）都藏在字段里。"""
import json
import sys
import urllib.error
import urllib.request

BASE = "https://panel.example.com"
EMAIL = "admin@nodeops.local"
PASSWORD = sys.argv[1]


def call(method, path, body=None, headers=None, raw=False):
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
            return r.status, (t if raw else (json.loads(t) if t.strip() else None))
    except urllib.error.HTTPError as e:
        t = e.read().decode()
        try:
            return e.code, json.loads(t)
        except Exception:
            return e.code, t


TOKEN = ""
st, body = call("POST", "/api/login", {"email": EMAIL, "password": PASSWORD})
TOKEN = body["token"]

print("=" * 78)
print("【主机】")
_, servers = call("GET", "/api/servers")
for s in servers:
    print(f"  {s['name']}  host={s['host']}  状态={s['status']}  "
          f"SSH={s['ssh_user']}@{s['ssh_host'] if 'ssh_host' in s else s['host']}:{s['ssh_port']}")
    print(f"     有凭据={s['has_credential']}  允许写={s['allow_write_ops']}  白名单={s['write_units']}")
    print(f"     CPU={s['cpu_percent']}% MEM={s['memory_percent']}% DISK={s['disk_percent']}% 延迟={s['latency_ms']}ms")
    print(f"     实测24h: ↑{s['measured_bytes_24h']} ↓?  portal记账24h={s['portal_bytes_24h']}")
    print(f"     traffic_api_url={s['traffic_api_url']!r}  secret_file={s['traffic_secret_file']!r}")
    st2, svc = call("GET", f"/api/servers/{s['id']}/services")
    print(f"     服务状态 HTTP={st2} units={len(svc.get('units', [])) if isinstance(svc, dict) else '?'} "
          f"note={svc.get('note') or svc.get('error') or '-' if isinstance(svc, dict) else svc}")
    for u in (svc.get("units", []) if isinstance(svc, dict) else []):
        print(f"         - {u.get('unit')}: active={u.get('active')} enabled={u.get('enabled')} {u.get('error','')}")

print()
print("=" * 78)
print("【节点】")
_, nodes = call("GET", "/api/nodes")
for n in nodes:
    print(f"  {n['name']}  id={n['id']}")
    print(f"     server_id={n['server_id']}  listen_port={n['listen_port']}  domain={n['domain']!r}  status={n['status']}")
    print(f"     api_url={n['api_url']!r}  has_api={n['has_api']}  users_count={n['users_count']}  "
          f"api_checked_at={n['api_checked_at']}")
    print(f"     用量 total={n['used_bytes_total']} 24h={n['used_bytes_24h']}  last_checked={n['last_checked_at']}")
    _, ov = call("GET", f"/api/nodes/{n['id']}/overview")
    if isinstance(ov, dict):
        print(f"     总览: meta={ov.get('meta')}")
        print(f"           summary={ov.get('summary')}")
        print(f"           probes={ov.get('probes')}")
        print(f"           unreachable={ov.get('unreachable')!r}")
        print(f"           用户数={len(ov.get('users') or [])}  代理服务数={len(ov.get('proxy_services') or [])}")
    else:
        print(f"     总览失败: {ov}")

print()
print("=" * 78)
print("【合并订阅】")
_, sub = call("GET", "/api/subscription")
print(f"  token={sub.get('token')}  enabled={sub.get('enabled')}  "
      f"access_count={sub.get('access_count')}  last_accessed={sub.get('last_accessed_at')}")
print(f"  url={sub.get('url')}")
print(f"  included={len(sub.get('included') or [])}: " +
      ", ".join(f"{x['name']}@{x['host']}:{x['port']}" for x in (sub.get('included') or [])))
print(f"  excluded={len(sub.get('excluded') or [])}")
for x in sub.get("excluded") or []:
    print(f"      - {x.get('name')}: {x.get('reason')}")

print()
print("=" * 78)
print("【订阅中心】")
_, sc = call("GET", "/api/subscriptions")
store = sc.get("store", {})
print(f"  商城对接 enabled={store.get('enabled')} key_hint={store.get('key_hint')}")
print(f"  ip_window_hours={store.get('ip_window_hours')} expiry_grace_hours={store.get('expiry_grace_hours')}")
subs = sc.get("subscriptions", [])
print(f"  共 {len(subs)} 条：")
for s in subs:
    print(f"    - {s['label'][:34]:34} enabled={s['enabled']} expired={s['expired']} "
          f"days_left={s['days_left']} ip={s['active_ip_count']}/{s['ip_limit']} "
          f"order={s['order_id'] or '-'}")

print()
print("=" * 78)
print("【任务 / 告警 / 流量】")
_, tasks = call("GET", "/api/tasks")
for t in tasks[:8]:
    print(f"  {t['operation_type']:14} {t['server_name']:16} {t['status']:9} "
          f"{t.get('result_summary') or t.get('error_code') or ''}"[:110])
_, alerts = call("GET", "/api/alerts?include=resolved")
print(f"  告警 {len(alerts)} 条：")
for a in alerts[:10]:
    print(f"    - [{a['severity']}/{a['status']}] {a['type']} {a['server_name'] or a['host']} "
          f"{a['node_name'] or ''}:{a['node_port'] or ''} {a['message'][:50]}")
_, tr = call("GET", "/api/traffic/hourly")
print(f"  24h 流量桶 {len(tr)} 个，总字节 {sum(b['tx_bytes'] + b['rx_bytes'] for b in tr)}")
_, rt = call("GET", "/api/runtime")
print(f"  运行时: {rt}")
