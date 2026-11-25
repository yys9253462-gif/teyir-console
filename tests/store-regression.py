#!/usr/bin/env python3
"""
商业层全量回归 —— 五轮。

每轮都从零建自己的订单数据，独立断言，互不影响；轮次之间只共享一个
商城对接密钥（由 --store-key 传入），不共享任何订阅。

覆盖：
  1. 下单          —— 必填校验、字段回显、四种 URL
  2. 幂等          —— 同 order_id 重放必须返回同一 token，且不新增行
  3. 密钥分离      —— 公开视图零口令；/sub/<token> 才带 password
  4. IP 限额       —— 前 N 个 IP 放行，第 N+1 个 403；已计过数的 IP 重访仍放行
  5. 到期          —— 已到期订阅：视图 200 且 expired=true 且无 URL；/sub 403 expired
                      （此态只存在于 auto_delete=false；缺省 true 时到期即回收，见下）
  6. 到期宽限与回收 —— 宽限期为 0：到期即刻回收。auto_delete=true（缺省）→ 视图与
                      /sub 同为 404，与「token 从未存在」不可区分；auto_delete=false
                      → 保留，视图 200 / /sub 403 expired，客户还有续费出路
  7. 续费          —— 未到期从原到期日往后接；已到期从当下重新起算
  8. 撤销          —— 默认停用（可续费恢复）；delete=true 才是真删
  9. 查询          —— 按 order_id 查回状态与剩余天数
 10. auto_delete   —— 显式 false 被尊重；缺省为 true

用法:
  python store-regression.py --base https://panel.example.com \
      --store-key shop_xxx [--rounds 5] [--only 3]
"""
import argparse
import json
import os
import subprocess as sp
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone

# ⚠️ 2026-10-03 重构：从 tests/_http.py 复用 HttpClient / Result / Round / Case / 报表。
# 之前 store-regression.py / store-e2e.py 各自重写一遍 Client / Round —— 重复 200+ 行，
# 抽到 tests/_http.py 公共 module。
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _http import HttpClient, Round, Case, report_to_json, print_round_summary, Client, Result  # noqa: E402
from _parse import status_dist as _status_dist  # noqa: E402

# ---------------------------------------------------------------- 配置

SSH_PREFIX = [sys.executable, ""]        # 占位，main() 里替换为真实脚本路径
ssh_alias = "panel-host"

# 服务器内部直连 api 用的端口。
#
# 为什么不直接用 --base 的端口：这两个端口**在真实部署里经常不是同一个**。
# --base 是「测试机怎么访问面板」（可能经隧道、可能经外层反代），
# 而 ip-probe 要的是「在服务器上直连 api 容器映射出来的那个端口」。
#
# 实测踩过：把 --base 设成 http://127.0.0.1:18080（隧道到 web），
# 推导出的探测端口也是 18080 —— 而服务器上 18080 恰好被另一个容器
# （ghost，Express）占着，它把 /sub/x 301 到 /sub/x/。
# 于是五条 IP 限额断言全部拿到 got=301，看上去像限额逻辑坏了，
# 实际是探测打到了无关服务上。
#
# 所以：能用 --probe-port 显式指定就不要推导；不指定时才从 --base 推导。
probe_port = 18080


def ssh_probe_available():
    """内部探测能不能做。

    两种情况都算可用：
      * 给了 ssh 别名 —— 从别的机器 ssh 过去探测；
      * 别名为空但脚本**就在目标机上跑** —— 那时不需要 ssh 跳板，
        ip-probe.py 会用本机 bash 直接 curl 127.0.0.1。

    早先这里只认第一种，于是在目标机上跑（--ssh-alias ""）会白白跳过
    五条 IP 限额断言，报一句"缺少 --ssh-alias"，看上去像环境不满足，
    其实是脚本没考虑到"本机执行"这种用法。
    """
    return SSH_PREFIX[1] != ""


def port_from_base(base):
    """从 --base 里取出端口，作为 --probe-port 缺省时的兜底。

    之所以要「内部直连」：服务端只认 X-Forwarded-For 的最后一跳（防伪造），
    经 Caddy 打进去时最后一跳永远是 Caddy 追加的真实地址，无论怎么伪造
    XFF 都会被算成同一个 IP —— 那样验证的是 Caddy 的转发行为，不是限额逻辑。

    解析失败的兜底是 18080 而不是 8080：宁可维持旧行为（至少和 --port 的
    默认值一致），也不要在 base 写错时静默换个端口继续跑。
    """
    try:
        p = urllib.parse.urlsplit(base).port
        return p or (443 if urllib.parse.urlsplit(base).scheme == "https" else 80)
    except Exception:                                        # noqa: BLE001
        return 18080

# ---------------------------------------------------------------- 断言框架

# Case / Round 已抽到 tests/_http.py（dataclass 版）—— 本文件用 _http 里的版本。
# 这里不再保留旧版（重复定义会遮蔽前一次的 import）。

# ---------------------------------------------------------------- HTTP
# Client 已抽到 tests/_http.py：HttpClient.raw 返 Result dataclass；
# raw_tuple 返 (status, body, text) 三元组 —— 后者保留是为了 store-regression
# 里成百处 `st, body, _ = cli.raw(...)` 3-tuple unpack 调用不需要改。
#
# 这里只是给 store-regression 的内部调用一个兼容层：
#   cli.raw(method, path, body=, headers=, xff=) → (status, body, text)
# 这样老断言代码（st, view, _ = cli.raw(...)、st, _, txt = cli.raw(...)）一行不动。
#
# 老 Client / Case / Round 现在都是 tests/_http.py 的 alias（或只是注释保留）。

class Client(HttpClient):
    """store-regression.py 的本地 Client：raw 返 3-tuple，原始签名不动。

    新代码请直接用 HttpClient + Result dataclass（命名字段）。
    """

    def raw(self, method, path, body=None, headers=None, xff=None):
        # ⚠️ 直接调 HttpClient.raw_tuple —— 不能 super().raw_tuple()，
        # 否则走 Client.raw 自身造成无限递归（子类同名方法 self.raw 仍调子类）。
        return HttpClient.raw_tuple(self, method, path, body, headers, xff)


# ---------------------------------------------------------------- 工具

def iso(ts):
    return datetime.fromtimestamp(ts, tz=timezone.utc).isoformat()


def store_headers(key, xff=None):
    h = {"X-Store-Key": key}
    if xff:
        h["X-Forwarded-For"] = xff
    return h


def make_order_id(round_index, tag):
    """订单号必须全局唯一，否则跨轮会命中幂等分支。"""
    return f"REG-R{round_index}-{tag}-{int(time.time())}-{round_index}"


# ---------------------------------------------------------------- 单轮

def run_round(cli, store_key, idx):
    r = Round(idx)
    tag = f"{int(time.time())}{idx}"
    now = int(time.time())
    # 每轮换一段独立网段（RFC 5737 文档用地址），避免跨轮撞上 24h 滑动窗口
    net = f"198.51.{100 + idx}"

    # ---------- 1) 下单：必填与校验 ----------
    # 这些负例都要带合法密钥，否则会先被鉴权拦成 401，测不到参数校验。
    st, _, _ = cli.raw("POST", "/api/store/orders", {}, store_headers(store_key))
    r.eq("下单：缺 order_id 被拒 400", st, 400)

    st, _, _ = cli.raw("POST", "/api/store/orders", {"order_id": tag + "-bad"},
                       store_headers(store_key))
    r.eq("下单：既无时长又无到期时间被拒 400", st, 400)

    st, _, _ = cli.raw("POST", "/api/store/orders",
                       {"order_id": tag + "-bad2", "duration_days": 99999},
                       store_headers(store_key))
    r.eq("下单：duration_days 超上限被拒 400", st, 400)

    st, _, _ = cli.raw("POST", "/api/store/orders",
                       {"order_id": tag + "-bad3", "duration_days": 30, "ip_limit": 99999},
                       store_headers(store_key))
    r.eq("下单：ip_limit 超上限被拒 400", st, 400)

    # 无密钥必须 401
    st, _, _ = cli.raw("POST", "/api/store/orders", {"order_id": tag + "-noauth", "duration_days": 30})
    r.eq("下单：无商城密钥被拒 401", st, 401)

    # ---------- 2) 正式下单 ----------
    order_main = make_order_id(idx, "MAIN")
    st, body, txt = cli.raw("POST", "/api/store/orders", {
        "order_id": order_main,
        "label": f"回归-{idx}-主单",
        "duration_days": 30,
        "ip_limit": 2,
        "user_email": f"reg{idx}@example.com",
        "note": "五轮回归",
    }, store_headers(store_key))
    r.eq("下单：正常下单返回 200", st, 200)
    if st != 200:
        return r
    token = body.get("token")
    r.check("下单：返回了 token 且长度 >= 20", isinstance(token, str) and len(token) >= 20,
            f"token={token!r}")
    r.eq("下单：ip_limit 回显为 2", body.get("ip_limit"), 2)
    r.eq("下单：缺省 auto_delete 回显为 True", body.get("auto_delete"), True)
    r.eq("下单：order_id 回显一致", body.get("order_id"), order_main)
    for field in ("subscription_url", "clash_url", "singbox_url", "uri_url", "delivery_url"):
        r.check(f"下单：给出 {field}", bool(body.get(field)), f"{body.get(field)!r}")

    # ---------- 3) 幂等 ----------
    st2, body2, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order_main, "duration_days": 30, "ip_limit": 2,
    }, store_headers(store_key))
    r.eq("幂等：重放返回 200", st2, 200)
    r.eq("幂等：重放拿到同一个 token", body2.get("token"), token)

    # 把一个不同时长塞进同一个 order_id，不应改掉已开的订阅
    st3, body3, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order_main, "duration_days": 365, "ip_limit": 99,
    }, store_headers(store_key))
    r.eq("幂等：改参数重放仍返回原 token", body3.get("token"), token)
    r.eq("幂等：改参数重放不改动 ip_limit", body3.get("ip_limit"), 2)

    # ---------- 4) auto_delete 显式 false ----------
    order_keep = make_order_id(idx, "KEEP")
    st, body_keep, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order_keep, "label": f"回归-{idx}-保留单",
        "duration_days": 30, "ip_limit": 2, "auto_delete": False,
    }, store_headers(store_key))
    r.eq("auto_delete：显式 false 下单 200", st, 200)
    r.eq("auto_delete：显式 false 被回显为 False", body_keep.get("auto_delete"), False)
    token_keep = body_keep.get("token")

    # ---------- 5) 密钥分离 ----------
    st, view, _ = cli.raw("GET", f"/api/sub/view/{token}")
    r.eq("公开视图：返回 200（无需鉴权）", st, 200)
    if st == 200:
        flat = json.dumps(view, ensure_ascii=False)
        r.check("公开视图：不含 password 字段名", "password" not in flat, flat[:200])
        r.check("公开视图：不含 obfs-password", "obfs-password" not in flat)
        r.check("公开视图：不含密钥字样 'secret'", "secret" not in flat.lower())
        r.eq("公开视图：ip_limit 正确", view.get("ip_limit"), 2)
        r.eq("公开视图：expired 为 False", view.get("expired"), False)
        r.check("公开视图：days_left >= 28", (view.get("days_left") or 0) >= 28,
                f"days_left={view.get('days_left')}")
        r.check("公开视图：renew_url 指向商城",
                "shop.example.com" in str(view.get("renew_url")), str(view.get("renew_url")))

    st, _, feed = cli.raw("GET", f"/sub/{token}")
    r.eq("订阅源：/sub/<token> 返回 200", st, 200)
    r.check("订阅源：确实带了节点口令（与公开视图相反）",
            "password" in feed or "obfs-password" in feed or "hysteria2" in feed,
            feed[:120])

    # ---------- 6) IP 限额 ----------
    # 必须绕开 Caddy：Caddy 会把真实客户端地址追加/覆盖到 X-Forwarded-For 末尾，
    # 而服务端只认最后一跳（这正是防伪造的设计）。从外部打公网入口时，无论怎么
    # 设置 XFF 都会被算成同一个 IP —— 那样验证的是 Caddy，不是限额。
    # 改为在服务器内部直连 api 端口，XFF 原样送达，才能真正区分客户端。
    #
    # 并且必须另开一条干净订阅：上面验证 /sub 是否带口令时已经从公网拉过一次，
    # 那次会把我本机真实出口 IP 记进配额；沿用同一条订阅的话，第一段限额断言
    # 就只剩一个空位，第二个 IP 会被直接拒成 403。
    order_ip = make_order_id(idx, "IPLIM")
    st, body_ip, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order_ip, "label": f"回归-{idx}-限额专用",
        "duration_days": 30, "ip_limit": 2,
    }, store_headers(store_key))
    r.eq("IP限额：另开一条干净订阅 200", st, 200)
    token_ip = body_ip.get("token")
    if not ssh_probe_available():
        r.check("IP限额：需要在服务器内部探测（当前环境不可用）", False,
                "缺少 --ssh-alias，无法绕开 Caddy 直连 api 端口")
    elif token_ip:
        # 探测走的是 `ssh <alias>`，偶发失败（ssh 连接抖动、远端瞬时不可达）
        # 会以 `{"error": ""}` + 退出码 1 的形式返回，且 stderr 是空的 ——
        # 看不出原因。这种情况重试一次通常就好，而一次 ssh 抖动不该让
        # 五条 IP 限额断言全报失败（那会把环境问题伪装成产品缺陷）。
        #
        # 只在「压根没解析出结果」时重试；一旦拿到 first 键就认定这次有效，
        # 断言该失败就失败，不能靠重试把真实缺陷刷绿。
        probe = None
        for attempt in range(3):
            ipres = sp.run([
                SSH_PREFIX[0], SSH_PREFIX[1],
                "--alias", ssh_alias,
                "--token", token_ip,
                "--ips", f"{net}.1,{net}.2,{net}.3",
                "--revisit", f"{net}.1",
                # 端口必须从 --base 推导后显式传下去。
                #
                # ip-probe.py 的 --port 默认值是 18080，那是历史部署的习惯值
                # （web 反代到宿主 127.0.0.1:18080）。而 API_PORT 现在是可配的，
                # 默认 8080 —— 一旦不是 18080，探测会打到没人监听的端口上，
                # 四条断言全挂，且报错是 KeyError: 'first'（probe 里是个 error
                # 字典），看上去像脚本坏了，其实只是端口没接上。
                "--port", str(probe_port),
            ], capture_output=True, text=True, timeout=180, check=False)
            try:
                probe = json.loads(ipres.stdout.strip().splitlines()[-1])
            except Exception:                                    # noqa: BLE001
                probe = {"error": (ipres.stdout + ipres.stderr)[:300]}
            if "first" in probe:
                break
            if attempt < 2:
                sys.stderr.write(
                    f"  [ip-probe 第 {attempt + 1} 次无结果，重试] "
                    f"rc={ipres.returncode} stdout={ipres.stdout!r} "
                    f"stderr={ipres.stderr!r}\n")
                time.sleep(1.0)
        r.check("IP限额：服务器内部探测成功",
                not probe.get("error"), str(probe.get("error")))
        if "first" not in probe:
            # 三次都没结果时留下现场。否则界面上只有五行 "got=None"，
            # 完全看不出探测收到的是什么。
            sys.stderr.write(f"  [ip-probe 最终 probe={probe!r}]\n")
            sys.stderr.write(f"  [ip-probe rc={ipres.returncode}]\n")
        if not probe.get("error"):
            # 用 .get() 而不是下标：探测失败时 probe 里只有 error 键，
            # 直接下标会抛 KeyError 打断整轮，前面已通过的断言一条都看不到。
            r.eq("IP限额：第 1 个 IP 放行 200", probe.get("first"), 200)
            r.eq("IP限额：第 2 个 IP 放行 200", probe.get("second"), 200)
            r.eq("IP限额：第 3 个 IP 被拒 403", probe.get("third"), 403)
            r.eq("IP限额：已计过数的 IP 重访仍放行 200", probe.get("revisit"), 200)
            r.eq("IP限额：公开视图 active_ip_count = 2", probe.get("active_ip_count"), 2)

    # ---------- 7) 已到期但保留的订阅（auto_delete=false）----------
    #
    # 这里必须显式传 auto_delete=false，原因见下。
    #
    # 设计上宽限期是 **0**：subscriptionExpiryGrace = 0，而 auto_delete 默认
    # true，所以「到期」与「回收」是同一时刻的两件事 —— 第一次读过期的订阅
    # 就把它删了，视图和 /sub 一起 404。
    #
    # 因此「已到期但仍能打开、返回 403 已到期」这个中间态，只有
    # auto_delete=false 的订阅才存在（运营想给客户留一条续费出路时用）。
    # 这段原本写的是「过期 2 天、7 天宽限内」，那是更早一版 7 天宽限设计的
    # 遗留：宽限既然为 0，那条订阅会被立刻回收，视图返回 404 而不是 200，
    # 断言必然失败 —— 失败的是这条测试，不是产品。
    # 交付给客户的真实行为由第 9 段（宽限外 auto_delete=true → 404）覆盖。
    order_exp = make_order_id(idx, "EXP")
    past_grace_inside = now - 2 * 86400      # 过期 2 天
    st, body_exp, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order_exp, "label": f"回归-{idx}-已到期保留",
        "expires_at": past_grace_inside, "ip_limit": 0,
        "auto_delete": False,            # 关键：保留，才能观察「到期未回收」态
    }, store_headers(store_key))
    r.eq("到期：构造已到期但保留的订阅 200", st, 200)
    token_exp = body_exp.get("token")
    r.eq("到期：下单响应里 expired 应为 True", body_exp.get("expired"), True)
    r.check("到期：下单响应不给 subscription_url（不改回收也一样）",
            not body_exp.get("subscription_url"), str(body_exp.get("subscription_url")))
    r.check("到期：下单响应不给 clash_url", not body_exp.get("clash_url"))
    r.check("到期：下单响应仍给 delivery_url（客户要看得到续费页）",
            bool(body_exp.get("delivery_url")))
    r.check("到期：下单响应给 renew_url", bool(body_exp.get("renew_url")))

    st, view_exp, _ = cli.raw("GET", f"/api/sub/view/{token_exp}")
    r.eq("到期：公开视图仍返回 200（能看不能连）", st, 200)
    if st == 200:
        r.eq("到期：视图 expired = True", view_exp.get("expired"), True)
        r.check("到期：视图不给 subscription_url",
                not view_exp.get("subscription_url"), str(view_exp.get("subscription_url")))
        r.check("到期：视图不给 clash_url",
                not view_exp.get("clash_url"), str(view_exp.get("clash_url")))
        r.check("到期：视图不给 singbox_url", not view_exp.get("singbox_url"))
        r.check("到期：视图不给 uri_url", not view_exp.get("uri_url"))
        r.check("到期：仍给续费地址（客户有出路）", bool(view_exp.get("renew_url")))

    st, _, txt = cli.raw("GET", f"/sub/{token_exp}", xff=f"{net}.9")
    r.eq("到期：订阅源拒绝服务 403", st, 403)
    r.check("到期：拒绝原因是已到期（不是设备超限）",
            "subscription expired" in txt, txt[:120])

    # ---------- 8) 到期优先于 IP 限额 ----------
    # 已到期且限额已满，必须报「已到期」，否则客户会以为换个网络就能用
    st, _, txt = cli.raw("GET", f"/sub/{token_exp}", xff=f"{net}.10")
    r.check("优先级：已到期 + 超限时，报到期而非超限",
            "subscription expired" in txt, txt[:120])

    # ---------- 9) 宽限外 + auto_delete=true 才真删 ----------
    order_purge = make_order_id(idx, "PURGE")
    way_past = now - 40 * 86400            # 超过 7 天宽限
    st, body_purge, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order_purge, "label": f"回归-{idx}-待回收",
        "expires_at": way_past, "auto_delete": True,
    }, store_headers(store_key))
    token_purge = body_purge.get("token")
    # 两个端点必须给同一个答案：宽限外就是「不认识」，404 而非 403。
    st, _, _ = cli.raw("GET", f"/api/sub/view/{token_purge}")
    r.eq("回收：宽限外 + auto_delete=true 公开视图 404", st, 404)
    st, _, _ = cli.raw("GET", f"/sub/{token_purge}")
    r.eq("回收：宽限外 + auto_delete=true 订阅源也是 404（两端一致）", st, 404)

    # 对照：宽限外但 auto_delete=false 的，不该被回收（应为 403，可续费救回）
    order_keep2 = make_order_id(idx, "KEEP2")
    st, body_keep2, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order_keep2, "label": f"回归-{idx}-宽限外保留",
        "expires_at": way_past, "auto_delete": False,
    }, store_headers(store_key))
    token_keep2 = body_keep2.get("token")
    st, _, _ = cli.raw("GET", f"/api/sub/view/{token_keep2}")
    r.eq("回收：宽限外但 auto_delete=false 不被删（视图 200）", st, 200)
    st, _, _ = cli.raw("GET", f"/sub/{token_keep2}")
    r.eq("回收：宽限外但 auto_delete=false 的订阅源是 403（不是 404）", st, 403)

    # ---------- 10) 续费 ----------
    # 未到期：从原到期日往后接
    st, before, _ = cli.raw("GET", f"/api/store/orders",
                            None, store_headers(store_key))
    # 查询接口用 query 参数，这里单独构造
    st, q_before, _ = cli.raw(
        "GET", "/api/store/orders?" + urllib.parse.urlencode({"order_id": order_main}),
        None, store_headers(store_key))
    r.eq("查询：按 order_id 查回 200", st, 200)
    r.eq("查询：查回的是同一 token", q_before.get("token"), token)
    r.eq("查询：回显 auto_delete", q_before.get("auto_delete"), True)
    r.check("查询：给出 days_left", "days_left" in q_before, str(q_before.get("days_left")))

    st, renewed, _ = cli.raw("POST", "/api/store/renew", {
        "order_id": order_main, "duration_days": 30,
    }, store_headers(store_key))
    r.eq("续费：未到期订阅续费 200", st, 200)
    r.eq("续费：token 不变（客户端无需改动）", renewed.get("token"), token)
    r.check("续费：剩余天数变多（未到期从原到期日顺延）",
            (renewed.get("days_left") or 0) > (q_before.get("days_left") or 0),
            f"{q_before.get('days_left')} -> {renewed.get('days_left')}")

    # 已到期：从当下重新起算
    st, renewed2, _ = cli.raw("POST", "/api/store/renew", {
        "order_id": order_exp, "duration_days": 30,
    }, store_headers(store_key))
    r.eq("续费：已到期订阅续费 200", st, 200)
    r.eq("续费：续费后过期标记清除", renewed2.get("expired"), False)
    r.check("续费：续费后剩余天数接近 30",
            28 <= (renewed2.get("days_left") or 0) <= 30,
            f"days_left={renewed2.get('days_left')}")
    st, _, _ = cli.raw("GET", f"/sub/{token_exp}", xff=f"{net}.9")
    r.eq("续费：续费后原 token 立刻恢复可用 200", st, 200)

    # ---------- 11) 撤销 ----------
    # 撤销后的断言一律带 XFF 走同一个已知地址，避免落到「我本机真实出口 IP」
    # 上从而撞到限额，把一个状态断言测成限额断言。
    st, revoked, _ = cli.raw("POST", "/api/store/revoke",
                             {"order_id": order_main}, store_headers(store_key))
    r.eq("撤销：默认撤销返回 200", st, 200)
    st, view_rev, _ = cli.raw("GET", f"/api/sub/view/{token}")
    r.eq("撤销：撤销后 enabled = False", view_rev.get("enabled"), False)
    r.eq("撤销：撤销后仍可见（不是 404，客户能看续费页）", st, 200)
    st, _, txt = cli.raw("GET", f"/sub/{token}", xff=f"{net}.20")
    r.eq("撤销：撤销后订阅源拒绝 403（可恢复，不是 404）", st, 403)
    r.check("撤销：拒绝原因明确说是暂停而非不存在",
            "disabled" in txt, txt[:160])

    # 撤销后仍可续费恢复
    st, back, _ = cli.raw("POST", "/api/store/renew",
                          {"order_id": order_main, "duration_days": 30}, store_headers(store_key))
    r.eq("撤销：撤销后仍可续费恢复 200", st, 200)
    st, _, _ = cli.raw("GET", f"/sub/{token}", xff=f"{net}.20")
    r.eq("撤销：恢复后订阅源可用 200", st, 200)

    # 真删
    order_del = make_order_id(idx, "DEL")
    st, body_del, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order_del, "label": f"回归-{idx}-真删", "duration_days": 30,
    }, store_headers(store_key))
    token_del = body_del.get("token")
    st, _, _ = cli.raw("POST", "/api/store/revoke",
                       {"order_id": order_del, "delete": True}, store_headers(store_key))
    r.eq("撤销：delete=true 删除 200", st, 200)
    st, _, _ = cli.raw("GET", f"/api/sub/view/{token_del}")
    r.eq("撤销：真删后公开视图 404", st, 404)
    st, _, _ = cli.raw("POST", "/api/store/renew",
                       {"order_id": order_del, "duration_days": 30}, store_headers(store_key))
    r.eq("撤销：真删后无法再续费（404）", st, 404)

    # ---------- 12) 不改回收测试（跨轮存活验证的锚点） ----------
    r.check("保留单：token_keep 存在且可访问",
            bool(token_keep) and cli.raw("GET", f"/api/sub/view/{token_keep}")[0] == 200)

    return r


# ---------------------------------------------------------------- 主流程

def main():
    global ssh_alias, probe_port
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--store-key", required=True)
    ap.add_argument("--rounds", type=int, default=5)
    ap.add_argument("--only", type=int, default=0, help="只跑指定轮次（从 1 起）")
    ap.add_argument("--ssh-alias", default="panel-host",
                    help="用于绕开 Caddy 做 IP 限额探测的 ssh 别名；留空则跳过该项")
    ap.add_argument("--probe-port", type=int, default=0,
                    help="服务器内部直连 api 的端口（默认从 --base 推导）。"
                         "当测试机经隧道/外层反代访问、而内部端口与外部不同时，必须显式指定")
    ap.add_argument("--json-out", default="")
    args = ap.parse_args()

    import os
    ssh_alias = args.ssh_alias or ""
    probe_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "ip-probe.py")
    SSH_PREFIX[1] = probe_path if os.path.exists(probe_path) else ""
    probe_port = args.probe_port or port_from_base(args.base)

    cli = Client(args.base)
    rounds = []
    t0 = time.time()

    print(f"目标: {args.base}")
    if ssh_probe_available():
        print(f"IP 限额探测: ssh {ssh_alias} → 127.0.0.1:{probe_port}")
    print(f"轮次: {args.rounds}\n")

    for i in range(1, args.rounds + 1):
        if args.only and i != args.only:
            continue
        ri = run_round(cli, args.store_key, i)
        rounds.append(ri)
        bad = ri.failed
        mark = "OK  " if not bad else "FAIL"
        print(f"[{mark}] 第 {i} 轮  {len(ri.cases) - len(bad)}/{len(ri.cases)} 通过")
        for c in bad:
            print(f"         x {c.label}  {c.detail}")

    elapsed = time.time() - t0
    total = sum(len(r.cases) for r in rounds)
    passed = total - sum(len(r.failed) for r in rounds)
    print(f"\n{'=' * 62}")
    print(f"合计 {passed}/{total} 通过   耗时 {elapsed:.1f}s")
    # 用公共 helper 算 status 分布（_http.HttpClient.log 多含 transport 字段）
    codes = _status_dist(cli.log)
    print(f"HTTP 状态分布: {json.dumps(codes, sort_keys=True)}")

    if args.json_out:
        # 用公共 report_to_json（统一三个脚本的报表 schema）
        with open(args.json_out, "w", encoding="utf-8") as f:
            json.dump(report_to_json(args.base, rounds, codes, elapsed, total, passed),
                      f, ensure_ascii=False, indent=2)
        print(f"报告: {args.json_out}")

    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
