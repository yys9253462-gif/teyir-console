#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""纯 API 管理 hy2 节点 —— 五轮深度测试。

针对本轮新增的「不依赖 SSH、只用节点侧 API 管理 hy2」这条链路。

五轮各打一个维度，不是把同一个测试跑五遍：
  R1  功能完整性 —— 每个动词/探测是否真的可用（含真实下发的写操作）
  R2  边界与校验 —— 超长输入、非法字符、路径穿越、巨大 body
  R3  并发与竞态 —— 同节点并发写、并发读、令牌过期，看有没有串号/数据错乱
  R4  故障与降级 —— 节点不可达、错凭据、错 token，看错误是否可读且不泄漏
  R5  幂等与安全 —— 危险动作门禁、重复下发、审计留痕

判定原则：不把「返回 200」当通过。要校验响应内容是否自洽、
是否符合声明语义、以及是否会泄漏凭据。

用法（在被测主机本机运行，直连 127.0.0.1:8080）：
  set -a && . ./.env && set +a && python3 portal-5rounds.py
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

API = f"http://127.0.0.1:{os.environ.get('API_PORT', '8080')}"
NODE_OK = os.environ.get("NODE_ID", "00000000-0000-0000-0000-000000000001")
NODE_BARE = os.environ.get("BARE_NODE_ID", "00000000-0000-0000-0000-000000000002")

results = []
round_no = 0


def call(method, path, body=None, token=None, headers=None, raw=False, timeout=90):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(API + path, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            text = resp.read().decode("utf-8", "replace")
            if raw:
                return resp.status, text
            try:
                return resp.status, json.loads(text or "{}")
            except json.JSONDecodeError:
                return resp.status, {"_raw": text[:400]}
    except urllib.error.HTTPError as exc:
        text = exc.read().decode("utf-8", "replace")
        if raw:
            return exc.code, text
        try:
            return exc.code, json.loads(text or "{}")
        except json.JSONDecodeError:
            return exc.code, {"_raw": text[:400]}
    except Exception as exc:  # noqa: BLE001
        return 0, {"_err": str(exc)}


def check(ok, name, detail=""):
    results.append((round_no, bool(ok), name, detail))
    flag = "PASS" if ok else "FAIL"
    print(f"    [{flag}] {name}" + (f"  :: {detail}" if detail else ""))


# 面板的配额这一轮改成了分层：读 60 次 / 10s、写 20 次 / 10s，两桶互相独立。
#
# 脚本必须跟着分开记账，否则两种错法都会制造出「像产品缺陷的假失败」：
#   * 全按 20 记：读操作白白多等好几轮，五轮测试被拖成十几分钟；
#   * 全按 60 记：写操作撞上 429，而 429 看起来和真的限流故障一模一样。
#
# 所以按 HTTP 方法分桶，各自逼近自己的上限时才停。
class _Budget:
    def __init__(self, label, window, limit):
        self.label, self.window, self.limit, self.used = label, window, limit, 0

    def spend(self):
        self.used += 1
        if self.used >= self.limit - 2:
            print(f"    … {self.label}已用 {self.used}/{self.limit}，等待 {self.window}s 让窗口滑过")
            time.sleep(self.window + 1)
            self.used = 0


READ_BUDGET = _Budget("读配额", 10, 60)
WRITE_BUDGET = _Budget("写配额", 10, 20)
READ_METHODS = ("GET", "HEAD", "OPTIONS")


def pace(method):
    if method.upper() in READ_METHODS:
        READ_BUDGET.spend()
    else:
        WRITE_BUDGET.spend()


def cool_down(why="轮间冷却"):
    """等两个窗口都滑过去，把两本账清零。

    并发用例刻意不走 pace()（它要的就是「同时打」），所以它们消耗的配额
    不记账 —— 只能靠这个统一的冷却来保证后续轮次干净。
    """
    window = max(READ_BUDGET.window, WRITE_BUDGET.window)
    print(f"\n  … {why}，等待 {window + 2}s")
    time.sleep(window + 2)
    READ_BUDGET.used = 0
    WRITE_BUDGET.used = 0


def call_paced(method, *args, **kwargs):
    pace(method)
    return call(method, *args, **kwargs)


def login():
    st, body = call("POST", "/api/login", {
        "email": os.environ["SEED_ADMIN_EMAIL"],
        "password": os.environ["SEED_ADMIN_PASSWORD"],
    })
    if st != 200 or "token" not in body:
        print(f"登录失败 HTTP {st} {body}")
        sys.exit(1)
    return body["token"]


# ---------------------------------------------------------------- R1
def round1(token):
    """功能完整性：每个能力是否真的可用。"""
    print("\n  R1 功能完整性")

    st, body = call_paced("GET", f"/api/nodes/{NODE_OK}/overview", token=token)
    check(st == 200, "节点概览可读", f"HTTP {st}")
    if st == 200:
        # overview 的真实字段是 summary/users，不是 users_count —— 早先脚本猜错了。
        check(isinstance(body.get("summary"), dict), "概览带 summary",
              f"keys={sorted(body.get('summary', {}))[:5]}")
        check(isinstance(body.get("users"), list), "概览带用户列表",
              f"{len(body.get('users', []))} 个")

    st, body = call_paced("GET", f"/api/nodes/{NODE_OK}/users", token=token)
    check(st == 200 and "users" in body, "用户列表可用",
          f"count={body.get('count')}" if st == 200 else f"HTTP {st}")
    first_uid = None
    if st == 200 and body.get("users"):
        # 真实的用户标识字段是 user_id。
        first_uid = body["users"][0].get("user_id")
        check(bool(first_uid), "用户条目有可用标识", str(first_uid)[:32])

    st, body = call_paced("GET", f"/api/nodes/{NODE_OK}/proxy-services", token=token)
    check(st == 200 and "services" in body, "代理服务列表可用",
          f"count={body.get('count')}" if st == 200 else f"HTTP {st}")

    # 七路探测
    st, body = call_paced("GET", f"/api/nodes/{NODE_OK}/portal-status", token=token)
    check(st == 200, "状态探测接口可用", f"HTTP {st}")
    probes = body.get("probes", {}) if isinstance(body, dict) else {}
    want = {"version", "bbr", "reality", "warp", "awg", "gost", "speed"}
    got = set(probes)
    check(want <= got, "七路探测齐全", f"缺 {sorted(want - got)}" if want - got else f"{len(got)} 路")
    healthy = [k for k, v in probes.items() if v.get("state") == "healthy"]
    check(len(healthy) >= 5, "多数探测健康", f"{len(healthy)}/{len(probes)} healthy")

    # 写操作：本轮新增了「能力门禁」——节点没装的组件，对应写操作不发过去。
    #
    # 这不是绕开真实写入，而是把一个真实缺陷钉住：节点未装 WARP 时反复
    # 下发 manage-warp，会让节点 portal 断连并短暂扰动网络（连 SSH 都
    # 一度不可达）。所以本环境（WARP 未安装）验证的正是「正确地拒绝」。
    warp_installed = bool((probes.get("warp", {}).get("payload") or {}).get("installed"))
    st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action",
                    {"verb": "warp_reset_rules"}, token=token)
    if warp_installed:
        check(st == 200 and body.get("result", {}).get("ok") is True,
              "已装 WARP 时写操作真实下发成功", f"HTTP {st}")
    else:
        check(st == 409 and body.get("missing_capability") == "warp",
              "未装 WARP 时写操作被前置拦下（能力门禁）",
              f"HTTP {st} cap={body.get('missing_capability')}")
        check(bool(body.get("install_verb")),
              "拒绝时给出可执行的安装指引", f"install_verb={body.get('install_verb')}")

    # 门禁只作用于声明了依赖的动词。无依赖的动词仍走原路径，
    # 由参数校验拒绝 —— 若这里变成 409，说明门禁把范围放大了。
    st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action",
                    {"verb": "set_bbr", "params": {"version": "v9"}}, token=token)
    check(st == 400, "无依赖的动词不经门禁（仍为参数校验 400）", f"HTTP {st}")


# ---------------------------------------------------------------- R2
def round2(token):
    """边界与校验：畸形输入不应 500，更不应打穿到节点。"""
    print("\n  R2 边界与校验")

    st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action", {"verb": ""}, token=token)
    check(st == 400, "空动词被拒", f"HTTP {st}")

    st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action", {"verb": "x" * 5000}, token=token)
    check(st == 400, "超长动词被拒", f"HTTP {st}")

    st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action",
                    {"verb": "warp_add_rule", "params": {"domain": "a" * 5000}}, token=token)
    check(st in (400, 200), "超长域名不 500", f"HTTP {st}")

    bad_domains = ["../../etc/passwd", "a;rm -rf /", "a$(whoami)", "a`id`", "a|cat /etc/shadow",
                   "a\nb", 'a"b', "a'b"]
    for d in bad_domains:
        st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action",
                        {"verb": "warp_add_rule", "params": {"domain": d}}, token=token)
        check(st == 400, f"注入串被拒 {d[:18]!r}", f"HTTP {st}")

    st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action",
                    {"verb": "set_bbr", "params": {"version": "v9"}}, token=token)
    check(st == 400, "非法 BBR 版本被拒", f"HTTP {st}")

    st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action",
                    {"verb": "manage_awg", "params": {"action": "add", "name": "../../evil"}}, token=token)
    check(st == 400, "非法 AWG 客户端名被拒", f"HTTP {st}")

    st, _ = call_paced("GET", f"/api/nodes/{NODE_OK}/portal-status", token=token, raw=True)
    check(st == 200, "畸形请求后接口仍可用", f"HTTP {st}")


# ---------------------------------------------------------------- R3
def round3(token):
    """并发与竞态：同节点并发读写不应串号或崩溃。

    注意：这里刻意不走 call_paced —— 并发测试要的就是「同时打」。
    但正因为如此，它消耗配额最快，跑完必须等窗口滑过去，
    否则后面的轮次会全被 429 淹没（这正是第一版踩的坑）。
    """
    print("\n  R3 并发与竞态")

    def probe(i):
        return call("GET", f"/api/nodes/{NODE_OK}/portal-status", token=token)[0]

    with ThreadPoolExecutor(max_workers=8) as pool:
        codes = list(pool.map(probe, range(8)))
    ok_read = all(c in (200, 429) for c in codes) and codes.count(200) >= 1
    check(ok_read, "8 路并发读不崩（200 或限流）", f"codes={sorted(set(codes))} 200×{codes.count(200)}")

    # 等限流窗口滑过去，让后续断言不被自己拖累
    cool_down("并发读之后")

    def write(i):
        return call("POST", f"/api/nodes/{NODE_OK}/portal-action",
                    {"verb": "warp_reset_rules"}, token=token)[0]

    with ThreadPoolExecutor(max_workers=4) as pool:
        wcodes = list(pool.map(write, range(4)))
    # 修复前这里是 [200, 502]：502 来自节点侧单线程 portal 被并发打崩，
    # 日志可见 unexpected EOF，随后连节点 SSH 都短暂不可达。
    # 门禁生效后并发写不再到达节点，502 应当消失。
    check(all(c in (200, 409, 429) for c in wcodes) and 502 not in wcodes,
          "4 路并发写不产生 502（未打到节点）", f"codes={sorted(set(wcodes))}")

    cool_down("并发写之后")

    st, body = call("GET", f"/api/nodes/{NODE_OK}/portal-status", token=token)
    check(st == 200, "并发后接口自愈", f"HTTP {st}")

    # ---- 配额分层：读不再侵占写 ----
    #
    # 这一轮把配额从一个「读写共用、10s 20 次」的桶拆成了两桶（读 60 / 写 20）。
    # 下面这段就是它的回归测试，且刻意只用**碰不到节点**的请求。
    #
    # ⚠️ 选请求的讲究（第一版就在这里栽了）：portal-action 的执行顺序是
    #     ①JSON 解码 ②动词查表 ③危险动词缺确认 ④★限流 ⑤参数校验（不碰节点）…
    #   配额在第 ④ 步才消耗。所以「{"verb":""}」——最顺手的那个畸形请求 ——
    #   在第 ② 步就 400 了，排在限流器之前，连打一百次也不会触发 429。
    #   用它来"打满写配额"，测出来的永远是"配额没生效"。
    #   能消耗配额又碰不到节点的请求只有一类：过了 ①②③、栽在第 ⑤ 步的。
    #   set_bbr 的 version 只允许白名单里的值，传 "v9" 正好卡在这里。
    #
    # 判别力：这段在旧实现下必然失败 —— 25 次连读会把那个 20 次的老桶打穿，
    # 随后那 3 次写操作全部 429。
    #
    # 为什么这里不断言 429 的响应体：那部分（Retry-After 头、limit_scope、
    # retry_after_seconds、写要说明"未下发"）由两个地方钉住 ——
    # server/internal/httpapi/ratelimit_test.go（确定性单测）和
    # tests/verify-layered-quota.sh（线上一次性验收，那里确实打满了写桶）。
    # 在常规回归里再人为凑一次 429，要多花 60+ 请求和 12 秒冷却，不划算。
    bad_verb = {"verb": "set_bbr", "params": {"version": "v9"}}  # 400，且已越过限流器

    print("    … 连打 25 次只读请求（旧实现下第 21 次起就会 429）")
    read_codes = [call("GET", "/api/subscriptions", token=token)[0] for _ in range(25)]
    check(all(c == 200 for c in read_codes),
          "读配额明显宽于写：25 次连读不被限",
          f"codes={sorted(set(read_codes))} 200×{read_codes.count(200)}")

    write_codes = [
        call("POST", f"/api/nodes/{NODE_OK}/portal-action", bad_verb, token=token)[0]
        for _ in range(3)
    ]
    check(write_codes == [400, 400, 400],
          "读连打 25 次后写配额毫发无损（旧实现下这里全是 429）",
          f"codes={sorted(set(write_codes))}")

    # 错误 token 并发不应拖垮服务。走的是认证失败路径，
    # 不该消耗有效用户的配额。
    def bad(i):
        return call("GET", f"/api/nodes/{NODE_OK}/portal-status", token="bad.token.here")[0]
    with ThreadPoolExecutor(max_workers=6) as pool:
        bcodes = list(pool.map(bad, range(6)))
    check(all(c == 401 for c in bcodes), "错误 token 一律 401", f"codes={sorted(set(bcodes))}")

    time.sleep(2)
    st, body = call("GET", f"/api/nodes/{NODE_OK}/portal-status", token=token)
    check(st == 200, "无效请求后正常 token 仍可用", f"HTTP {st}")


# ---------------------------------------------------------------- R4
def round4(token):
    """故障与降级：错误必须可读、可行动，且不泄漏凭据。"""
    print("\n  R4 故障与降级")

    st, body = call_paced("GET", f"/api/nodes/{NODE_BARE}/portal-status", token=token)
    check(st == 400, "未绑 API 节点返回 400", f"HTTP {st}")
    msg = body.get("error", "") if isinstance(body, dict) else ""
    check("绑定" in msg, "错误文案给出下一步动作", msg[:60])

    st, body = call_paced("GET", "/api/nodes/00000000-0000-0000-0000-000000000000/portal-status",
                    token=token)
    check(st in (400, 404), "不存在的节点被拒", f"HTTP {st}")

    st, body = call_paced("GET", "/api/nodes/not-a-uuid/portal-status", token=token)
    check(st in (400, 404), "非法 UUID 被拒", f"HTTP {st}")

    st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action",
                    {"verb": "manage_proxy", "params": {"action": "add", "type": "bogus"}},
                    token=token)
    check(st == 400, "非法代理类型被拒", f"HTTP {st}")

    # 凭据泄漏检查：任何响应里都不该出现 Basic 密码 / api_key 明文
    leaks = []
    for path in [f"/api/nodes", f"/api/nodes/{NODE_OK}/portal-status"]:
        st, text = call_paced("GET", path, token=token, raw=True)
        for needle in ["portal_basic_password", "Basic ", "api_key"]:
            if needle in text:
                leaks.append(f"{path}:{needle}")
    check(not leaks, "响应不泄漏凭据字段", str(leaks) if leaks else "无")


# ---------------------------------------------------------------- R5
def round5(token):
    """幂等与安全：危险动作门禁、重复下发、审计留痕。"""
    print("\n  R5 幂等与安全")

    # 只发「不带确认头」的请求：这会在下发到节点之前就被 428 拦下，
    # 请求头里也带有 dangerous 标记，足以验证门禁存在。
    #
    # ⚠️ 不要在这里补测「带上确认头后会放行」—— 那等于真的执行重启/升级。
    # 早先版本这么做过，审计里留下了真实的 portal_reboot 记录。
    # 「确认后放行」属于无法在集成测试里安全验证的行为，交给单测覆盖其
    # 配置（动词是否被标记 Dangerous），而不是真的打出去。
    for verb in ["reboot", "upgrade_core", "upgrade_portal", "reality_reset"]:
        st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action",
                        {"verb": verb}, token=token)
        check(st == 428 and body.get("dangerous") is True,
              f"危险动词 {verb} 需二次确认且带危险标记",
              f"HTTP {st} dangerous={body.get('dangerous')}")

    # 上面那条断言已经覆盖了「危险动作被拦」的全部分支；
    # 不在这里追加任何会真正执行动作的用例（原因见上）。

    # 门禁的拒绝必须稳定：重复请求不能时好时坏，更不能因为重试
    # 而悄悄把请求放到节点上去。这是门禁能不能被信任的关键。
    codes = []
    for _ in range(3):
        st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action",
                              {"verb": "warp_reset_rules"}, token=token)
        codes.append(st)
    check(set(codes) == {409}, "门禁拒绝稳定不抖动（3 次一致）", f"codes={codes}")

    # 参数校验先于门禁：畸形输入在任何节点状态下都该是 400，
    # 而不是「因为没装 WARP 所以 409」——后者会让操作者误以为
    # 装上 WARP 就能用非法域名。
    st, body = call_paced("POST", f"/api/nodes/{NODE_OK}/portal-action",
                    {"verb": "warp_add_rule", "params": {"domain": "bad\nname"}},
                    token=token)
    check(st == 400, "畸形参数优先返回 400（不被门禁掩盖）", f"HTTP {st}")

    st, body = call_paced("GET", "/api/audit", token=token)
    check(st == 200, "审计接口可读", f"HTTP {st}")
    txt = json.dumps(body, ensure_ascii=False) if isinstance(body, (dict, list)) else str(body)
    check("portal" in txt or "节点" in txt, "审计记录了 portal 操作", "已留痕" if "portal" in txt else "未见关键字")

    # 审计里不该有明文口令
    check("portal_basic_password" not in txt, "审计不含凭据字段名",
          "干净" if "portal_basic_password" not in txt else "发现泄漏")


def main():
    global round_no
    token = login()
    print("=" * 72)
    print(f"纯 API 管理 hy2 节点 · 五轮测试   {time.strftime('%Y-%m-%d %H:%M:%S')}")
    print("=" * 72)

    for i, fn in enumerate((round1, round2, round3, round4, round5), start=1):
        round_no = i
        if i > 1:
            # 轮间冷却。不等的话后一轮会拿到 429，看着像产品坏了，
            # 其实是测试把自己打限流了 —— 第一版就在这里栽过。
            cool_down(f"轮间冷却（进 R{i} 前）")
        try:
            fn(token)
        except Exception as exc:  # noqa: BLE001
            check(False, f"R{i} 执行异常", str(exc)[:120])

    print("\n" + "=" * 72)
    fails = [r for r in results if not r[1]]
    for rnd in range(1, 6):
        rs = [r for r in results if r[0] == rnd]
        if rs:
            bad = [r for r in rs if not r[1]]
            print(f"  R{rnd}: {len(rs) - len(bad)}/{len(rs)} 通过"
                  + ("" if not bad else f"   失败 → {[b[2] for b in bad]}"))
    print(f"\n  合计 {len(results) - len(fails)}/{len(results)} 通过")
    print("=" * 72)
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
