"""
公共解析工具（替代 store-e2e.py 内嵌的 parse_clash_proxies 等）。

⚠️ 之前的正则太严，把 `server:...` 写死了，结果 clash 实际是 JSON
格式时（"server": "..."），所有 5 轮断言都返空。改用 JSON-first + 正则 fallback。
"""
from __future__ import annotations

import json
import re
from typing import Any


def parse_clash_proxies(clash_text: str) -> list[str]:
    """从 /sub/<token>?format=clash 返的 clash 文本提取所有 server 域名。

    ⚠️ /sub/{token} 的 clash 端点**实际返 JSON**而非 YAML（"server": "..."，
    不是 server: ...）。先尝试 JSON，失败再 fallback 到正则。
    """
    if not clash_text:
        return []
    text = clash_text.strip()
    if text.startswith("{"):
        try:
            obj = json.loads(text)
            return sorted({p["server"] for p in obj.get("proxies", [])
                          if isinstance(p, dict) and p.get("server")})
        except Exception:                                            # noqa: BLE001
            pass
    # fallback：正则
    servers = set()
    for m in re.finditer(r'server["\s:]+([^\s\n,"}]+)', text):
        s = m.group(1).strip().strip('"').strip("'")
        if s:
            servers.add(s)
    return sorted(servers)


def parse_active_ips(body: Any) -> list[dict]:
    """公开视图 active_ips 字段标准化。"""
    if not isinstance(body, dict):
        return []
    return body.get("active_ips") or []


def status_dist(records: list[dict]) -> dict[int, int]:
    """汇总 HttpClient.log 的 status 分布。"""
    out: dict[int, int] = {}
    for r in records:
        out[r["status"]] = out.get(r["status"], 0) + 1
    return out


def make_order_id(round_index: int, tag: str, prefix: str = "") -> str:
    """订单号必须全局唯一 —— 用 prefix(round_idx)(tag) 防跨轮命中幂等。
    prefix 是「可选」，例如 "REG-R{idx}-{tag}」、「E2E-R{idx}-{tag}」。
    """
    if prefix:
        return f"{prefix}-{tag}"
    return f"R{round_index}-{tag}"