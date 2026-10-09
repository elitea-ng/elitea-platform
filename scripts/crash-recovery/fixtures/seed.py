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


def render(template, sleep_ms):
    text = (HERE / 'pipelines' / template).read_text()
    lines = [l for l in text.splitlines() if not l.startswith('#')]
    return '\n'.join(lines).replace('{{sleep_ms}}', str(int(sleep_ms))) + '\n'


def seed(client, project_id, base_url, out_path, suffix):
    fixtures = {'project_id': project_id, 'pipelines': {}, 'agents': {}}
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
