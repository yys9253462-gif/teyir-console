#!/usr/bin/env python3
#
# 用法：python3 portal-users-list-online-ip-get-branch.py [--apply] [portal.py 路径]
# 不传路径时默认 /etc/hysteria/portal.py。
# 补齐 GET 路径的 users/list —— 第一版补丁只改了 POST 分支（2896），
# 而采集器读的是 GET 分支（4260），所以 online_ips 一度没出现在响应里。
#
# 为什么两个分支都要改：portal 同时暴露 POST /api/v1/* 与 GET /api/v1/*，
# 面板的采集器用 GET，商城对账脚本用 POST。只改一边会让另一个仍然缺字段。
# 两处出参结构完全一致（users_out.append({...}) 直推，含重复的 status 键），
# 所以同一个锚点必须**恰好命中两次**，否则说明这个版本结构不同，该停下人工看。
import io
import os
import shutil
import sys
import time

_POSITIONAL = [a for a in sys.argv[1:] if not a.startswith("-")]
TARGET = _POSITIONAL[0] if _POSITIONAL else "/etc/hysteria/portal.py"
MARKER = "'online_ips': ["
EXPECTED = 2

ANCHOR = """                                'ip_limit': info.get('ip_limit', 0),
                                'created_at': info.get('created_at', now_ts),
                                'status': info.get('status', 'active'),
                            })
"""

REPLACEMENT = """                                'ip_limit': info.get('ip_limit', 0),
                                'created_at': info.get('created_at', now_ts),
                                'status': info.get('status', 'active'),
                                # 真实在线 IP：来自 auth 时更新的 ip_tracker，
                                # 窗口与限流判定同一个 IP_TIMEOUT_SECONDS。
                                # 内存态 —— portal 重启后为空，这是真实语义：
                                # 重启后确实要等客户端重新认证才知道谁在线。
                                'online_ips': [
                                    {'ip': ip, 'last_seen': int(t)}
                                    for ip, t in sorted(ip_tracker.get(uid, {}).items())
                                    if now_ts - t < IP_TIMEOUT_SECONDS
                                ],
                            })
"""


def main() -> int:
    apply = "--apply" in sys.argv
    with io.open(TARGET, "r", encoding="utf-8") as fh:
        src = fh.read()

    already = src.count(MARKER)
    if already >= EXPECTED:
        print("SKIP already patched on both branches (%d sites)" % already)
        return 0

    count = src.count(ANCHOR)
    if count != EXPECTED - already:
        # 不是"缺一处"或"全新"这种已知状态 —— 结构与预期不符，必须人工看。
        print("ABORT anchor=%d already=%d, expected one of (%d, %d)"
              % (count, already, EXPECTED, 0))
        return 3

    out = src.replace(ANCHOR, REPLACEMENT)
    try:
        compile(out, TARGET, "exec")
    except SyntaxError as exc:
        print("ABORT syntax error after patch: %s" % exc)
        return 4

    if not apply:
        print("DRY-RUN ok, %d -> %d bytes, %d sites, pass --apply to write"
              % (len(src), len(out), count))
        return 0

    stamp = time.strftime("%Y%m%d-%H%M%S")
    backup = "%s.bak-onlineip-get-%s" % (TARGET, stamp)
    shutil.copy2(TARGET, backup)
    tmp = "%s.tmp-onlineip-get" % TARGET
    with io.open(tmp, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(out)
    shutil.copymode(TARGET, tmp)
    os.replace(tmp, TARGET)
    print("PATCHED sites=%d backup=%s" % (count, backup))
    return 0


if __name__ == "__main__":
    sys.exit(main())
