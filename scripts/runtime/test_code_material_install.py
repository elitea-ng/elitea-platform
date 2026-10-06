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
ENABLED = {"ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED": "true",
           "ELITEA_RUNTIME_CODE_PLATFORM_ENABLED": "true",
           "ELITEA_RUNTIME_CODE_DEBUG_ARTIFACTS_ENABLED": "true"}


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

    def test_default_off_requires_no_code_material(self):
        def remove(source):
            for name in CODE_FILES:
                (source / name).unlink()
        root, result = self.run_install(change=remove)
        self.assertEqual(result.returncode, 0, result.stderr)
        for name in CODE_FILES + ("agent-checkpoint-connection",):
            self.assertFalse((root / "dst/main" / name).exists())

    def test_enabled_material_is_scoped_to_main_with_private_modes(self):
        root, result = self.run_install(ENABLED)
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

    def test_invalid_flags_or_missing_owner_fail_before_any_copy(self):
        for flags in ({"ELITEA_RUNTIME_CODE_PLATFORM_ENABLED": "true"},
                      {"ELITEA_RUNTIME_CODE_DEBUG_ARTIFACTS_ENABLED": "true"},
                      {"ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED": "yes"}):
            with self.subTest(flags=flags):
                root, result = self.run_install(flags)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((root / "dst").exists())

    def test_missing_symlink_empty_and_oversized_code_files_fail_before_copy(self):
        for name, maximum in ((CODE_FILES[0], 1048576), (CODE_FILES[1], 1048576),
                              (CODE_FILES[2], 4096)):
            for kind in ("missing", "symlink", "empty", "oversized"):
                with self.subTest(name=name, kind=kind):
                    def change(source):
                        path = source / name
                        path.unlink()
                        if kind == "symlink":
                            path.symlink_to(source / "runtime-ca.crt")
                        elif kind != "missing":
                            path.write_bytes(b"x" * (maximum + 1) if kind == "oversized" else b"")
                    root, result = self.run_install(ENABLED, change)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse((root / "dst").exists())


if __name__ == "__main__":
    unittest.main()
