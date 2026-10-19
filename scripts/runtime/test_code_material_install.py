#!/usr/bin/env python3
"""Check material copies with synthetic bytes and recorded ownership operations."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
BASE_FILES = (
    "runtime-ca.crt control-server.crt output-server.crt content-server.crt "
    "command-signing-keyring.json control-server.key output-server.key content-server.key "
    "command-signing-key.pem auth-attempt-key "
    "auth-pat-signing-key auth-form-users.json vault-master-key "
    "agent-worker-client.crt agent-worker-client.key "
    "worker-output-spool-key agent-checkpoint-connection "
    "platform-edge.crt platform-edge.key"
).split()
CODE_FILES = ("code-owner-client.crt", "code-owner-client.key", "code-platform-content-keys.json")
REMOVED_FLAGS = ("ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED", "ELITEA_RUNTIME_CODE_PLATFORM_ENABLED",
                 "ELITEA_RUNTIME_CODE_DEBUG_ARTIFACTS_ENABLED", "ELITEA_RUNTIME_CODE_WORKSPACE_ENABLED")


class InstallerTests(unittest.TestCase):
    def run_install(self, flags=None, change=None):
        directory = tempfile.TemporaryDirectory(prefix="code-material-test-")
        self.addCleanup(directory.cleanup)
        root = Path(directory.name).resolve()
        source = root / "src"
        source.mkdir()
        for name in BASE_FILES + list(CODE_FILES):
            (source / name).write_bytes(b"synthetic-test-material")
        if change:
            change(source)
        binaries = root / "bin"
        binaries.mkdir()
        chown = binaries / "chown"
        chown.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$CHOWN_LOG"\n')
        chown.chmod(0o700)
        script = (ROOT / "deploy/runtime/install-material.sh").read_text()
        script = script.replace("SRC=/src", "SRC=" + str(source))
        script = script.replace("/dst/", str(root / "dst") + "/")
        executable = root / "install.sh"
        executable.write_text(script)
        env = {"PATH": str(binaries) + ":" + os.environ["PATH"],
               "CHOWN_LOG": str(root / "chown.log"), **(flags or {})}
        result = subprocess.run(["/bin/sh", str(executable)], env=env,
                                capture_output=True, text=True, timeout=10, check=False)
        return root, result

    def test_without_code_material_only_the_checkpoint_connection_reaches_main(self):
        def remove(source):
            for name in CODE_FILES:
                (source / name).unlink()
        root, result = self.run_install(change=remove)
        self.assertEqual(result.returncode, 0, result.stderr)
        for name in CODE_FILES:
            self.assertFalse((root / "dst/main" / name).exists())
        self.assertTrue((root / "dst/main/agent-checkpoint-connection").exists())

    def test_code_material_has_no_switch_and_is_scoped_to_main_with_private_modes(self):
        root, result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        main = root / "dst/main"
        log = (root / "chown.log").read_text()
        for name in CODE_FILES + ("agent-checkpoint-connection",):
            path = main / name
            self.assertEqual(path.read_bytes(), b"synthetic-test-material")
            mode = 0o644 if name.endswith(".crt") else 0o600
            self.assertEqual(path.stat().st_mode & 0o777, mode)
            self.assertIn("65532:65532 " + str(path), log)
        for consumer in ("worker", "edge"):
            for name in CODE_FILES:
                self.assertFalse((root / "dst" / consumer / name).exists())

    def test_removed_flags_and_a_half_owner_pair_fail_before_any_copy(self):
        def remove_owner_key(source):
            (source / CODE_FILES[1]).unlink()
        cases = [({flag: value}, None) for flag in REMOVED_FLAGS for value in ("true", "false", "")]
        cases.append(({}, remove_owner_key))
        for flags, change in cases:
            with self.subTest(flags=flags, change=getattr(change, "__name__", None)):
                root, result = self.run_install(flags, change)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((root / "dst").exists())

    def test_symlink_empty_and_oversized_code_files_fail_before_copy(self):
        for name, maximum in ((CODE_FILES[0], 1048576), (CODE_FILES[1], 1048576),
                              (CODE_FILES[2], 4096)):
            for kind in ("symlink", "empty", "oversized"):
                with self.subTest(name=name, kind=kind):
                    def change(source):
                        path = source / name
                        path.unlink()
                        if kind == "symlink":
                            path.symlink_to(source / "runtime-ca.crt")
                        else:
                            path.write_bytes(b"x" * (maximum + 1) if kind == "oversized" else b"")
                    root, result = self.run_install(None, change)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse((root / "dst").exists())

if __name__ == "__main__":
    unittest.main()
