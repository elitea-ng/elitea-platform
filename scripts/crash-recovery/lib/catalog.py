"""Scenario catalog (DESIGN §3.2). Files are JSON, not YAML: the harness is standard-library only and Python
ships no YAML parser. The schema is the one in DESIGN §3.2."""
import copy
import json
import re

from .common import SUITE, HarnessError

CATALOG = SUITE / 'catalog'
MATRIX = CATALOG / 'language-matrix.json'
REQUIRED = ('id', 'cell', 'now', 'target', 'tier', 'fixture', 'trigger', 'faults', 'expect')
CLASSES = {'R', 'I', 'C', 'F', 'L'}
ACTIONS = {'kill9', 'stop', 'start', 'pause', 'unpause', 'replace_worker', 'wait', 'wait_until', 'user_stop'}


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
    if set(s.get('stack_setup', {})) - {'worker_config'} or s.get('stack_setup', {}).get('worker_config') not in (None, 'stock'):
        raise HarnessError(f"{s['id']}: unsupported stack_setup")
    requires = s.get('requires')
    if requires is not None and set(requires) != {'feature', 'reason'}:
        raise HarnessError(f"{s['id']}: requires names a feature and a reason")
    if s.get('unsupported') is not None and not isinstance(s['unsupported'], str):
        raise HarnessError(f"{s['id']}: unsupported is the reason text")
    if s['trigger'].get('kind', 'obs') != 'obs':
        raise HarnessError(f"{s['id']}: crashpoint triggers wait for WP-2/WP-3")
    return s


def not_supported_reason(scenario, stack):
    """Why this row cannot run on this stack, or None. It is recorded as NOT-SUPPORTED, never skipped silently."""
    if scenario.get('unsupported'):
        return scenario['unsupported']
    requires = scenario.get('requires')
    if requires and not stack.feature(requires['feature']):
        return requires['reason']
    return None


# ---- language matrix (catalog/language-matrix.json) ----------------------------------------------------------
def _fill(value, names):
    """Fill {name} placeholders. A whole-string placeholder keeps the value's type (numbers stay numbers)."""
    if isinstance(value, str):
        whole = re.fullmatch(r'\{(\w+)\}', value)
        try:
            if whole:
                return names[whole.group(1)]
            return re.sub(r'\{(\w+)\}', lambda m: str(names[m.group(1)]), value)
        except KeyError as missing:
            raise HarnessError(f'language matrix: unknown placeholder {missing}') from None
    if isinstance(value, list):
        return [_fill(v, names) for v in value]
    if isinstance(value, dict):
        return {k: _fill(v, names) for k, v in value.items()}
    return value


def _matrix():
    """Expand the language matrix: {'rows': {id: {'scenario'|'alias': ...}}, 'tiers': {tier: [ids]}}."""
    if not MATRIX.exists():
        return {'rows': {}, 'tiers': {}}
    spec = json.loads(MATRIX.read_text())
    rows, per_lang = {}, {lang: [] for lang in spec['languages']}

    def build(row_id, fault, lang):
        template = copy.deepcopy(spec['faults'][fault])
        base = copy.deepcopy(spec.get('common', {}))
        expect = {**base.pop('expect', {}), **template.pop('expect', {})}
        scenario = {**base, **template, 'expect': expect, 'id': row_id, 'language': lang}
        return validate(_fill(scenario, {'lang': lang, **spec['languages'][lang]}))

    for lang in spec['languages']:
        for fault in spec['order']:
            row_id = f'LM-{fault}-{lang}'
            rows[row_id] = {'scenario': build(row_id, fault, lang)}
            per_lang[lang].append(row_id)
    prep_ids = list(spec.get('prep', {}))
    for row_id, entry in spec.get('prep', {}).items():
        lang = row_id.rsplit('-', 1)[1]
        if lang not in per_lang:
            raise HarnessError(f'{row_id}: language {lang} is not in the matrix')
        rows[row_id] = ({'alias': entry['alias'], 'requires': entry.get('requires')} if 'alias' in entry
                        else {'scenario': build(row_id, entry['fault'], entry['lang'])})
        per_lang[lang].append(row_id)
    for row_id, reason in spec.get('unsupported', {}).items():
        if row_id not in rows:
            raise HarnessError(f'language matrix: unsupported names unknown row {row_id}')
        rows[row_id]['unsupported'] = reason
    tiers = {f'LM-{lang}': ids for lang, ids in per_lang.items()}
    tiers['LM'] = [i for ids in per_lang.values() for i in ids]
    tiers['LM-PREP'] = prep_ids
    return {'rows': rows, 'tiers': tiers}


def aliases():
    """{LM id: scenario id that proves it} for the rows that reference an existing scenario."""
    return {i: r['alias'] for i, r in _matrix()['rows'].items() if 'alias' in r}


def _load_file(scenario_id):
    path = CATALOG / 'scenarios' / f'{scenario_id}.json'
    if not path.exists():
        return None
    return validate(json.loads(path.read_text()))


def load(scenario_id):
    s = _load_file(scenario_id)
    if s is not None:
        return s
    row = _matrix()['rows'].get(scenario_id)
    if row is None:
        raise HarnessError(f'no scenario {scenario_id}')
    if 'alias' in row:
        # An alias is not a copy: the run is the referenced scenario, recorded under its own id. The LM id travels
        # with it so the summary and the report can show which scenario proved which matrix row.
        s = _load_file(row['alias'])
        if s is None:
            raise HarnessError(f"{scenario_id}: alias {row['alias']} is not a scenario")
        s['lm_id'] = scenario_id
        if row.get('requires'):
            s['requires'] = row['requires']
    else:
        s = row['scenario']
    if row.get('unsupported'):
        s['unsupported'] = row['unsupported']
    return validate(s)


def all_ids():
    return sorted([p.stem for p in (CATALOG / 'scenarios').glob('*.json')] + list(_matrix()['rows']))


def tiers():
    return {**json.loads((CATALOG / 'tiers.json').read_text()), **_matrix()['tiers']}


def select(scope):
    known = tiers()
    ids = []
    for item in scope:
        ids += known[item] if item in known else [item]
    out, seen = [], {}
    for i in ids:
        s = load(i)
        # Two LM rows can share one proving scenario, and a scenario can be named by an LM tier and its own: run once.
        if s['id'] in seen:
            if s.get('lm_id'):
                seen[s['id']].setdefault('lm_ids', []).append(s['lm_id'])
            continue
        if s.get('lm_id'):
            s['lm_ids'] = [s['lm_id']]
        seen[s['id']] = s
        out.append(s)
    return out
