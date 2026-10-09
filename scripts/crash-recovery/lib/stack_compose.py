"""Bring-up and identity of the dedicated compose crash stack (DESIGN §3.4).

Everything private (stack.env values, the product dump, certificates, keys, rendered configs) lives in a local
stack directory outside the repository, mode 700. The repository holds only the overlay and templates.
"""
import json
import os
import pathlib
import re
import shutil
import stat
import subprocess

from .common import REPO, HarnessError, read_json, run, write_json

DEFAULT_STACK_DIR = pathlib.Path(os.environ.get(
    'CRASH_STACK_DIR', '/Users/romanmitusov/Documents/Work/.claude/handoffs/crash-recovery-suite/stack'))
# Only these keys are taken from the real-model stack.env. Its STANDALONE_PROJECT and ports belong to the
# shared verify stack and must never leak into the crash stack's environment.
STACK_ENV_KEYS = ('SECRETS_MASTER_KEY', 'GATEWAY_EGRESS_ALLOWLIST', 'ELITEA_INITIAL_GLOBAL_ADMINS')
MAIN_OWNER_IDENTITY = 'dns:elitea-main'
PROJECT_RE = re.compile(r'^elitea-crash(-[a-z0-9]+)?$')
HOST_RE = re.compile(r'^[a-z0-9-]+\.localhost$')
PORT_KEYS = {
    'web': 'STANDALONE_PORT', 'pg': 'STANDALONE_PG_PORT', 'gateway': 'STANDALONE_GATEWAY_PORT',
    'mock': 'STANDALONE_MOCK_PORT', 'qtest': 'STANDALONE_QTEST_PORT', 'ado': 'STANDALONE_ADO_PORT',
    'otel_grpc': 'STANDALONE_OTEL_GRPC_PORT', 'otel_http': 'STANDALONE_OTEL_HTTP_PORT', 'oidc': 'E2E_OIDC_PORT',
}
BASE_FILES = ('deploy/docker-compose.standalone-full.yml', 'deploy/docker-compose.standalone-rust-agent.yml')
# Opt-in features (stack.json "features") add their overlays at fixed positions (see each overlay's header).
PREPARATION_NETWORK = 'elitea-python-preparation-resolver'
PREPARATION_AUDIENCE = 'dns:elitea-sandbox-preparation'
CONTENT_ORIGIN = 'https://elitea-main:9445'
# The services the fault catalog targets (DESIGN §1.1).
TARGETS = {
    'main': 'elitea-main', 'worker': 'elitea-worker', 'supervisor': 'elitea-sandbox', 'nats': 'nats',
    'postgres': 'postgres', 'pgbouncer': 'pgbouncer', 'gateway': 'elitea-llm-gateway',
}
CONFIG_TEMPLATE = {
    'project': 'elitea-crash',
    'host': 'crash.localhost',
    'ports': {'web': 18140, 'pg': 18141, 'gateway': 18142, 'mock': 18143, 'qtest': 18144, 'ado': 18145,
              'otel_grpc': 18146, 'otel_http': 18147, 'oidc': 19540},
    'stack_env': '/Users/romanmitusov/Documents/Work/.claude/handoffs/real-model-stack/stack.env',
    'dump': '/Users/romanmitusov/Documents/Work/.claude/handoffs/real-model-stack/product-real-models-main-c0f2e5f9b.dump',
    'source_commit': '58abb650c',
    'images': {},
    'sandbox': {'supervisor_image': '', 'deno_image': '', 'rust_image': '', 'timeout_seconds': 180,
                'memory_bytes': 536870912, 'cpu_limit': 1.0, 'concurrency': 4,
                'deno_policy_revision': 'crash-deno-v1', 'rust_policy_revision': 'crash-rust-v1'},
    'docker_socket': '/var/run/docker.sock',
    'docker_gid': '0',
    'features': {'preparation': False, 'compiled': False},
    'oidc_subject': 'admin@centry.user',
}


class Stack:
    def __init__(self, stack_dir=None):
        self.dir = pathlib.Path(stack_dir or DEFAULT_STACK_DIR)
        self.config_path = self.dir / 'stack.json'
        self.state_path = self.dir / 'state.json'
        cfg = read_json(self.config_path)
        if cfg is None:
            raise HarnessError(f'no stack config at {self.config_path}; run `crashctl init`')
        if not PROJECT_RE.match(cfg['project']):
            raise HarnessError('the crash stack project name must be elitea-crash[-suffix]')
        if not HOST_RE.match(cfg['host']):
            raise HarnessError('the crash stack host must be <name>.localhost')
        self.cfg = cfg
        self.project = cfg['project']
        self.material = self.dir / 'material'

    # ---- configuration -------------------------------------------------------------------------------------
    @staticmethod
    def init(stack_dir=None):
        root = pathlib.Path(stack_dir or DEFAULT_STACK_DIR)
        root.mkdir(parents=True, exist_ok=True)
        os.chmod(root, 0o700)
        path = root / 'stack.json'
        if not path.exists():
            write_json(path, CONFIG_TEMPLATE)
            os.chmod(path, 0o600)
        return path

    @property
    def base_url(self):
        return f"http://{self.cfg['host']}:{self.cfg['ports']['web']}"

    def state(self):
        return read_json(self.state_path, {'spool_name': f'{self.project}-worker-spool-0', 'spool_generation': 0})

    def save_state(self, value):
        write_json(self.state_path, value)

    def _stack_env(self):
        values = {}
        path = pathlib.Path(self.cfg['stack_env'])
        for line in path.read_text().splitlines():
            key, sep, value = line.partition('=')
            if sep and key.strip() in STACK_ENV_KEYS:
                values[key.strip()] = value.strip()
        return values

    def env(self, extra=None):
        env = dict(os.environ)
        env.update(self._stack_env())
        ports = self.cfg['ports']
        env.update({PORT_KEYS[k]: str(v) for k, v in ports.items()})
        sandbox = self.cfg['sandbox']
        state = self.state()
        env.update({
            'STANDALONE_PROJECT': self.project,
            'STANDALONE_HOST': self.cfg['host'],
            'STANDALONE_WORKER': 'rust',
            'STANDALONE_OVERLAY': ' '.join(str(p) for p in self.overlay_files()),
            'ELITEA_SANDBOX_SUPERVISOR_IMAGE': sandbox['supervisor_image'],
            'ELITEA_SANDBOX_DOCKER_SOCKET': self.cfg['docker_socket'],
            'ELITEA_SANDBOX_DOCKER_GID': str(self.cfg['docker_gid']),
            # Required by docker-compose.sandbox.yml interpolation; the crash overlay replaces these binds
            # with volumes filled by crash-material.
            'ELITEA_SANDBOX_DENO_MATERIAL': str(self.material / 'sandbox-deno'),
            'ELITEA_SANDBOX_RUST_MATERIAL': str(self.material / 'sandbox-rust'),
            'ELITEA_SANDBOX_WORKER_CONFIG': str(self.material / 'worker-runtime.json'),
            'ELITEA_CRASH_MATERIAL_DIR': str(self.material),
            'ELITEA_CRASH_SPOOL_NAME': state['spool_name'],
            'ELITEA_CRASH_CODE_OWNER_CONFIG': self.owner_config(),
            'ELITEA_SANDBOX_PREPARATION_MATERIAL': str(self.material / 'sandbox-preparation'),
        })
        if state.get('compiled_profiles_sha256'):
            env['ELITEA_RUST_COMPILED_PROFILES_SHA256'] = state['compiled_profiles_sha256']
        env.update(self.cfg.get('mock_env', {}))
        env.update(extra or {})
        return env

    def feature(self, name):
        return bool(self.cfg.get('features', {}).get(name))

    def overlay_files(self):
        files = ['deploy/docker-compose.sandbox.yml']
        if self.feature('preparation'):
            files.append('deploy/docker-compose.sandbox-preparation.yml')
        files.append('deploy/docker-compose.crash-rehearsal.yml')
        if self.feature('preparation'):
            files.append('deploy/docker-compose.crash-preparation.yml')
        if self.feature('compiled'):
            files.append('deploy/docker-compose.crash-compiled.yml')
        return [REPO / f for f in files] + [self.dir / 'images.yml', self.dir / 'ports.yml']

    def compose_args(self):
        args = ['docker', 'compose', '-p', self.project]
        for f in BASE_FILES:
            args += ['-f', str(REPO / f)]
        for f in self.overlay_files():
            args += ['-f', str(f)]
        return args

    def compose(self, *args, timeout=300, check=True, extra_env=None):
        return run(self.compose_args() + list(args), timeout=timeout, env=self.env(extra_env), check=check, cwd=REPO)

    # ---- material ------------------------------------------------------------------------------------------
    def prepare_material(self, python_with_cryptography):
        """Create every private file the overlay needs. Idempotent; never rotates existing keys."""
        certs = REPO / 'deploy/certs/runtime'
        if not (certs / 'runtime-ca.crt').exists():
            env = dict(os.environ, PATH=f'{pathlib.Path(python_with_cryptography).parent}:{os.environ["PATH"]}')
            run(['bash', 'deploy/scripts/standalone-stack.sh', 'certs'], cwd=REPO, env=env, timeout=600)
        self.material.mkdir(parents=True, exist_ok=True)
        os.chmod(self.material, 0o700)
        issued = self.dir / 'sandbox-certs'
        # --native issues the preparation server and every content-client leaf (deno, preparation, rust).
        run(['bash', 'deploy/scripts/gen-sandbox-certs.sh', str(certs), str(issued), '--native'], cwd=REPO, timeout=180)
        sandbox = self.cfg['sandbox']
        digests = {k: image_id(sandbox[f'{k}_image']) for k in ('deno', 'rust')}
        profiles = {
            'deno': {'port': 9446, 'languages': ['python', 'javascript', 'typescript']},
            'rust': {'port': 9447, 'languages': ['rust']},
        }
        db_url = 'postgresql://elitea:elitea@postgres:5432/agentstate?sslmode=verify-full'
        compiled_pin = compiled_path = None
        if self.feature('compiled'):
            from . import compiled_manifest
            raw = compiled_manifest.build(sandbox['rust_image'], sandbox['rust_policy_revision'])
            compiled_path = self.material / 'rust-compiled-profiles.json'
            compiled_pin = compiled_manifest.write(compiled_path, raw)
            # The stock installer reads it from deploy/certs/runtime (no nested bind, see crash-compiled overlay).
            compiled_manifest.write(certs / 'rust-compiled-profiles.json', raw)
            state = self.state()
            state['compiled_profiles_sha256'] = compiled_pin
            self.save_state(state)
        for name, profile in profiles.items():
            out = self.material / f'sandbox-{name}'
            out.mkdir(exist_ok=True)
            os.chmod(out, 0o700)
            base = f'/run/elitea-sandbox/{name}'
            _copy_private(certs / 'runtime-ca.crt', out / 'client-ca.pem')
            _copy_private(certs / 'runtime-ca.crt', out / 'database-ca.pem')
            _copy_private(certs / 'command-signing-keyring.json', out / 'command-signing-keyring.json')
            _copy_private(issued / f'elitea-sandbox-{name}.crt', out / 'server.pem')
            _copy_private(issued / f'elitea-sandbox-{name}.key', out / 'server.key')
            _write_private(out / 'database-url', db_url)
            config = {
                'revision': 1, 'purpose': 'execution', 'listen_address': f"0.0.0.0:{profile['port']}",
                'owner': f'{self.project}-sandbox-{name}', 'audience': f'dns:elitea-sandbox-{name}',
                'ca_path': f'{base}/client-ca.pem', 'certificate_path': f'{base}/server.pem',
                'private_key_path': f'{base}/server.key',
                'verification_keyring_path': f'{base}/command-signing-keyring.json',
                'database_url_path': f'{base}/database-url', 'database_ca_path': f'{base}/database-ca.pem',
                'database_connections': 4, 'image_digest': digests[name],
                'policy_revision': sandbox[f'{name}_policy_revision'], 'languages': profile['languages'],
                'concurrency': sandbox['concurrency'], 'memory_bytes': sandbox['memory_bytes'],
                'cpu_limit': sandbox['cpu_limit'], 'timeout_seconds': sandbox['timeout_seconds'],
                'code_owner_requester': MAIN_OWNER_IDENTITY,
            }
            # Hydration (Deno) and compiled-executable transfer (Rust) move content through Main's :9445 listener.
            if (name == 'deno' and self.feature('preparation')) or (name == 'rust' and self.feature('compiled')):
                config['dependency_content'] = self._content_client(certs, issued, out, base, name)
            if name == 'rust' and self.feature('compiled'):
                config['compiled_snapshot'] = {'profiles_file': f'{base}/rust-compiled-profiles.json',
                                               'profiles_sha256': compiled_pin, 'dependency_bundle_sha256': ''}
                _copy_private(compiled_path, out / 'rust-compiled-profiles.json')
            _write_private(out / 'config.json', json.dumps(config, indent=2) + '\n')
        if self.feature('preparation'):
            self._preparation_profile(certs, issued, digests['deno'], db_url)
        self._issue_owner_certificate(certs)
        pg = self.material / 'postgres'
        pg.mkdir(exist_ok=True)
        os.chmod(pg, 0o700)
        _copy_private(issued / 'postgres.crt', pg / 'server.crt')
        _copy_private(issued / 'postgres.key', pg / 'server.key')
        worker = json.loads((REPO / 'deploy/runtime/worker-runtime.crash.json').read_text())
        worker['sandbox_runtimes'] = [
            {'language': lang, 'target': f"elitea-sandbox-{name}:{profiles[name]['port']}",
             'audience': f'dns:elitea-sandbox-{name}', 'image_digest': digests[name],
             'policy_revision': sandbox[f'{name}_policy_revision'], 'timeout_seconds': sandbox['timeout_seconds']}
            for name in ('deno', 'rust') for lang in profiles[name]['languages']]
        for entry in worker['sandbox_runtimes']:
            if entry['language'] == 'python' and self.feature('preparation'):
                entry['preparation'] = {'target': 'elitea-sandbox-preparation:9448', 'audience': PREPARATION_AUDIENCE,
                                        'image_digest': digests['deno'],
                                        'policy_revision': sandbox.get('preparation_policy_revision', 'crash-preparation-v1'),
                                        'timeout_seconds': sandbox.get('preparation_timeout_seconds', 120)}
            if entry['language'] == 'rust' and self.feature('compiled'):
                entry['compiled_snapshot'] = {'profiles_file': '/run/elitea-runtime/rust-compiled-profiles.json',
                                              'profiles_sha256': compiled_pin, 'dependency_bundle_sha256': ''}
        path = self.material / 'worker-runtime.json'
        path.write_text(json.dumps(worker, indent=2) + '\n')
        os.chmod(path, 0o644)
        # Stock-Helm equivalent (D1): both recovery flags off, as deploy/helm/elitea/values.yaml defaults them.
        stock = {k: v for k, v in worker.items() if k not in ('agent_model_checkpoint_recovery', 'agent_node_recovery')}
        for entry in stock['sandbox_runtimes']:
            entry.pop('preparation', None)
        stock_path = self.material / 'worker-runtime.stock.json'
        stock_path.write_text(json.dumps(stock, indent=2) + '\n')
        os.chmod(stock_path, 0o644)
        self._write_overrides()
        return {'deno_image_digest': digests['deno'], 'rust_image_digest': digests['rust']}

    def _content_client(self, certs, issued, out, base, name):
        """Dependency-content client (dns:elitea-sandbox-<name>, clientAuth) towards Main's content listener."""
        _copy_private(certs / 'runtime-ca.crt', out / 'content-ca.pem')
        _copy_private(issued / f'elitea-sandbox-{name}-content-client.crt', out / 'content-client.pem')
        _copy_private(issued / f'elitea-sandbox-{name}-content-client.key', out / 'content-client.key')
        return {'origin': CONTENT_ORIGIN, 'ca_path': f'{base}/content-ca.pem',
                'certificate_path': f'{base}/content-client.pem', 'private_key_path': f'{base}/content-client.key',
                'staging_root': f'/run/elitea-sandbox-content/{name}', 'capacity': 1, 'timeout_seconds': 30}

    def _preparation_profile(self, certs, issued, image_digest, db_url):
        """Python dependency preparation on the existing Deno runner image (purpose: preparation, port 9448)."""
        sandbox = self.cfg['sandbox']
        out = self.material / 'sandbox-preparation'
        out.mkdir(exist_ok=True)
        os.chmod(out, 0o700)
        base = '/run/elitea-sandbox/preparation'
        _copy_private(certs / 'runtime-ca.crt', out / 'client-ca.pem')
        _copy_private(certs / 'runtime-ca.crt', out / 'database-ca.pem')
        _copy_private(certs / 'command-signing-keyring.json', out / 'command-signing-keyring.json')
        _copy_private(issued / 'elitea-sandbox-preparation.crt', out / 'server.pem')
        _copy_private(issued / 'elitea-sandbox-preparation.key', out / 'server.key')
        _write_private(out / 'database-url', db_url)
        config = {
            'revision': 1, 'purpose': 'preparation', 'backend': {'kind': 'docker'},
            'preparation_network': PREPARATION_NETWORK, 'listen_address': '0.0.0.0:9448',
            'owner': f'{self.project}-sandbox-preparation', 'audience': PREPARATION_AUDIENCE,
            'ca_path': f'{base}/client-ca.pem', 'certificate_path': f'{base}/server.pem',
            'private_key_path': f'{base}/server.key', 'verification_keyring_path': f'{base}/command-signing-keyring.json',
            'database_url_path': f'{base}/database-url', 'database_ca_path': f'{base}/database-ca.pem',
            'database_connections': 2, 'image_digest': image_digest,
            'policy_revision': sandbox.get('preparation_policy_revision', 'crash-preparation-v1'),
            'languages': ['python'], 'concurrency': 1,
            'memory_bytes': sandbox.get('preparation_memory_bytes', 1073741824), 'cpu_limit': 1.0,
            'timeout_seconds': sandbox.get('preparation_timeout_seconds', 120),
            'dependency_content': self._content_client(certs, issued, out, base, 'preparation'),
        }
        _write_private(out / 'config.json', json.dumps(config, indent=2) + '\n')

    def _issue_owner_certificate(self, certs):
        """Main's original-Code owner client identity (dns:elitea-main, clientAuth), issued by the runtime CA.
        No repository script issues it; the material installer only copies it (install-material.sh:49-55)."""
        out = self.material / 'code-owner'
        out.mkdir(exist_ok=True)
        os.chmod(out, 0o700)
        crt, key = out / 'code-owner-client.crt', out / 'code-owner-client.key'
        if crt.exists() and key.exists():
            if not (certs / 'code-owner-client.crt').exists():
                _copy_private(key, certs / 'code-owner-client.key')
                shutil.copyfile(crt, certs / 'code-owner-client.crt')
            return
        ext = out / 'extensions.cnf'
        ext.write_text('basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n'
                       'extendedKeyUsage=clientAuth\nsubjectAltName=DNS:elitea-main\n')
        csr = out / 'request.csr'
        run(['openssl', 'req', '-new', '-newkey', 'rsa:2048', '-nodes', '-keyout', str(key), '-out', str(csr),
             '-subj', '/CN=elitea-main'], timeout=60)
        run(['openssl', 'x509', '-req', '-in', str(csr), '-CA', str(certs / 'runtime-ca.crt'), '-CAkey',
             str(certs / 'runtime-ca.key'), '-set_serial', '0x' + os.urandom(16).hex(), '-days', '30', '-sha256',
             '-extfile', str(ext), '-out', str(crt)], timeout=60)
        csr.unlink()
        ext.unlink()
        os.chmod(key, 0o600)
        os.chmod(crt, 0o644)
        # runtime-material reads deploy/certs/runtime (gitignored); a nested file bind into that read-only
        # directory cannot be created, so the files are placed beside the other runtime material.
        _copy_private(key, certs / 'code-owner-client.key')
        shutil.copyfile(crt, certs / 'code-owner-client.crt')

    def owner_config(self):
        return json.dumps({
            'main_workload_identity': MAIN_OWNER_IDENTITY,
            'certificate_chain_path': '/run/elitea-runtime/code-owner-client.crt',
            'private_key_path': '/run/elitea-runtime/code-owner-client.key',
            'server_ca_path': '/run/elitea-runtime/runtime-ca.crt',
            'supervisors': [{'audience': 'dns:elitea-sandbox-deno', 'https_origin': 'https://elitea-sandbox-deno:9446'},
                            {'audience': 'dns:elitea-sandbox-rust', 'https_origin': 'https://elitea-sandbox-rust:9447'}],
        }, separators=(',', ':'))

    def _write_overrides(self):
        lines = ['services:']
        for service, image in sorted(self.cfg['images'].items()):
            lines.append(f'  {service}: {{image: "{image}"}}')
        (self.dir / 'images.yml').write_text('\n'.join(lines) + '\n')
        ports = self.cfg['ports']
        published = {
            'postgres': [f"127.0.0.1:{ports['pg']}:5432"],
            'oidc-mock': [f"127.0.0.1:{ports['oidc']}:{ports['oidc']}"],
            'elitea-llm-gateway': [f"127.0.0.1:{ports['gateway']}:8083"],
            'llm-mock': [f"127.0.0.1:{ports['mock']}:8090"],
            'otel-collector': [f"127.0.0.1:{ports['otel_grpc']}:4317", f"127.0.0.1:{ports['otel_http']}:4318"],
            'traefik': [f"127.0.0.1:{ports['web']}:80"],
        }
        lines = ['services:'] + [f'  {svc}: {{ports: !override {json.dumps(p)}}}' for svc, p in published.items()]
        (self.dir / 'ports.yml').write_text('\n'.join(lines) + '\n')

    # ---- lifecycle -----------------------------------------------------------------------------------------
    def restore_dump(self):
        """Restore the real-model product dump into an empty product DB (real-model runbook steps 3-4)."""
        self.compose('up', '-d', '--wait', 'postgres', timeout=300)
        pg = self.container_id('postgres')
        tables = run(['docker', 'exec', pg, 'psql', '-U', 'elitea', '-d', 'elitea', '-At', '-c',
                      "SELECT count(*) FROM pg_tables WHERE schemaname = 'centry'"]).stdout.decode().strip()
        if tables != '0':
            return 'skipped: product DB already populated'
        with open(self.cfg['dump'], 'rb') as dump:
            proc = subprocess.run(['docker', 'exec', '-i', pg, 'pg_restore', '-U', 'elitea', '-d', 'elitea',
                                   '--no-owner', '--exit-on-error'], stdin=dump, capture_output=True, timeout=1800)
        if proc.returncode != 0:
            raise HarnessError('pg_restore failed: ' + proc.stderr.decode('utf-8', 'replace')[-400:])
        run(['docker', 'exec', pg, 'psql', '-U', 'elitea', '-d', 'elitea', '-c',
             "DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace "
             "WHERE e.extname = 'vector' AND n.nspname <> 'public') THEN ALTER EXTENSION vector SET SCHEMA public; "
             "END IF; END $$"])
        return 'restored'

    def ensure_preparation_network(self):
        """The preparer-only resolver bridge (non-internal: preparers reach the package registries). Docker cannot
        restrict its egress to hostnames; the preparer's Deno --allow-net is the only destination limit."""
        if not self.feature('preparation'):
            return None
        if run(['docker', 'network', 'inspect', PREPARATION_NETWORK], check=False).returncode == 0:
            return 'exists'
        run(['docker', 'network', 'create', '--label', f'io.elitea.crash.stack={self.project}', PREPARATION_NETWORK])
        return 'created'

    def preparation_network_members(self):
        """Container ids attached to the resolver bridge (the harness checks they are this stack's preparers)."""
        out = run(['docker', 'network', 'inspect', PREPARATION_NETWORK, '--format', '{{json .Containers}}'],
                  check=False)
        if out.returncode != 0:
            return None
        return sorted((json.loads(out.stdout) or {}).keys())

    def up(self):
        refuse_if_kind_running()
        self.ensure_preparation_network()
        run(['bash', 'deploy/scripts/standalone-stack.sh', 'up'], cwd=REPO, env=self.env(), timeout=1800)

    def down(self, volumes=False):
        args = ['down', '--remove-orphans'] + (['-v'] if volumes else [])
        self.compose(*args, timeout=600)

    def is_up(self):
        out = run(['docker', 'ps', '-q', '--filter', f'label=com.docker.compose.project={self.project}'],
                  check=False).stdout.decode().split()
        return bool(out)

    # ---- containers ----------------------------------------------------------------------------------------
    def container_id(self, service, include_stopped=True):
        args = ['ps', '-q'] + (['-a'] if include_stopped else []) + [service]
        ids = self.compose(*args, timeout=60).stdout.decode().split()
        if len(ids) != 1:
            raise HarnessError(f'expected one {service} container, found {len(ids)}')
        return ids[0]

    def recreate_worker(self, config_name='worker-runtime.json'):
        """Recreate the Worker with another rendered config (scenario stack_setup); returns its flags."""
        path = self.material / config_name
        self.compose('up', '-d', '--no-deps', '--force-recreate', 'elitea-worker', timeout=180,
                     extra_env={'ELITEA_SANDBOX_WORKER_CONFIG': str(path)})
        config = json.loads(path.read_text())
        return {'config': config_name, 'agent_model_checkpoint_recovery': bool(config.get('agent_model_checkpoint_recovery')),
                'agent_node_recovery': bool(config.get('agent_node_recovery'))}

    def rotate_spool(self):
        """Point the Worker at a new, empty spool volume (D2 pod-replacement emulation). Old spools are kept."""
        state = self.state()
        state.setdefault('retired_spools', []).append(state['spool_name'])
        state['spool_generation'] += 1
        state['spool_name'] = f"{self.project}-worker-spool-{state['spool_generation']}"
        self.save_state(state)
        return state['spool_name']


def image_id(ref):
    if not ref:
        raise HarnessError('a sandbox image reference is missing from stack.json')
    out = run(['docker', 'image', 'inspect', '--format', '{{.Id}}', ref]).stdout.decode().strip()
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', out):
        raise HarnessError(f'unexpected image id for {ref}')
    return out


def inspect(container_id):
    """Process identity used as fault evidence: the container id, its PID and start time change on a restart."""
    raw = run(['docker', 'inspect', container_id]).stdout
    data = json.loads(raw)[0]
    st = data['State']
    return {'id': data['Id'], 'name': data['Name'].lstrip('/'), 'image': data['Image'], 'status': st['Status'],
            'running': st['Running'], 'paused': st['Paused'], 'pid': st['Pid'], 'started_at': st['StartedAt'],
            'finished_at': st['FinishedAt'], 'exit_code': st['ExitCode'], 'oom_killed': st['OOMKilled'],
            'restart_count': data.get('RestartCount', 0),
            'health': (st.get('Health') or {}).get('Status')}


def refuse_if_kind_running():
    """Compose and the kind crash cluster never run together (16 GB VM, DESIGN §3.6)."""
    if shutil.which('kind') is None:
        return
    clusters = run(['kind', 'get', 'clusters'], check=False).stdout.decode().split()
    if 'elitea-crash' in clusters:
        raise HarnessError('the kind cluster elitea-crash exists; delete it before starting the compose crash stack')


def _write_private(path, text):
    path = pathlib.Path(path)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w') as handle:
        handle.write(text)
    os.chmod(path, 0o600)


def _copy_private(src, dst):
    src = pathlib.Path(src)
    if src.is_symlink() or not src.is_file():
        raise HarnessError(f'material source {src.name} must be a regular file')
    shutil.copyfile(src, dst)
    os.chmod(dst, stat.S_IRUSR | stat.S_IWUSR)
