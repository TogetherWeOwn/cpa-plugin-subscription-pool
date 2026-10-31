"""Regression tests: deploy script tests can never reach live CLIProxy.

On 2026-10-31 two agent test runs sent fixture management keys to
127.0.0.1:8317 and got the host banned from the management API. The runtime's
BASH_ENV reset PATH in the scripts' bash, so the curl stub was bypassed and the
real curl ran against the live default origin.
"""

import http.server
import os
import pathlib
import re
import subprocess
import tempfile
import threading
import unittest

import hermetic
from hermetic import tearDownModule  # noqa: F401  (fails the module on a bypassed stub)

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPTS = ("verify-live.sh", "verify-live-opencodego.sh", "rollback.sh")
LIVE_ORIGINS = ("http://127.0.0.1:8317", "http://[::1]:8317", "http://localhost:8317")


class _Recorder(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.server.hits.append(self.path)
        self.send_response(503)
        self.end_headers()

    do_DELETE = do_GET

    def log_message(self, *_args):
        pass


class HermeticTest(unittest.TestCase):
    def script_env(self, root, **extra):
        key = root / "management.key"
        key.write_text("fixture-management-marker\n")
        key.chmod(0o600)
        backup = root / "config.yaml.backup"
        backup.write_text("backup\n")
        backup.chmod(0o600)
        env = os.environ.copy()
        env.update(
            {
                "CLIPROXY_MANAGEMENT_KEY_FILE": str(key),
                "ZAI_CODING_PLAN_KEY": "fixture-plan-marker",
                "OPENCODE_GO_DASHBOARD_API_KEY": "fixture-dashboard-marker",
                "CLIPROXY_SNAPSHOT_ONLY": "1",
                "CLIPROXY_USAGE_DIR": str(root / "usage"),
                "CLIPROXY_CONFIG": str(root / "config.yaml"),
                "CLIPROXY_BACKUP": str(backup),
                "CLIPROXY_CONTAINER": "cliproxy-test",
            }
        )
        env.update(extra)
        return env

    def recording_curl(self, root):
        bin_dir = root / "bin"
        bin_dir.mkdir()
        log = root / "curl.log"
        curl = bin_dir / "curl"
        curl.write_text(f"#!/bin/sh\necho called >> {log}\nexit 7\n")
        curl.chmod(0o700)
        return bin_dir, log

    def test_harness_exports_an_ephemeral_loopback_origin(self):
        self.assertRegex(hermetic.ORIGIN, r"^http://127\.0\.0\.1:\d+$")
        self.assertNotIn(hermetic.ORIGIN, LIVE_ORIGINS)
        self.assertEqual(os.environ["CLIPROXY_TEST_MANAGEMENT_ORIGIN"], hermetic.ORIGIN)
        self.assertEqual(os.environ["CLIPROXY_MANAGEMENT_URL"], hermetic.ORIGIN)
        self.assertNotIn("BASH_ENV", os.environ)

    def test_scripts_refuse_live_origins_under_the_harness_before_any_curl(self):
        for script in SCRIPTS:
            for url in LIVE_ORIGINS + (None,):
                with self.subTest(script=script, url=url), tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory)
                    bin_dir, log = self.recording_curl(root)
                    env = self.script_env(root, PATH=f"{bin_dir}:{os.environ['PATH']}")
                    if url is None:
                        # A test that forgets the URL falls back to the script default.
                        env.pop("CLIPROXY_MANAGEMENT_URL")
                    else:
                        env["CLIPROXY_MANAGEMENT_URL"] = url
                    completed = subprocess.run(
                        [str(ROOT / "deploy" / script)], cwd=ROOT, env=env, text=True, capture_output=True
                    )
                    self.assertNotEqual(completed.returncode, 0, completed.stdout)
                    self.assertIn("origin is not approved", completed.stderr)
                    self.assertFalse(log.exists(), "curl ran against a live origin")

    def test_scripts_reject_test_origins_that_could_be_live(self):
        bad = (
            "http://127.0.0.1:8317",
            "http://localhost:40000",
            "http://[::1]:40000",
            "http://127.0.0.1:80",
            "http://127.0.0.1:9999",
            "https://127.0.0.1:40000",
            "http://127.0.0.1:40000/",
            "http://user@127.0.0.1:40000",
            "http://127.0.0.1:040000",
            "http://127.0.0.1:not-a-port",
        )
        for script in SCRIPTS:
            for origin in bad:
                with self.subTest(script=script, origin=origin), tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory)
                    bin_dir, log = self.recording_curl(root)
                    env = self.script_env(
                        root,
                        PATH=f"{bin_dir}:{os.environ['PATH']}",
                        CLIPROXY_TEST_MANAGEMENT_ORIGIN=origin,
                        CLIPROXY_MANAGEMENT_URL=origin,
                    )
                    completed = subprocess.run(
                        [str(ROOT / "deploy" / script)], cwd=ROOT, env=env, text=True, capture_output=True
                    )
                    self.assertNotEqual(completed.returncode, 0)
                    self.assertIn("CLIPROXY_TEST_MANAGEMENT_ORIGIN must be", completed.stderr)
                    self.assertFalse(log.exists())

    def test_bypassed_stub_reaches_only_the_test_origin(self):
        # Reproduces the incident: a BASH_ENV file resets PATH, so the stub dir
        # is dropped and the real curl runs. The request must land on the test
        # origin, never on the live port.
        for script in ("verify-live.sh", "verify-live-opencodego.sh"):
            with self.subTest(script=script), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Recorder)
                server.hits = []
                threading.Thread(target=server.serve_forever, daemon=True).start()
                self.addCleanup(server.server_close)
                self.addCleanup(server.shutdown)
                origin = f"http://127.0.0.1:{server.server_port}"
                bin_dir, log = self.recording_curl(root)
                bash_env = root / "bash_env"
                bash_env.write_text(f"export PATH={os.environ['PATH']!r}\n")
                env = self.script_env(
                    root,
                    PATH=f"{bin_dir}:{os.environ['PATH']}",
                    BASH_ENV=str(bash_env),
                    CLIPROXY_TEST_MANAGEMENT_ORIGIN=origin,
                    CLIPROXY_MANAGEMENT_URL=origin,
                )
                completed = subprocess.run(
                    [str(ROOT / "deploy" / script)], cwd=ROOT, env=env, text=True, capture_output=True
                )
                self.assertNotEqual(completed.returncode, 0)
                self.assertFalse(log.exists(), "stub should have been bypassed in this reproduction")
                self.assertTrue(server.hits, completed.stderr)
                self.assertTrue(all("/management/" in path for path in server.hits), server.hits)

    def test_every_script_test_module_imports_the_harness(self):
        runs_script = re.compile(r"(verify-live[\w-]*|rollback)\.sh")
        for path in sorted((ROOT / "deploy").glob("test_*.py")):
            text = path.read_text()
            if "subprocess" not in text or not runs_script.search(text):
                continue
            with self.subTest(module=path.name):
                self.assertRegex(text, r"(?m)^import hermetic$")
                self.assertRegex(text, r"(?m)^from hermetic import tearDownModule\b")


if __name__ == "__main__":
    unittest.main()
