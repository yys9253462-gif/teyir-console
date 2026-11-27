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

    def test_env_special_chars_roundtrip_and_custom_vars_preserved(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-envrt-") as tmp:
            app_dir = Path(tmp) / "inst"
            app_dir.mkdir()
            valid_key = base64.b64encode(b"0123456789abcdef0123456789abcdef").decode("ascii")
            (app_dir / ".env").write_bytes(
                (
                    f"POSTGRES_PASSWORD=keep-pg-pass-12345678901234567890\n"
                    f"JWT_SECRET=keep-jwt-secret-12345678901234567890123456\n"
                    f"CREDENTIAL_KEY={valid_key}\n"
                    f"SEED_ADMIN_EMAIL=keep@example.com\n"
                    f"SEED_ADMIN_PASSWORD='OldPass$2026! #1' # 行内注释\n"
                    f"GEO_OFFLINE_DB=/data/custom-geo.json\n"
                    f"IP_PURITY_OFFLINE_DB=/data/custom-purity.json\n"
                    f"IP_PURITY_API_BASE_URL=https://purity.example.org\n"
                    f"TRUSTED_PROXY_CIDRS=10.10.0.0/16,127.0.0.1/32\n"
                    f"PORTAL_TLS_PINS='{{\"node.example.com:8443\":\"AABBCC\"}}'\n"
                    f"SUBSCRIPTION_PUBLIC_BASE_URL=https://sub.example.org\n"
                    f"ALLOWED_ORIGIN=https://legacy.example.org,https://extra.example.org\n"
                    f"export HTTP_PROXY=http://127.0.0.1:7890\n"
                    f"MY_CUSTOM_FLAG=\"keep me # untouched\"\n"
                ).encode("utf-8")
            )
            script_posix = self._posix(INSTALL_SH)
            app_posix = self._posix(app_dir)
            snippet = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                f'INSTALL_DIR="{app_posix}"\n'
                f'PROJECT_NAME="teyir-rt"\n'
                f'ACCESS_MODE="host-caddy"\n'
                f'DOMAIN="panel.example.com"\n'
                f'SAVED_DOMAIN="old.example.com"\n'
                f'WEB_BIND="127.0.0.1"\n'
                f'WEB_PORT="18080"\n'
                f'WEB_TLS_PORT="18443"\n'
                'API_BIND="127.0.0.1"\n'
                'API_PORT="8080"\n'
                "SUB_NAME='Teyir $VIP #1'\n"
                'ENABLE_COLLECTOR="true"\n'
                'write_env_file\n'
                f'printf "PASS=%s\\n" "$(read_kv_file "{app_posix}/.env" SEED_ADMIN_PASSWORD)"\n'
                f'printf "SUB=%s\\n" "$(read_kv_file "{app_posix}/.env" SUBSCRIPTION_DEFAULT_NAME)"\n'
                f'printf "PINS=%s\\n" "$(read_kv_file "{app_posix}/.env" PORTAL_TLS_PINS)"\n'
                f'printf "PDB=%s\\n" "$(read_kv_file "{app_posix}/.env" IP_PURITY_OFFLINE_DB)"\n'
                f'printf "PURL=%s\\n" "$(read_kv_file "{app_posix}/.env" IP_PURITY_API_BASE_URL)"\n'
                f'printf "GDB=%s\\n" "$(read_kv_file "{app_posix}/.env" GEO_OFFLINE_DB)"\n'
                f'printf "CIDR=%s\\n" "$(read_kv_file "{app_posix}/.env" TRUSTED_PROXY_CIDRS)"\n'
                f'printf "SUBPUB=%s\\n" "$(read_kv_file "{app_posix}/.env" SUBSCRIPTION_PUBLIC_BASE_URL)"\n'
                f'printf "ORIG=%s\\n" "$(read_kv_file "{app_posix}/.env" ALLOWED_ORIGIN)"\n'
                f'printf "PROXY=%s\\n" "$(read_kv_file "{app_posix}/.env" HTTP_PROXY)"\n'
                f'printf "CUST=%s\\n" "$(read_kv_file "{app_posix}/.env" MY_CUSTOM_FLAG)"\n'
            )
            res = self._run_bash_snippet(snippet)
            self.assertEqual(res.returncode, 0, res.stderr)
            out = res.stdout
            self.assertIn("PASS=OldPass$2026! #1\n", out)
            self.assertIn("SUB=Teyir $VIP #1\n", out)
            self.assertIn('PINS={"node.example.com:8443":"AABBCC"}\n', out)
            self.assertIn("PDB=/data/custom-purity.json\n", out)
            self.assertIn("PURL=https://purity.example.org\n", out)
            self.assertIn("GDB=/data/custom-geo.json\n", out)
            self.assertIn("CIDR=10.10.0.0/16,127.0.0.1/32\n", out)
            self.assertIn("SUBPUB=https://sub.example.org\n", out)
            self.assertIn("ORIG=https://panel.example.com,https://legacy.example.org,https://extra.example.org\n", out)
            self.assertIn("PROXY=http://127.0.0.1:7890\n", out)
            self.assertIn("CUST=keep me # untouched\n", out)
            # 含 $ 和空格的值必须被安全引号包裹，防止 Docker Compose 变量展开
            env_raw = (app_dir / ".env").read_text(encoding="utf-8")
            self.assertIn("SEED_ADMIN_PASSWORD='OldPass$2026! #1'\n", env_raw)
            self.assertIn("SUBSCRIPTION_DEFAULT_NAME='Teyir $VIP #1'\n", env_raw)

    def test_invalid_credential_key_in_existing_env_rejected_without_overwrite(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-cred-") as tmp:
            app_dir = Path(tmp) / "inst"
            app_dir.mkdir()
            env_file = app_dir / ".env"
            orig_content = (
                "POSTGRES_PASSWORD=keep-pg-pass-12345678901234567890\n"
                "JWT_SECRET=keep-jwt-secret-12345678901234567890123456\n"
                "CREDENTIAL_KEY=not-valid-base64-url-_key-of-bad-length\n"
                "SEED_ADMIN_EMAIL=a@example.com\n"
                "SEED_ADMIN_PASSWORD=ValidLongPassword123\n"
            )
            env_file.write_bytes(orig_content.encode("utf-8"))
            script_posix = self._posix(INSTALL_SH)
            app_posix = self._posix(app_dir)
            snippet = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                f'INSTALL_DIR="{app_posix}"\n'
                'PROJECT_NAME="teyir-cred"; ACCESS_MODE="local"; DOMAIN=""\n'
                'WEB_BIND="127.0.0.1"; WEB_PORT="18080"; WEB_TLS_PORT="18443"\n'
                'API_BIND="127.0.0.1"; API_PORT="8080"; SUB_NAME="Teyir"; ENABLE_COLLECTOR="true"\n'
                'write_env_file\n'
            )
            res = self._run_bash_snippet(snippet)
            self.assertNotEqual(res.returncode, 0)
            self.assertIn("CREDENTIAL_KEY 格式非法", res.stderr)
            self.assertEqual(env_file.read_text(encoding="utf-8"), orig_content, "拒绝时绝不得覆写原 .env")

    def test_caddyfile_conflict_multidomain_case_port_and_block_interior_idempotent(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-caddy2-") as tmp:
            caddy = Path(tmp) / "Caddyfile"
            orig_bytes = (
                "{\n"
                "  admin off\n"
                "}\n"
                "# comment.example.com {\n"
                "a.example.com,HTTPS://Multi.Example.COM:443 {\n"
                "  reverse_proxy 127.0.0.1:9000\n"
                "}\n"
                "pathsite.example.com/sub/* {\n"
                "  reverse_proxy 127.0.0.1:9001\n"
                "}\n"
                "other.example.com {\n"
                "  reverse_proxy {\n"
                "    to interior.example.com:8080\n"
                "  }\n"
                "}\n"
            ).encode("utf-8")
            caddy.write_bytes(orig_bytes)
            script_posix = self._posix(INSTALL_SH)
            caddy_posix = self._posix(caddy)
            snippet = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                f'caddyfile_has_domain_conflict "{caddy_posix}" "multi.example.com" && echo HIT_MULTI\n'
                f'caddyfile_has_domain_conflict "{caddy_posix}" "pathsite.example.com" && echo HIT_PATH\n'
                f'caddyfile_has_domain_conflict "{caddy_posix}" "comment.example.com" && echo BAD_COMMENT || echo OK_COMMENT\n'
                f'caddyfile_has_domain_conflict "{caddy_posix}" "interior.example.com" && echo BAD_INTERIOR || echo OK_INTERIOR\n'
                f'caddyfile_has_domain_conflict "{caddy_posix}" "sub.multi.example.com" && echo BAD_SUB || echo OK_SUB\n'
                f'PROJECT_NAME="teyir-idem"\n'
                'for i in 1 2 3; do\n'
                f'  strip_managed_block_to_stdout "{caddy_posix}" > "{caddy_posix}.tmp"\n'
                f'  gen_external_caddy_block "panel.example.com" "127.0.0.1:18080" >> "{caddy_posix}.tmp"\n'
                f'  cat "{caddy_posix}.tmp" > "{caddy_posix}"\n'
                'done\n'
                f'strip_managed_block_to_stdout "{caddy_posix}" > "{caddy_posix}.clean"\n'
            )
            res = self._run_bash_snippet(snippet)
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("HIT_MULTI", res.stdout)
            self.assertIn("HIT_PATH", res.stdout)
            self.assertIn("OK_COMMENT", res.stdout)
            self.assertIn("OK_INTERIOR", res.stdout)
            self.assertIn("OK_SUB", res.stdout)
            # 反复追加并摘除后字节级完全还原，不堆积空行
            self.assertEqual((Path(tmp) / "Caddyfile.clean").read_bytes(), orig_bytes)

    def test_interactive_menu_isolates_errexit_and_die_with_rollback(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-menu-") as tmp:
            caddy = Path(tmp) / "Caddyfile"
            caddy.write_bytes(b"orig.example.com {\n  respond \"ok\" 200\n}\n")
            bak = Path(tmp) / "Caddyfile.bak"
            bak.write_bytes(caddy.read_bytes())
            script_posix = self._posix(INSTALL_SH)
            caddy_posix = self._posix(caddy)
            bak_posix = self._posix(bak)
            snippet = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                'validate_external_caddy() { return 0; }\n'
                'reload_external_caddy() { return 0; }\n'
                'bad_step() {\n'
                f'  CADDY_MODE="host-caddy"; CADDY_FILE="{caddy_posix}"; CADDY_BACKUP_FILE="{bak_posix}"; CADDY_BACKED_UP=1\n'
                f'  printf "CORRUPTED\\n" > "{caddy_posix}"\n'
                '  false\n'
                '  echo "LEAKED_AFTER_FALSE"\n'
                '}\n'
                'die_step() { die "boom_in_menu"; echo "LEAKED_AFTER_DIE"; }\n'
                'run_menu_action bad_step\n'
                'echo "AFTER_BAD_RC=$LAST_MENU_RC"\n'
                'run_menu_action die_step\n'
                'echo "AFTER_DIE_RC=$LAST_MENU_RC"\n'
                'echo "OUTER_MENU_ALIVE"\n'
            )
            res = self._run_bash_snippet(snippet)
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertNotIn("LEAKED_AFTER_FALSE", res.stdout)
            self.assertNotIn("LEAKED_AFTER_DIE", res.stdout)
            self.assertIn("AFTER_BAD_RC=1", res.stdout)
            self.assertIn("AFTER_DIE_RC=1", res.stdout)
            self.assertIn("OUTER_MENU_ALIVE", res.stdout)
            self.assertEqual(
                caddy.read_text(encoding="utf-8"),
                'orig.example.com {\n  respond "ok" 200\n}\n',
                "子壳失败时必须触发 on_fail 还原共享 Caddyfile",
            )

    def test_menu_configure_clears_optional_integrations_and_guards_self_ports(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-cfg-") as tmp:
            app_dir = Path(tmp) / "inst"
            app_dir.mkdir()
            valid_key = base64.b64encode(b"abcdef0123456789abcdef0123456789").decode("ascii")
            (app_dir / "install.conf").write_bytes(
                (
                    "PROJECT_NAME=teyir-cfg\n"
                    "ACCESS_MODE=self\n"
                    "DOMAIN=panel.example.com\n"
                    "WEB_BIND=0.0.0.0\n"
                    "WEB_PORT=80\n"
                    "WEB_TLS_PORT=443\n"
                    "API_BIND=127.0.0.1\n"
                    "API_PORT=8080\n"
                    "SUB_NAME=Teyir\n"
                    "ENABLE_COLLECTOR=true\n"
                ).encode("utf-8")
            )
            (app_dir / ".env").write_bytes(
                (
                    f"POSTGRES_PASSWORD=keep-pg-pass-12345678901234567890\n"
                    f"JWT_SECRET=keep-jwt-secret-12345678901234567890123456\n"
                    f"CREDENTIAL_KEY={valid_key}\n"
                    f"SEED_ADMIN_EMAIL=admin@teyir.local\n"
                    f"SEED_ADMIN_PASSWORD=ValidLongPassword123\n"
                    f"STORE_RENEW_BASE_URL=https://shop.example.com/\n"
                    f"HY2_INSTALL_SCRIPT_URL=https://raw.githubusercontent.com/org/repo/main/install.sh\n"
                    f"HY2_INSTALLER_REPO=org/repo\n"
                ).encode("utf-8")
            )
            script_posix = self._posix(INSTALL_SH)
            app_posix = self._posix(app_dir)
            # 在 menu_configure 中依次：
            # 1) 选 5，将 3 个外部联动地址全部输入 '-' 清空；
            # 2) 选 2（self 模式），验证不问 WEB_PORT，只把 API_PORT 改为 18081；
            # 3) 选 0 退出子菜单
            snippet = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                f'INSTALL_DIR="{app_posix}"\n'
                'require_root() { return 0; }\n'
                'detect_existing_proxy() { DETECTED_PROXY_TYPE="none"; }\n'
                'is_port_used() { return 1; }\n'
                'start_and_verify_stack() { return 0; }\n'
                'apply_external_caddy() { return 0; }\n'
                'NO_SHORTCUT=1\n'
                'TEYIR_FORCE_STDIN_TTY=1 menu_configure <<\'INP\'\n'
                '5\n'
                '-\n'
                '-\n'
                '-\n'
                '2\n'
                '18081\n'
                '0\n'
                'INP\n'
            )
            res = self._run_bash_snippet(snippet)
            self.assertEqual(res.returncode, 0, res.stderr)
            env_text = (app_dir / ".env").read_text(encoding="utf-8")
            self.assertIn("STORE_RENEW_BASE_URL=\n", env_text)
            self.assertIn("HY2_INSTALL_SCRIPT_URL=\n", env_text)
            self.assertIn("HY2_INSTALLER_REPO=\n", env_text)
            self.assertIn("WEB_PORT=80\n", env_text)
            self.assertIn("WEB_TLS_PORT=443\n", env_text)
            self.assertIn("API_PORT=18081\n", env_text)
            self.assertIn("前端端口固定使用 80/443", res.stderr)

    def test_parse_args_conflicts_and_strict_validators(self):
        bad_cases = [
            (["--domain", "panel.example.com", "--no-domain", "--dry-run"], "不能同时指定 --domain 与 --no-domain"),
            (["--no-domain", "--domain", "panel.example.com", "--dry-run"], "不能同时指定 --domain 与 --no-domain"),
            (["--no-domain", "--access-mode", "host-caddy", "--dry-run"], "--no-domain 不能与 --access-mode host-caddy"),
            (["--domain", "panel.example.com", "--access-mode", "direct", "--dry-run"], "--access-mode direct"),
            (["--web-port", "08080", "--dry-run", "--install"], "前端 HTTP 端口非法"),
            (["--web-bind", "999.999.999.999", "--dry-run", "--install"], "前端监听地址非法"),
            (["--dir", "/opt/foo/../etc", "--dry-run", "--install"], "安装目录不合法"),
            (["--dir", "/", "--dry-run", "--install"], "绝对路径）：/"),
            (["--branch", "-bad", "--dry-run", "--install"], "分支或标签名不合法"),
            (["--branch", "a..b", "--dry-run", "--install"], "分支或标签名不合法"),
        ]
        for argv, expect in bad_cases:
            with self.subTest(argv=argv):
                res = self._run_script(argv)
                self.assertNotEqual(res.returncode, 0, f"应拦截 {argv}")
                self.assertIn(expect, res.stderr)

        # 合法 Punycode 域名、IPv6 绑定与等号传值含已知标志名应放行
        with tempfile.TemporaryDirectory(prefix="teyir-inst-ok-") as tmp:
            target = self._posix(Path(tmp) / "app")
            res_ok = self._run_script(
                [
                    "--dry-run",
                    "--yes",
                    "--install",
                    "--dir",
                    target,
                    "--access-mode",
                    "local",
                    "--domain",
                    "panel.xn--fiqs8s",
                    "--api-bind",
                    "::1",
                    "--admin-password=--skip-dns-check",
                ]
            )
            self.assertEqual(res_ok.returncode, 0, res_ok.stderr)
            self.assertIn("panel.xn--fiqs8s", res_ok.stderr)

    def test_port_range_expansion_nat_avoidance_and_saved_port_recheck(self):
        script_posix = self._posix(INSTALL_SH)
        snippet = (
            f'eval "$(sed \'$d\' "{script_posix}")"\n'
            'printf "0.0.0.0:18080-18082->18080-18082/tcp, :::8088->80/tcp\\n" | extract_docker_published_ports | tr "\\n" ","\n'
            'printf "\\n"\n'
            'load_used_ports() { USED_PORTS_CACHE="8080"; RESERVED_TCP_RANGES="18080-18084"; RESERVED_UDP_RANGES="20000-20010"; }\n'
            'own_project_holds_port() { return 1; }\n'
            'other_container_holds_port() { return 1; }\n'
            'printf "PICK=%s\\n" "$(pick_free_port 18080)"\n'
            'warn_if_udp_hop_overlap 20005 "测试"\n'
            # 验证 install.conf 回填但被占用的旧端口会被自动避让
            'detect_existing_proxy() { DETECTED_PROXY_TYPE="none"; }\n'
            'ACCESS_MODE="direct"; DOMAIN=""; FLAG_WEB_PORT=0; WEB_PORT="18081"; FLAG_API_PORT=0; API_PORT="8080"\n'
            'plan_access_and_ports\n'
            'printf "REPLAN_WEB=%s REPLAN_API=%s\\n" "$WEB_PORT" "$API_PORT"\n'
        )
        res = self._run_bash_snippet(snippet)
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn("18080,18081,18082,8088,", res.stdout)
        self.assertIn("PICK=18085", res.stdout)
        self.assertIn("UDP 端口跳跃区间（20000-20010）", res.stderr)
        self.assertIn("REPLAN_WEB=", res.stdout)
        self.assertNotIn("REPLAN_WEB=18081", res.stdout)
        self.assertNotIn("REPLAN_API=8080", res.stdout)

    def test_docker_caddy_host_network_dualstack_gateway_and_custom_config(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-dcaddy-") as tmp:
            root = Path(tmp)
            cfg_dir = root / "caddy_conf"
            (cfg_dir / "conf").mkdir(parents=True)
            cfg_file = cfg_dir / "conf" / "Caddyfile"
            cfg_file.write_bytes(b"a.example.com { respond \"ok\" 200 }\n")
            cfg_dir_posix = self._posix(cfg_dir)
            cfg_file_posix = self._posix(cfg_file)
            script_posix = self._posix(INSTALL_SH)

            # 场景 A：双栈网络 + 父目录挂载 + 自定义 --config
            snippet_bridge = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                'docker() {\n'
                '  case "$*" in\n'
                "    *'.Args'*) printf 'caddy run --config /etc/caddy/conf/Caddyfile ' ;;\n"
                f"    *'.Mounts'*) printf '/etc/caddy|{cfg_dir_posix}\\n' ;;\n"
                "    *'.HostConfig.NetworkMode'*) printf 'edge-net\\n' ;;\n"
                "    *'.Gateway'*) printf 'fd00::1 172.22.0.1 \\n' ;;\n"
                "    *'.NetworkSettings.Networks'*) printf 'edge-net \\n' ;;\n"
                '  esac\n'
                '}\n'
                'inspect_docker_caddy_details caddy-proxy\n'
                'printf "FILE=%s INNER=%s UP=%s\\n" "$CADDY_FILE" "$CADDY_INNER" "$UPSTREAM_HOST"\n'
            )
            res_a = self._run_bash_snippet(snippet_bridge)
            self.assertEqual(res_a.returncode, 0, res_a.stderr)
            self.assertIn(f"FILE={cfg_file_posix}", res_a.stdout)
            self.assertIn("INNER=/etc/caddy/conf/Caddyfile", res_a.stdout)
            self.assertIn("UP=172.22.0.1", res_a.stdout)

            # 场景 B：host 网络模式下的 docker-caddy 上游地址必须为 127.0.0.1
            snippet_host = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                'docker() {\n'
                '  case "$*" in\n'
                "    *'.HostConfig.NetworkMode'*) printf 'host\\n' ;;\n"
                f"    *'.Mounts'*) printf '/etc/caddy/Caddyfile|{cfg_file_posix}\\n' ;;\n"
                "    *'.NetworkSettings.Networks'*) printf 'host \\n' ;;\n"
                '    *) : ;;\n'
                '  esac\n'
                '}\n'
                'inspect_docker_caddy_details caddy-host\n'
                'printf "NET=%s UP=%s\\n" "$CADDY_NET" "$UPSTREAM_HOST"\n'
            )
            res_b = self._run_bash_snippet(snippet_host)
            self.assertEqual(res_b.returncode, 0, res_b.stderr)
            self.assertIn("NET=host UP=127.0.0.1", res_b.stdout)

    def test_prune_backups_pipefail_safe_and_save_state_keeps_newer_repo_script(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-prune-") as tmp:
            root = Path(tmp)
            app_dir = root / "inst"
            app_dir.mkdir()
            (app_dir / "install.sh").write_bytes(b"#!/usr/bin/env bash\n# NEW_REPO_VERSION\n")
            old_self = root / "old-install.sh"
            old_self.write_bytes(b"#!/usr/bin/env bash\n# OLD_HOME_COPY\n")
            (root / "Caddyfile.bak-20260101-mine").write_bytes(b"user-backup")
            for idx in range(1, 6):
                p = root / f"Caddyfile.bak-2026010{idx}-000000-preTeyir"
                p.write_bytes(f"bak-{idx}".encode("utf-8"))
                os.utime(p, (1700000000 + idx * 10, 1700000000 + idx * 10))

            script_posix = self._posix(INSTALL_SH)
            root_posix = self._posix(root)
            app_posix = self._posix(app_dir)
            old_self_posix = self._posix(old_self)
            snippet = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                # 空匹配在 pipefail 下必须返回 0
                f'prune_backups 3 "{app_posix}" "no-such-prefix-" ".bak"\n'
                # 保留最新 3 份，不误删用户自己的备份
                f'prune_backups 3 "{root_posix}" "Caddyfile.bak-" "-preTeyir"\n'
                # save_state 绝不能用外部旧脚本覆盖目录内已有的新版仓库 install.sh
                f'INSTALL_DIR="{app_posix}"\n'
                f'SELF_PATH="{old_self_posix}"\n'
                'NO_SHORTCUT=1\n'
                'save_state\n'
            )
            res = self._run_bash_snippet(snippet)
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertTrue((root / "Caddyfile.bak-20260101-mine").exists(), "不得删除用户自定义备份")
            kept = sorted(p.name for p in root.glob("Caddyfile.bak-*-preTeyir"))
            self.assertEqual(len(kept), 3)
            self.assertIn("Caddyfile.bak-20260105-000000-preTeyir", kept)
            self.assertIn("NEW_REPO_VERSION", (app_dir / "install.sh").read_text(encoding="utf-8"))

    def test_uninstall_passes_env_file_and_shortcut_priv_cmd(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-uninst-") as tmp:
            app_dir = Path(tmp) / "inst"
            app_dir.mkdir()
            (app_dir / "docker-compose.yml").write_bytes(b"services: {}\n")
            (app_dir / ".env").write_bytes(b"COMPOSE_PROJECT_NAME=teyir-u\nWEB_PORT=18080\nAPI_PORT=8080\n")
            (app_dir / "install.conf").write_bytes(b"PROJECT_NAME=teyir-u\nACCESS_MODE=local\nWEB_PORT=18080\nAPI_PORT=8080\n")
            script_posix = self._posix(INSTALL_SH)
            app_posix = self._posix(app_dir)
            call_log = Path(tmp) / "docker.log"
            call_log_posix = self._posix(call_log)
            snippet = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                f'INSTALL_DIR="{app_posix}"\n'
                'ASSUME_YES=1\n'
                'require_root() { return 0; }\n'
                'docker_bin_ok() { return 0; }\n'
                'docker_daemon_ok() { return 0; }\n'
                f'docker() {{ printf "DOCKER_CALL=%s\\n" "$*" >> "{call_log_posix}"; return 0; }}\n'
                'do_uninstall\n'
                # 验证快捷命令模式下不拼出 bash teyir
                'SELF="teyir"; SELF_IS_SHORTCUT=1; id() { printf "1000"; }\n'
                'printf "PRIV=%s\\n" "$(render_priv_cmd --upgrade)"\n'
            )
            res = self._run_bash_snippet(snippet)
            self.assertEqual(res.returncode, 0, res.stderr)
            docker_log = call_log.read_text(encoding="utf-8")
            self.assertIn(f"--env-file {app_posix}/.env down --remove-orphans", docker_log)
            self.assertNotIn("bash teyir", res.stdout)
            self.assertIn("teyir --upgrade", res.stdout)

    def test_tarball_check_update_and_upgrade_preserves_runtime_files(self):
        with tempfile.TemporaryDirectory(prefix="teyir-inst-tar-") as tmp:
            root = Path(tmp)
            app_dir = root / "inst"
            app_dir.mkdir()
            (app_dir / ".env").write_bytes(b"KEEP_ME=secret-token\n")
            (app_dir / "install.conf").write_bytes(
                b"PROJECT_NAME=teyir-tar\nREPO=org/demo\nBRANCH=main\nSOURCE_COMMIT=1111111\nACCESS_MODE=local\nWEB_PORT=18080\nAPI_PORT=8080\n"
            )
            (app_dir / "geo-data").mkdir()
            (app_dir / "geo-data" / "geo-offline.json").write_bytes(b'{"entries":[]}')
            (app_dir / "docker-compose.yml").write_bytes(b"# old compose\n")

            # 构造一份模拟 GitHub tarball（顶层单目录 + 新版文件 + 同名 .env 不应覆盖目标）
            src_pkg = root / "pkg" / "demo-main"
            src_pkg.mkdir(parents=True)
            (src_pkg / "docker-compose.yml").write_bytes(b"# new compose v2\n")
            (src_pkg / "Dockerfile.api").write_bytes(b"FROM scratch\n")
            (src_pkg / ".env").write_bytes(b"SHOULD_NOT_OVERWRITE=1\n")
            tar_path = root / "fake.tar.gz"
            subprocess.check_call(["tar", "-C", str(root / "pkg"), "-czf", str(tar_path), "demo-main"])

            script_posix = self._posix(INSTALL_SH)
            app_posix = self._posix(app_dir)
            tar_posix = self._posix(tar_path)
            snippet = (
                f'eval "$(sed \'$d\' "{script_posix}")"\n'
                f'INSTALL_DIR="{app_posix}"\n'
                'curl() {\n'
                '  case "$*" in\n'
                '    *"api.github.com/repos/"*)\n'
                '      printf \'{"sha": "2222222abcdef"}\\n\'\n'
                '      ;;\n'
                '    *"codeload.github.com/"*)\n'
                '      local out="" prev=""\n'
                '      for a in "$@"; do\n'
                '        if [ "$prev" = "-o" ]; then out="$a"; fi\n'
                '        prev="$a"\n'
                '      done\n'
                f'      cp "{tar_posix}" "$out"\n'
                '      ;;\n'
                '  esac\n'
                '}\n'
                'do_check_update\n'
                f'download_source_tarball_to "{app_posix}"\n'
                'printf "NEW_SHA=%s\\n" "$SOURCE_COMMIT"\n'
            )
            res = self._run_bash_snippet(snippet)
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("2222222", res.stderr)
            self.assertIn("NEW_SHA=2222222", res.stdout)
            self.assertIn("new compose v2", (app_dir / "docker-compose.yml").read_text(encoding="utf-8"))
            self.assertEqual((app_dir / ".env").read_text(encoding="utf-8"), "KEEP_ME=secret-token\n")
            self.assertTrue((app_dir / "geo-data" / "geo-offline.json").exists())


if __name__ == "__main__":
    unittest.main()
