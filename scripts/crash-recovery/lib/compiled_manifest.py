"""Hand-built compiled-Rust release manifest for an empty dependency bundle (built-in vendor material).

scripts/runtime/rust_compiled_release_manifest.py only produces native Cargo bundles. A locally built code-runner
rust-runtime image uses the built-in /opt/rust-vendor tree instead, so this module replicates the runner's own
measurement (services/elitea-code-runner/src/compiled_snapshot.rs `verify_binding` with target=None) and emits the
canonical profile file the supervisor/worker read (deploy/scripts/check-compiled-sandbox-material.py).
Standard library only.
"""
import base64
import hashlib
import json
import os
import pathlib

from .common import HarnessError, run, sha256_hex

VENDOR_ROOT = '/opt/rust-vendor'
PROFILE_DIR = '/opt/elitea-rust'
PROFILE_FILES = ('Cargo.toml', 'Cargo.lock', 'src/main.rs', '.cargo/config.toml')
ADAPTER = '/usr/local/bin/elitea-code-rust'
TOOL_ENV = 'PATH=/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo'
MAX_OUTPUT = 64 * 1024 * 1024
MAX_VERSION = 8192
MAX_VENDOR_ENTRIES = 16384
ARCH = {
    'arm64': ('linux/arm64/gnu', 'aarch64-unknown-linux-gnu'),
    'amd64': ('linux/amd64/gnu', 'x86_64-unknown-linux-gnu'),
}
FIELDS = (
    'revision', 'reuse_policy', 'tenant_id', 'project_id', 'base_prepared_request_sha256', 'source_sha256',
    'compilation_image_digest', 'execution_image_digest', 'platform', 'target', 'policy_revision',
    'cargo_manifest_sha256', 'cargo_lock_sha256', 'cargo_config_sha256', 'vendor_sha256', 'toolchain_sha256',
    'adapter_sha256', 'wrapper_sha256', 'compiler_flags_sha256',
)
MEASURED_FIELDS = ('cargo_manifest_sha256', 'cargo_lock_sha256', 'cargo_config_sha256', 'wrapper_sha256',
                   'adapter_sha256', 'vendor_sha256', 'toolchain_sha256', 'compiler_flags_sha256')
FLAGS_ENV = {
    'CARGO_HOME': '/workspace/rust-job/cargo-home', 'CARGO_INCREMENTAL': '0', 'CARGO_TERM_COLOR': 'never',
    'HOME': '/workspace', 'PATH': '/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin',
    'RUSTUP_HOME': '/usr/local/rustup', 'TMPDIR': '/workspace/rust-job/tmp',
}
SECTION = b'\n@@SECTION:%s@@\n'
# One inert container prints sections; NUL separates records so odd file names cannot forge fields.
SCRIPT = f'''set -eu
ENVV="{TOOL_ENV}"
printf '\\n@@SECTION:profile@@\\n'
for f in {" ".join(PROFILE_DIR + "/" + p for p in PROFILE_FILES)} {ADAPTER}; do
  printf '%s %s\\n' "$(sha256sum "$f" | cut -d' ' -f1)" "$f"
done
printf '\\n@@SECTION:rustc@@\\n'
env -i $ENVV /usr/local/cargo/bin/rustc -Vv > /tmp/rustc.v
base64 -w0 /tmp/rustc.v
printf '\\n@@SECTION:cargo@@\\n'
env -i $ENVV /usr/local/cargo/bin/cargo -Vv > /tmp/cargo.v
base64 -w0 /tmp/cargo.v
printf '\\n@@SECTION:entries@@\\n'
find {VENDOR_ROOT} -mindepth 1 -printf '%y %s %m %P\\0'
printf '\\n@@SECTION:hashes@@\\n'
find {VENDOR_ROOT} -type f -exec sha256sum -z {{}} +
printf '\\n@@SECTION:end@@\\n'
'''


def encoded(value, sorted_keys=False):
    return json.dumps(value, ensure_ascii=False, separators=(',', ':'), sort_keys=sorted_keys,
                      allow_nan=False).encode()


def domain_hash(name, raw):
    """sha256(domain || u64 big-endian length || bytes) as hex (compiled_code.rs `domain_hash`)."""
    return hashlib.sha256(name + len(raw).to_bytes(8, 'big') + raw).hexdigest()


def vendor_hash(entries):
    """entries: iterable of {path, bytes, sha256, mode}; struct field order, byte-order sort, compact UTF-8."""
    files = sorted(({'path': e['path'], 'bytes': e['bytes'], 'sha256': e['sha256'], 'mode': e['mode']}
                    for e in entries), key=lambda e: e['path'].encode('utf-8'))
    return domain_hash(b'elitea.rust.vendor-tree.v1\0', encoded(files))


def toolchain_hash(rustc_bytes, cargo_bytes):
    """serde_json::to_vec(&[Vec<u8>, Vec<u8>]) is two arrays of integers."""
    return domain_hash(b'elitea.rust.toolchain.v1\0', encoded([list(rustc_bytes), list(cargo_bytes)]))


def flags_hash():
    """Default (non-native) profile: target is null. serde_json without preserve_order sorts keys."""
    value = {'argv': ['build', '--locked', '--offline', '-j', '2'], 'target': None, 'env': FLAGS_ENV}
    return domain_hash(b'elitea.rust.compiler-configuration.v1\0', encoded(value, True))


def _sections(raw):
    parts = (b'\n' + raw).split(b'\n@@SECTION:')
    result = {}
    for part in parts[1:]:
        name, _, body = part.partition(b'@@\n')
        result[name.decode()] = body[:-1] if body.endswith(b'\n') else body
    if 'end' not in result:
        raise HarnessError('measurement output truncated')
    return result


def _hex(value, what):
    text = value.decode('ascii', 'replace')
    if len(text) != 64 or any(c not in '0123456789abcdef' for c in text):
        raise HarnessError(f'bad digest for {what}')
    return text


def parse_measurement(raw):
    """Turn the container stream into the measured fields."""
    sec = _sections(raw)
    profile = {}
    for line in sec['profile'].strip().splitlines():
        digest, _, path = line.partition(b' ')
        profile[path.decode()] = _hex(digest, path.decode())
    versions = []
    for name in ('rustc', 'cargo'):
        data = base64.b64decode(sec[name].strip(), validate=True)
        if not 0 < len(data) <= MAX_VERSION:
            raise HarnessError(f'{name} version output out of bounds')
        versions.append(data)
    hashes = {}
    for record in sec['hashes'].split(b'\0'):
        if not record:
            continue
        digest, _, path = record.partition(b'  ')
        prefix = (VENDOR_ROOT + '/').encode()
        if not path.startswith(prefix):
            raise HarnessError('unexpected vendor hash record')
        hashes[path[len(prefix):].decode('utf-8')] = _hex(digest, 'vendor file')
    entries = []
    count = 0
    for record in sec['entries'].split(b'\0'):
        if not record:
            continue
        count += 1
        if count > MAX_VENDOR_ENTRIES:
            raise HarnessError('vendor tree has too many entries')
        kind, size, mode, path = record.split(b' ', 3)
        if kind == b'd':
            continue
        if kind != b'f':
            raise HarnessError(f'vendor entry of type {kind.decode()} is neither file nor directory')
        text = path.decode('utf-8')
        if text not in hashes:
            raise HarnessError('vendor file without hash')
        entries.append({'path': text, 'bytes': int(size), 'sha256': hashes[text], 'mode': int(mode, 8) & 0o777})
    if len(entries) != len(hashes):
        raise HarnessError('vendor entry/hash count mismatch')
    return {
        'cargo_manifest_sha256': profile[f'{PROFILE_DIR}/Cargo.toml'],
        'cargo_lock_sha256': profile[f'{PROFILE_DIR}/Cargo.lock'],
        'cargo_config_sha256': profile[f'{PROFILE_DIR}/.cargo/config.toml'],
        'wrapper_sha256': profile[f'{PROFILE_DIR}/src/main.rs'],
        'adapter_sha256': profile[ADAPTER],
        'vendor_sha256': vendor_hash(entries),
        'toolchain_sha256': toolchain_hash(*versions),
        'compiler_flags_sha256': flags_hash(),
    }


def measure(image_ref):
    """Measure the image with one inert container; return the measured fields plus platform, target, image_id."""
    inspect = run(['docker', 'image', 'inspect', '--format', '{{.Id}} {{.Architecture}}', image_ref]).stdout
    image_id, _, arch = inspect.decode().strip().partition(' ')
    if arch not in ARCH or not image_id.startswith('sha256:'):
        raise HarnessError(f'unsupported image architecture or id: {arch!r}')
    result = run(['docker', 'run', '--rm', '--network', 'none', '--read-only', '--cap-drop', 'ALL',
                  '--security-opt', 'no-new-privileges', '--user', '10001:10001', '--pids-limit', '64',
                  '--memory', '512m', '--tmpfs', '/tmp:rw,size=16m', '--entrypoint', '/bin/sh', image_ref,
                  '-c', SCRIPT], timeout=300)
    if len(result.stdout) > MAX_OUTPUT:
        raise HarnessError('measurement output exceeds the bound')
    measured = parse_measurement(result.stdout)
    measured['platform'], measured['target'] = ARCH[arch]
    measured['image_id'] = image_id
    return measured


def manifest_bytes(measured, image_id, platform, target, policy):
    template = sha256_hex(b'elitea.rust.release-template.v1\0')
    values = {
        'revision': 1, 'reuse_policy': 'snapshot_v1', 'tenant_id': 'release_template', 'project_id': 1,
        'base_prepared_request_sha256': template, 'source_sha256': template,
        'compilation_image_digest': image_id, 'execution_image_digest': image_id,
        'platform': platform, 'target': target, 'policy_revision': policy,
    }
    values.update({name: measured[name] for name in MEASURED_FIELDS})
    binding = {name: values[name] for name in FIELDS}
    return encoded({'revision': 1, 'profiles': [{'binding': binding, 'dependency_bundle_sha256': ''}]})


def build(image_ref, policy_revision):
    measured = measure(image_ref)
    return manifest_bytes(measured, measured['image_id'], measured['platform'], measured['target'], policy_revision)


def write(path, raw):
    """Write a 0644 regular file (overwrite allowed); return its sha256 hex."""
    path = pathlib.Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + '.tmp')
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o644)
    with os.fdopen(fd, 'wb') as handle:
        handle.write(raw)
    os.chmod(tmp, 0o644)
    os.replace(tmp, path)
    return sha256_hex(raw)
