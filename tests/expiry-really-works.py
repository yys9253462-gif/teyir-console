#!/usr/bin/env python3
"""到期回收的「真实有效性」验收 —— 回答「到期了还能不能用」。

⚠️ 这个脚本会证明产品当前存在的 P0 问题，不是 bug 复现玩具。
   跑之前先读文件末尾的「结论」。

与 buyer-delivery-e2e.py 的区别：
  后者只验「面板侧」的承诺（订阅链接 404、库里删了行）。
  本脚本验「节点侧」的实际连通性 —— 买家真正能不能上网，取决于节点侧。

判据（缺一不可）：
  A. 回收后面板不再下发配置         -> 订阅链接 404
  B. 回收后买家**抄走的口令连不上** -> 节点侧 /auth 必须拒绝
  C. 节点侧存在「非共享」的每买家账号 -> /auth 返回的 id 必须是该买家的，
     不能是 admin_master 这种全局口令

用法：
    python tests/expiry-really-works.py --ssh-alias panel-host \
        --node-host <节点IP或域名> --node-key <portal api_key>
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
    ap.add_argument("--node-host", required=True, help="节点 IP，如 198.51.100.35")
    ap.add_argument("--node-key", required=True, help="节点侧 portal.json 的 api_key")
    ap.add_argument("--node-ssh-key", default=None, help="连节点用的私钥路径")
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

    def node_auth(password, ip="203.0.113.7"):
        """直接问节点侧：这个口令能不能过认证。这是最终判据。"""
        body = json.dumps({
            "addr": ip + ":12345",
            "auth": password,
            "reqAddr": "example.com:443",
            "rx": 0, "tx": 0,
        })
        out = node(
            "curl -s -m 8 -X POST http://127.0.0.1:29861/auth "
            "-H 'Content-Type: application/json' -d " + json.dumps(body)
        )
        try:
            return json.loads(out)
        except json.JSONDecodeError:
            return {"_raw": out[:200]}

    print("=" * 62)
    print("到期回收「真实有效性」验收   base={}  node={}".format(args.base, args.node_host))

    # --- 下单 ---
    oid = "ord_expiry_real_{}".format(int(time.time()))
    order = http("POST", "/api/store/orders",
                 {"order_id": oid, "duration_days": 1, "ip_limit": 2},
                 {"X-Store-Key": args.store_key})
    if order.get("_raw") or order.get("error"):
        print("下单失败:", json.dumps(order, ensure_ascii=False)[:400])
        return 1
    token = (order.get("delivery_url") or "").rstrip("/").split("/")[-1]
    print("  订单 {}  token={}".format(oid, token[:16] + "..."))

    # --- 取买家实际拿到的口令 ---
    print("\n[1] 商城下发给买家的配置里，口令是什么")
    raw = se("curl -s -m 10 " + json.dumps(
        "{}/sub/{}?format=clash".format(args.base.rstrip("/"), token)))
    buyer_pwd = None
    try:
        for p in json.loads(raw).get("proxies", []):
            buyer_pwd = p.get("password")
    except Exception:
        pass
    r.check("拿到买家口令", bool(buyer_pwd), "pwd=" + (str(buyer_pwd)[:16] + "..." if buyer_pwd else "None"))
    if not buyer_pwd:
        return 1

    # --- 节点侧这个口令是谁 ---
    print("\n[2] 这个口令在节点侧对应哪个账号")
    who = node_auth(buyer_pwd, "203.0.113.8")
    ident = who.get("id", "")
    r.check("口令能通过节点侧认证", who.get("ok") is True, json.dumps(who, ensure_ascii=False))
    r.check(
        "口令不属于每买家独立账号（是共享账号则为 P0）",
        not ident or ident == "admin_master",
        "id={} （期望：不应该是 admin_master / 空）".format(ident or "(空)"),
    )

    # --- 回收 ---
    print("\n[3] 执行到期回收")
    se("docker exec -i hy2-ops-console-db-1 psql -U nodeops -d nodeops -tA -c "
       + json.dumps("DELETE FROM subscription_tokens WHERE token='{}'".format(token)))
    code = se("curl -s -m 10 -o /dev/null -w '%{http_code}' "
              + json.dumps("{}/sub/{}?format=clash".format(args.base.rstrip("/"), token)))
    r.check("回收后订阅链接失效（404）", code.strip() == "404", "http=" + code.strip())

    # --- 决定性判据 ---
    print("\n[4] 决定性判据：抄走口令的买家还能不能连")
    after = node_auth(buyer_pwd, "203.0.113.9")
    still_on = after.get("ok") is True
    r.check(
        "回收后买家口令**不能**再通过节点侧认证",
        not still_on,
        "节点侧回答 = " + json.dumps(after, ensure_ascii=False),
    )

    p, t = r.summary()
    print("""
结论说明
--------
第 [4] 项如果为 x（口令仍可用），说明：面板侧的「到期收回」是真的，
但**买家只要在到期前把配置导入过客户端、或抄下了口令，到期后照样能连**。
因为订阅下发的是节点侧共享口令，而不是每买家独立账号。

要真正堵住，必须让商城下单时在节点侧**为这个买家单独开户**
（portal 的 POST /api/v1/users/create），到期/回收时再删除该用户。
光删 subscription_tokens 那一行不够。
""")
    return 0 if p == t else 2


if __name__ == "__main__":
    sys.exit(main())
