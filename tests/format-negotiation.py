#!/usr/bin/env python3
"""
第 1 轮：订阅格式协商与渲染正确性。

这是真实客户端的入口路径 —— Clash/Mihomo/Shadowrocket/sing-box 都不带 ?format=，
而是靠 User-Agent 被识别。这条路径此前从未被测过。

覆盖：
  A. ?format= 的别名集合（clashmeta/mihomo/yaml、sing-box/sfa/nekobox、base64/v2ray…）
  B. User-Agent 嗅探（各客户端真实 UA 字符串）
  C. 未知 format / 未知 UA 的默认回退（应为 clash）
  D. 两种来源冲突时 ?format= 优先于 UA
  E. 渲染结果可被独立解析（YAML / JSON / base64 解码）
  F. Content-Type 与 Content-Disposition 头正确
"""
import argparse
import base64
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


class R:
    def __init__(self):
        self.cases = []

    def check(self, label, ok, detail=""):
        self.cases.append((label, bool(ok), detail))

    def eq(self, label, got, want):
        self.check(label, got == want, f"got={got!r} want={want!r}")


def req(base, path, headers=None):
    """发一个 GET，遇 429 退避重试。

    为什么必须退避：订阅 feed 的限流桶是 `sub:<token>`、额度 20 次 / 10 秒
    （见 server.go 的 newSlidingLimiter(10*time.Second, 20)）。
    这套件对**同一个 token** 连打 86 次以上（每种格式 × 每个 UA），
    远超额度，于是后半程整片变成
        {"error":"请求过于频繁，请稍后再试"}
    看上去像格式协商坏了，其实是测试自己撞了限流。

    429 不是失败，是「等一会儿再来」——退避重试即可。真正要保留的是
    那些非 429 的响应：它们才反映被测行为。
    """
    delay = 0.6
    for attempt in range(6):
        r = urllib.request.Request(base + path, headers=headers or {}, method="GET")
        try:
            with urllib.request.urlopen(r, timeout=30) as resp:
                return resp.status, resp.read(), dict(resp.headers)
        except urllib.error.HTTPError as e:
            if e.code == 429 and attempt < 5:
                time.sleep(delay)
                delay = min(delay * 1.8, 6)
                continue
            return e.code, e.read(), dict(e.headers)
        except Exception as e:                                   # noqa: BLE001
            return 0, str(e).encode(), {}
    return 429, b'{"error":"rate limited after retries"}', {}


def head(b, n=80):
    """把响应体切成可放进 JSON 报告的短字符串（bytes 不能直接进 JSON）。"""
    return b[:n].decode("utf-8", "replace")


def get_header(hdrs, name):
    """按名字取响应头，大小写不敏感。

    HTTP 头名大小写不敏感，中间代理（Caddy）会把它规范化成 `Profile-Update-Interval`
    这种形式。直接用 dict 查小写 key 会拿到 None，误判成「头没发」。
    """
    low = name.lower()
    for k, v in hdrs.items():
        if k.lower() == low:
            return v
    return None


def store_post(base, key, path, body):
    r = urllib.request.Request(
        base + path, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "X-Store-Key": key}, method="POST")
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        return e.code, None
    except Exception:                                            # noqa: BLE001
        return 0, None


def is_valid_yaml_like(text):
    """Clash 配置是否可被解析。

    注意：服务端用 json.MarshalIndent 输出，而 JSON 是 YAML 1.2 的子集，
    所以合法 JSON 也是合法 YAML，客户端能正常吃下。断言不能靠「文本里有没有
    `proxies:` 这种 YAML 风格写法」来判断 —— 那只在手工写的 YAML 上成立。
    这里改为真正解析一遍，并检查 Clash 必需的三个顶层键。
    """
    try:
        import yaml
    except ImportError:
        # 没有 PyYAML 时退化为 JSON 解析（覆盖当前实现的真实形态）
        try:
            doc = json.loads(text)
        except Exception:                                        # noqa: BLE001
            return False
    else:
        try:
            doc = yaml.safe_load(text)
        except Exception:                                        # noqa: BLE001
            return False
    if not isinstance(doc, dict):
        return False
    need = ["proxies", "proxy-groups", "rules"]
    return all(k in doc for k in need)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--store-key", required=True)
    ap.add_argument("--rounds", type=int, default=1)
    ap.add_argument("--json-out", default="")
    args = ap.parse_args()

    r = R()
    base, key = args.base, args.store_key

    # 准备一条有效订阅
    oid = f"FMT-{int(time.time())}"
    st, body = store_post(base, key, "/api/store/orders",
                          {"order_id": oid, "label": "格式协商测试", "duration_days": 30})
    if st != 200 or not body:
        print(f"建订阅失败: {st}")
        return 1
    tok = body["token"]
    sub = f"/sub/{tok}"

    # ---- A. ?format= 别名 ----
    clash_alias = ["clash", "clashmeta", "mihomo", "yaml", "yml"]
    for a in clash_alias:
        st, b, h = req(base, f"{sub}?format={a}")
        txt = b.decode("utf-8", "replace")
        r.check(f"别名 ?format={a} -> 200", st == 200, f"status={st}")
        r.check(f"别名 ?format={a} -> 是 YAML 配置",
                is_valid_yaml_like(txt), txt[:80])
        r.check(f"别名 ?format={a} -> Content-Type 为 yaml",
                "yaml" in (get_header(h, "Content-Type") or ""), get_header(h, "Content-Type"))

    sb_alias = ["singbox", "sing-box", "sfa", "sfi", "sft", "nekobox"]
    for a in sb_alias:
        st, b, h = req(base, f"{sub}?format={a}")
        try:
            j = json.loads(b.decode())
            parsed = True
        except Exception:                                        # noqa: BLE001
            parsed, j = False, None
        r.check(f"别名 ?format={a} -> 200", st == 200, f"status={st}")
        r.check(f"别名 ?format={a} -> 是合法 JSON", parsed, head(b))
        if parsed:
            r.check(f"别名 ?format={a} -> 有 outbounds", "outbounds" in j,
                    str(list(j.keys()))[:80])
        r.check(f"别名 ?format={a} -> Content-Type 为 json",
                "json" in (get_header(h, "Content-Type") or ""), get_header(h, "Content-Type"))

    uri_alias = ["uri", "base64", "v2ray", "v2rayn", "v2rayng", "text"]
    for a in uri_alias:
        st, b, h = req(base, f"{sub}?format={a}")
        txt = b.decode("utf-8", "replace").strip()
        dec = None
        try:
            dec = base64.b64decode(txt, validate=True).decode("utf-8", "replace")
        except Exception:                                        # noqa: BLE001
            pass
        r.check(f"别名 ?format={a} -> 200", st == 200, f"status={st}")
        r.check(f"别名 ?format={a} -> 是 base64", dec is not None, txt[:60])
        if dec is not None:
            r.check(f"别名 ?format={a} -> 解出 hysteria2:// 链接",
                    "hysteria2://" in dec, dec[:80])

    # ---- B. User-Agent 嗅探 ----
    ua_clash = [
        "ClashforWindows/0.20.39",
        "mihomo/1.18.0",
        "clash-verge/v1.5.11",
        "ClashX/1.95.0",
        "Stash/2.5.0",
        "Surfboard/2.28.0",
    ]
    for ua in ua_clash:
        st, b, h = req(base, sub, {"User-Agent": ua})
        r.check(f"UA 嗅探 Clash（{ua.split('/')[0]}）-> YAML",
                is_valid_yaml_like(b.decode("utf-8", "replace")), head(b))

    ua_sb = ["sing-box/1.9.0", "SFA/1.9.0", "SFI/1.9.0", "NekoBox/1.2.0", "NekoRay/3.26"]
    for ua in ua_sb:
        st, b, h = req(base, sub, {"User-Agent": ua})
        ok = False
        try:
            ok = "outbounds" in json.loads(b.decode())
        except Exception:                                        # noqa: BLE001
            pass
        r.check(f"UA 嗅探 sing-box（{ua.split('/')[0]}）-> JSON", ok, head(b))

    ua_uri = ["Hysteria/1.0", "v2rayNG/1.8.5", "Shadowrocket/2.2.0", "Kitsunebi/1.0"]
    for ua in ua_uri:
        st, b, h = req(base, sub, {"User-Agent": ua})
        dec = None
        try:
            dec = base64.b64decode(b.decode().strip(), validate=True).decode()
        except Exception:                                        # noqa: BLE001
            pass
        r.check(f"UA 嗅探 URI（{ua.split('/')[0]}）-> base64",
                dec is not None and "hysteria2://" in dec, head(b))

    # ---- C. 默认回退 ----
    st, b, h = req(base, sub)
    r.check("无 format 无 UA -> 默认回退为 Clash YAML",
            is_valid_yaml_like(b.decode("utf-8", "replace")), head(b))

    st, b, h = req(base, sub, {"User-Agent": "curl/8.0.1"})
    r.check("未知 UA（curl）-> 默认回退为 Clash YAML",
            is_valid_yaml_like(b.decode("utf-8", "replace")), head(b))

    st, b, h = req(base, f"{sub}?format=nonsense")
    r.check("未知 format 值 -> 默认回退为 Clash YAML",
            is_valid_yaml_like(b.decode("utf-8", "replace")), head(b))

    st, b, h = req(base, f"{sub}?format=")
    r.check("空 format 值 -> 默认回退为 Clash YAML",
            is_valid_yaml_like(b.decode("utf-8", "replace")), head(b))

    # ---- D. ?format= 优先于 UA ----
    st, b, h = req(base, f"{sub}?format=singbox", {"User-Agent": "ClashforWindows/0.20.39"})
    ok = False
    try:
        ok = "outbounds" in json.loads(b.decode())
    except Exception:                                            # noqa: BLE001
        pass
    r.check("format=singbox 覆盖 Clash UA", ok, head(b))

    st, b, h = req(base, f"{sub}?format=uri", {"User-Agent": "ClashforWindows/0.20.39"})
    dec = None
    try:
        dec = base64.b64decode(b.decode().strip(), validate=True).decode()
    except Exception:                                            # noqa: BLE001
        pass
    r.check("format=uri 覆盖 Clash UA", dec is not None and "hysteria2://" in dec, head(b))

    st, b, h = req(base, f"{sub}?format=clash", {"User-Agent": "sing-box/1.9.0"})
    r.check("format=clash 覆盖 sing-box UA",
            is_valid_yaml_like(b.decode("utf-8", "replace")), head(b))

    # ---- E. 大小写与空白 ----
    st, b, h = req(base, f"{sub}?format=CLASH")
    r.check("format 大写 CLASH 也能识别（大小写不敏感）",
            is_valid_yaml_like(b.decode("utf-8", "replace")), head(b))

    st, b, h = req(base, f"{sub}?format=%20clash%20")
    r.check("format 带空白也识别（TrimSpace）",
            is_valid_yaml_like(b.decode("utf-8", "replace")), head(b))

    # ---- F. 响应头 ----
    st, b, h = req(base, f"{sub}?format=clash")
    r.check("Clash 带 Content-Disposition 附件名",
            "clash.yaml" in (get_header(h, "Content-Disposition") or ""),
            get_header(h, "Content-Disposition"))
    r.check("带 Cache-Control: no-store（凭证不能缓存）",
            "no-store" in (get_header(h, "Cache-Control") or ""), get_header(h, "Cache-Control"))
    r.check("带 profile-update-interval（客户端会按它重拉）",
            get_header(h, "profile-update-interval") == "12", get_header(h, "profile-update-interval"))

    st, b, h = req(base, f"{sub}?format=singbox")
    r.check("sing-box 带 Content-Disposition",
            "sing-box.json" in (get_header(h, "Content-Disposition") or ""),
            get_header(h, "Content-Disposition"))

    st, b, h = req(base, f"{sub}?format=uri")
    r.check("uri 带 Content-Disposition",
            "hy2-subscription.txt" in (get_header(h, "Content-Disposition") or ""),
            get_header(h, "Content-Disposition"))

    # ---- 汇总 ----
    total = len(r.cases)
    passed = sum(1 for _, ok, _ in r.cases if ok)
    print(f"第 1 轮（格式协商）  {passed}/{total} 通过")
    for label, ok, detail in r.cases:
        if not ok:
            print(f"  x {label}  {detail}")
    if args.json_out:
        with open(args.json_out, "w", encoding="utf-8") as f:
            json.dump({"total": total, "passed": passed,
                       "cases": [{"label": l, "pass": o, "detail": d}
                                 for l, o, d in r.cases]},
                      f, ensure_ascii=False, indent=2)
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
