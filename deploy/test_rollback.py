import os
import pathlib
import subprocess
import sys
import tempfile
import textwrap
import unittest

import hermetic
from hermetic import tearDownModule  # noqa: F401  (fails the module on a bypassed stub)

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "rollback.sh"


class RollbackSecurityTest(unittest.TestCase):
    def write_executable(self, path, body):
        path.write_text(textwrap.dedent(body).lstrip())
        path.chmod(0o700)

    def run_rollback(
        self, management_url, extra_env=None, stat_uid="0", stat_gid="0",
        strip_key_uid_check=False,
    ):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            config_dir = root / "config"
            config_dir.mkdir()
            config = config_dir / "config.yaml"
            backup = config_dir / "config.yaml.backup"
            key_file = root / "management.key"
            config.write_text("current\n")
            backup.write_text("backup\n")
            key_file.write_text("fixture-management-marker\n")
            config_dir.chmod(0o700)
            backup.chmod(0o600)
            key_file.chmod(0o600)
            curl_log = root / "curl.log"
            docker_log = root / "docker.log"
            systemctl_log = root / "systemctl.log"
            self.write_executable(
                bin_dir / "stat",
                f"""
                #!/usr/bin/env python3
                import pathlib, sys
                path=pathlib.Path(sys.argv[-1])
                format_value=sys.argv[sys.argv.index('-c') + 1]
                if format_value == '%u:%g:%a' and path.name == 'config':
                    print({stat_uid!r} + ':' + {stat_gid!r} + ':700')
                elif format_value == '%u:%a' and path.name == 'config.yaml.backup':
                    print({stat_uid!r} + ':600')
                else:
                    raise SystemExit(1)
                """,
            )
            self.write_executable(
                bin_dir / "curl",
                f"""
                #!/usr/bin/env python3
                import pathlib
                pathlib.Path({str(curl_log)!r}).write_text('called')
                """,
            )
            self.write_executable(
                bin_dir / "docker",
                f"""
                #!/usr/bin/env python3
                import pathlib, sys
                pathlib.Path({str(docker_log)!r}).write_text(' '.join(sys.argv[1:]))
                """,
            )
            if strip_key_uid_check:
                # TEST-ONLY interpreter shim: runs every python3 invocation
                # through the real interpreter byte-identical, except the
                # rollback key-gate stdin script, from which it removes ONLY
                # the root-uid comparison. Symlink, mode, and single-line
                # checks still execute for real; the root gate itself is
                # pinned by test_leaves_live_config_unchanged, which uses no
                # shim. If the upstream marker changes, the strip no-ops and
                # the test fails instead of silently passing.
                real_python = sys.executable
                self.write_executable(
                    bin_dir / "python3",
                    f"""#!{real_python}
import os, subprocess, sys
REAL = {real_python!r}
UID_MARKER = b"key_path.stat().st_uid != 0 or "
if len(sys.argv) > 1 and sys.argv[1] == "-":
    data = sys.stdin.buffer.read()
    stripped = data.replace(UID_MARKER, b"")
    if stripped != data:
        proc = subprocess.run([REAL] + sys.argv[1:], input=stripped)
        raise SystemExit(proc.returncode)
    proc = subprocess.run([REAL] + sys.argv[1:], input=data)
    raise SystemExit(proc.returncode)
os.execv(REAL, [REAL] + sys.argv[1:])
""",
                )
            self.write_executable(
                bin_dir / "systemctl",
                f"""
                #!/usr/bin/env python3
                import pathlib
                pathlib.Path({str(systemctl_log)!r}).write_text('called')
                """,
            )
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{bin_dir}:{env['PATH']}",
                    "CLIPROXY_MANAGEMENT_URL": management_url,
                    "CLIPROXY_CONFIG": str(config),
                    "CLIPROXY_BACKUP": str(backup),
                    "CLIPROXY_MANAGEMENT_KEY_FILE": str(key_file),
                    "CLIPROXY_USAGE_DIR": str(root / "usage"),
                }
            )
            if extra_env:
                env.update(extra_env)
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=env, text=True, capture_output=True)
            return completed, {
                "curl": curl_log.exists(),
                "docker": docker_log.read_text() if docker_log.exists() else "",
                "systemctl": systemctl_log.exists(),
            }

    def test_rejects_unsafe_management_urls_before_credentialed_curl(self):
        cases = {
            "userinfo": hermetic.ORIGIN.replace("http://", "http://user@"),
            "wrong origin": "http://127.0.0.1:9999",
            "non-loopback": "https://example.com:8317",
            "query": hermetic.ORIGIN + "?target=evil",
            "fragment": hermetic.ORIGIN + "#evil",
            "invalid port": "http://127.0.0.1:invalid",
        }
        for name, url in cases.items():
            with self.subTest(name=name):
                completed, calls = self.run_rollback(url)
                self.assertNotEqual(completed.returncode, 0)
                self.assertFalse(calls["curl"])
                self.assertRegex(completed.stderr, r"management URL|origin")

    def test_docker_restart_replaces_legacy_systemd_path(self):
        completed, calls = self.run_rollback(
            hermetic.ORIGIN,
            {"CLIPROXY_CONTAINER": "cliproxy-main"},
            strip_key_uid_check=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("restart cliproxy-main", calls["docker"])
        self.assertFalse(calls["systemctl"])

    def test_explicit_restart_command_wins_over_docker(self):
        completed, calls = self.run_rollback(
            hermetic.ORIGIN,
            {
                "CLIPROXY_CONTAINER": "cliproxy-main",
                "CLIPROXY_RESTART_COMMAND": "true docker-bypassed",
            },
            strip_key_uid_check=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertEqual(calls["docker"], "")
        self.assertFalse(calls["systemctl"])

    def test_rejects_unsafe_container_names(self):
        completed, calls = self.run_rollback(
            hermetic.ORIGIN, {"CLIPROXY_CONTAINER": "evil;reboot"}
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(calls["docker"], "")
        self.assertFalse(calls["systemctl"])
        self.assertIn("CLIPROXY_CONTAINER", completed.stderr)

    def test_config_ownership_is_parameterized_never_changed(self):
        completed, calls = self.run_rollback(
            hermetic.ORIGIN,
            {"CLIPROXY_CONFIG_UID": "1000", "CLIPROXY_CONFIG_GID": "1000"},
            stat_uid="1000",
            stat_gid="1000",
            strip_key_uid_check=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        completed, _ = self.run_rollback(
            hermetic.ORIGIN,
            {"CLIPROXY_CONFIG_UID": "1000", "CLIPROXY_CONFIG_GID": "1000"},
            stat_uid="0",
            stat_gid="0",
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("CLIPROXY_CONFIG_UID", completed.stderr)
        completed, _ = self.run_rollback(
            hermetic.ORIGIN, {"CLIPROXY_CONFIG_UID": "root"}
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("numeric", completed.stderr)

    def test_leaves_live_config_unchanged_when_management_key_validation_fails(self):
        # The management key file in this fixture is owned by the test-runner's own
        # uid, never uid 0, so rollback.sh's key validation (which requires
        # root-ownership) always raises here -- exercising exactly the "invalid
        # management key" mode/content failure the release-blocking defect was about.
        # If rollback.sh still replaced CLIPROXY_CONFIG before validating the key
        # (the pre-fix ordering), this failure would land after the restore and the
        # live config would already read "backup"; the regression is that it must not.
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            config_dir = root / "config"
            config_dir.mkdir()
            config = config_dir / "config.yaml"
            backup = config_dir / "config.yaml.backup"
            key_file = root / "management.key"
            config.write_text("current\n")
            backup.write_text("backup\n")
            key_file.write_text("fixture-management-marker\n")
            config_dir.chmod(0o700)
            backup.chmod(0o600)
            key_file.chmod(0o600)
            curl_log = root / "curl.log"
            reload_log = root / "reload.log"
            self.write_executable(
                bin_dir / "stat",
                """
                #!/usr/bin/env python3
                import pathlib, sys
                path=pathlib.Path(sys.argv[-1])
                format_value=sys.argv[sys.argv.index('-c') + 1]
                if format_value == '%u:%g:%a' and path.name == 'config':
                    print('0:0:700')
                elif format_value == '%u:%a' and path.name == 'config.yaml.backup':
                    print('0:600')
                else:
                    raise SystemExit(1)
                """,
            )
            self.write_executable(
                bin_dir / "curl",
                f"""
                #!/usr/bin/env python3
                import pathlib
                pathlib.Path({str(curl_log)!r}).write_text('called')
                """,
            )
            self.write_executable(
                bin_dir / "systemctl",
                f"""
                #!/usr/bin/env python3
                import pathlib
                pathlib.Path({str(reload_log)!r}).write_text('called')
                """,
            )
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{bin_dir}:{env['PATH']}",
                    "CLIPROXY_MANAGEMENT_URL": hermetic.ORIGIN,
                    "CLIPROXY_CONFIG": str(config),
                    "CLIPROXY_BACKUP": str(backup),
                    "CLIPROXY_MANAGEMENT_KEY_FILE": str(key_file),
                    "CLIPROXY_USAGE_DIR": str(root / "usage"),
                }
            )
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=env, text=True, capture_output=True)
            self.assertNotEqual(completed.returncode, 0)
            self.assertRegex(completed.stderr, r"management key file")
            self.assertEqual(config.read_text(), "current\n")
            self.assertFalse(curl_log.exists())
            self.assertFalse(reload_log.exists())


if __name__ == "__main__":
    unittest.main()
