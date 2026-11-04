"""Follow one execution after `restore-snapshot` (a deployed re-proof of a stuck state): wait for its settlement,
let cleanup run, then check settle-once, a drained agent stream and no live sandbox job or runtime. Read-only."""
import time

from .collectors import Collector
from .common import iso, write_json

LIVE_PHASES = ('reserved', 'dispatched')


def assess(final, after_grace, containers, jsz, expect):
    """Pure verdict over collected evidence (unit-tested)."""
    settlements = [s for s in final['settlements'] or [] if s.get('committed_at')]
    jobs = after_grace['sandbox'] or []
    consumer, stream = jsz.get('consumer') or {}, jsz.get('stream') or {}
    facts = {
        'settlements': len(settlements),
        'disposition': settlements[-1]['disposition'] if settlements else None,
        'error_code': settlements[-1]['error_code'] if settlements else None,
        'execution_state': (final['execution'] or [{}])[-1].get('state'),
        'live_jobs': [j['job_key'] for j in jobs if j['phase'] in LIVE_PHASES],
        'running_runtimes': [c['id'][:12] for c in containers if c['running']],
        'stream_messages': stream.get('messages'), 'ack_pending': consumer.get('num_ack_pending'),
        'pending': consumer.get('num_pending'),
    }
    checks = {
        'settled_once': facts['settlements'] == 1,
        'expected_disposition': facts['disposition'] == expect,
        'no_live_job': not facts['live_jobs'],
        'no_running_runtime': not facts['running_runtimes'],
        'nats_drained': (facts['stream_messages'], facts['ack_pending'], facts['pending']) == (0, 0, 0),
    }
    return {'ok': all(checks.values()), 'checks': checks, 'facts': facts}


def observe(stack, exec_id, expect, out_dir, budget_s=600, grace_s=60, interval_s=5):
    col = Collector(stack)
    t0 = time.monotonic()
    record = {'execution_id': exec_id, 'expect': expect, 't_start': iso()}
    while True:
        settlements = col.sql('product', 'settlements', exec_id=exec_id) or []
        if any(s.get('committed_at') for s in settlements) or time.monotonic() - t0 >= budget_s:
            break
        time.sleep(interval_s)
    record['t_settled_seen'] = iso()
    record['waited_s'] = round(time.monotonic() - t0, 1)
    final = col.product(exec_id)
    time.sleep(grace_s)
    after = col.agentstate(exec_id)
    labels = {j['runtime_label'] for j in after['sandbox'] or [] if j.get('runtime_label')}
    containers = col.code_containers(labels)
    jsz = col.jsz()
    record.update(final=final, after_grace=after, containers=containers, jsz=jsz, grace_s=grace_s, t_end=iso())
    record['verdict'] = assess(final, after, containers, jsz, expect)
    write_json(out_dir / 'observe.json', record)
    return record['verdict']
