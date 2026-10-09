"""Observation triggers (DESIGN §3.3). A predicate is polled every 250 ms; the fault fires on the first hold,
after one immediate re-check (the 1084 controller's "repeated cut"). A predicate that turns false between the
check and the fault makes the scenario INCONCLUSIVE. Crashpoints (cp:) are WP-2/WP-3 and wait for #1084."""
import time

from .common import now, parse_iso

TERMINAL_STATES = {'SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED'}
LIVE_STATES = {'CLAIMED', 'RUNNING'}


def _execution(ctx):
    rows = ctx.collector.sql('product', 'execution', exec_id=ctx.exec_id) or []
    return rows[-1] if rows else None


def _live_execution(ctx):
    row = _execution(ctx)
    return row if row and row['state'] in LIVE_STATES and not row['settled_at'] else None


def _jobs(ctx):
    return ctx.collector.sql('agentstate', 'sandbox', exec_id=ctx.exec_id) or []


def _job_age_s(job):
    if not job.get('dispatched_at'):
        return None
    return (now() - parse_iso(job['dispatched_at'])).total_seconds()


def admitted(ctx, params):
    row = _execution(ctx)
    return bool(row), {'state': row and row['state']}


def code_runtime_running(ctx, params):
    """A Code job is dispatched, bound to a runtime container that is running, and has no receipt yet.
    The job must have been running for min_age_s and leave at least `remaining_s` of the fixture's sleep."""
    if not _live_execution(ctx):
        return False, {'reason': 'execution not live'}
    sleep_s = params.get('sleep_s', 50)
    min_age, remaining = params.get('min_age_s', 3), params.get('remaining_s', 20)
    for job in _jobs(ctx):
        if job.get('audience') == 'dns:elitea-sandbox-preparation':
            continue
        if job.get('phase') != 'dispatched' or job.get('has_result') or not job.get('runtime_id'):
            continue
        age = _job_age_s(job)
        if age is None or not (min_age <= age <= sleep_s - remaining):
            continue
        if not ctx.collector.container_running(job['runtime_id']):
            continue
        return True, {'job_key': job['job_key'], 'runtime_id': job['runtime_id'], 'job_age_s': round(age, 3),
                      'lease_epoch': job['lease_epoch'], 'activation_id': job['activation_id']}
    return False, {'reason': 'no running Code job inside the window'}


PREPARATION_AUDIENCE = 'dns:elitea-sandbox-preparation'


def _split(jobs):
    prep = [j for j in jobs if j.get('audience') == PREPARATION_AUDIENCE]
    return prep, [j for j in jobs if j.get('audience') != PREPARATION_AUDIENCE]


def preparation_resolving(ctx, params):
    """The preparer is downloading and resolving: its job is dispatched with a running container and no
    bundle recorded yet, and no execution job exists (sandbox_jobs.preparation_bundle_json, migration 0009)."""
    if not _live_execution(ctx):
        return False, {'reason': 'execution not live'}
    prep, other = _split(_jobs(ctx))
    for job in prep:
        if (job.get('phase') == 'dispatched' and not job.get('has_preparation_bundle') and job.get('runtime_id')
                and ctx.collector.container_running(job['runtime_id'])):
            age = _job_age_s(job)
            if age is not None and age >= params.get('min_age_s', 1):
                return True, {'job_key': job['job_key'], 'runtime_id': job['runtime_id'], 'job_age_s': round(age, 3),
                              'execution_jobs': len(other)}
    return False, {'reason': 'no preparation resolving', 'preparation_jobs': len(prep)}


def hydration_running(ctx, params):
    """The prepared bundle is committed and the execution container is being hydrated: the preparation job is
    completed and the execution job is reserved with a bound runtime but not yet dispatched."""
    if not _live_execution(ctx):
        return False, {'reason': 'execution not live'}
    prep, other = _split(_jobs(ctx))
    if not prep or not all(j.get('phase') == 'completed' for j in prep):
        return False, {'reason': 'preparation not committed'}
    for job in other:
        if job.get('phase') == 'reserved' and job.get('runtime_id') and not job.get('dispatched_at'):
            return True, {'job_key': job['job_key'], 'runtime_id': job['runtime_id'],
                          'preparation_job_keys': [j['job_key'] for j in prep]}
    return False, {'reason': 'no hydration in progress'}


def prepared_execution_running(ctx, params):
    """The prepared job executes: preparation completed, and the execution job runs inside the window."""
    prep, _ = _split(_jobs(ctx))
    if not prep or not all(j.get('phase') == 'completed' for j in prep):
        return False, {'reason': 'preparation not committed'}
    held, evidence = code_runtime_running(ctx, params)
    return held, dict(evidence, preparation_job_keys=[j['job_key'] for j in prep])


def streaming(ctx, params):
    """The model stream is projected (>= min_events node events) and the turn has not finished."""
    if not _live_execution(ctx):
        return False, {'reason': 'execution not live'}
    replay = ctx.collector.sql('product', 'replay', exec_id=ctx.exec_id, t_fault='')
    if replay['terminal']:
        return False, {'reason': 'terminal already projected'}
    node_events = sum(n for k, n in replay['by_type'].items() if k.startswith('execution.node_event'))
    held = node_events >= params.get('min_events', 10)
    return held, {'node_events': node_events, 'by_type': replay['by_type']}


def model_request_pending(ctx, params):
    """The model request reached the provider (journal) and no stream event is projected yet: the window
    before the first token, held open by MOCK_LLM_SLOW_FIRST_CHUNK_DELAY_MS."""
    if not _live_execution(ctx):
        return False, {'reason': 'execution not live'}
    slow = [e for e in ctx.collector.llm_journal() if e.get('mode') == params.get('mode', 'slow')]
    if not slow:
        return False, {'reason': 'no model request yet'}
    age = time.time() - float(slow[-1]['at'])
    replay = ctx.collector.sql('product', 'replay', exec_id=ctx.exec_id, t_fault='')
    streamed = sum(n for k, n in replay['by_type'].items()
                   if k.startswith('execution.node_event') and k.split(':', 1)[-1] in params.get(
                       'stream_types', ('agent_llm_chunk', 'agent_response', 'full_message')))
    held = streamed == 0 and params.get('min_age_s', 1) <= age <= params.get('max_age_s', 15)
    return held, {'model_requests': len(slow), 'request_age_s': round(age, 3), 'streamed_events': streamed,
                  'by_type': replay['by_type']}


def queued(ctx, params):
    """The command is published and waits in the stream; no Worker has granted authority."""
    rows = ctx.collector.sql('product', 'outbox', exec_id=ctx.exec_id) or []
    if not rows or not rows[-1]['published_at'] or rows[-1]['authority_granted_at']:
        return False, {'reason': 'not published or already granted'}
    js = ctx.collector.jsz()
    held = (js['stream'] or {}).get('messages', 0) >= 1 and (js['consumer'] or {}).get('num_ack_pending', 1) == 0
    return held, {'stream': js['stream'], 'consumer': js['consumer']}


def snapshot_publishing(ctx, params):
    """The compiled snapshot is Publishing. With `after_compile` (the chat 852 cut) the compile job must also
    hold its typed descriptor: the compiler has finished and the executable is being published to Main. The
    Publishing row exists from the start of compilation, so without it the cut can land mid-compile."""
    if not _live_execution(ctx):
        return False, {'reason': 'execution not live'}
    snaps = ctx.collector.sql('agentstate', 'compiled_snapshots', exec_id=ctx.exec_id) or []
    pub = [s for s in snaps if s['state'] == 'publishing']
    evidence = {'snapshots': [{'state': s['state'], 'snapshot_key': s['snapshot_key']} for s in snaps]}
    if not pub or not params.get('after_compile'):
        return bool(pub), evidence
    keys = {s['compilation_job_key'] for s in pub}
    compile_jobs = [j for j in _jobs(ctx) if j.get('job_key') in keys]
    done = [j for j in compile_jobs if j.get('has_compiled_descriptor') and j.get('phase') == 'dispatched']
    evidence['compile_jobs'] = [{'phase': j['phase'], 'descriptor': j.get('has_compiled_descriptor'),
                                 'export_verified': j.get('compiled_export_verified'),
                                 'lease_epoch': j['lease_epoch']} for j in compile_jobs]
    return bool(done), evidence


def claim_attempt_at_least(ctx, params):
    """A replacement claim exists (used between the steps of a combination such as CX-01)."""
    claims = ctx.collector.sql('product', 'claims', exec_id=ctx.exec_id) or []
    attempt = max((c['claim_attempt'] for c in claims), default=0)
    return attempt >= params['attempt'], {'claim_attempt': attempt}


PREDICATES = {f.__name__: f for f in (admitted, code_runtime_running, streaming,
                                      preparation_resolving, hydration_running, prepared_execution_running,
                                      model_request_pending, queued, snapshot_publishing, claim_attempt_at_least)}


def wait_for(ctx, name, params, timeout_s, interval_s=0.25):
    """Poll a predicate; on the first hold re-check it at once. Returns (status, evidence, t_held).
    status: 'held' | 'timeout' | 'flapped'."""
    fn = PREDICATES[name]
    deadline = time.monotonic() + timeout_s
    last = {}
    while time.monotonic() < deadline:
        held, evidence = fn(ctx, params)
        last = evidence
        if held:
            again, evidence2 = fn(ctx, params)
            if not again:
                return 'flapped', {'first': evidence, 'recheck': evidence2}, now()
            return 'held', evidence2, now()
        time.sleep(interval_s)
    return 'timeout', last, now()
