"""Read-only evidence collectors (DESIGN §2): SQL as the read-only crash_reader role, Docker state and events,
the NATS monitor, and the mock journals. Nothing here writes product, AgentState or broker state."""
import json
import pathlib
import subprocess
import urllib.request

from .common import SUITE, HarnessError, parse_iso, run

INVARIANTS = SUITE / 'invariants'
AGENT_STREAM = 'ELITEA_RT_V1_AGENT'
AGENT_DURABLE = 'elitea-agent-worker-v1'
DEADLETTER_STREAM = 'KV_ELITEA_RT_V1_DEADLETTER'
CODE_JOB_LABEL = 'io.elitea.code.job'
DBS = {'product': 'elitea', 'agentstate': 'agentstate'}


class Collector:
    def __init__(self, stack):
        self.stack = stack
        self._pg = None
        self._original_visits = None

    # ---- SQL -----------------------------------------------------------------------------------------------
    def sql(self, db, name, **binds):
        """Run invariants/<db>/<name>.sql with psql variables (never string interpolation); return its JSON."""
        path = INVARIANTS / db / f'{name}.sql'
        args = ['docker', 'exec', '-i', self._postgres(), 'psql', '-X', '-U', 'crash_reader', '-d', DBS[db],
                '-At', '-v', 'ON_ERROR_STOP=1']
        for key, value in binds.items():
            args += ['-v', f'{key}={"" if value is None else value}']
        result = run(args, input_bytes=path.read_bytes(), timeout=30)
        text = result.stdout.decode().strip()
        return json.loads(text) if text else None

    def _postgres(self):
        if self._pg is None:
            self._pg = self.stack.container_id('postgres')
        return self._pg

    def product(self, exec_id, chat_schema=None, t_fault=None):
        out = {
            'execution': self.sql('product', 'execution', exec_id=exec_id),
            'admission': self.sql('product', 'admission', exec_id=exec_id),
            'settlements': self.sql('product', 'settlements', exec_id=exec_id),
            'claims': self.sql('product', 'claims', exec_id=exec_id),
            'replay': self.sql('product', 'replay', exec_id=exec_id, t_fault=t_fault or ''),
            'outbox': self.sql('product', 'outbox', exec_id=exec_id),
            'output_inbox': self.sql('product', 'output_inbox', exec_id=exec_id),
            'node_recovery': self.sql('product', 'node_recovery', exec_id=exec_id),
        }
        if chat_schema:
            out['chat_answers'] = self.sql('product', 'chat_answers', exec_id=exec_id, chat_schema=chat_schema)
        if self._original_visits is None:
            self._original_visits = bool(self.sql('product', 'preflight', since='1970-01-01T00:00:00Z')
                                         .get('original_code_visits_table'))
        if self._original_visits:
            out['original_code_visits'] = self.sql('product', 'original_code_visits', exec_id=exec_id)
        return out

    def agentstate(self, exec_id, last_claim_id=None, t_takeover=None):
        return {
            'sandbox': self.sql('agentstate', 'sandbox', exec_id=exec_id),
            'checkpoints': self.sql('agentstate', 'checkpoints', exec_id=exec_id, last_claim_id=last_claim_id or '',
                                    t_takeover=t_takeover or ''),
            'compiled_snapshots': self.sql('agentstate', 'compiled_snapshots', exec_id=exec_id),
        }

    # ---- Docker --------------------------------------------------------------------------------------------
    def code_containers(self, labels):
        """Sandbox containers of THIS stack only: those whose job label is one of `labels` (runtime_label from
        sandbox.sql). The daemon is shared with other stacks; never act on, or count, anything else."""
        out = []
        for label in sorted(labels):
            out += run(['docker', 'ps', '-a', '--no-trunc', '--filter', f'label={CODE_JOB_LABEL}={label}',
                        '--format', '{{.ID}}']).stdout.decode().split()
        rows = []
        for cid in out:
            data = json.loads(run(['docker', 'inspect', cid], check=False).stdout or b'[{}]')
            if not data or not data[0]:
                continue
            d = data[0]
            labels = d['Config'].get('Labels') or {}
            rows.append({'id': d['Id'], 'job': labels.get(CODE_JOB_LABEL),
                         'role': labels.get('io.elitea.code.workspace.role'), 'status': d['State']['Status'],
                         'running': d['State']['Running'], 'oom_killed': d['State']['OOMKilled'],
                         'exit_code': d['State']['ExitCode'], 'created': d['Created'],
                         'started_at': d['State']['StartedAt'], 'finished_at': d['State']['FinishedAt']})
        return rows

    @staticmethod
    def container_running(container_id):
        result = run(['docker', 'inspect', '--format', '{{.State.Running}}', container_id], check=False)
        return result.returncode == 0 and result.stdout.decode().strip() == 'true'

    @staticmethod
    def container_exists(container_id):
        """The 1084 controller's removal check: HTTP 404 for the runtime id (docker inspect fails)."""
        if not container_id:
            return False
        return run(['docker', 'inspect', '--format', '{{.Id}}', container_id], check=False).returncode == 0

    # ---- NATS ----------------------------------------------------------------------------------------------
    def jsz(self):
        """Stream and durable counters from the monitor port, read from inside the compose network."""
        probe = ("import json,urllib.request;"
                 "d=json.load(urllib.request.urlopen('http://nats:8222/jsz?accounts=true&streams=true&consumers=true',timeout=5));"
                 "print(json.dumps(d))")
        raw = run(['docker', 'exec', self.stack.container_id('nats-health'), 'python3', '-c', probe], timeout=20).stdout
        data = json.loads(raw)
        out = {'stream': None, 'consumer': None, 'deadletter_messages': None, 'server_id': data.get('server_id'),
               'now': data.get('now')}
        for account in data.get('account_details') or []:
            for stream in account.get('stream_detail') or []:
                name = stream.get('name')
                state = stream.get('state') or {}
                if name == AGENT_STREAM:
                    out['stream'] = {k: state.get(k) for k in ('messages', 'bytes', 'first_seq', 'last_seq',
                                                             'consumer_count')}
                    for consumer in stream.get('consumer_detail') or []:
                        if consumer.get('name') == AGENT_DURABLE:
                            out['consumer'] = {
                                'num_pending': consumer.get('num_pending'),
                                'num_ack_pending': consumer.get('num_ack_pending'),
                                'num_redelivered': consumer.get('num_redelivered'),
                                'num_waiting': consumer.get('num_waiting'),
                                'delivered_consumer_seq': (consumer.get('delivered') or {}).get('consumer_seq'),
                                'delivered_stream_seq': (consumer.get('delivered') or {}).get('stream_seq'),
                                'ack_floor_stream_seq': (consumer.get('ack_floor') or {}).get('stream_seq')}
                elif name == DEADLETTER_STREAM:
                    out['deadletter_messages'] = state.get('messages')
        return out

    # ---- mocks ---------------------------------------------------------------------------------------------
    def llm_journal(self):
        url = f"http://127.0.0.1:{self.stack.cfg['ports']['mock']}/__journal"
        with urllib.request.urlopen(url, timeout=10) as resp:
            data = json.load(resp)
        return [{'mode': e.get('mode'), 'model': e.get('model'), 'at': e.get('at'), 'tools': e.get('tools')}
                for e in data.get('data', [])]

    def tool_journal(self):
        url = f"http://127.0.0.1:{self.stack.cfg['ports']['mock']}/tool/__journal"
        with urllib.request.urlopen(url, timeout=10) as resp:
            data = json.load(resp)
        # The POST body can carry fixture text; keep only the method, path and time.
        return [{'method': e.get('method'), 'path': e.get('path'), 'at': e.get('at')} for e in data.get('data', [])]

    def mcp_journal(self):
        probe = ("import json,ssl,urllib.request;"
                 "c=ssl.create_default_context(cafile='/opt/mock-mcp/tls/ca.crt');"
                 "print(urllib.request.urlopen('https://localhost:8443/__journal',context=c,timeout=5).read().decode())")
        result = run(['docker', 'exec', self.stack.container_id('mcp-mock'), 'python3', '-c', probe],
                     timeout=20, check=False)
        if result.returncode != 0:
            return None  # an mcp-mock image without the WP-6 journal
        data = json.loads(result.stdout)
        return [{'method': e.get('method'), 'tool': e.get('tool'), 'at': e.get('at')} for e in data.get('data', [])]

    def reset_journals(self):
        base = f"http://127.0.0.1:{self.stack.cfg['ports']['mock']}"
        for path in ('/__journal', '/tool/__journal'):
            urllib.request.urlopen(urllib.request.Request(base + path, method='DELETE'), timeout=10).read()
        probe = ("import ssl,urllib.request;c=ssl.create_default_context(cafile='/opt/mock-mcp/tls/ca.crt');"
                 "urllib.request.urlopen(urllib.request.Request('https://localhost:8443/__journal',method='DELETE'),"
                 "context=c,timeout=5).read()")
        run(['docker', 'exec', self.stack.container_id('mcp-mock'), 'python3', '-c', probe], timeout=20, check=False)


class EventStream:
    """Background `docker events` for sandbox containers over the whole scenario (I2 runtime start count)."""

    def __init__(self, path):
        self.path = pathlib.Path(path)
        self.proc = None

    def start(self):
        self.path.parent.mkdir(parents=True, exist_ok=True)
        handle = open(self.path, 'wb')
        self.proc = subprocess.Popen(
            ['docker', 'events', '--filter', 'type=container', '--filter', f'label={CODE_JOB_LABEL}',
             '--format', '{{json .}}'], stdout=handle, stderr=subprocess.DEVNULL)

    def stop(self):
        if self.proc:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()

    def summary(self):
        """Per job label: counts of create/start/die/oom/destroy. Only ids, labels and actions are kept."""
        jobs = {}
        if not self.path.exists():
            return jobs
        for line in self.path.read_text().splitlines():
            try:
                ev = json.loads(line)
            except ValueError:
                continue
            attrs = (ev.get('Actor') or {}).get('Attributes') or {}
            job = attrs.get(CODE_JOB_LABEL)
            action = (ev.get('Action') or ev.get('status') or '').split(':')[0]
            if not job or action not in ('create', 'start', 'die', 'oom', 'kill', 'destroy'):
                continue
            entry = jobs.setdefault(job, {'containers': set(), 'role': attrs.get('io.elitea.code.workspace.role'),
                                          'start_times': []})
            entry[action] = entry.get(action, 0) + 1
            entry['containers'].add((ev.get('Actor') or {}).get('ID') or ev.get('id'))
            if action == 'start' and ev.get('timeNano'):
                entry['start_times'].append(int(ev['timeNano']) // 1_000_000)
        for entry in jobs.values():
            entry['containers'] = sorted(c for c in entry['containers'] if c)
        return jobs


def first_after(timestamps, t):
    """The first ISO timestamp strictly after t, or None."""
    later = sorted(x for x in timestamps if x and parse_iso(x) > parse_iso(t))
    return later[0] if later else None
