#!/usr/bin/env python3
"""
第 2 轮：并发与竞态。

重点：IP 限额的「读-判断-写」三步没有加锁。
两个请求并发拉同一条订阅时，都会把 active_ips 读进内存、各自判断「没超」，
然后各自整体覆写这一列 —— 后写的覆盖先写的，某个已放行的地址会被静默丢掉。
这会让「已计过数的 IP 重访仍放行」在并发下失效，也可能让限额被绕过。

本脚本用真实并发击穿它，并区分两种结果：
  * 超卖（放进来的不同 IP 数 > ip_limit）—— 限额被绕过，是安全性质的问题
  * 丢记录（响应 200 但 active_ips 里查不到该 IP）—— 配额账目不可信，是正确性问题

同时验证并发下单的幂等：N 个线程同时用同一个 order_id 下单，
必须只产生一个 token（靠 order_id 唯一约束 + 冲突处理，不是靠应用层判断）。
"""
import argparse
import json
import socket
import ssl
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor


def https_post(base, path, body, headers=None):
    hdrs = {"Content-Type": "application/json"}
    if headers:
        hdrs.update(headers)
    r = urllib.request.Request(base + path, data=json.dumps(body).encode(),
                               headers=hdrs, method="POST")
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read())
        except Exception:                                        # noqa: BLE001
            return e.code, None
    except Exception as e:                                       # noqa: BLE001
        return 0, {"transport": str(e)[:100]}


def store_post(base, key, path, body):
    return https_post(base, path, body, {"X-Store-Key": key})


# 服务器内部直连 api 的端口，由 --probe-port 指定（缺省沿用历史值 18080）。
#
# --base 是「测试机怎么访问面板」，这个端口是「服务器上 api 容器映射出来的
# 端口」，真实部署里两者经常不同；写死会让探测打到无关服务上，
# 于是断言拿到一串 301（实测踩过：那台机器 18080 被别的服务占着）。
PROBE_PORT = "18080"


def ssh_probe(alias, token, ips, port="18080"):
    """从服务器内部直连 api 端口，并发打同一批 IP，返回 {ip: code}。

    必须绕开 Caddy：服务端只认 X-Forwarded-For 的最后一跳，经 Caddy 时那一跳
    永远是它追加的真实地址，所有 IP 会被算成同一个。

    用 `&` 并发提交，但**不能按返回顺序**对应 IP —— 完成顺序不确定。
    每个子进程把结果写进自己的临时文件，最后按 IP 读回来。

    两处必须做对，否则断言会以"看起来像产品缺陷"的方式失败：
      * alias 为空 = 就在目标机上跑，直接用本机 bash（ssh 别名是调用方的概念，
        在目标机上解析不了）。调用方与 ssh_probe 都用这个约定。
      * 429 退避重试：订阅 feed 限流是 `sub:<token>` 桶、20 次 / 10 秒，
        而并发段一次就打好几个 IP、还要重访，从本机打（无网络延迟）很容易
        几十毫秒内打满额度，于是后半程整片 429。429 不是"被拒"，是"等会儿再来"，
        不退避就会把限额逻辑的断言打成一片假失败。
    """
    script = r'''
set -u
TOKEN="$1"; PORT="$2"; OUTDIR="$3"
shift 3
mkdir -p "$OUTDIR"
rm -f "$OUTDIR"/*.code

# 带 429 退避的单次请求：最多试 6 次，间隔 0.4s 起指数退避
fetch_code() {
  local ip="$1" delay=0.4 i=0 code
  while [ $i -lt 6 ]; do
    code=$(curl -s -o /dev/null -w "%{http_code}" \
      -H "X-Forwarded-For: $ip" \
      --max-time 10 \
      "http://127.0.0.1:$PORT/sub/$TOKEN")
    [ "$code" != "429" ] && { printf '%s' "$code"; return; }
    sleep "$delay"
    delay=$(awk -v d="$delay" 'BEGIN{printf "%.2f", (d*1.8>6?6:d*1.8)}')
    i=$((i+1))
  done
  printf '%s' "$code"
}

for ip in "$@"; do
  (
    fetch_code "$ip" > "$OUTDIR/$ip.code"
  ) &
done
wait
for f in "$OUTDIR"/*.code; do
  ip=$(basename "$f" .code)
  printf '%s=%s\n' "$ip" "$(cat "$f")"
done
'''
    if alias:
        cmd = ["ssh", alias, "bash -s -- " + " ".join([token, port, "/tmp/ipprobe", *ips])]
    else:
        cmd = ["bash", "-s", "--", token, port, "/tmp/ipprobe", *ips]
    res = subprocess.run(cmd, input=script.encode(), capture_output=True, timeout=180)
    out = res.stdout.decode("utf-8", "replace").strip()
    mapping = {}
    for line in out.splitlines():
        if "=" in line:
            ip, _, code = line.partition("=")
            mapping[ip.strip()] = code.strip()
    if not mapping:
        mapping["__error__"] = (out + res.stderr.decode("utf-8", "replace"))[:200]
    return mapping


def get_ips(alias, token, port="18080"):
    """读回这条订阅的 IP 记账（active_ip_count / ip_limit / 明细）。

    alias 为空表示就在目标机上跑（同 ssh_probe）。
    """
    script = r'''
set -u
curl -s "http://127.0.0.1:$1/api/sub/view/$2" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(json.dumps({'count':d.get('active_ip_count'),'limit':d.get('ip_limit'),'raw':d.get('active_ips',[])}))
"
'''
    if alias:
        cmd = ["ssh", alias, "bash -s -- " + f"{port} {token}"]
    else:
        cmd = ["bash", "-s", "--", port, token]
    res = subprocess.run(cmd, input=script.encode(), capture_output=True, timeout=60)
    try:
        return json.loads(res.stdout.decode().strip().splitlines()[-1])
    except Exception:                                            # noqa: BLE001
        return {"error": (res.stdout + res.stderr).decode("utf-8", "replace")[:200]}


def main():
    global PROBE_PORT
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--store-key", required=True)
    ap.add_argument("--ssh-alias", default="panel-host")
    ap.add_argument("--concurrency", type=int, default=8)
    ap.add_argument("--probe-port", default="18080",
                    help="服务器内部直连 api 的端口（默认 18080）。"
                         "当测试机经隧道/外层反代访问、而内部端口与外部不同时，必须显式指定")
    ap.add_argument("--json-out", default="")
    args = ap.parse_args()

    PROBE_PORT = args.probe_port
    base, key, alias = args.base, args.store_key, args.ssh_alias
    cases = []

    def check(label, ok, detail=""):
        cases.append((label, bool(ok), str(detail)[:200]))

    # ---------------- A. 并发争抢最后一个名额 ----------------
    # ip_limit=1，8 个不同 IP 同时请求。正确行为是：恰好 1 个 200，其余 403。
    # 若出现 2 个以上 200，说明限额被并发绕过（超卖）。
    oid_a = f"RACE-A-{int(time.time())}"
    st, body = store_post(base, key, "/api/store/orders",
                          {"order_id": oid_a, "label": "并发限额", "duration_days": 30, "ip_limit": 1})
    if st != 200:
        print(f"建订阅失败 {st}")
        return 1
    tok_a = body["token"]

    ips = [f"203.0.115.{10 + i}" for i in range(args.concurrency)]
    m = ssh_probe(alias, tok_a, ips, PROBE_PORT)
    n200 = sum(1 for c in m.values() if c == "200")
    n403 = sum(1 for c in m.values() if c == "403")
    check(f"并发争抢：恰好 1 个 IP 被放行（实得 {n200} 个 200 / {n403} 个 403）",
          n200 == 1, json.dumps(m, ensure_ascii=False))
    check("并发争抢：其余请求全部 403", n403 == len(ips) - 1, json.dumps(m, ensure_ascii=False))

    info = get_ips(alias, tok_a, PROBE_PORT)
    check("并发争抢：账上只记 1 个 IP（无丢失/无超记）",
          info.get("count") == 1, json.dumps(info, ensure_ascii=False))

    # ---------------- B. 已放行的 IP 并发重访 ----------------
    # 一个已知 IP + 多个新 IP 同时打：已知的必须放行，新的必须被拒。
    oid_b = f"RACE-B-{int(time.time())}"
    st, body = store_post(base, key, "/api/store/orders",
                          {"order_id": oid_b, "label": "并发重访", "duration_days": 30, "ip_limit": 2})
    tok_b = body["token"]
    known = "203.0.116.1"
    ssh_probe(alias, tok_b, [known], PROBE_PORT)                 # 先让它被记上
    before = get_ips(alias, tok_b, PROBE_PORT)
    check("并发重访：预热后已记 1 个 IP", before.get("count") == 1, json.dumps(before))

    # 4 个已知 IP + 4 个新 IP 同时打。配额 2、已用 1，所以：
    #   * 已知 IP 必须全部 200（它已经占着名额，不该被自己挤掉）
    #   * 新 IP 里只能有 1 个成功（补上最后那个空位），其余 403
    new_ips = [f"203.0.116.{50 + i}" for i in range(4)]
    batch = [known] * 4 + new_ips
    m2 = ssh_probe(alias, tok_b, batch, PROBE_PORT)
    known_codes = [m2.get(known)]
    new_codes = [m2.get(i) for i in new_ips]
    check(f"并发重访：已知 IP 全部放行（实得 {known_codes}）",
          all(c == "200" for c in known_codes), json.dumps(m2, ensure_ascii=False))
    new_ok = sum(1 for c in new_codes if c == "200")
    check(f"并发重访：新 IP 只放行 1 个（实得 {new_ok} 个）",
          new_ok == 1, json.dumps(m2, ensure_ascii=False))
    check(f"并发重访：其余新 IP 被拒（实得 {new_codes}）",
          sum(1 for c in new_codes if c == "403") == 3, json.dumps(m2, ensure_ascii=False))

    after = get_ips(alias, tok_b, PROBE_PORT)
    # 账上应是：1 个已知 + 1 个新 = 2，正好等于 limit；不能超过，也不能少记
    check("并发重访：账目精确为 2（无丢失、无超记）",
          after.get("count") == 2, json.dumps(after, ensure_ascii=False))

    # ---------------- C. 并发下单幂等 ----------------
    # 8 个线程同时用同一 order_id 下单，必须只产生一个 token。
    oid_c = f"RACE-C-{int(time.time())}"
    results = []
    lock = threading.Lock()

    def place():
        st, b = store_post(base, key, "/api/store/orders",
                           {"order_id": oid_c, "label": "并发下单",
                            "duration_days": 30, "ip_limit": 3})
        with lock:
            results.append((st, (b or {}).get("token") if b else None))

    with ThreadPoolExecutor(max_workers=args.concurrency) as ex:
        list(ex.map(lambda _: place(), range(args.concurrency)))

    ok_count = sum(1 for st, _ in results if st == 200)
    tokens = {t for _, t in results if t}
    check(f"并发下单：所有请求都返回 200（实得 {ok_count}/{len(results)}）",
          ok_count == len(results), f"statuses={[s for s,_ in results]}")
    check(f"并发下单：只产生 1 个 token（实得 {len(tokens)} 个）",
          len(tokens) == 1, f"tokens={list(tokens)[:3]}")
    check("并发下单：没有 500（唯一约束冲突被正确消化）",
          all(st != 500 for st, _ in results), f"statuses={[s for s,_ in results]}")

    # 库里该 order_id 只应有一行
    check("并发下单：token 非空", len(tokens) == 1 and bool(list(tokens)[0]))

    # ---------------- D. 并发续费不丢更新 ----------------
    # 5 个线程同时续费同一条，天数应精确累加而不是相互覆盖。
    oid_d = f"RACE-D-{int(time.time())}"
    st, body = store_post(base, key, "/api/store/orders",
                          {"order_id": oid_d, "label": "并发续费", "duration_days": 1})
    tok_d = body["token"]
    import urllib.parse
    r = urllib.request.Request(base + "/api/store/orders?" + urllib.parse.urlencode({"order_id": oid_d}),
                               headers={"X-Store-Key": key})
    with urllib.request.urlopen(r, timeout=30) as resp:
        d0 = json.loads(resp.read())
    days0 = d0.get("days_left")

    def renew():
        store_post(base, key, "/api/store/renew", {"order_id": oid_d, "duration_days": 10})

    with ThreadPoolExecutor(max_workers=5) as ex:
        list(ex.map(lambda _: renew(), range(5)))

    with urllib.request.urlopen(r, timeout=30) as resp:
        d1 = json.loads(resp.read())
    days1 = d1.get("days_left")
    # 5 次续费各加 10 天 = +50；允许 1 天误差（跨秒/次日边界）
    check(f"并发续费：天数累加正确（{days0} -> {days1}，预期约 +50）",
          days1 is not None and days0 is not None and (days1 - days0) >= 49,
          f"delta={None if days0 is None or days1 is None else days1 - days0}")

    total = len(cases)
    passed = sum(1 for _, ok, _ in cases if ok)
    print(f"第 2 轮（并发与竞态）  {passed}/{total} 通过")
    for label, ok, detail in cases:
        if not ok:
            print(f"  x {label}\n      {detail}")
    if args.json_out:
        with open(args.json_out, "w", encoding="utf-8") as f:
            json.dump({"total": total, "passed": passed,
                       "cases": [{"label": l, "pass": o, "detail": d} for l, o, d in cases]},
                      f, ensure_ascii=False, indent=2)
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
