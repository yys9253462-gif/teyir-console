#!/usr/bin/env python3
"""验收 P0 修复：到期回收后，抄走口令的买家真的连不上了。

这个脚本是 tests/expiry-really-works.py 的「修复后端」。两者判据不同：

  expiry-really-works.py  修复前应有的样子：断言「共享口令仍可用」
                          —— 它证明 bug 存在，修复后必定失败。
  pending-expiry-verify.py 修复后应有的样子：断言
                          ① 买家拿到的是**独立口令**，不是共享口令
                          ② 回收后这个独立口令在节点侧被拒绝
                          —— 它证明 bug 已修复，修复前必定失败。

用法与前者相同：

    python tests/expiry-fixed-verify.py --ssh-alias panel-host \
        --store-key <key> --node-host <节点IP> --node-key <portal api_key> \
        --node-ssh-key <私钥路径>
"""
import argparse
import json
import subprocess
import sys
import time


def ssh(host, cmd, timeout=60, key=None):
    base = ["ssh", "-o", "ConnectTimeout=12", "-o", "BatchMode=yes"]
    if key:
        base += ["-i", key]
    base += [host, cmd]
    r = subprocess.run(base, capture_output=True, timeout=timeout)
    return r.stdout.decode("utf-8", "replace").strip(), r.stderr.decode("utf-8", "replace").strip()


class Report:
    def __init__(self):
        self.rows = []

    def check(self, label, ok, detail=""):
        self.rows.append((label, bool(ok), detail))
        print("  {} {}  {}".format("v" if ok else "x", label, detail))

    def summary(self):
        passed = sum(1 for _, ok, _ in self.rows if ok)
        print("\n{}  {}/{} 通过".format("=" * 60, passed, len(self.rows)))
        return passed, len(self.rows)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:18080")
    ap.add_argument("--store-key", required=True)
    ap.add_argument("--ssh-alias", default="panel-host")
    ap.add_argument("--node-host", required=True)
    ap.add_argument("--node-key", required=True, help="节点侧 portal.json 的 api_key")
    ap.add_argument("--node-ssh-key", default=None)
    args = ap.parse_args()

    r = Report()

    def se(cmd, t=60):
        return ssh(args.ssh_alias, cmd, timeout=t)[0]

    def node(cmd, t=30):
        return ssh("root@" + args.node_host, cmd, timeout=t, key=args.node_ssh_key)[0]

    def http(method, path, payload=None, headers=None):
        cmd = "curl -s -m 25 -X {} {}".format(method, json.dumps(args.base.rstrip("/") + path))
        for k, v in (headers or {}).items():
            cmd += " -H " + json.dumps("{}: {}".format(k, v))
        if payload is not None:
            cmd += " -H 'Content-Type: application/json' -d " + json.dumps(json.dumps(payload))
        out = se(cmd)
        try:
            return json.loads(out)
        except json.JSONDecodeError:
            return {"_raw": out[:400]}

    def node_auth(password, ip):
        body = json.dumps({"addr": ip + ":12345", "auth": password,
                           "reqAddr": "example.com:443", "rx": 0, "tx": 0})
        out = node("curl -s -m 8 -X POST http://127.0.0.1:29861/auth "
                   "-H 'Content-Type: application/json' -d " + json.dumps(body))
        try:
            return json.loads(out)
        except json.JSONDecodeError:
            return {"_raw": out[:200]}

    def node_users():
        # The node portal exposes no list endpoint (POST/GET /api/v1/users/list
        # returns "API endpoint not found"), so counting accounts must go through
        # the data file the portal actually persists to. Doing this the wrong way
        # is worse than not doing it: a broken probe reads 0 before and 0 after
        # and looks consistent, which is exactly how it hid a real orphan.
        raw = node("python3 -c "
                   + json.dumps("import json;print(len(json.load(open('/etc/hysteria/portal.json')).get('users',{})))"))
        try:
            return int(raw.strip().splitlines()[-1])
        except (ValueError, IndexError):
            return -1

    print("=" * 62)
    print("P0 修复验收   base={}  node={}".format(args.base, args.node_host))

    # --- 取证：节点侧现有账号（用于证明「开户」确实发生了）---
    print("\n[0] 节点侧开户前的账号数")
    before_count = node_users()
    print("  现有账号 {} 个".format(before_count))

    # --- 下单 ---
    oid = "ord_expiry_fixed_{}".format(int(time.time()))
    order = http("POST", "/api/store/orders",
                 {"order_id": oid, "duration_days": 1, "ip_limit": 2},
                 {"X-Store-Key": args.store_key})
    if order.get("_raw") or order.get("error"):
        print("下单失败:", json.dumps(order, ensure_ascii=False)[:400])
        return 1
    token = (order.get("delivery_url") or "").rstrip("/").split("/")[-1]
    print("  订单 {}  token={}...".format(oid, token[:16]))

    # --- 关键：买家拿到的口令必须是独立的 ---
    print("\n[1] 商城下发给买家的口令，必须**不是**共享口令")
    raw = se("curl -s -m 10 " + json.dumps(
        "{}/sub/{}?format=clash".format(args.base.rstrip("/"), token)))
    buyer_pwd = None
    try:
        for p in json.loads(raw).get("proxies", []):
            buyer_pwd = p.get("password")
    except Exception:
        pass
    r.check("拿到买家口令", bool(buyer_pwd),
            "pwd=" + (str(buyer_pwd)[:16] + "..." if buyer_pwd else "None"))

    shared = "example_shared_password_replace_me"
    r.check("买家口令 != 节点共享口令（这是本次修复的核心）",
            bool(buyer_pwd) and buyer_pwd != shared,
            "（若 == {} 则说明修复未生效）".format(shared[:12] + "..."))

    if not buyer_pwd:
        return 1

    # --- 这个口令在节点侧对应谁 ---
    print("\n[2] 该口令在节点侧对应哪个账号")
    who = node_auth(buyer_pwd, "203.0.113.20")
    ident = who.get("id", "")
    r.check("口令能通过节点侧认证（开户成功）", who.get("ok") is True,
            json.dumps(who, ensure_ascii=False))
    r.check("对应的是该买家自己的账号，不是 admin_master",
            bool(ident) and ident != "admin_master", "id=" + (ident or "(空)"))

    # --- 节点侧账号数应当 +1 ---
    after_count = node_users()
    r.check("节点侧账号数确实增加了", after_count > before_count,
            "{} -> {}".format(before_count, after_count))

    # --- 回收 ---
    #
    # This must exercise the APPLICATION's reclaim path. The previous version of
    # this script ran `DELETE FROM subscription_tokens` through psql, which
    # bypasses the server entirely: it deletes the row, produces the 404, and
    # never calls revocation at all. It therefore reported the 404 as a pass
    # while the buyer's account was still alive — the test proved its own DELETE
    # worked, not that the fix worked.
    #
    # The honest trigger is the shop's revoke endpoint, which is the path a real
    # shop takes: POST /api/store/revoke with delete=true. That is what the
    # panel's own reclaim logic is wired into, so what survives here is what
    # survives for a real customer.
    print("\n[3] 执行到期回收（走商城吊销接口，不是直接删表）")
    rv = http("POST", "/api/store/revoke",
              {"order_id": oid, "delete": True},
              {"X-Store-Key": args.store_key})
    r.check("吊销接口返回成功", rv.get("ok") is True,
            json.dumps(rv, ensure_ascii=False)[:200])
    # Ask for the status line via -I-style headers, not via -w. curl's
    # `-w '%{http_code}'` has to survive three shell layers here (bash on
    # Windows -> ssh -> bash on the server) and the braces keep losing their
    # escaping, producing a bare "}". Grepping the response's own status line
    # needs no format string at all.
    sub_url = "{}/sub/{}?format=clash".format(args.base.rstrip("/"), token)
    head = se("curl -s -m 10 -D - -o /dev/null " + json.dumps(sub_url))
    status = ""
    for line in head.splitlines():
        if line.upper().startswith("HTTP/"):
            parts = line.split()
            if len(parts) >= 2:
                status = parts[1]
            break
    r.check("回收后订阅链接失效（404）", status == "404", "http=" + (status or "(无状态行)"))

    # --- 决定性判据 ---
    print("\n[4] 决定性判据：抄走口令的买家还能不能连")
    time.sleep(1.5)  # 销号是异步并发做的，给它一点时间
    gone = node_auth(buyer_pwd, "203.0.113.21")
    r.check("回收后买家口令**不能**再通过节点侧认证",
            gone.get("ok") is not True,
            "节点侧回答 = " + json.dumps(gone, ensure_ascii=False))

    final_count = node_users()
    r.check("节点侧该账号已被删除", final_count <= before_count,
            "{} -> {}".format(after_count, final_count))

    p, t = r.summary()
    if p == t:
        print("\n结论：修复生效 —— 到期回收后，买家手里那份配置真的连不上了。")
    else:
        print("\n结论：仍有问题，见上面标 x 的项。")
    return 0 if p == t else 2


if __name__ == "__main__":
    sys.exit(main())
