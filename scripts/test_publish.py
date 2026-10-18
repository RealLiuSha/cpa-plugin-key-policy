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

def write_format7(state_path):
    """A converted dataset as migration-check --out writes it."""
    state_path = Path(state_path)
    publish.write_json(state_path, {"version": 7, "dataset_id": "example", "keys": [], "models": []})
    publish.write_json(state_path.with_name(publish.USAGE_FILE), {"version": 7, "dataset_id": "example", "timezone": "Asia/Shanghai",
                                                                 "history_through": "2026-09-11", "keys": {}})
    publish.write_json(state_path.with_name(publish.HISTORY_FILE),
                       {"version": 7, "dataset_id": "example", "through": "2026-09-11", "days": {}})


def checker(target, models=1, fail=None):
    """Stands in for migration-check: --config passes, --out writes the format 7 dataset."""
    def run(args):
        if "--config" in args:
            return b'{"result":"passed"}'
        if fail:
            raise publish.PublishError(fail)
        if "--out" in args:
            out = Path(args[args.index("--out") + 1])
            out.mkdir()
            write_format7(out / Path(target["state_path"]).name)
        # Converting merges the two format 6 rows that share an upstream model.
        return json.dumps({"result": "passed", "target_version": 7, "keys": 0, "models": models,
                           "policy_sha256": "unchanged-policy"}).encode()
    return run


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
            check = checker(target)
            events = []

            def command(args, **kwargs):
                if Path(args[0]).name == "migration-check":
                    events.append("config" if "--config" in args else "check")
                    return check(args)
                events.append(" ".join(args[:2]))
                return b""

            with contextlib.ExitStack() as stack:
                stack.enter_context(patch.object(publish, "inspect_target", return_value=target))
                stack.enter_context(patch.object(publish, "command", side_effect=command))
                stack.enter_context(patch.object(publish, "docker_inspect", return_value={"State": {"Running": False}}))
                verify = stack.enter_context(patch.object(publish, "verify_runtime", return_value={"ui_sha256": "page"}))
                publish.deploy_worker(directory)
            status = publish.read_json(directory / "status.json")
            self.assertEqual(status["phase"], "succeeded")
            self.assertFalse(status["business_request_tested"])
            self.assertEqual(status["migration"]["target_version"], 7)
            # The configuration is vetted before the stop; conversion and verification run stopped/started.
            self.assertEqual(events, ["config", "docker stop", "check", "docker start", "check"])
            # Start-up verification expects the merged row count, not the format 6 model count.
            self.assertEqual(verify.call_args[0][0]["models"], 1)
            self.assertEqual(Path(target["config_path"]).read_bytes(), changed)
            plugins = Path(target["state_path"]).parent
            self.assertEqual(publish.read_json(target["state_path"])["version"], 7)
            self.assertEqual(publish.read_json(target["usage_path"])["version"], 7)
            self.assertTrue((plugins / publish.HISTORY_FILE).is_file())
            # The format 6 recovery pair left with the old data; the audit log stayed; no staging is left.
            self.assertFalse((plugins / "cpa-key-policy-state.json.before-v6.json").exists())
            self.assertEqual((plugins / publish.AUDIT_FILE).read_bytes(), b'{"action":"create_key"}\n')
            self.assertEqual(sorted(path.name for path in plugins.iterdir() if path.name.startswith(".")), [])
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
        state.write_text(json.dumps({"version": 6, "dataset_id": "example", "keys": [],
                                     "models": [{"name": "Chat", "provider": "codex", "target_model": "gpt-5.6"},
                                                {"name": "Fast", "provider": "codex", "target_model": "gpt-5.6"}]}))
        usage.write_text(json.dumps({"version": 6, "dataset_id": "example", "usage": {}}))
        recovery = plugins / "cpa-key-policy-state.json.before-v6.json"
        recovery.write_text("{}")
        audit = plugins / publish.AUDIT_FILE
        audit.write_bytes(b'{"action":"create_key"}\n')
        target = {
            "remote_dir": str(root), "container": "cpa-example", "container_id": "test-container",
            "image_id": "fixed-image", "config_path": str(config), "config_sha256": publish.sha(CONFIG),
            "plugin_path": str(old), "plugin_sha256": publish.sha(old.read_bytes()),
            "plugin_dir": str(plugins), "plugin_dir_container": "/CLIProxyAPI/plugins",
            "plugin_container_path": "/CLIProxyAPI/plugins/linux/amd64/" + old.name,
            "state_path": str(state), "usage_path": str(usage), "dataset_id": "example",
            "backup_files": [str(config), str(old), str(state), str(usage), str(recovery), str(audit)],
            "version": "0.6.0", "timezone": "Asia/Shanghai", "keys": 0, "models": 2,
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

    def test_failed_conversion_restarts_old_version_without_touching_data(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory, target = self.fixture(Path(temporary))
            original = {name: Path(target[name]).read_bytes() for name in ("state_path", "usage_path")}
            events = []
            check = checker(target, fail="migration rejected")

            def command(args, **kwargs):
                if Path(args[0]).name == "migration-check":
                    return check(args)
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
            self.assertEqual(status["detail"], "新版尚未启动，原版本已恢复；账本未改动")
            manifest = publish.read_json(Path(status["backup"]) / "manifest.json")
            for item in manifest["files"]:
                self.assertEqual(publish.sha((Path(status["backup"]) / item["file"]).read_bytes()), item["sha256"])

    def test_failure_after_start_preserves_new_ledger_and_never_rolls_back(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory, target = self.fixture(Path(temporary))
            new_usage = b'{"version":7,"dataset_id":"example","timezone":"Asia/Shanghai","history_through":"","keys":{"new-charge":{}}}'

            check = checker(target)

            def command(args, **kwargs):
                if Path(args[0]).name == "migration-check":
                    return check(args)
                if args[:2] == ["docker", "start"]:
                    Path(target["usage_path"]).write_bytes(new_usage)
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

    def test_live_usage_without_its_history_file_after_start_needs_attention(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory, target = self.fixture(Path(temporary))

            check = checker(target)

            def command(args, **kwargs):
                if Path(args[0]).name == "migration-check":
                    return check(args)
                if args[:2] == ["docker", "start"]:
                    Path(target["usage_path"]).with_name(publish.HISTORY_FILE).unlink()
                return b""

            with contextlib.ExitStack() as stack:
                stack.enter_context(patch.object(publish, "inspect_target", return_value=target))
                stack.enter_context(patch.object(publish, "command", side_effect=command))
                stack.enter_context(patch.object(publish, "docker_inspect", return_value={"State": {"Running": False}}))
                stack.enter_context(patch.object(publish, "verify_runtime", return_value={"ui_sha256": "page"}))
                restore = stack.enter_context(patch.object(publish, "restore_before_start"))
                publish.deploy_worker(directory)
            status = publish.read_json(directory / "status.json")
            self.assertEqual(status["phase"], "needs_attention")
            self.assertIn("history", status["error"])
            restore.assert_not_called()

    def test_failure_after_the_swap_restores_the_format_6_data_before_start(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory, target = self.fixture(Path(temporary))
            plugins = Path(target["state_path"]).parent
            original = {path.name: path.read_bytes() for path in plugins.iterdir() if path.is_file()}
            check = checker(target)

            def command(args, **kwargs):
                if Path(args[0]).name == "migration-check":
                    return check(args)
                if args[:2] == ["docker", "stop"]:
                    # The library disappears after the preflight hash check: installing it fails after the swap.
                    (directory / "cpa-key-policy-v0.7.0.so").unlink()
                return b""

            with contextlib.ExitStack() as stack:
                stack.enter_context(patch.object(publish, "inspect_target", return_value=target))
                stack.enter_context(patch.object(publish, "command", side_effect=command))
                stack.enter_context(patch.object(publish, "docker_inspect", return_value={"State": {"Running": False}}))
                stack.enter_context(patch.object(publish, "verify_runtime"))
                publish.deploy_worker(directory)
            status = publish.read_json(directory / "status.json")
            self.assertEqual(status["phase"], "restored")
            self.assertEqual(status["detail"], "新版尚未启动，原版本和停机时的账本已从备份恢复")
            self.assertEqual({path.name: path.read_bytes() for path in plugins.iterdir() if path.is_file()}, original)
            self.assertEqual(Path(target["config_path"]).read_bytes(), CONFIG)

    def test_paired_data_accepts_formats_6_and_7_and_requires_the_referenced_history(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            state, usage, history = root / "state.json", root / publish.USAGE_FILE, root / publish.HISTORY_FILE
            publish.write_json(state, {"version": 6, "dataset_id": "d"})
            publish.write_json(usage, {"version": 6, "dataset_id": "d", "usage": {}})
            publish.paired_data(state, usage)
            publish.write_json(state, {"version": 7, "dataset_id": "d"})
            publish.write_json(usage, {"version": 7, "dataset_id": "d", "history_through": "", "keys": {}})
            publish.paired_data(state, usage)
            publish.write_json(usage, {"version": 7, "dataset_id": "d", "history_through": "2026-09-11", "keys": {}})
            with self.assertRaises(publish.PublishError):
                publish.paired_data(state, usage)
            publish.write_json(history, {"version": 7, "dataset_id": "d", "through": "2026-09-10", "days": {}})
            with self.assertRaises(publish.PublishError):
                publish.paired_data(state, usage)
            publish.write_json(history, {"version": 7, "dataset_id": "d", "through": "2026-09-11", "days": {}})
            publish.paired_data(state, usage)
            publish.write_json(state, {"version": 5, "dataset_id": "d"})
            with self.assertRaises(publish.PublishError):
                publish.paired_data(state, usage)

    def test_ledger_files_cover_history_audit_rotations_and_every_recovery_pair(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            state = root / "state.json"
            expected = [root / publish.USAGE_FILE, root / publish.HISTORY_FILE, root / publish.AUDIT_FILE,
                        root / (publish.AUDIT_FILE + ".1"), root / "state.json.before-v6.json",
                        root / "state.json.before-v7.json", root / "state.json.before-v7.json.0123abcd"]
            for path in [state, *expected, root / "unrelated.json", root / "state.json.bak"]:
                path.write_bytes(b"{}")
            self.assertEqual(sorted(publish.ledger_files(state)), sorted(str(path) for path in expected))


if __name__ == "__main__":
    unittest.main()
