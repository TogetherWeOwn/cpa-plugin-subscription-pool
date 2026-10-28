import hashlib
import json
import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
README = ROOT / "deploy" / "README.md"
CONFIG = ROOT / "deploy" / "config.yaml.tmpl"
REGISTRY = ROOT / "registry.json"
PINNED_COMMIT = "RELEASE_SHA"
PINNED_DIGEST = "REGISTRY_SHA256"


class RunbookSecurityTest(unittest.TestCase):
    def test_management_key_is_not_interpolated_into_curl_arguments(self):
        text = README.read_text() + (ROOT / "deploy" / "verify-live.sh").read_text()
        self.assertNotRegex(text, r'-H\s+["\']Authorization: Bearer \$\{?management_key')
        self.assertNotIn("management_key=$(<", text)
        self.assertIn('--config "$curl_config"', text)

    def test_registry_source_is_immutable_and_digest_verified(self):
        config = CONFIG.read_text()
        readme = README.read_text()
        self.assertNotIn("/main/registry.json", config)
        version = json.loads(REGISTRY.read_text())["plugins"][0]["version"]
        self.assertRegex(version, r"^\d+\.\d+\.\d+$")
        self.assertIn(f"v{version}/registry.json", config)
        self.assertIn(f'release_dir=/path/to/downloaded-v{version}-release-assets', readme)
        self.assertIn('"$release_dir/registry.json"', readme)
        self.assertIn('grep -F "  registry.json" "$release_dir/checksums.txt"', readme)
        self.assertIn("sha256sum --check --status", readme)
        self.assertIn('release_sha=$(<"$release_dir/release-sha.txt")', readme)
        self.assertEqual(len(hashlib.sha256(REGISTRY.read_bytes()).hexdigest()), 64)

    def test_config_replacement_is_same_directory_fsynced_and_atomic(self):
        install = README.read_text().split("## Exact host install and validation", 1)[1].split("## Rollback", 1)[0]
        self.assertIn('mktemp --tmpdir="$config_dir"', install)
        self.assertIn("os.fsync(handle.fileno())", install)
        self.assertIn('mv -T "$candidate" "$config"', install)
        self.assertIn("os.fsync(directory_fd)", install)
        self.assertIn('docker restart "$container"', install)
        self.assertNotIn("systemctl", install)
        self.assertNotIn("OmniRoute", install)

    def test_rollback_restores_atomically_and_reloads_running_service(self):
        rollback = README.read_text().split("## Rollback", 1)[1]
        script = (ROOT / "deploy" / "rollback.sh").read_text()
        self.assertIn("config=/home/ubuntu/stacks/cliproxy/config.yaml", rollback)
        self.assertIn('"$repo/deploy/rollback.sh"', rollback)
        self.assertIn("CLIPROXY_CONTAINER=", rollback)
        self.assertIn("CLIPROXY_CONFIG_UID", rollback)
        self.assertIn('install -m 0600 -- "$CLIPROXY_BACKUP" "$restored"', script)
        self.assertIn('mktemp --tmpdir="$config_dir"', script)
        self.assertIn('mv -T -- "$restored" "$CLIPROXY_CONFIG"', script)
        self.assertIn("os.fsync(directory_fd)", script)
        self.assertIn('docker restart "$CLIPROXY_CONTAINER"', script)

    def test_rollback_removes_usage_output_with_no_follow_helper(self):
        script = (ROOT / "deploy" / "rollback.sh").read_text()
        helper = (ROOT / "deploy" / "remove-usage-output.py").read_text()
        self.assertIn('remove-usage-output.py', script)
        self.assertNotIn("rm -f /srv/cliproxy-usage/zai.json", script)
        self.assertIn("O_NOFOLLOW", helper)
        self.assertIn("dir_fd=directory_fd", helper)
        self.assertIn("follow_symlinks=False", helper)
        self.assertIn("os.unlink(path.name, dir_fd=directory_fd)", helper)

    def test_plugin_install_curl_is_bounded_and_suppresses_error_bodies(self):
        install = README.read_text().split("# After the container reload exposes the custom source", 1)[1].split("usage_dir=", 1)[0]
        for option in (
            "--fail",
            "--fail-early",
            "--max-redirs 0",
            "--connect-timeout 2",
            "--max-time 10",
            "--max-filesize 1048576",
            '--output "$install_response"',
            '--stderr "$install_error"',
        ):
            self.assertIn(option, install)
        self.assertNotIn("--fail-with-body", install)
        self.assertIn("response body suppressed", install)
        self.assertIn("plugin install response did not confirm the expected release", install)

    def test_usage_directory_is_validated_before_any_mutating_action(self):
        install = README.read_text().split("## Exact host install and validation", 1)[1].split(
            "## OpenCode Go", 1
        )[0]
        tail = install.split("usage_dir=/srv/cliproxy-usage", 1)[1].split(
            "CLIPROXY_MANAGEMENT_KEY_FILE=", 1
        )[0]
        self.assertIn('python3 "$repo/deploy/prepare-usage-dir.py" "$usage_dir"', tail)
        self.assertNotIn("install -d", tail)
        self.assertNotIn("chmod", tail)
        self.assertNotIn("chown", tail)

    def test_rollback_delete_is_bounded_and_suppresses_response_body(self):
        script = (ROOT / "deploy" / "rollback.sh").read_text()
        self.assertLess(script.index("install -m 0600"), script.index("-X DELETE"))
        for option in (
            "--fail",
            "--fail-early",
            "--max-redirs 0",
            "--connect-timeout 2",
            "--max-time 5",
            "--max-filesize 1048576",
            '--output "$delete_response"',
            '--stderr "$delete_error"',
        ):
            self.assertIn(option, script)
        self.assertNotIn("--fail-with-body", script)
        rollback = README.read_text().split("## Rollback", 1)[1]
        self.assertIn("response body suppressed", script)
        self.assertIn("umask 077", script)
        self.assertRegex(script, r"grep -Eq '[^']+' \"\$delete_error\"")

    def test_runbook_passes_both_secret_marker_files_to_live_verification(self):
        text = README.read_text()
        self.assertIn("CLIPROXY_MANAGEMENT_KEY_FILE=", text)
        self.assertIn("ZAI_CODING_PLAN_KEY_FILE=", text)
        self.assertIn("CLIPROXY_SNAPSHOT_ONLY=1", text)
        self.assertIn("CLIPROXY_CONTAINER=", text)
        self.assertIn("OPENCODE_GO_ALLOW_UNBOUND=1", text)
        self.assertIn("OmniRoute is retired", text)
        install = text.split("## Exact host install and validation", 1)[1].split("## Rollback", 1)[0]
        self.assertNotIn("CLIPROXY_DASHBOARD_URL=", install)
        self.assertNotIn("CLIPROXY_SERVICE_UNIT=", install)

    def test_current_host_section_names_docker_layout_and_snapshot_discipline(self):
        section = README.read_text().split("### Current-host Docker acceptance", 1)[1].split("### Release preflight", 1)[0]
        for marker in (
            "/home/ubuntu/stacks/cliproxy/config.yaml",
            "/CLIProxyAPI/config.yaml",
            "/home/ubuntu/stacks/cliproxy/plugins/linux/amd64/",
            "/home/ubuntu/secure-drop/cliproxy.env",
            "CLIPROXY_MGMT_KEY",
            "CLIPROXY_SNAPSHOT_ONLY=1",
            "docker ps",
            "UNAVAILABLE",
            "identity",
            "cooldown",
            "ABI 1",
        ):
            self.assertIn(marker, section)


if __name__ == "__main__":
    unittest.main()
