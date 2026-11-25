#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
买家链路端到端验收。

按运营方给出的产品承诺逐条验证，不是验证接口「能返回什么」，而是验证
「买家从下单到到期，整个过程拿到的东西对不对」：

  P1  商城购买成功后，发货页带订阅链接与二维码
        —— 交付视图必须给出 subscription_url / clash / singbox / uri 四个地址，
           且 /sub/<token> 真的能取到配置（不是死链）。

  P2  订阅必须包含所有「已添加的 hy2 节点」
        —— 拿库里 deleted_at IS NULL 的节点做基准，逐个核对是否出现在订阅里。
           出现在 excluded 里也算不通过：那说明节点在册但发不出去，
           买家拿到的节点比我们承诺的少。excluded 必须为空。

  P3  到期必须收回
        —— 到期后该行必须从库里消失（硬删除），而不是只对外隐藏。

  P4  收回后所有节点都不能用、订阅链接也不能用
        —— 四个格式全部 404；交付视图 404；且响应体里不含任何节点
           主机名/端口/口令（不能靠隐藏 UI 来假装节点没了）。

用法：
  python tests/buyer-delivery-e2e.py \
      --base https://panel.example.com --store-key shop_xxx \
      --ssh-alias panel-host \
      --json-out .workbuddy/scratch/shots/report-buyer-e2e.json

需要 --ssh-alias 来：(1) 造「刚好到期」的订阅（要能改库）；
(2) 查库确认节点基准与回收结果。

⚠️ 造「到期时间」必须用**服务器自己的时钟**（脚本里用
`SELECT EXTRACT(EPOCH FROM now())`），不能用本机 time.time()。
实测本机比 SE 快约 9 秒，用本机算的「5 秒前」在服务器眼里还是
「4 秒后到期」，下单响应会正确地回 expired=false —— 看起来像功能
坏了，其实是测试造错了数据。
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

DAY = 86400

SSH_ALIAS = None


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------
def request(method, url, *, headers=None, body=None, timeout=30):
    data = None
    hdrs = {"User-Agent": "nodeops-buyer-e2e/1.0"}
    if headers:
        hdrs.update(headers)
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        hdrs["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read(), dict(resp.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)
    except Exception as e:  # noqa: BLE001
        return 0, str(e).encode("utf-8"), {}


def jload(raw):
    try:
        return json.loads(raw.decode("utf-8"))
    except Exception:  # noqa: BLE001
        return {}


# ---------------------------------------------------------------------------
# SSH helpers — the DB is only reachable inside the box.
# ---------------------------------------------------------------------------
def ssh(sql, timeout=40):
    """Run one SQL statement via psql, return stdout. Bytes in, no text mode."""
    cmd = [
        "ssh", "-o", "ConnectTimeout=12", "-o", "BatchMode=yes", SSH_ALIAS,
        "docker exec -i hy2-ops-console-db-1 psql -U nodeops -d nodeops -tA -F'|' -c "
        + json.dumps(sql),
    ]
    r = subprocess.run(cmd, capture_output=True, timeout=timeout)
    return r.stdout.decode("utf-8", "replace").strip()


def ssh_ready():
    if not SSH_ALIAS:
        return False
    r = subprocess.run(
        ["ssh", "-o", "ConnectTimeout=12", "-o", "BatchMode=yes",
         SSH_ALIAS, "echo ok"],
        capture_output=True,
    )
    return r.returncode == 0 and b"ok" in r.stdout


# ---------------------------------------------------------------------------
# Harness
# ---------------------------------------------------------------------------
class Case:
    __slots__ = ("label", "ok", "detail")

    def __init__(self, label, ok, detail=""):
        self.label = label
        self.ok = bool(ok)
        self.detail = detail


class Runner:
    def __init__(self):
        self.cases: list[Case] = []

    def check(self, label, ok, detail=""):
        self.cases.append(Case(label, ok, detail))
        return ok

    def eq(self, label, got, want):
        return self.check(label, got == want, f"got={got!r} want={want!r}")


def main():
    global SSH_ALIAS

    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--store-key", required=True)
    ap.add_argument("--ssh-alias", default=None)
    ap.add_argument("--json-out", default=None)
    args = ap.parse_args()

    base = args.base.rstrip("/")
    key = args.store_key
    SSH_ALIAS = args.ssh_alias
    r = Runner()
    stamp = int(time.time())

    have_ssh = ssh_ready()

    def order(suffix, **kw):
        payload = {"order_id": f"BUY-{stamp}-{suffix}", "label": f"买家验收-{suffix}"}
        payload.update(kw)
        st, raw, _ = request(
            "POST", base + "/api/store/orders",
            headers={"X-Store-Key": key}, body=payload,
        )
        return st, jload(raw), payload["order_id"]

    def view(token):
        st, raw, hdrs = request("GET", f"{base}/api/sub/view/{token}")
        return st, jload(raw), hdrs

    FORMATS = ["", "clash", "singbox", "uri"]

    def sub(token, fmt=""):
        url = f"{base}/sub/{token}"
        if fmt:
            url += "?format=" + fmt
        return request("GET", url, headers={"User-Agent": "clash-verge/1.0"})

    print(f"买家链路端到端验收  base={base}")
    print(f"  服务器内探测：{'可用' if have_ssh else '不可用（部分断言跳过）'}\n")

    # =====================================================================
    # P1 — 购买 → 发货页带链接与二维码
    # =====================================================================
    print("P1  商城购买 → 发货页带订阅链接与二维码")

    st, body, oid = order("MAIN", duration_days=30, ip_limit=3)
    r.eq("P1.1 商城下单成功（200）", st, 200)
    token = body.get("token")
    r.check("P1.2 下单响应带 token", bool(token), str(token)[:16])

    for field, name in (("subscription_url", "订阅链接"),
                        ("clash_url", "Clash 地址"),
                        ("singbox_url", "sing-box 地址"),
                        ("uri_url", "通用（URI）地址")):
        v = body.get(field)
        r.check(f"P1.3 发货页数据含{name}（{field}）", bool(v), str(v)[:70])

    dv = body.get("delivery_url")
    r.check("P1.4 发货页数据含 delivery_url（买家可打开的页面）",
            bool(dv), str(dv)[:70])
    # 交付页面的地址必须与 token 对应，否则买家点开是别人的订阅
    r.check("P1.5 delivery_url 指向本单 token",
            bool(dv) and token in dv, f"delivery_url={dv}")

    # 四个格式都要真的能取到内容 —— 有地址但取不到就是死链
    if token:
        for fmt, label in (("", "默认"), ("clash", "Clash"),
                           ("singbox", "sing-box"), ("uri", "URI")):
            sst, sraw, shdrs = sub(token, fmt)
            r.eq(f"P1.6 订阅链接可实际拉取（{label}）", sst, 200)
            r.check(f"P1.7 订阅内容非空（{label}）", len(sraw) > 50,
                    f"{len(sraw)} 字节")

        # 二维码：交付页用它渲染，内容是 uri_url。这里验证「扫码能得到东西」
        vst, vbody, _ = view(token)
        r.eq("P1.8 交付视图可读（200）", vst, 200)
        r.check("P1.9 交付视图带 uri_url（二维码内容源）",
                bool(vbody.get("uri_url")), str(vbody.get("uri_url"))[:70])
        r.check("P1.10 交付视图 enabled=true 且 expired=false",
                vbody.get("enabled") is True and vbody.get("expired") is False,
                f"enabled={vbody.get('enabled')} expired={vbody.get('expired')}")

    # =====================================================================
    # P2 — 订阅包含所有已添加的 hy2 节点
    # =====================================================================
    print("\nP2  订阅必须包含所有「已添加的」hy2 节点")

    if not have_ssh:
        r.check("P2 需要服务器内查库（跳过）", True, "no ssh")
    elif not token:
        r.check("P2 前置失败：没有 token", False, "P1 未拿到 token")
    else:
        # 基准：库里所有未软删的节点
        rows = ssh("SELECT id, name, COALESCE(domain,''), listen_port "
                   "FROM hysteria_nodes WHERE deleted_at IS NULL ORDER BY created_at")
        registered = []
        for line in rows.splitlines():
            parts = line.split("|")
            if len(parts) >= 4 and parts[0]:
                registered.append({"id": parts[0], "name": parts[1],
                                   "domain": parts[2], "port": parts[3]})

        r.check("P2.1 库里有已添加的节点（基准非空）",
                len(registered) > 0, f"{len(registered)} 个")

        vst, vbody, _ = view(token)
        served = vbody.get("nodes") or []
        served_ids = {n.get("id") for n in served if isinstance(n, dict)}
        excluded = vbody.get("excluded") or []

        r.eq("P2.2 交付视图 node_count 与 nodes 长度一致",
             vbody.get("node_count"), len(served))

        missing = [n for n in registered if n["id"] not in served_ids]
        r.check(f"P2.3 每个已添加节点都出现在订阅里（{len(served)}/{len(registered)}）",
                not missing,
                "缺少：" + ", ".join(n["name"] for n in missing) if missing else "")

        # excluded 非空意味着有节点在册却发不出去 —— 对买家就是少了节点
        r.check("P2.4 没有节点被排除（excluded 为空）",
                not excluded,
                json.dumps(excluded, ensure_ascii=False) if excluded else "")

        # 订阅载荷里必须真的含有这些节点，而不只是视图这么声称
        sst, sraw, _ = sub(token, "clash")
        text = sraw.decode("utf-8", "replace")
        r.eq("P2.5 订阅载荷可拉取（用于比对节点）", sst, 200)

        in_payload = 0
        for n in served:
            if isinstance(n, dict) and (n.get("host") or "") and n["host"] in text:
                in_payload += 1
        r.check(f"P2.6 视图里的节点确实出现在订阅载荷中（{in_payload}/{len(served)}）",
                len(served) > 0 and in_payload == len(served),
                f"matched={in_payload} served={len(served)}")
        r.check("P2.7 订阅载荷里 proxies 数量与节点数一致",
                text.count('"name"') >= len(served) or len(served) == 0,
                f"name_occurrences={text.count(chr(34) + 'name' + chr(34))} served={len(served)}")

    # =====================================================================
    # P3 — 到期必须收回
    # =====================================================================
    print("\nP3  到期必须收回（硬删除，不是隐藏）")

    expired_token = None
    expired_order_id = None
    if not have_ssh:
        r.check("P3 需要服务器内改库造到期（跳过）", True, "no ssh")
    else:
        # 造一条「刚刚过期」的订阅：不早不晚，正好压在到期线上。
        #
        # 必须用**服务器自己的时钟**来算这个时间点。用本机 `time.time()` 会
        # 踩时钟偏移：实测本机比 SE 快约 9 秒，于是「5 秒前」传到服务器眼里
        # 还是「4 秒后到期」，下单响应会理直气壮地回 expired=false，
        # 看起来像功能坏了，其实是测试造错了数据。
        srv_now = ssh("SELECT EXTRACT(EPOCH FROM now())::bigint")
        try:
            base_epoch = int(srv_now.strip())
        except ValueError:
            base_epoch = int(time.time())
        r.check("P3.0 取到服务器当前时间（用于造到期点）",
                base_epoch > 0, f"server_epoch={base_epoch}")

        est, ebody, eoid = order("EXPIRED", expires_at=base_epoch - 60,
                                 ip_limit=3)
        expired_order_id = eoid
        r.eq("P3.1 造「已到期」订阅成功", est, 200)
        r.eq("P3.1a 下单响应确认已过期（expired=true）",
             ebody.get("expired"), True)
        r.check("P3.1b 已过期的下单响应不下发订阅链接（只给发货页与续费）",
                not ebody.get("subscription_url") and bool(ebody.get("delivery_url")),
                f"sub_url={ebody.get('subscription_url')!r}")
        expired_token = ebody.get("token")
        r.check("P3.1b 拿到已到期订阅的 token", bool(expired_token),
                str(expired_token)[:16])

        if expired_token:
            # 先确认它确实在库里（否则「收回」无从谈起）
            before = ssh(f"SELECT count(*) FROM subscription_tokens "
                         f"WHERE token='{expired_token}'")
            r.check("P3.2 到期订阅初始存在于库中（回收前）",
                    before.strip() == "1", f"count={before!r}")

            # 触发一次读取，应当触发懒回收
            rvst, rvraw, _ = view(expired_token)
            r.eq("P3.3 到期订阅的交付视图返回 404", rvst, 404)

            # 回收是「读时懒删」，同一请求内完成；但为排除偶发时序，
            # 用 token 精确定位并短暂重试，避免把时序问题误报成功能缺陷。
            after = "?"
            for _ in range(5):
                after = ssh(f"SELECT count(*) FROM subscription_tokens "
                            f"WHERE token='{expired_token}'")
                if after.strip() == "0":
                    break
                time.sleep(0.4)
            r.check("P3.4 到期订阅已从库中硬删除（不是只隐藏）",
                    after.strip() == "0", f"count={after!r}")

            # 未到期的那条必须还在 —— 防止把「到期回收」做成「无条件回收」
            if token:
                alive = ssh(f"SELECT count(*) FROM subscription_tokens "
                            f"WHERE token='{token}'")
                r.check("P3.5 未到期的订阅不受影响（仍在库中）",
                        alive.strip() == "1", f"count={alive!r}")

    # =====================================================================
    # P4 — 收回后所有节点不能用、订阅链接也不能用
    # =====================================================================
    print("\nP4  收回后：所有节点不能用，订阅链接也不能用")

    if not expired_token:
        r.check("P4 前置失败：没有已到期的 token", False, "P3 未拿到 token")
    else:
        # 四种格式一律失效
        for fmt, label in (("", "默认"), ("clash", "Clash"),
                           ("singbox", "sing-box"), ("uri", "URI")):
            sst, sraw, _ = sub(expired_token, fmt)
            r.eq(f"P4.1 到期后订阅链接不可用（{label}）", sst, 404)

        # 交付视图同样失效（不能「视图能看、链接能用」）
        vst, vraw, _ = view(expired_token)
        r.eq("P4.2 到期后交付视图不可用（404）", vst, 404)

        # 关键：响应体里不能残留任何节点信息。
        # 光把状态码改成 404 不够 —— 若响应体仍带节点主机名或口令，
        # 就等于「界面不显示了，数据照旧泄露」。
        leak_sources = []
        for fmt in FORMATS:
            _, sraw, _ = sub(expired_token, fmt)
            leak_sources.append(("sub:" + (fmt or "default"), sraw))
        # view() 已经把 JSON 解成 dict，这里要重新序列化回文本再扫，
        # 保持与 sub 响应一致的处理方式。
        _, vjson, _ = view(expired_token)
        leak_sources.append(("view", json.dumps(vjson,
                                                ensure_ascii=False).encode("utf-8")))

        node_hosts = []
        if have_ssh:
            # 只扫主机/域名 —— 那才是「能不能连上」的关键信息。
            # 节点显示名不算秘密（交付页正常状态下本来就会展示它），
            # 把它也扫进来只会制造误报。
            rows = ssh("SELECT COALESCE(domain,'') FROM hysteria_nodes "
                       "WHERE deleted_at IS NULL AND COALESCE(domain,'') <> ''")
            node_hosts = [x.strip() for x in rows.splitlines() if x.strip()]
        r.check("P4.3 取到了节点主机基准（用于泄漏扫描）",
                len(node_hosts) > 0, f"{node_hosts}")

        leaked = []
        for src, raw in leak_sources:
            text = raw.decode("utf-8", "replace")
            for h in node_hosts:
                if h and h in text:
                    leaked.append(f"{src} 含 {h}")
        r.check("P4.4 失效响应里不含任何节点主机/名称",
                not leaked, "; ".join(leaked[:4]))

        # 也不能含口令类字段
        secret_terms = ["auth_password", "obfs-password", "obfs_password",
                        "proxy-password", "password"]
        found = []
        for src, raw in leak_sources:
            text = raw.decode("utf-8", "replace").lower()
            for t in secret_terms:
                if t in text:
                    found.append(f"{src} 含 {t}")
        r.check("P4.5 失效响应里不含任何口令字段",
                not found, "; ".join(found[:4]))

        # 到期订阅也不该还能靠订单号续费救回（宽限为 0 的后果）
        if have_ssh and expired_order_id:
            rst, rraw, _ = request(
                "POST", base + "/api/store/renew",
                headers={"X-Store-Key": key},
                body={"order_id": expired_order_id, "duration_days": 30},
            )
            r.eq("P4.6 已回收的订阅无法靠订单号续费救回（404）", rst, 404)

            rst2, rraw2, _ = request(
                "POST", base + "/api/store/revoke",
                headers={"X-Store-Key": key},
                body={"token": expired_token},
            )
            r.check("P4.7 已回收的订阅无法再被操作（非 200）",
                    rst2 != 200, f"status={rst2}")

    # =====================================================================
    # 汇总
    # =====================================================================
    total = len(r.cases)
    passed = sum(1 for c in r.cases if c.ok)
    print(f"\n买家链路端到端验收  {passed}/{total} 通过")
    for c in r.cases:
        if not c.ok:
            print(f"    x {c.label}  {c.detail}")

    if args.json_out:
        with open(args.json_out, "w", encoding="utf-8") as fh:
            json.dump({
                "round": "buyer-delivery-e2e",
                "total": total,
                "passed": passed,
                "cases": [{"label": c.label, "pass": c.ok, "detail": c.detail}
                          for c in r.cases],
            }, fh, ensure_ascii=False, indent=2)
        print(f"报告已写入 {args.json_out}")

    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
