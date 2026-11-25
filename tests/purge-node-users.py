#!/usr/bin/env python3
"""清点并清理节点侧残留账号。

## 为什么需要它

每跑一轮 store-regression / buyer-delivery-e2e，都会在节点侧开出一批
`sub_*` 账号（商城下单时「在节点侧为每个买家单独开户，让到期真正断网」）。
订阅到期后订阅记录被回收，节点侧账号要显式删 —— 跑几十轮就会攒下上百个。
实测：同一节点跑了一下午回归后，节点侧积了 91 个 `sub_*` 孤儿账号。

## 两条硬规则（都来自 tests/README.md，是踩过坑才写下的）

1. **必须走应用层删，不要直接改 portal.json / DELETE 表。**
   portal 进程持有那份数据并在内存里维护一致性（save_data() 会整体覆盖），
   直接改文件要么被覆盖、要么绕过它的一致性维护。

2. **清点要用可信的信源。**
   一个曾经把人骗过去的做法：用 `GET /api/v1/users/list` 数账号，
   当时该端点返回 404，于是恒得到 0 —— `0 -> 0` 看着很一致，
   正好掩盖了真实的孤儿。
   本脚本因此把「信源可用性」显式验证一遍：两个端点都试，
   都不可用就直连 SSH 读 portal.json 的 users 键，并在报告里写明用的是哪个。

## 用法

    # 只看不删
    python tests/purge-node-users.py --ssh-alias panel-host

    # 真删（默认只删 sub_* 前缀，admin_master 永远保留）
    python tests/purge-node-users.py --ssh-alias panel-host --apply

    # 指定节点 id、或改前缀白名单
    python tests/purge-node-users.py --ssh-alias panel-host --node-id <uuid> --apply
    python tests/purge-node-users.py --ssh-alias panel-host --prefix sub_,hy2_ --apply
"""
import argparse
import json
import subprocess
import sys
import time
import urllib.error
import urllib.request


def http_json(method, url, body=None, headers=None, timeout=25):
    """发一个请求，返回 (status, parsed_or_raw)。不抛异常，把错误也当结果。"""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header('Content-Type', 'application/json')
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
            try:
                return r.status, json.loads(raw or '{}')
            except Exception:                                    # noqa: BLE001
                return r.status, raw
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw or '{}')
        except Exception:                                        # noqa: BLE001
            return e.code, raw
    except Exception as e:                                       # noqa: BLE001
        return 0, {'transport': str(e)[:200]}


def ssh_run(alias, command, timeout=60):
    """在远端跑一条命令。

    input 保持 bytes、不开 text=True —— Windows 文本模式会往 stdin 注入 CR，
    远端 bash 会报 $'\\r': command not found，且报错位置指向无关行。
    （这条规矩在 tests/README.md 与本仓库多处都记着。）

    alias 传空串表示「就在本机跑」：这个脚本经常被直接放到节点所在的那台
    机器上执行（那时它要读的是本机的 /etc/hysteria/portal.json），
    而 ssh 别名是**调用方那台机器**的概念，在目标机上是解析不了的
    （实测报 `Could not resolve hostname se`）。
    """
    if not alias:
        res = subprocess.run(['bash', '-c', command],
                             capture_output=True, timeout=timeout)
        return (res.returncode,
                res.stdout.decode('utf-8', 'replace'),
                res.stderr.decode('utf-8', 'replace'))
    res = subprocess.run(
        ['ssh', '-o', 'ConnectTimeout=12', '-o', 'BatchMode=yes', alias, command],
        capture_output=True, timeout=timeout)
    return (res.returncode,
            res.stdout.decode('utf-8', 'replace'),
            res.stderr.decode('utf-8', 'replace'))


def count_via_api(base, node_id, token, portal_api_url=None, portal_key=None):
    """优先用面板接口清点（它自己会去找节点）；返回 (count, source, ids)。"""
    st, body = http_json('GET', f'{base}/api/nodes/{node_id}/users',
                         headers={'Authorization': f'Bearer {token}'})
    if st == 200 and isinstance(body, dict) and 'users' in body:
        users = body['users']
        return len(users), 'panel /api/nodes/{id}/users', [u.get('user_id') for u in users]
    return None, f'panel 接口不可用（HTTP {st}）', []


def count_via_portal_direct(portal_api_url, portal_key):
    """直接问节点 portal 的 users/list。

    ⚠️ 历史坑：这个端点**曾经不存在**（404 API endpoint not found），
    当时用它清点恒得 0，把真实孤儿掩盖了。所以这里必须检查返回体形状，
    不能只看 HTTP 200 —— 一个「ok:true 但没有 users 字段」的响应也是不可用的。
    """
    st, body = http_json('GET', f'{portal_api_url}/api/v1/users/list',
                         headers={'Authorization': f'Bearer {portal_key}'})
    if st == 200 and isinstance(body, dict):
        users = body.get('users') or body.get('data')
        if isinstance(users, list):
            return len(users), 'portal /api/v1/users/list', [u.get('user_id') or u.get('id') for u in users]
    return None, f'portal 接口不可用（HTTP {st}）', []


def count_via_ssh_portal_json(alias):
    """兜底：SSH 读 /etc/hysteria/portal.json 的 users 键。

    这是可信信源 —— portal 自己就把账号存在这里。用它交叉验证前两种方式。
    """
    code, out, err = ssh_run(
        alias,
        "python3 -c \"import json;d=json.load(open('/etc/hysteria/portal.json'));"
        "print(json.dumps(list(d.get('users',{}).keys())))\"")
    if code != 0:
        return None, f'SSH 读取失败：{(err or out)[:150]}', []
    try:
        ids = json.loads(out.strip().splitlines()[-1])
        return len(ids), 'ssh portal.json', ids
    except Exception as e:                                       # noqa: BLE001
        return None, f'解析失败：{e}', []


def delete_with_backoff(base, node_id, uid, token, max_try=6):
    """删一个节点用户；遇 429 退避重试。

    面板写接口按用户限流，一次连删上百个会大量 429。429 不是失败，
    是「等一会儿再来」，所以退避重试而不是记为失败
    （实测：不退避时 110 个只成功 19 个）。
    """
    delay = 1.0
    for _ in range(max_try):
        st, _body = http_json('DELETE', f'{base}/api/nodes/{node_id}/users/{uid}',
                              headers={'Authorization': f'Bearer {token}',
                                       'X-Confirm-Dangerous': 'yes'})
        if st in (200, 204):
            return True
        if st == 429:
            time.sleep(delay)
            delay = min(delay * 1.8, 15)
            continue
        return False
    return False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--base', default='http://127.0.0.1:8080',
                    help='面板 API 地址。默认直连 api 端口，绕开 Caddy 在配了域名后'
                         '的 HTTP->HTTPS 308 跳转')
    ap.add_argument('--ssh-alias', default='panel-host',
                    help='用于兜底读 portal.json 的 ssh 别名。'
                         '传空串（--ssh-alias ""）表示就在本机读 —— '
                         '把脚本直接放到节点那台机器上跑时用这个，'
                         '否则会报 Could not resolve hostname（ssh 别名是调用方的概念）')
    ap.add_argument('--admin-email', default='admin@nodeops.local')
    ap.add_argument('--admin-password-file', default='/root/nodeops-admin-password.txt')
    ap.add_argument('--node-id', default='', help='不指定则用清单里的第一个节点')
    ap.add_argument('--portal-api-url', default='http://127.0.0.1:40601')
    ap.add_argument('--prefix', default='sub_',
                    help='只删这个前缀的账号（逗号分隔可给多个）。admin_master 永不删')
    ap.add_argument('--apply', action='store_true', help='不传则只清点不删除')
    args = ap.parse_args()

    prefixes = tuple(p.strip() for p in args.prefix.split(',') if p.strip())

    try:
        pw = open(args.admin_password_file).read().strip()
    except OSError as e:
        print(f'读不到管理员口令：{e}')
        return 1

    st, body = http_json('POST', f'{args.base}/api/login',
                         {'email': args.admin_email, 'password': pw})
    if st != 200 or not isinstance(body, dict) or 'token' not in body:
        print(f'登录失败：HTTP {st} {str(body)[:200]}')
        return 1
    token = body['token']

    st, nodes = http_json('GET', f'{args.base}/api/nodes',
                          headers={'Authorization': f'Bearer {token}'})
    nlist = nodes if isinstance(nodes, list) else (nodes or {}).get('nodes', [])
    if not nlist:
        print('面板里没有登记任何节点')
        return 1
    node = next((n for n in nlist if n['id'] == args.node_id), nlist[0])
    node_id = node['id']

    print(f'节点：{node.get("name")}  id={node_id}')
    print(f'模式：{"实际清理" if args.apply else "只清点（加 --apply 才删）"}\n')

    # ---- 三个信源交叉清点，并说清各自是否可信 ----
    portal_key = None
    code, out, _err = ssh_run(args.ssh_alias,
                              "python3 -c \"import json;print(json.load(open('/etc/hysteria/portal.json'))['api_key'])\"")
    if code == 0 and out.strip():
        portal_key = out.strip().splitlines()[-1]
    counts = {}
    n1, src1, ids1 = count_via_api(args.base, node_id, token)
    counts[src1] = (n1, ids1)
    if portal_key:
        n2, src2, ids2 = count_via_portal_direct(args.portal_api_url, portal_key)
        counts[src2] = (n2, ids2)
    n3, src3, ids3 = count_via_ssh_portal_json(args.ssh_alias)
    counts[src3] = (n3, ids3)

    for src, (n, ids) in counts.items():
        mark = '可用' if n is not None else '不可用'
        print(f'  [{mark}] {src}: {n if n is not None else "-"} 个账号')

    # 以 SSH 读到的为准（它是 portal 自己的持久化数据），缺了再退到别的信源
    ids = ids3 if n3 is not None else (ids1 if n1 is not None else ids2)
    if not ids:
        print('\n三个信源都拿不到账号清单，中止（不要按 0 处理，那正是历史踩过的坑）')
        return 1

    targets = [u for u in ids if u and u.startswith(prefixes) and u != 'admin_master']
    print(f'\n当前账号 {len(ids)} 个，命中前缀 {prefixes} 的 {len(targets)} 个')
    for u in targets[:10]:
        print(f'  - {u}')
    if len(targets) > 10:
        print(f'  ... 还有 {len(targets) - 10} 个')

    if not args.apply:
        print('\n（只清点，未删除。加 --apply 执行）')
        return 0

    if not targets:
        print('\n没有需要清理的账号')
        return 0

    print('\n开始清理（逐个走 users/delete，429 退避重试）...')
    done = 0
    for i, uid in enumerate(targets, 1):
        if delete_with_backoff(args.base, node_id, uid, token):
            done += 1
        else:
            print(f'  {uid} 最终失败')
        if i % 10 == 0:
            cur, _s, _i2 = count_via_ssh_portal_json(args.ssh_alias)
            print(f'  进度 {i}/{len(targets)}，已删 {done}，节点侧现存 {cur}')
            time.sleep(0.4)

    time.sleep(2)
    left, _s, ids_left = count_via_ssh_portal_json(args.ssh_alias)
    print(f'\n完成：删除 {done}/{len(targets)}')
    print(f'节点侧剩余 {left} 个：{ids_left}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
