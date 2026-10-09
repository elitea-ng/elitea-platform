"""Invariant evaluation and the verdict engine (DESIGN §2). Each invariant yields hold | violated | n/a with
its evidence; the observed class follows the R/I/C/F/L rules and is compared with the scenario target."""
from .common import parse_iso, seconds_between

RANK = {'L': 0, 'F': 1, 'C': 2, 'I': 3, 'R': 4}
HOLD, VIOLATED, NA = 'hold', 'violated', 'n/a'


def _inv(status, **evidence):
    return {'status': status, **evidence}


def _last(rows):
    return rows[-1] if rows else None


def i1_one_execution(run):
    p = run['final']['product']
    executions = p['execution'] or []
    settlements = [s for s in p['settlements'] or [] if s.get('committed_at')]
    terminal = p['replay']['terminal']
    answers = p.get('chat_answers') or []
    facts = {'executions': len(executions), 'generations': sorted({e['generation'] for e in executions}),
             'same_question_executions': p['admission']['same_question_executions'],
             'settlements': len(settlements), 'terminal_projections': len(terminal),
             'chat_answer_groups': len(answers),
             'chat_streaming_left': sum(1 for a in answers if a['is_streaming'])}
    ok = (facts['executions'] == 1 and facts['same_question_executions'] == 1 and facts['settlements'] == 1
          and facts['terminal_projections'] == 1 and facts['chat_answer_groups'] == 1
          and facts['chat_streaming_left'] == 0)
    return _inv(HOLD if ok else VIOLATED, **facts)


def i2_no_code_rerun(run, scenario):
    pre_jobs = {j['job_key']: j for j in run.get('pre_fault', {}).get('agentstate', {}).get('sandbox') or []}
    final_jobs = run['final']['agentstate']['sandbox'] or []
    starts = run.get('events') or {}
    ours = {j['runtime_id'] for j in final_jobs if j.get('runtime_id')}
    our_labels = {label for label, e in starts.items() if set(e.get('containers', [])) & ours}
    multi_start = {label: e.get('start', 0) for label, e in starts.items() if label in our_labels and e.get('start', 0) > 1}
    unmatched = [j['activation_id'] for j in final_jobs if not j.get('job_matched')]
    changed_runtime = [k for k, j in pre_jobs.items() if j.get('runtime_id') and any(
        f['job_key'] == k and f.get('runtime_id') != j['runtime_id'] for f in final_jobs)]
    oracle_jobs = scenario.get('_oracle', {}).get('jobs')
    extra_jobs = (len(final_jobs) - oracle_jobs) if oracle_jobs is not None else None
    probe = run.get('answer', {}).get('probe') or {}
    t_fault_ms = run.get('t_fault_ms')
    sentinel = None
    if probe.get('started_at_ms') is not None and t_fault_ms and run.get('trigger', {}).get('predicate') in (
            'code_runtime_running',):
        sentinel = probe['started_at_ms'] < t_fault_ms
    facts = {'jobs': len(final_jobs), 'oracle_jobs': oracle_jobs, 'extra_jobs': extra_jobs,
             'runtime_starts_over_one': multi_start, 'unmatched_dispatches': unmatched,
             'runtime_changed_for_pre_fault_job': changed_runtime, 'started_before_fault': sentinel,
             'nonce_sha256': probe.get('nonce_sha256')}
    bad = bool(multi_start) or bool(changed_runtime) or (extra_jobs is not None and extra_jobs > 0) or sentinel is False
    if not final_jobs and not starts:
        return _inv(NA, **facts)
    return _inv(VIOLATED if bad else HOLD, **facts)


def i3_effects(run, scenario):
    expect = scenario['expect'].get('effects', {})
    items = sum(1 for e in run['final'].get('tool_journal') or [] if e['method'] == 'POST' and e['path'].startswith('/tool/items'))
    mcp = run['final'].get('mcp_journal')
    mcp_calls = None if mcp is None else sum(1 for e in mcp if e['method'] == 'tools/call')
    model_calls = len(run['final'].get('llm_journal') or [])
    facts = {'tool_items': items, 'mcp_tools_call': mcp_calls, 'model_calls': model_calls, 'expected': expect}
    bad = items > expect.get('tool_items', 0) or (mcp_calls is not None and mcp_calls > expect.get('mcp_tools_call', 0))
    return _inv(VIOLATED if bad else HOLD, **facts)


def i4_frozen_identity(run):
    pre = run.get('pre_fault')
    if not pre:
        return _inv(NA, reason='no pre-fault snapshot')
    diffs = []
    keys = ('execution_id', 'generation', 'command_id', 'request_digest', 'idempotency_key')
    for a, b in zip(pre['product']['execution'] or [], run['final']['product']['execution'] or []):
        diffs += [f'execution.{k}' for k in keys if a.get(k) != b.get(k)]
    for a, b in zip(pre['product']['outbox'] or [], run['final']['product']['outbox'] or []):
        diffs += [f'outbox.{k}' for k in ('outbox_id', 'prepared_digest', 'published_digest', 'prepared_key_id')
                  if a.get(k) != b.get(k) and a.get(k) is not None]
    if len(pre['product']['outbox'] or []) != len(run['final']['product']['outbox'] or []):
        diffs.append('outbox.rows')
    pw = {w['thread_id_sha256']: w for w in pre['agentstate']['checkpoints']['writers']}
    for w in run['final']['agentstate']['checkpoints']['writers']:
        old = pw.get(w['thread_id_sha256'])
        if old and (old['definition_digest'] != w['definition_digest'] or old['checkpoint_family'] != w['checkpoint_family']):
            diffs.append('writer.definition')
    fj = {j['job_key']: j for j in run['final']['agentstate']['sandbox'] or []}
    for j in pre['agentstate']['sandbox'] or []:
        f = fj.get(j['job_key'])
        if f is None:
            diffs.append('sandbox.job_missing')
        elif j.get('job_request_digest') and f.get('job_request_digest') != j['job_request_digest']:
            diffs.append('sandbox.request_digest')
    return _inv(VIOLATED if diffs else HOLD, differences=diffs)


def i5_claims_and_fencing(run, scenario):
    p = run['final']['product']
    claims = p['claims'] or []
    allowed = scenario['expect'].get('takeovers', [0])
    attempts = [c['claim_attempt'] for c in claims]
    epochs = [c['lease_epoch'] for c in claims]
    settlement = _last([s for s in p['settlements'] or [] if s.get('committed_at')])
    last = _last(claims)
    writers = run['final']['agentstate']['checkpoints']['writers']
    facts = {'attempts': attempts, 'epochs': epochs, 'recovery_modes': [c['recovery_mode'] for c in claims],
             'takeovers': max(len(claims) - 1, 0), 'allowed_takeovers': allowed,
             'unreleased_after_settle': sum(1 for c in claims if not c['released_at']),
             'stale_outputs_after_takeover': p['output_inbox']['stale_after_takeover'],
             'stale_checkpoints_after_takeover': run['final']['agentstate']['checkpoints']['stale_checkpoints_after_takeover'],
             'stale_session_events_after_takeover': run['final']['agentstate']['checkpoints']['stale_session_events_after_takeover'],
             'settlement_bound_to_last_claim': bool(settlement and last and settlement['claim_attempt'] == last['claim_attempt']
                                                    and settlement['lease_epoch'] == last['lease_epoch']),
             'writer_on_last_claim': all(w['writer_claim_attempt'] == (last or {}).get('claim_attempt') for w in writers) if writers else None}
    ok = (attempts == list(range(1, len(attempts) + 1)) and all(b > a for a, b in zip(epochs, epochs[1:]))
          and facts['takeovers'] in allowed and facts['stale_outputs_after_takeover'] == 0
          and facts['stale_checkpoints_after_takeover'] == 0 and facts['stale_session_events_after_takeover'] == 0
          and (settlement is None or facts['settlement_bound_to_last_claim'])
          and facts['writer_on_last_claim'] is not False)
    return _inv(HOLD if ok else VIOLATED, **facts)


def i6_no_orphan(run):
    jobs = run['after_grace']['agentstate']['sandbox'] or []
    live = [j['job_key'] for j in jobs if j['phase'] in ('reserved', 'dispatched')]
    cleanup_fail = [j['code_recovery_cleanup_failure'] for j in jobs if j.get('code_recovery_cleanup_failure')]
    present = run['after_grace'].get('runtime_present') or {}
    remaining = [rid for rid, exists in present.items() if exists]
    facts = {'jobs': len(jobs), 'live_jobs': live, 'cleanup_failures': cleanup_fail,
             'runtimes_checked': len(present), 'runtimes_still_present': len(remaining),
             'grace_s': run['after_grace'].get('grace_s')}
    if not jobs:
        return _inv(NA, **facts)
    return _inv(VIOLATED if (live or cleanup_fail or remaining) else HOLD, **facts)


def i7_nats_drained(run):
    js = run['final']['jsz']
    base = run.get('preflight', {}).get('jsz') or {}
    stream, consumer = js.get('stream') or {}, js.get('consumer') or {}
    outbox = run['final']['product']['outbox'] or []
    facts = {'messages': stream.get('messages'), 'num_pending': consumer.get('num_pending'),
             'num_ack_pending': consumer.get('num_ack_pending'),
             'redelivered_delta': (consumer.get('num_redelivered') or 0) - ((base.get('consumer') or {}).get('num_redelivered') or 0),
             'deadletter_delta': (js.get('deadletter_messages') or 0) - (base.get('deadletter_messages') or 0),
             'outbox_closed': all(o['retired_at'] or o['authority_granted_at'] for o in outbox)}
    ok = (facts['messages'] == 0 and facts['num_pending'] == 0 and facts['num_ack_pending'] == 0
          and facts['deadletter_delta'] == 0 and facts['outbox_closed'])
    return _inv(HOLD if ok else VIOLATED, **facts)


def i8_typed_failure(run, scenario):
    p = run['final']['product']
    settlement = _last([s for s in p['settlements'] or [] if s.get('committed_at')])
    if not settlement or settlement['disposition'] != 'FAILED':
        return _inv(NA, disposition=settlement and settlement['disposition'])
    terminal = _last(p['replay']['terminal']) or {}
    answer = _last(p.get('chat_answers') or []) or {}
    admission = _last(p['admission']['rows']) or {}
    allowed = scenario['expect'].get('error_codes') or []
    facts = {'error_code': settlement['error_code'], 'allowed': allowed, 'replay_code': terminal.get('code'),
             'safe_message_present': bool(terminal.get('safe_message')),
             'generic_safe_message': terminal.get('safe_message') == 'The runtime operation failed.',
             'chat_is_error': answer.get('is_error'), 'chat_error_code': answer.get('error_code'),
             'support_reference_is_response_message': bool(admission.get('client_message_id')
                                                           and admission.get('client_message_id') == answer.get('message_id'))}
    ok = ((not allowed or settlement['error_code'] in allowed) and terminal.get('code') and facts['safe_message_present']
          and answer.get('is_error') and facts['support_reference_is_response_message'])
    return _inv(HOLD if ok else VIOLATED, **facts)


def measurements(run):
    p = run['final']['product']
    t_fault = run.get('t_fault')
    claims = p['claims'] or []
    settlement = _last([s for s in p['settlements'] or [] if s.get('committed_at')])
    return {
        't_fault_to_first_progress_s': seconds_between(t_fault, p['replay'].get('first_progress_after_fault')),
        't_fault_to_settled_s': seconds_between(t_fault, settlement and settlement['committed_at']),
        't_admit_to_settled_s': seconds_between(run.get('t_admit'), settlement and settlement['committed_at']),
        'takeover_latency_s': seconds_between(t_fault, claims[-1]['claimed_at']) if len(claims) > 1 else None,
        'model_calls': len(run['final'].get('llm_journal') or []),
        'nats_redelivered_while_open': run.get('max_num_redelivered_while_open'),
        'replay_events': p['replay']['events'],
    }


def observed_class(run, scenario, inv):
    p = run['final']['product']
    settlement = _last([s for s in p['settlements'] or [] if s.get('committed_at')])
    if not settlement:
        return 'L', 'no settlement within budget'
    if inv['I1']['status'] == VIOLATED:
        return 'L', 'duplicate execution, settlement or answer'
    if inv['I2']['status'] == VIOLATED:
        return 'L', 'user Code ran again'
    if inv['I3']['status'] == VIOLATED:
        return 'L', 'an effect repeated'
    if inv['I6']['status'] == VIOLATED:
        return 'L', 'orphan runtime or live sandbox row after the grace period'
    disposition = settlement['disposition']
    expected = scenario['expect'].get('settlement', 'SUCCEEDED')
    if disposition == expected and disposition in ('SUCCEEDED', 'CANCELLED'):
        if disposition == 'SUCCEEDED' and not run.get('answer', {}).get('oracle_ok'):
            return 'L', 'settled SUCCEEDED but the answer does not match the oracle'
        if (inv['I5']['status'] == HOLD and scenario['expect'].get('classify_rerun_as') == 'I'
                and inv['I5']['takeovers'] > 0):
            return 'I', 'completed; an idempotent step re-ran'
        return 'R', 'completed from the durable state'
    if (run['final']['product']['node_recovery']['visits'] and inv['I3']['tool_items'] <= 1
            and any(v['status'] == 'RECONCILED' for v in run['final']['product']['node_recovery']['visits'])):
        return 'C', 'reconciled from node-recovery receipts'
    if disposition in ('FAILED', 'CANCELLED', 'OUTCOME_UNKNOWN'):
        if inv['I8']['status'] == HOLD or disposition == 'CANCELLED':
            return 'F', f'typed failure {settlement["error_code"] or disposition}'
        return 'L', f'untyped or misleading failure {settlement["error_code"]}'
    return 'L', f'unexpected disposition {disposition}'


def evaluate(scenario, run):
    if run.get('inconclusive'):
        return {'verdict': 'INCONCLUSIVE', 'reason': run['inconclusive'], 'observed': None, 'invariants': {}}
    inv = {
        'I1': i1_one_execution(run), 'I2': i2_no_code_rerun(run, scenario), 'I3': i3_effects(run, scenario),
        'I4': i4_frozen_identity(run), 'I5': i5_claims_and_fencing(run, scenario), 'I6': i6_no_orphan(run),
        'I7': i7_nats_drained(run), 'I8': i8_typed_failure(run, scenario),
    }
    observed, why = observed_class(run, scenario, inv)
    target = scenario['target']
    structural = [k for k in ('I4', 'I5', 'I7') if inv[k]['status'] == VIOLATED]
    if RANK[observed] >= RANK[target] and not structural:
        verdict = 'PASS'
    elif scenario.get('gap') and observed == scenario.get('expected_gap_class') and not structural:
        verdict = 'EXPECTED-GAP'
    else:
        verdict = 'REGRESSION'
    return {'verdict': verdict, 'observed': observed, 'why': why, 'target': target, 'now': scenario['now'],
            'gap': scenario.get('gap'), 'structural_violations': structural, 'invariants': inv,
            'measurements': measurements(run)}
