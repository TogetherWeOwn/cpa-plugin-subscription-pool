import os
import pathlib
import subprocess
import tempfile
import textwrap
import time
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "deploy" / "verify-live.sh"
# The host's /v0/management/plugins answer while the pool plugin is live.
REGISTERED_PLUGIN_LISTING = '{"plugins_enabled": true, "plugins": [{"id": "subscription-pool", "registered": true, "effective_enabled": true}]}'


class VerifyLiveTest(unittest.TestCase):
    def write_executable(self, path, body):
        path.write_text(textwrap.dedent(body).lstrip())
        path.chmod(0o700)

    def run_verify(
        self,
        dashboard="dashboard ok",
        service_log="service ok",
        projected="projected ok",
        projected_json=None,
        management_url=None,
        status_plan="pro",
        status_plan_json=None,
        dashboard_json=None,
        coordinator_status_json=None,
        plugins_json=None,
        log_stalls=False,
        journal_timeout="1",
        router_dry_run=None,
        canary_command=None,
        python_optimize=False,
        container="cliproxy-test",
        log_command=None,
        snapshot_only=False,
        dashboard_url="http://127.0.0.1/dashboard",
        collector_fails=False,
        status_extra_accounts=None,
    ):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            usage_dir = root / "usage"
            management_file = root / "management.key"
            plan_file = root / "plan.key"
            management_file.write_text("fixture-management-marker\n")
            plan_file.write_text("fixture-plan-marker\n")
            management_file.chmod(0o600)
            plan_file.chmod(0o600)
            curl_log = root / "curl-argv.log"
            docker_log = root / "docker-argv.log"
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
                elif args and args[-1].endswith("/v0/management/plugins"):
                    print({plugins_json if plugins_json is not None else REGISTERED_PLUGIN_LISTING!r})
                elif {"dashboard"!r} in " ".join(args):
                    dashboard_json={dashboard_json!r}
                    if dashboard_json is not None:
                        print(dashboard_json)
                    else:
                        print(json.dumps({{"status": {dashboard!r}}}))
                elif {"subscription-pool/status"!r} in " ".join(args):
                    coordinator_json={coordinator_status_json!r}
                    if coordinator_json is not None:
                        print(coordinator_json)
                    else:
                        print(json.dumps({{
                            "plugin":"subscription-pool",
                            "status":"registered",
                            "version":"0.0.0-dev",
                            "generated_at":"2026-09-10T15:00:00Z",
                            "providers":{{"zai":{{"provider":"zai"}},"opencode-go":{{"provider":"opencode-go"}}}}
                        }}))
                else:
                    status_json={status_plan_json!r}
                    if status_json is not None:
                        print(status_json)
                        raise SystemExit
                    accounts=[{{
                        "identity":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                        "cooldown":{{"active":False,"until":None,"reason":"","source":"zai_runtime_health_v1"}},
                        "name":"zai-pro-1","key_suffix":"redacted","plan":{status_plan!r},
                        "five_hour_utilization":0.1,"weekly_utilization":0.2,
                        "five_hour_resets_at":None,"weekly_resets_at":None,
                        "quota_source":"estimate","quota_observed_at":"2026-09-10T15:00:00Z",
                        "quota_age_seconds":1,"quota_stale":False,"offpeak":False,"health":"healthy",
                        "estimator_complete_since":"2026-09-10T15:00:00Z","delivery_warning":False,
                        "persistence_warning":False,"unknown_model_warning":False,
                        "heuristic_dedup_warning":False,"dedup_mode":"bounded_hash_heuristic"
                    }}]
                    extra={status_extra_accounts!r}
                    if extra is not None:
                        accounts.extend(extra)
                    print(json.dumps({{
                        "plugin":"zai-coding-plan",
                        "status":"registered",
                        "version":"0.0.0-dev",
                        "generated_at":"2026-09-10T15:00:00Z",
                        "accounts":accounts
                    }}))
                """,
            )
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
                    bin_dir / "collector-zai",
                    "#!/usr/bin/env python3\nraise SystemExit('collector must not run in snapshot-only mode')\n",
                )
            else:
                self.write_executable(
                    bin_dir / "collector-zai",
                    f"""
                    #!/usr/bin/env python3
                    import json, os, pathlib
                    from datetime import datetime, timezone
                    output=pathlib.Path({str(usage_dir / "zai.json")!r})
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
                            "lane":"zai",
                            "records":[{{
                                "identity":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                                "cooldown":{{"active":False,"until":None,"reason":"","source":"zai_runtime_health_v1"}},
                                "key_suffix":"redacted","note":{projected!r}
                            }}]
                        }}))
                    os.chmod(output, 0o600)
                    """,
                )
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{bin_dir}:{env['PATH']}",
                    "CLIPROXY_MANAGEMENT_KEY_FILE": str(management_file),
                    "ZAI_CODING_PLAN_KEY_FILE": str(plan_file),
                    "CLIPROXY_USAGE_DIR": str(usage_dir),
                    "COLLECTOR_ZAI": str(bin_dir / "collector-zai"),
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
            if dashboard_url is not None:
                env["CLIPROXY_DASHBOARD_URL"] = dashboard_url
            else:
                env.pop("CLIPROXY_DASHBOARD_URL", None)
            if snapshot_only:
                env["CLIPROXY_SNAPSHOT_ONLY"] = "1"
            if python_optimize:
                env["PYTHONOPTIMIZE"] = "1"
            if management_url is not None:
                env["CLIPROXY_MANAGEMENT_URL"] = management_url
            if router_dry_run is not None:
                env["CLIPROXY_ROUTER_DRY_RUN"] = router_dry_run
            if canary_command is not None:
                env["CLIPROXY_CANARY_COMMAND"] = canary_command
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=env, text=True, capture_output=True)
            docker_argv = docker_log.read_text() if docker_log.exists() else ""
            return completed, curl_log.read_text() if curl_log.exists() else "", docker_argv

    def test_management_key_never_appears_in_curl_argv(self):
        completed, curl_argv, _ = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn("fixture-management-marker", curl_argv)

    def test_rejects_unapproved_management_origin_before_curl(self):
        completed, curl_argv, _ = self.run_verify(management_url="http://127.0.0.1:9999")
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(curl_argv, "")
        self.assertIn("origin is not approved", completed.stderr)

    def test_authenticated_curl_is_time_size_and_proxy_bounded(self):
        completed, curl_argv, _ = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        invocations = [part for part in curl_argv.split("---\n") if "management.curl" in part]
        self.assertEqual(len(invocations), 3)
        for invocation in invocations:
            args = invocation.splitlines()
            self.assertEqual(args[0], "-q")
            self.assertIn("--max-time\n5", invocation)
            self.assertIn("--max-filesize\n1048576", invocation)
            self.assertIn("--max-redirs\n0", invocation)
            self.assertIn("--noproxy\n*", invocation)
            self.assertIn("--proxy\n", invocation)

    def test_unauthenticated_loopback_curl_disables_ambient_config_and_proxy(self):
        completed, curl_argv, _ = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        invocations = [part for part in curl_argv.split("---\n") if "%{http_code}" in part]
        self.assertEqual(len(invocations), 1)
        args = invocations[0].splitlines()
        self.assertEqual(args[0], "-q")
        self.assertIn("--noproxy\n*", invocations[0])
        self.assertIn("--proxy\n", invocations[0])

    def test_rejects_unsafe_journal_timeout_before_curl(self):
        for value in ("0", "-1", "--help", "1 day", "nan", "inf"):
            with self.subTest(value=value):
                completed, curl_argv, _ = self.run_verify(journal_timeout=value)
                self.assertNotEqual(completed.returncode, 0)
                self.assertEqual(curl_argv, "")
                self.assertIn("must be a positive duration", completed.stderr)

    def test_schema_checks_still_fail_under_python_optimize(self):
        completed, _, _ = self.run_verify(status_plan="fixture-plan-marker", python_optimize=True)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("confidential-value scan", completed.stderr)

    def test_malformed_status_schema_fails_under_python_optimize(self):
        completed, _, _ = self.run_verify(status_plan_json='{"plugin":"zai-coding-plan"}', python_optimize=True)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("unexpected top-level fields", completed.stderr)

    def test_docker_log_capture_is_bounded_and_namespaced(self):
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

    def test_rejects_unsafe_container_names_before_any_curl(self):
        for container in ("evil;reboot", "$(reboot)", "cliproxy test", "../escape", "-leading-dash"):
            with self.subTest(container=container):
                completed, curl_argv, _ = self.run_verify(container=container)
                self.assertNotEqual(completed.returncode, 0)
                self.assertEqual(curl_argv, "")
                self.assertIn("CLIPROXY_CONTAINER", completed.stderr)

    def test_explicit_log_command_replaces_docker_capture(self):
        completed, _, docker_argv = self.run_verify(log_command="printf 'explicit log ok'")
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertEqual(docker_argv, "")

    def test_dashboard_step_is_skipped_when_url_unset(self):
        completed, curl_argv, _ = self.run_verify(dashboard_url=None)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn("dashboard", curl_argv)
        self.assertIn("dashboard step skipped", completed.stdout)

    def test_snapshot_only_skips_collector_and_checks_freshness(self):
        completed, _, _ = self.run_verify(snapshot_only=True, collector_fails=True)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("zai.json", completed.stderr)

    def test_snapshot_only_accepts_fresh_existing_snapshot(self):
        import datetime
        import json as json_module

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            usage_dir = root / "usage"
            usage_dir.mkdir(parents=True)
            observed = (
                datetime.datetime.now(datetime.timezone.utc)
                .strftime("%Y-%m-%dT%H:%M:%SZ")
            )
            (usage_dir / "zai.json").write_text(
                json_module.dumps(
                    {
                        "schemaVersion": 1,
                        "observedAt": observed,
                        "staleAfterSeconds": 300,
                        "lane": "zai",
                        "records": [
                            {
                                "identity": "bb" * 32,
                                "cooldown": {
                                    "active": False,
                                    "until": None,
                                    "reason": "",
                                    "source": "zai_runtime_health_v1",
                                },
                                "key_suffix": "redacted",
                            }
                        ],
                    }
                )
            )
            management_file = root / "management.key"
            plan_file = root / "plan.key"
            management_file.write_text("fixture-management-marker\n")
            plan_file.write_text("fixture-plan-marker\n")
            management_file.chmod(0o600)
            plan_file.chmod(0o600)
            self.write_executable(
                bin_dir / "curl",
                """
                #!/usr/bin/env python3
                import json, sys
                args = sys.argv[1:]
                if "%{http_code}" in args:
                    print("401", end="")
                elif args[-1].endswith("/v0/management/plugins"):
                    print('{"plugins_enabled": true, "plugins": [{"id": "subscription-pool", "registered": true, "effective_enabled": true}]}')
                elif "subscription-pool/status" in " ".join(args):
                    print(json.dumps({
                        "plugin": "subscription-pool",
                        "status": "registered",
                        "version": "0.0.0-dev",
                        "generated_at": "2026-09-10T15:00:00Z",
                        "providers": {"zai": {"provider": "zai"},
                                      "opencode-go": {"provider": "opencode-go"}}
                    }))
                else:
                    print(json.dumps({
                        "plugin": "zai-coding-plan",
                        "status": "registered",
                        "version": "0.0.0-dev",
                        "generated_at": "2026-09-10T15:00:00Z",
                        "accounts": [{
                            "identity": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
                            "cooldown": {"active": False, "until": None, "reason": "",
                                         "source": "zai_runtime_health_v1"},
                            "name": "zai-pro-1", "key_suffix": "redacted", "plan": "pro",
                            "five_hour_utilization": 0.1, "weekly_utilization": 0.2,
                            "five_hour_resets_at": None, "weekly_resets_at": None,
                            "quota_source": "estimate",
                            "quota_observed_at": "2026-09-10T15:00:00Z",
                            "quota_age_seconds": 1, "quota_stale": False,
                            "offpeak": False, "health": "healthy",
                            "estimator_complete_since": "2026-09-10T15:00:00Z",
                            "delivery_warning": False, "persistence_warning": False,
                            "unknown_model_warning": False,
                            "heuristic_dedup_warning": False,
                            "dedup_mode": "bounded_hash_heuristic"
                        }]
                    }))
                """,
            )
            self.write_executable(
                bin_dir / "docker", "#!/usr/bin/env python3\nprint('service ok')\n"
            )
            self.write_executable(
                bin_dir / "collector-zai",
                "#!/usr/bin/env python3\nraise SystemExit('collector must not run')\n",
            )
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{bin_dir}:{env['PATH']}",
                    "CLIPROXY_MANAGEMENT_KEY_FILE": str(management_file),
                    "ZAI_CODING_PLAN_KEY_FILE": str(plan_file),
                    "CLIPROXY_USAGE_DIR": str(usage_dir),
                    "COLLECTOR_ZAI": str(bin_dir / "collector-zai"),
                    "CLIPROXY_CONTAINER": "cliproxy-test",
                    "CLIPROXY_SNAPSHOT_ONLY": "1",
                    "CLIPROXY_JOURNAL_TIMEOUT": "1",
                }
            )
            env.pop("CLIPROXY_DASHBOARD_URL", None)
            completed = subprocess.run(
                [str(SCRIPT)], cwd=ROOT, env=env, text=True, capture_output=True
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertIn("snapshot-only", completed.stdout)

    def test_stale_snapshot_fails_freshness_check(self):
        import json as json_module

        stale = {
            "schemaVersion": 1,
            "observedAt": "2026-09-10T15:00:00Z",
            "staleAfterSeconds": 300,
            "lane": "zai",
            "records": [
                {
                    "identity": "cc" * 32,
                    "cooldown": {
                        "active": False,
                        "until": None,
                        "reason": "",
                        "source": "zai_runtime_health_v1",
                    },
                    "key_suffix": "redacted",
                }
            ],
        }
        completed, _, _ = self.run_verify(
            projected_json=json_module.dumps(stale), dashboard_url=None
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("stale", completed.stderr)

    def test_status_without_identity_or_bad_cooldown_fails(self):
        bad_status = (
            '{"plugin":"zai-coding-plan","status":"registered","version":"0.0.0-dev",'
            '"generated_at":"2026-09-10T15:00:00Z","accounts":[{'
            '"name":"zai-pro-1","key_suffix":"redacted","plan":"pro",'
            '"five_hour_utilization":0.1,"weekly_utilization":0.2,'
            '"five_hour_resets_at":null,"weekly_resets_at":null,"quota_source":"estimate",'
            '"quota_observed_at":"2026-09-10T15:00:00Z","quota_age_seconds":1,'
            '"quota_stale":false,"offpeak":false,"health":"healthy",'
            '"estimator_complete_since":"2026-09-10T15:00:00Z","delivery_warning":false,'
            '"persistence_warning":false,"unknown_model_warning":false,'
            '"heuristic_dedup_warning":false,"dedup_mode":"bounded_hash_heuristic"}]}'
        )
        completed, _, _ = self.run_verify(status_plan_json=bad_status)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("unexpected fields", completed.stderr)

    def test_duplicate_status_identities_fail(self):
        extra = [
            {
                "identity": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                "cooldown": {
                    "active": False,
                    "until": None,
                    "reason": "",
                    "source": "zai_runtime_health_v1",
                },
                "name": "zai-pro-2",
                "key_suffix": "redacted",
                "plan": "pro",
                "five_hour_utilization": 0.1,
                "weekly_utilization": 0.2,
                "five_hour_resets_at": None,
                "weekly_resets_at": None,
                "quota_source": "estimate",
                "quota_observed_at": "2026-09-10T15:00:00Z",
                "quota_age_seconds": 1,
                "quota_stale": False,
                "offpeak": False,
                "health": "healthy",
                "estimator_complete_since": "2026-09-10T15:00:00Z",
                "delivery_warning": False,
                "persistence_warning": False,
                "unknown_model_warning": False,
                "heuristic_dedup_warning": False,
                "dedup_mode": "bounded_hash_heuristic",
            }
        ]
        completed, _, _ = self.run_verify(status_extra_accounts=extra)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("not unique", completed.stderr)

    @staticmethod
    def unicode_escape(value):
        return "".join(f"\\u{ord(character):04x}" for character in value)

    def test_unicode_escaped_status_markers_are_rejected_after_json_decode(self):
        escaped = self.unicode_escape("fixture-plan-marker")
        status = (
            '{"plugin":"zai-coding-plan","status":"registered","version":"0.0.0-dev",'
            '"generated_at":"2026-09-10T15:00:00Z","accounts":[{'
            '"name":"zai-pro-1","key_suffix":"redacted","plan":"' + escaped + '",'
            '"five_hour_utilization":0.1,"weekly_utilization":0.2,'
            '"five_hour_resets_at":null,"weekly_resets_at":null,"quota_source":"estimate",'
            '"quota_observed_at":"2026-09-10T15:00:00Z","quota_age_seconds":1,'
            '"quota_stale":false,"offpeak":false,"health":"healthy",'
            '"estimator_complete_since":"2026-09-10T15:00:00Z","delivery_warning":false,'
            '"persistence_warning":false,"unknown_model_warning":false,'
            '"heuristic_dedup_warning":false,"dedup_mode":"bounded_hash_heuristic"}]}'
        )
        completed, _, _ = self.run_verify(status_plan_json=status)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("decoded confidential-value scan", completed.stderr)

    def test_unicode_escaped_dashboard_suffix_is_rejected_after_json_decode(self):
        escaped = self.unicode_escape("marker")
        completed, _, _ = self.run_verify(dashboard_json='{"note":"' + escaped + '"}')
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("decoded confidential-value scan", completed.stderr)

    def test_unicode_escaped_projected_full_marker_is_rejected_after_json_decode(self):
        escaped = self.unicode_escape("fixture-management-marker")
        projected = '{"schemaVersion":1,"lane":"zai","records":[{"key_suffix":"redacted","note":"' + escaped + '"}]}'
        completed, _, _ = self.run_verify(projected_json=projected)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("decoded confidential-value scan", completed.stderr)

    def test_unicode_escaped_projected_plan_suffix_is_rejected_after_json_decode(self):
        escaped = self.unicode_escape("marker")
        projected = '{"schemaVersion":1,"lane":"zai","records":[{"key_suffix":"redacted","note":"' + escaped + '"}]}'
        completed, _, _ = self.run_verify(projected_json=projected)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("decoded confidential-value scan", completed.stderr)

    def test_oversized_service_log_is_rejected_before_full_capture(self):
        completed, _, _ = self.run_verify(service_log="x" * 1_048_577)
        self.assertNotEqual(completed.returncode, 0)

    def test_stalling_docker_logs_is_terminated_by_explicit_timeout(self):
        started = time.monotonic()
        completed, _, _ = self.run_verify(log_stalls=True)
        elapsed = time.monotonic() - started
        self.assertNotEqual(completed.returncode, 0)
        self.assertLess(elapsed, 10)
        self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)
        self.assertNotIn("fixture-plan-marker", completed.stdout + completed.stderr)

    def test_router_dry_run_stdout_is_bounded_during_execution(self):
        command = "python3 -c 'import os; os.write(1, b\"x\" * 1048577)'"
        completed, curl_argv, _ = self.run_verify(router_dry_run=command)
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(curl_argv, "")
        self.assertIn("output exceeded 1 MiB per stream", completed.stderr)

    def test_router_dry_run_stderr_is_bounded_during_execution(self):
        command = "python3 -c 'import os; os.write(2, b\"x\" * 1048577)'"
        completed, curl_argv, _ = self.run_verify(router_dry_run=command)
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(curl_argv, "")
        self.assertIn("output exceeded 1 MiB per stream", completed.stderr)

    def test_canary_stdout_and_stderr_accept_bounded_output(self):
        command = "python3 -c 'import os; os.write(1, b\"ok\"); os.write(2, b\"warning\")'"
        completed, _, _ = self.run_verify(canary_command=command)
        self.assertEqual(completed.returncode, 0, completed.stderr)

    def test_raw_authenticated_status_rejects_markers_before_projection(self):
        for marker in ("fixture-management-marker", "fixture-plan-marker", "marker"):
            with self.subTest(marker=marker):
                completed, _, _ = self.run_verify(status_plan=marker)
                self.assertNotEqual(completed.returncode, 0)
                self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)
                self.assertNotIn("fixture-plan-marker", completed.stdout + completed.stderr)
                self.assertIn("authenticated status response failed confidential-value scan", completed.stderr)

    def test_coordinator_status_proves_both_provider_modules_present(self):
        completed, curl_argv, _ = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("subscription-pool/status", curl_argv)

    def test_missing_provider_in_coordinator_status_fails_closed(self):
        coordinator_json = (
            '{"plugin":"subscription-pool","status":"registered","version":"0.0.0-dev",'
            '"generated_at":"2026-09-10T15:00:00Z","providers":{"zai":{"provider":"zai"}}}'
        )
        completed, _, _ = self.run_verify(coordinator_status_json=coordinator_json)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("does not list both zai and opencode-go", completed.stderr)

    def test_bounded_scans_reject_full_or_suffix_markers_without_printing_them(self):
        for source, value in (
            ("projected", "fixture-plan-marker"),
            ("dashboard", "fixture-plan-marker"),
            ("service_log", "fixture-management-marker"),
            ("projected", "marker"),
            ("dashboard", "marker"),
            ("service_log", "marker"),
        ):
            with self.subTest(source=source, value=value):
                completed, _, _ = self.run_verify(**{source: value})
                self.assertNotEqual(completed.returncode, 0)
                self.assertNotIn("fixture-plan-marker", completed.stdout + completed.stderr)
                self.assertNotIn("fixture-management-marker", completed.stdout + completed.stderr)


    def test_plugin_missing_from_host_snapshot_fails_closed(self):
        for listing in (
            '{"plugins_enabled": true, "plugins": []}',
            '{"plugins_enabled": true, "plugins": [{"id": "subscription-pool", "registered": false, "effective_enabled": false}]}',
            '{"plugins_enabled": true, "plugins": [{"id": "subscription-pool", "registered": true, "effective_enabled": false}]}',
            "not json",
        ):
            with self.subTest(listing=listing):
                completed, _, _ = self.run_verify(plugins_json=listing)
                self.assertNotEqual(completed.returncode, 0)
                self.assertIn("host plugin snapshot", completed.stderr)

    def test_host_plugin_snapshot_is_checked(self):
        completed, curl_argv, _ = self.run_verify()
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("/v0/management/plugins\n", curl_argv)

    def test_rejected_coordinator_config_fails_closed(self):
        coordinator_json = (
            '{"plugin":"subscription-pool","status":"reconfigure_rejected","version":"0.0.0-dev",'
            '"generated_at":"2026-09-10T15:00:00Z","validation_error":"rejected",'
            '"providers":{"zai":{"provider":"zai"},"opencode-go":{"provider":"opencode-go"}}}'
        )
        completed, _, _ = self.run_verify(coordinator_status_json=coordinator_json)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("rejected plugin config", completed.stderr)

if __name__ == "__main__":
    unittest.main()
