#!/usr/bin/env python3
"""Read public inert-container evidence. Produce one pinned Main release profile."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import selectors
import stat
import subprocess
import time
import zlib

MIB = 1024 * 1024
FILE_LIMIT, RAW_LIMIT, ARCHIVE_LIMIT, COUNT_LIMIT = 32*MIB, 256*MIB, 128*MIB, 16384
PROFILE = "/workspace/rust-job/profile"
ADAPTER = "/usr/local/bin/elitea-code-rust"
ENV = {"PATH":"/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin", "RUSTUP_HOME":"/usr/local/rustup", "CARGO_HOME":"/workspace/rust-job/cargo-home", "CARGO_INCREMENTAL":"0", "CARGO_TERM_COLOR":"never", "HOME":"/workspace", "TMPDIR":"/workspace/rust-job/tmp"}
TARGETS = {"arm64":"aarch64-unknown-linux-gnu", "amd64":"x86_64-unknown-linux-gnu"}
MEASURED = ("cargo_manifest_sha256", "cargo_lock_sha256", "cargo_config_sha256", "vendor_sha256", "toolchain_sha256", "adapter_sha256", "wrapper_sha256", "compiler_flags_sha256")
AUDIT_FIELDS = ("revision", "kind", "image_digest", "image_id", "platform", "target", "policy_revision", "dependency_bundle_sha256", "native_metadata_sha256", "native_record_sha256", "native_archive_sha256", "container_id", "image_inspection_sha256", "container_policy_sha256", "measured")

class Rejected(ValueError):
    """Use fixed codes. Do not expose container output or request content."""


def require(condition, code):
    if not condition:
        raise Rejected(code)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def encoded(value, sorted_keys=False):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), sort_keys=sorted_keys, allow_nan=False).encode()


def domain(name, raw):
    return sha(name + len(raw).to_bytes(8, "big") + raw)


def digest(value):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value) is not None


def image(value):
    return isinstance(value, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", value) is not None


def integer(value, low, high):
    return type(value) is int and low <= value <= high


def fields(value, names, code):
    require(isinstance(value, dict) and set(value) == set(names), code)


def strict_json(raw):
    def pairs(items):
        out = {}
        for key, value in items:
            require(key not in out, "json.duplicate")
            out[key] = value
        return out
    try:
        return json.loads(raw, object_pairs_hook=pairs, parse_constant=lambda _: (_ for _ in ()).throw(Rejected("json.number")))
    except (UnicodeError, json.JSONDecodeError) as error:
        raise Rejected("json.invalid") from error


def safe_path(path):
    return isinstance(path, str) and 0 < len(path.encode()) <= 255 and not path.startswith("/") and all(ord(c) >= 32 and ord(c) != 127 and c not in "\\:" for c in path) and all(part not in ("", ".", "..") and len(part.encode()) <= 100 for part in path.split("/"))


def retained(path):
    return path in ("Cargo.toml", "Cargo.lock", ".cargo/config.toml", "src/main.rs", "src/user.rs") or path.startswith("vendor/")


def flags(target):
    return domain(b"elitea.rust.compiler-configuration.v1\0", encoded({"argv":["build","--locked","--offline","-j","2"], "target":target, "env":ENV}, True))


def native_metadata(raw, expected_image, root, policy, arch):
    require(0 < len(raw) <= 128*1024, "native.metadata_bound")
    value = strict_json(raw)
    fields(value, ("digest", "execution_image_digest", "execution_policy_revision", "kind", "language", "payload", "platform", "preparation_sha256", "revision", "source_sha256"), "native.fields")
    require(encoded(value, True) == raw, "native.canonical")
    content = {key:item for key, item in value.items() if key != "digest"}
    require(value["digest"] == root and sha(encoded(content, True)) == root, "native.root")
    require(value["revision"] == 2 and type(value["revision"]) is int and value["kind"] == "cargo" and value["language"] == "rust", "native.kind")
    require(value["execution_image_digest"] == expected_image and value["execution_policy_revision"] == policy and value["platform"] == {"abi":"gnu", "arch":arch, "os":"linux"}, "native.profile")
    payload = value["payload"]
    fields(payload, ("declaration_sha256", "objects", "preparation_sha256", "profile", "revision"), "native.payload")
    profile = payload["profile"]
    fields(profile, ("arch", "execution_image", "os", "policy_revision", "preparation_image", "rust_revision", "target"), "native.runtime")
    require(payload["revision"] == 1 and type(payload["revision"]) is int and payload["declaration_sha256"] == value["source_sha256"] and payload["preparation_sha256"] == value["preparation_sha256"] and digest(value["source_sha256"]) and digest(value["preparation_sha256"]), "native.identity")
    require(profile["execution_image"] == expected_image and image(profile["preparation_image"]) and profile["policy_revision"] == policy and profile["os"] == "linux" and profile["arch"] == arch and profile["target"] == TARGETS[arch] and profile["rust_revision"] == "1.97.1", "native.runtime")
    require(isinstance(payload["objects"], list) and len(payload["objects"]) == 2, "native.objects")
    for obj, role, bound in zip(payload["objects"], ("record", "archive"), (8*MIB, ARCHIVE_LIMIT)):
        fields(obj, ("bytes", "name", "role", "sha256"), "native.object")
        require(obj["role"] == role and digest(obj["sha256"]) and obj["name"] == obj["sha256"]+".blob" and integer(obj["bytes"], 1, bound), "native.object")
    require(payload["objects"][0]["name"] != payload["objects"][1]["name"], "native.objects")
    return payload


def preparation_record(raw, payload):
    require(len(raw) == payload["objects"][0]["bytes"] and sha(raw) == payload["objects"][0]["sha256"], "record.pin")
    value = strict_json(raw)
    fields(value, ("revision", "format", "declaration_sha256", "manifest_sha256", "lock_sha256", "vendor_config_sha256", "wrapper_sha256", "content"), "record.fields")
    require(type(value["revision"]) is int and value["revision"] == 1 and value["format"] == "elitea.rust-native-preparation.v1" and value["declaration_sha256"] == payload["declaration_sha256"], "record.identity")
    require(all(digest(value[key]) for key in ("declaration_sha256", "manifest_sha256", "lock_sha256", "vendor_config_sha256", "wrapper_sha256")), "record.hashes")
    content = value["content"]
    fields(content, ("sha256", "compressed_bytes", "raw_bytes", "files"), "record.content")
    require(content["sha256"] == payload["objects"][1]["sha256"] and content["compressed_bytes"] == payload["objects"][1]["bytes"] and integer(content["compressed_bytes"], 1, ARCHIVE_LIMIT) and integer(content["raw_bytes"], 1, RAW_LIMIT), "record.archive")
    require(isinstance(content["files"], list) and 1 <= len(content["files"]) <= COUNT_LIMIT, "record.count")
    previous, paths = "", set()
    for item in content["files"]:
        fields(item, ("path", "bytes", "sha256", "mode"), "record.file")
        path = item["path"]
        require(safe_path(path) and retained(path) and path > previous and integer(item["bytes"], 0, FILE_LIMIT) and digest(item["sha256"]) and type(item["mode"]) is int and item["mode"] in (0o644, 0o755), "record.file")
        require(not any(str(parent) in paths for parent in PurePosixPath(path).parents if str(parent) != "."), "record.path")
        paths.add(path)
        previous = path
    by_path = {item["path"]:item for item in content["files"]}
    for path, key in (("Cargo.toml", "manifest_sha256"), ("Cargo.lock", "lock_sha256"), (".cargo/config.toml", "vendor_config_sha256"), ("src/main.rs", "wrapper_sha256")):
        require(path in by_path and by_path[path]["sha256"] == value[key], "record.root_files")
    require("src/user.rs" in by_path, "record.placeholder")
    return value


def verify_tar(raw, expected, runtime=False):
    """Verify regular USTAR bytes. Never extract or execute archive content."""
    import tarfile
    require(len(raw) <= RAW_LIMIT+32*MIB, "tree.raw_bound")
    found, end, directories = [], 0, 0
    try:
        with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as archive:
            for item in archive:
                require(item.offset_data == item.offset+512 and raw[item.offset+257:item.offset+263] == b"ustar\0" and not item.pax_headers, "tree.ustar")
                path = item.name.removeprefix("./") if runtime else item.name
                if runtime and item.isdir():
                    directories += 1
                    require(directories <= COUNT_LIMIT and (path in ("", ".") or safe_path(path.rstrip("/"))), "tree.directory")
                else:
                    require(item.isreg() and safe_path(path) and retained(path) and 0 <= item.size <= FILE_LIMIT and item.mode in (0o644, 0o755), "tree.regular")
                    require(item.uid == 0 and item.gid == 0 and item.mtime == 0, "tree.metadata")
                    file = archive.extractfile(item)
                    data = file.read(FILE_LIMIT+1)
                    require(len(data) == item.size, "tree.file_bound")
                    found.append({"path":path, "bytes":item.size, "sha256":sha(data), "mode":item.mode})
                    require(len(found) <= COUNT_LIMIT, "tree.count")
                end = item.offset_data + ((item.size+511)//512)*512
    except (tarfile.TarError, OSError) as error:
        raise Rejected("tree.invalid") from error
    require(not raw[end:].strip(b"\0"), "tree.trailing")
    if runtime:
        found.sort(key=lambda item:item["path"])
    require(found == expected, "tree.content")


def verify_archive(raw, content):
    require(len(raw) == content["compressed_bytes"] and sha(raw) == content["sha256"], "archive.pin")
    try:
        stream = zlib.decompressobj(16+zlib.MAX_WBITS)
        expanded = stream.decompress(raw, RAW_LIMIT+1)
        require(stream.eof and not stream.unused_data and not stream.unconsumed_tail and len(expanded) == content["raw_bytes"], "archive.compression")
    except zlib.error as error:
        raise Rejected("archive.invalid") from error
    verify_tar(expanded, content["files"])


def tree_metadata(raw):
    require(len(raw) <= 8*MIB, "tree.inventory_bound")
    try:
        parts = raw.decode().split("\0")
        require(parts.pop() == "" and len(parts)%7 == 0 and len(parts)//7 <= COUNT_LIMIT+8, "tree.inventory")
        files = {}
        for index in range(0, len(parts), 7):
            path, kind, size, mode, links, device, inode = parts[index:index+7]
            require(path == "" or safe_path(path), "tree.path")
            require(kind in ("d", "f") and path not in files, "tree.type")
            values = (int(size), int(mode, 8), int(links), int(device), int(inode))
            if kind == "f":
                require(retained(path) and 0 <= values[0] <= FILE_LIMIT and values[2] == 1, "tree.links")
            files[path] = (kind, *values)
        require(files.get("", (None,))[0] == "d", "tree.root")
        vendor_count = sum(path.startswith("vendor/") for path in files)
        require(vendor_count <= COUNT_LIMIT, "tree.vendor_entries")
        return files
    except (UnicodeError, ValueError) as error:
        if isinstance(error, Rejected):
            raise
        raise Rejected("tree.inventory") from error


IMAGE_FORMAT = '{"id":{{json .Id}},"os":{{json .Os}},"architecture":{{json .Architecture}},"user":{{json .Config.User}},"repo_digests":[{{range .RepoDigests}}{{$parts := split . "@"}}{{if eq (len $parts) 2}}{{json (index $parts 1)}},{{end}}{{end}}""]}'
COMPILED_NAMES = ("ELITEA_COMPILED_CODE_SNAPSHOTS", "ELITEA_COMPILED_CODE_JOB_PURPOSE", "ELITEA_COMPILED_CODE_CONTROL_SHA256", "ELITEA_COMPILED_CODE_IMAGE_DIGEST", "ELITEA_COMPILED_CODE_POLICY_REVISION")
ENV_GUARD = '[{{range .Config.Env}}{{if or '+ ' '.join('(eq (index (split . "=") 0) "'+name+'")' for name in COMPILED_NAMES)+'}}1,{{end}}{{end}}0]'
CONTAINER_FORMAT = '{"image":{{json .Image}},"running":{{json .State.Running}},"user":{{json .Config.User}},"entrypoint":{{json .Config.Entrypoint}},"cmd":{{json .Config.Cmd}},"readonly":{{json .HostConfig.ReadonlyRootfs}},"network":{{json .HostConfig.NetworkMode}},"privileged":{{json .HostConfig.Privileged}},"cap_add":{{json .HostConfig.CapAdd}},"cap_drop":{{json .HostConfig.CapDrop}},"security":{{json .HostConfig.SecurityOpt}},"memory":{{json .HostConfig.Memory}},"swap":{{json .HostConfig.MemorySwap}},"cpus":{{json .HostConfig.NanoCpus}},"pids":{{json .HostConfig.PidsLimit}},"pid_mode":{{json .HostConfig.PidMode}},"devices":{{len .HostConfig.Devices}},"mounts":[{{range .Mounts}}{"type":{{json .Type}},"destination":{{json .Destination}}},{{end}}{}],"tmpfs":{{json .HostConfig.Tmpfs}},"compiled":'+ENV_GUARD+'}'



def tmpfs_policy(options):
    require(isinstance(options, str) and 0 < len(options) <= 128, "container.workspace")
    permitted = {"rw", "exec", "nosuid", "nodev", "size", "uid", "gid", "mode"}
    parsed = {}
    for option in options.split(","):
        key, separator, value = option.partition("=")
        require(key in permitted and key not in parsed, "container.workspace")
        parsed[key] = value if separator else None
    size = parsed.get("size")
    expected = {"rw":None, "nosuid":None, "nodev":None, "size":size, "uid":"10001", "gid":"10001", "mode":"0700"}
    if "exec" in parsed:
        expected["exec"] = None
    require(parsed == expected, "container.workspace")
    require(isinstance(size, str) and re.fullmatch(r"[1-9][0-9]{0,9}[kmg]", size) is not None, "container.workspace_bound")
    require(int(size[:-1])*{"k":1024,"m":MIB,"g":1024*MIB}[size[-1]] <= 4*1024*MIB, "container.workspace_bound")


def container_policy(value, expected_image):
    require(value["image"] == expected_image and value["running"] is True and value["user"] in ("10001", "10001:10001"), "container.identity")
    require(value["readonly"] is True and value["network"] == "none" and value["privileged"] is False and value["cap_add"] in (None, []) and value["cap_drop"] == ["ALL"] and value["security"] in (["no-new-privileges"], ["no-new-privileges=true"]), "container.isolation")
    require(integer(value["memory"], 64*MIB, 4*1024*MIB) and value["swap"] == value["memory"] and integer(value["cpus"], 1, 2_000_000_000) and integer(value["pids"], 1, 128) and value["pid_mode"] == "" and value["devices"] == 0, "container.resources")
    require(value["compiled"] == [0], "container.compiled_dispatch")
    require(value["entrypoint"] == ["/bin/sh"] and isinstance(value["cmd"], list) and len(value["cmd"]) == 2 and value["cmd"][0] == "-c" and re.fullmatch(r"exec sleep [1-9][0-9]{0,3}", value["cmd"][1]) and int(value["cmd"][1].split()[-1]) <= 3600, "container.inert")
    require(value["mounts"][-1:] == [{}] and all(mount["type"] == "tmpfs" and mount["destination"] in ("/workspace", "/tmp") for mount in value["mounts"][:-1]), "container.overlay")
    require(isinstance(value["tmpfs"], dict) and "/workspace" in value["tmpfs"] and set(value["tmpfs"]) <= {"/workspace", "/tmp"}, "container.workspace")
    for options in value["tmpfs"].values():
        tmpfs_policy(options)


class DockerReadback:
    """Run fixed public metadata commands. Root owns the inert container lifecycle."""
    def __init__(self, container):
        require(re.fullmatch(r"[0-9a-f]{64}", container) is not None, "container.id")
        self.container, self.commands = container, []
        self.deadline = time.monotonic()+600

    def command(self, args, cap=128*1024, timeout=30):
        started = time.monotonic()
        require(started < self.deadline, "audit.deadline")
        timeout = min(timeout, self.deadline-started)
        child = subprocess.Popen(["docker", *args], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        buffers = {child.stdout:bytearray(), child.stderr:bytearray()}
        selector = selectors.DefaultSelector()
        for pipe in buffers:
            selector.register(pipe, selectors.EVENT_READ)
        try:
            while selector.get_map():
                require(time.monotonic()-started <= timeout, "command.deadline")
                for key, _ in selector.select(0.1):
                    raw = os.read(key.fileobj.fileno(), 65536)
                    if not raw:
                        selector.unregister(key.fileobj)
                    else:
                        target = buffers[key.fileobj]
                        require(len(target)+len(raw) <= (cap if key.fileobj is child.stdout else 64*1024), "command.output_bound")
                        target.extend(raw)
            require(child.wait(timeout=max(0.1, timeout-(time.monotonic()-started))) == 0, "command.failed")
            raw = bytes(buffers[child.stdout])
            self.commands.append({"argv":["docker", *args], "stdout_bytes":len(raw), "stdout_sha256":sha(raw)})
            return raw
        finally:
            if child.poll() is None:
                child.kill()
                child.wait(timeout=5)
            selector.close()
            for pipe in buffers:
                pipe.close()

    def exec(self, *argv, cap=128*1024, timeout=30):
        return self.command(["exec", "--user", "10001:10001", self.container, *argv], cap, timeout)

    def regular(self, path, cap):
        command = ("stat", "-c", "%f:%s:%h:%d:%i", path)
        before = self.exec(*command)
        parts = before.decode().strip().split(":")
        require(len(parts) == 5 and stat.S_ISREG(int(parts[0], 16)) and int(parts[2]) == 1 and int(parts[1]) <= cap, "file.regular")
        raw = self.exec("cat", path, cap=cap, timeout=120 if cap > FILE_LIMIT else 30)
        require(len(raw) == int(parts[1]) and self.exec(*command) == before, "file.changed")
        return raw


def audit_native(reader, expected_image, root, policy, image_id):
    require(image(expected_image) and image(image_id) and digest(root) and re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", policy) is not None, "audit.selection")
    image_raw = reader.command(["image", "inspect", image_id, "--format", IMAGE_FORMAT])
    image_info = strict_json(image_raw)
    require(image_info["id"] == image_id and image_info["os"] == "linux" and image_info["architecture"] in TARGETS and image_info["user"] in ("10001", "10001:10001"), "image.identity")
    require(image_info["repo_digests"][-1:] == [""] and all(image(value) for value in image_info["repo_digests"][:-1]) and (expected_image == image_id or expected_image in image_info["repo_digests"][:-1]), "image.release_digest")
    initial_policy = strict_json(reader.command(["inspect", reader.container, "--format", CONTAINER_FORMAT]))
    container_policy(initial_policy, image_id)
    arch = image_info["architecture"]
    metadata = reader.regular("/workspace/native-bundle/elitea-native-bundle-v2.json", 128*1024)
    payload = native_metadata(metadata, expected_image, root, policy, arch)
    for path in ("/workspace/native-bundle/elitea-native-ready-v2.json", "/workspace/rust-job/.elitea-cargo-profile.json"):
        require(reader.regular(path, 128*1024) == metadata, "native.ready")
    record_raw = reader.regular("/workspace/native-bundle/objects/"+payload["objects"][0]["name"], 8*MIB)
    record = preparation_record(record_raw, payload)
    archive = reader.regular("/workspace/native-bundle/objects/"+payload["objects"][1]["name"], ARCHIVE_LIMIT)
    verify_archive(archive, record["content"])
    del archive
    inventory_command = ("find", "-P", PROFILE, "-printf", "%P\\0%y\\0%s\\0%m\\0%n\\0%D\\0%i\\0")
    before = tree_metadata(reader.exec(*inventory_command, cap=8*MIB))
    expected = {item["path"]:item for item in record["content"]["files"]}
    require({path for path, value in before.items() if value[0] == "f"} == set(expected), "tree.files")
    for path, item in expected.items():
        require(before[path][1:3] == (item["bytes"], item["mode"]), "tree.metadata")
    tree = reader.exec("tar", "--format=ustar", "--owner=0", "--group=0", "--mtime=@0", "-C", PROFILE, "-cf", "-", ".", cap=RAW_LIMIT+32*MIB, timeout=120)
    verify_tar(tree, record["content"]["files"], True)
    del tree
    require(tree_metadata(reader.exec(*inventory_command, cap=8*MIB)) == before, "tree.changed")
    wrapper = sha(reader.regular("/opt/elitea-rust/src/main.rs", MIB))
    require(wrapper == record["wrapper_sha256"], "image.wrapper")
    adapter = reader.regular(ADAPTER, FILE_LIMIT)
    require(adapter.startswith(b"\x7fELF"), "image.adapter")
    versions = [reader.exec("env", "-i", "PATH=/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin", "RUSTUP_HOME=/usr/local/rustup", "CARGO_HOME=/usr/local/cargo", "/usr/local/cargo/bin/"+program, "-Vv", cap=8192) for program in ("rustc", "cargo")]
    require(b"release: 1.97.1\n" in versions[0], "image.toolchain")
    vendor = [{"path":item["path"][7:], "bytes":item["bytes"], "sha256":item["sha256"], "mode":item["mode"]} for item in record["content"]["files"] if item["path"].startswith("vendor/")]
    require(sum(item["bytes"] for item in vendor) <= RAW_LIMIT, "vendor.bound")
    measured = dict(zip(MEASURED, (record["manifest_sha256"], record["lock_sha256"], record["vendor_config_sha256"], domain(b"elitea.rust.vendor-tree.v1\0", encoded(vendor)), domain(b"elitea.rust.toolchain.v1\0", encoded([list(value) for value in versions])), sha(adapter), wrapper, flags(TARGETS[arch]))))
    final_policy = strict_json(reader.command(["inspect", reader.container, "--format", CONTAINER_FORMAT]))
    require(final_policy == initial_policy, "container.changed")
    return dict(zip(AUDIT_FIELDS, (1, "elitea.rust.compiled-release-audit.v1", expected_image, image_id, "linux/"+arch+"/gnu", TARGETS[arch], policy, root, sha(metadata), sha(record_raw), payload["objects"][1]["sha256"], reader.container, sha(image_raw), sha(encoded(initial_policy)), measured)))


def produce(raw, pin, expected_image, root, policy, image_id):
    require(0 < len(raw) <= 16*1024 and digest(pin) and sha(raw) == pin, "audit.pin")
    value = strict_json(raw)
    fields(value, AUDIT_FIELDS, "audit.fields")
    require(list(value) == list(AUDIT_FIELDS) and encoded(value) == raw and type(value["revision"]) is int and value["revision"] == 1 and value["kind"] == "elitea.rust.compiled-release-audit.v1", "audit.contract")
    require(image(expected_image) and image(image_id) and value["image_id"] == image_id and digest(root) and value["image_digest"] == expected_image and value["dependency_bundle_sha256"] == root and value["policy_revision"] == policy and re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", policy) is not None, "audit.selection")
    require((value["platform"], value["target"]) in (("linux/arm64/gnu", TARGETS["arm64"]), ("linux/amd64/gnu", TARGETS["amd64"])), "audit.platform")
    require(all(digest(value[name]) for name in ("native_metadata_sha256", "native_record_sha256", "native_archive_sha256", "container_id", "image_inspection_sha256", "container_policy_sha256")), "audit.native")
    fields(value["measured"], MEASURED, "audit.measured")
    require(list(value["measured"]) == list(MEASURED) and all(digest(value["measured"][name]) for name in MEASURED) and value["measured"]["compiler_flags_sha256"] == flags(value["target"]), "audit.measured")
    template = sha(b"elitea.rust.release-template.v1\0")
    binding = {"revision":1, "reuse_policy":"snapshot_v1", "tenant_id":"release_template", "project_id":1, "base_prepared_request_sha256":template, "source_sha256":template, "compilation_image_digest":expected_image, "execution_image_digest":expected_image, "platform":value["platform"], "target":value["target"], "policy_revision":policy, **value["measured"]}
    return encoded({"revision":1, "profiles":[{"binding":binding, "dependency_bundle_sha256":root}]})


def public_file(path, bound):
    path = Path(path)
    require(path.is_absolute() and path.resolve() == path, "file.path")
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(descriptor)
        require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_mode & 0o400 and not info.st_mode & 0o133 and 0 < info.st_size <= bound, "file.permissions")
        with os.fdopen(descriptor, "rb", closefd=False) as stream:
            raw = stream.read(bound+1)
        require(len(raw) == info.st_size, "file.changed")
        return raw
    finally:
        os.close(descriptor)


def publish(path, raw):
    path = Path(path)
    require(path.is_absolute() and path.parent.resolve() == path.parent, "output.path")
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o644)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="mode", required=True)
    audit = commands.add_parser("audit-native")
    audit.add_argument("--container-id", required=True)
    production = commands.add_parser("produce")
    production.add_argument("--audit", required=True)
    production.add_argument("--audit-sha256", required=True)
    for command in (audit, production):
        command.add_argument("--image-digest", required=True)
        command.add_argument("--image-id", required=True)
        command.add_argument("--bundle-sha256", required=True)
        command.add_argument("--policy-revision", required=True)
        command.add_argument("--output", required=True)
    args = parser.parse_args()
    try:
        if args.mode == "audit-native":
            reader = DockerReadback(args.container_id)
            raw = encoded(audit_native(reader, args.image_digest, args.bundle_sha256, args.policy_revision, args.image_id))
        else:
            raw = produce(public_file(args.audit, 16*1024), args.audit_sha256, args.image_digest, args.bundle_sha256, args.policy_revision, args.image_id)
        publish(args.output, raw)
        print(encoded({"status":"written", "sha256":sha(raw), "bytes":len(raw), "acceptance":"pending_native_cohort"}).decode())
    except (Rejected, OSError, subprocess.SubprocessError, KeyError, TypeError, ValueError) as error:
        print(encoded({"status":"rejected", "code":str(error) if isinstance(error, Rejected) else "operation.failed"}).decode())
        raise SystemExit(1)


if __name__ == "__main__":
    main()
