#!/usr/bin/env python3
"""Test compiled-snapshot deployment contracts using synthetic material only."""
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[3]
CHART = ROOT / 'deploy/helm/elitea'
SPEC = importlib.util.spec_from_file_location('preflight', ROOT / 'deploy/scripts/check-compiled-sandbox-material.py')
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)
RAW = (ROOT / 'deploy/helm/tests/fixtures/compiled-profiles.synthetic.json').read_bytes()
PIN = hashlib.sha256(RAW).hexdigest()
IMAGE = json.loads(RAW)['profiles'][0]['binding']['execution_image_digest']
SAFE_ENV = {'PATH': os.environ['PATH'], 'COMPOSE_DISABLE_ENV_FILE': '1'}
QUOTAS = {'globalEntries': '100', 'globalBytes': '1073741824', 'tenantEntries': '10',
          'tenantBytes': '268435456', 'publishingTtlSeconds': '60', 'readyTtlSeconds': '3600'}


def setting(path):
    return dict(profiles_file=path, profiles_sha256=PIN, dependency_bundle_sha256='')


def values():
    return {
        'runtimeRedis': {'enabled': True},
        'postgresql': {'maxConnections': 250},
        'main': {'runtime': {'sandboxAudiences': ['dns:rust', 'dns:python'], 'rustCompiledSnapshots': dict(enabled=True, profilesSha256=PIN, **QUOTAS)}},
        'worker': {'enabled': True, 'implementation': 'rust', 'runtime': {'sandboxRuntimes': [
            dict(language='python', target='python:9446', audience='dns:python', image_digest=IMAGE,
                 policy_revision='python-v1', timeout_seconds=60,
                 preparation=dict(target='python-prep:9448', audience='dns:python-prep',
                                  image_digest=IMAGE, policy_revision='python-preparation-v1', timeout_seconds=120)),
            dict(language='rust', target='rust:9447', audience='dns:rust', image_digest=IMAGE,
                 policy_revision='p1', timeout_seconds=60,
                 compiled_snapshot=setting('/run/elitea-runtime/rust-compiled-profiles.json')),
        ]}},
        'sandboxKubernetes': {'enabled': True, 'executionNamespace': 'code-execution', 'supervisor': {
            'enabled': True, 'image': 'registry/supervisor@sha256:' + 'a' * 64,
            'materialSecret': 'sandbox-material', 'profiles': [
                dict(file='deno.json', port=9446),
                dict(file='rust.json', port=9447, purpose='execution', languages=['rust'],
                     dependencyContentEnabled=True, image_digest=IMAGE, policy_revision='p1',
                     compiled_snapshot=setting('/run/elitea-sandbox/rust-compiled-profiles.json')),
            ],
        }},
    }


class Material(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix='compiled-material-')
        self.root = Path(self.directory.name).resolve()
        self.manifest = self.write('profiles.json', RAW)
        v = values()
        self.worker = {'sandbox_runtimes': v['worker']['runtime']['sandboxRuntimes']}
        self.supervisor = copy.deepcopy(v['sandboxKubernetes']['supervisor']['profiles'][1])
        self.supervisor['compiled_snapshot']['profiles_file'] = '/run/elitea-sandbox/rust/rust-compiled-profiles.json'
        self.supervisor.pop('dependencyContentEnabled')
        self.supervisor['dependency_content'] = {'staging_root': '/run/staging/private'}
        self.worker_file = self.write('worker.json', CHECK.canonical(self.worker))
        self.supervisor_file = self.write('supervisor.json', CHECK.canonical(self.supervisor))

    def tearDown(self):
        self.directory.cleanup()

    def write(self, name, body, mode=0o644):
        path = self.root / name
        if path.exists():
            path.chmod(0o600)
        path.write_bytes(body)
        path.chmod(mode)
        return str(path)

    def verify(self, worker=None, supervisor=None, raw=None, pin=None):
        if worker is not None:
            self.write('worker.json', CHECK.canonical(worker))
        if supervisor is not None:
            self.write('supervisor.json', CHECK.canonical(supervisor))
        if raw is not None:
            self.write('profiles.json', raw)
        return CHECK.verify(self.manifest, pin or PIN, self.worker_file, self.supervisor_file)

    def test_canonical_public_and_private_modes(self):
        for mode in (0o444, 0o644, 0o600):
            with self.subTest(mode=mode):
                Path(self.manifest).chmod(mode)
                self.assertEqual(self.verify(), 1)

    def test_wrong_pin_and_noncanonical_bytes(self):
        for raw, pin in ((RAW, 'a' * 64), (RAW + b'\n', hashlib.sha256(RAW + b'\n').hexdigest())):
            with self.subTest(pin=pin):
                with self.assertRaises(ValueError):
                    self.verify(raw=raw, pin=pin)

    def test_manifest_file_boundary(self):
        for mode in (0o666, 0o755, 0o000):
            with self.subTest(mode=mode):
                Path(self.manifest).chmod(mode)
                with self.assertRaises((ValueError, OSError)):
                    CHECK.read_public(self.manifest)
        self.write('profiles.json', RAW)
        link = self.root / 'link.json'
        link.symlink_to(self.manifest)
        for name in (str(link), str(self.root) + '/../' + self.root.name + '/profiles.json', 'profiles.json'):
            with self.subTest(name=name):
                with self.assertRaises((ValueError, OSError)):
                    CHECK.read_public(name)
        for raw in (b'', b'x' * (CHECK.LIMIT + 1)):
            self.write('profiles.json', raw)
            with self.assertRaises(ValueError):
                CHECK.read_public(self.manifest)

    def test_manifest_structure_and_ambiguity(self):
        manifest = json.loads(RAW)
        bads = []
        boolean_revision = copy.deepcopy(manifest); boolean_revision['revision'] = True; bads.append(boolean_revision)
        boolean_binding = copy.deepcopy(manifest); boolean_binding['profiles'][0]['binding']['revision'] = True; bads.append(boolean_binding)
        extra = copy.deepcopy(manifest); extra['extra'] = True; bads.append(extra)
        empty = copy.deepcopy(manifest); empty['profiles'] = []; bads.append(empty)
        duplicate = copy.deepcopy(manifest); duplicate['profiles'] *= 2; bads.append(duplicate)
        ambiguous = copy.deepcopy(manifest)
        second = copy.deepcopy(ambiguous['profiles'][0]); second['binding']['source_sha256'] = 'b' * 64
        ambiguous['profiles'].append(second); bads.append(ambiguous)
        many = copy.deepcopy(manifest); many['profiles'] *= 65; bads.append(many)
        order = copy.deepcopy(manifest)
        order['profiles'][0]['binding'] = dict(reversed(list(order['profiles'][0]['binding'].items())))
        bads.append(order)
        for value in bads:
            raw = CHECK.canonical(value)
            with self.subTest(value=list(value)):
                with self.assertRaises(ValueError):
                    self.verify(raw=raw, pin=hashlib.sha256(raw).hexdigest())
        raw = RAW.replace(b'"revision":1', b'"revision":1,"revision":1', 1)
        with self.assertRaises(ValueError):
            self.verify(raw=raw, pin=hashlib.sha256(raw).hexdigest())

    def test_selection_pin_and_phase_boundary(self):
        mutations = [
            lambda w: w['sandbox_runtimes'][1].update(language='python'),
            lambda w: w['sandbox_runtimes'][1].update(compiled_snapshot=None),
            lambda w: w['sandbox_runtimes'][1]['compiled_snapshot'].update(extra='bad'),
            lambda w: w['sandbox_runtimes'][1]['compiled_snapshot'].pop('profiles_sha256'),
            lambda w: w['sandbox_runtimes'][1]['compiled_snapshot'].update(profiles_sha256='b' * 64),
            lambda w: w['sandbox_runtimes'][1]['compiled_snapshot'].update(profiles_file='/elsewhere/profiles.json'),
            lambda w: w['sandbox_runtimes'][1]['compiled_snapshot'].update(dependency_bundle_sha256='B' * 64),
            lambda w: w['sandbox_runtimes'][0]['preparation'].update(compiled_snapshot=setting('/run/x')),
        ]
        for change in mutations:
            worker = copy.deepcopy(self.worker); change(worker)
            with self.assertRaises(ValueError):
                self.verify(worker=worker)
        self.write('worker.json', CHECK.canonical(self.worker))
        for key, value in [('purpose', 'preparation'), ('languages', ['rust', 'python']),
                           ('dependency_content', None), ('image_digest', 'sha256:' + 'b' * 64),
                           ('policy_revision', 'other')]:
            supervisor = copy.deepcopy(self.supervisor); supervisor[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.verify(supervisor=supervisor)

    def test_retained_bundle_platform(self):
        root = 'b' * 64
        manifest = json.loads(RAW); manifest['profiles'][0]['dependency_bundle_sha256'] = root
        raw = CHECK.canonical(manifest); pin = hashlib.sha256(raw).hexdigest()
        worker = copy.deepcopy(self.worker); supervisor = copy.deepcopy(self.supervisor)
        for target in (worker['sandbox_runtimes'][1], supervisor):
            target['compiled_snapshot']['profiles_sha256'] = pin
            target['compiled_snapshot']['dependency_bundle_sha256'] = root
        platform = dict(os='linux', arch='arm64', abi='gnu')
        supervisor['native_platform'] = platform
        worker['sandbox_runtimes'][1]['preparation'] = {'native_platform': platform}
        self.assertEqual(self.verify(worker=worker, supervisor=supervisor, raw=raw, pin=pin), 1)
        worker['sandbox_runtimes'][1]['preparation']['native_platform'] = dict(os='linux', arch='amd64', abi='gnu')
        with self.assertRaises(ValueError):
            self.verify(worker=worker, supervisor=supervisor, raw=raw, pin=pin)

    def test_quota_bounds(self):
        env = {'ELITEA_RUST_COMPILED_' + key: str(min(limit, 10)) for key, limit in CHECK.QUOTAS.items()}
        CHECK.quotas(env)
        for key, value in [('GLOBAL_ENTRIES', '100001'), ('GLOBAL_BYTES', '1099511627777'),
                           ('TENANT_ENTRIES', '11'), ('TENANT_BYTES', '11'),
                           ('PUBLISHING_TTL_SECONDS', '301'), ('READY_TTL_SECONDS', '86401'),
                           ('GLOBAL_ENTRIES', '010'), ('GLOBAL_BYTES', '1e9')]:
            bad = dict(env); bad['ELITEA_RUST_COMPILED_' + key] = value
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                CHECK.quotas(bad)


class Helm(unittest.TestCase):
    def render(self, value):
        with tempfile.TemporaryDirectory(prefix='compiled-render-') as directory:
            filename = Path(directory) / 'values.yaml'; filename.write_text(yaml.safe_dump(value))
            result = subprocess.run([
                'helm', 'template', 'compiled', str(CHART), '-f', str(CHART / 'values-standalone.yaml'),
                '-f', str(filename), '--namespace', 'platform',
                '--set', 'llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1',
                '--set', 'llmGateway.egressPosture=public-unrestricted',
            ], env=SAFE_ENV, capture_output=True, text=True, timeout=30)
        return result

    def test_enabled_material_and_identity(self):
        v = values(); result = self.render(v)
        self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d]
        by_name = {(d['kind'], d['metadata']['name']): d for d in docs}
        main = by_name['ConfigMap', 'elitea-main-config']['data']
        self.assertEqual(main['ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_PROFILES_SHA256'], PIN)
        self.assertEqual(main['ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_PROFILES_FILE'], '/run/elitea-runtime/rust-compiled-profiles.json')
        self.assertEqual(main['ELITEA_RUST_COMPILED_AGENTSTATE_DSN_FILE'], '/run/elitea-runtime/agent-checkpoint-connection')
        for field, suffix in [('globalEntries', 'GLOBAL_ENTRIES'), ('globalBytes', 'GLOBAL_BYTES'),
                              ('tenantEntries', 'TENANT_ENTRIES'), ('tenantBytes', 'TENANT_BYTES'),
                              ('publishingTtlSeconds', 'PUBLISHING_TTL_SECONDS'), ('readyTtlSeconds', 'READY_TTL_SECONDS')]:
            self.assertEqual(main['ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_' + suffix], QUOTAS[field])
        runtime = json.loads(by_name['ConfigMap', 'elitea-worker-runtime']['data']['runtime.json'])
        self.assertEqual(runtime['sandbox_runtimes'], v['worker']['runtime']['sandboxRuntimes'])
        worker = by_name['Deployment', 'elitea-worker']['spec']['template']['spec']
        files = worker['initContainers'][0]['env'][0]['value'].split()
        self.assertEqual(files.count('rust-compiled-profiles.json'), 1)
        mount = next(m for m in worker['containers'][0]['volumeMounts'] if m['name'] == 'material')
        self.assertTrue(mount['readOnly'])
        supervisor = by_name['Deployment', 'elitea-sandbox-supervisor']['spec']['template']['spec']
        self.assertTrue(supervisor['containers'][0]['securityContext']['readOnlyRootFilesystem'])
        self.assertTrue(next(m for m in supervisor['containers'][0]['volumeMounts'] if m['name'] == 'private-material')['readOnly'])
        policies = [d for d in docs if d['kind'] == 'NetworkPolicy' and d['metadata']['namespace'] == 'code-execution']
        self.assertEqual(policies[0]['spec']['egress'], [])

    def test_default_off_preserves_existing_consumer_shape(self):
        v = values(); del v['main']['runtime']['rustCompiledSnapshots']
        del v['worker']['runtime']['sandboxRuntimes'][1]['compiled_snapshot']
        v['sandboxKubernetes']['supervisor']['profiles'][1] = dict(file='rust.json', port=9447)
        result = self.render(v); self.assertEqual(result.returncode, 0, result.stderr)
        docs = [d for d in yaml.safe_load_all(result.stdout) if d]
        main = next(d for d in docs if d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'elitea-main-config')
        self.assertFalse(any('COMPILED' in key for key in main['data']))
        worker = next(d for d in docs if d['kind'] == 'Deployment' and d['metadata']['name'] == 'elitea-worker')['spec']['template']['spec']
        files = worker['initContainers'][0]['env'][0]['value'].split()
        self.assertNotIn('rust-compiled-profiles.json', files)
        mount = next(m for m in worker['containers'][0]['volumeMounts'] if m['name'] == 'material')
        self.assertNotIn('readOnly', mount)

    def test_bounded_quota_defaults_render_canonical_integers(self):
        v = values()
        v['main']['runtime']['rustCompiledSnapshots'] = dict(enabled=True, profilesSha256=PIN)
        result = self.render(v)
        self.assertEqual(result.returncode, 0, result.stderr)
        main = next(d for d in yaml.safe_load_all(result.stdout)
                    if d and d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'elitea-main-config')['data']
        self.assertEqual(main['ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_GLOBAL_BYTES'], '1073741824')
        self.assertEqual(main['ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_TENANT_BYTES'], '268435456')

    def test_fixed_original_receipt_database_budget(self):
        v = values(); v['postgresql']['maxConnections'] = 200
        result = self.render(v)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('184 PostgreSQL connections', result.stderr)

    def test_pgbouncer_independent_receipt_pool_server_budget(self):
        v = values()
        v['pgbouncer'] = dict(enabled=True, poolSize=48, reservePoolSize=4, maxClientConn=1024,
                              postgresHost='postgres.synthetic', database='compiled_fixture',
                              credentialsSecretName='synthetic-pgbouncer')
        v['postgresql'] = dict(maxConnections=100, reservedConnections=25)
        v['main']['autoscaling'] = dict(enabled=True, maxReplicas=8)
        result = self.render(v)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('84 PostgreSQL server connections', result.stderr)
        self.assertIn('52 PgBouncer plus 32 independent Code receipt connections across 8 Main replicas', result.stderr)
        self.assertIn('only 75 are available', result.stderr)
        for maximum, pool_size, accepted in [(109, 48, True), (100, 39, True), (100, 40, False)]:
            with self.subTest(maximum=maximum, pool_size=pool_size):
                v['postgresql']['maxConnections'] = maximum
                v['pgbouncer']['poolSize'] = pool_size
                result = self.render(v)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)

    def test_pgbouncer_fixed_replica_receipt_pool_budget(self):
        v = values()
        v['pgbouncer'] = dict(enabled=True, poolSize=48, reservePoolSize=4, maxClientConn=1024,
                              postgresHost='postgres.synthetic', database='compiled_fixture',
                              credentialsSecretName='synthetic-pgbouncer')
        v['postgresql'] = dict(maxConnections=100, reservedConnections=25)
        v['main']['autoscaling'] = dict(enabled=False)
        for count, accepted in [(5, True), (6, False)]:
            with self.subTest(count=count):
                v['main']['replicaCount'] = count
                result = self.render(v)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)

    def test_pgbouncer_default_off_preserves_server_budget(self):
        v = values()
        v['pgbouncer'] = dict(enabled=True, poolSize=48, reservePoolSize=4, maxClientConn=1024,
                              postgresHost='postgres.synthetic', database='compiled_fixture',
                              credentialsSecretName='synthetic-pgbouncer')
        v['postgresql'] = dict(maxConnections=100, reservedConnections=25)
        v['main']['autoscaling'] = dict(enabled=True, maxReplicas=8)
        del v['main']['runtime']['rustCompiledSnapshots']
        del v['worker']['runtime']['sandboxRuntimes'][1]['compiled_snapshot']
        v['sandboxKubernetes']['supervisor']['profiles'][1] = dict(file='rust.json', port=9447)
        result = self.render(v)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_retained_cargo_platform_matches(self):
        v = values(); root = 'b' * 64; platform = dict(os='linux', arch='arm64', abi='gnu')
        worker = v['worker']['runtime']['sandboxRuntimes'][1]
        supervisor = v['sandboxKubernetes']['supervisor']['profiles'][1]
        worker['compiled_snapshot']['dependency_bundle_sha256'] = root
        supervisor['compiled_snapshot']['dependency_bundle_sha256'] = root
        worker['preparation'] = dict(native_platform=platform, target='rust-prep:9452',
                                    audience='dns:rust-prep', image_digest=IMAGE,
                                    policy_revision='rust-preparation-v2', timeout_seconds=600)
        supervisor['native_platform'] = platform
        result = self.render(v); self.assertEqual(result.returncode, 0, result.stderr)
        supervisor['native_platform'] = dict(os='linux', arch='amd64', abi='gnu')
        result = self.render(v); self.assertNotEqual(result.returncode, 0)
        self.assertIn('platforms differ', result.stderr)
        worker['preparation'].pop('native_platform')
        result = self.render(v); self.assertNotEqual(result.returncode, 0)

    def test_invalid_opt_in_settings_fail_render(self):
        mutations = [
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(enabled=False),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(enabled='true'),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].pop('profilesSha256'),
            lambda v: v['main']['runtime'].update(sandboxAudiences=[]),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(profilesSha256='A' * 64),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(extra=True),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(globalEntries='100001'),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(globalEntries=1.5),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(globalEntries=True),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(globalEntries='010'),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(globalBytes='999999999999999999999999'),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(tenantBytes='1073741825'),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(publishingTtlSeconds='301'),
            lambda v: v['main']['runtime']['rustCompiledSnapshots'].update(readyTtlSeconds='86401'),
            lambda v: v['main'].setdefault('env', {}).update(ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_ENABLED='true'),
            lambda v: v['worker'].update(implementation='python'),
            lambda v: v['worker']['runtime']['sandboxRuntimes'][1].update(language='python'),
            lambda v: v['worker']['runtime']['sandboxRuntimes'][1].update(compiled_snapshot=None),
            lambda v: v['worker']['runtime']['sandboxRuntimes'][1].pop('compiled_snapshot'),
            lambda v: v['worker']['runtime']['sandboxRuntimes'][1]['compiled_snapshot'].pop('profiles_file'),
            lambda v: v['worker']['runtime']['sandboxRuntimes'][1]['compiled_snapshot'].update(extra='bad'),
            lambda v: v['worker']['runtime']['sandboxRuntimes'][1]['compiled_snapshot'].update(profiles_sha256='b' * 64),
            lambda v: v['worker']['runtime']['sandboxRuntimes'][1]['compiled_snapshot'].update(profiles_file='/wrong/file'),
            lambda v: v['worker']['runtime']['sandboxRuntimes'][0]['preparation'].update(compiled_snapshot=setting('/run/x')),
            lambda v: v['sandboxKubernetes']['supervisor']['profiles'][1].update(purpose='preparation'),
            lambda v: v['sandboxKubernetes']['supervisor']['profiles'][1].update(languages=['python']),
            lambda v: v['sandboxKubernetes']['supervisor']['profiles'][1].update(dependencyContentEnabled=False),
            lambda v: v['sandboxKubernetes']['supervisor']['profiles'][1].update(image_digest='sha256:' + 'b' * 64),
            lambda v: v['sandboxKubernetes']['supervisor']['profiles'][1].update(policy_revision='other'),
            lambda v: v['sandboxKubernetes']['supervisor']['profiles'][1]['compiled_snapshot'].update(dependency_bundle_sha256='b' * 64),
        ]
        for index, change in enumerate(mutations):
            v = values(); change(v)
            with self.subTest(case=index):
                result = self.render(v)
                self.assertNotEqual(result.returncode, 0, 'invalid compiled deployment rendered')


class Compose(unittest.TestCase):
    def render(self, enabled, missing_pin=False):
        files = ['standalone-full', 'standalone-rust-agent', 'sandbox']
        if enabled:
            files.append('sandbox-compiled-rust')
        env = dict(SAFE_ENV, ELITEA_SANDBOX_SUPERVISOR_IMAGE='local/supervisor@sha256:' + 'a' * 64,
                   ELITEA_SANDBOX_DOCKER_GID='999', ELITEA_SANDBOX_WORKER_CONFIG='/private/synthetic/worker.json',
                   ELITEA_SANDBOX_DENO_MATERIAL='/private/synthetic/deno', ELITEA_SANDBOX_RUST_MATERIAL='/private/synthetic/rust',
                   ELITEA_SANDBOX_DOCKER_SOCKET='/private/synthetic/docker.sock',
                   ELITEA_RUST_COMPILED_PROFILES_FILE='/private/synthetic/release$immutable.json',
                   ELITEA_RUST_COMPILED_PROFILES_SHA256=PIN,
                   ELITEA_RUST_COMPILED_GLOBAL_ENTRIES='100', ELITEA_RUST_COMPILED_GLOBAL_BYTES='1073741824',
                   ELITEA_RUST_COMPILED_TENANT_ENTRIES='10', ELITEA_RUST_COMPILED_TENANT_BYTES='268435456',
                   ELITEA_RUST_COMPILED_PUBLISHING_TTL_SECONDS='60', ELITEA_RUST_COMPILED_READY_TTL_SECONDS='3600')
        if missing_pin:
            env.pop('ELITEA_RUST_COMPILED_PROFILES_SHA256')
        command = ['docker', 'compose', '--env-file', '/dev/null', '--project-name', 'compiled-test']
        for name in files:
            command.extend(['-f', str(ROOT / ('deploy/docker-compose.' + name + '.yml'))])
        command.extend(['config', '--format', 'json'])
        return subprocess.run(command, env=env, capture_output=True, text=True, timeout=30)

    def test_opt_in_preserves_base_and_readonly_paths(self):
        before = self.render(False); after = self.render(True)
        self.assertEqual(before.returncode, 0, before.stderr)
        self.assertEqual(after.returncode, 0, after.stderr)
        old, new = json.loads(before.stdout)['services'], json.loads(after.stdout)['services']
        self.assertFalse(any('COMPILED' in key for key in old['elitea-main']['environment']))
        self.assertEqual(new['elitea-main']['environment']['ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_PROFILES_SHA256'], PIN)
        for name in old:
            for key in ('command', 'user', 'read_only', 'cap_drop', 'security_opt', 'networks'):
                self.assertEqual(old[name].get(key), new[name].get(key), (name, key))
        for name in ('elitea-main', 'elitea-worker'):
            self.assertEqual(old[name]['volumes'], new[name]['volumes'])
        for name, target in [('runtime-material', '/src/rust-compiled-profiles.json'),
                             ('elitea-sandbox', '/run/elitea-sandbox/rust/rust-compiled-profiles.json')]:
            mount = next(m for m in new[name]['volumes'] if m['target'] == target)
            self.assertTrue(mount['read_only'])
            self.assertFalse(mount['bind']['create_host_path'])
            self.assertEqual(mount['source'].replace('$$', '$'), '/private/synthetic/release$immutable.json')

    def test_compose_export_roundtrip_keeps_dollars(self):
        result = self.render(True)
        self.assertEqual(result.returncode, 0, result.stderr)
        with tempfile.TemporaryDirectory(prefix='compiled-compose-') as directory:
            filename = Path(directory) / 'exported.json'
            filename.write_text(result.stdout)
            roundtrip = subprocess.run(['docker', 'compose', '--env-file', '/dev/null',
                                        '-f', str(filename), 'config', '--format', 'json'],
                                       env=SAFE_ENV, capture_output=True, text=True, timeout=30)
        self.assertEqual(roundtrip.returncode, 0, roundtrip.stderr)
        self.assertEqual(json.loads(roundtrip.stdout)['services'], json.loads(result.stdout)['services'])

    def test_opt_in_requires_pin(self):
        result = self.render(True, missing_pin=True)
        self.assertNotEqual(result.returncode, 0)


class Installer(unittest.TestCase):
    def run_installer(self, enabled, pin=PIN):
        directory = tempfile.TemporaryDirectory(prefix='compiled-installer-')
        self.addCleanup(directory.cleanup)
        root = Path(directory.name).resolve()
        source = root / 'src'; source.mkdir()
        names = (
            'runtime-ca.crt control-server.crt output-server.crt content-server.crt '
            'command-signing-keyring.json control-server.key output-server.key content-server.key '
            'command-signing-key.pem redis-producer-password redis-auth-password auth-attempt-key '
            'auth-pat-signing-key auth-form-users.json vault-master-key redis-server.crt redis-server.key '
            'redis-users.acl redis-bootstrap-password agent-worker-client.crt agent-worker-client.key '
            'redis-worker-password worker-output-spool-key agent-checkpoint-connection '
            'platform-edge.crt platform-edge.key'
        ).split()
        for name in names:
            (source / name).write_bytes(b'synthetic-test-material')
            (source / name).chmod(0o600)
        (source / 'rust-compiled-profiles.json').write_bytes(RAW)
        bin_dir = root / 'bin'; bin_dir.mkdir()
        chown = bin_dir / 'chown'
        chown.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$CHOWN_LOG"\n')
        chown.chmod(0o700)
        script = (ROOT / 'deploy/runtime/install-material.sh').read_text()
        script = script.replace('SRC=/src', 'SRC=' + str(source)).replace('/dst/', str(root / 'dst') + '/')
        filename = root / 'install.sh'; filename.write_text(script)
        env = dict(SAFE_ENV, PATH=str(bin_dir) + ':' + SAFE_ENV['PATH'], CHOWN_LOG=str(root / 'chown.log'))
        if enabled is not None:
            env['ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_ENABLED'] = enabled
        if pin is not None:
            env['ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_PROFILES_SHA256'] = pin
        result = subprocess.run(['/bin/sh', str(filename)], env=env, capture_output=True, text=True, timeout=10)
        return root, result

    def test_default_off_copies_no_new_main_authority(self):
        root, result = self.run_installer(None, None)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((root / 'dst/main/agent-checkpoint-connection').exists())
        self.assertFalse((root / 'dst/main/rust-compiled-profiles.json').exists())
        self.assertFalse((root / 'dst/worker/rust-compiled-profiles.json').exists())

    def test_enabled_copy_preserves_exact_pin_and_private_dsn(self):
        root, result = self.run_installer('true')
        self.assertEqual(result.returncode, 0, result.stderr)
        main = root / 'dst/main'; worker = root / 'dst/worker'
        for file in (main / 'rust-compiled-profiles.json', worker / 'rust-compiled-profiles.json'):
            self.assertEqual(file.read_bytes(), RAW)
            self.assertEqual(hashlib.sha256(file.read_bytes()).hexdigest(), PIN)
        self.assertEqual((main / 'rust-compiled-profiles.json').stat().st_mode & 0o777, 0o644)
        self.assertEqual((worker / 'rust-compiled-profiles.json').stat().st_mode & 0o777, 0o600)
        self.assertEqual((main / 'agent-checkpoint-connection').stat().st_mode & 0o777, 0o600)
        log = (root / 'chown.log').read_text()
        self.assertIn('65532:65532 ' + str(main / 'agent-checkpoint-connection'), log)
        self.assertIn('10001:10001 ' + str(worker / 'rust-compiled-profiles.json'), log)
        self.assertFalse((worker / 'command-signing-key.pem').exists())
        self.assertFalse((main / 'agent-worker-client.key').exists())

    def test_invalid_opt_in_stops_before_copy(self):
        for enabled, pin in [('true', None), ('true', 'b' * 64), ('yes', PIN), ('false', PIN)]:
            with self.subTest(enabled=enabled, pin=pin):
                root, result = self.run_installer(enabled, pin)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((root / 'dst').exists())


if __name__ == '__main__':
    unittest.main(verbosity=2)
