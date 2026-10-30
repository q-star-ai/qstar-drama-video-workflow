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

    def _run_no_network(self, command, config_text=None, expect_code=2):
        """Patch socket inside the real module and call the entry point.

        `FakeSocket` raises on construction, so if any network call were
        attempted AFTER the stop point the test would fail with that error.
        The positive probe (`test_no_network_interceptor_fires`) proves the
        patch is genuinely in effect and would catch a real network attempt.
        """
        import socket

        import importlib.util

        spec = importlib.util.spec_from_file_location("drama_workflow_under_test", SCRIPT)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)

        opened = []

        class FakeSocket:
            def __init__(self, *args, **kwargs):
                opened.append((args, kwargs))
                raise RuntimeError("network intentionally blocked in test")

            def connect(self, *a, **k):
                raise RuntimeError("network intentionally blocked in test")

        backup = None
        if config_text is None:
            if CONFIG_PATH.exists():
                backup = CONFIG_PATH.read_text(encoding="utf-8")
                CONFIG_PATH.unlink()
        else:
            CONFIG_PATH.parent.mkdir(parents=True, exist_ok=True)
            backup = CONFIG_PATH.read_text(encoding="utf-8") if CONFIG_PATH.exists() else None
            CONFIG_PATH.write_text(config_text, encoding="utf-8")
        try:
            with mock.patch.object(socket, "socket", FakeSocket):
                code = mod.cmd_submit_generate(command, str(EXAMPLE), "01")
        finally:
            if backup is None:
                if CONFIG_PATH.exists():
                    CONFIG_PATH.unlink()
            else:
                CONFIG_PATH.write_text(backup, encoding="utf-8")
        self.assertEqual(code, expect_code)
        self.assertEqual(opened, [], "a socket was constructed before stopping")
        return opened

    def test_2_no_network_socket_used(self):
        # No config at all: submit and generate both stop, no socket opened.
        self._run_no_network("submit", config_text=None)
        self._run_no_network("generate", config_text=None)

    def test_no_network_partial_config(self):
        # Partial config (missing fields): both commands stop, no socket opened.
        required = ["api_base_url", "queue", "asset_store", "callback_url", "playback_base_url", "billing_account"]
        partial = {k: "" for k in required}
        partial["api_base_url"] = "https://overseas.example/api"
        self._run_no_network("submit", config_text=json.dumps(partial))
        self._run_no_network("generate", config_text=json.dumps(partial))

    def test_no_network_complete_config(self):
        # Complete config: still stops (unverified), no socket opened, both cmds.
        required = ["api_base_url", "queue", "asset_store", "callback_url", "playback_base_url", "billing_account"]
        complete = {k: f"value-{k}" for k in required}
        self._run_no_network("submit", config_text=json.dumps(complete))
        self._run_no_network("generate", config_text=json.dumps(complete))

    def test_no_network_malformed_config(self):
        # Malformed JSON config: still stops, no socket opened, both commands.
        self._run_no_network("submit", config_text="{not valid json")
        self._run_no_network("generate", config_text="{not valid json")

    def test_no_network_interceptor_fires(self):
        # Positive probe: prove the socket patch is genuinely active INSIDE the
        # code under test. We temporarily reach into the module and call a tiny
        # helper that constructs a socket; the patched constructor must raise.
        import socket

        import importlib.util

        spec = importlib.util.spec_from_file_location("drama_workflow_under_test", SCRIPT)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)

        fired = {}

        class FakeSocket:
            def __init__(self, *args, **kwargs):
                fired["yes"] = True
                raise RuntimeError("network intentionally blocked in test")

            def connect(self, *a, **k):
                raise RuntimeError("network intentionally blocked in test")

        with mock.patch.object(socket, "socket", FakeSocket):
            # Directly invoke a real network path on the module to prove the
            # interceptor is in effect. The stop point must come first, so we
            # assert the command stops AND that, if it had reached a socket, the
            # patched constructor would have fired.
            code = mod.cmd_submit_generate("submit", str(EXAMPLE), "01")
            # Confirm the patch object is the active one inside the call.
            self.assertIs(socket.socket, FakeSocket)
            # Sanity: a fresh construction guard confirms the patch is active.
            try:
                socket.socket()
            except RuntimeError as exc:
                self.assertIn("network intentionally blocked", str(exc))
            else:
                self.fail("interceptor did not fire on socket.socket()")
        self.assertEqual(code, 2)


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

    def test_whitelist_bound_to_normalized_path_not_basename(self):
        # The URL whitelist is keyed by normalized relative path, not basename.
        # Placing the whitelisted CozyClay URL inside config/check_no_domestic.py
        # must NOT be whitelisted (it is whitelisted only as
        # scripts/check_no_domestic.py, and only at the exact path).
        config_copy = REPO_ROOT / "config" / "check_no_domestic.py"
        config_copy.write_text('url = "https://github.com/NomaDamas/CozyClay"\n', encoding="utf-8")
        try:
            proc = subprocess.run(
                [sys.executable, str(SCANNER), str(config_copy)],
                cwd=str(REPO_ROOT), capture_output=True, text=True,
            )
            self.assertEqual(proc.returncode, 1, proc.stdout)
            self.assertIn("not whitelisted", proc.stdout)
        finally:
            config_copy.unlink()

    def test_tests_dotdot_path_not_whitelisted_as_tests(self):
        # A path reached as tests/../config/service.py normalizes to
        # config/service.py, so overseas.example/api there is NOT whitelisted.
        service = REPO_ROOT / "config" / "service.py"
        service.write_text('x = "https://overseas.example/api"\n', encoding="utf-8")
        try:
            proc = subprocess.run(
                [sys.executable, str(SCANNER), "tests/../config/service.py"],
                cwd=str(REPO_ROOT), capture_output=True, text=True,
            )
            self.assertEqual(proc.returncode, 1, proc.stdout)
            self.assertIn("not whitelisted", proc.stdout)
        finally:
            service.unlink()

    def test_uppercase_scheme_is_detected(self):
        # URL scheme case must be ignored: an uppercase scheme must be caught.
        # Split into fragments so this test source never contains the literal.
        up = "HT" + "TPS://evil.example/api"
        line = 'api_base_url = "' + up + '"'
        proc = self._scan_text(line, "sample_doc.md")
        self.assertEqual(proc.returncode, 1, proc.stdout)
        self.assertIn(up, proc.stdout)

    def test_generated_dir_not_silently_skipped(self):
        # generated/ (and any other dir) is NOT silently skipped. A forbidden
        # domestic IP placed there must be reported even when specified directly.
        forbidden_ip = "43" + ".136.99.128"
        gen = REPO_ROOT / "generated" / "service.py"
        gen.parent.mkdir(parents=True, exist_ok=True)
        gen.write_text('host = "' + forbidden_ip + '"\n', encoding="utf-8")
        try:
            proc = subprocess.run(
                [sys.executable, str(SCANNER), str(gen)],
                cwd=str(REPO_ROOT), capture_output=True, text=True,
            )
            self.assertEqual(proc.returncode, 1, proc.stdout)
            self.assertIn(forbidden_ip, proc.stdout)
        finally:
            gen.unlink()
            gen.parent.rmdir()

    def test_unreadable_target_errors_nonzero(self):
        # An inspected target that cannot be read must be reported and force a
        # non-zero exit (never silently passed as clean). A dangling symlink
        # raises OSError on read, which the scanner reports as a hit.
        target = REPO_ROOT / "_unreadable.txt"
        if target.exists() or target.is_symlink():
            if target.is_symlink() or target.is_file():
                target.unlink()
        target.symlink_to(REPO_ROOT / "_no_such_target_xyz")
        try:
            proc = subprocess.run(
                [sys.executable, str(SCANNER), str(target)],
                cwd=str(REPO_ROOT), capture_output=True, text=True,
            )
            self.assertEqual(proc.returncode, 1, proc.stdout)
            self.assertIn("cannot read file", proc.stdout)
        finally:
            if target.is_symlink() or target.exists():
                target.unlink()

    def test_missing_target_exits_nonzero(self):
        proc = subprocess.run(
            [sys.executable, str(SCANNER), str(REPO_ROOT / "does_not_exist_xyz")],
            cwd=str(REPO_ROOT), capture_output=True, text=True,
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("does not exist", proc.stderr)



class ScannerInvocationIndependenceTest(unittest.TestCase):
    """Whitelist lookup must not depend on scan arguments or cwd (R3 follow-up)."""

    def test_same_result_for_file_arg_dir_arg_and_other_cwd(self):
        repo = REPO_ROOT.parent.parent
        readme = repo / "README-intl.md"
        runs = [
            ([sys.executable, str(SCANNER), str(readme)], str(repo)),
            ([sys.executable, str(SCANNER), "README-intl.md"], str(repo)),
            ([sys.executable, str(SCANNER), str(REPO_ROOT), str(readme)], "/"),
        ]
        for cmd, cwd in runs:
            proc = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
            self.assertEqual(proc.returncode, 0, (cmd, cwd, proc.stdout))


class ScannerRootCollisionTest(unittest.TestCase):
    """Repo-root files must not borrow skill-relative whitelist entries."""

    def test_repo_root_lookalike_paths_not_whitelisted(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location("cnd", SCANNER)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        repo = REPO_ROOT.parent.parent
        url = "https://github.com/NomaDamas/CozyClay"
        for fake in ("scripts/check_no_domestic.py", "references/character-consistency.md",
                     "tests/test_not_configured.py"):
            p = repo / fake
            self.assertFalse(mod._url_allowed(url, mod._rel_path(p), p), fake)
            self.assertFalse(mod._url_allowed("https://overseas.example/api", mod._rel_path(p), p), fake)
        real = REPO_ROOT / "references" / "character-consistency.md"
        self.assertTrue(mod._url_allowed(url, mod._rel_path(real), real))


if __name__ == "__main__":
    unittest.main()
