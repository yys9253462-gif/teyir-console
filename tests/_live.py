"""
客户端真连的「后置门」封装。

⚠️ 设计原则（2026-10-03 管理员要求）：
  - **不重复实现** hysteria 客户端拨号的细节 —— 既有 tests/subscription-live-check.py
    / tests/verify-real-node-subscription.py 已经完整覆盖。
  - 这里是**事后钩子**：store-e2e 跑完业务链后，可以选 --include-cli 把客户端
    真连的检查加进来。不加时所有断言走业务链视角。
  - 用 pytest-like 的后置 fixture 模型 —— 但因为老脚本不用 pytest，改成一个
    独立的 `live_check(cli, token, ...)` 函数。

调用：
  if cli_opts.get("include_cli"):
      res = live_check(cli, token=token, alias="panel-host")
      if not res.ok:
          r.check(f"[R{idx}] 客户端真连", False, res.detail)

真实实现放 tests/live.py 的 live_check() —— 由 store-e2e 在主调用点用
try/except 触发。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(slots=True)
class LiveCheckResult:
    ok: bool
    detail: str
    exit_ip: str = ""
    expected_ip: str = ""


def live_check_disabled(*args: Any, **kwargs: Any) -> LiveCheckResult:
    """默认 fallback：客户端分支禁用。

    本模块不实现客户端拨号 —— 既有 tests/subscription-live-check.py 完整覆盖。
    这里只在用户传 --include-cli 时报警「需要客户端环境」。
    """
    return LiveCheckResult(
        ok=False,
        detail="客户端真连未启用（继承 live-check.py，请用 --include-cli 调用）",
    )