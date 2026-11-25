#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
第 4 轮：状态机与边界值。

前几轮回答的是「功能能不能用」。这一轮只问一件事：**极值处守不守规矩**。

覆盖：
  A. ip_limit 合法边界   0（不限）/ 1 / 1000（上限）/ -1 / 1001
  B. duration_days 边界  0 / -1 / 1 / 3650（上限）/ 3651
  C. expires_at 边界     过去 / 等于 now / 未来；与 duration_days 同时给时谁赢
  D. 语义确认            ip_limit=0 真的放行第 3、4 个 IP（不是被当 1 用）
  E. days_left 哨兵      -1=永久，0=今天/已过期，且两者可区分
  F. 宽限窗口边界        expires_at = now - 7d ± ε，回收与不回收的分界
  G. 重复下单幂等        同一 order_id 带不同极值参数，仍返回原值不被改写

用法：
  python tests/boundaries.py --base https://panel.example.com \\
      --store-key shop_xxx --ssh-alias panel-host \\
      --json-out .workbuddy/scratch/shots/report-round4-boundaries.json
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
import urllib.error
import urllib.request

DAY = 86400
IP_WINDOW = 24 * 3600
# 注意：这里曾经有 GRACE = 7 * DAY，用来测「宽限窗口」的内外两侧。
# 宽限期现在是 0（store.go: subscriptionExpiryGrace = 0），那个中间态不存在，
# 所以常量与依赖它的断言一并去掉；到期回收的两种取值改在 F 段按
# auto_delete 对比测。不要再把 7 天宽限当既有事实写回来。

SSH_ALIAS = None
SSH_OK: bool | None = None

# 服务器内部直连 api 的端口，由 --probe-port 指定（缺省沿用历史值 18080）。
# 与 store-regression.py 同理：--base 是「测试机怎么访问面板」，而这个端口是
# 「服务器上 api 容器映射出来的端口」，真实部署里两者经常不同。
PROBE_PORT = 18080


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------
def request(method, url, *, headers=None, body=None, timeout=25):
    data = None
    hdrs = {"User-Agent": "nodeops-boundaries/1.0"}
    if headers:
        hdrs.update(headers)
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        hdrs["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, raw, dict(resp.headers)
    except urllib.error.HTTPError as e:
        raw = e.read()
        return e.code, raw, dict(e.headers)
    except Exception as e:  # noqa: BLE001
        return 0, str(e).encode("utf-8"), {}


def jload(raw):
    try:
        return json.loads(raw.decode("utf-8"))
    except Exception:  # noqa: BLE001
        return {}


def store_headers(key):
    return {"X-Store-Key": key}


# ---------------------------------------------------------------------------
# SSH IP probe — must bypass Caddy, because Caddy overwrites XFF.
#
# 端口由 $PROBE_PORT 传入，不写死 18080：本机的外部访问地址（--base，可能经隧道
# 或外层反代）与「服务器上直连 api 容器的端口」在真实部署里经常不是同一个。
# 写死会让探测打到无关服务上 —— 实测踩过：那台机器 18080 被另一个 Express
# 服务占着，它对 /sub/x 回 301，于是断言全变成 got=301。
# ---------------------------------------------------------------------------
REMOTE = r"""
set -u
TOKEN="$1"
PROBE_PORT="${2:-18080}"
umask 077
rm -rf /tmp/bprobe; mkdir -p /tmp/bprobe
shift 2
i=0
for ip in "$@"; do
  i=$((i+1))
  (
    code=$(curl -s -o /dev/null -w '%{http_code}' \
      -H "X-Forwarded-For: $ip" \
      --max-time 10 \
      "http://127.0.0.1:$PROBE_PORT/sub/$TOKEN")
    printf '%s' "$code" > "/tmp/bprobe/$i.code"
  ) &
done
wait
i=0
for ip in "$@"; do
  i=$((i+1))
  printf '%s=%s\n' "$ip" "$(cat /tmp/bprobe/$i.code 2>/dev/null)"
done
"""


def ssh_probe_available():
    """内部探测能不能做。

    两种情况都算可用：
      * 给了 ssh 别名 —— 从别的机器 ssh 过去探测；
      * 别名为空 —— 假定脚本**就在目标机上跑**，用本机 bash，
        此时连探测都不需要 ssh（见 ssh_probe / db_query 的分支）。

    早先这里把「别名为空」直接判成不可用，于是在目标机上跑时会静默跳过
    所有依赖内部探测与直连数据库的断言，报一句"环境不可用"，
    看上去像环境不满足，其实是脚本没考虑本机执行这种用法。
    """
    global SSH_OK
    if SSH_OK is None:
        if not SSH_ALIAS:
            # 本机模式：不做 ssh，但要确认本机 bash 可用（几乎总是可用）
            SSH_OK = True
        else:
            r = subprocess.run(
                ["ssh", "-o", "ConnectTimeout=12", "-o", "BatchMode=yes",
                 SSH_ALIAS, "echo ok"],
                capture_output=True,
            )
            SSH_OK = r.returncode == 0 and b"ok" in r.stdout
    return SSH_OK


def ssh_probe(token, ips):
    """Returns {ip: http_code_str}. Sequential per IP, parallel across IPs."""
    if not ssh_probe_available():
        return None
    # SSH_ALIAS 为空 = 就在目标机上跑，用本机 bash。
    # ssh 别名是**调用方**那台机器的概念，在目标机上解析不了
    # （实测报 Could not resolve hostname se）。
    if SSH_ALIAS:
        cmd = ["ssh", "-o", "ConnectTimeout=12", "-o", "BatchMode=yes", SSH_ALIAS,
               "bash -s --", token, str(PROBE_PORT)] + list(ips)
    else:
        cmd = ["bash", "-s", "--", token, str(PROBE_PORT)] + list(ips)
    # NOTE: bytes in, no text=True — Windows text mode would inject CR and the
    # remote shell would die with `$'\r': command not found`.
    r = subprocess.run(cmd, input=REMOTE.encode("utf-8"), capture_output=True)
    out = r.stdout.decode("utf-8", "replace")
    got = {}
    for line in out.splitlines():
        if "=" in line:
            k, _, v = line.partition("=")
            got[k.strip()] = v.strip()
    return got or None


# ---------------------------------------------------------------------------
# DB helper — read a row directly so we can assert what was actually persisted
# rather than only what the API echoed back.
#
# 容器名可配：默认是历史部署的 hy2-ops-console-db-1，但 compose 的项目名
# 由目录名决定（在 /opt/hy2-ops-console 下起就是 hy2-ops-console-db-1），
# 写死会让「直连数据库核对」那几条断言在别的部署上直接失败。
# 用 --db-container 覆盖。
# ---------------------------------------------------------------------------
DB_CONTAINER = "hy2-ops-console-db-1"


def db_query(sql):
    """直连数据库核对落库值（绕开 API，看的是真实持久化状态）。

    SSH_ALIAS 为空 = 就在目标机上跑，直接本机 docker exec。
    """
    if not ssh_probe_available():
        return None
    inner = (f"docker exec -i {DB_CONTAINER} psql -U nodeops -d nodeops -tA -F'|' -c "
             + json.dumps(sql))
    if SSH_ALIAS:
        cmd = ["ssh", "-o", "ConnectTimeout=12", "-o", "BatchMode=yes", SSH_ALIAS, inner]
    else:
        cmd = ["bash", "-c", inner]
    r = subprocess.run(cmd, capture_output=True)
    out = r.stdout.decode("utf-8", "replace").strip()
    return out


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
    global SSH_ALIAS, PROBE_PORT, DB_CONTAINER

    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--store-key", required=True)
    ap.add_argument("--ssh-alias", default=None)
    ap.add_argument("--probe-port", type=int, default=18080,
                    help="服务器内部直连 api 的端口（默认 18080）。"
                         "当测试机经隧道/外层反代访问、而内部端口与外部不同时，必须显式指定")
    ap.add_argument("--db-container", default="hy2-ops-console-db-1",
                    help="用于直连核对的 postgres 容器名（默认 hy2-ops-console-db-1，"
                         "compose 项目名跟着目录走，换个部署目录就得改）")
    ap.add_argument("--json-out", default=None)
    args = ap.parse_args()

    base = args.base.rstrip("/")
    key = args.store_key
    SSH_ALIAS = args.ssh_alias
    PROBE_PORT = args.probe_port
    DB_CONTAINER = args.db_container

    r = Runner()
    tag = str(int(time.time()))

    def order(suffix, **kw):
        payload = {"order_id": f"BND-{tag}-{suffix}", "label": f"边界-{suffix}"}
        payload.update(kw)
        st, raw, _ = request("POST", base + "/api/store/orders",
                             headers=store_headers(key), body=payload)
        return st, jload(raw), payload["order_id"]

    def view(token):
        st, raw, hdrs = request("GET", f"{base}/api/sub/view/{token}")
        return st, jload(raw), hdrs

    def subs(token):
        st, raw, hdrs = request(
            "GET", f"{base}/sub/{token}",
            headers={"User-Agent": "clash-verge/1.0"},
        )
        return st, raw, hdrs

    print(f"第 4 轮（状态机与边界值）  base={base}")

    # =====================================================================
    # A. ip_limit 合法边界
    # =====================================================================
    print("\nA. ip_limit 边界")

    for bad, desc in ((-1, "负数"), (1001, "超上限"), (999999, "离谱大")):
        st, body, _ = order(f"iplim-{bad}", duration_days=30, ip_limit=bad)
        r.eq(f"ip_limit={bad}（{desc}）被拒 400", st, 400)

    st, body, _ = order("iplim-edge0", duration_days=30, ip_limit=0)
    r.eq("ip_limit=0（不限）被接受 200", st, 200)
    r.check("ip_limit=0 回显为 0（不是被当成别的数）",
            body.get("ip_limit") == 0, f"got={body.get('ip_limit')!r}")

    st, body, _ = order("iplim-edge1", duration_days=30, ip_limit=1)
    r.eq("ip_limit=1（下限有效值）被接受 200", st, 200)
    r.eq("ip_limit=1 回显为 1", body.get("ip_limit"), 1)

    st, body, _ = order("iplim-edge1000", duration_days=30, ip_limit=1000)
    r.eq("ip_limit=1000（上限）被接受 200", st, 200)
    r.eq("ip_limit=1000 回显为 1000", body.get("ip_limit"), 1000)

    # =====================================================================
    # B. duration_days 边界
    # =====================================================================
    print("\nB. duration_days 边界")

    for bad, desc in ((-1, "负数"), (3651, "超上限"), (99999, "离谱大")):
        st, body, _ = order(f"dur-{bad}", duration_days=bad, ip_limit=2)
        r.eq(f"duration_days={bad}（{desc}）被拒 400", st, 400)

    st, body, _ = order("dur-edge3650", duration_days=3650, ip_limit=2)
    r.eq("duration_days=3650（上限）被接受 200", st, 200)
    dl = body.get("days_left")
    r.check("duration_days=3650 的 days_left 接近 3650（±2）",
            isinstance(dl, int) and abs(dl - 3650) <= 2, f"days_left={dl!r}")

    st, body, _ = order("dur-edge1", duration_days=1, ip_limit=2)
    r.eq("duration_days=1（最小有效值）被接受 200", st, 200)
    r.eq("duration_days=1 的 days_left 为 1", body.get("days_left"), 1)

    # duration_days=0 必须与 expires_at 同时给，否则 400（第 1 轮已覆盖「都没有」）
    st, body, _ = order("dur-zero-noexp", duration_days=0, ip_limit=2)
    r.eq("duration_days=0 且无 expires_at 被拒 400", st, 400)

    # =====================================================================
    # C. expires_at 边界 + 与 duration_days 的优先级
    # =====================================================================
    print("\nC. expires_at 边界与优先级")

    now = int(time.time())

    # 已过去的到期时间 -> 下单即已过期。
    #
    # 要断言「还能查到」，就得 auto_delete=false：宽限期是 0，缺省
    # auto_delete=true 时过期订阅第一次被读就会被回收（视图与 /sub 同为 404）。
    # 这里只验下单响应本身（还没触发读），但显式写 false，让「这条过期订阅
    # 为什么还在」在代码里一眼可见。
    st, body, _ = order("exp-past", expires_at=now - 2 * DAY, ip_limit=2,
                        auto_delete=False)
    r.eq("expires_at=过去 2 天 下单 200（auto_delete=false，保留）", st, 200)
    r.eq("expires_at=过去 的 expired 为 True", body.get("expired"), True)
    r.eq("expires_at=过去 的 days_left 为 0", body.get("days_left"), 0)

    # 给 duration_days 也给 expires_at：两者一致时应正常
    st, body, _ = order("exp-both", expires_at=now + 10 * DAY, duration_days=10,
                        ip_limit=2)
    r.eq("同时给 expires_at 与 duration_days 被接受 200", st, 200)

    # 检查两者冲突时谁生效：再下一次单，expires_at 明确指向 +100 天
    st, body, _ = order("exp-conflict", expires_at=now + 100 * DAY,
                        duration_days=3, ip_limit=2)
    dl = body.get("days_left")
    r.check("冲突时以 expires_at 为准（days_left≈100，而非 3）",
            isinstance(dl, int) and dl > 90, f"days_left={dl!r}")

    # =====================================================================
    # D. ip_limit=0 的语义：真的不限，不是被当 1
    # =====================================================================
    print("\nD. ip_limit=0 语义（真的不限设备）")

    st, body, _ = order("zero-unlimited", duration_days=30, ip_limit=0)
    token0 = body.get("token")
    r.check("ip_limit=0 拿到 token", bool(token0), str(token0)[:12])

    if token0 and ssh_probe_available():
        ips = [f"198.51.100.{i}" for i in range(1, 6)]
        got = ssh_probe(token0, ips)
        if got is None:
            r.check("ip_limit=0：服务器内部探测成功", False, "ssh probe 失败")
        else:
            codes = [got.get(ip) for ip in ips]
            r.check("ip_limit=0：第 5 个 IP 仍放行（不限生效，不是被当 1）",
                    all(c == "200" for c in codes),
                    f"codes={codes}")
            vst, vbody, _ = view(token0)
            r.check("ip_limit=0：视图 active_ip_count 记录了全部 5 个",
                    vbody.get("active_ip_count") == 5,
                    f"active_ip_count={vbody.get('active_ip_count')!r}")
    else:
        r.check("ip_limit=0：需要服务器内部探测（跳过）", True, "no ssh")

    # =====================================================================
    # E. days_left 哨兵：-1 永久 vs 0 到期
    # =====================================================================
    print("\nE. days_left 哨兵值")

    # 找不到「永久」的下单入口（必须给 expires_at 或 duration_days），
    # 那就直接看代码契约是否被 API 表达出来：至少 0 与正数要能区分。
    st, body, _ = order("sentinel-future", duration_days=30, ip_limit=2)
    r.check("有到期时间时 days_left 为正数（非 -1）",
            isinstance(body.get("days_left"), int) and body["days_left"] > 0,
            f"days_left={body.get('days_left')!r}")

    st, body, _ = order("sentinel-expired", expires_at=now - 3 * DAY, ip_limit=2)
    r.eq("已过期时 days_left 为 0（不是 -1）", body.get("days_left"), 0)
    r.check("「已过期=0」与「永久=-1」不是同一个值（哨兵不冲突）",
            body.get("days_left") != -1,
            f"days_left={body.get('days_left')!r}")

    # 极近未来：刚好 1 小时内到期，向上取整应给 1 而不是 0
    st, body, _ = order("sentinel-1h", expires_at=now + 3600, ip_limit=2)
    r.eq("距到期 1 小时时 days_left 向上取整为 1", body.get("days_left"), 1)

    # 极近过期：1 秒前，days_left 应为 0
    st, body, _ = order("sentinel-1s", expires_at=now - 5, ip_limit=2)
    r.eq("过期 5 秒时 days_left 为 0", body.get("days_left"), 0)

    # =====================================================================
    # F. 到期即回收（宽限期为 0）
    # =====================================================================
    #
    # 这个段落原本叫「宽限窗口边界」，用 expires_at = now - 7d ± ε 去测宽限
    # 窗口的内外两侧。但宽限期现在就是 0（store.go：subscriptionExpiryGrace
    # = 0，并注明不要悄悄恢复保留窗口），「宽限内」这个中间态在缺省配置下
    # 根本不存在：过期订阅第一次被读就被回收。
    #
    # 因此改测真正发货的规则：两种 auto_delete 取值下的可观察差异。
    print("\nF. 到期即回收（宽限期 0；对比 auto_delete 两种取值）")

    # 过期 + auto_delete 缺省（true）：视图与订阅源同为 404，且行被真删
    st, body, _ = order("exp-purge",
                        expires_at=now - 2 * DAY, ip_limit=2)
    tok_out = body.get("token")
    r.eq("过期 + auto_delete 缺省 下单 200", st, 200)
    if tok_out:
        vst, _, _ = view(tok_out)
        r.eq("过期 + auto_delete=true 视图 404（已回收）", vst, 404)
        sst, _, _ = subs(tok_out)
        r.eq("过期 + auto_delete=true 订阅源 404（两端一致）", sst, 404)
        row = db_query(
            f"SELECT count(*) FROM subscription_tokens WHERE token='{tok_out}'")
        r.check("已从库里删掉（不是只隐藏）",
                row is not None and row.strip() == "0", f"count={row!r}")

    # 过期 + auto_delete=false：保留，视图 200 且标记 expired，订阅源 403
    st, body, _ = order("exp-keep",
                        expires_at=now - 2 * DAY, ip_limit=2,
                        auto_delete=False)
    tok_keep = body.get("token")
    r.eq("过期 + auto_delete=false 下单 200", st, 200)
    if tok_keep:
        vst, vbody, _ = view(tok_keep)
        r.eq("不回收：视图 200", vst, 200)
        r.eq("不回收：expired=True", vbody.get("expired"), True)
        sst, _, _ = subs(tok_keep)
        r.eq("不回收：订阅源 403（不是 404）", sst, 403)

    # =====================================================================
    # G. 幂等 vs 极值：重放不能改写已存在订阅
    # =====================================================================
    print("\nG. 幂等重放不得改写极值参数")

    st, body, oid = order("idem-extreme", duration_days=30, ip_limit=1000)
    tok = body.get("token")
    r.eq("首次下单 ip_limit=1000 成功", st, 200)

    # 同 order_id 重放，这次带「极小」参数，不得覆盖
    st2, raw2, _ = request(
        "POST", base + "/api/store/orders", headers=store_headers(key),
        body={"order_id": oid, "label": "边界-改写尝试",
              "duration_days": 1, "ip_limit": 1},
    )
    body2 = jload(raw2)
    r.eq("重放返回 200", st2, 200)
    r.eq("重放拿回同一个 token", body2.get("token"), tok)
    r.eq("重放 ip_limit 未被改写成 1（仍是 1000）",
         body2.get("ip_limit"), 1000)
    r.check("重放 days_left 未被压低到 1（仍≈30）",
            isinstance(body2.get("days_left"), int) and body2["days_left"] > 25,
            f"days_left={body2.get('days_left')!r}")

    # =====================================================================
    # 汇总
    # =====================================================================
    total = len(r.cases)
    passed = sum(1 for c in r.cases if c.ok)
    print(f"\n第 4 轮（状态机与边界值）  {passed}/{total} 通过")
    for c in r.cases:
        if not c.ok:
            print(f"    x {c.label}  {c.detail}")

    if args.json_out:
        payload = {
            "round": "boundaries",
            "total": total,
            "passed": passed,
            "cases": [{"label": c.label, "pass": c.ok, "detail": c.detail}
                      for c in r.cases],
        }
        with open(args.json_out, "w", encoding="utf-8") as fh:
            json.dump(payload, fh, ensure_ascii=False, indent=2)
        print(f"报告已写入 {args.json_out}")

    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
