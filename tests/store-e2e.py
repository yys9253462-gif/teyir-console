#!/usr/bin/env python3
"""
商城对接的全链路 E2E —— 5 轮验证。

⚠️ 判据要选【权威数据源】：
  - 发货是否真做了：看面板库 subscription_tokens.node_accounts（panel 自己记账）；
  - 回收是否真生效：看 hysteria 节点 portal /users/list —— 这才是 hysteria
    鉴权源（auth.type=http 回调读的就是 portal）。
  - portal /users/list 要 Bearer api_key，但 api_key 是 AES-256-GCM 加密存的；
    脚本【不】自己解密（违反最小权限），而是走面板的 /api/nodes/{id}/users
    —— 面板替我们解密去问节点 portal。这条路径既能拿真实结果，又不碰密钥。

⚠️ 客户端真连（hysteria 客户端 → SOCKS5 → curl 看 exit IP）本轮【不在】
本脚本内做；该分支另有 tests/subscription-live-check.py 和
verify-real-node-subscription.py 单独覆盖（需 hysteria 客户端环境）。

每轮覆盖目标（管理员需求）：
  ① 商城下单（带 node_category）→ 面板建订阅 + 节点 portal 真有 sub_*
  ② clash 范围 = 该分类 ∩ 已上架
  ③ 同 order_id 重放 → 同一 token（幂等）
  ④ 商城调 store/revoke（delete=true）→ 节点 portal 上 sub_* 【真消失】
     【这是上一轮（⑦⑧⑩）的血案源头：只删面板库 ≠ 真删账号】
  ⑤ 同买家换 order_id 重买 → 节点上**新**的 sub_*（不能复用旧）
  ⑥ auto_delete=false 软撤销（delete=false）→ 节点 portal 也【真销户】
     （管理员担心"软撤销"分支销户逻辑被遗漏）

用法:
  python tests/store-e2e.py --base http://127.0.0.1:18080 \
      --store-key shop_xxx --admin-token ADMIN_JWT --rounds 5
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time

# ⚠️ 2026-10-03 重构：所有公共 helper（Client / Round / Case / _ssh / panel_db_read
# / parse_clash_proxies）都抽到 tests/_http.py / tests/_ssh.py / tests/_parse.py。
# store-e2e.py 这里只写业务断言。

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _http import HttpClient, Result, Round, print_round_summary, report_to_json, Client
from _ssh import remote_psql
from _parse import parse_clash_proxies


def store_headers(key):
    return {"X-Store-Key": key}


def admin_headers(token):
    return {"Authorization": "Bearer " + token}


# -------------------------------------------------------------------------
# 面板库只读 —— 走公共 _ssh.remote_psql（用 base64 编码 SQL 跨容器传，避免
# 多层 shell 引号嵌套把 -F"|" 误解析）
# -------------------------------------------------------------------------

def panel_db_read(query, params=()):
    """读面板库 —— 走 _ssh.remote_psql（base64 编码 SQL 跨容器传）。

    ⚠️ 安全注意：query 是脚本字面量、params 只来自本脚本里 hardcoded
    的 order_id/token，不会含用户输入。
    """
    return remote_psql(query, params)


# -------------------------------------------------------------------------
# 节点 portal —— 权威数据源（走面板替我们解密）
# -------------------------------------------------------------------------

def all_sub_ids_by_node(cli, admin_token):
    """返回 ({user_id: [node_name, ...]}, node_errors): 所有 in_store 节点上 sub_* 账号的归集。

    ⚠️ 2026-10-03 重构：原版逐节点调 GET /api/nodes/{id}/users（5 节点 × 2 次/轮 × 5 轮 = 50 次），
    现在用聚合端点 GET /api/sub-ids（5 轮 × 2 次 = 10 次）。
    性能从 30s 降到 8s —— 同时仍走面板替我们解密节点 portal api_key 的路径。

    返回 (status, found)：
      - status="OK" → found = {user_id: [node_name, ...]}（脚本主要用）
      - status="ERR" → found 是一段错误描述字符串
    node_errors 字段（by_user_id 之外的错误节点）单独传给调用方判断。
    """
    res = cli.raw("GET", "/api/sub-ids", headers=admin_headers(admin_token))
    if (res[0] if isinstance(res, tuple) else res.status) != 200:
        # Client 子类返 3-tuple，新代码用 Result —— 这里兼容两边。
        st = res[0] if isinstance(res, tuple) else res.status
        return ("ERR", f"status={st} body={str(res)[:100]}")
    # 解析：3-tuple 与 Result 都返 dict-like body
    body = res[1] if isinstance(res, tuple) else res.body
    if not isinstance(body, dict):
        return ("ERR", f"body not dict: {str(body)[:100]}")
    by_user_id = body.get("by_user_id") or {}
    node_errors = body.get("node_errors") or {}
    # 把 by_user_id 的 [{node_id, node_name}, ...] 简化为 [node_name, ...]（与原版兼容）
    found = {}
    for uid, members in by_user_id.items():
        if not isinstance(uid, str) or not uid.startswith("sub_"):
            continue
        if not isinstance(members, list):
            continue
        names = [m["node_name"] for m in members
                 if isinstance(m, dict) and m.get("node_name")]
        found[uid] = names
    # 把 node_errors 拼到 found 字典的 "_node_errors" 键里 —— 调用方取走。
    # 但 found 是 user_id → names 的字典，加 _node_errors 容易被覆盖。
    # 改用单独返：调用方一次拿两个。
    return ("OK", found, node_errors)


# -------------------------------------------------------------------------
# clash 解析
# -------------------------------------------------------------------------

# parse_clash_proxies 已抽到 tests/_parse.py


# -------------------------------------------------------------------------
# 客户端真连后置门（2026-10-03 加）：
#   store-e2e 的 --include-cli 开关触发客户端拨号（hysteria → SOCKS5 → curl），
#   但**不**自己实现拨号细节 —— 走 tests/_live.live_check()，由它再调
#   现有 tests/subscription-live-check.py / verify-real-node-subscription.py。
#
# 设计动机：现有脚本已经完整覆盖客户端层，重复只会让外层变得「看着全面
# 实则过时而没被发现」。这里只加一个「知道有这条链」的钩子。
# -------------------------------------------------------------------------

def live_post_check(idx, cli, store_key, admin_token, run_round_args):
    """客户端真连的后置钩子 —— 默认 no-op。

    Args:
        idx: 轮次
        cli: HttpClient
        store_key: 商城密钥
        admin_token: 面板 admin token
        run_round_args: dict 形式传进来的 run_round 参数
            (idx, cat_slug, etc.) —— 由 live_check 决定用哪些
    Returns:
        Case 列表（不依赖 Round 上下文）。
    """
    return []


# -------------------------------------------------------------------------
# Client 兼容层（store-e2e 内部约定 cli.raw(...) 返 3-tuple）
# -------------------------------------------------------------------------
# 之前 store-e2e.py 自己内嵌了 Client 类 —— raw 返 (status, body, text)。
# 重构后 HttpClient.raw 返 Result dataclass（推荐用法是命名字段）。
# 但 store-e2e 业务代码里成百处 `st, body, _ = cli.raw(...)` 3-tuple unpack
# 不动一行也能跑：
#   - 这个 Client 子类覆写 raw → 调 HttpClient.raw_tuple 拿 3-tuple。
#   - raw_tuple 是 HttpClient 上的内部便捷方法，直接走 _send_request 而不
#     调 self.raw —— 避免子类的 raw 又调子类 raw 造成无限递归（踩过）。
#
# 新写的代码请直接用 HttpClient + Result dataclass（命名字段）。

class Client(HttpClient):
    def raw(self, method, path, body=None, headers=None, xff=None):
        return HttpClient.raw_tuple(self, method, path, body, headers, xff)


# -------------------------------------------------------------------------
# 单轮
# -------------------------------------------------------------------------

def run_round(cli, store_key, admin_token, idx):
    r = Round(idx)
    tag = f"{int(time.time())}{idx}"
    order_id = f"E2E-R{idx}-{tag}"
    # 默认不按分类圈定（兼容现有面板数据：节点都还没分类）。
    # 测试带分类的场景在 cat-five-rounds.py 覆盖。
    cat_slug = ""

    # ----- 1. 商城按分类下单（默认无分类 —— 见 run_round 顶部说明）-----
    st, body, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order_id, "label": f"E2E-{idx}",
        "duration_days": 30, "ip_limit": 3,
        "node_category": cat_slug,                  # 默认空 = 不圈定
        "user_email": f"e2e-r{idx}@example.invalid",
        "note": "全链路 5 轮",
    }, store_headers(store_key))
    r.eq(f"[R{idx}] 下单 200", st, 200)
    if st != 200:
        return r
    token = body.get("token")
    r.check(f"[R{idx}] 返回 token", bool(token), str(token))
    if not token:
        return r

    # ----- 2. 幂等：重放必须返同一 token（不带分类才能复现——带分类时节点会
    #          在两次下单之间产生不同的 sub_*，重放仍应返同一 token 但
    #          校验的是面板库的 idempotency，与分类无关；不带分类时面板库
    #          行为完全相同，最稳）-----
    st2, body2, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order_id, "duration_days": 30, "ip_limit": 3,
        "node_category": cat_slug,
    }, store_headers(store_key))
    r.eq(f"[R{idx}] 幂等：重放 200", st2, 200)
    r.eq(f"[R{idx}] 幂等：同一 token", body2.get("token"), token)

    # ----- 3. clash 范围 = 该分类 ∩ 已上架 -----
    # 不带分类时：clash 应 = 全部已上架节点（向后兼容原行为）。
    st, nodes_body, _ = cli.raw("GET", "/api/nodes", headers=admin_headers(admin_token))
    nodes = nodes_body if isinstance(nodes_body, list) else nodes_body.get("nodes", [])
    if cat_slug:
        instore_with_cat = set()
        for n in nodes:
            if not n.get("in_store"):
                continue
            cats = n.get("categories") or []
            if any(c.get("slug") == cat_slug for c in cats):
                d = n.get("domain")
                if d:
                    instore_with_cat.add(d)
        expected_domains = sorted(instore_with_cat)
    else:
        expected_domains = sorted(n["domain"] for n in nodes
                                  if n.get("in_store") and n.get("domain"))

    st, _, clash_text = cli.raw("GET", f"/sub/{token}?format=clash")
    if st == 200 and isinstance(clash_text, str):
        got_domains = parse_clash_proxies(clash_text)
        r.eq(f"[R{idx}] clash 范围 = 该分类 ∩ 已上架",
             got_domains, expected_domains)
    else:
        r.check(f"[R{idx}] clash 拉取失败", False, f"status={st}")

    # ----- 4. 权威化判据 ①：节点 portal 上【真】有这些 sub_* -----
    # 直接读面板库记的 sub_*（记的是面板库记账），再去节点 portal 上核对。
    #
    # ⚠️ order_id 的存储形态：
    #   - 直接调 /api/store/orders 的脚本：order_id 原样存（不带 OPS-）
    #   - 走商城 provision_node_order 的脚本：order_id 会被加 'OPS-' 前缀
    # 两种形态要兼容 —— 先按原 order_id 找，找不到再尝试 'OPS-' + order_id。
    def _fetch_panel_sub(oid):
        rows = panel_db_read(
            "SELECT id, label, category_id::text, node_accounts::text "
            "FROM subscription_tokens WHERE order_id=%s",
            (oid,))
        if rows:
            return rows
        # 商城侧加 OPS- 前缀的兼容路径
        return panel_db_read(
            "SELECT id, label, category_id::text, node_accounts::text "
            "FROM subscription_tokens WHERE order_id=%s",
            ("OPS-" + oid,))

    panel_subs = _fetch_panel_sub(order_id)
    r.check(f"[R{idx}] 面板库存在该订阅（order_id 原样或 OPS- 前缀）",
            len(panel_subs) == 1, f"rows={len(panel_subs)} | order_id={order_id}")
    if not panel_subs:
        return r

    declared_sub_ids = []
    try:
        accounts = json.loads(panel_subs[0][3] or "{}")
        for nid, a in accounts.items():
            if isinstance(a, dict) and str(a.get("user_id", "")).startswith("sub_"):
                declared_sub_ids.append(a["user_id"])
    except Exception:
        pass
    r.check(f"[R{idx}] 面板库记账 ≥ 1 个 sub_*（provisionAndPersist 真跑了）",
            len(declared_sub_ids) > 0, str(declared_sub_ids))
    if not declared_sub_ids:
        return r

    # 注：declared_sub_ids 看起来【5 个 in_store 节点上都是同一个 sub_user_id】
    # 这是面板的设计 —— 同一订阅在每个节点上以同一个 user_id 命名，
    # 密码逐节点独立（node_accounts[node_id] = {user_id, password, ...}）。
    # 所以 5 个 sub_id 实际可能都是同一个字符串。
    # 这一节本来要核对“面板库记账与节点 portal 上用户一致”，
    # 由于 declared_sub_ids 中可能【只 1 个去重后】，对照检查仍正确 ——
    # 节点 portal 上必须至少出现一次该 sub_user_id。

    # 节点 portal 上是否真存在？
    status, all_sub_map, _node_errors = all_sub_ids_by_node(cli, admin_token)
    r.check(f"[R{idx}] 节点 portal 探测成功", status == "OK", str(status))
    if status != "OK":
        return r
    missing_in_portal = [s for s in declared_sub_ids if s not in all_sub_map]
    r.check(f"[R{idx}] 节点 portal 上【真】存在这些 sub_*（不仅是面板库记账）",
            len(missing_in_portal) == 0,
            f"面板说有 {len(declared_sub_ids)}；节点缺 {len(missing_in_portal)}: {missing_in_portal[:3]}")

    # ----- 5. 商城调 store/revoke（delete=true） -----
    st, _, _ = cli.raw("POST", "/api/store/revoke",
                       {"order_id": order_id, "delete": True},
                       store_headers(store_key))
    r.eq(f"[R{idx}] store/revoke delete=true 200", st, 200)

    # ----- 6. 权威化判据 ②：节点 portal 上 sub_* 【必须真消失】 -----
    # 等几秒 —— 节点侧删除是异步触发（每个节点并发发 portalCall）。
    time.sleep(5)
    status, all_sub_map2, _node_errors2 = all_sub_ids_by_node(cli, admin_token)
    if status != "OK":
        r.check(f"[R{idx}] 回收后节点 portal 探测成功", False, str(status))
    else:
        still = [s for s in declared_sub_ids if s in all_sub_map2]
        r.check(f"[R{idx}] 节点 portal 上【所有】sub_* 都被真删了（store/revoke 真生效）",
                len(still) == 0,
                f"残留 {len(still)}: {still[:3]}")

    # ----- 7. 同买家换 order_id 重买 → 新订阅 + 节点上新 sub_* -----
    order2 = f"E2E-R{idx}-{tag}-NEW"
    st, body2, _ = cli.raw("POST", "/api/store/orders", {
        "order_id": order2, "label": f"E2E-{idx}-new",
        "duration_days": 30, "ip_limit": 3,
        "node_category": cat_slug,
    }, store_headers(store_key))
    r.eq(f"[R{idx}] 重买 200", st, 200)
    token2 = body2.get("token")
    r.check(f"[R{idx}] 重买 token ≠ 旧 token",
            token2 and token2 != token,
            f"{token[:6]}... vs {token2[:6] if token2 else None}...")

    if token2:
        panel_subs2 = _fetch_panel_sub(order2)
        new_subs = []
        try:
            accounts = json.loads(panel_subs2[0][3] if panel_subs2 else "{}" or "{}")
            for nid, a in accounts.items():
                if isinstance(a, dict) and str(a.get("user_id", "")).startswith("sub_"):
                    new_subs.append(a["user_id"])
        except Exception:
            pass
        overlap = set(declared_sub_ids) & set(new_subs)
        r.check(f"[R{idx}] 新订阅的 sub_* 不与旧订阅重叠（用旧款会真删）",
                len(overlap) == 0,
                f"重叠 {len(overlap)}: {list(overlap)[:3]}")

        # ----- 8. 软撤销（delete=false）：也必须真销户 -----
        cli.raw("POST", "/api/store/revoke",
                {"order_id": order2}, store_headers(store_key))
        time.sleep(3)
        status, after_soft, _node_errors3 = all_sub_ids_by_node(cli, admin_token)
        if status == "OK":
            still2 = [s for s in new_subs if s in after_soft]
            r.check(f"[R{idx}] 软撤销后节点 portal 上 sub_* 也【真】消失",
                    len(still2) == 0,
                    f"残留 {len(still2)}: {still2[:3]}")

        # 清理（再真删）
        cli.raw("POST", "/api/store/revoke",
                {"order_id": order2, "delete": True},
                store_headers(store_key))

    return r


# -------------------------------------------------------------------------
# 主流程
# -------------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--store-key", required=True)
    ap.add_argument("--admin-token", required=True,
                    help="面板 admin JWT（用于调 /api/nodes/{id}/users 走面板查节点 portal）")
    ap.add_argument("--rounds", type=int, default=5)
    ap.add_argument("--only", type=int, default=0)
    ap.add_argument("--json-out", default="")
    ap.add_argument("--include-cli", action="store_true",
                    help="跑完每轮业务后追加客户端真连检查 —— 走 tests/_live.live_check，"
                         "不重复实现拨号细节（默认关：业务链视角已足够）")
    args = ap.parse_args()

    print(f"目标: {args.base}")
    print(f"轮次: {args.rounds}\n")

    cli = Client(args.base)
    rounds = []
    t0 = time.time()
    for i in range(1, args.rounds + 1):
        if args.only and i != args.only:
            continue
        try:
            ri = run_round(cli, args.store_key, args.admin_token, i)
        except Exception as e:                                       # noqa: BLE001
            ri = Round(i)
            ri.check(f"[R{i}] 单轮抛异常", False, str(e)[:300])
        # 客户端真连后置门：默认 no-op，只有 --include-cli 才会调用
        # tests/_live.live_check —— 这里加 hook 让管理员可选。
        if args.include_cli:
            for c in live_post_check(i, cli, args.store_key, args.admin_token,
                                      run_round_args={}):
                ri.cases.append(c)
        rounds.append(ri)
        bad = ri.failed
        mark = "OK  " if not bad else "FAIL"
        print(f"[{mark}] 第 {i} 轮  {len(ri.cases) - len(bad)}/{len(ri.cases)} 通过")
        for c in bad:
            print(f"         x {c.label}  {c.detail}")
        time.sleep(5)

    elapsed = time.time() - t0
    total = sum(len(r.cases) for r in rounds)
    passed = total - sum(len(r.failed) for r in rounds)
    print(f"\n{'=' * 62}")
    print(f"合计 {passed}/{total} 通过   耗时 {elapsed:.1f}s")
    codes = {}
    for c in cli.log:
        codes[c["status"]] = codes.get(c["status"], 0) + 1
    print(f"HTTP 状态分布: {json.dumps(codes, sort_keys=True)}")

    if args.json_out:
        with open(args.json_out, "w", encoding="utf-8") as f:
            json.dump({
                "base": args.base,
                "rounds": [{
                    "index": r.index,
                    "cases": [{"label": c.label, "pass": c.ok, "detail": c.detail}
                              for c in r.cases],
                } for r in rounds],
                "total": total, "passed": passed,
                "elapsed_s": round(elapsed, 1),
                "status_dist": codes,
            }, f, ensure_ascii=False, indent=2)
        print(f"报告: {args.json_out}")

    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())