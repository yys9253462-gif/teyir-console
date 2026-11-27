"""一键交互菜单安装脚本 install.sh 的离线回归单元测试（零副作用、无需真实 Docker 守护进程）。"""
import base64
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
INSTALL_SH = ROOT / "install.sh"


def _find_bash():
    """优先使用 Git Bash，避免在 Windows 上命中缺少 cygpath 的 WSL bash.exe。"""
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
class InstallScriptTest(unittest.TestCase):
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

    def _run_script(self, args, env_extra=None, stdin_text=None, script_path=None):
        target = self._posix(script_path or INSTALL_SH)
        env = dict(os.environ)
        env["LC_ALL"] = "C.UTF-8"
        if env_extra:
            env.update(env_extra)
        return subprocess.run(
            [BASH, target, *args],
            input=stdin_text,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            env=env,
            timeout=30,
        )

    def _run_bash_snippet(self, snippet: str, env_extra=None):
        env = dict(os.environ)
        env["LC_ALL"] = "C.UTF-8"
        if env_extra:
            env.update(env_extra)
        return subprocess.run(
            [BASH, "-c", snippet],
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            env=env,
            timeout=30,
        )

    def test_syntax_and_lf_only(self):
        raw = INSTALL_SH.read_bytes()
        self.assertFalse(raw.startswith(b"\xef\xbb\xbf"), "install.sh 不得含 UTF-8 BOM")
        self.assertEqual(raw.count(b"\r"), 0, "install.sh 不得含 CR 回车符")
        res = subprocess.run(
            [BASH, "-n", self._posix(INSTALL_SH)],
            capture_output=True,
            text=True,
            encoding="utf-8",
        )
        self.assertEqual(res.returncode, 0, res.stderr)

    def test_help_and_version_stdout(self):
        res_h = self._run_script(["--help"])
        self.assertEqual(res_h.returncode, 0, res_h.stderr)
        self.assertIn("Teyir 控制台", res_h.stdout)
        self.assertIn("--access-mode", res_h.stdout)
        self.assertIn("--dry-run", res_h.stdout)

        res_v = self._run_script(["--version"])
        self.assertEqual(res_v.returncode, 0, res_v.stderr)
        self.assertRegex(res_v.stdout.strip(), r"^teyir-console [0-9]+\.[0-9]+\.[0-9]+$")

    def test_typo_suggestion_and_missing_value_guard(self):
        res1 = self._run_script(["--uninstal"])
        self.assertNotEqual(res1.returncode, 0)
        self.assertIn("--uninstall", res1.stderr)

        res1b = self._run_script(["--instll"])
        self.assertNotEqual(res1b.returncode, 0)
        self.assertIn("--install", res1b.stderr)

        res2 = self._run_script(["status"])
        self.assertNotEqual(res2.returncode, 0)
        self.assertIn("--status", res2.stderr)

        res3 = self._run_script(["--domain", "--yes"])
        self.assertNotEqual(res3.returncode, 0)
        self.assertIn("漏写了 --domain 的值", res3.stderr)

        res4 = self._run_script(["--install", "--uninstall"])
        self.assertNotEqual(res4.returncode, 0)
        self.assertIn("冲突动作", res4.stderr)

    def test_invalid_inputs_rejected(self):
        cases = [
            (["--access-mode", "bogus", "--dry-run"], "非法值"),
            (["--collector", "maybe", "--dry-run"], "--collector"),
            (["--build-network", "bridge", "--dry-run"], "--build-network"),
            (["--web-port", "99999", "--dry-run", "--install"], "前端 HTTP 端口非法"),
            (["--api-port", "abc", "--dry-run", "--install"], "后端 API 端口非法"),
            (["--domain", "not_a_domain", "--dry-run", "--install"], "域名格式非法"),
            (["--dir", "/", "--dry-run", "--install"], "安装目录不合法"),
            (["--dir", "/etc", "--dry-run", "--install"], "安装目录不合法"),
            (["--admin-password", "short123", "--dry-run", "--install"], "至少需要 12 位"),
            (["--admin-email", "bad-email", "--dry-run", "--install"], "管理员邮箱格式非法"),
            (["--project-name", "Bad Name!", "--dry-run", "--install"], "项目名仅支持"),
            (["--no-domain", "--access-mode", "self", "--dry-run"], "--no-domain"),
        ]
        for argv, expect in cases:
            with self.subTest(argv=argv):
                res = self._run_script(argv)
                self.assertNotEqual(res.returncode, 0, f"应拒绝参数 {argv}")
                self.assertIn(expect, res.stderr)

    def test_dry_run_install_and_domain_normalization(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-dry-") as tmp:
            target = self._posix(Path(tmp) / "app")
            res = self._run_script(
                [
                    "--dry-run",
                    "--yes",
                    "--install",
                    "--dir",
                    target,
                    "--access-mode",
                    "local",
                    "--domain",
                    "HTTPS://Panel.Example.COM/login/",
                    "--web-port=18123",
                    "--web-tls-port=18543",
                    "--api-port=18080",
                    "--sub-name=MyTeyir",
                    "--collector=off",
                ]
            )
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("panel.example.com", res.stderr)
            self.assertIn("# SITE_ADDRESS=", res.stderr)
            self.assertIn("SUBSCRIPTION_DEFAULT_NAME=MyTeyir", res.stderr)
            self.assertIn("ENABLE_COLLECTOR=false", res.stderr)
            self.assertFalse((Path(tmp) / "app").exists(), "--dry-run 不得创建目录")

    def test_state_file_injection_never_executed(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-inj-") as tmp:
            app_dir = Path(tmp) / "inst"
            app_dir.mkdir()
            pwned = Path(tmp) / "PWNED_BY_CONF"
            pwned_posix = self._posix(pwned)
            (app_dir / "install.conf").write_bytes(
                (
                    f"EVIL=$(touch '{pwned_posix}')\n"
                    f"DOMAIN=panel.example.com\n"
                    f"ACCESS_MODE=local\n"
                    f"WEB_PORT=18222\n"
                ).encode("utf-8")
            )
            res = self._run_script(
                ["--dry-run", "--yes", "--install", "--dir", self._posix(app_dir), "--no-domain"]
            )
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertFalse(pwned.exists(), "加载 install.conf 绝不能执行命令注入")
            # 验证 --no-domain 优先于状态文件里的旧 DOMAIN
            self.assertNotIn("绑定域名：      panel.example.com", res.stderr)

    def test_write_env_preserves_secrets_and_site_address_semantics(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-env-") as tmp:
            app_dir = Path(tmp) / "inst"
            app_dir.mkdir()
            script_posix = self._posix(INSTALL_SH)
            app_posix = self._posix(app_dir)

            # 加载脚本函数但不执行 main
            loader = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                f'INSTALL_DIR="{app_posix}"\n'
                f'PROJECT_NAME="teyir-unit"\n'
                f'ACCESS_MODE="direct"\n'
                f'DOMAIN=""\n'
                f'WEB_BIND="0.0.0.0"\n'
                f'WEB_PORT="18088"\n'
                f'WEB_TLS_PORT="18443"\n'
                f'API_BIND="127.0.0.1"\n'
                f'API_PORT="18080"\n'
                f'SUB_NAME="Teyir"\n'
                f'ENABLE_COLLECTOR="true"\n'
                f'write_env_file\n'
            )
            res1 = self._run_bash_snippet(loader)
            self.assertEqual(res1.returncode, 0, res1.stderr)
            env_path = app_dir / ".env"
            text1 = env_path.read_text(encoding="utf-8")
            self.assertIn("# SITE_ADDRESS=\n", text1)
            self.assertNotIn("\nSITE_ADDRESS=\n", text1)

            kv1 = {}
            for line in text1.splitlines():
                if "=" in line and not line.startswith("#"):
                    k, v = line.split("=", 1)
                    kv1[k] = v

            self.assertEqual(len(base64.b64decode(kv1["CREDENTIAL_KEY"])), 32)
            self.assertRegex(kv1["POSTGRES_PASSWORD"], r"^[A-Za-z0-9_-]{32,}$")
            self.assertGreaterEqual(len(kv1["JWT_SECRET"]), 32)
            self.assertGreaterEqual(len(kv1["SEED_ADMIN_PASSWORD"]), 12)

            # 第二次切到 self 模式带域名，核对旧密钥原封不动且 SITE_ADDRESS 启用
            loader2 = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                f'INSTALL_DIR="{app_posix}"\n'
                f'PROJECT_NAME="teyir-unit"\n'
                f'ACCESS_MODE="self"\n'
                f'DOMAIN="panel.example.com"\n'
                f'WEB_BIND="0.0.0.0"\n'
                f'WEB_PORT="80"\n'
                f'WEB_TLS_PORT="443"\n'
                f'API_BIND="127.0.0.1"\n'
                f'API_PORT="18080"\n'
                f'SUB_NAME="TeyirPro"\n'
                f'ENABLE_COLLECTOR="false"\n'
                f'write_env_file\n'
            )
            res2 = self._run_bash_snippet(loader2)
            self.assertEqual(res2.returncode, 0, res2.stderr)
            text2 = env_path.read_text(encoding="utf-8")
            self.assertIn("\nSITE_ADDRESS=panel.example.com\n", text2)
            kv2 = {}
            for line in text2.splitlines():
                if "=" in line and not line.startswith("#"):
                    k, v = line.split("=", 1)
                    kv2[k] = v

            self.assertEqual(kv2["CREDENTIAL_KEY"], kv1["CREDENTIAL_KEY"], "重配不得覆盖 CREDENTIAL_KEY")
            self.assertEqual(kv2["POSTGRES_PASSWORD"], kv1["POSTGRES_PASSWORD"], "重配不得覆盖 POSTGRES_PASSWORD")
            self.assertEqual(kv2["JWT_SECRET"], kv1["JWT_SECRET"], "重配不得覆盖 JWT_SECRET")
            self.assertEqual(kv2["SUBSCRIPTION_DEFAULT_NAME"], "TeyirPro")
            self.assertEqual(kv2["ENABLE_COLLECTOR"], "false")

    def test_caddyfile_conflict_detection_and_managed_block_strip(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-caddy-") as tmp:
            caddy = Path(tmp) / "Caddyfile"
            caddy.write_bytes(
                (
                    "existing.example.com {\n"
                    "  respond \"ok\" 200\n"
                    "}\n"
                    "axexamplexcom {\n"
                    "  respond \"ok\" 200\n"
                    "}\n"
                ).encode("utf-8")
            )
            script_posix = self._posix(INSTALL_SH)
            caddy_posix = self._posix(caddy)
            snippet = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                f'if caddyfile_has_domain_conflict "{caddy_posix}" "existing.example.com"; then echo HIT_EXISTING; fi\n'
                f'if caddyfile_has_domain_conflict "{caddy_posix}" "a.example.com"; then echo HIT_REGEX_BUG; else echo SAFE_DOT_ESCAPE; fi\n'
                f'PROJECT_NAME="teyir-test"\n'
                f'gen_external_caddy_block "panel.example.com" "127.0.0.1:18080" >> "{caddy_posix}"\n'
                f'if caddyfile_has_domain_conflict "{caddy_posix}" "panel.example.com"; then echo HIT_OWN_BUG; else echo OWN_BLOCK_IDEMPOTENT; fi\n'
                f'strip_managed_block_to_stdout "{caddy_posix}"\n'
            )
            res = self._run_bash_snippet(snippet)
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("HIT_EXISTING", res.stdout)
            self.assertIn("SAFE_DOT_ESCAPE", res.stdout)
            self.assertIn("OWN_BLOCK_IDEMPOTENT", res.stdout)
            self.assertNotIn("panel.example.com", res.stdout)
            self.assertIn("existing.example.com", res.stdout)

    def test_self_location_finds_adjacent_install_conf(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-self-") as tmp:
            app_dir = Path(tmp) / "custom-teyir"
            app_dir.mkdir()
            copied = app_dir / "install.sh"
            copied.write_bytes(INSTALL_SH.read_bytes())
            (app_dir / "install.conf").write_bytes(
                b"PROJECT_NAME=custom-proj\nACCESS_MODE=local\nWEB_BIND=127.0.0.1\nWEB_PORT=18999\nWEB_TLS_PORT=18998\nAPI_BIND=127.0.0.1\nAPI_PORT=18997\nSUB_NAME=CustomSub\nENABLE_COLLECTOR=false\n"
            )
            res = self._run_script(["--status"], script_path=copied)
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("custom-proj", res.stderr)
            self.assertIn("CustomSub", res.stderr)
            self.assertIn("18999", res.stderr)

    def test_status_distinguishes_missing_dir_vs_interrupted_install(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-state-") as tmp:
            missing = Path(tmp) / "never-installed"
            res1 = self._run_script(["--status", "--dir", self._posix(missing)])
            self.assertNotEqual(res1.returncode, 0)
            self.assertIn("还没安装过", res1.stderr)

            half = Path(tmp) / "half-installed"
            half.mkdir()
            res2 = self._run_script(["--status", "--dir", self._posix(half)])
            self.assertNotEqual(res2.returncode, 0)
            self.assertIn("中途中断", res2.stderr)

    def test_local_mode_clears_detected_external_caddy_file(self):
        script_posix = self._posix(INSTALL_SH)
        snippet = (
            f'eval "$(sed \'$d\' "{script_posix}")"\n'
            'detect_existing_proxy() { DETECTED_PROXY_TYPE="host-caddy"; CADDY_FILE="/etc/caddy/Caddyfile"; CADDY_INNER="/etc/caddy/Caddyfile"; }\n'
            'is_port_used() { return 1; }\n'
            'ACCESS_MODE="local"\n'
            'DOMAIN=""\n'
            'plan_access_and_ports\n'
            'printf "MODE=%s FILE=%s\\n" "$CADDY_MODE" "$CADDY_FILE"\n'
        )
        res = self._run_bash_snippet(snippet)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn("MODE=none FILE=", res.stdout)
        self.assertNotIn("/etc/caddy/Caddyfile", res.stdout)


if __name__ == "__main__":
    unittest.main()
