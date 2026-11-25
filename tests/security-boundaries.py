#!/usr/bin/env python3
"""
第 3 轮：越权与安全边界。

这套面板对外暴露两类凭证入口：
  * /sub/<token> 与 /api/sub/view/<token> —— 公开，令牌本身就是凭证
  * /api/store/*  —— 商城用共享密钥
  * /api/* 其余   —— 操作员 JWT

本轮把「能不能用别人的凭证/绕过凭证」这件事逐条试一遍。

覆盖：
  A. 令牌边界：太短/太长/含空格含斜杠 -> 一律 404，且不能成为存在性oracle
  B. JWT 越权：无 token / 伪造 token / 篡改签名 / alg=none / 过期
  C. 商城密钥：错密钥、缺密钥、前缀正确但尾部错、空密钥
  D. 凭证泄漏面：任何公开端点都不应吐出 password / obfs-password / api_key
  E. 路径注入：令牌里塞 ../ 、%2F 、null 字节
  F. 过期与撤销的订阅不得因任何参数而放行
"""
import argparse
import base64
import json
import time
import urllib.error
import urllib.request
import urllib.parse


def call(base, path, method="GET", body=None, headers=None):
    hdrs = {"Accept": "*/*"}
    if body is not None:
        hdrs["Content-Type"] = "application/json"
    if headers:
        hdrs.update(headers)
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(base + path, data=data, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:                                       # noqa: BLE001
        return 0, f"<transport> {e}"[:200]


def store_post(base, key, path, body):
    return call(base, path, "POST", body, {"X-Store-Key": key} if key else None)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--store-key", required=True)
    ap.add_argument("--json-out", default="")
    args = ap.parse_args()
    base, key = args.base, args.store_key
    cases = []

    def check(label, ok, detail=""):
        cases.append((label, bool(ok), str(detail)[:200]))

    # 备一条有效订阅
    oid = f"SEC-{int(time.time())}"
    st, txt = store_post(base, key, "/api/store/orders",
                         {"order_id": oid, "label": "安全边界", "duration_days": 30, "ip_limit": 5})
    if st != 200:
        print(f"建订阅失败 {st} {txt[:120]}")
        return 1
    tok = json.loads(txt)["token"]

    # ---------------- A. 令牌边界 ----------------
    bad_tokens = [
        ("", "空令牌"),
        ("short", "过短令牌"),
        ("x" * 19, "19 字符（下限下）"),
        ("x" * 129, "129 字符（上限上）"),
        ("x" * 64 + "%20", "尾部编码空格"),
        ("x" * 32 + "/extra", "含斜杠"),
    ]
    for t, label in bad_tokens:
        st1, b1 = call(base, f"/api/sub/view/{t}")
        st2, b2 = call(base, f"/sub/{t}")
        check(f"令牌边界：{label} -> 视图 404", st1 == 404, f"status={st1}")
        check(f"令牌边界：{label} -> 订阅源 404", st2 == 404, f"status={st2}")

    # 未知但格式合法的令牌，与格式非法的令牌，响应必须一致（不给存在性 oracle）
    st_unknown, b_unknown = call(base, "/api/sub/view/" + "z" * 43)
    st_malformed, b_malformed = call(base, "/api/sub/view/short")
    check("存在性 oracle：格式合法但不存在 == 格式非法（响应一致）",
          st_unknown == st_malformed == 404, f"{st_unknown} vs {st_malformed}")

    # ---------------- B. JWT 越权 ----------------
    protected = [
        ("GET", "/api/nodes"),
        ("GET", "/api/subscriptions"),
        ("GET", "/api/users"),
        ("GET", "/api/audit"),
        ("GET", "/api/servers"),
        ("GET", "/api/subscription"),
    ]
    for method, path in protected:
        st, _ = call(base, path, method)
        check(f"JWT：无 token 访问 {path} -> 401", st == 401, f"status={st}")

        st, _ = call(base, path, method, headers={"Authorization": "Bearer not-a-jwt"})
        check(f"JWT：垃圾 token 访问 {path} -> 401", st == 401, f"status={st}")

        # alg=none 篡改：把 header 改成 {"alg":"none"}，payload 伪造 admin
        hdr = base64.urlsafe_b64encode(json.dumps({"alg": "none", "typ": "JWT"}).encode()).rstrip(b"=")
        pl = base64.urlsafe_b64encode(json.dumps(
            {"sub": "x", "role": "admin", "exp": int(time.time()) + 3600}).encode()).rstrip(b"=")
        forged = f"{hdr.decode()}.{pl.decode()}."
        st, _ = call(base, path, method, headers={"Authorization": f"Bearer {forged}"})
        check(f"JWT：alg=none 伪造 admin 访问 {path} -> 401", st == 401, f"status={st}")

        # 签名被篡改
        st, _ = call(base, path, method,
                     headers={"Authorization": "Bearer " + forged[:-1] + "AAAA"})
        check(f"JWT：篡改签名访问 {path} -> 401", st == 401, f"status={st}")

    # 用真实登录拿到合法 token，再试越权到 admin-only 端点
    import os
    email = os.environ.get("NODEOPS_ADMIN_EMAIL", "")
    password = os.environ.get("NODEOPS_ADMIN_PASSWORD", "")
    if email and password:
        st, txt = call(base, "/api/login", "POST", {"email": email, "password": password})
        if st == 200:
            real = json.loads(txt).get("token")
            st, _ = call(base, "/api/nodes", headers={"Authorization": f"Bearer {real}"})
            check("JWT：合法 token 可访问受保护端点", st == 200, f"status={st}")
        else:
            check("JWT：管理员登录成功", False, f"status={st} {txt[:100]}")

    # ---------------- C. 商城密钥 ----------------
    st, _ = store_post(base, None, "/api/store/orders", {"order_id": "X", "duration_days": 1})
    check("商城密钥：完全不带密钥 -> 401", st == 401, f"status={st}")

    st, _ = store_post(base, "wrong-key", "/api/store/orders", {"order_id": "X", "duration_days": 1})
    check("商城密钥：错误密钥 -> 401", st == 401, f"status={st}")

    # 前缀对但尾部错（考验是否做了完整比较）
    near = key[:-2] + ("aa" if not key.endswith("aa") else "bb")
    st, _ = store_post(base, near, "/api/store/orders", {"order_id": "X", "duration_days": 1})
    check("商城密钥：前缀正确尾部错误 -> 401", st == 401, f"status={st}")

    st, _ = store_post(base, key + "extra", "/api/store/orders", {"order_id": "X", "duration_days": 1})
    check("商城密钥：正确密钥多尾巴 -> 401", st == 401, f"status={st}")

    st, _ = store_post(base, "", "/api/store/orders", {"order_id": "X", "duration_days": 1})
    check("商城密钥：空字符串 -> 401", st == 401, f"status={st}")

    # Authorization: Bearer 形式也应被接受
    st, _ = store_post(base, None, "/api/store/orders",
                       {"order_id": f"SEC-BEARER-{int(time.time())}", "duration_days": 1})
    st2, _ = call(base, "/api/store/orders", "POST",
                  {"order_id": f"SEC-BEARER2-{int(time.time())}", "duration_days": 1},
                  {"Authorization": f"Bearer {key}"})
    check("商城密钥：支持 Authorization: Bearer 形式", st2 in (200, 201), f"status={st2}")

    # 商城密钥不能用来访问操作员端点
    st, _ = call(base, "/api/nodes", headers={"X-Store-Key": key})
    check("商城密钥：不能访问操作员端点 /api/nodes -> 401", st == 401, f"status={st}")
    st, _ = call(base, "/api/subscriptions", headers={"X-Store-Key": key})
    check("商城密钥：不能访问 /api/subscriptions -> 401", st == 401, f"status={st}")

    # ---------------- D. 凭证泄漏面 ----------------
    # 公开端点绝不该吐出口令
    leak_terms = ["obfs-password", "auth_password", "api_key", "CREDENTIAL_KEY",
                  "JWT_SECRET", "POSTGRES_PASSWORD"]
    safe_pub = [
        f"/api/sub/view/{tok}",
        "/api/health",
    ]
    for p in safe_pub:
        st, body = call(base, p)
        for term in leak_terms:
            check(f"泄漏面：{p} 不含 {term}", term not in body, body[:120])

    # /sub 是凭证本身，必须含口令（客户要连）；但不能含**节点侧的管理密钥**
    st, feed = call(base, f"/sub/{tok}?format=clash")
    check("泄漏面：/sub 不含节点侧 api_key", "api_key" not in feed, feed[:120])

    # ---------------- E. 路径注入 ----------------
    inject = [
        ("..%2F..%2Fetc%2Fpasswd", "路径穿越"),
        ("%2e%2e%2f%2e%2e%2fapi%2fnodes", "编码穿越到受保护端点"),
        ("valid" + "%00" + "tail", "null 字节截断"),
        ("%0d%0aX-Injected:%20yes", "CRLF 注入"),
    ]
    for t, label in inject:
        st, body = call(base, f"/api/sub/view/{t}")
        check(f"注入：{label} -> 404", st == 404, f"status={st}")
        check(f"注入：{label} 未产生注入头/越权",
              st != 200 and "Injected" not in body, body[:100])

    # ---------------- F. 失效订阅不能靠参数放行 ----------------
    #
    # 这里要测的是「过期/撤销之后，加任何参数都不能放行」。关键前提是那条
    # 订阅**还在**：宽限期为 0 且 auto_delete 缺省 true 时，过期订阅第一次
    # 被读就被回收，返回 404 而不是 403 —— 那就变成在测「不认识」，测不出
    # 「已失效但拒绝服务」这条规则。故显式 auto_delete=false 保留它。
    oid_exp = f"SEC-EXP-{int(time.time())}"
    st, txt = store_post(base, key, "/api/store/orders",
                         {"order_id": oid_exp, "label": "安全-已过期",
                          "expires_at": int(time.time()) - 86400, "ip_limit": 0,
                          "auto_delete": False})
    tok_exp = json.loads(txt)["token"]
    for q in ["", "?format=clash", "?format=singbox", "?format=uri", "?format=clash&force=1"]:
        st, _ = call(base, f"/sub/{tok_exp}{q}")
        check(f"失效订阅：过期 + '{q or '无参数'}' -> 403", st == 403, f"status={st}")

    # 撤销的订阅同理
    oid_rev = f"SEC-REV-{int(time.time())}"
    st, txt = store_post(base, key, "/api/store/orders",
                         {"order_id": oid_rev, "label": "安全-已撤销", "duration_days": 30})
    tok_rev = json.loads(txt)["token"]
    store_post(base, key, "/api/store/revoke", {"order_id": oid_rev})
    st, _ = call(base, f"/sub/{tok_rev}")
    check("失效订阅：撤销后 /sub -> 403", st == 403, f"status={st}")
    st, _ = call(base, f"/sub/{tok_rev}?format=clash")
    check("失效订阅：撤销后带 format 仍 403", st == 403, f"status={st}")
    st, body = call(base, f"/api/sub/view/{tok_rev}")
    check("失效订阅：撤销后视图可读但不给链接",
          st == 200 and "subscription_url" not in body, f"status={st} {body[:120]}")

    total = len(cases)
    passed = sum(1 for _, ok, _ in cases if ok)
    print(f"第 3 轮（越权与安全边界）  {passed}/{total} 通过")
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
    raise SystemExit(main())
