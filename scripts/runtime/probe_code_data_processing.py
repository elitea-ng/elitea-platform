#!/usr/bin/env python3
"""Run the fixed data fixture in disposable, bounded, offline Linux containers."""
import argparse
from datetime import datetime
import json
from pathlib import Path
import secrets
import subprocess
import tempfile
import time

FIXTURE = Path(__file__).parent / 'fixtures/code-data-processing'
STAGES = [('python', 'python.py', 'input', 'python_data'),
          ('javascript', 'javascript.js', 'python_data', 'javascript_data'),
          ('typescript', 'typescript.ts', 'javascript_data', 'typescript_data'),
          ('rust', 'rust.rs', 'typescript_data', 'messages')]


def command(*args, **kwargs):
    return subprocess.check_output(list(args), timeout=kwargs.pop('timeout', 30), **kwargs)


def run_stage(image, stage, state):
    language, filename, key, _ = stage
    name = 'elitea-data-probe-' + secrets.token_hex(6)
    created = False
    with tempfile.TemporaryDirectory(prefix='elitea-data-probe-') as directory:
        root = Path(directory)
        (root / 'job.json').write_text(json.dumps({'argv': ['/usr/local/bin/elitea-code-execute', '/workspace/.elitea-code.json'], 'timeout_seconds': 60}))
        (root / 'code.json').write_text(json.dumps({'revision': 1, 'language': language,
            'source': (FIXTURE / filename).read_text(), 'input': {key: state[key]},
            'image_digest': image, 'policy_revision': 'data-probe-v1', 'timeout_seconds': 60}))
        for file in root.iterdir():
            file.chmod(0o644)
        try:
            command('docker', 'create', '--pull=never', '--name', name,
                '--user', '10001:10001', '--memory', '512m', '--memory-swap', '512m',
                '--cpus', '1', '--pids-limit', '64', '--network', 'none', '--read-only',
                '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
                '--tmpfs', f'/workspace:rw,{"exec" if language == "rust" else "noexec"},nosuid,nodev,size=256m,uid=10001,gid=10001,mode=0700',
                '--mount', f'type=bind,source={root / "job.json"},target=/workspace/.elitea-job.json,readonly',
                '--mount', f'type=bind,source={root / "code.json"},target=/workspace/.elitea-code.json,readonly',
                '--log-driver', 'local', '--log-opt', 'max-size=256m', '--log-opt', 'max-file=1', '--log-opt', 'compress=false', image)
            created = True
            command('docker', 'start', name)
            exit_code = command('docker', 'wait', name, timeout=75).decode().strip()
            receipt_bytes = command('docker', 'logs', name)
            receipt = json.loads(receipt_bytes)
            assert exit_code == '0' and receipt['status'] == 'completed', (language, receipt)
            assert command('docker', 'logs', name) == receipt_bytes, 'Terminal receipt changed'
            result = json.loads(receipt['stdout'])['result']
            result_bytes = len(json.dumps(result, separators=(',', ':')).encode())
            assert result_bytes <= 256 * 1024, (language, result_bytes)
            lifecycle = json.loads(command('docker', 'inspect', name, '--format', '{{json .State}}'))
            assert not lifecycle['Running'] and not lifecycle['OOMKilled'] and lifecycle['Pid'] == 0
            def timestamp(value):
                # Docker uses nanoseconds; datetime accepts microseconds.
                base, fraction = value.rstrip('Z').split('.')
                return datetime.fromisoformat(base + '.' + fraction[:6] + '+00:00')
            runtime_ms = (timestamp(lifecycle['FinishedAt']) - timestamp(lifecycle['StartedAt'])).total_seconds() * 1000
            metric = {'stage': language, 'sandbox_ms': round(runtime_ms, 3),
                      'processing_ms': result['metrics'][-1]['processing_ms'], 'result_bytes': result_bytes}
            return result, metric
        finally:
            if created:
                command('docker', 'rm', '-f', name)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--deno-image', required=True)
    parser.add_argument('--rust-image', required=True)
    parser.add_argument('--runs', type=int, default=3)
    parser.add_argument('--report', required=True, type=Path)
    args = parser.parse_args()
    for image in [args.deno_image, args.rust_image]:
        if not image.startswith('sha256:') or len(image) != 71:
            parser.error('Use canonical local immutable Docker image IDs')
    if not 1 <= args.runs <= 10:
        parser.error('Use 1 to 10 runs')
    expected = json.loads((FIXTURE / 'expected.json').read_text())
    report = {'deno_image': args.deno_image, 'rust_image': args.rust_image,
              'limits': {'cpu': 1, 'memory_mib': 512, 'workspace_mib': 256, 'network': 'none'}, 'runs': []}
    for index in range(args.runs):
        start = time.perf_counter()
        state = {'input': (FIXTURE / 'input.json').read_text()}
        stages = []
        for stage in STAGES:
            image = args.rust_image if stage[0] == 'rust' else args.deno_image
            result, metric = run_stage(image, stage, state)
            state[stage[3]] = result
            stages.append(metric)
            print(json.dumps({'run': index + 1, **metric}), flush=True)
        for key, value in expected.items():
            assert result[key] == value, (key, result[key], value)
        item = {'run': index + 1, 'harness_wall_ms': round((time.perf_counter() - start) * 1000, 3),
                'stages': stages, 'result': result}
        report['runs'].append(item)
        args.report.write_text(json.dumps(report, indent=2) + '\n')
        print(f'Run {index + 1}: exact expected result, unchanged receipts, and cleanup passed.', flush=True)


if __name__ == '__main__':
    main()
