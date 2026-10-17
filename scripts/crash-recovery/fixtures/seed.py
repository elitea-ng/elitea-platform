#!/usr/bin/env python3
"""Create the crash fixtures through the public API only and record their ids (DESIGN §3.1, WP-6).

The mock model credential is written with deploy/scripts/seed-llm-api.py, the product's own configuration API,
under its own titles so the restored real-model rows stay untouched. No SQL write happens here."""
import json
import pathlib
import re
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
from lib.common import REPO, write_json  # noqa: E402

MOCK_CREDENTIAL = 'crash-mock-vllm'
MOCK_MODEL = 'vllm/E2E-MOCK-MODEL'
PIPELINE_VARIANTS = {
    # name: (template, sleep_ms). 50 s holds a single-fault window; 150 s keeps the Code job running across a
    # 60 s AckWait redelivery and a replacement claim (combinations such as CX-01 and CX-03b).
    'code-nonce-sleep-50': ('code-nonce-sleep.yaml', 50000),
    'code-nonce-sleep-150': ('code-nonce-sleep.yaml', 150000),
    'code-throw': ('code-throw.yaml', None),
}

# Language matrix (catalog/language-matrix.json): code-lang.yaml rendered per language and kind.
LANGUAGE_TEMPLATE = 'code-lang.yaml'
LANGUAGE_NAMES = ('python', 'javascript', 'typescript', 'rust')
LANGUAGE_SLEEP_MS = 50000


def _sleep_sentinel(first_code, sentinel_code, first_id='sleeper'):
    return {'first_id': first_id, 'first_output': 'probe', 'first_transition': 'sentinel_9999', 'sentinel': True,
            'first_code': first_code, 'sentinel_code': sentinel_code}


_JS_SLEEP = '''export default async function run(state) {
  const started_at_ms = Date.now();
  const nonce = crypto.randomUUID();
  await new Promise(resolve => setTimeout(resolve, {{sleep_ms}}));
  return {started_at_ms, nonce, count: 2};
}'''
_JS_NPM = '''import slugify from "npm:slugify@1.6.6";
export default async function run(state) {
  const started_at_ms = Date.now();
  const nonce = crypto.randomUUID();
  await new Promise(resolve => setTimeout(resolve, {{sleep_ms}}));
  return {started_at_ms, nonce, count: 2, slug: slugify("Crash Recovery")};
}'''
_JS_SENTINEL = '''export default async function run(state) {
  return "CRASHRESULT " + JSON.stringify(state.probe) + " SENTINEL9999";
}'''
# TypeScript is transpiled, not type-checked; the annotations only prove the TypeScript path is taken.
_TS_SLEEP = '''export default async function run(state: Record<string, unknown>): Promise<{started_at_ms: number; nonce: string; count: number}> {
  const started_at_ms: number = Date.now();
  const nonce: string = crypto.randomUUID();
  await new Promise<void>(resolve => setTimeout(resolve, {{sleep_ms}}));
  return {started_at_ms, nonce, count: 2};
}'''
_TS_NPM = '''import slugify from "npm:slugify@1.6.6";
export default async function run(state: Record<string, unknown>): Promise<{started_at_ms: number; nonce: string; count: number; slug: string}> {
  const started_at_ms: number = Date.now();
  const nonce: string = crypto.randomUUID();
  await new Promise<void>(resolve => setTimeout(resolve, {{sleep_ms}}));
  return {started_at_ms, nonce, count: 2, slug: slugify("Crash Recovery")};
}'''
_TS_SENTINEL = '''export default async function run(state: Record<string, unknown>): Promise<string> {
  return "CRASHRESULT " + JSON.stringify(state.probe) + " SENTINEL9999";
}'''
# Pyodide script: the last expression is the result; elitea_state holds the selected input.
_PY_SLEEP = '''import asyncio, time, uuid
started_at_ms = int(time.time() * 1000)
nonce = str(uuid.uuid4())
await asyncio.sleep({{sleep_ms}} / 1000)
{'started_at_ms': started_at_ms, 'nonce': nonce, 'count': 2}'''
_PY_SENTINEL = '''import json
"CRASHRESULT " + json.dumps(elitea_state['probe'], separators=(',', ':')) + " SENTINEL9999"'''
# Dependency-free Rust: serde_json is always available. The nonce is hex of the clock nanos and the process id.
_RS_SLEEP = '''pub fn run(_state: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> {
    let since_epoch = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH)?;
    let started_at_ms = since_epoch.as_millis() as u64;
    let nonce = format!("{:x}-{:x}", since_epoch.as_nanos(), std::process::id());
    std::thread::sleep(std::time::Duration::from_millis({{sleep_ms}}));
    Ok(serde_json::json!({"started_at_ms": started_at_ms, "nonce": nonce, "count": 2}))
}'''
_RS_SENTINEL = '''pub fn run(state: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> {
    let probe = state.get("probe").cloned().unwrap_or(serde_json::Value::Null);
    Ok(serde_json::Value::String(format!("CRASHRESULT {} SENTINEL9999", probe)))
}'''


def _throw(code):
    return {'first_id': 'thrower', 'first_output': 'messages', 'first_transition': 'END', 'sentinel': False,
            'first_code': code, 'sentinel_code': ''}


# Per-language node sources. `sleep-npm` (JavaScript and TypeScript only) adds a literal npm: import, which the
# native preparation profile resolves and downloads; Python and Rust package preparation reuse their own fixtures.
LANGUAGES = {
    'python': {
        'sleep': _sleep_sentinel(_PY_SLEEP, _PY_SENTINEL),
        'throw': _throw('raise ValueError("CRASHTHROW deliberate fixture error")'),
    },
    'javascript': {
        'sleep': _sleep_sentinel(_JS_SLEEP, _JS_SENTINEL),
        'sleep-npm': _sleep_sentinel(_JS_NPM, _JS_SENTINEL),
        'throw': _throw('export default async function run(state) {\n'
                        '  throw new Error("CRASHTHROW deliberate fixture error");\n}'),
    },
    'typescript': {
        'sleep': _sleep_sentinel(_TS_SLEEP, _TS_SENTINEL),
        'sleep-npm': _sleep_sentinel(_TS_NPM, _TS_SENTINEL),
        'throw': _throw('export default async function run(state: Record<string, unknown>): Promise<never> {\n'
                        '  throw new Error("CRASHTHROW deliberate fixture error");\n}'),
    },
    'rust': {
        'sleep': _sleep_sentinel(_RS_SLEEP, _RS_SENTINEL),
        'throw': _throw('pub fn run(_state: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> {\n'
                        '    Err("CRASHTHROW deliberate fixture error".into())\n}'),
    },
}
# name: (language, kind, sleep_ms). The catalog's fixture names follow code-<language>-<sleep|npm|throw>[-50].
LANGUAGE_VARIANTS = {
    **{f'code-{lang}-sleep-50': (lang, 'sleep', LANGUAGE_SLEEP_MS) for lang in LANGUAGE_NAMES},
    **{f'code-{lang}-throw': (lang, 'throw', None) for lang in LANGUAGE_NAMES},
}


# Features of the stack (stack.json "features") gate the fixtures that need them.
FEATURE_PIPELINES = {
    'preparation': {'python-prepared-sleep-50': ('python-prepared-sleep.yaml', {'sleep_s': 50})},
    # Rendered per run with a fresh marker (see rust-compiled.yaml); seed records only the template.
    'compiled': {'rust-compiled': ('rust-compiled.yaml', {})},
    # JS/TS npm: needs the native Deno profile (deploy/docker-compose.crash-npm.yml).
    'npm': {f'code-{lang}-npm-50': (LANGUAGE_TEMPLATE, {'language': lang, 'kind': 'sleep-npm', 'sleep_ms': LANGUAGE_SLEEP_MS})
            for lang in ('javascript', 'typescript')},
}
PER_RUN = {'rust-compiled'}


def render(template, sleep_ms=None, **params):
    """Fill a pipeline template. With code-lang.yaml, `language` and `kind` select the node sources from LANGUAGES."""
    if template == LANGUAGE_TEMPLATE:
        params = _language_params(params.pop('language'), params.pop('kind'))
    text = (HERE / 'pipelines' / template).read_text()
    keep = params.get('sentinel', True)
    lines, skipping = [], False
    for line in text.splitlines():
        if line.startswith('# @if '):
            skipping = line.split()[2] == 'sentinel' and not keep
        elif line.startswith('# @end'):
            skipping = False
        elif not skipping and not line.startswith('#'):
            lines.append(line)
    text = '\n'.join(lines) + '\n'
    if sleep_ms is not None:
        params['sleep_ms'] = int(sleep_ms)
    # A placeholder alone on a line takes a multi-line value and keeps the line's indentation.
    block = re.compile(r'^( *)\{\{(\w+)\}\}$')
    expanded = []
    for line in text.splitlines():
        found = block.match(line)
        if found and '\n' in str(params.get(found.group(2), '')):
            expanded += [found.group(1) + l if l else l for l in str(params[found.group(2)]).splitlines()]
        else:
            expanded.append(line)
    text = '\n'.join(expanded) + '\n'
    for key, value in params.items():
        text = text.replace('{{' + key + '}}', str(value))
    if '{{' in text:
        raise ValueError(f'{template}: unfilled placeholder')
    return text


def _language_params(language, kind):
    try:
        return dict(LANGUAGES[language][kind], language=language)
    except KeyError:
        raise ValueError(f'no {kind} fixture for {language}') from None


def create_per_run(client, fixtures, name, marker):
    """Create a fresh pipeline for one run (a fixture whose source must differ per run)."""
    spec = fixtures['pipelines'][name]
    app_id, version_id = client.create_pipeline(fixtures['project_id'], f"{spec['name']}-{marker[:12]}",
                                                render(spec['template'], marker=marker))
    return dict(spec, app_id=app_id, version_id=version_id, name=f"{spec['name']}-{marker[:12]}")


def seed_features(client, project_id, features, suffix):
    out = {}
    for feature in features:
        for name, (template, params) in FEATURE_PIPELINES[feature].items():
            entry = {'agent_type': 'pipeline', 'prompt': 'crash-recovery code fixture', 'feature': feature,
                     'answer_contains': ['SENTINEL9999', 'CRASHRESULT'], 'name': f'crash-{name}-{suffix}'}
            if name in PER_RUN:
                out[name] = dict(entry, template=template, per_run=True)
                continue
            app_id, version_id = client.create_pipeline(project_id, entry['name'], render(template, **params))
            out[name] = dict(entry, app_id=app_id, version_id=version_id, **params)
    return out


def seed_variants(client, project_id, suffix, have=()):
    """Create the base and language-matrix pipelines that are not in `have` (a re-seed adds only the missing)."""
    sources = {name: (render(template, sleep_ms), sleep_ms) for name, (template, sleep_ms) in PIPELINE_VARIANTS.items()}
    sources.update({name: (render(LANGUAGE_TEMPLATE, sleep_ms, language=lang, kind=kind), sleep_ms)
                    for name, (lang, kind, sleep_ms) in LANGUAGE_VARIANTS.items()})
    out = {}
    for name, (text, sleep_ms) in sources.items():
        if name in have:
            continue
        app_id, version_id = client.create_pipeline(project_id, f'crash-{name}-{suffix}', text)
        out[name] = {'app_id': app_id, 'version_id': version_id, 'sleep_ms': sleep_ms,
                     'name': f'crash-{name}-{suffix}', 'agent_type': 'pipeline',
                     'prompt': 'crash-recovery code fixture', 'answer_contains': ['SENTINEL9999', 'CRASHRESULT']}
    return out


def seed(client, project_id, base_url, out_path, suffix, features=()):
    fixtures = {'project_id': project_id, 'pipelines': {}, 'agents': {}}
    fixtures['pipelines'].update(seed_features(client, project_id, features, suffix))
    fixtures['pipelines'].update(seed_variants(client, project_id, suffix))
    subprocess.run([sys.executable, str(REPO / 'deploy/scripts/seed-llm-api.py'), '--base-url', base_url,
                    '--project', str(project_id), '--token', client.token, '--credential-title', MOCK_CREDENTIAL,
                    '--credential-type', 'vllm', '--api-key', 'mock-key-not-used', '--api-base', 'http://llm-mock:8090',
                    '--model', MOCK_MODEL], check=True, capture_output=True, timeout=180)
    spec = json.loads((HERE / 'agents' / 'model-slow.json').read_text())
    app_id, version_id = client.create_agent(project_id, f"{spec['name']}-{suffix}", spec['instructions'],
                                             spec['model_name'], model_project_id=project_id)
    fixtures['agents']['model-slow'] = {'app_id': app_id, 'version_id': version_id, 'agent_type': 'openai',
                                        'name': f"{spec['name']}-{suffix}", 'prompt': spec['prompt'],
                                        'answer_contains': spec['answer_contains']}
    write_json(out_path, fixtures)
    return fixtures
