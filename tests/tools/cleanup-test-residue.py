#!/usr/bin/env python3
"""清掉测试套件在商城留下的残留订阅，让节点侧账号一并被收回。

两条硬规矩：

1. **绝不直接 DELETE 表。** 那会绕过吊销，在节点上留下仍然能连的孤儿
   账号 —— 正是本项目修过的那个 P0 本身。逐条走
   POST /api/store/revoke {delete:true}，和真实商城撤销同一条路。

2. **绝不要在测试套件运行期间执行本脚本。** 各套件的 order_id 都有固定
   前缀（store-regression 用 REG-R<轮>-<标签>-<ts>），脚本按前缀匹配，
   会把正在跑的套件的夹具一起吊销掉：表现为幂等重放突然拿到空响应
   （body2 = None）、或某条断言莫名失败。这坑踩过一次，不是产品问题。
   要清就先跑完所有套件再清。
"""
import json, os, subprocess, sys, urllib.request

BASE = "https://panel.example.com"
KEY = os.environ.get("NODEOPS_TEST_STORE_KEY", "shop_TEST_ONLY_NOT_A_REAL_CREDENTIAL_0000000000")
ALIAS = os.environ.get("NODEOPS_SSH_ALIAS", "panel-host")

def ssh(cmd):
    r = subprocess.run(["ssh", "-o", "ConnectTimeout=12", "-o", "BatchMode=yes",
                        ALIAS, cmd], capture_output=True, timeout=120)
    return r.stdout.decode("utf-8", "replace").strip()

# 各套件的订单前缀。改动这里前先确认没有同名前缀被生产订单使用。
TEST_PREFIXES = ("REG-R", "BND-", "SEC-", "RACE-", "FMT-", "BUY-", "ord_", "prdord_")
where = " OR ".join(f"order_id LIKE '{p}%'" for p in TEST_PREFIXES)
sql = ("SELECT DISTINCT order_id FROM subscription_tokens WHERE deleted_at IS NULL "
       f"AND ({where})")
raw = ssh("docker exec hy2-ops-console-db-1 psql -U nodeops -d nodeops -tA -c "
          + json.dumps(sql))
oids = [l.strip() for l in raw.splitlines() if l.strip()]
print("待吊销 order_id 数:", len(oids))

ok = fail = 0
for oid in oids:
    body = json.dumps({"order_id": oid, "delete": True}).encode()
    req = urllib.request.Request(BASE + "/api/store/revoke", data=body,
                                 headers={"Content-Type": "application/json",
                                          "X-Store-Key": KEY}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            json.loads(resp.read().decode())
            ok += 1
    except Exception as e:
        fail += 1
        if fail <= 5:
            print("  失败:", oid, e)
print(f"成功 {ok}，失败 {fail}")
print("剩余行数:", ssh("docker exec hy2-ops-console-db-1 psql -U nodeops -d nodeops -tA -c "
                     + json.dumps("SELECT count(*) FROM subscription_tokens WHERE deleted_at IS NULL")))
