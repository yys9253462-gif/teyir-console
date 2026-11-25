"""
公共 HTTP 客户端 + 断言框架（tests/ 复用）。

⚠️ 设计原则（2026-10-03 重构）：
  - **Result dataclass**：raw 返 (status, payload, text) 三元组太容易
    unpack 出 bug（"expected 2, got 3"）。改返命名字段 result.status / body / text。
  - **连接层错**：超时 / SSL / 连接拒绝都归为 transport error（status=0 +
    detail.type = "timeout"|"ssl"|"refused"）。上层断言可区分。
  - **断言框架**：Case / Round / eq() 与原 store-regression.py 同款，
    重写是为了"分散错误类型"。
  - **JSON 输出**：所有详情走 --json-out；print 只用于人眼看的进度。
"""
from __future__ import annotations

import dataclasses
import json
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


# -------------------------------------------------------------------------
# Case / Round —— 断言框架
# -------------------------------------------------------------------------

@dataclasses.dataclass(slots=True)
class Case:
    label: str
    ok: bool
    detail: str = ""

    def to_dict(self):
        return {"label": self.label, "pass": self.ok, "detail": self.detail}


class Round:
    __slots__ = ("index", "cases")

    def __init__(self, index):
        self.index = index
        self.cases: list[Case] = []

    def check(self, label: str, ok: bool, detail: str = "") -> bool:
        self.cases.append(Case(label, bool(ok), detail))
        return bool(ok)

    def eq(self, label: str, got: Any, want: Any) -> bool:
        return self.check(label, got == want, f"got={got!r} want={want!r}")

    @property
    def failed(self) -> list[Case]:
        return [c for c in self.cases if not c.ok]


# -------------------------------------------------------------------------
# Result + Client —— HTTP 调用
# -------------------------------------------------------------------------

# 连接层错的细分类型。原来 status=0 一刀切，分不清超时 / SSL / 拒绝。
@dataclasses.dataclass(slots=True)
class Result:
    status: int                                  # HTTP status; 0 表示连接层错
    body: Any                                    # 解析后的 JSON（解析失败为 None）
    text: str                                       # 原始响应体
    transport_error: str = ""                    # 连接层错时是 "timeout"/"ssl"/"refused"/"other"

    @property
    def is_transport_error(self) -> bool:
        return self.status == 0

    def to_dict(self):
        d = {"status": self.status}
        if self.transport_error:
            d["transport_error"] = self.transport_error
        d["body_preview"] = self.text[:120] if self.text else ""
        return d


class HttpClient:
    """HTTP 客户端（替代 store-regression.py / store-e2e.py 的内嵌 Client）。

    设计要点：
      - raw 返 Result（命名字段），调用方读 .status / .body / .text 不会 unpack 错。
      - 连接层错细分（超时/SSL/连接拒绝），不再用 status=0 一刀切。
      - 一次会话复用 TCP（HTTPConnection 池）—— 减少 SSL 握手开销 5 轮 100+ 调用。
    """

    def __init__(self, base: str, timeout: float = 60.0):
        self.base = base.rstrip("/")
        self.timeout = timeout
        self.log: list[dict] = []
        # ⚠️ 不用 requests —— 减少依赖。urllib.request 自带连接池靠 opener。
        self._opener = urllib.request.build_opener(
            urllib.request.HTTPHandler,
            urllib.request.HTTPSHandler,
        )

    def raw(self, method: str, path: str, body: Any = None,
            headers: dict | None = None) -> Result:
        url = self.base + path
        data = None
        hdrs = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            hdrs["Content-Type"] = "application/json"
        if headers:
            hdrs.update(headers)
        req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
        try:
            with self._opener.open(req, timeout=self.timeout) as resp:
                text = resp.read().decode("utf-8", "replace")
                status = resp.status
                transport = ""
        except urllib.error.HTTPError as e:
            text = e.read().decode("utf-8", "replace")
            status = e.code
            transport = ""
        except TimeoutError:
            text = ""
            status = 0
            transport = "timeout"
        except (ConnectionResetError, ConnectionRefusedError):
            text = ""
            status = 0
            transport = "refused"
        except urllib.error.URLError as e:
            reason = str(e.reason) if hasattr(e, "reason") else str(e)
            text = ""
            status = 0
            if "SSL" in reason or "CERTIFICATE" in reason.upper():
                transport = "ssl"
            elif "timed out" in reason.lower():
                transport = "timeout"
            elif "refused" in reason.lower():
                transport = "refused"
            else:
                transport = "other"
        except Exception as e:                                       # noqa: BLE001
            text = f"<transport error: {e}>"
            status = 0
            transport = "other"

        self.log.append({"method": method, "path": path, "status": status,
                         "transport": transport})
        try:
            payload = json.loads(text) if text else None
        except Exception:                                            # noqa: BLE001
            payload = None
        return Result(status=status, body=payload, text=text, transport_error=transport)

    # ---- 便捷：3-tuple 老 API（兼容 store-regression.py 现有 3-tuple unpack） ----
    def raw_tuple(self, method: str, path: str, body: Any = None,
                  headers: dict | None = None,
                  xff: str | None = None) -> tuple[int, Any, str]:
        """返 (status, body, text) 三元组。

        ⚠️ 老 store-regression.py 用 `st, body, _ = cli.raw(...)` 与
        `st, view, _ = cli.raw(...)` 3-tuple unpack，本函数为它保留。
        Result dataclass 是新代码的推荐用法（命名字段，不易 unpack 错）。

        ⚠️ 必须直接调内部 _send_request 而不能调 self.raw —— 子类
        重写 raw 时会让 self.raw 走到子类，导致无限递归。
        """
        if xff:
            headers = dict(headers or {})
            headers["X-Forwarded-For"] = xff
        url = self.base + path
        data = None
        hdrs = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            hdrs["Content-Type"] = "application/json"
        if headers:
            hdrs.update(headers)
        req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
        try:
            with self._opener.open(req, timeout=self.timeout) as resp:
                text = resp.read().decode("utf-8", "replace")
                status = resp.status
                transport = ""
        except urllib.error.HTTPError as e:
            text = e.read().decode("utf-8", "replace")
            status = e.code
            transport = ""
        except TimeoutError:
            text = ""; status = 0; transport = "timeout"
        except (ConnectionResetError, ConnectionRefusedError):
            text = ""; status = 0; transport = "refused"
        except urllib.error.URLError as e:
            reason = str(e.reason) if hasattr(e, "reason") else str(e)
            text = ""; status = 0
            if "SSL" in reason or "CERTIFICATE" in reason.upper():
                transport = "ssl"
            elif "timed out" in reason.lower():
                transport = "timeout"
            elif "refused" in reason.lower():
                transport = "refused"
            else:
                transport = "other"
        except Exception as e:                                       # noqa: BLE001
            text = f"<transport error: {e}>"; status = 0; transport = "other"

        self.log.append({"method": method, "path": path, "status": status,
                         "transport": transport})
        try:
            payload = json.loads(text) if text else None
        except Exception:                                            # noqa: BLE001
            payload = None
        return (status, payload, text)

    # ---- 便捷封装 ----

    def status(self, method: str, path: str, body: Any = None,
               headers: dict | None = None) -> int:
        """只要 status 不要 body —— 字段校验类断言常用。"""
        return self.raw(method, path, body, headers).status

    def is_2xx(self, method: str, path: str, body: Any = None,
               headers: dict | None = None) -> bool:
        return 200 <= self.status(method, path, body, headers) < 300


# -------------------------------------------------------------------------
# 报表
# -------------------------------------------------------------------------

def report_to_json(base: str, rounds: list[Round], status_dist: dict,
                    elapsed_s: float, total: int, passed: int) -> dict:
    """统一 JSON 报表（替代 3 个脚本里重复的 json.dump 段）。"""
    return {
        "base": base,
        "rounds": [{
            "index": r.index,
            "cases": [c.to_dict() for c in r.cases],
        } for r in rounds],
        "total": total,
        "passed": passed,
        "elapsed_s": round(elapsed_s, 1),
        "status_dist": status_dist,
    }


def print_round_summary(round: Round) -> None:
    """人眼看的进度输出。
    ⚠️ 详细信息由 --json-out 唯一给出；这里只打 PASS/FAIL + 失败 case 标签。"""
    bad = round.failed
    mark = "OK  " if not bad else "FAIL"
    print(f"[{mark}] 第 {round.index} 轮  {len(round.cases) - len(bad)}/{len(round.cases)} 通过")
    for c in bad:
        print(f"         x {c.label}  {c.detail[:200]}")


# -------------------------------------------------------------------------
# 模块级便捷：旧 API 兼容
# -------------------------------------------------------------------------

# 一些旧脚本里直接用 `Client` / `Case` / `Round` —— 顶层 re-export 让它们
# 不改也能 import 到。
Client = HttpClient        # noqa: F811  (alias for legacy callers)