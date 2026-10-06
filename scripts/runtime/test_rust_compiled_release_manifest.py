#!/usr/bin/env python3
"""Test public audit boundaries. These fixtures are not release evidence."""
import copy
import gzip
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

import rust_compiled_release_manifest as release

IMAGE = "sha256:"+release.sha(b"unit-only image")
CONTAINER = release.sha(b"unit-only container")
POLICY = "cargo-execute-v1"


def tar_bytes(files, directories=False, extra=None):
    target = io.BytesIO()
    with tarfile.open(fileobj=target, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        if directories:
            for path in (".", ".cargo", "src", "vendor", "vendor/demo", "vendor/demo/src"):
                item = tarfile.TarInfo(path)
                item.type, item.mode = tarfile.DIRTYPE, 0o700
                archive.addfile(item)
        for path, data in sorted(files.items()):
            item = tarfile.TarInfo(("./" if directories else "")+path)
            item.size, item.mode = len(data), 0o644
            archive.addfile(item, io.BytesIO(data))
        if extra is not None:
            archive.addfile(extra)
    return target.getvalue()


def policy():
    return {"image":IMAGE, "running":True, "user":"10001:10001", "entrypoint":["/bin/sh"], "cmd":["-c", "exec sleep 600"], "readonly":True, "network":"none", "privileged":False, "cap_add":None, "cap_drop":["ALL"], "security":["no-new-privileges"], "memory":release.MIB*1024, "swap":release.MIB*1024, "cpus":1_000_000_000, "pids":64, "pid_mode":"", "devices":0, "mounts":[{}], "tmpfs":{"/workspace":"rw,nosuid,nodev,size=512m,uid=10001,gid=10001,mode=0700"}, "compiled":[0]}


class FakeReadback:
    """Supply in-memory facts. Never call Docker or execute a file."""
    def __init__(self, digest=IMAGE):
        self.container = CONTAINER
        self.image = {"id":IMAGE, "os":"linux", "architecture":"arm64", "user":"10001:10001", "repo_digests":[digest, ""]}
        self.policy = policy()
        self.files = {".cargo/config.toml":b"[source.crates-io]\nreplace-with='vendored-sources'\n[source.vendored-sources]\ndirectory='vendor'\n", "Cargo.lock":b"version=4\n", "Cargo.toml":b"[package]\nname='elitea-code-job'\nversion='0.1.0'\n", "src/main.rs":b"fn main() {}\n", "src/user.rs":b"// unit-only placeholder\n", "vendor/demo/Cargo.toml":b"[package]\nname='demo'\nversion='1.0.0'\n", "vendor/demo/src/lib.rs":b"pub fn example() {}\n"}
        self.records = [{"path":path, "bytes":len(data), "sha256":release.sha(data), "mode":0o644} for path, data in sorted(self.files.items())]
        raw = tar_bytes(self.files)
        archive = gzip.compress(raw, mtime=0)
        hashes = {item["path"]:item["sha256"] for item in self.records}
        record = {"revision":1, "format":"elitea.rust-native-preparation.v1", "declaration_sha256":release.sha(b"[dependencies]\ndemo='1'\n"), "manifest_sha256":hashes["Cargo.toml"], "lock_sha256":hashes["Cargo.lock"], "vendor_config_sha256":hashes[".cargo/config.toml"], "wrapper_sha256":hashes["src/main.rs"], "content":{"sha256":release.sha(archive), "compressed_bytes":len(archive), "raw_bytes":len(raw), "files":self.records}}
        record_raw = release.encoded(record)
        objects = [{"role":role, "name":release.sha(value)+".blob", "bytes":len(value), "sha256":release.sha(value)} for role, value in (("record", record_raw), ("archive", archive))]
        preparation = release.sha(b"unit-only preparation")
        self.bundle = {"revision":2, "kind":"cargo", "language":"rust", "platform":{"os":"linux", "arch":"arm64", "abi":"gnu"}, "preparation_sha256":preparation, "source_sha256":record["declaration_sha256"], "execution_image_digest":digest, "execution_policy_revision":POLICY, "payload":{"revision":1, "preparation_sha256":preparation, "declaration_sha256":record["declaration_sha256"], "profile":{"preparation_image":IMAGE, "execution_image":digest, "rust_revision":"1.97.1", "target":release.TARGETS["arm64"], "os":"linux", "arch":"arm64", "policy_revision":POLICY}, "objects":objects}}
        self.root = release.sha(release.encoded(self.bundle, True))
        self.bundle["digest"] = self.root
        metadata = release.encoded(self.bundle, True)
        self.regulars = {"/workspace/native-bundle/elitea-native-bundle-v2.json":metadata, "/workspace/native-bundle/elitea-native-ready-v2.json":metadata, "/workspace/rust-job/.elitea-cargo-profile.json":metadata, "/opt/elitea-rust/src/main.rs":self.files["src/main.rs"], release.ADAPTER:b"\x7fELFunit-only"}
        self.regulars.update({"/workspace/native-bundle/objects/"+obj["name"]:value for obj, value in zip(objects, (record_raw, archive))})
        inventory = []
        for index, path in enumerate(("", ".cargo", "src", "vendor", "vendor/demo", "vendor/demo/src")):
            inventory.extend((path, "d", "0", "700", "2", "1", str(index+1)))
        for index, (path, data) in enumerate(sorted(self.files.items())):
            inventory.extend((path, "f", str(len(data)), "644", "1", "1", str(index+100)))
        self.inventory = ("\0".join(inventory)+"\0").encode()
        self.tree = tar_bytes(self.files, True)
        self.calls = []

    def command(self, argv, cap=128*1024, timeout=30):
        self.calls.append(argv)
        if argv[:2] == ["image", "inspect"]:
            return release.encoded(self.image)
        if argv[0] == "inspect":
            return release.encoded(self.policy)
        raise AssertionError(argv)

    def regular(self, path, cap):
        self.calls.append(["regular", path])
        data = self.regulars[path]
        assert len(data) <= cap
        return data

    def exec(self, *argv, cap=128*1024, timeout=30):
        self.calls.append(argv)
        if argv[0] == "find":
            return self.inventory
        if argv[0] == "tar":
            return self.tree
        if argv[0] == "env":
            return b"rustc 1.97.1\nrelease: 1.97.1\n" if argv[-2].endswith("rustc") else b"cargo 1.97.1\nrelease: 1.97.1\n"
        raise AssertionError(argv)


def audit(reader=None, digest=IMAGE):
    reader = reader or FakeReadback(digest)
    return release.audit_native(reader, digest, reader.root, POLICY, IMAGE)


class ReleaseTests(unittest.TestCase):
    def rejected(self, code, function, *args):
        with self.assertRaisesRegex(release.Rejected, "^"+code.replace(".", r"\.")+"$"):
            function(*args)

    def test_exact_native_audit_and_main_field_order(self):
        reader = FakeReadback()
        value = audit(reader)
        raw = release.encoded(value)
        manifest = release.produce(raw, release.sha(raw), IMAGE, reader.root, POLICY, IMAGE)
        parsed = json.loads(manifest)
        binding = parsed["profiles"][0]["binding"]
        self.assertEqual(list(binding), ["revision", "reuse_policy", "tenant_id", "project_id", "base_prepared_request_sha256", "source_sha256", "compilation_image_digest", "execution_image_digest", "platform", "target", "policy_revision", *release.MEASURED])
        self.assertEqual(binding["tenant_id"], "release_template")
        self.assertEqual(parsed["profiles"][0]["dependency_bundle_sha256"], reader.root)
        self.assertEqual(value["native_record_sha256"], reader.bundle["payload"]["objects"][0]["sha256"])
        self.assertNotIn(b"\n", manifest)
        self.assertEqual(manifest, release.encoded(parsed))

    def test_registry_manifest_and_inspected_local_id_remain_distinct(self):
        digest = "sha256:"+release.sha(b"unit-only registry manifest")
        reader = FakeReadback(digest)
        value = audit(reader, digest)
        raw = release.encoded(value)
        manifest = json.loads(release.produce(raw, release.sha(raw), digest, reader.root, POLICY, IMAGE))
        self.assertEqual(manifest["profiles"][0]["binding"]["execution_image_digest"], digest)
        reader.image["repo_digests"] = [""]
        self.rejected("image.release_digest", audit, reader, digest)

    def test_mutable_tags_and_unknown_image_are_rejected(self):
        reader = FakeReadback()
        self.rejected("audit.selection", release.audit_native, reader, "runner:latest", reader.root, POLICY, IMAGE)
        reader.image["id"] = "sha256:"+release.sha(b"different")
        self.rejected("image.identity", audit, reader)

    def test_container_policy_refuses_authority_and_overlays(self):
        for key, value in (("network", "bridge"), ("readonly", False), ("user", "0:0"), ("privileged", True), ("cap_add", ["SYS_ADMIN"]), ("cap_drop", []), ("security", ["no-new-privileges", "seccomp=unconfined"]), ("memory", 0), ("swap", -1), ("cpus", 0), ("pids", -1), ("pid_mode", "host"), ("devices", 1), ("cmd", ["-c", "cargo build"]), ("compiled", [1, 0]), ("mounts", [{"type":"bind", "destination":release.ADAPTER}, {}]), ("tmpfs", {"/workspace":"rw,size=0"})):
            with self.subTest(key=key):
                item = policy()
                item[key] = value
                with self.assertRaises(release.Rejected):
                    release.container_policy(item, IMAGE)

    def test_tmpfs_conflicting_flags_regression(self):
        item = policy()
        item["tmpfs"]["/workspace"] += ",uid=0,gid=0,mode=0777,suid,dev"
        self.rejected("container.workspace", release.container_policy, item, IMAGE)

    def test_tmpfs_rejects_every_duplicate_key(self):
        for option in (policy()["tmpfs"]["/workspace"]+",exec").split(","):
            for changed in (option, option.split("=")[0]+"=0"):
                with self.subTest(option=changed):
                    item = policy()
                    item["tmpfs"]["/workspace"] += ",exec,"+changed
                    self.rejected("container.workspace", release.container_policy, item, IMAGE)

    def test_tmpfs_rejects_unknown_flags_and_noncanonical_values(self):
        original = policy()["tmpfs"]["/workspace"]
        for suffix in (",suid", ",dev", ",ro", ",exec=1", ",noexec", ",", ",unknown=1"):
            with self.subTest(suffix=suffix):
                item = policy()
                item["tmpfs"]["/workspace"] = original+suffix
                self.rejected("container.workspace", release.container_policy, item, IMAGE)
        for before, after in (("rw", "rw=1"), ("uid=10001", "uid=010001"), ("gid=10001", "gid=0"), ("mode=0700", "mode=700"), ("mode=0700", "mode=0777"), ("size=512m", "size=0512m"), ("size=512m", "size=512M"), ("size=512m", "size=0m"), ("size=512m", "size=5g"), ("size=512m", "size=512m=1"), ("size=512m", "size"), ("rw,", "")):
            with self.subTest(replacement=after):
                item = policy()
                item["tmpfs"]["/workspace"] = original.replace(before, after)
                with self.assertRaises(release.Rejected):
                    release.container_policy(item, IMAGE)

    def test_tmpfs_accepts_unique_safe_flags_and_bounded_sizes(self):
        for size in ("1k", "512m", "4g"):
            item = policy()
            item["tmpfs"]["/workspace"] = "mode=0700,gid=10001,uid=10001,size="+size+",nodev,nosuid,rw,exec"
            item["tmpfs"]["/tmp"] = "rw,nosuid,nodev,size=512m,uid=10001,gid=10001,mode=0700"
            release.container_policy(item, IMAGE)

    def test_container_id_cannot_be_a_name_or_argument(self):
        for value in ("rehearsal", "--privileged", "A"*64):
            self.rejected("container.id", release.DockerReadback, value)

    def test_native_pin_platform_and_canonical_fields_are_exact(self):
        reader = FakeReadback()
        raw = reader.regulars["/workspace/native-bundle/elitea-native-bundle-v2.json"]
        for changed in (raw+b"\n", b"{}", raw.replace(b'"cargo"', b'"npm"')):
            with self.assertRaises(release.Rejected):
                release.native_metadata(changed, IMAGE, reader.root, POLICY, "arm64")
        self.rejected("native.profile", release.native_metadata, raw, IMAGE, reader.root, POLICY, "amd64")

    def test_record_rejects_unknown_paths_and_missing_root(self):
        reader = FakeReadback()
        payload = reader.bundle["payload"]
        record_path = "/workspace/native-bundle/objects/"+payload["objects"][0]["name"]
        record = json.loads(reader.regulars[record_path])
        for path in ("../outside", "target/program", "src/extra.rs"):
            changed = copy.deepcopy(record)
            changed["content"]["files"][0]["path"] = path
            raw = release.encoded(changed)
            bound = copy.deepcopy(payload)
            bound["objects"][0].update(bytes=len(raw), sha256=release.sha(raw))
            with self.assertRaises(release.Rejected):
                release.preparation_record(raw, bound)

    def test_archive_is_exact_and_contains_no_links_or_extra_members(self):
        reader = FakeReadback()
        original = tar_bytes(reader.files)
        release.verify_tar(original, reader.records)
        linked = tarfile.TarInfo("vendor/link")
        linked.type, linked.linkname = tarfile.SYMTYPE, "/outside"
        with self.assertRaises(release.Rejected):
            release.verify_tar(tar_bytes(reader.files, extra=linked), reader.records)
        changed = dict(reader.files)
        changed["vendor/extra"] = b"unknown"
        self.rejected("tree.content", release.verify_tar, tar_bytes(changed), reader.records)
        self.rejected("tree.trailing", release.verify_tar, original+b"extra", reader.records)

    def test_archive_compression_crc_and_concatenation_are_checked(self):
        reader = FakeReadback()
        raw = gzip.compress(tar_bytes(reader.files), mtime=0)
        for changed in (raw[:-1], raw+gzip.compress(b"extra", mtime=0), raw[:-8]+b"12345678"):
            content = {"compressed_bytes":len(changed), "sha256":release.sha(changed), "raw_bytes":len(tar_bytes(reader.files)), "files":reader.records}
            with self.assertRaises(release.Rejected):
                release.verify_archive(changed, content)

    def test_runtime_tree_hardlinks_and_symlinks_are_rejected(self):
        reader = FakeReadback()
        self.assertTrue(release.tree_metadata(reader.inventory))
        self.rejected("tree.links", release.tree_metadata, reader.inventory.replace(b"\x00f\x00", b"\x00f\x00", 1).replace(b"\x00644\x001\x00", b"\x00644\x002\x00", 1))
        self.rejected("tree.type", release.tree_metadata, reader.inventory.replace(b"\x00f\x00", b"\x00l\x00", 1))

    def test_audit_refuses_changed_tree_wrapper_or_non_elf_adapter(self):
        for stage in ("wrapper", "adapter", "tree", "ready"):
            reader = FakeReadback()
            if stage == "wrapper":
                reader.regulars["/opt/elitea-rust/src/main.rs"] += b"changed"
            elif stage == "adapter":
                reader.regulars[release.ADAPTER] = b"#!/bin/sh\n"
            elif stage == "tree":
                reader.tree = tar_bytes({**reader.files, "vendor/unknown":b"extra"}, True)
            else:
                reader.regulars["/workspace/native-bundle/elitea-native-ready-v2.json"] += b"changed"
            with self.subTest(stage=stage), self.assertRaises(release.Rejected):
                audit(reader)

    def test_producer_requires_external_pin_and_exact_audit_contract(self):
        reader = FakeReadback()
        value = audit(reader)
        raw = release.encoded(value)
        self.rejected("audit.pin", release.produce, raw, "0"*64, IMAGE, reader.root, POLICY, IMAGE)
        for changed in ({"revision":1,"binding":{}}, {**value, "unexpected":True}, {**value, "revision":True}, dict(reversed(list(value.items())))):
            data = release.encoded(changed)
            with self.assertRaises(release.Rejected):
                release.produce(data, release.sha(data), IMAGE, reader.root, POLICY, IMAGE)
        duplicate = raw.replace(b'"revision":1', b'"revision":1,"revision":1', 1)
        self.rejected("json.duplicate", release.produce, duplicate, release.sha(duplicate), IMAGE, reader.root, POLICY, IMAGE)

    def test_producer_rejects_wrong_cohort_and_flags(self):
        reader = FakeReadback()
        value = audit(reader)
        for key, bad in (("image_digest", "runner:latest"), ("image_id", "sha256:"+release.sha(b"other")), ("dependency_bundle_sha256", "0"*64), ("target", "x86_64-unknown-linux-gnu")):
            changed = copy.deepcopy(value)
            changed[key] = bad
            raw = release.encoded(changed)
            with self.assertRaises(release.Rejected):
                release.produce(raw, release.sha(raw), IMAGE, reader.root, POLICY, IMAGE)
        value["measured"]["compiler_flags_sha256"] = "0"*64
        raw = release.encoded(value)
        self.rejected("audit.measured", release.produce, raw, release.sha(raw), IMAGE, reader.root, POLICY, IMAGE)

    def test_public_file_boundary_and_no_overwrite(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            path = root/"audit.json"
            release.publish(path, b"public")
            self.assertEqual(release.public_file(path, 16), b"public")
            with self.assertRaises(FileExistsError):
                release.publish(path, b"replacement")
            linked = root/"linked"
            linked.symlink_to(path)
            self.rejected("file.path", release.public_file, linked, 16)
            path.chmod(0o666)
            self.rejected("file.permissions", release.public_file, path, 16)

    def test_produce_cli_is_offline_and_emits_only_profile_and_pin(self):
        reader = FakeReadback()
        raw = release.encoded(audit(reader))
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            source = root/"audit.json"
            source.write_bytes(raw)
            source.chmod(0o600)
            destination = root/"profiles.json"
            result = subprocess.run([sys.executable, release.__file__, "produce", "--audit", str(source), "--audit-sha256", release.sha(raw), "--image-digest", IMAGE, "--image-id", IMAGE, "--bundle-sha256", reader.root, "--policy-revision", POLICY, "--output", str(destination)], capture_output=True, check=True)
            status = json.loads(result.stdout)
            self.assertEqual(status["sha256"], release.sha(destination.read_bytes()))
            self.assertEqual(status["bytes"], destination.stat().st_size)
            self.assertEqual(result.stderr, b"")

    def test_readback_output_bound_reaps_the_mock_command(self):
        original = subprocess.Popen
        children = []
        def spawn(_argv, **kwargs):
            child = original([sys.executable, "-c", "print('bounded-output')"], **kwargs)
            children.append(child)
            return child
        reader = release.DockerReadback(CONTAINER)
        with mock.patch.object(release.subprocess, "Popen", side_effect=spawn):
            self.rejected("command.output_bound", reader.command, ["fixed-test"], 1)
        self.assertTrue(all(child.poll() is not None for child in children))

    def test_total_readback_deadline_prevents_any_command(self):
        reader = release.DockerReadback(CONTAINER)
        reader.deadline = 0
        with mock.patch.object(release.subprocess, "Popen") as spawn:
            self.rejected("audit.deadline", reader.command, ["fixed-test"])
            spawn.assert_not_called()

    def test_regular_readback_rejects_hardlinks_and_identity_change(self):
        reader = release.DockerReadback(CONTAINER)
        with mock.patch.object(reader, "exec", return_value=b"81a4:3:2:1:100\n"):
            self.rejected("file.regular", reader.regular, release.ADAPTER, 8)
        with mock.patch.object(reader, "exec", side_effect=[b"81a4:3:1:1:100\n", b"abc", b"81a4:3:1:1:101\n"]):
            self.rejected("file.changed", reader.regular, release.ADAPTER, 8)

    def test_hash_domains_use_length_prefix_and_exact_json(self):
        import hashlib
        raw = b"public"
        self.assertEqual(release.domain(b"domain\0", raw), hashlib.sha256(b"domain\0"+b"\0\0\0\0\0\0\0\x06"+raw).hexdigest())
        self.assertNotEqual(release.flags(None), release.flags(release.TARGETS["arm64"]))
        self.assertNotEqual(release.flags(release.TARGETS["arm64"]), release.flags(release.TARGETS["amd64"]))


if __name__ == "__main__":
    unittest.main()
