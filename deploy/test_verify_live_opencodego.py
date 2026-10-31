import json
import os
import pathlib
import subprocess
import tempfile
import textwrap
import time
import unittest

import hermetic
from hermetic import tearDownModule  # noqa: F401  (fails the module on a bypassed stub)

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "verify-live-opencodego.sh"


class VerifyLiveOpencodegoTest(unittest.TestCase):
    def write_executable(self, path, body):
        path.write_text(textwrap.dedent(body).lstrip())
        path.chmod(0o700)

    def run_verify(
        self,
        service_log="service ok",
        projected_json=None,
        management_url=None,
        status_account_name="opencodego-1",
        status_json=None,
        log_stalls=False,
        journal_timeout="1",
        container="cliproxy-test",
        log_command=None,
        snapshot_only=False,
        collector_fails=False,
        allow_unbound=False,
        credential_bound=True,
    ):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            usage_dir = root / "usage"
            management_file = root / "management.key"
            dashboard_file = root / "dashboard.key"
            management_file.write_text("fixture-management-marker\n")
            dashboard_file.write_text("fixture-dashboard-marker\n")
            management_file.chmod(0o600)
            dashboard_file.chmod(0o600)
            curl_log = root / "curl-argv.log"
            self.write_executable(
                bin_dir / "curl",
                f"""
                #!/usr/bin/env python3
                import json, pathlib, sys
                with pathlib.Path({str(curl_log)!r}).open("a") as log:
                    log.write("\\n".join(sys.argv[1:]) + "\\n---\\n")
                args=sys.argv[1:]
                if "%{{http_code}}" in args:
                    print("401", end="")
                else:
                    status_json={status_json!r}
                    if status_json is not None:
                        print(status_json)
                        raise SystemExit
                    print(json.dumps({{
                        "plugin":"subscription-pool",
                        "status":"registered",
                        "version":"0.0.0-dev",
                        "generated_at":"2026-09-10T15:00:00Z",
                        "providers":{{
                            "opencode-go":{{
                                "provider":"opencode-go",
                                "status":"registered",
                                "credential_bound":{credential_bound!r},
                                "observation_gaps":[],
                                "accounts":[{{
                                    "name":{status_account_name!r},
                                    "disabled":False,
                                    "windows":{{
                                        "five_hour":{{"known":True,"utilization":0.1,"exhausted":False,"resets_at":"2026-09-10T20:00:00Z","source":"poll","authoritative":True}},
                                        "weekly":{{"known":True,"utilization":0.2,"exhausted":False,"resets_at":"2026-09-16T00:00:00Z","source":"poll","authoritative":True}},
                                        "monthly":{{"known":True,"utilization":0.05,"exhausted":False,"resets_at":"2026-10-01T00:00:00Z","source":"poll","authoritative":True}}
                                    }}
                                }}]
                            }}
                        }}
                    }}))
                """,
            )
            docker_log = root / "docker-argv.log"
            log_body = (
                """
                #!/usr/bin/env python3
                import time
                time.sleep(60)
                """
                if log_stalls
                else f"""
                #!/usr/bin/env python3
                import pathlib, sys
                with pathlib.Path({str(docker_log)!r}).open("a") as log:
                    log.write(" ".join(sys.argv[1:]) + "\\n")
                print({service_log!r})
                """
            )
            self.write_executable(bin_dir / "docker", log_body)
            if collector_fails:
                self.write_executable(
                    bin_dir / "collector-opencodego",
                    "#!/usr/bin/env python3\nraise SystemExit('collector must not run in snapshot-only mode')\n",
                )
            else:
                self.write_executable(
                    bin_dir / "collector-opencodego",
                    f"""
                    #!/usr/bin/env python3
                    import json, os, pathlib
                    from datetime import datetime, timezone
                    output=pathlib.Path({str(usage_dir / "opencode-go.json")!r})
                    output.parent.mkdir(parents=True, exist_ok=True)
                    os.chmod(output.parent, 0o700)
                    projected_json={projected_json!r}
                    if projected_json is not None:
                        output.write_text(projected_json)
                    else:
                        output.write_text(json.dumps({{
                            "schemaVersion":1,
                            "observedAt":datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
                            "staleAfterSeconds":300,
                            "lane":"opencode-go","credential_bound":{credential_bound!r},
                            "records":[{{"name":{status_account_name!r},"disabled":False,"windows":{{}}}}]
                        }}))
                    os.chmod(output, 0o600)
                    """,
                )
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{bin_dir}:{env['PATH']}",
                    "CLIPROXY_MANAGEMENT_KEY_FILE": str(management_file),
                    "OPENCODE_GO_DASHBOARD_API_KEY_FILE": str(dashboard_file),
                    "CLIPROXY_USAGE_DIR": str(usage_dir),
                    "COLLECTOR_OPENCODEGO": str(bin_dir / "collector-opencodego"),
                    "CLIPROXY_JOURNAL_TIMEOUT": journal_timeout,
                    "HTTP_PROXY": "http://proxy.invalid:9876",
                    "HTTPS_PROXY": "http://proxy.invalid:9876",
                    "ALL_PROXY": "socks5://proxy.invalid:9876",
                    "NO_PROXY": "",
                }
            )
            if container is not None:
                env["CLIPROXY_CONTAINER"] = container
            else:
                env.pop("CLIPROXY_CONTAINER", None)
            if log_command is not None:
                env["CLIPROXY_LOG_COMMAND"] = log_command
            if snapshot_only:
                env["CLIPROXY_SNAPSHOT_ONLY"] = "1"
            if allow_unbound:
                env["OPENCODE_GO_ALLOW_UNBOUND"] = "1"
            if management_url is not None:
                env["CLIPROXY_MANAGEMENT_URL"] = management_url
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=env, text=True, capture_output=True)
            docker_argv = docker_log.read_text() if docker_log.exists() else ""
            return completed, curl_log.read_text() if curl_log.exists() else "", docker_argv

    def test_management_and_dashboard_keys_never_appear_in_curl_argv(self):
        completed, curl_argv, _ = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn("fixture-management-marker", curl_argv)
        self.assertNotIn("fixture-dashboard-marker", curl_argv)

    def test_rejects_unapproved_management_origin_before_curl(self):
        completed, curl_argv, _ = self.run_verify(management_url="http://127.0.0.1:9999")
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(curl_argv, "")
        self.assertIn("origin is not approved", completed.stderr)

    def test_authenticated_curl_is_time_size_and_proxy_bounded(self):
        completed, curl_argv, _ = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        invocations = [part for part in curl_argv.split("---\n") if "management.curl" in part]
        self.assertEqual(len(invocations), 1)
        args = invocations[0].splitlines()
        self.assertEqual(args[0], "-q")
        self.assertIn("--max-time\n5", invocations[0])
        self.assertIn("--max-filesize\n1048576", invocations[0])
        self.assertIn("--max-redirs\n0", invocations[0])
        self.assertIn("--noproxy\n*", invocations[0])
        self.assertIn("--proxy\n", invocations[0])

    def test_rejects_unsafe_journal_timeout_before_curl(self):
        for value in ("0", "-1", "--help", "1 day", "nan", "inf"):
            with self.subTest(value=value):
                completed, curl_argv, _ = self.run_verify(journal_timeout=value)
                self.assertNotEqual(completed.returncode, 0)
                self.assertEqual(curl_argv, "")
                self.assertIn("must be a positive duration", completed.stderr)

    def test_missing_provider_entry_fails(self):
        status = (
            '{"plugin":"subscription-pool","status":"registered","version":"0.0.0-dev",'
            '"generated_at":"2026-09-10T15:00:00Z","providers":{}}'
        )
        completed, _, _ = self.run_verify(status_json=status)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("missing the opencode-go provider entry", completed.stderr)

    def test_docker_log_capture_is_container_namespaced(self):
        text = SCRIPT.read_text()
        self.assertIn('docker logs --since 15m --tail 2000 "$CLIPROXY_CONTAINER"', text)
        self.assertNotIn("journalctl", text)
        self.assertNotIn("CLIPROXY_SERVICE_UNIT", text)
        completed, _, docker_argv = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("logs --since 15m --tail 2000 cliproxy-test", docker_argv)

    def test_requires_container_or_explicit_log_command(self):
        completed, _, docker_argv = self.run_verify(container=None)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("CLIPROXY_CONTAINER", completed.stderr)
        self.assertEqual(docker_argv, "")

    def test_snapshot_only_skips_collector(self):
        completed, _, _ = self.run_verify(snapshot_only=True, collector_fails=True)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("opencode-go.json", completed.stderr)

    def test_unbound_lane_fails_strict_and_reports_unavailable_when_allowed(self):
        unbound_windows = (
            '{"five_hour":{"known":false,"exhausted":false,"source":"unknown"},'
            '"weekly":{"known":false,"exhausted":false,"source":"unknown"},'
            '"monthly":{"known":false,"exhausted":false,"source":"unknown"}}'
        )
        status = (
            '{"plugin":"subscription-pool","status":"registered","version":"0.0.0-dev",'
            '"generated_at":"2026-09-10T15:00:00Z",'
            '"providers":{"opencode-go":{"provider":"opencode-go","status":"registered",'
            '"credential_bound":false,'
            '"observation_gaps":["no dashboard credential is configured"],'
            '"accounts":[{"name":"opencodego-1","disabled":false,'
            '"windows":' + unbound_windows + "}]}}}"
        )
        projected = (
            '{"schemaVersion":1,"observedAt":"2030-01-01T00:00:00Z",'
            '"staleAfterSeconds":300,"lane":"opencode-go","credential_bound":false,'
            '"records":[]}'
        )
        completed, _, _ = self.run_verify(
            status_json=status, credential_bound=False, projected_json=projected
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("no bound dashboard credential", completed.stderr)
        completed, _, _ = self.run_verify(
            status_json=status,
            credential_bound=False,
            projected_json=projected,
            allow_unbound=True,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("dated in the future", completed.stderr)

    def test_unbound_lane_disposition_requires_matching_snapshot(self):
        import datetime
        import json as json_module

        observed = datetime.datetime.now(datetime.timezone.utc).strftime(
            "%Y-%m-%dT%H:%M:%SZ"
        )
        unbound_windows = (
            '{"five_hour":{"known":false,"exhausted":false,"source":"unknown"},'
            '"weekly":{"known":false,"exhausted":false,"source":"unknown"},'
            '"monthly":{"known":false,"exhausted":false,"source":"unknown"}}'
        )
        status = (
            '{"plugin":"subscription-pool","status":"registered","version":"0.0.0-dev",'
            '"generated_at":"2026-09-10T15:00:00Z",'
            '"providers":{"opencode-go":{"provider":"opencode-go","status":"registered",'
            '"credential_bound":false,'
            '"observation_gaps":["no dashboard credential is configured"],'
            '"accounts":[{"name":"opencodego-1","disabled":false,'
            '"windows":' + unbound_windows + "}]}}}"
        )
        matching = json_module.dumps(
            {
                "schemaVersion": 1,
                "observedAt": observed,
                "staleAfterSeconds": 300,
                "lane": "opencode-go",
                "credential_bound": False,
                "records": [],
            }
        )
        completed, _, _ = self.run_verify(
            status_json=status,
            credential_bound=False,
            projected_json=matching,
            allow_unbound=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("UNAVAILABLE", completed.stdout)
        mismatched = json_module.dumps(
            {
                "schemaVersion": 1,
                "observedAt": observed,
                "staleAfterSeconds": 300,
                "lane": "opencode-go",
                "credential_bound": True,
                "records": [
                    {"name": "opencodego-1", "disabled": False, "windows": {}}
                ],
            }
        )
        completed, _, _ = self.run_verify(
            status_json=status,
            credential_bound=False,
            projected_json=mismatched,
            allow_unbound=True,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("unavailable lane disposition", completed.stderr)

    def test_stalling_docker_logs_is_terminated_by_explicit_timeout(self):
        started = time.monotonic()
        completed, _, _ = self.run_verify(log_stalls=True)
        elapsed = time.monotonic() - started
        self.assertNotEqual(completed.returncode, 0)
        self.assertLess(elapsed, 10)
        self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)
        self.assertNotIn("fixture-dashboard-marker", completed.stdout + completed.stderr)

    def test_raw_authenticated_status_rejects_markers_before_projection(self):
        completed, _, _ = self.run_verify(status_account_name="fixture-dashboard-marker")
        self.assertNotEqual(completed.returncode, 0)
        self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)
        self.assertNotIn("fixture-dashboard-marker", completed.stdout + completed.stderr)
        self.assertIn("authenticated status response failed confidential-value scan", completed.stderr)

    def test_bounded_scans_reject_markers_without_printing_them(self):
        for source, value in (
            ("service_log", "fixture-management-marker"),
            ("service_log", "fixture-dashboard-marker"),
        ):
            with self.subTest(source=source, value=value):
                completed, _, _ = self.run_verify(**{source: value})
                self.assertNotEqual(completed.returncode, 0)
                self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)
                self.assertNotIn("fixture-dashboard-marker", completed.stdout + completed.stderr)

    def test_oversized_service_log_is_rejected_before_full_capture(self):
        completed, _, _ = self.run_verify(service_log="x" * 1_048_577)
        self.assertNotEqual(completed.returncode, 0)


if __name__ == "__main__":
    unittest.main()
