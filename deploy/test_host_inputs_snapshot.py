"""Current-host executable inputs: in-process key intake and the real
snapshot-service feed envelope.

The host's usage-snapshot service writes feeds with exactly the top-level
keys ``observedAt``/``records``/``schemaVersion``/``staleAfterSeconds`` (no
``lane`` key, no collector record shape), and the host holds no standalone
key files -- only the established ``secure-drop/cliproxy.env`` sources keys
in-process. These tests prove snapshot-only acceptance runs against that
real envelope with environment keys and no key files, without invoking
collectors or rewriting feeds.
"""

import datetime
import json
import os
import pathlib
import subprocess
import tempfile
import textwrap
import unittest

import hermetic
from hermetic import tearDownModule  # noqa: F401  (fails the module on a bypassed stub)

ROOT = pathlib.Path(__file__).resolve().parents[1]
ZAI_SCRIPT = ROOT / "deploy" / "verify-live.sh"
GO_SCRIPT = ROOT / "deploy" / "verify-live-opencodego.sh"

MANAGEMENT_MARKER = "fixture-management-marker"
PLAN_MARKER = "fixture-plan-marker"
# The host's /v0/management/plugins answer while the pool plugin is live.
REGISTERED_PLUGIN_LISTING = {"plugins_enabled": True, "plugins": [{"id": "subscription-pool", "registered": True, "effective_enabled": True}]}

# Exact record-key sets observed on the host (values below are synthetic).
ZAI_RECORD_KEYS = (
    "account_key",
    "five_hour_credits_bucket",
    "five_hour_credits_used",
    "five_hour_resets_at",
    "five_hour_utilization",
    "governing_window",
    "health",
    "lane",
    "plan",
    "source",
    "weekly_credits_bucket",
    "weekly_credits_used",
    "weekly_resets_at",
    "weekly_utilization",
    "weight",
    "window_seconds",
)
GO_RECORD_KEYS = (
    "account",
    "account_key",
    "cn_capable",
    "governing_window",
    "health",
    "health_floor_reason",
    "lane",
    "observationQuality",
    "weight",
    "window_seconds",
)


def fresh_observed():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def zai_feed(records=1, observed=None, stale_after=300, drop=None, leak=None):
    record = {key: f"fixture-{key}-value" for key in ZAI_RECORD_KEYS}
    if leak:
        record["source"] = leak
    payload = {
        "observedAt": observed or fresh_observed(),
        "records": [dict(record) for _ in range(records)],
        "schemaVersion": 1,
        "staleAfterSeconds": stale_after,
    }
    if drop:
        del payload[drop]
    return json.dumps(payload)


def go_feed(records=3, observed=None, stale_after=300, leak=None):
    record = {key: f"fixture-{key}-value" for key in GO_RECORD_KEYS}
    if leak:
        record["health_floor_reason"] = leak
    return json.dumps(
        {
            "observedAt": observed or fresh_observed(),
            "records": [dict(record) for _ in range(records)],
            "schemaVersion": 1,
            "staleAfterSeconds": stale_after,
        }
    )


def zai_status_payload(coordinator=False):
    if coordinator:
        return {
            "plugin": "subscription-pool",
            "status": "registered",
            "version": "0.0.0-dev",
            "generated_at": "2026-09-10T15:00:00Z",
            "providers": {"zai": {"provider": "zai"}, "opencode-go": {"provider": "opencode-go"}},
        }
    return {
        "plugin": "zai-coding-plan",
        "status": "registered",
        "version": "0.0.0-dev",
        "generated_at": "2026-09-10T15:00:00Z",
        "accounts": [
            {
                "identity": "bb" * 32,
                "cooldown": {"active": False, "until": None, "reason": "", "source": "zai_runtime_health_v1"},
                "name": "zai-pro-1",
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
        ],
    }


def go_status_payload(credential_bound):
    if credential_bound:
        windows = {
            "five_hour": {"known": True, "utilization": 0.1, "exhausted": False, "resets_at": "2026-09-10T20:00:00Z", "source": "poll", "authoritative": True},
            "weekly": {"known": True, "utilization": 0.2, "exhausted": False, "resets_at": "2026-09-16T00:00:00Z", "source": "poll", "authoritative": True},
            "monthly": {"known": True, "utilization": 0.05, "exhausted": False, "resets_at": "2026-10-01T00:00:00Z", "source": "poll", "authoritative": True},
        }
        accounts = [{"name": "opencodego-1", "disabled": False, "windows": windows}]
        gaps = []
    else:
        windows = {kind: {"known": False, "exhausted": False} for kind in ("five_hour", "weekly", "monthly")}
        accounts = [{"name": "opencodego-1", "disabled": True, "windows": windows}]
        gaps = ["no bound dashboard credential"]
    return {
        "plugin": "subscription-pool",
        "status": "registered",
        "version": "0.0.0-dev",
        "generated_at": "2026-09-10T15:00:00Z",
        "providers": {
            "opencode-go": {
                "provider": "opencode-go",
                "status": "registered",
                "credential_bound": credential_bound,
                "observation_gaps": gaps,
                "accounts": accounts,
            }
        },
    }


class SnapshotHarness(unittest.TestCase):
    def write_executable(self, path, body):
        path.write_text(textwrap.dedent(body).lstrip())
        path.chmod(0o700)

    def run_script(self, script, feed_name, feed_json, status_payload, env_extra, collector_var, coordinator_payload=None):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            usage_dir = root / "usage"
            usage_dir.mkdir(parents=True)
            (usage_dir / feed_name).write_text(feed_json)
            if coordinator_payload is None:
                coordinator_payload = status_payload
            stub = (
                "#!/usr/bin/env python3\n"
                "import json, sys\n"
                "args = sys.argv[1:]\n"
                'if "%{http_code}" in args:\n'
                '    print("401", end="")\n'
                'elif args[-1].endswith("/v0/management/plugins"):\n'
                f"    print(json.dumps({REGISTERED_PLUGIN_LISTING!r}))\n"
                "elif \"subscription-pool/status\" in \" \".join(args):\n"
                f"    print(json.dumps({coordinator_payload!r}))\n"
                "else:\n"
                f"    print(json.dumps({status_payload!r}))\n"
            )
            self.write_executable(bin_dir / "curl", stub)
            self.write_executable(bin_dir / "docker", "#!/usr/bin/env python3\nprint('service ok')\n")
            self.write_executable(
                bin_dir / "collector-blocker",
                "#!/usr/bin/env python3\nraise SystemExit('collector must not run in snapshot-only mode')\n",
            )
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{bin_dir}:{env['PATH']}",
                    "CLIPROXY_USAGE_DIR": str(usage_dir),
                    "CLIPROXY_CONTAINER": "cliproxy-test",
                    "CLIPROXY_SNAPSHOT_ONLY": "1",
                    "CLIPROXY_JOURNAL_TIMEOUT": "1",
                    collector_var: str(bin_dir / "collector-blocker"),
                }
            )
            env.pop("CLIPROXY_DASHBOARD_URL", None)
            # No key files exist in this fixture: intake is environment-only.
            env.pop("CLIPROXY_MANAGEMENT_KEY_FILE", None)
            env.pop("ZAI_CODING_PLAN_KEY_FILE", None)
            env.pop("OPENCODE_GO_DASHBOARD_API_KEY_FILE", None)
            env.update(env_extra)
            completed = subprocess.run([str(script)], cwd=ROOT, env=env, text=True, capture_output=True)
            # The fixture feed must survive the run untouched.
            self.assertEqual((usage_dir / feed_name).read_text(), feed_json)
            return completed


class ZaiHostInputsTest(SnapshotHarness):
    def run_zai(self, feed_json, extra=None):
        env_extra = {"CLIPROXY_MANAGEMENT_KEY": MANAGEMENT_MARKER}
        if extra:
            env_extra.update(extra)
        return self.run_script(
            ZAI_SCRIPT,
            "zai.json",
            feed_json,
            zai_status_payload(False),
            env_extra,
            "COLLECTOR_ZAI",
            coordinator_payload=zai_status_payload(True),
        )

    def test_real_envelope_passes_with_env_intake_and_no_key_files(self):
        completed = self.run_zai(zai_feed())
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("snapshot-only", completed.stdout)
        self.assertIn("plan-key marker scan", completed.stderr)

    def test_real_envelope_passes_with_plan_marker_present(self):
        completed = self.run_zai(zai_feed(), {"ZAI_CODING_PLAN_KEY": PLAN_MARKER})
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn("plan-key marker scan", completed.stderr)

    def test_stale_real_envelope_fails_freshness(self):
        completed = self.run_zai(zai_feed(observed="2026-09-10T15:00:00Z"))
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("stale", completed.stderr)

    def test_envelope_missing_records_key_fails(self):
        completed = self.run_zai(zai_feed(drop="records"))
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("envelope", completed.stderr)

    def test_management_marker_leak_in_feed_value_fails(self):
        completed = self.run_zai(zai_feed(leak=MANAGEMENT_MARKER))
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("confidential-value scan", completed.stderr)

    def test_empty_env_key_fails_closed(self):
        completed = self.run_zai(zai_feed(), {"CLIPROXY_MANAGEMENT_KEY": ""})
        self.assertNotEqual(completed.returncode, 0)

    def test_missing_management_input_fails_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            env = os.environ.copy()
            env.update({"PATH": f"{bin_dir}:{env['PATH']}", "CLIPROXY_CONTAINER": "x", "CLIPROXY_SNAPSHOT_ONLY": "1"})
            for var in ("CLIPROXY_MANAGEMENT_KEY_FILE", "CLIPROXY_MANAGEMENT_KEY", "ZAI_CODING_PLAN_KEY_FILE", "ZAI_CODING_PLAN_KEY"):
                env.pop(var, None)
            completed = subprocess.run([str(ZAI_SCRIPT)], cwd=ROOT, env=env, text=True, capture_output=True)
            self.assertNotEqual(completed.returncode, 0)
            self.assertIn("CLIPROXY_MANAGEMENT_KEY_FILE", completed.stderr)


class GoHostInputsTest(SnapshotHarness):
    def run_go(self, feed_json, credential_bound, extra=None):
        env_extra = {"CLIPROXY_MANAGEMENT_KEY": MANAGEMENT_MARKER}
        if extra:
            env_extra.update(extra)
        return self.run_script(
            GO_SCRIPT, "opencode-go.json", feed_json, go_status_payload(credential_bound), env_extra, "COLLECTOR_OPENCODEGO"
        )

    def test_real_envelope_unbound_passes_without_dashboard_input(self):
        completed = self.run_go(go_feed(), False, {"OPENCODE_GO_ALLOW_UNBOUND": "1"})
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("UNAVAILABLE", completed.stdout)

    def test_bound_mode_requires_dashboard_input(self):
        completed = self.run_go(go_feed(), True)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("OPENCODE_GO_DASHBOARD_API_KEY_FILE", completed.stderr)

    def test_stale_real_envelope_fails_freshness(self):
        completed = self.run_go(go_feed(observed="2026-09-10T15:00:00Z"), False, {"OPENCODE_GO_ALLOW_UNBOUND": "1"})
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("stale", completed.stderr)

    def test_management_marker_leak_in_feed_value_fails(self):
        completed = self.run_go(go_feed(leak=MANAGEMENT_MARKER), False, {"OPENCODE_GO_ALLOW_UNBOUND": "1"})
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("confidential-value scan", completed.stderr)


if __name__ == "__main__":
    unittest.main()
