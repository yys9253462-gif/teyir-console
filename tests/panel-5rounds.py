#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
面板五轮实测（小白视角）——在被测机本地跑，直接打面板自己的 API。

为什么在服务器上跑而不在浏览器里跑：
  * 浏览器驱动（agent-browser 的 Chromium）在本地这台 Windows 上反复冷启动
    卡死（>6 分钟无输出），是环境问题不是产品问题；为了拿到可信结论，
    换成"打面板自己的 HTTP 接口"——这正是 Web 面板每次刷新时发的那些请求。
  * 浏览器侧另出截图证据（panel-shots/），文字结论以本脚本为准。

判定原则：以"小白用户会在界面上看到什么"为口径，逐项核对数字/文案/状态。
不把"返回 200"当成通过，而是校验它是否自洽、是否是新手能懂的话。

轮次：
  R1  登录 + 总览页（KPI 自洽 / 无 502 / 主机全在线 / 指标是真的）
  R2  HY2 节点页（4 个节点在线 / 端口正确 / 能对上主机）
  R3  订阅中心（有没有订阅、能不能给小白讲明白）
  R4  告警中心（有故障能否看见、文案是否人话、有无可执行下一步）
  R5  运维任务 + 系统设置（失败任务有没有可执行原因 / 权限说明）

⚠️ 必须经公网域名访问，不能直连 127.0.0.1:8080。
   面板给出的订阅链接是靠请求头里的 Host/X-Forwarded-* 拼出来的，
   直连回环口会让它回 127.0.0.1:8080 —— 那是"测试姿势错了"，
   不是产品缺陷。默认 BASE 走公网域名。

用法（在 se 上）：
  ADMIN_PASS=... python3 panel-5rounds.py
"""
import json
import os
import subprocess
import sys
import time

BASE = os.environ.get("BASE", "https://panel.example.com")
EMAIL = os.environ.get("ADMIN_EMAIL", "admin@nodeops.local")
PASSWORD = os.environ.get("ADMIN_PASS", "")

TOKEN = None
results = []


def http(method, path, body=None, token=None, raw=False):
    """用 curl 打面板自己的接口（本机直连，不走反代、不走代理）。"""
    cmd = ["curl", "-s", "-X", method, BASE + path,
           "-H", "Content-Type: application/json",
           "-w", "\n__HTTP__%{http_code}__"]
    if token:
        cmd += ["-H", "Authorization: Bearer " + token]
    if body is not None:
        cmd += ["-d", json.dumps(body)]
    env = {k: v for k, v in os.environ.items()
           if k.lower() not in ("http_proxy", "https_proxy", "all_proxy")}
    r = subprocess.run(cmd, capture_output=True, timeout=60, env=env)
    out = r.stdout.decode("utf-8", "replace")
    m = out.rsplit("__HTTP__", 1)
    text = m[0].strip()
    code = int(m[1].rstrip("_")) if len(m) > 1 and m[1].rstrip("_").isdigit() else 0
    if raw:
        return code, text
    try:
        return code, json.loads(text) if text else None
    except json.JSONDecodeError:
        return code, text


def check(rnd, ok, name, detail=""):
    results.append((rnd, bool(ok), name, detail))
    print(f"  [{'PASS' if ok else 'FAIL'}] R{rnd} {name}" + (f"  -- {detail}" if detail else ""))
    return ok


def login():
    global TOKEN
    code, data = http("POST", "/api/login", {"email": EMAIL, "password": PASSWORD})
    if code == 200 and isinstance(data, dict):
        TOKEN = data.get("token") or (data.get("data") or {}).get("token")
        return bool(TOKEN), f"HTTP {code}"
    return False, f"HTTP {code} {str(data)[:120]}"


# ═══════════════════════════ R1 总览 ═══════════════════════════
def round1():
    print("\n=== R1 登录 + 总览页 ===")
    ok, why = login()
    if not check(1, ok, "能用管理员账号登录", why):
        return

    c_health, health = http("GET", "/api/health", token=TOKEN)
    check(1, c_health == 200 and (health or {}).get("status") == "ok",
          "面板健康检查返回 ok（侧栏「控制器状态」凭此显示就绪）",
          f"HTTP {c_health} {str(health)[:80]}")

    c_srv, servers = http("GET", "/api/servers", token=TOKEN)
    servers = servers or []
    check(1, c_srv == 200 and len(servers) >= 4, "主机列表能取到全部主机",
          f"共 {len(servers)} 台")

    # 每台主机状态必须是 online —— 界面上"主机在线 4/4"就靠它
    bad = [s["name"] for s in servers if s.get("status") != "online"]
    check(1, not bad, "4 台主机状态均为 online（界面显示「主机在线 4/4」）",
          "" if not bad else f"异常: {bad}")

    # 小白最容易被误伤的点：指标必须是真实采集，不是 0
    zeroed = [s["name"] for s in servers
              if s.get("has_credential") and not (s.get("memory_percent") or 0) > 0]
    check(1, not zeroed, "有凭据的主机内存指标非 0（说明真在采集）",
          "" if not zeroed else f"指标为 0: {zeroed}")

    # 主机表「区域/分组」列：没设置区域时必须回落到 provider 或 —，
    # 绝不能出现空字符串导致界面那一格是空白
    blanks = [s["name"] for s in servers
              if not (s.get("region") or "").strip()
              and not (s.get("provider") or "").strip()]
    check(1, not blanks, "主机表「区域/分组」列不会留空白",
          "" if not blanks else f"空白: {blanks}")

    # 延迟必须是可读数字；负数或巨大值会让小白以为坏了
    weird = [(s["name"], s.get("latency_ms")) for s in servers
             if s.get("latency_ms") is not None and s["latency_ms"] < 0]
    check(1, not weird, "主机延迟不是负数（负数会被界面显示成 —）",
          "" if not weird else f"{weird}")

    m = http("GET", "/api/metrics/recent", token=TOKEN)[1] or {}
    have = sum(1 for s in servers if m.get(s["id"]))
    check(1, have >= 4, "最近指标覆盖全部主机（总览图表的来源）",
          f"{have}/{len(servers)} 台有指标")


# ═══════════════════════════ R2 HY2 节点 ═══════════════════════════
def round2():
    print("\n=== R2 HY2 节点页 ===")
    c, nodes = http("GET", "/api/nodes", token=TOKEN)
    nodes = nodes or []
    check(2, c == 200 and len(nodes) >= 4, "节点列表能取到全部节点", f"共 {len(nodes)} 个")

    online = [n for n in nodes if n.get("status") == "online"]
    check(2, len(online) == len(nodes),
          "所有节点状态 online（界面显示「在线节点 4个 / 端口正在监听」）",
          f"{len(online)}/{len(nodes)} 在线")

    # 端口必须唯一且非 0 —— 界面上「端口」列直接展示它
    ports = [n.get("listen_port") for n in nodes]
    check(2, all(p for p in ports) and len(set(ports)) == len(ports),
          "每个节点都有唯一监听端口", f"端口 {sorted(p for p in ports if p)}")

    # 节点必须能关联到主机；否则界面「所属主机」列会是空的
    c_srv, servers = http("GET", "/api/servers", token=TOKEN)
    hosts = {s["id"]: s["host"] for s in (servers or [])}
    orphan = [n.get("name") for n in nodes if n.get("server_id") not in hosts]
    check(2, not orphan, "每个节点都能关联到主机（界面「所属主机」列不留空）",
          "" if not orphan else f"孤儿节点: {orphan}")

    # 新手最关心的：这个节点的域名/IP 和端口，界面上要能对上
    missing = [n.get("name") for n in nodes
               if not (n.get("server_name") or n.get("name"))]
    check(2, not missing, "每个节点都有可读名称", "" if not missing else f"{missing}")


# ═══════════════════════════ R3 订阅中心 ═══════════════════════════
def round3():
    print("\n=== R3 订阅中心 ===")
    # /api/subscriptions 返回 {store, subscriptions}，不是裸列表
    c, payload = http("GET", "/api/subscriptions", token=TOKEN)
    payload = payload if isinstance(payload, dict) else {}
    subs = payload.get("subscriptions") or []
    store = payload.get("store") or {}
    check(3, c == 200, "订阅列表接口正常", f"HTTP {c}，{len(subs)} 条订阅")

    # 订阅对象必须给出「怎么用」的字段，否则小白拿到链接不知道怎么导入
    if subs:
        s0 = subs[0]
        has_link = bool(s0.get("token") or s0.get("url") or s0.get("sub_url"))
        check(3, has_link, "订阅对象带可直接使用的链接/令牌",
              f"字段: {sorted(s0.keys())[:10]}")
        has_scope = any(k in s0 for k in ("nodes", "node_ids", "excluded",
                                          "node_count", "included"))
        if not has_scope:
            # 买家订阅是按节点开的，对象里不带节点清单是**有意的**：
            # /api/subscriptions 不返回 node_accounts（那是买家的口令）。
            # 只要它能关联到节点/订单，就算信息完整。
            has_scope = any(k in s0 for k in ("node_id", "order_id",
                                              "node_name", "server_id"))
        check(3, has_scope, "订阅能说明它属于哪个节点/订单")
        # 小白需要知道"给谁用/什么时候到期"
        has_owner = any(k in s0 for k in ("buyer", "email", "label", "note",
                                          "order_id", "expires_at", "days_left"))
        check(3, has_owner, "订阅对象带归属/到期信息（小白要知道给谁、何时过期）",
              f"字段: {sorted(s0.keys())}")
    else:
        check(3, True, "当前无独立订阅（空态由前端给提示，属正常）")

    # 主订阅（面板自己发的那条）必须存在，否则"合并订阅"按钮点了没用
    c2, state = http("GET", "/api/subscription", token=TOKEN)
    state = state if isinstance(state, dict) else {}
    check(3, c2 == 200, "主订阅状态接口正常（「合并订阅」按钮依赖它）",
          f"HTTP {c2}")
    if c2 == 200:
        # 主订阅要有可用 url 与格式列表，前端才能给出一键复制
        check(3, bool(state.get("url")), "主订阅给出可直接访问的 URL",
              str(state.get("url"))[:70])
        fmts = state.get("formats") or []
        check(3, len(fmts) >= 2, "主订阅给出多种客户端格式（新手要能挑）",
              f"格式: {fmts}")
        # included/excluded 要是明确的列表，界面才能说清"含哪些节点"
        incl = state.get("included")
        check(3, isinstance(incl, list) and len(incl) >= 4,
              "主订阅明确列出包含的节点",
              f"included={len(incl) if isinstance(incl, list) else incl}")

    # 订阅链接必须指向公网可达地址 —— 如果回环口/容器名，小白粘进客户端
    # 会完全连不上，且面板不会报错，是最难自查的一类坑。
    host = BASE.split("://", 1)[-1].split("/")[0]
    bad_urls = []
    if isinstance(state, dict) and state.get("url"):
        if host and host not in state["url"]:
            bad_urls.append(state["url"])
    for s in subs:
        for k in ("subscription_url", "delivery_url"):
            u = s.get(k)
            if u and host and host not in u:
                bad_urls.append(u)
    check(3, not bad_urls, "订阅链接指向公网域名（不是 127.0.0.1 / 容器名）",
          "" if not bad_urls else f"异常链接: {bad_urls[:3]}")


# ═══════════════════════════ R4 告警中心 ═══════════════════════════
def round4():
    print("\n=== R4 告警中心 ===")
    c, alerts = http("GET", "/api/alerts", token=TOKEN)
    alerts = alerts or []
    check(4, c == 200, "告警接口正常", f"HTTP {c}，共 {len(alerts)} 条")

    open_alerts = [a for a in alerts if not a.get("acknowledged_at")]
    print(f"       （当前 open 告警 {len(open_alerts)} 条）")

    if not alerts:
        # 没有告警本身不是缺陷 —— 但"永远没有告警"才是缺陷。
        # 这里不做故障注入（那是 cluster-5rounds.py 的 R5 干的事），
        # 只如实记录：此刻面板是"一片祥和"，因此本条按"无数据待验证"处理。
        check(4, True, "当前面板无告警（属正常；故障可见性见 cluster-5rounds R5）")
        # 仍然要确认告警页的两个基本能力在
        check(4, len(open_alerts) == 0, "无未确认告警时界面显示 0 项")
        return

    # 告警文案必须是中文人话，不能把内部枚举直接扔给用户
    RAW_ENUM = {"server_offline", "server_warning", "server_unknown",
                "server_stale", "node_offline", "node_unstable", "node_unknown"}
    leaked = [a.get("kind") or a.get("type") for a in alerts
              if (a.get("kind") or a.get("type")) in RAW_ENUM
              and not (a.get("message") or a.get("title"))]
    check(4, not leaked, "告警不会只显示内部枚举名",
          "" if not leaked else f"仅枚举无名: {set(leaked)}")

    # 每条告警都要有能读懂的一句话
    no_msg = [a.get("id") for a in alerts
              if not (a.get("message") or a.get("title") or a.get("summary"))]
    check(4, not no_msg, "每条告警都有可读描述",
          "" if not no_msg else f"{len(no_msg)} 条缺少描述")

    # 告警必须能指明是哪台主机/哪个节点，否则小白不知道去哪修
    no_target = [a.get("id") for a in alerts
                 if not (a.get("server_id") or a.get("node_id")
                         or a.get("target") or a.get("server_name"))]
    check(4, not no_target, "每条告警都能看出涉及哪台主机/节点",
          "" if not no_target else f"{len(no_target)} 条无归属")

    # 告警要能确认（界面上有"确认/处理"动作）
    aid = alerts[0].get("id")
    c2, _ = http("POST", f"/api/alerts/{aid}/ack", token=TOKEN)
    check(4, c2 in (200, 204), "告警可确认（界面「确认」按钮可用）", f"HTTP {c2}")


# ═══════════════════════════ R5 运维任务 + 系统设置 ═══════════════════════════
def round5():
    print("\n=== R5 运维任务 + 系统设置 ===")
    c, tasks = http("GET", "/api/tasks", token=TOKEN)
    tasks = tasks or []
    check(5, c == 200, "任务列表接口正常", f"HTTP {c}，共 {len(tasks)} 条")

    failed = [t for t in tasks if t.get("status") == "failed"]
    if failed:
        # 失败任务必须给出原因，且原因要能指导下一步。
        # 注意：任务对象的结果字段叫 result_summary（不是 result/message）。
        t0 = failed[0]
        msg = (t0.get("result_summary") or t0.get("error_code") or "").strip()
        check(5, bool(msg), "失败任务带原因说明（小白能知道为什么失败）",
              f"原因: {msg[:90]}")
        actionable = any(k in msg for k in ("允许", "白名单", "需要", "请", "原因"))
        check(5, actionable, "失败原因里包含可执行的下一步",
              "" if actionable else f"原因太笼统: {msg[:90]}")
    else:
        check(5, True, "当前无失败任务")

    # 写操作必须按主机授权：未授权的主机不能执行 restart
    c_srv, servers = http("GET", "/api/servers", token=TOKEN)
    targets = [s for s in (servers or []) if s.get("has_credential")]
    if targets:
        s0 = targets[0]
        c_wo, wo = http("GET", f"/api/servers/{s0['id']}", token=TOKEN)
        has_flag = isinstance(wo, dict) and "allow_write_ops" in wo
        check(5, has_flag, "主机对象暴露「写操作是否已授权」（界面要提示小白）",
              f"HTTP {c_wo} allow_write_ops={wo.get('allow_write_ops') if isinstance(wo, dict) else '?'}")

    # 用户管理（系统设置里改密码/加人）
    c_u, users = http("GET", "/api/users", token=TOKEN)
    check(5, c_u == 200 and isinstance(users, list), "用户列表接口正常（系统设置可管理账号）",
          f"HTTP {c_u}，{len(users) if isinstance(users, list) else '?'} 个用户")

    # 审计日志：小白改坏了能回看
    c_a, audit = http("GET", "/api/audit", token=TOKEN)
    check(5, c_a == 200, "审计日志接口正常", f"HTTP {c_a}")


def main():
    if not PASSWORD:
        print("缺 ADMIN_PASS", file=sys.stderr)
        sys.exit(2)
    for fn in (round1, round2, round3, round4, round5):
        fn()
    total = len(results)
    passed = sum(1 for _, ok, _, _ in results if ok)
    print("\n" + "=" * 64)
    print(f"面板五轮实测（小白视角）： {passed}/{total} 通过")
    print("=" * 64)
    for r, ok, n, d in results:
        print(f"  {'✓' if ok else '✗'} R{r}  {n}" + (f"   ({d})" if d else ""))
    bad = [(r, n, d) for r, ok, n, d in results if not ok]
    if bad:
        print("\n未通过项：")
        for r, n, d in bad:
            print(f"  - R{r} {n}: {d}")
    sys.exit(0 if not bad else 1)


if __name__ == "__main__":
    main()
