#!/usr/bin/env python3
"""Test public receipt trust boundaries without secrets, containers, or services."""
import base64
import copy
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import code_platform_public_trust as trust

# RFC 8032 public test key. This fixture contains no private signing material.
PUBLIC = bytes.fromhex("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
IMAGE = "example.invalid/elitea/code-runner@sha256:" + "a" * 64
ROOT = Path(__file__).resolve().parents[2]


def ring(count=1):
    return {"schema_version": trust.SCHEMA, "keys": [
        {"key_id": f"main-{index}", "public_key_base64": base64.b64encode(
            PUBLIC if index == 0 else bytes([index]) * 32).decode("ascii")}
        for index in range(count)]}


def asset():
    return {"revision": 1, "keys": [{"key_id": "main-0", "public_key_hex": PUBLIC.hex()}]}


class PublicTrustTests(unittest.TestCase):
    def rejected(self, code, operation, *args):
        with self.assertRaisesRegex(trust.Rejected, "^" + code.replace(".", r"\.") + "$"):
            operation(*args)

    def test_public_base64_maps_to_exact_runner_hex_and_identity(self):
        converted = trust.convert(trust.encoded(ring()), required_key_id="main-0")
        self.assertEqual(converted, trust.encoded(asset()))
        self.assertEqual(trust.validate(converted), converted)
        entry = json.loads(converted)["keys"][0]
        self.assertEqual(bytes.fromhex(entry["public_key_hex"]), PUBLIC)
        self.assertEqual(entry["key_id"], ring()["keys"][0]["key_id"])
        self.assertLessEqual(len(converted), 8192)

    def test_one_through_eight_keys_are_deterministic(self):
        for count in (1, 8):
            source = ring(count)
            expected = trust.convert(trust.encoded(source))
            source["keys"].reverse()
            self.assertEqual(trust.convert(trust.encoded(source)), expected)
            self.assertEqual(trust.validate(expected), expected)

    def test_rotation_selection_requires_explicit_present_unique_ids(self):
        source = trust.encoded(ring(64))
        self.rejected("key.count", trust.convert, source)
        selected = trust.convert(source, ["main-63", "main-0"], "main-0")
        self.assertEqual([key["key_id"] for key in json.loads(selected)["keys"]], ["main-0", "main-63"])
        for ids, code in (([], "key.count"), (["main-0"] * 2, "key.duplicate_id"),
                          (["missing"], "key.selection"), ([f"main-{index}" for index in range(9)], "key.count")):
            self.rejected(code, trust.convert, source, ids)
        self.rejected("key.required", trust.convert, source, ["main-0"], "main-1")

    def test_main_key_count_is_bounded_even_with_selection(self):
        for count in (0, 65):
            self.rejected("key.count", trust.convert, trust.encoded(ring(count)), ["main-0"])
        for count in (0, 9):
            value = asset()
            value["keys"] *= count
            self.rejected("key.count", trust.validate, trust.encoded(value))

    def test_duplicate_ids_and_public_material_are_rejected_in_both_formats(self):
        for field, code in (("key_id", "key.duplicate_id"), ("public", "key.duplicate_material")):
            source = ring(2)
            second = source["keys"][1]
            second["key_id" if field == "key_id" else "public_key_base64"] = source["keys"][0][
                "key_id" if field == "key_id" else "public_key_base64"]
            self.rejected(code, trust.convert, trust.encoded(source))
            target = asset()
            target["keys"].append({"key_id": "main-1", "public_key_hex": (bytes([1]) * 32).hex()})
            target["keys"][1]["key_id" if field == "key_id" else "public_key_hex"] = target["keys"][0][
                "key_id" if field == "key_id" else "public_key_hex"]
            self.rejected(code, trust.validate, trust.encoded(target))

    def test_duplicate_json_fields_are_rejected_at_each_level(self):
        for operation, valid in ((trust.convert, ring()), (trust.validate, asset())):
            raw = trust.encoded(valid)
            first_field = "schema_version" if operation is trust.convert else "revision"
            duplicate = b'"' + first_field.encode() + b'":null,'
            self.rejected("json.duplicate", operation, b"{" + duplicate + raw[1:])
            self.rejected("json.duplicate", operation, raw.replace(b'"key_id":', b'"key_id":"other","key_id":'))
            public_field = b'"public_key_base64":' if operation is trust.convert else b'"public_key_hex":'
            self.rejected("json.duplicate", operation, raw.replace(public_field, public_field + b'null,' + public_field))

    def test_private_and_unknown_fields_never_reach_output(self):
        for operation, original in ((trust.convert, ring()), (trust.validate, asset())):
            for field in ("private_key", "private_key_pem", "private_key_base64", "seed", "key_base64url", "unknown"):
                for nested in (False, True):
                    value = copy.deepcopy(original)
                    (value["keys"][0] if nested else value)[field] = "private-input-sentinel"
                    self.rejected("json.fields", operation, trust.encoded(value))
        private_shape = {"revision": 1, "current_key_id": "main-0", "keys": [{"id": "main-0", "key_base64url": "private-input-sentinel"}]}
        for operation in (trust.convert, trust.validate):
            self.rejected("json.fields", operation, trust.encoded(private_shape))

    def test_schema_revision_and_field_types_are_exact(self):
        for schema in (None, 1, trust.SCHEMA + ".future"):
            source = ring()
            source["schema_version"] = schema
            self.rejected("keyring.schema", trust.convert, trust.encoded(source))
        for revision in (True, 1.0, "1", None, 0, 2):
            target = asset()
            target["revision"] = revision
            self.rejected("trust.revision", trust.validate, trust.encoded(target))
        for keys in ({}, None, "keys"):
            for operation, value in ((trust.convert, ring()), (trust.validate, asset())):
                value["keys"] = keys
                self.rejected("key.count", operation, trust.encoded(value))
        for operation in (trust.convert, trust.validate):
            for raw in (b"null", b"[]", b"1"):
                self.rejected("json.fields", operation, raw)

    def test_key_ids_match_runner_printable_ascii_bounds(self):
        for identity in ("", "x" * 257, "a b", "a\tb", "a\nb", "a\rb", "a\0b", "a\x7fb", "é", 1, None):
            for operation, value in ((trust.convert, ring()), (trust.validate, asset())):
                value["keys"][0]["key_id"] = identity
                self.rejected("key.id", operation, trust.encoded(value))
        value = ring()
        value["keys"][0]["key_id"] = "!" + "x" * 254 + "~"
        self.assertEqual(len(json.loads(trust.convert(trust.encoded(value)))["keys"][0]["key_id"]), 256)
        value["keys"][0]["key_id"] = 'main-"\\[{]}'
        self.assertEqual(json.loads(trust.convert(trust.encoded(value)))["keys"][0]["key_id"], value["keys"][0]["key_id"])

    def test_required_active_key_id_cannot_be_missing_blank_or_incompatible(self):
        raw = trust.encoded(asset())
        self.rejected("key.required", trust.validate, raw, "missing")
        for identity in ("", "main 0", "é"):
            self.rejected("key.id", trust.validate, raw, identity)

    def test_base64_is_strict_canonical_and_exactly_32_bytes(self):
        canonical = ring()["keys"][0]["public_key_base64"]
        for value in (None, 4, "", canonical[:-1], canonical + "=", canonical[:3] + "\n" + canonical[4:],
                      "-" * 43 + "=", "é" * 44, base64.b64encode(b"x" * 31).decode(),
                      base64.b64encode(b"x" * 33).decode(), canonical[:-2] + "p=", canonical[:-1] + "A"):
            source = ring()
            source["keys"][0]["public_key_base64"] = value
            self.rejected("key.base64", trust.convert, trust.encoded(source))

    def test_hex_is_lowercase_and_exactly_32_bytes(self):
        for value in (None, 4, "", PUBLIC.hex().upper(), "g" * 64, "a" * 63, "a" * 65, "é" * 64):
            target = asset()
            target["keys"][0]["public_key_hex"] = value
            self.rejected("key.hex", trust.validate, trust.encoded(target))
        source = ring()
        source["keys"][0]["public_key_base64"] = base64.b64encode(bytes(32)).decode()
        self.rejected("key.zero", trust.convert, trust.encoded(source))
        target = asset()
        target["keys"][0]["public_key_hex"] = "0" * 64
        self.rejected("key.zero", trust.validate, trust.encoded(target))

    def test_json_is_bounded_and_rejects_trailing_nonfinite_and_nested_input(self):
        for operation, bound, raw in ((trust.convert, trust.KEYRING_LIMIT, trust.encoded(ring())),
                                      (trust.validate, trust.TRUST_LIMIT, trust.encoded(asset()))):
            for value in (b"", b" " * (bound + 1)):
                self.rejected("json.bound", operation, value)
            for value in (raw + b"{}", b"\xff", b"\xef\xbb\xbf" + raw):
                self.rejected("json.invalid", operation, value)
            self.rejected("json.depth", operation, b"[" * 9 + b"]" * 9)
            for value in (b"NaN", b"Infinity", b"-Infinity"):
                self.rejected("json.number", operation, value)
            self.assertEqual(operation(raw + b"\n "), operation(raw))
            self.assertEqual(operation(raw + b" " * (bound - len(raw))), operation(raw))

    def test_image_reference_requires_repository_digest(self):
        for image in (IMAGE, "runner@sha256:" + "b" * 64, "localhost:5000/code/runner@sha256:" + "c" * 64,
                      "elitea-code-rust@sha256:355653a4eaced3bdd74a57eb5df9a4655a1e1117158dcdb2168cc5a6a574ecfd",
                      "elitea-code-deno@sha256:b16da2d30ffb417e7b362337321ba6f587d049f6b37587aff8a83ebeeb04c6aa"):
            trust.validate_runner_image(image)
        for image in (None, "", "runner:latest", "runner:1", "sha256:" + "a" * 64,
                      IMAGE.upper(), IMAGE + "\n", IMAGE + " --privileged", IMAGE[:-1], "https://" + IMAGE,
                      "../" + IMAGE, "runner:latest@sha256:" + "a" * 64):
            self.rejected("image.digest", trust.validate_runner_image, image)

    def test_public_reader_accepts_safe_regular_files_and_applies_byte_bound(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder).resolve() / "public.json"
            path.write_bytes(b"public")
            for mode in (0o400, 0o600, 0o644, 0o444):
                path.chmod(mode)
                self.assertEqual(trust.public_file(path, 6), b"public")
            self.rejected("file.regular", trust.public_file, path, 5)
            path.chmod(0o600)
            path.write_bytes(b"")
            self.rejected("file.regular", trust.public_file, path, 6)

    def test_public_reader_rejects_links_devices_directories_and_unsafe_modes(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            path = root / "public.json"
            path.write_bytes(b"public")
            link = root / "symlink"
            link.symlink_to(path)
            self.rejected("file.path", trust.public_file, link, 16)
            alias = root / "hardlink"
            os.link(path, alias)
            self.rejected("file.regular", trust.public_file, path, 16)
            alias.unlink()
            for mode in (0o666, 0o640 | 0o010, 0o644 | 0o1000, 0o044):
                path.chmod(mode)
                self.rejected("file.regular", trust.public_file, path, 16)
            special = mock.Mock(st_mode=stat.S_IFREG | 0o4644, st_nlink=1, st_size=6)
            with mock.patch.object(trust.Path, "lstat", return_value=special):
                self.rejected("file.regular", trust.public_file, path, 16)
            self.rejected("file.regular", trust.public_file, root, 16)
            self.rejected("file.regular", trust.public_file, Path("/dev/null"), 16)
            path.chmod(0o644)
            self.rejected("file.path", trust.public_file, str(root) + "/./public.json", 16)
            self.rejected("file.path", trust.public_file, "relative.json", 16)
            parent_link = root / "alias"
            parent_link.symlink_to(root, target_is_directory=True)
            self.rejected("file.path", trust.public_file, parent_link / path.name, 16)

    def test_fifo_input_is_rejected_without_waiting(self):
        with tempfile.TemporaryDirectory() as folder:
            fifo = Path(folder).resolve() / "fifo"
            os.mkfifo(fifo, 0o600)
            result = subprocess.run([sys.executable, trust.__file__, "validate", "--input", str(fifo)],
                                    capture_output=True, timeout=3)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(json.loads(result.stdout)["code"], "file.regular")

    def test_public_reader_detects_changed_opened_file_identity(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder).resolve() / "public.json"
            path.write_bytes(b"public")
            before = path.stat()
            changed = mock.Mock(**{name: getattr(before, name) for name in
                                   ("st_dev", "st_ino", "st_mode", "st_nlink", "st_size", "st_mtime_ns", "st_ctime_ns")})
            changed.st_ino += 1
            for metadata in ([changed], [before, changed]):
                with mock.patch.object(trust.os, "fstat", side_effect=metadata):
                    self.rejected("file.changed", trust.public_file, path, 16)

    def test_publish_is_exclusive_readonly_and_rejects_path_aliases(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            path = root / "main-receipt-keys.json"
            raw = trust.encoded(asset())
            trust.publish(path, raw)
            self.assertEqual(path.read_bytes(), raw)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o444)
            with self.assertRaises(FileExistsError):
                trust.publish(path, b"replacement")
            self.assertEqual(path.read_bytes(), raw)
            alias = root / "alias"
            alias.symlink_to(root, target_is_directory=True)
            self.rejected("file.path", trust.publish, alias / "new.json", raw)
            linked = root / "linked.json"
            linked.symlink_to(path)
            with self.assertRaises(FileExistsError):
                trust.publish(linked, raw)
            self.assertEqual(path.read_bytes(), raw)

    def test_cli_converts_validates_and_emits_only_fixed_status_and_digest(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            context = root / "public-only"
            context.mkdir()
            source, output = root / "command-signing-keyring.json", context / "main-receipt-keys.json"
            source.write_bytes(trust.encoded(ring(2)))
            result = subprocess.run([sys.executable, trust.__file__, "convert", "--input", str(source),
                                     "--key-id", "main-0", "--require-key-id", "main-0", "--output", str(output)],
                                    capture_output=True, check=True)
            self.assertEqual(result.stderr, b"")
            self.assertEqual(output.read_bytes(), trust.encoded(asset()))
            self.assertEqual(list(context.iterdir()), [output])
            self.assertEqual(json.loads(result.stdout), {"status": "written", "sha256": hashlib.sha256(output.read_bytes()).hexdigest(),
                                                        "bytes": output.stat().st_size, "trust_path": trust.TRUST_PATH})
            check = subprocess.run([sys.executable, trust.__file__, "validate", "--input", str(output),
                                    "--runner-image", IMAGE, "--require-key-id", "main-0"], capture_output=True, check=True)
            self.assertEqual(check.stderr, b"")
            self.assertEqual(json.loads(check.stdout)["status"], "validated")

    def test_cli_rejects_private_unknown_and_mutable_input_without_publication_or_echo(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            source, output = root / "input.json", root / "output.json"
            private = ring()
            private["keys"][0]["private_key"] = "private-input-sentinel"
            source.write_bytes(trust.encoded(private))
            for mode, extra in (("convert", []), ("validate", ["--runner-image", "runner:latest"])):
                result = subprocess.run([sys.executable, trust.__file__, mode, "--input", str(source),
                                         "--output", str(output), *extra], capture_output=True)
                self.assertEqual(result.returncode, 1)
                self.assertEqual(json.loads(result.stdout)["status"], "rejected")
                self.assertNotIn(b"private-input-sentinel", result.stdout + result.stderr)
                self.assertFalse(output.exists())

    def test_source_contract_matches_fixed_receipt_signer_and_trust_fields(self):
        launch = (ROOT / "services/elitea-code-runner/src/code_platform_launch.rs").read_text()
        self.assertIn(f'const TRUST_PATH: &str = "{trust.TRUST_PATH}";', launch)
        self.assertIn("read_fixed(TRUST_PATH, 8192)", launch)
        self.assertIn("key_file.revision != 1", launch)
        self.assertIn("key_file.keys.len() > 8", launch)
        self.assertIn("public_key_hex: String", launch)
        keyring = (ROOT / "services/elitea-main/internal/runtimecomposition/verification_keyring.go").read_text()
        self.assertIn(trust.SCHEMA, keyring)
        self.assertIn('json:"public_key_base64"', keyring)
        composition = (ROOT / "services/elitea-main/internal/runtimecomposition/composition.go").read_text()
        self.assertIn("verificationKeys.requireActiveSigningKey(config.SigningKeyID, privateKey)", composition)
        self.assertIn("control.NewSandboxGrantIssuer(config.SigningKeyID, privateKey", composition)
        signer = (ROOT / "services/elitea-main/internal/transport/runtimegrpc/control/code_platform_signature.go").read_text()
        self.assertIn("ed25519.Sign(issuer.key,input)", signer.replace(" ", ""))
        main_domain = (ROOT / "services/elitea-main/internal/domain/codeplatform/signature.go").read_text()
        runner = (ROOT / "services/elitea-code-runner/src/code_platform_signature.rs").read_text()
        self.assertIn("elitea.code.platform-committed-reply.ed25519.v1", main_domain)
        self.assertIn("elitea.code.platform-committed-reply.ed25519.v1", runner)

    def test_wrapper_copies_only_validated_public_asset_and_preserves_nonroot_command(self):
        wrapper = (ROOT / "services/elitea-code-runner/Containerfile.code-platform").read_text()
        self.assertIn("FROM ${CODE_RUNNER_IMAGE} AS code-platform", wrapper)
        self.assertIn('--runner-image "${CODE_RUNNER_IMAGE}"', wrapper)
        self.assertIn('--require-key-id "${CODE_PLATFORM_REQUIRED_KEY_ID}"', wrapper)
        self.assertIn("COPY --from=code-platform-trust /main-receipt-keys.json", wrapper)
        self.assertIn("mkdir -m 0755 -p /out/elitea-code-trust", wrapper)
        self.assertIn("--output /out/elitea-code-trust/main-receipt-keys.json", wrapper)
        self.assertIn("COPY --from=validate-trust --chown=0:0", wrapper)
        self.assertIn("/out/elitea-code-trust /opt/elitea-code-trust", wrapper)
        runtime = wrapper.split("FROM ${CODE_RUNNER_IMAGE} AS code-platform", 1)[1]
        self.assertEqual(runtime.strip().splitlines()[-1], "USER 10001:10001")
        for instruction in ("RUN ", "CMD ", "ENTRYPOINT ", "USER root", "ADD ", "--chmod"):
            self.assertNotIn(instruction, runtime)
        self.assertNotIn("code-platform-content-keys", wrapper)
        self.assertNotIn("command-signing-key.pem", wrapper)

    def test_validated_directory_tree_retains_traversal_and_readonly_public_file(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            source = root / "out" / "elitea-code-trust"
            source.mkdir(parents=True, mode=0o755)
            source.chmod(0o755)
            public = source / "main-receipt-keys.json"
            trust.publish(public, trust.validate(trust.encoded(asset())))
            destination = root / "opt" / "elitea-code-trust"
            shutil.copytree(source, destination)
            for directory in (source, destination):
                self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o755)
                self.assertEqual([path.name for path in directory.iterdir()], ["main-receipt-keys.json"])
                copied = directory / public.name
                self.assertEqual(stat.S_IMODE(copied.stat().st_mode), 0o444)
                self.assertEqual(copied.read_bytes(), trust.encoded(asset()))
                self.assertTrue(directory.stat().st_mode & stat.S_IXOTH)
                self.assertTrue(copied.stat().st_mode & stat.S_IROTH)


if __name__ == "__main__":
    unittest.main()
