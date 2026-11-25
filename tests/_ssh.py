"""
公共 SSH + DB 访问辅助（替代 store-e2e.py 内嵌的 _ssh / panel_db_read）。

⚠️ 设计原则（2026-10-03）：
  - **base64 + stdin** 传 SQL 给远程 psql —— 避免多层 shell 引号嵌套
    （实测 `sh -c 'psql -F"|"'` 会让 `-F` 解析成单个 `"`，列被合并）。
  - **decode="utf-8" / "replace"**：Windows 上 stdout.decode() 不带
    `text=True` 会用 cp936，二进制字节会爆 UnicodeDecodeError。
  - **无用户输入**：本模块的所有 query 是模块字面量，params 只来自脚本里
    hardcoded 的 order_id/token。⚠️ 拿用户输入时不要走这里 —— 走 stdin 注入即可。

调用约定：
  ssh_to("panel-host", "ls /tmp")                       # 任意 ssh
  remote_psql(query, params, ssh="panel-host")           # 跑 psql 返 list[list[str]]
"""
from __future__ import annotations

import base64
import re
import subprocess
from typing import Any, Sequence


def ssh_run(host: str, cmd: str, timeout: int = 30) -> str:
    """在 ssh host 上跑 shell 命令，返 stdout。
    失败抛 RuntimeError（带 stderr 摘要）。
    """
    ssh = subprocess.run(
        ["ssh", "-o", "ConnectTimeout=10", "-o", "BatchMode=yes", host, cmd],
        capture_output=True, timeout=timeout,
    )
    if ssh.returncode != 0:
        err = ssh.stderr.decode("utf-8", "replace") if ssh.stderr else ""
        raise RuntimeError(f"ssh 失败: {err.strip()[:200]}")
    out = ssh.stdout.decode("utf-8", "replace") if ssh.stdout else ""
    return out.strip()


def _build_sql(query: str, params: Sequence[Any]) -> str:
    """简单的 %s 占位符替换。参数含单引号时按 SQL 标准转义双单引号。

    ⚠️ 这是给模块字面量 + 短字面参数用的；如要远程用户输入，应当用
    server-side prepared statements 而不是本函数。
    """
    esc = []
    for p in params:
        if isinstance(p, str):
            esc.append("'" + p.replace("'", "''") + "'")
        else:
            esc.append(str(p))
    parts = re.split(r"(%s)", query)
    out_parts = []
    pi = 0
    for part in parts:
        if part == "%s":
            out_parts.append(esc[pi]); pi += 1
        else:
            out_parts.append(part)
    return "".join(out_parts)


def remote_psql(query: str, params: Sequence[Any] = (),
                ssh: str = "panel-host", db_container: str = "hy2-ops-console-db-1",
                timeout: int = 30) -> list[list[str]]:
    """在 ssh 上的 docker db 容器里跑 psql，返 list[list[str]]。

    关键设计：把 SQL base64 编码后塞进命令，避免多层 shell 引号嵌套
    把 `-F"|"` 误解析成单引号。详见 tests/store-e2e.py 注释。

    ⚠️ 假定 db 容器里既有 psql 又有 base64（Debian postgres image 默认有）。
    """
    final_sql = _build_sql(query, params)
    b64 = base64.b64encode(final_sql.encode("utf-8")).decode("ascii")
    cmd = (
        f"docker exec -i {db_container} env sh -c '"
        f"SQL=$(echo {b64} | base64 -d); "
        f"psql -U \"$POSTGRES_USER\" -d \"$POSTGRES_DB\" "
        f"-t -A -F\"|\" -c \"$SQL\"'"
    )
    out = ssh_run(ssh, cmd, timeout=timeout)
    rows = []
    for line in out.splitlines():
        line = line.strip()
        if not line:
            continue
        rows.append(line.split("|"))
    return rows


def remote_mysql(query: str, params: Sequence[Any] = (),
                 ssh: str = "panel-host", container: str = "external-store-1",
                 timeout: int = 30) -> list[list[str]]:
    """在 ssh 上的 docker 容器里跑 mysql 客户端（默认 external-store-1）。

    实现方式：用 docker exec 进容器直接 `mysql -e '...'`，所有参数
    都是字面量；复杂传参建议在容器里写一个 /tmp 脚本传过去。
    """
    final_sql = _build_sql(query, params)
    # 把 SQL 用 mysqlclient 运行；用 stdin 喂避免命令行长
    b64 = base64.b64encode(final_sql.encode("utf-8")).decode("ascii")
    cmd = (
        f"docker exec -i {container} sh -c "
        f"'SQL=$(echo {b64} | base64 -d); mysql -u root \"$MYSQL_DATABASE\" -e \"$SQL\"'"
    )
    out = ssh_run(ssh, cmd, timeout=timeout)
    rows = []
    for line in out.splitlines():
        line = line.strip()
        if not line:
            continue
        rows.append(line.split("\t"))
    return rows


# -------------------------------------------------------------------------
# 远程 Python 脚本执行（替代 cat-five-rounds.py 内嵌的 import os, pymysql）
# -------------------------------------------------------------------------

def remote_python(script: str, ssh: str = "panel-host",
                  container: str = "external-store-1",
                  timeout: int = 30) -> str:
    """在 ssh 上的 docker 容器里跑一段 Python，返 stdout。

    ⚠️ script 是 base64 编码后传入 —— 避免引号问题。
    用法：remote_python("import os; print(os.environ.get('X'))", container=...)
    """
    b64 = base64.b64encode(script.encode("utf-8")).decode("ascii")
    cmd = (
        f"docker exec -i {container} sh -c "
        f"'python3 -c \"exec(__import__(\\\"base64\\\").b64decode(\\\"{b64}\\\".strip()).decode())\"'"
    )
    return ssh_run(ssh, cmd, timeout=timeout)
