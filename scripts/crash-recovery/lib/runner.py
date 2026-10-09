"""One scenario repeat (DESIGN §3.5): preflight, admit, trigger, faults, bounded wait, collect twice, verdict,
cleanup. Raw evidence goes to the local run directory; nothing here writes product rows."""
import json
import os
import pathlib
import re
import subprocess
import time
import uuid

from . import faults_compose as faults
from . import triggers
from .collectors import Collector, EventStream
from .common import REPO, HarnessError, iso, now, parse_iso, poll, read_json, sha256_hex, write_json
from .verdict import evaluate

SETTLE_POLL_S = 2
REPEAT_READ_GAP_S = 5
CLEANUP_GRACE_S = 60
# Delivery gate §3b floor. CRASH_MIN_FREE_GIB may lower it for scenario runs only (no build, no new stack).
MIN_FREE_GIB = float(os.environ.get('CRASH_MIN_FREE_GIB', '30'))
RESULT_RE = re.compile(r'CRASHRESULT (\{.*?\}) SENTINEL9999')


class Inconclusive(HarnessError):
    pass


class Ctx:
    def __init__(self, collector, exec_id, client=None, project_id=None, response_message_id=None):
        self.collector = collector
        self.exec_id = exec_id
        self.client = client
        self.project_id = project_id
        self.response_message_id = response_message_id


def preflight(stack, col):
    """Refuse to start on any failure (DESIGN §3.5 step 1)."""
    problems = []
    free = faults.disk_free_gib()
    if free < MIN_FREE_GIB:
        problems.append(f'only {free} GiB free')
    for target in ('main', 'worker', 'supervisor', 'nats'):
        info = faults._identity(stack, faults.TARGETS[target])
        if not info.get('running'):
            problems.append(f'{target} not running')
    product = col.sql('product', 'preflight', since=stack.state().get('baseline_at', '1970-01-01T00:00:00Z'))
    agent = col.sql('agentstate', 'preflight')
    js = col.jsz()
    if product['open_executions']:
        problems.append(f"{product['open_executions']} open executions")
    if agent['live_jobs'] or agent['publishing_snapshots']:
        problems.append('live sandbox jobs or publishing snapshots')
    if (js['stream'] or {}).get('messages') or (js['consumer'] or {}).get('num_ack_pending') or (
            js['consumer'] or {}).get('num_pending'):
        problems.append('agent stream or durable not empty')
    out = {'disk_free_gib': free, 'product': product, 'agentstate': agent, 'jsz': js,
           'builds': faults.heavy_builds_running(), 'problems': problems}
    if problems:
        raise Inconclusive('preflight: ' + '; '.join(problems))
    return out


def _fixture(fixtures, name):
    for group in ('pipelines', 'agents'):
        if name in fixtures.get(group, {}):
            return fixtures[group][name]
    raise HarnessError(f'fixture {name} is not seeded')


def _answer(client, project_id, conversation_id, fixture):
    answers = client.assistant_answers(project_id, conversation_id)
    out = {'assistant_items': len(answers)}
    if not answers:
        return {**out, 'oracle_ok': False}
    content = answers[-1].get('content') or ''
    if not isinstance(content, str):
        content = json.dumps(content)
    if content.startswith('"'):
        # A pipeline that writes a string into `messages` answers with that string JSON-encoded.
        try:
            decoded = json.loads(content)
            content = decoded if isinstance(decoded, str) else content
        except ValueError:
            pass
    out.update({'content_sha256': sha256_hex(content), 'content_chars': len(content),
                'is_error': (answers[-1].get('metadata') or {}).get('is_error'),
                'markers': {m: content.count(m) for m in fixture.get('answer_contains', [])}})
    match = RESULT_RE.search(content)
    if match:
        try:
            probe = json.loads(match.group(1))
        except ValueError:
            probe = {}
        out['probe'] = {'started_at_ms': probe.get('started_at_ms'), 'count': probe.get('count'),
                        'nonce_sha256': sha256_hex(str(probe.get('nonce')))}
    out['oracle_ok'] = (len(answers) == 1 and all(n == 1 for n in out['markers'].values())
                        and out['is_error'] is False)
    return out


def _collect(col, exec_id, chat_schema, t_fault, t_takeover=None, last_claim_id=None):
    return {'at': iso(), 'product': col.product(exec_id, chat_schema, t_fault),
            'agentstate': col.agentstate(exec_id, last_claim_id, t_takeover), 'jsz': col.jsz(),
            'llm_journal': col.llm_journal(), 'tool_journal': col.tool_journal(), 'mcp_journal': col.mcp_journal()}


def _takeover(claims):
    if claims and len(claims) > 1:
        return claims[-1]['claimed_at'], claims[-1]['claim_id']
    return None, None


class Browser:
    """Drives apps/elitea-web/e2e/crash through the control-file contract (WP-7)."""

    def __init__(self, stack, control_dir, scenario_id, conversation_id, prompt, send_via):
        self.dir = pathlib.Path(control_dir)
        self.dir.mkdir(parents=True, exist_ok=True)
        env = dict(os.environ, CRASH_SCENARIO=scenario_id, CRASH_CONTROL_DIR=str(self.dir),
                   PLAYWRIGHT_BASE_URL=stack.base_url, CRASH_OIDC_SUBJECT=stack.cfg['oidc_subject'],
                   CRASH_CONVERSATION_ID=str(conversation_id), CRASH_PROMPT=prompt, CRASH_SEND_VIA=send_via,
                   CRASH_STATE_DIR=str(stack.dir / 'browser-state'),
                   CRASH_BROWSER_CHANNEL=os.environ.get('CRASH_BROWSER_CHANNEL', 'chrome'))
        self.log = open(self.dir / 'playwright.log', 'wb')
        # apps/elitea-web/node_modules must resolve @playwright/test (a local install or a symlink to one).
        self.proc = subprocess.Popen(['npx', 'playwright', 'test', '-c', 'playwright.crash.config.ts'],
                                     cwd=REPO / 'apps/elitea-web',
                                     env=env, stdout=self.log, stderr=subprocess.STDOUT)

    def wait_file(self, name, timeout_s):
        value, _ = poll(lambda: read_json(self.dir / name), timeout_s=timeout_s, interval_s=0.5)
        if value is None:
            raise Inconclusive(f'browser did not write {name}')
        return value

    def release(self, payload):
        write_json(self.dir / 'released.json', payload)

    def abort(self, reason):
        write_json(self.dir / 'abort.json', {'reason': reason})

    def finish(self, timeout_s=300):
        try:
            rc = self.proc.wait(timeout=timeout_s)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            rc = 'timeout'
        self.log.close()
        return {'exit': rc, 'result': read_json(self.dir / 'browser' / 'result.json')}


def run_scenario(stack, client, scenario, fixtures, out_dir, *, browser=False):
    out_dir = pathlib.Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    col = Collector(stack)
    rec = {'scenario': scenario['id'], 't_start': iso(), 'pre_faults': [], 'faults': []}
    events = EventStream(out_dir / 'docker-events.jsonl')
    br = None
    setup = scenario.get('stack_setup', {})
    try:
        if setup.get('worker_config') == 'stock':
            rec['stack_setup'] = stack.recreate_worker('worker-runtime.stock.json')
            time.sleep(15)  # NATS pull subscription and runtime session readiness after a re-create
        rec['preflight'] = preflight(stack, col)
        col.reset_journals()
        events.start()
        fixture = _fixture(fixtures, scenario['fixture'])
        project_id = fixtures['project_id']
        if fixture.get('per_run'):
            from fixtures import seed as seed_mod
            fixture = seed_mod.create_per_run(client, fixtures, scenario['fixture'], uuid.uuid4().hex)
            rec['fixture'] = {'app_id': fixture['app_id'], 'version_id': fixture['version_id']}
        author = client.author()
        for step in scenario.get('pre_faults', []):
            rec['pre_faults'].append(faults.apply(stack, None, step))
        conv = client.create_conversation(project_id, f"crash {scenario['id']} {int(time.time())}", author['id'],
                                          fixture['app_id'], fixture['name'], fixture['version_id'], fixture['agent_type'])
        rec['conversation'] = {k: conv[k] for k in ('conversation_id', 'conversation_uuid', 'participant_id')}
        rec['t_admit'] = iso()
        if browser:
            br = Browser(stack, out_dir / 'control', scenario['id'], conv['conversation_id'], fixture['prompt'], 'ui')
            br.wait_file('browser-ready.json', 180)
            sent = br.wait_file('sent.json', 120)
            if sent.get('status') not in (200, 201):
                raise Inconclusive(f"browser send returned {sent.get('status')}")
            rec['admission'] = {'via': 'browser', 'question_id': sent['question_id'], 'status': sent['status']}
            exec_id, response_message_id = sent['execution_id'], sent['response_message_id']
        else:
            qid, status, resp = client.send(project_id, conv, fixture['prompt'])
            rec['admission'] = {'via': 'api', 'question_id': qid, 'status': status, 'created': resp.get('created')}
            exec_id, response_message_id = resp['execution_id'], resp['response_message_id']
        rec.update({'execution_id': exec_id, 'response_message_id': response_message_id})
        ctx = Ctx(col, exec_id, client, project_id, response_message_id)
        row, _ = poll(lambda: (col.sql('product', 'execution', exec_id=exec_id) or [None])[-1], timeout_s=30)
        if not row:
            raise Inconclusive('admitted execution row not visible')
        chat_schema = f"p_{row['projection_project_id']}"
        rec['chat_schema'] = chat_schema
        trig = scenario['trigger']
        status, evidence, t_held = triggers.wait_for(ctx, trig['until'], trig.get('params', {}), trig['timeout_s'],
                                                     trig.get('interval_s', 0.25))
        rec['trigger'] = {'predicate': trig['until'], 'status': status, 'evidence': evidence, 't_held': iso(t_held)}
        if status != 'held':
            raise Inconclusive(f"trigger {trig['until']} {status}")
        rec['pre_fault'] = {'product': col.product(exec_id, chat_schema), 'agentstate': col.agentstate(exec_id)}
        rec['t_fault'] = iso()
        rec['t_fault_ms'] = int(parse_iso(rec['t_fault']).timestamp() * 1000)
        for step in scenario['faults']:
            try:
                rec['faults'].append(faults.apply(stack, ctx, step))
            except faults.StepMissed as missed:
                rec['faults'].append(missed.record)
                raise Inconclusive(str(missed)) from missed
        budget = scenario['expect'].get('budget_s', 300)
        deadline = parse_iso(rec['t_admit']).timestamp() + budget

        def settled():
            rows = col.sql('product', 'settlements', exec_id=exec_id) or []
            return [r for r in rows if r.get('committed_at')] or None
        redelivered = []
        while time.time() < deadline and not settled():
            # num_redelivered counts unacked redeliveries only, so it is sampled while the run is open.
            consumer = (col.jsz().get('consumer') or {})
            redelivered.append(consumer.get('num_redelivered') or 0)
            time.sleep(SETTLE_POLL_S)
        rec['max_num_redelivered_while_open'] = max(redelivered, default=0)
        rec['settled_within_budget'] = bool(settled())
        rec['t_settle_observed'] = iso()
        claims = col.sql('product', 'claims', exec_id=exec_id) or []
        t_takeover, last_claim = _takeover(claims)
        rec['final'] = _collect(col, exec_id, chat_schema, rec['t_fault'], t_takeover, last_claim)
        time.sleep(REPEAT_READ_GAP_S)
        rec['repeat_read'] = _collect(col, exec_id, chat_schema, rec['t_fault'], t_takeover, last_claim)
        rec['repeat_read_equal'] = _stable(rec['final'], rec['repeat_read'])
        rec['answer'] = _answer(client, project_id, conv['conversation_id'], fixture)
        if br:
            payload = ({'expect': 'answer', 'answer_contains': fixture['answer_contains']}
                       if scenario['expect'].get('settlement', 'SUCCEEDED') == 'SUCCEEDED' and rec['answer']['oracle_ok']
                       else {'expect': 'failure', 'message_id': response_message_id,
                             'error_code': ((rec['final']['product']['settlements'] or [{}])[-1]).get('error_code')})
            br.release(payload)
        time.sleep(scenario['expect'].get('cleanup_grace_s', CLEANUP_GRACE_S))
        jobs = col.agentstate(exec_id, last_claim, t_takeover)
        runtimes = {j['runtime_id']: col.container_exists(j['runtime_id']) for j in jobs['sandbox'] if j.get('runtime_id')}
        labelled = col.code_containers({j['runtime_label'] for j in jobs['sandbox'] if j.get('runtime_label')})
        runtimes.update({c['id']: True for c in labelled})
        if stack.feature('preparation'):
            members = stack.preparation_network_members() or []
            ours = {c['id'] for c in labelled}
            rec['resolver_network'] = {'members': len(members),
                                       'foreign_members': len([m for m in members if m not in ours])}
        rec['after_grace'] = {'agentstate': jobs, 'runtime_present': runtimes,
                              'grace_s': scenario['expect'].get('cleanup_grace_s', CLEANUP_GRACE_S)}
        if br:
            rec['browser'] = br.finish()
            br = None
    except Inconclusive as err:
        rec['inconclusive'] = str(err)
        if br:
            br.abort(str(err))
    except HarnessError as err:
        rec['inconclusive'] = f'harness error: {err}'
        if br:
            br.abort(str(err))
    finally:
        if setup.get('worker_config'):
            try:
                rec['stack_restored'] = stack.recreate_worker('worker-runtime.json')
                time.sleep(15)
            except HarnessError as err:
                rec['restore_error'] = str(err)
        events.stop()
        rec['events'] = events.summary()
        try:
            rec['restored'] = faults.ensure_running(stack)
        except HarnessError as err:
            rec['restore_error'] = str(err)
        if br:
            rec['browser'] = br.finish(60)
    rec['t_end'] = iso()
    result = evaluate(scenario, rec)
    write_json(out_dir / 'evidence.json', rec)
    write_json(out_dir / 'verdict.json', result)
    return rec, result


def _stable(a, b):
    """The 1084 "independent repeat read": terminal facts must not move between two reads 5 s apart."""
    def facts(x):
        p = x['product']
        return json.dumps([p['execution'], p['settlements'], p['claims'], p['replay']['terminal'],
                           p['output_inbox']['terminal_outputs'], x['jsz']['stream'], x['agentstate']['sandbox']],
                          sort_keys=True, default=str)
    return facts(a) == facts(b)
