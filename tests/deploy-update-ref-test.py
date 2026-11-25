"""Exercise deployment ref selection without a remote, Docker daemon, or app data."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "deploy" / "update.sh"
BASH = shutil.which("bash")


@unittest.skipUnless(BASH, "bash is required")
class DeploymentRefTest(unittest.TestCase):
    def run_fixture(self, mode, args=()):
        with tempfile.TemporaryDirectory(prefix="hy2-deploy-ref-") as tmp:
            root = Path(tmp)
            (root / ".git").mkdir()
            (root / ".env").write_text("API_PORT=8080\n", encoding="utf-8")
            shutil.copy2(SCRIPT, root / "deploy.sh")
            bindir = root / "bin"
            bindir.mkdir()
            # A reset ends the fixture deliberately, before migration/build steps.
            (bindir / "git").write_text('''#!/usr/bin/env bash
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
  "reset --hard "*) printf 'RESET %s\\n' "$*"; exit 73;;
  *) printf 'unexpected git: %s\\n' "$*" >&2; exit 99;;
esac
''', encoding="utf-8", newline="\n")
            (bindir / "docker").write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8", newline="\n")
            for path in bindir.iterdir():
                path.chmod(0o755)
            # Git Bash accepts Windows paths as PATH entries once converted by cygpath.
            def posix(path):
                if os.name == "nt":
                    return subprocess.check_output([BASH, "-c", 'cygpath -u "$1"', "fixture", str(path)], text=True).strip()
                return str(path)
            script = posix(root / "deploy.sh")
            binpath = posix(bindir)
            env = dict(os.environ, DEPLOY_NO_BACKUP="1", FIXTURE_MODE=mode)
            result = subprocess.run([BASH, "-c", 'export PATH="$1:$PATH"; shift; exec bash "$@"', "fixture", binpath, script, *args], env=env, text=True, encoding="utf-8", capture_output=True)
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
        self.assertIn("CHECKOUT checkout -q -b new-branch --track origin/new-branch", output)
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


if __name__ == "__main__":
    unittest.main()
