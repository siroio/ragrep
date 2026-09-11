import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from staged_smoke import copy_asset_cache, mcp_exchange, system_path


class HungProcess:
    def __init__(self):
        self.killed = False

    def communicate(self, input=None, timeout=None):
        if self.killed:
            return b"", b""
        raise subprocess.TimeoutExpired(["fake-mcp"], timeout)

    def kill(self):
        self.killed = True


class ErrorProcess:
    def communicate(self, input=None, timeout=None):
        return (json.dumps({"jsonrpc": "2.0", "id": 1, "error": {"code": -1}}).encode() + b"\n", b"")


class StagedSmokeTests(unittest.TestCase):
    def test_release_workflow_uses_built_archive_binary_for_gates(self):
        workflow = Path(__file__).parents[2] / ".github" / "workflows" / "release-verification.yml"
        content = workflow.read_text(encoding="utf-8")
        self.assertIn('mkdir -p "$XDG_CACHE_HOME" "$XDG_CONFIG_HOME"', content)
        self.assertIn("tools/release/release.py build", content)
        self.assertNotIn("go -C src build", content)
        self.assertIn("--ragrep \"$RUNNER_TEMP/extracted/ragrep\"", content)
        self.assertIn("--binary \"$RUNNER_TEMP/extracted/ragrep\"", content)
        self.assertIn('--asset-cache "$XDG_CACHE_HOME"', content)

    def test_ordinary_ci_runs_staged_smoke_tests(self):
        workflow = Path(__file__).parents[2] / ".github" / "workflows" / "ci.yml"
        content = workflow.read_text(encoding="utf-8")
        self.assertIn("tools/ci/test_staged_smoke.py", content)

    def test_asset_copy_excludes_daemon_discovery(self):
        with tempfile.TemporaryDirectory() as temp:
            source = Path(temp) / "source" / "ragrep"
            destination = Path(temp) / "isolated"
            source.mkdir(parents=True)
            (source / "model.onnx").write_bytes(b"model")
            (source / "ort-1.24.4").mkdir()
            (source / "ort-1.24.4" / "onnxruntime.dll").write_bytes(b"runtime")
            (source / "daemon.json").write_text("secret", encoding="utf-8")
            (source / "user.json").write_text("secret", encoding="utf-8")

            copy_asset_cache(source.parent, destination)

            self.assertEqual((destination / "ragrep" / "model.onnx").read_bytes(), b"model")
            self.assertEqual((destination / "ragrep" / "ort-1.24.4" / "onnxruntime.dll").read_bytes(), b"runtime")
            self.assertFalse((destination / "ragrep" / "daemon.json").exists())
            self.assertFalse((destination / "ragrep" / "user.json").exists())

    def test_system_path_does_not_include_toolchain(self):
        path = system_path({"SystemRoot": r"C:\Windows", "PATH": r"C:\Go\bin"})
        self.assertNotIn("Go", path)
        if os.name == "nt":
            self.assertEqual(path, r"C:\Windows\System32")
        else:
            self.assertEqual(path, "")

    def test_hung_mcp_is_killed_with_bounded_exchange(self):
        process = HungProcess()
        with self.assertRaises(RuntimeError) as raised:
            mcp_exchange(process, timeout=0.01)
        self.assertIn("timed out", str(raised.exception))
        self.assertTrue(process.killed)

    def test_mcp_error_response_is_rejected(self):
        with self.assertRaises(RuntimeError) as raised:
            mcp_exchange(ErrorProcess(), timeout=1)
        self.assertIn("protocol error", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
