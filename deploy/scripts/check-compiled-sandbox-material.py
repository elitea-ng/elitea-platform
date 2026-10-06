#!/usr/bin/env python3
"""Check public compiled-snapshot material before an opt-in Compose launch."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys

LIMIT = 1024 * 1024
FIELDS = (
    'revision', 'reuse_policy', 'tenant_id', 'project_id',
    'base_prepared_request_sha256', 'source_sha256',
    'compilation_image_digest', 'execution_image_digest', 'platform', 'target',
    'policy_revision', 'cargo_manifest_sha256', 'cargo_lock_sha256',
    'cargo_config_sha256', 'vendor_sha256', 'toolchain_sha256', 'adapter_sha256',
    'wrapper_sha256', 'compiler_flags_sha256',
)
QUOTAS = {
    'GLOBAL_ENTRIES': 100000, 'GLOBAL_BYTES': 1099511627776,
    'TENANT_ENTRIES': 100000, 'TENANT_BYTES': 1099511627776,
    'PUBLISHING_TTL_SECONDS': 300, 'READY_TTL_SECONDS': 86400,
}


def require(condition):
    if not condition:
        raise ValueError('compiled snapshot deployment contract is invalid')


def digest(value):
    return isinstance(value, str) and re.fullmatch('[a-f0-9]{64}', value) is not None


def read_public(filename):
    path = Path(filename)
    require(path.is_absolute() and str(path) == filename and path.resolve() == path)
    fd = os.open(filename, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        metadata = os.fstat(fd)
        require(stat.S_ISREG(metadata.st_mode) and 0 < metadata.st_size <= LIMIT)
        require(metadata.st_mode & 0o400 and not metadata.st_mode & 0o133)
        body = os.read(fd, LIMIT + 1)
        require(len(body) == metadata.st_size and len(body) <= LIMIT)
        return body
    finally:
        os.close(fd)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result)
        result[key] = value
    return result


def decode(body):
    return json.loads(body, object_pairs_hook=unique_object)


def canonical(value):
    return json.dumps(value, separators=(',', ':'), ensure_ascii=False).encode()


def profiles(filename, pin):
    require(digest(pin))
    body = read_public(filename)
    require(hashlib.sha256(body).hexdigest() == pin)
    value = decode(body)
    require(isinstance(value, dict) and list(value) == ['revision', 'profiles'])
    require(type(value['revision']) is int and value['revision'] == 1)
    require(isinstance(value['profiles'], list) and 1 <= len(value['profiles']) <= 64)
    require(canonical(value) == body)
    identities = set()
    for record in value['profiles']:
        require(list(record) == ['binding', 'dependency_bundle_sha256'])
        binding = record['binding']
        require(list(binding) == list(FIELDS))
        require(type(binding['revision']) is int and binding['revision'] == 1)
        require(binding['reuse_policy'] == 'snapshot_v1')
        require(re.fullmatch('[a-zA-Z0-9_-]{1,128}', binding['tenant_id']) is not None)
        require(type(binding['project_id']) is int and 1 <= binding['project_id'] <= 2147483647)
        require(re.fullmatch('[a-zA-Z0-9_.-]{1,128}', binding['policy_revision']) is not None)
        image = binding['execution_image_digest']
        require(isinstance(image, str) and image.startswith('sha256:') and digest(image[7:]))
        require(binding['compilation_image_digest'] == image)
        require((binding['platform'], binding['target']) in (
            ('linux/arm64/gnu', 'aarch64-unknown-linux-gnu'),
            ('linux/amd64/gnu', 'x86_64-unknown-linux-gnu'),
        ))
        for key in FIELDS:
            if key.endswith('_sha256'):
                require(digest(binding[key]))
        bundle = record['dependency_bundle_sha256']
        require(bundle == '' or digest(bundle))
        identity = canonical(record)
        require(identity not in identities)
        identities.add(identity)
    return value['profiles']


def setting(config, expected_path, pin):
    require(isinstance(config, dict) and set(config) == {
        'profiles_file', 'profiles_sha256', 'dependency_bundle_sha256',
    })
    require(all(isinstance(value, str) for value in config.values()))
    require(config['profiles_file'] == expected_path and config['profiles_sha256'] == pin)
    require(config['dependency_bundle_sha256'] == '' or digest(config['dependency_bundle_sha256']))
    return config['dependency_bundle_sha256']


def platform(config):
    require(isinstance(config, dict) and set(config) == {'os', 'arch', 'abi'})
    return '/'.join(config[key] for key in ('os', 'arch', 'abi'))


def verify(profiles_file, pin, worker_file, supervisor_file):
    records = profiles(profiles_file, pin)
    worker = decode(read_public(worker_file))
    supervisor = decode(read_public(supervisor_file))
    require(isinstance(worker, dict) and isinstance(supervisor, dict))
    require(isinstance(worker.get('sandbox_runtimes'), list))
    selected = []
    for profile in worker['sandbox_runtimes']:
        require(isinstance(profile, dict))
        require('compiled_snapshot' not in (profile.get('preparation') or {}))
        if 'compiled_snapshot' in profile:
            require(profile.get('language') == 'rust')
            bundle = setting(profile['compiled_snapshot'],
                             '/run/elitea-runtime/rust-compiled-profiles.json', pin)
            selected.append((profile, bundle))
    require(len(selected) == 1)
    profile, bundle = selected[0]
    require(supervisor.get('purpose') == 'execution' and supervisor.get('languages') == ['rust'])
    require(isinstance(supervisor.get('dependency_content'), dict) and supervisor['dependency_content'])
    require(setting(supervisor.get('compiled_snapshot'),
                    '/run/elitea-sandbox/rust/rust-compiled-profiles.json', pin) == bundle)
    require(profile['image_digest'] == supervisor['image_digest'])
    require(profile['policy_revision'] == supervisor['policy_revision'])
    matches = [record for record in records
               if record['binding']['execution_image_digest'] == profile['image_digest']
               and record['binding']['policy_revision'] == profile['policy_revision']
               and record['dependency_bundle_sha256'] == bundle]
    require(len(matches) == 1)
    if bundle:
        require(platform(supervisor.get('native_platform')) == matches[0]['binding']['platform'])
        require(platform((profile.get('preparation') or {}).get('native_platform')) == matches[0]['binding']['platform'])
    return len(records)


def quotas(environment):
    values = {}
    for suffix, maximum in QUOTAS.items():
        value = environment.get('ELITEA_RUST_COMPILED_' + suffix, '')
        require(re.fullmatch('[1-9][0-9]{0,12}', value) is not None)
        values[suffix] = int(value)
        require(values[suffix] <= maximum)
    require(values['TENANT_ENTRIES'] <= values['GLOBAL_ENTRIES'])
    require(values['TENANT_BYTES'] <= values['GLOBAL_BYTES'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--profiles-file', required=True)
    parser.add_argument('--profiles-sha256', required=True)
    parser.add_argument('--worker-config', required=True)
    parser.add_argument('--rust-config', required=True)
    args = parser.parse_args()
    try:
        count = verify(args.profiles_file, args.profiles_sha256, args.worker_config, args.rust_config)
        quotas(os.environ)
    except (ValueError, OSError, TypeError, KeyError):
        print('compiled snapshot material preflight failed', file=sys.stderr)
        return 1
    print(f'compiled snapshot material preflight passed: {count} release profiles')
    return 0


if __name__ == '__main__':
    sys.exit(main())
