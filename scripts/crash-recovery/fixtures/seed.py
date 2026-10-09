#!/usr/bin/env python3
"""Create the crash fixtures through the public API only and record their ids (DESIGN §3.1, WP-6).

The mock model credential is written with deploy/scripts/seed-llm-api.py, the product's own configuration API,
under its own titles so the restored real-model rows stay untouched. No SQL write happens here."""
import json
import pathlib
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
}


# Features of the stack (stack.json "features") gate the fixtures that need them.
FEATURE_PIPELINES = {
    'preparation': {'python-prepared-sleep-50': ('python-prepared-sleep.yaml', {'sleep_s': 50})},
    # Rendered per run with a fresh marker (see rust-compiled.yaml); seed records only the template.
    'compiled': {'rust-compiled': ('rust-compiled.yaml', {})},
}
PER_RUN = {'rust-compiled'}


def render(template, sleep_ms=None, **params):
    text = (HERE / 'pipelines' / template).read_text()
    lines = [l for l in text.splitlines() if not l.startswith('#')]
    text = '\n'.join(lines) + '\n'
    if sleep_ms is not None:
        params['sleep_ms'] = int(sleep_ms)
    for key, value in params.items():
        text = text.replace('{{' + key + '}}', str(value))
    if '{{' in text:
        raise ValueError(f'{template}: unfilled placeholder')
    return text


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


def seed(client, project_id, base_url, out_path, suffix, features=()):
    fixtures = {'project_id': project_id, 'pipelines': {}, 'agents': {}}
    fixtures['pipelines'].update(seed_features(client, project_id, features, suffix))
    for name, (template, sleep_ms) in PIPELINE_VARIANTS.items():
        app_id, version_id = client.create_pipeline(project_id, f'crash-{name}-{suffix}', render(template, sleep_ms))
        fixtures['pipelines'][name] = {'app_id': app_id, 'version_id': version_id, 'sleep_ms': sleep_ms,
                                       'name': f'crash-{name}-{suffix}', 'agent_type': 'pipeline',
                                       'prompt': 'crash-recovery code fixture',
                                       'answer_contains': ['SENTINEL9999', 'CRASHRESULT']}
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
