#!/usr/bin/env python3
#
# 用法：python3 portal-users-list-online-ip.py [--apply] [portal.py 路径]
# 不传路径时默认 /etc/hysteria/portal.py。
# 给 portal.py 的 users/list 出参加上真实在线 IP。
#
# 为什么需要：面板现在的 active_ips 记的是"谁拉取过订阅链接"（面板侧视角），
# 而运维真正想知道的是"此刻哪些 IP 正在这条订阅上跑流量"。
# 节点侧本来就有内存态 ip_tracker（auth 时更新，180 秒窗口），
# 但它只喂给了页面渲染，从来没进过 API —— 这里把它暴露出去。
#
# 幂等：已打过补丁则原样退出，避免重复插入。
import io
import os
import shutil
import sys
import time

_POSITIONAL = [a for a in sys.argv[1:] if not a.startswith("-")]
TARGET = _POSITIONAL[0] if _POSITIONAL else "/etc/hysteria/portal.py"
MARKER = "'online_ips': ["

ANCHOR = """                                'ip_limit': info.get('ip_limit', 0),
                                'created_at': info.get('created_at', now_ts),
                            }
"""

REPLACEMENT = """                                'ip_limit': info.get('ip_limit', 0),
                                'created_at': info.get('created_at', now_ts),
                                # 真实在线 IP：来自 auth 时更新的 ip_tracker，
                                # 窗口与限流判定同一个 IP_TIMEOUT_SECONDS。
                                # 内存态 —— portal 重启后为空，这是真实语义：
                                # 重启后确实要等客户端重新认证才知道谁在线。
                                'online_ips': [
                                    {'ip': ip, 'last_seen': int(t)}
                                    for ip, t in sorted(ip_tracker.get(uid, {}).items())
                                    if now_ts - t < IP_TIMEOUT_SECONDS
                                ],
                            }
"""


def main() -> int:
    apply = "--apply" in sys.argv
    with io.open(TARGET, "r", encoding="utf-8") as fh:
        src = fh.read()

    if MARKER in src:
        print("SKIP already patched")
        return 0

    count = src.count(ANCHOR)
    if count != 1:
        print("ABORT anchor found %d times, expected exactly 1" % count)
        return 2

    out = src.replace(ANCHOR, REPLACEMENT)

    # 语法自检：补丁打坏了 portal 就起不来了，必须在落盘前拦住。
    try:
        compile(out, TARGET, "exec")
    except SyntaxError as exc:
        print("ABORT syntax error after patch: %s" % exc)
        return 3

    if not apply:
        print("DRY-RUN ok, %d -> %d bytes, pass --apply to write" % (len(src), len(out)))
        return 0

    stamp = time.strftime("%Y%m%d-%H%M%S")
    backup = "%s.bak-onlineip-%s" % (TARGET, stamp)
    shutil.copy2(TARGET, backup)

    tmp = "%s.tmp-onlineip" % TARGET
    with io.open(tmp, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(out)
    shutil.copymode(TARGET, tmp)
    os.replace(tmp, TARGET)

    print("PATCHED backup=%s" % backup)
    return 0


if __name__ == "__main__":
    sys.exit(main())
