"""Scenario catalog (DESIGN §3.2). Files are JSON, not YAML: the harness is standard-library only and Python
ships no YAML parser. The schema is the one in DESIGN §3.2."""
import json

from .common import SUITE, HarnessError

CATALOG = SUITE / 'catalog'
REQUIRED = ('id', 'cell', 'now', 'target', 'tier', 'fixture', 'trigger', 'faults', 'expect')
CLASSES = {'R', 'I', 'C', 'F', 'L'}
ACTIONS = {'kill9', 'stop', 'start', 'pause', 'unpause', 'replace_worker', 'wait', 'wait_until'}


def validate(s):
    missing = [k for k in REQUIRED if k not in s]
    if missing:
        raise HarnessError(f"{s.get('id', '?')}: missing {missing}")
    if s['target'] not in CLASSES or (s.get('expected_gap_class') and s['expected_gap_class'] not in CLASSES):
        raise HarnessError(f"{s['id']}: bad class")
    if s.get('gap') and not s.get('expected_gap_class'):
        raise HarnessError(f"{s['id']}: a gap scenario names its expected_gap_class")
    for step in s.get('pre_faults', []) + s['faults']:
        if step['action'] not in ACTIONS:
            raise HarnessError(f"{s['id']}: unknown action {step['action']}")
    if s['trigger'].get('kind', 'obs') != 'obs':
        raise HarnessError(f"{s['id']}: crashpoint triggers wait for WP-2/WP-3")
    return s


def load(scenario_id):
    path = CATALOG / 'scenarios' / f'{scenario_id}.json'
    if not path.exists():
        raise HarnessError(f'no scenario {scenario_id}')
    return validate(json.loads(path.read_text()))


def all_ids():
    return sorted(p.stem for p in (CATALOG / 'scenarios').glob('*.json'))


def select(scope):
    tiers = json.loads((CATALOG / 'tiers.json').read_text())
    ids = []
    for item in scope:
        ids += tiers[item] if item in tiers else [item]
    return [load(i) for i in ids]
