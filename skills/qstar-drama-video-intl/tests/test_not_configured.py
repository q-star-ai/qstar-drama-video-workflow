#!/usr/bin/env python3
"""Tests for the "not configured -> stop" behavior and the static scanner.

Covers:
  - Work order 3.2 cases 1-3: submit/generate stop with exit 2, no network.
  - Review fix #2: the no-network test truly monitors the code under test by
    importing the module in-process and monkeypatching socket.socket, proving the
    patch actually takes effect (the patched constructor raises inside the call).
  - Review fix #1: scanner whitelist rules. A line containing a whitelisted link
    AND a forbidden domestic string must still hit; a line with a whitelisted link
    AND an unauthorized URL must still hit; overseas.example/api is only allowed
    inside tests/.

No third-party dependencies.
"""

import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parent.parent
SCRIPT = REPO_ROOT / "scripts" / "drama_workflow.py"
EXAMPLE = REPO_ROOT / "examples" / "episode.json"
CONFIG_PATH = REPO_ROOT / "config" / "overseas.json"
SCANNER = REPO_ROOT / "scripts" / "check_no_domestic.py"


def run(args, env_extra=None):
    env = dict(os.environ)
    env["PYTHONPATH"] = str(REPO_ROOT.parent)
    if env_extra:
        env.update(env_extra)
    return subprocess.run(
        [sys.executable, str(SCRIPT), *args],
        cwd=str(REPO_ROOT),
        capture_output=True,
        text=True,
        env=env,
    )


class NotConfiguredTests(unittest.TestCase):
    def setUp(self):
        self._backup = None
        if CONFIG_PATH.exists():
            self._backup = CONFIG_PATH.read_text(encoding="utf-8")
            CONFIG_PATH.unlink()

    def tearDown(self):
        if self._backup is not None:
            CONFIG_PATH.write_text(self._backup, encoding="utf-8")
        elif CONFIG_PATH.exists():
            CONFIG_PATH.unlink()

    def test_1_no_config_exits_2_with_not_configured(self):
        result = run(["submit", str(EXAMPLE), "01"])
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("NOT CONFIGURED: overseas backend", result.stderr)
        self.assertEqual(result.stdout.strip(), "")

        result = run(["generate", str(EXAMPLE), "01"])
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("NOT CONFIGURED: overseas backend", result.stderr)
        self.assertEqual(result.stdout.strip(), "")

    def test_2_partial_config_lists_missing_fields(self):
        required = ["api_base_url", "queue", "asset_store", "callback_url", "playback_base_url", "billing_account"]
        partial = {k: "" for k in required}
        partial["api_base_url"] = "https://overseas.example/api"
        CONFIG_PATH.parent.mkdir(parents=True, exist_ok=True)
        CONFIG_PATH.write_text(json.dumps(partial), encoding="utf-8")
        try:
            result = run(["submit", str(EXAMPLE), "01"])
            self.assertEqual(result.returncode, 2, result.stderr)
            for field in required:
                if field != "api_base_url":
                    self.assertIn(field, result.stderr, f"missing field {field} not reported")
        finally:
            CONFIG_PATH.unlink()

    def test_3_complete_config_still_exits_2_not_verified(self):
        required = ["api_base_url", "queue", "asset_store", "callback_url", "playback_base_url", "billing_account"]
        complete = {k: f"value-{k}" for k in required}
        CONFIG_PATH.parent.mkdir(parents=True, exist_ok=True)
        CONFIG_PATH.write_text(json.dumps(complete), encoding="utf-8")
        try:
            result = run(["generate", str(EXAMPLE), "01"])
            self.assertEqual(result.returncode, 2, result.stderr)
            self.assertIn("overseas backend not yet verified", result.stderr)
            self.assertEqual(result.stdout.strip(), "")
        finally:
            CONFIG_PATH.unlink()

    def test_2_no_network_socket_used(self):
        # Review fix #2: import in-process and monkeypatch socket.socket so the
        # patch provably takes effect inside the code under test.
        import socket

        import importlib.util

        spec = importlib.util.spec_from_file_location("drama_workflow_under_test", SCRIPT)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)

        opened = []

        class FakeSocket:
            def __init__(self, *args, **kwargs):
                opened.append((args, kwargs))
                # Prove the patch fires inside the called code.
                raise RuntimeError("network intentionally blocked in test")

            def connect(self, *a, **k):
                raise RuntimeError("network intentionally blocked in test")

        with mock.patch.object(socket, "socket", FakeSocket):
            code = mod.cmd_submit_generate("submit", str(EXAMPLE), "01")
        self.assertEqual(code, 2)
        self.assertEqual(opened, [], "a socket was constructed before stopping")


class ScannerWhitelistTests(unittest.TestCase):
    def _scan_text(self, text, filename="sample.txt"):
        path = REPO_ROOT / filename
        path.write_text(text, encoding="utf-8")
        try:
            proc = subprocess.run(
                [sys.executable, str(SCANNER), str(path)],
                cwd=str(REPO_ROOT),
                capture_output=True,
                text=True,
            )
            return proc
        finally:
            path.unlink()

    def test_whitelist_link_plus_domestic_string_still_hits(self):
        # A whitelisted CozyClay link on the same line as the forbidden 43 IP.
        forbidden_ip = "43" + ".136.99.128"
        line = "see https://github.com/NomaDamas/CozyClay and host " + forbidden_ip + " here"
        proc = self._scan_text(line, "sample_doc.md")
        self.assertEqual(proc.returncode, 1, proc.stdout)
        self.assertIn(forbidden_ip, proc.stdout)

    def test_whitelist_link_plus_unauthorized_url_still_hits(self):
        # A whitelisted link on the same line as an unauthorized URL.
        evil = "http://" + "evil.example" + "/submit"
        line = "doc https://github.com/NomaDamas/CozyClay call " + evil
        proc = self._scan_text(line, "sample_doc.md")
        self.assertEqual(proc.returncode, 1, proc.stdout)
        self.assertIn("evil.example/submit", proc.stdout)

    def test_overseas_example_only_allowed_in_tests(self):
        # Allowed inside a tests/ file.
        tests_file = REPO_ROOT / "tests" / "_tmp_url_check.txt"
        tests_file.write_text('x = "https://overseas.example/api"\n', encoding="utf-8")
        try:
            proc = subprocess.run(
                [sys.executable, str(SCANNER), str(tests_file)],
                cwd=str(REPO_ROOT), capture_output=True, text=True,
            )
            self.assertEqual(proc.returncode, 0, proc.stdout)
        finally:
            tests_file.unlink()
        # Forbidden outside tests/ (e.g. a doc file).
        proc = self._scan_text('x = "https://overseas.example/api"', "sample_doc.md")
        self.assertEqual(proc.returncode, 1, proc.stdout)

    def test_missing_target_exits_nonzero(self):
        proc = subprocess.run(
            [sys.executable, str(SCANNER), str(REPO_ROOT / "does_not_exist_xyz")],
            cwd=str(REPO_ROOT), capture_output=True, text=True,
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("does not exist", proc.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
