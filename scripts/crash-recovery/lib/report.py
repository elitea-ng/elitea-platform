"""Run aggregation for the source mapping (DESIGN §3.8). It never edits recovery-guarantees.md: it emits the
proposed cell changes for a human to apply."""
import hashlib
import json
import pathlib
import statistics
import tempfile

from . import catalog
from .common import read_json, run

FINAL = ('REGRESSION', 'PASS', 'EXPECTED-GAP', 'INCONCLUSIVE')


def build(run_dirs):
    rows = {}
    for run_dir in run_dirs:
        for verdict_path in sorted(pathlib.Path(run_dir).glob('*/r*/verdict.json')):
            verdict = read_json(verdict_path)
            evidence = read_json(verdict_path.with_name('evidence.json'))
            sid = evidence['scenario']
            row = rows.setdefault(sid, {'scenario': sid, 'repeats': []})
            row['repeats'].append({
                'run': pathlib.Path(run_dir).name, 'repeat': verdict_path.parent.name,
                'execution_id': evidence.get('execution_id'), 'verdict': verdict['verdict'],
                'observed': verdict.get('observed'), 'why': verdict.get('why') or verdict.get('reason'),
                'measurements': verdict.get('measurements'), 'browser': (evidence.get('browser') or {}).get('result'),
                'invariants': {k: v['status'] for k, v in (verdict.get('invariants') or {}).items()},
                'faults': [{'action': f['action'], 'target': f.get('target'), 'process_changed': f.get('process_changed')}
                           for f in evidence.get('faults', [])]})
    out = []
    for sid, row in sorted(rows.items()):
        s = catalog.load(sid)
        verdicts = [r['verdict'] for r in row['repeats']]
        counted = [v for v in verdicts if v != 'INCONCLUSIVE']
        if not counted:
            final = 'INCONCLUSIVE'
        elif 'REGRESSION' in counted:
            final = 'REGRESSION'
        elif all(v == 'PASS' for v in counted):
            final = 'PASS'
        elif all(v == 'EXPECTED-GAP' for v in counted):
            final = 'EXPECTED-GAP'
        else:
            final = 'MIXED'
        row.update({'cell': s['cell'], 'now': s['now'], 'target': s['target'], 'gap': s.get('gap'),
                    'tier': s['tier'], 'final': final, 'counted_repeats': len(counted),
                    'observed': sorted({r['observed'] for r in row['repeats'] if r['observed']}),
                    'timings': _timings(row['repeats']), 'matrix_proposal': _proposal(s, final, counted)})
        out.append(row)
    return {'scenarios': out}


def _timings(repeats):
    keys = ('t_fault_to_first_progress_s', 't_fault_to_settled_s', 'takeover_latency_s', 'model_calls',
            'nats_redelivered_while_open')
    out = {}
    for key in keys:
        values = [r['measurements'][key] for r in repeats if r.get('measurements') and r['measurements'].get(key) is not None]
        if values:
            out[key] = {'min': min(values), 'median': statistics.median(values), 'max': max(values), 'n': len(values)}
    return out


def _proposal(scenario, final, counted):
    cell = scenario.get('matrix_cell')
    if not cell or final != 'PASS' or len(counted) < scenario.get('repeat', 1):
        return None
    return {'cell': cell, 'from': scenario['now'], 'to_status': 'P', 'class': scenario['target'],
            'evidence': f"crash-recovery-suite {scenario['id']}"}


def markdown(report):
    lines = ['| Scenario | Cell | Now | Target | Verdict | Repeats | Observed | t_fault→settled (median s) |',
             '|---|---|---|---|---|---|---|---|']
    for row in report['scenarios']:
        t = row['timings'].get('t_fault_to_settled_s', {}).get('median')
        lines.append(f"| {row['scenario']} | {row['cell']} | {row['now']} | {row['target']} | {row['final']} | "
                     f"{row['counted_repeats']}/{len(row['repeats'])} | {','.join(row['observed'])} | {t} |")
    return '\n'.join(lines)


def binary_check(spec):
    """Gate §3b: extract the binary from each image (docker create + docker cp) and look for a marker string.
    `expect` true means the marker must be present (a string unique to the evidence source)."""
    results = []
    for item in spec:
        cid = run(['docker', 'create', item['image']]).stdout.decode().strip()
        try:
            with tempfile.TemporaryDirectory() as tmp:
                dst = pathlib.Path(tmp) / 'bin'
                run(['docker', 'cp', f"{cid}:{item['path']}", str(dst)], timeout=300)
                data = dst.read_bytes()
                found = data.count(item['marker'].encode())
                results.append({'image': item['image'],
                                'image_id': run(['docker', 'image', 'inspect', '--format', '{{.Id}}', item['image']]).stdout.decode().strip(),
                                'path': item['path'], 'binary_sha256': hashlib.sha256(data).hexdigest(),
                                'binary_bytes': len(data), 'marker': item['marker'], 'marker_count': found,
                                'ok': (found > 0) == item.get('expect', True)})
        finally:
            run(['docker', 'rm', cid], check=False)
    return results
