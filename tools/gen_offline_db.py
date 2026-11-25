#!/usr/bin/env python3
"""离线归属库生成工具 —— 把「IP 段 → 地区/商家」写成 geo 包认识的 JSON。

为什么需要这个脚本
------------------
geo 包支持外部 JSON 离线库（GEO_OFFLINE_DB），但那个文件谁来生成？
控制台本身不生成它（服务跑在生产机上，不该干数据加工的活）。
所以提供这个独立工具，在**能上网的机器上**跑一次，产出文件后
scp 到服务器即可。

为什么离线库比纯 API 更值得配
------------------------------
- 不把「我有哪几台机器」的 IP 清单持续告诉第三方
- 无限速（API 免费版 45 次/分钟，批量识别会撞）
- 面板在没出网的机器上也能识别

数据来源
--------
本工具用 ip-api 的 **batch 端点**（POST http://ip-api.com/batch，一次最多
100 个 IP）。免费版同样限速 15 次/分钟的 batch 请求，所以脚本内置了
节流与重试。

⚠️ 关于用途限制：ip-api 免费版**仅允许非商业用途**，商用需购买。
这个限制同样适用于本脚本产出的数据。若你的控制台用于商业运营，
请改用付费端点或自备数据源（脚本里 SOURCE 常量说明了数据出处，
换源时请一并更新，便于日后复核合规性）。

用法
----
    # 从一批 IP 生成（每行一个 IP，支持 # 注释）
    python3 gen_offline_db.py --ips ips.txt --out geo-offline.json

    # 从已有库的网段反查（增量扩库）
    python3 gen_offline_db.py --seed geo-offline.json --out geo-offline.json

    # 生成后自查
    python3 gen_offline_db.py --selftest geo-offline.json
"""

from __future__ import annotations

import argparse
import ipaddress
import json
import sys
import time
import urllib.error
import urllib.request
from datetime import date

BATCH_URL = "http://ip-api.com/batch"
# 只取需要的字段，减小响应体。fields 是 ip-api 的正式参数。
FIELDS = "status,message,country,countryCode,city,regionName,isp,org,as,query"
# 免费版 batch 上限是 100，但用 80 留余量（避免边界行为差异）。
BATCH_SIZE = 80
# 免费版 batch 限速 15 次/分钟 = 每 4 秒一次；用 4.5 秒留余量。
THROTTLE_SECONDS = 4.5
SOURCE_NOTE = "ip-api.com batch"

# Windows 控制台默认是 GBK/CP936，直接 print 非 GBK 字符（如 ✓ ✗ ⚠️）会抛
# UnicodeEncodeError 把整个脚本打断 —— 实测踩过，且报错信息完全指不到根因。
# 所以：① 统一用 ASCII 标记代替 emoji；② 顺手把 stdout 的错误处理调成
# replace，这样即使遇到别的非 GBK 字符（比如 ip-api 返回带重音的地名）
# 也只是显示成 ?，不会让整批数据白跑。
if hasattr(sys.stdout, "reconfigure"):
    try:
        sys.stdout.reconfigure(errors="replace")
    except Exception:
        pass
if hasattr(sys.stderr, "reconfigure"):
    try:
        sys.stderr.reconfigure(errors="replace")
    except Exception:
        pass

# 文本标记统一用 ASCII，避免 GBK 终端崩溃。
OK = "[OK]"
BAD = "[!!]"
WARN = "[warn]"

# 与 Go 侧 geo.go 的 knownProviders 保持一致的短名归一。
#
# ⚠️ 这两处必须同步：Go 侧负责「查到的数据怎么归一」，这里负责
# 「写进库的数据长什么样」。若只改一边，会出现"离线库查出来的商家名
# 和 API 查出来的不一样"，同一台机器在两条路径下得到不同名字 ——
# 那是很难排查的不一致。改这里时请一并改 server/internal/geo/geo.go。
KNOWN_PROVIDERS: list[tuple[str, list[str], list[str]]] = [
    ("AWS", ["amazon", "aws"], ["AS16509", "AS14618", "AS8987", "AS38895"]),
    ("Azure", ["microsoft", "azure"], ["AS8075", "AS8068"]),
    ("GCP", ["google"], ["AS396982", "AS15169", "AS19527"]),
    ("Oracle", ["oracle"], ["AS31898", "AS14589"]),
    ("Hetzner", ["hetzner"], ["AS24940", "AS213230"]),
    ("OVH", ["ovh"], ["AS16276", "AS35540"]),
    ("Vultr", ["vultr", "choopa", "constant"], ["AS20473"]),
    ("DigitalOcean", ["digitalocean", "digital ocean"], ["AS14061", "AS46652"]),
    ("Linode", ["linode", "akamai"], ["AS63949", "AS20940"]),
    ("Contabo", ["contabo"], ["AS51167", "AS40021"]),
    ("Cloudflare", ["cloudflare"], ["AS13335"]),
    ("Scaleway", ["scaleway"], ["AS12876"]),
    ("Netcup", ["netcup"], ["AS197540"]),
    ("RackNerd", ["racknerd"], ["AS36352", "AS33182"]),
    ("Alibaba Cloud", ["alibaba", "aliyun", "alicloud"], ["AS45102", "AS37963"]),
    ("Tencent Cloud", ["tencent"], ["AS45090", "AS132203"]),
    ("Huawei Cloud", ["huawei"], ["AS136907", "AS55990"]),
    ("M247", ["m247"], ["AS9009"]),
    ("Leaseweb", ["leaseweb"], ["AS60781", "AS28753"]),
    ("Gcore", ["gcore"], ["AS199524", "AS202422"]),
    ("Zenlayer", ["zenlayer"], ["AS21859"]),
    ("NTT", ["ntt"], ["AS2914"]),
    ("Cogent", ["cogent"], ["AS174"]),
]

_ASN_INDEX: dict[str, str] = {}
for _short, _patterns, _asns in KNOWN_PROVIDERS:
    for _asn in _asns:
        _ASN_INDEX.setdefault(_asn, _short)


def clean_place(raw: str) -> str:
    """收拾地名：去括号补充、压空格。与 Go 侧 cleanPlaceName 对齐。"""
    value = (raw or "").strip()
    if not value:
        return ""
    if "(" in value:
        head = value.split("(", 1)[0].strip()
        if head:
            value = head
    return " ".join(value.split())


def clean_provider(raw: str) -> str:
    """把 ASN 组织名收拾成短标签。与 Go 侧 cleanProviderName 对齐。"""
    value = (raw or "").strip()
    if not value:
        return ""
    if "(" in value:
        head = value.split("(", 1)[0].strip()
        if head:
            value = head
    drop = {
        "gmbh", "inc", "inc.", "ltd", "ltd.", "llc", "llc.", "corp", "corp.",
        "co.", "sa", "s.a.", "bv", "b.v.", "ag", "pte", "pty", "srl", "s.r.l.",
        "oy", "ab", "as", "kk", "k.k.",
    }
    kept = [w for w in value.split() if w.lower() not in drop]
    return " ".join(kept) if kept else value


def extract_asn(raw: str) -> str:
    """从 "AS16509 Amazon.com, Inc." 里取出 "AS16509"。"""
    parts = (raw or "").strip().split()
    if not parts:
        return ""
    head = parts[0]
    if len(head) > 2 and head[:2].upper() == "AS" and head[2:].isdigit():
        return "AS" + head[2:]
    return ""


def match_known(org: str, isp: str, asn: str) -> str:
    """把组织信息归一到已知商家短名。ASN 优先于子串（与 Go 侧一致）。"""
    if asn and asn in _ASN_INDEX:
        return _ASN_INDEX[asn]
    haystack = f"{org or ''} {isp or ''}".lower()
    if not haystack.strip():
        return ""
    for short, patterns, _asns in KNOWN_PROVIDERS:
        for pattern in patterns:
            if pattern in haystack:
                return short
    return ""


def query_batch(ips: list[str], retries: int = 3) -> list[dict]:
    """打一次 batch 请求，带重试。返回与 ips 等长的结果列表。"""
    body = json.dumps([{"query": ip, "fields": FIELDS} for ip in ips]).encode()
    last_error = ""
    for attempt in range(1, retries + 1):
        request = urllib.request.Request(
            BATCH_URL,
            data=body,
            headers={"Content-Type": "application/json", "User-Agent": "hy2-ops-console-geodb/1.0"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                payload = json.loads(response.read().decode())
            if isinstance(payload, list):
                return payload
            last_error = f"响应不是数组: {type(payload).__name__}"
        except urllib.error.HTTPError as err:
            # 429 是限速 —— 退避后重试。
            last_error = f"HTTP {err.code}"
            if err.code == 429:
                time.sleep(THROTTLE_SECONDS * attempt)
                continue
        except Exception as err:  # 网络问题一律重试
            last_error = str(err)
        time.sleep(2 * attempt)
    print(f"  {WARN} 批次失败（已重试 {retries} 次）: {last_error}", file=sys.stderr)
    return []


def record_from(item: dict, prefix: int = 32, v6_prefix: int = 48) -> dict | None:
    """把一条 ip-api 结果转成离线库记录。转换不出有效内容时返回 None。

    ⚠️ 关于「把 /32 放宽成网段」这个决定（prefix 参数）
    ---------------------------------------------------
    第一版按 ip-api 返回的单个 IP 写成 `/32`，端到端测通了，但那个库
    **对本功能毫无用处**：它只能识别你"已经填进清单里"的那几个 IP，
    而自动识别的全部价值在于**处理你还没见过的新机器**。
    新买一台 VPS，它的 IP 不在库里的 /32 列表上，照样得走 API。

    所以默认把单 IP 放宽成它所在的网段（IPv4 /24，IPv6 /48）。
    这不是凭空扩大：同一个 /24 在云厂商那里几乎总属于同一机房同一商家
    （AWS 的一个 /24 不会跨东京和法兰克福）。所以用一台已知机器的归属
    去覆盖它所在的 /24，命中率远高于风险。

    风险也说清楚：**放宽必然是近似的**。若某个 /24 里恰好混了不同机房的
    机器（少见，但在小型转售商那里可能发生），识别会给错地区。
    这类错误的表现是"某台机器地区标错"，而且它是 auto 的 —— 用户在界面上
    改一次就永久生效（来源变 manual），所以可纠正。
    真要精确，就 `--exact-prefix` 保留 /32 语义。
    """
    if item.get("status") != "success":
        return None
    query = (item.get("query") or "").strip()
    if not query:
        return None
    try:
        address = ipaddress.ip_address(query)
    except ValueError:
        return None

    # 按配置放宽到网段。
    bits = v6_prefix if address.version == 6 else prefix
    try:
        network = ipaddress.ip_network(f"{address}/{bits}", strict=False)
    except ValueError:
        return None

    city = clean_place(item.get("city", ""))
    country = clean_place(item.get("country", ""))
    asn = extract_asn(item.get("as", ""))
    org = (item.get("org") or "").strip()
    isp = (item.get("isp") or "").strip()

    short = match_known(org, isp, asn)
    provider_long = org or isp

    record: dict[str, str] = {"network": str(network)}
    if city:
        record["region"] = city
    if city and country:
        record["region_long"] = f"{city}, {country}"
    elif country:
        record["region"] = country
        record["region_long"] = country
    code = (item.get("countryCode") or "").strip().upper()
    if code:
        record["country"] = code
    if short:
        record["provider"] = short
    elif provider_long:
        # 注意 provider_long 有时是地名而不是公司名（实测：阿里云香港那台
        # ip-api 给的 org 就是 "Hong Kong"）。那种情况该条没有可用的商家名，
        # 但 ASN 仍在，靠 ASN 匹配已知商家才是正路 —— 所以只填 long 不填短名，
        # 让"没有可靠商家名"这个事实保持可见，而不是塞一个地名进去冒充商家。
        cleaned = clean_provider(provider_long)
        if cleaned and not _looks_like_place(cleaned):
            record["provider"] = cleaned
    if provider_long:
        record["provider_long"] = provider_long
    if asn:
        record["asn"] = asn

    # 只有网段的记录没有价值。
    if len(record) <= 1:
        return None
    return record


# 明显是地名而非公司名的组织字段。用于避免把 "Hong Kong" 当成商家名。
_PLACE_HINTS = {
    "hong kong", "singapore", "tokyo", "osaka", "seoul", "taipei", "manila",
    "new york", "los angeles", "san jose", "seattle", "ashburn", "dallas",
    "frankfurt", "helsinki", "amsterdam", "london", "paris", "stockholm",
    "sydney", "mumbai", "dubai", "beijing", "shanghai", "shenzhen", "guangzhou",
    "china", "japan", "korea", "germany", "france", "netherlands", "sweden",
    "united states", "united kingdom", "canada", "australia", "india",
}


def _looks_like_place(value: str) -> bool:
    return value.strip().lower() in _PLACE_HINTS


def load_ips(path: str) -> list[str]:
    """读 IP 清单：每行一个，支持 # 注释与空行。

    用 utf-8-sig 而不是 utf-8：Windows 的记事本/PowerShell 写文件时经常带
    BOM，而普通 utf-8 解码会在第一个字符就炸（"Unexpected UTF-8 BOM"）。
    用户完全可能拿记事本存这个清单，所以必须容忍 BOM。
    """
    out: list[str] = []
    with open(path, "r", encoding="utf-8-sig") as handle:
        for line in handle:
            value = line.split("#", 1)[0].strip()
            if not value:
                continue
            try:
                ipaddress.ip_address(value)
            except ValueError:
                print(f"  {WARN} 跳过非法 IP: {value}", file=sys.stderr)
                continue
            out.append(value)
    return out


def load_seed(path: str) -> list[str]:
    """从已有离线库里取出全部网段，用于增量扩库。"""
    with open(path, "r", encoding="utf-8-sig") as handle:
        data = json.load(handle)
    out: list[str] = []
    for record in data.get("entries", []):
        network = record.get("network", "")
        try:
            # 取网段的第一个可用地址作为该段代表。
            net = ipaddress.ip_network(network, strict=False)
            out.append(str(net.network_address))
        except ValueError:
            continue
    return out


def selftest(path: str) -> int:
    """自查离线库：结构、网段合法性、字段完整性、可解析性。

    这个自查存在的意义：库是脚本生成的、几十 MB，人工翻不现实；
    而库坏掉的症状是"识别率莫名变低"，非常难归因。
    """
    print(f"自查 {path}")
    try:
        with open(path, "r", encoding="utf-8-sig") as handle:
            data = json.load(handle)
    except Exception as err:
        print(f"  {BAD} 无法解析: {err}")
        return 1

    entries = data.get("entries", [])
    print(f"  条目数: {len(entries)}")
    print(f"  _meta: {data.get('_meta', {})}")

    bad_network = 0
    empty_entry = 0
    have_provider = 0
    have_region = 0
    prefix_hist: dict[int, int] = {}
    for record in entries:
        try:
            net = ipaddress.ip_network(record.get("network", ""), strict=False)
        except ValueError:
            bad_network += 1
            continue
        prefix_hist[net.prefixlen] = prefix_hist.get(net.prefixlen, 0) + 1
        if not any(record.get(k) for k in ("region", "region_long", "provider", "provider_long", "country", "asn")):
            empty_entry += 1
        if record.get("provider"):
            have_provider += 1
        if record.get("region"):
            have_region += 1

    print(f"  非法网段: {bad_network}")
    print(f"  空条目: {empty_entry}")
    print(f"  带地区: {have_region}  带商家: {have_provider}")
    if prefix_hist:
        dist = ", ".join(f"/{p}×{n}" for p, n in sorted(prefix_hist.items()))
        print(f"  前缀分布: {dist}")
        # 全 /32 的库对本功能没意义 —— 只能认已知 IP，认不出新机器。
        if set(prefix_hist) == {32} and len(entries) > 1:
            print(f"  {WARN} 警告: 全部是 /32 单 IP 条目。这样的库只能识别清单里已有的 IP，"
                  "新买的机器仍然要打公网 API。建议去掉 --exact-prefix 用网段生成。")

    ok = bad_network == 0 and empty_entry == 0 and len(entries) > 0
    print("  结论:", f"{OK} 可用" if ok else f"{BAD} 有问题")
    return 0 if ok else 1


def main() -> int:
    parser = argparse.ArgumentParser(description="生成 geo 离线归属库")
    parser.add_argument("--ips", help="IP 清单文件（每行一个）")
    parser.add_argument("--seed", help="已有离线库，取出其网段做增量更新")
    parser.add_argument("--out", help="输出 JSON 路径")
    parser.add_argument("--selftest", help="只做自查，不生成")
    parser.add_argument("--prefix", type=int, default=24,
                        help="IPv4 放宽到的前缀长度（默认 24）。用 32 表示只记单 IP")
    parser.add_argument("--v6-prefix", type=int, default=48,
                        help="IPv6 放宽到的前缀长度（默认 48）")
    parser.add_argument("--exact-prefix", action="store_true",
                        help="等价于 --prefix 32 --v6-prefix 128（只记单 IP，库最小但只能认已知机器）")
    args = parser.parse_args()

    if args.selftest:
        return selftest(args.selftest)

    if not args.out:
        parser.error("必须指定 --out")
    if not args.ips and not args.seed:
        parser.error("至少要给 --ips 或 --seed 之一")

    prefix = 32 if args.exact_prefix else args.prefix
    v6_prefix = 128 if args.exact_prefix else args.v6_prefix
    if not (0 <= prefix <= 32):
        parser.error("--prefix 必须在 0..32")
    if not (0 <= v6_prefix <= 128):
        parser.error("--v6-prefix 必须在 0..128")

    targets: list[str] = []
    if args.ips:
        targets.extend(load_ips(args.ips))
        print(f"从 {args.ips} 读到 {len(targets)} 个 IP")
    if args.seed:
        seeded = load_seed(args.seed)
        print(f"从 {args.seed} 取到 {len(seeded)} 个网段代表地址")
        targets.extend(seeded)

    # 去重但保序（稳定输出便于 diff 与复现）。
    seen: set[str] = set()
    unique: list[str] = []
    for ip in targets:
        if ip not in seen:
            seen.add(ip)
            unique.append(ip)
    targets = unique
    print(f"去重后共 {len(targets)} 个目标")

    if not targets:
        print("没有可查询的目标，退出", file=sys.stderr)
        return 1

    batches = [targets[i:i + BATCH_SIZE] for i in range(0, len(targets), BATCH_SIZE)]
    print(f"分 {len(batches)} 批查询（每批最多 {BATCH_SIZE}，间隔 {THROTTLE_SECONDS}s 以避开限速）")

    entries: list[dict] = []
    failures = 0
    for index, batch in enumerate(batches, 1):
        print(f"  批次 {index}/{len(batches)} ...", flush=True)
        results = query_batch(batch)
        if not results:
            failures += 1
        for item in results:
            record = record_from(item, prefix=prefix, v6_prefix=v6_prefix)
            if record:
                entries.append(record)
        if index < len(batches):
            time.sleep(THROTTLE_SECONDS)

    # 同一网段可能被多个 IP 映射到（放宽之后尤其常见：一个 /24 里的三台机器
    # 会产出三条 network 相同的记录）。必须去重，否则库会白白膨胀，
    # 而且 Go 侧的最长前缀匹配会在同前缀条目里"取先出现的那个" ——
    # 结果取决于排序，不可预测。
    #
    # 去重策略：**第一个胜出**（保序）。同一个 /24 里的机器几乎总属于同一
    # 商家同一机房，所以留哪条都一样；真正的冲突（同 /24 不同机房）属于
    # 罕见情况，此时保序至少让结果可复现。
    deduped: dict[str, dict] = {}
    for record in entries:
        deduped.setdefault(record["network"], record)
    entries = list(deduped.values())

    # 按网段排序，输出稳定。
    entries.sort(key=lambda r: r["network"])

    payload = {
        "_meta": {
            "source": SOURCE_NOTE,
            "generated": date.today().isoformat(),
            "count": len(entries),
            "ipv4_prefix": prefix,
            "ipv6_prefix": v6_prefix,
            "note": "由 tools/gen_offline_db.py 生成；切勿手工编辑。ip-api 免费版仅限非商业用途。",
            "failures": failures,
        },
        "entries": entries,
    }
    with open(args.out, "w", encoding="utf-8", newline="\n") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=1)
        handle.write("\n")

    print(f"\n已写出 {args.out}：{len(entries)} 条（{failures} 个批次失败）")
    print("自查：")
    return selftest(args.out)


if __name__ == "__main__":
    sys.exit(main())
