#!/usr/bin/env python3
"""
从服务器内部直连 api 端口探测 IP 限额，绕开 Caddy。

为什么必须绕开：服务端只认 X-Forwarded-For 的**最后一跳**（这是防伪造的关键
设计）。经 Caddy 时，最后一跳永远是 Caddy 追加的真实客户端地址，所以从外部
无论怎么伪造 XFF 都会被算成同一个 IP —— 那样验证的是 Caddy，不是限额。

用法（在能 ssh 到目标机的机器上跑）:
  python ip-probe.py --alias se --token <token> --ips a,b,c --revisit a
输出一行 JSON。
"""
import argparse
import json
import subprocess
import sys


REMOTE = r'''
set -e
TOKEN="$1"
IPS="$2"
REVISIT="$3"
PORT="${4:-18080}"

code_for() {
  curl -s -o /dev/null -w "%{http_code}" \
    -H "X-Forwarded-For: $1" \
    "http://127.0.0.1:$PORT/sub/$TOKEN"
}

IFS=',' read -r I1 I2 I3 <<< "$IPS"
A=$(code_for "$I1")
B=$(code_for "$I2")
C=$(code_for "$I3")
D=$(code_for "$REVISIT")

# 顺带取一下计数，证明真的记了两个地址而不是碰巧放行
CNT=$(curl -s "http://127.0.0.1:$PORT/api/sub/view/$TOKEN" \
      | grep -oE '"active_ip_count":[0-9]+' | head -1 | cut -d: -f2)

printf '{"first":%s,"second":%s,"third":%s,"revisit":%s,"active_ip_count":%s}\n' \
  "$A" "$B" "$C" "$D" "${CNT:-null}"
'''


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--alias", required=True, help="ssh 别名，如 se")
    ap.add_argument("--token", required=True)
    ap.add_argument("--ips", required=True, help="逗号分隔的三个 IP")
    ap.add_argument("--revisit", required=True, help="复用第一个 IP 验证重访")
    ap.add_argument("--port", default="18080")
    args = ap.parse_args()

    # 脚本体一律用二进制模式喂给 bash：Windows 上 text=True 会把 \n 转成 \r\n，
    # 远端报 `$'\r': command not found`，且报错位置指向无关行，很难看出是换行符问题。
    #
    # --alias 为空表示「就在目标机上跑」：这时不需要 ssh 跳板，
    # 直接用本机 bash 执行同一段脚本。ssh 别名是**调用方**那台机器的概念，
    # 在目标机上解析不了（实测报 Could not resolve hostname se）。
    if args.alias:
        cmd = ["ssh", args.alias, "bash -s -- "
               f"{args.token!r} {args.ips!r} {args.revisit!r} {args.port!r}"]
    else:
        cmd = ["bash", "-s", "--",
               args.token, args.ips, args.revisit, args.port]

    res = subprocess.run(
        cmd,
        input=REMOTE.encode("utf-8"),
        capture_output=True, timeout=120,
    )
    out = (res.stdout or b"").decode("utf-8", "replace")
    err = (res.stderr or b"").decode("utf-8", "replace")
    if res.returncode != 0:
        print(json.dumps({"error": (err or out)[:400]}))
        return 1
    print(out.strip().splitlines()[-1])
    return 0


if __name__ == "__main__":
    sys.exit(main())
