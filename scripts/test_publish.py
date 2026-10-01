"""Filesystem recovery checks; never connects to a server or starts a container."""
import contextlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("publish", Path(__file__).with_name("publish.py"))
publish = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(publish)

CONFIG = b'''# keep this file's formatting
port: 8317
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    cpa-key-policy:
      enabled: true
      state_file: /CLIProxyAPI/plugins/cpa-key-policy-state.json
      store:
        version: "0.6.0" # selected version
        release-tag: "v0.6.0"
    other:
      store: {version: "0.6.0"}
'''


class PublishTests(unittest.TestCase):
    def test_successful_publish_bumps_minor_and_preserves_unrelated_configuration(self):
        self.assertEqual(publish.next_version("0.6.0", "0.5.1"), "0.7.0")
        self.assertEqual(publish.next_version("0.7.0", "0.7.0"), "0.8.0")
        changed = publish.patch_config(CONFIG, "0.7.0")
        self.assertIn(b'version: "0.7.0" # selected version', changed)
        self.assertIn(b'store: {version: "0.6.0"}', changed)
        self.assertEqual(changed.count(b'"0.7.0"'), 1)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name in publish.VERSION_FILES:
                (root / name).parent.mkdir(parents=True, exist_ok=True)
            (root / publish.VERSION_FILES[0]).write_text('package plugin\nconst Version = "0.6.0"\n')
            (root / publish.VERSION_FILES[1]).write_text('{"version":"0.6.0","dependencies":{"dep":"0.6.0"}}')
            (root / publish.VERSION_FILES[2]).write_text('{"version":"0.6.0","packages":{"":{"version":"0.6.0"},"node_modules/dep":{"version":"0.6.0"}}}')
            publish.bump_source(root, "0.6.0", "0.7.0")
            self.assertEqual(publish.source_version(root), "0.7.0")
            self.assertEqual(publish.read_json(root / publish.VERSION_FILES[2])["packages"]["node_modules/dep"]["version"], "0.6.0")
            directory, target = self.fixture(root)

            def command(args, **kwargs):
                if Path(args[0]).name == "migration-check":
                    return b'{"result":"passed","target_version":5,"policy_sha256":"unchanged-policy"}'
                if args[:2] == ["docker", "start"]:
                    for name in ("state_path", "usage_path"):
                        value = publish.read_json(target[name])
                        value["version"] = 5
                        publish.write_json(target[name], value)
                return b""

            with contextlib.ExitStack() as stack:
                stack.enter_context(patch.object(publish, "inspect_target", return_value=target))
                stack.enter_context(patch.object(publish, "command", side_effect=command))
                stack.enter_context(patch.object(publish, "docker_inspect", return_value={"State": {"Running": False}}))
                stack.enter_context(patch.object(publish, "verify_runtime", return_value={"ui_sha256": "page"}))
                publish.deploy_worker(directory)
            status = publish.read_json(directory / "status.json")
            self.assertEqual(status["phase"], "succeeded")
            self.assertFalse(status["business_request_tested"])
            self.assertEqual(Path(target["config_path"]).read_bytes(), changed)
            self.assertEqual(publish.read_json(target["state_path"])["version"], 5)
            self.assertTrue(Path(status["backup"]).is_dir())
        args = publish.ssh_arguments('ssh -p 2222 -i "/key with spaces" root@example')
        self.assertIn('/key with spaces', args)
        with self.assertRaises(publish.PublishError):
            publish.ssh_arguments('ssh root@example echo unexpected')

    def fixture(self, root):
        plugins = root / "cpa" / "plugins"
        libraries = plugins / "linux" / "amd64"
        libraries.mkdir(parents=True)
        config = root / "cpa" / "config.yaml"
        config.write_bytes(CONFIG)
        old = libraries / "cpa-key-policy-v0.6.0.so"
        old.write_bytes(b"old plugin")
        state = plugins / "cpa-key-policy-state.json"
        usage = plugins / "cpa-key-policy-usage.json"
        state.write_text(json.dumps({"version": 4, "dataset_id": "example", "keys": [], "models": []}))
        usage.write_text(json.dumps({"version": 4, "dataset_id": "example", "usage": {}}))
        target = {
            "remote_dir": str(root), "container": "cpa-example", "container_id": "test-container",
            "image_id": "fixed-image", "config_path": str(config), "config_sha256": publish.sha(CONFIG),
            "plugin_path": str(old), "plugin_sha256": publish.sha(old.read_bytes()),
            "plugin_dir": str(plugins), "plugin_dir_container": "/CLIProxyAPI/plugins",
            "plugin_container_path": "/CLIProxyAPI/plugins/linux/amd64/" + old.name,
            "state_path": str(state), "usage_path": str(usage), "dataset_id": "example",
            "backup_files": [str(config), str(old), str(state), str(usage)],
            "version": "0.6.0", "timezone": "Asia/Shanghai", "keys": 0, "models": 0,
        }
        directory = root / ".cpa-key-policy-publish" / "20260912T000000Z-12345678"
        directory.mkdir(parents=True)
        library = "cpa-key-policy-v0.7.0.so"
        (directory / library).write_bytes(b"new plugin")
        (directory / "migration-check").write_bytes(b"checker")
        publish.write_json(directory / "request.json", {
            "target": target, "version": "0.7.0", "library": library, "ui_sha256": "page",
            "artifact_hashes": {name: publish.sha((directory / name).read_bytes()) for name in (library, "migration-check")},
        })
        return directory, target

    def test_failed_migration_rehearsal_restarts_old_version_without_touching_data(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory, target = self.fixture(Path(temporary))
            original = {name: Path(target[name]).read_bytes() for name in ("state_path", "usage_path")}
            events = []

            def command(args, **kwargs):
                if Path(args[0]).name == "migration-check":
                    raise publish.PublishError("migration rejected")
                events.append(args[:2])
                return b""

            with contextlib.ExitStack() as stack:
                stack.enter_context(patch.object(publish, "inspect_target", return_value=target))
                stack.enter_context(patch.object(publish, "command", side_effect=command))
                stack.enter_context(patch.object(publish, "docker_inspect", return_value={"State": {"Running": False}}))
                verify = stack.enter_context(patch.object(publish, "verify_runtime"))
                publish.deploy_worker(directory)
            status = publish.read_json(directory / "status.json")
            self.assertEqual(status["phase"], "restored")
            self.assertEqual(events, [["docker", "stop"], ["docker", "start"]])
            verify.assert_called_once_with(target, "0.6.0", target["plugin_container_path"])
            self.assertEqual(Path(target["config_path"]).read_bytes(), CONFIG)
            for name, raw in original.items():
                self.assertEqual(Path(target[name]).read_bytes(), raw)
            manifest = publish.read_json(Path(status["backup"]) / "manifest.json")
            for item in manifest["files"]:
                self.assertEqual(publish.sha((Path(status["backup"]) / item["file"]).read_bytes()), item["sha256"])

    def test_failure_after_start_preserves_new_ledger_and_never_rolls_back(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory, target = self.fixture(Path(temporary))
            new_usage = b'{"version":5,"dataset_id":"example","usage":{"new-charge":{"usd":1}}}'

            def command(args, **kwargs):
                if Path(args[0]).name == "migration-check":
                    return b'{"result":"passed","target_version":5,"policy_sha256":"unchanged-policy"}'
                if args[:2] == ["docker", "start"]:
                    Path(target["usage_path"]).write_bytes(new_usage)
                    state = publish.read_json(target["state_path"])
                    state["version"] = 5
                    publish.write_json(target["state_path"], state)
                return b""

            with contextlib.ExitStack() as stack:
                stack.enter_context(patch.object(publish, "inspect_target", return_value=target))
                stack.enter_context(patch.object(publish, "command", side_effect=command))
                stack.enter_context(patch.object(publish, "docker_inspect", return_value={"State": {"Running": False}}))
                stack.enter_context(patch.object(publish, "verify_runtime", side_effect=publish.PublishError("page mismatch")))
                restore = stack.enter_context(patch.object(publish, "restore_before_start"))
                publish.deploy_worker(directory)
            status = publish.read_json(directory / "status.json")
            self.assertEqual(status["phase"], "needs_attention")
            restore.assert_not_called()
            self.assertEqual(Path(target["usage_path"]).read_bytes(), new_usage)
            self.assertIn(b'version: "0.7.0"', Path(target["config_path"]).read_bytes())
            self.assertTrue(Path(status["backup"]).is_dir())


if __name__ == "__main__":
    unittest.main()
