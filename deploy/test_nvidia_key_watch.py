import importlib.util
import json
import os
import pathlib
import stat
import tempfile
import unittest

MODULE_PATH = pathlib.Path(__file__).with_name("nvidia-key-watch.py")
SPEC = importlib.util.spec_from_file_location("nvidia_key_watch", MODULE_PATH)
watcher = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(watcher)

KEY = "nvapi-test-only-0123456789abcdefghijklmnop"


class NvidiaKeyWatchTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.source = self.root / "nvidia.env"
        self.dest = self.root / "nvidia.provider.json"

    def source_key(self, value=KEY):
        self.source.write_text("KEY=" + value + "\n")
        self.source.chmod(0o600)

    def test_missing_and_placeholder_wait_without_output(self):
        self.assertEqual(watcher.pickup(self.source, self.dest), "waiting_for_key")
        self.source_key("placeholder")
        self.assertEqual(watcher.pickup(self.source, self.dest), "waiting_for_key")
        self.assertFalse(self.dest.exists())

    def test_pickup_stages_only_no_model_entitlement_or_activation(self):
        self.source_key()
        self.assertEqual(watcher.pickup(self.source, self.dest), "staged_not_activated")
        value = json.loads(self.dest.read_text())
        provider = value["openai-compatibility"][0]
        self.assertEqual(provider["name"], "nvidia-nim")
        self.assertEqual(provider["base-url"], "https://integrate.api.nvidia.com/v1")
        self.assertEqual(provider["api-key-entries"], [{"api-key": KEY}])
        self.assertEqual(provider["models"], [])
        self.assertNotIn("plugins", value)
        self.assertEqual(stat.S_IMODE(self.dest.stat().st_mode), 0o600)
        self.assertEqual(watcher.pickup(self.source, self.dest), "already_staged_not_activated")

    def test_never_overwrites_a_different_existing_key(self):
        self.source_key()
        watcher.pickup(self.source, self.dest)
        original = self.dest.read_bytes()
        self.source_key(KEY + "different")
        with self.assertRaisesRegex(watcher.PickupError, "existing_stage_conflict"):
            watcher.pickup(self.source, self.dest)
        self.assertEqual(self.dest.read_bytes(), original)

    def test_rejects_public_source_symlinks_and_malformed_env_without_key_echo(self):
        self.source_key()
        self.source.chmod(0o644)
        with self.assertRaises(watcher.PickupError):
            watcher.pickup(self.source, self.dest)
        self.source.chmod(0o600)
        link = self.root / "link.env"
        link.symlink_to(self.source)
        with self.assertRaises(watcher.PickupError):
            watcher.pickup(link, self.dest)
        for data in ("KEY=" + KEY + "\nKEY=" + KEY, "KEY=" + KEY + "\nOTHER=secret", "KEY=$(touch /tmp/not-run)", "KEY=" + KEY + " bad", "KEY=short"):
            with self.subTest(data=data):
                self.source.write_text(data)
                with self.assertRaises(watcher.PickupError) as raised:
                    watcher.pickup(self.source, self.dest)
                self.assertNotIn(KEY, str(raised.exception))

    def test_rejects_symlink_or_public_destination_and_public_directory(self):
        self.source_key()
        other = self.root / "other"
        other.write_text("untouched")
        self.dest.symlink_to(other)
        with self.assertRaises(watcher.PickupError):
            watcher.pickup(self.source, self.dest)
        self.assertEqual(other.read_text(), "untouched")
        self.dest.unlink()
        self.dest.write_text("{}")
        self.dest.chmod(0o644)
        with self.assertRaises(watcher.PickupError):
            watcher.pickup(self.source, self.dest)
        self.dest.unlink()
        self.root.chmod(0o755)
        with self.assertRaises(watcher.PickupError):
            watcher.pickup(self.source, self.dest)

    def test_receipt_contains_only_closed_vocabulary(self):
        self.source_key()
        receipt = watcher.receipt(watcher.pickup(self.source, self.dest))
        self.assertEqual(receipt, {"provider": "nvidia-nim", "status": "staged_not_activated", "production_changed": False})
        self.assertNotIn(KEY, json.dumps(receipt))


if __name__ == "__main__":
    unittest.main()
