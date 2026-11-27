"""部署脚本 deploy/update.sh 的离线单元测试（不依赖远端仓库、Docker 守护进程或业务数据）。"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "deploy" / "update.sh"


def _find_bash():
    """优先选择 Git Bash（避免 Windows 上命中无 cygpath 的 system32 WSL bash.exe）。"""
    if os.name == "nt":
        git = shutil.which("git")
        candidates = []
        if git:
            git_root = Path(git).resolve().parent.parent
            candidates.extend([
                git_root / "bin" / "bash.exe",
                git_root / "usr" / "bin" / "bash.exe",
            ])
        candidates.extend([
            Path(r"C:\Program Files\Git\bin\bash.exe"),
            Path(r"C:\Program Files\Git\usr\bin\bash.exe"),
        ])
        for cand in candidates:
            if cand.is_file():
                return str(cand)
    return shutil.which("bash")


BASH = _find_bash()


@unittest.skipUnless(BASH, "bash is required")
class DeploymentRefTest(unittest.TestCase):
    def _posix(self, path: Path) -> str:
        if os.name == "nt":
            return subprocess.check_output(
                [
                    BASH,
                    "-c",
                    'if command -v cygpath >/dev/null 2>&1; then cygpath -u "$1"; else wslpath -u "$1"; fi',
                    "fixture",
                    str(path),
                ],
                text=True,
                encoding="utf-8",
                errors="replace",
            ).strip()
        return str(path)

    def run_fixture(
        self,
        mode,
        args=(),
        env_text="API_PORT=8080\n",
        compose_text=None,
        docker_script=None,
        curl_script=None,
        extra_env=None,
    ):
        with tempfile.TemporaryDirectory(prefix="teyir-deploy-ref-") as tmp:
            root = Path(tmp)
            (root / ".git").mkdir()
            (root / ".env").write_bytes(env_text.encode("utf-8"))
            if compose_text is not None:
                (root / "docker-compose.yml").write_bytes(compose_text.encode("utf-8"))
            shutil.copy2(SCRIPT, root / "deploy.sh")
            bindir = root / "bin"
            bindir.mkdir()
            # reset --hard 默认以约定退出码 73 提前结束（验证分支切换逻辑），full 模式允许走完健康检查。
            reset_action = (
                'printf \'RESET %s\\n\' "$*"'
                if mode == "full"
                else 'printf \'RESET %s\\n\' "$*"; exit 73'
            )
            (bindir / "git").write_text(
                f'''#!/usr/bin/env bash
case "$*" in
  "rev-parse --git-dir") printf '.git\\n';;
  "rev-parse --short HEAD") printf 'fixture-head\\n';;
  "status --porcelain --untracked-files=no") :;;
  "rev-parse --abbrev-ref HEAD") printf 'fixture-branch\\n';;
  "fetch --prune origin") printf 'FETCHED\\n';;
  "rev-parse --verify --quiet refs/remotes/origin/"*)
    [ "$FIXTURE_MODE" != missing ];;
  "show-ref --verify --quiet refs/heads/"*)
    [ "$FIXTURE_MODE" != new ];;
  checkout*)
    printf 'CHECKOUT %s\\n' "$*"
    [ "$FIXTURE_MODE" != checkout-fails ] || exit 42;;
  "rev-list --count "*) printf '0\\n';;
  "reset --hard "*) {reset_action};;
  *) printf 'unexpected git: %s\\n' "$*" >&2; exit 99;;
esac
''',
                encoding="utf-8",
                newline="\n",
            )
            (bindir / "docker").write_text(
                docker_script or "#!/usr/bin/env bash\nexit 0\n",
                encoding="utf-8",
                newline="\n",
            )
            if curl_script is not None:
                (bindir / "curl").write_text(
                    curl_script, encoding="utf-8", newline="\n"
                )
            for path in bindir.iterdir():
                path.chmod(0o755)
            script = self._posix(root / "deploy.sh")
            binpath = self._posix(bindir)
            env = dict(
                os.environ,
                DEPLOY_NO_BACKUP="1",
                DEPLOY_NO_DB_BACKUP="1",
                FIXTURE_MODE=mode,
            )
            if extra_env:
                env.update(extra_env)
            result = subprocess.run(
                [
                    BASH,
                    "-c",
                    'export PATH="$1:$PATH"; shift; exec bash "$@"',
                    "fixture",
                    binpath,
                    script,
                    *args,
                ],
                env=env,
                text=True,
                encoding="utf-8",
                capture_output=True,
            )
            return result.returncode, result.stdout + result.stderr

    def test_default_ref_is_resolved_before_logging(self):
        code, output = self.run_fixture("existing")
        self.assertEqual(code, 73, output)
        self.assertIn("拉取 origin/fixture-branch", output)
        self.assertNotIn("unbound variable", output)
        self.assertIn("RESET reset --hard origin/fixture-branch", output)

    def test_explicit_new_branch_is_checked_after_fetch(self):
        code, output = self.run_fixture("new", ["new-branch"])
        self.assertEqual(code, 73, output)
        self.assertIn(
            "CHECKOUT checkout -q -b new-branch --track origin/new-branch", output
        )
        self.assertNotIn("-B", output)

    def test_missing_remote_ref_stops_before_checkout(self):
        code, output = self.run_fixture("missing", ["missing-branch"])
        self.assertEqual(code, 1, output)
        self.assertIn("FETCHED", output)
        self.assertNotIn("CHECKOUT", output)
        self.assertNotIn("RESET", output)

    def test_failed_checkout_does_not_force_reset_branch(self):
        code, output = self.run_fixture("checkout-fails")
        self.assertEqual(code, 42, output)
        self.assertEqual(output.count("CHECKOUT"), 1, output)
        self.assertNotIn("-B", output)
        self.assertNotIn("RESET", output)

    def test_api_bind_mount_guard_blocks_override(self):
        compose = "services:\n  api:\n    volumes:\n      - ./releases/old/api:/api:ro\n"
        code, output = self.run_fixture("existing", compose_text=compose)
        self.assertEqual(code, 1, output)
        self.assertIn("镜像里的二进制会被覆盖", output)
        self.assertNotIn("FETCHED", output)

    def test_read_env_handles_quotes_comments_and_wildcard_bind(self):
        env_text = "API_BIND='0.0.0.0' # 对外监听\r\nAPI_PORT=\"19090\" # 自定义端口\r\n"
        curl_stub = (
            "#!/usr/bin/env bash\n"
            "printf 'CURL_ARGS=%s\\n' \"$*\" >&2\n"
            "printf '200'\n"
        )
        code, output = self.run_fixture(
            "full", env_text=env_text, curl_script=curl_stub
        )
        self.assertEqual(code, 0, output)
        self.assertIn("等待健康 (127.0.0.1:19090/api/health)", output)
        self.assertIn("--noproxy *", output)
        self.assertIn("http://127.0.0.1:19090/api/health", output)
        self.assertIn("检测到 .env 含 CRLF 回车符", output)

    def test_ipv6_bind_host_is_bracketed_in_health_url(self):
        env_text = "API_BIND=::1\nAPI_PORT=8088\n"
        curl_stub = (
            "#!/usr/bin/env bash\n"
            "printf 'CURL_ARGS=%s\\n' \"$*\" >&2\n"
            "printf '200'\n"
        )
        code, output = self.run_fixture(
            "full", env_text=env_text, curl_script=curl_stub
        )
        self.assertEqual(code, 0, output)
        self.assertIn("http://[::1]:8088/api/health", output)

    def test_rejects_option_injection_ref_and_supports_export_env(self):
        code_bad, out_bad = self.run_fixture("existing", ["--all"])
        self.assertEqual(code_bad, 1, out_bad)
        self.assertIn("非法分支参数", out_bad)
        self.assertNotIn("FETCHED", out_bad)

        env_text = 'export API_BIND="0.0.0.0" # 对外\nexport API_PORT=\'19191\' # 端口\n'
        curl_stub = (
            "#!/usr/bin/env bash\n"
            "printf 'CURL_ARGS=%s\\n' \"$*\" >&2\n"
            "printf '200'\n"
        )
        code_ok, out_ok = self.run_fixture("full", env_text=env_text, curl_script=curl_stub)
        self.assertEqual(code_ok, 0, out_ok)
        self.assertIn("http://127.0.0.1:19191/api/health", out_ok)


if __name__ == "__main__":
    unittest.main()
