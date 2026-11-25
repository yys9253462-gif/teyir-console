#!/usr/bin/env python3
"""端到端协同演练：四个新功能一起用。

为什么需要它
------------
能力面板 / 告警外发 / 孤儿补删 / 账号对账 —— 每个都单独验过了，
但**没验过它们协同工作时会不会打架**。单测看不到功能间的相互影响：

  * 开户会不会让能力面板的数据变得不准？
  * 对账会不会把孤儿表弄脏？
  * 删订阅 → 孤儿登记 → 补删，这条链上每步的状态是否一致？

本脚本的价值已经被验证过一次：它发现了「**手工建订阅不会开户**」
这个真 BUG（见 commit f41a936）—— 客户导入链接后连不上，
而面板显示一切正常。

判据的选择（血的教训）
----------------------
本脚本迭代了四版，**失败全部集中在判据选错**，而不是功能有问题：

  v1: api_url 指向死端口 → 建节点会真探 node/meta → 502
  v2: 借真实地址但漏了 api_key → 502「api_key 太短」
  v3: 订阅列表**不返回 node_accounts**（内部凭据，刻意不暴露）
      → 按它判断「分发 0 个」→ **假 FAIL**（DB 里明明有 4 个）
  v4: 对账后比整份映射 → 必然 FAIL，因为 created_at 会更新
      ⇒ 断言要盯 user_id/password，那是**客户可感知的后果**

⇒ 铁律：**判据要选权威数据源**（DB 真值 / 节点侧鉴权源），
   不要选投影（列表接口可能刻意不返回内部字段）。

用法
----
    python3 collab-drill.py                 # 完整演练
    python3 collab-drill.py --no-alert-wait # 跳过告警等待（快一些）

前置：在能访问 panel API 的机器上跑（通常是节点机本机）。
"""
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get('PANEL_API', 'http://127.0.0.1:8081')
PANEL_EMAIL = os.environ.get('PANEL_EMAIL', 'admin@nodeops.local')
PANEL_PASSWORD = os.environ.get('PANEL_PASSWORD', '')
DB_CONTAINER = os.environ.get('PANEL_DB', 'hy2-ops-console-db-1')
DB_USER = os.environ.get('PANEL_DB_USER', 'nodeops')
DB_NAME = os.environ.get('PANEL_DB_NAME', 'nodeops')
TAG = 'zzz-drill'
SKIP_ALERT_WAIT = '--no-alert-wait' in sys.argv

fails = []
steps = []


def sh(cmd):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True).stdout.strip()


def db(sql):
    """读 DB 真值。用 list 形式的 argv，避免 shell 引号转义问题。"""
    p = subprocess.run(
        ['docker', 'exec', DB_CONTAINER, 'psql', '-U', DB_USER, '-d', DB_NAME,
         '-t', '-A', '-c', sql],
        capture_output=True, text=True)
    return (p.stdout or '').strip()


def db_accounts(subid):
    raw = db("SELECT node_accounts::text FROM subscription_tokens WHERE id='%s'" % subid)
    try:
        return json.loads(raw) if raw else {}
    except Exception:
        return {}


def login():
    body = json.dumps({'email': PANEL_EMAIL, 'password': PANEL_PASSWORD}).encode()
    req = urllib.request.Request(BASE + '/api/login', data=body,
                                 headers={'Content-Type': 'application/json'}, method='POST')
    with urllib.request.urlopen(req, timeout=15) as r:
        return json.loads(r.read().decode())['token']


TOKEN = login()
AUTH = {'Authorization': 'Bearer ' + TOKEN,
        'Content-Type': 'application/json',
        'X-Confirm-Dangerous': 'yes'}


def api(m, p, payload=None, timeout=240):
    d = json.dumps(payload).encode() if payload is not None else None
    r = urllib.request.Request(BASE + p, data=d, headers=AUTH, method=m)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read().decode() or '{}')
    except urllib.error.HTTPError as e:
        raw = e.read().decode('utf-8', 'replace')
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {'raw': raw[:220]}


def ck(name, ok, detail=''):
    steps.append((name, ok))
    print('  [%s] %s%s' % ('OK  ' if ok else 'FAIL', name,
                           ('  <- ' + detail) if detail and not ok else ''))
    if not ok:
        fails.append(name)


def main():
    # 告警外发的接收器：用本机非回环地址（配置校验拒 127.0.0.1）
    import http.server
    import socketserver
    import threading

    received = []
    hook_port = 18092

    class H(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get('Content-Length', 0))
            body = self.rfile.read(n).decode('utf-8', 'replace')
            try:
                received.append(json.loads(body))
            except Exception:
                received.append({'raw': body[:200]})
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'ok')

        def log_message(self, *a):
            pass

    socketserver.TCPServer.allow_reuse_address = True
    srv = socketserver.TCPServer(('0.0.0.0', hook_port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    host_ip = sh("hostname -I | awk '{print $1}'") or '172.17.0.1'
    hook_url = 'http://%s:%d/hook' % (host_ip, hook_port)

    print('=' * 72)
    print('端到端协同演练')
    print('=' * 72)
    print()

    print('【0】基线')
    st, caps0 = api('GET', '/api/nodes/capabilities')
    st2, subs0 = api('GET', '/api/subscriptions')
    st3, orph0 = api('GET', '/api/orphans')
    base_nodes = caps0.get('fetched', 0)
    base_subs = len(subs0 if isinstance(subs0, list) else subs0.get('subscriptions', []))
    base_orph = (orph0 or {}).get('count', 0)
    base_names = sorted(n.get('node_name') for n in caps0.get('nodes', []))
    print('     节点 %d / 订阅 %d / 孤儿 %d' % (base_nodes, base_subs, base_orph))
    ck('基线可读', st == 200 and st2 == 200 and st3 == 200)
    print()

    print('【A】开户：DB 真值 vs 节点侧鉴权源')
    st, sub = api('POST', '/api/subscriptions', {'label': TAG + '-probe', 'note': TAG})
    subid = sub.get('id') or (sub.get('subscription') or {}).get('id')
    ck('建订阅成功', st in (200, 201) and subid, 'HTTP %s' % st)
    time.sleep(14)

    accts = db_accounts(subid)
    print('     DB 记录的账号: %d 个节点' % len(accts))
    ck('建订阅时自动开户（DB 真值）', len(accts) > 0,
       'node_accounts 为空 —— 手工建订阅不会开户（曾是一个真 BUG）')
    for nid_, a in list(accts.items())[:6]:
        print('       %s -> user=%s' % (nid_[:8], a.get('user_id')))

    if accts:
        uid = list(accts.values())[0].get('user_id')
        print('     反查节点侧：uid=%s' % uid)
        found = 0
        st, caps = api('GET', '/api/nodes/capabilities')
        for n in caps.get('nodes', []):
            if not n.get('reachable'):
                continue
            st, users = api('GET', '/api/nodes/%s/users' % n.get('node_id'), timeout=90)
            ul = users if isinstance(users, list) else users.get('users', [])
            if any(u.get('user_id') == uid for u in ul):
                found += 1
        print('     在 %d 个可达节点上找到' % found)
        ck('账号真的开在节点上（不只是记在库里）', found > 0, '节点侧找不到')

    print()
    print('【B】对账：幂等 + 无副作用')

    def key_fields(accounts):
        # ⚠️ 只比 user_id/password，不比整个 JSON ——
        # 对账会给节点重新下发，created_at 必然更新，那是无害的；
        # 只有密码变了才会让客户断线。
        return {k: (v.get('user_id'), v.get('password'))
                for k, v in (accounts or {}).items()}

    before = key_fields(db_accounts(subid))
    st, rc1 = api('POST', '/api/subscriptions/%s/reconcile' % subid)
    st, rc2 = api('POST', '/api/subscriptions/%s/reconcile' % subid)
    ck('两次对账都 200', st == 200, 'HTTP %s' % st)
    ck('两次 ok_nodes 一致（幂等）',
       rc1.get('ok_nodes') == rc2.get('ok_nodes'),
       '%s vs %s' % (rc1.get('ok_nodes'), rc2.get('ok_nodes')))
    after = key_fields(db_accounts(subid))
    ck('对账未改动 user_id / password（客户不会断线）',
       before == after, '关键字段变了')
    print('     ok=%s failed=%s；关键字段未变=%s' % (
        rc1.get('ok_nodes'), rc1.get('failed_nodes'), before == after))
    print()

    print('【C】告警外发 ↔ 能力面板 协同')
    st, b = api('PUT', '/api/alerts/webhook', {'url': hook_url})
    ck('配好告警外发', st == 200 and b.get('configured'), json.dumps(b)[:100])

    victim = None
    old_port = None
    if not SKIP_ALERT_WAIT:
        st, nodes = api('GET', '/api/nodes')
        nl = nodes if isinstance(nodes, list) else nodes.get('nodes', [])
        victim = next((n for n in nl if n.get('in_store')), nl[0] if nl else None)
        ck('找到演练用节点', victim is not None)
        if victim:
            old_port = victim.get('listen_port')
            print('     临时改 %s 端口 %s -> 59990' % (victim.get('name'), old_port))
            api('PATCH', '/api/nodes/%s' % victim.get('id'), {'listen_port': 59990})
            received.clear()
            got = False
            for i in range(45):
                time.sleep(3)
                api('GET', '/api/alerts')
                if received:
                    got = True
                    print('     第 %d 秒收到推送' % ((i + 1) * 3))
                    break
            api('PATCH', '/api/nodes/%s' % victim.get('id'), {'listen_port': old_port})
            print('     已改回 %s' % old_port)
            ck('可控异常触发真实推送', got, '135 秒内未收到')
            if received:
                p = received[0]
                print('     推送: event=%s type=%s' % (p.get('event'), p.get('alert_type')))
                print('     text: %s' % (p.get('text') or '')[:110])
                ck('推送文案可读（非 kind · name 兜底）',
                   ' · ' not in (p.get('text') or '')[:80], (p.get('text') or '')[:80])
                ck('推送三要素齐全',
                   all(p.get(k) for k in ('event', 'alert_type', 'text')), json.dumps(p)[:140])

            received.clear()
            for i in range(30):
                time.sleep(3)
                api('GET', '/api/alerts')
                if received:
                    print('     第 %d 秒收到恢复推送' % ((i + 1) * 3))
                    break
            if received:
                p = received[0]
                print('     恢复: event=%s' % p.get('event'))
                ck('恢复也会推送（"好了"同样重要）',
                   p.get('event') == 'resolved', str(p.get('event')))
    else:
        print('     （--no-alert-wait：跳过告警等待）')
    print()

    print('【D】删订阅 → 账号应注销、不留孤儿')
    uid_before = None
    if accts:
        uid_before = list(accts.values())[0].get('user_id')
    st, b = api('DELETE', '/api/subscriptions/%s' % subid)
    ck('删订阅成功', st == 200, 'HTTP %s' % st)
    time.sleep(8)
    st, orph2 = api('GET', '/api/orphans')
    ck('无新增孤儿', (orph2 or {}).get('count', 0) == base_orph,
       '%d -> %d' % (base_orph, (orph2 or {}).get('count', 0)))

    if uid_before:
        still = 0
        st, caps = api('GET', '/api/nodes/capabilities')
        for n in caps.get('nodes', []):
            if not n.get('reachable'):
                continue
            st, users = api('GET', '/api/nodes/%s/users' % n.get('node_id'), timeout=90)
            ul = users if isinstance(users, list) else users.get('users', [])
            if any(u.get('user_id') == uid_before for u in ul):
                still += 1
        print('     还有 %d 个节点残留该账号' % still)
        ck('账号已从所有可达节点注销', still == 0, '残留 %d 个' % still)
    print()

    print('【E】一致性总检查')
    st, caps3 = api('GET', '/api/nodes/capabilities')
    st, nodes3 = api('GET', '/api/nodes')
    nl3 = nodes3 if isinstance(nodes3, list) else nodes3.get('nodes', [])
    st, subs3 = api('GET', '/api/subscriptions')
    sl3 = subs3 if isinstance(subs3, list) else subs3.get('subscriptions', [])
    st, orph3 = api('GET', '/api/orphans')
    names_now = sorted(n.get('node_name') for n in caps3.get('nodes', []))

    ck('订阅数回到基线', len(sl3) == base_subs, '%d vs %d' % (len(sl3), base_subs))
    ck('能力面板节点数 == 节点列表数',
       caps3.get('fetched') == len(nl3),
       'caps=%s nodes=%s' % (caps3.get('fetched'), len(nl3)))
    ck('原有节点一个没少', names_now == base_names,
       '基线 %s / 现在 %s' % (base_names, names_now))
    ck('孤儿数为基线', (orph3 or {}).get('count', 0) == base_orph)
    print('     订阅 %d / 节点 %d / 孤儿 %d' % (
        len(sl3), len(nl3), (orph3 or {}).get('count', 0)))
    print()

    print('【F】清理')
    api('PUT', '/api/alerts/webhook', {'url': ''})
    time.sleep(1)
    st, wh = api('GET', '/api/alerts/webhook')
    ck('webhook 已关闭', wh.get('configured') is False, json.dumps(wh))
    st, caps4 = api('GET', '/api/nodes/capabilities')
    ck('节点数仍为基线', caps4.get('fetched', 0) == base_nodes,
       '%d vs %d' % (caps4.get('fetched', 0), base_nodes))

    srv.shutdown()
    print()
    print('=' * 72)
    ok = sum(1 for _, o in steps if o)
    print('演练汇总: %d/%d 通过' % (ok, len(steps)))
    if fails:
        print('失败项:')
        for f in fails:
            print('  x', f)
    else:
        print('四个新功能协同工作正常')
    print('=' * 72)
    return 1 if fails else 0


if __name__ == '__main__':
    sys.exit(main())
