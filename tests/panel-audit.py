#!/usr/bin/env python3
"""面板功能体检：登录 -> 逐个打真实接口 -> 记录真实响应。

只读接口：真打，看 200 且结构对不对。
写接口：一律发「必定非法」的载荷，只验证路由存在且校验生效（期待 400/422），
        绝不能因为体检而在节点上开户 / 轮换商城密钥 / 删东西。
有副作用的（rotate / store-key / 所有 DELETE）一律不打，只标注「未测」。
"""
import json
import sys
import urllib.error
import urllib.request

BASE = "https://panel.example.com"
EMAIL = "admin@nodeops.local"
PASSWORD = sys.argv[1] if len(sys.argv) > 1 else ""

TOKEN = ""


def call(method, path, body=None, headers=None, raw=False):
    req = urllib.request.Request(
        BASE + path,
        method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={
            "Authorization": f"Bearer {TOKEN}",
            "Content-Type": "application/json",
            **(headers or {}),
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            text = r.read().decode()
            return r.status, (text if raw else (json.loads(text) if text.strip() else None))
    except urllib.error.HTTPError as e:
        text = e.read().decode()
        try:
            return e.code, (text if raw else json.loads(text))
        except Exception:
            return e.code, text
    except Exception as e:
        return 0, str(e)


results = []


def check(label, method, path, expect, body=None, headers=None, raw=False, note=""):
    st, data = call(method, path, body, headers, raw)
    ok = st in expect
    detail = ""
    if isinstance(data, dict) and "error" in data:
        detail = str(data["error"])[:90]
    elif isinstance(data, str):
        detail = data[:90].replace("\n", " ")
    results.append((ok, label, f"{method} {path}", st, detail, note))


# ---------------------------------------------------------------- 登录
st, body = call("POST", "/api/login", {"email": EMAIL, "password": PASSWORD})
if st != 200 or not isinstance(body, dict) or "token" not in body:
    print(f"登录失败: {st} {body}")
    sys.exit(1)
TOKEN = body["token"]
print(f"登录成功 role={body.get('role')}\n")

# ---------------------------------------------------------------- 基线
st, servers = call("GET", "/api/servers")
st2, nodes = call("GET", "/api/nodes")
st3, subs = call("GET", "/api/subscriptions")
st4, tasks = call("GET", "/api/tasks")
print(f"主机 {len(servers) if isinstance(servers, list) else '?'} 台 / "
      f"节点 {len(nodes) if isinstance(nodes, list) else '?'} 个 / "
      f"订阅 {len(subs.get('subscriptions', [])) if isinstance(subs, dict) else '?'} 条 / "
      f"任务 {len(tasks) if isinstance(tasks, list) else '?'} 条\n")

# ---------------------------------------------------------------- 只读接口
check("健康检查", "GET", "/api/health", {200})
check("主机列表", "GET", "/api/servers", {200})
check("运行时状态", "GET", "/api/runtime", {200})
check("最近指标(总览 sparkline)", "GET", "/api/metrics/recent", {200})
check("节点列表", "GET", "/api/nodes", {200})
check("合并订阅状态", "GET", "/api/subscription", {200})
check("订阅中心", "GET", "/api/subscriptions", {200})
check("任务列表", "GET", "/api/tasks", {200})
check("告警列表", "GET", "/api/alerts", {200})
check("告警列表(含已解决)", "GET", "/api/alerts?include=resolved", {200})
check("24h 流量分桶", "GET", "/api/traffic/hourly", {200})
check("审计日志", "GET", "/api/audit?limit=50", {200})
check("用户列表", "GET", "/api/users", {200})

for f in ("clash", "singbox", "uri"):
    check(f"订阅预览 {f}", "GET", f"/api/subscription/preview?format={f}", {200}, raw=True)

# 依赖具体 id 的
if isinstance(servers, list) and servers:
    sid = servers[0]["id"]
    check("主机详情", "GET", f"/api/servers/{sid}", {200})
    check("主机指标时序", "GET", f"/api/servers/{sid}/metrics", {200})
    check("主机 SSH 凭据元信息", "GET", f"/api/servers/{sid}/credential", {200})
    check("主机服务状态(systemd)", "GET", f"/api/servers/{sid}/services", {200})
else:
    results.append((None, "主机相关接口", "-", 0, "没有主机，跳过", ""))

if isinstance(nodes, list) and nodes:
    nid = nodes[0]["id"]
    check("节点总览(proxy)", "GET", f"/api/nodes/{nid}/overview", {200})
    check("节点用户列表", "GET", f"/api/nodes/{nid}/users", {200})
    check("节点代理服务列表", "GET", f"/api/nodes/{nid}/proxy-services", {200})
    check("节点 API 刷新", "GET", f"/api/nodes/{nid}/api", {200})
else:
    results.append((None, "节点相关接口", "-", 0, "没有节点，跳过", ""))

if isinstance(tasks, list) and tasks:
    check("任务日志", "GET", f"/api/tasks/{tasks[0]['id']}/logs", {200})
else:
    results.append((None, "任务日志", "-", 0, "没有任务，跳过", ""))

# ---------------------------------------------------------------- 写接口（非法载荷探测路由+校验）
check("创建主机(空体应400)", "POST", "/api/servers", {400}, {})
check("创建节点(空体应400)", "POST", "/api/nodes", {400}, {})
check("节点探测(空体应400)", "POST", "/api/nodes/preview", {400}, {"url": "", "api_key": ""})
check("创建订阅(越界天数应400)", "POST", "/api/subscriptions", {400}, {"duration_days": 99999})
check("创建用户(空体应400)", "POST", "/api/users", {400}, {})
check("改密码(空体应400)", "POST", "/api/auth/change-password", {400}, {})
check("建任务(非法操作应400/422)", "POST", "/api/tasks", {400, 422},
      {"server_id": "00000000-0000-0000-0000-000000000000", "operation_type": "nope"})

if isinstance(servers, list) and servers:
    sid = servers[0]["id"]
    check("上传凭据(空体应400)", "POST", f"/api/servers/{sid}/credential", {400}, {"auth_type": "zzz", "secret": ""})
    check("写操作白名单(空体应400)", "POST", f"/api/servers/{sid}/write-ops", {400},
          {"allow": True, "units": []}, {"X-Confirm-Dangerous": "yes"})
    check("更新主机(非法端口应400)", "PATCH", f"/api/servers/{sid}", {400}, {"ssh_port": 0})
    check("更新节点(越界端口应400)", "PATCH", f"/api/nodes/{nodes[0]['id'] if nodes else 'x'}", {400},
          {"listen_port": 99999})

if isinstance(subs, dict) and subs.get("subscriptions"):
    sid0 = subs["subscriptions"][0]["id"]
    check("更新订阅(越界 ip_limit 应400)", "PATCH", f"/api/subscriptions/{sid0}", {400}, {"ip_limit": 99999})

# ---------------------------------------------------------------- 未测（有副作用）
for label, m, p in [
    ("轮换合并订阅链接", "POST", "/api/subscription/rotate"),
    ("轮换商城密钥", "POST", "/api/subscriptions/store-key"),
    ("删除订阅", "DELETE", "/api/subscriptions/{id}"),
    ("轮换单条订阅", "POST", "/api/subscriptions/{id}/rotate"),
    ("删除主机/节点/凭据/用户", "DELETE", "/api/servers|nodes|users/{id}"),
    ("节点开户/续期/删户", "POST", "/api/nodes/{id}/users"),
]:
    results.append((None, label, f"{m} {p}", 0, "有副作用，体检不执行", ""))

# ---------------------------------------------------------------- 输出
print(f"{'':2} {'检查项':34} {'请求':52} {'状态':>4}  说明")
print("-" * 120)
fails = []
for ok, label, req, st, detail, note in results:
    mark = "OK" if ok else ("--" if ok is None else "XX")
    print(f"{mark:2} {label:34} {req:52} {st:>4}  {detail}{(' | ' + note) if note else ''}")
    if ok is False:
        fails.append((label, req, st, detail))

print("-" * 120)
bad = [r for r in results if r[0] is False]
print(f"通过 {sum(1 for r in results if r[0])} / 未测 {sum(1 for r in results if r[0] is None)} / 失败 {len(bad)}")
for label, req, st, detail in bad:
    print(f"  失败: {label}  {req}  -> {st} {detail}")
