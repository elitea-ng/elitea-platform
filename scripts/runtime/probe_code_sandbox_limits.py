"""Verify disposable OCI sandbox limits. This is not Code-node acceptance."""
import argparse
import json
import subprocess
import uuid

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--image', required=True, help='Local Python image name or digest; no pull is performed')
args = parser.parse_args()
IMAGE = subprocess.check_output(
    ['docker', 'image', 'inspect', args.image, '--format', '{{.Id}}'], text=True
).strip()
print(json.dumps({'image': IMAGE}), flush=True)

def run_case(label, code, expected, timeout=30):
    name = 'elitea-code-probe-' + uuid.uuid4().hex[:12]
    command = ['docker', 'run', '--pull=never', '--name', name, '--network=none',
               '--user=10001:10001', '--read-only', '--cap-drop=ALL',
               '--security-opt=no-new-privileges', '--memory=64m',
               '--memory-swap=64m', '--cpus=0.25', '--pids-limit=16',
               '--tmpfs=/tmp:rw,nosuid,nodev,size=8m', IMAGE,
               'python', '-u', '-c', code]
    try:
        try:
            result = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
        except subprocess.TimeoutExpired:
            if expected != 'timeout':
                raise
            state = json.loads(subprocess.check_output(['docker', 'inspect', name]))[0]['State']
            assert state['Running'], 'Expected a live workload at timeout'
            print(json.dumps({'case': label, 'supervisor_timeout': True}), flush=True)
            return
        state = json.loads(subprocess.check_output(['docker', 'inspect', name]))[0]['State']
        record = dict(case=label, exit_code=result.returncode,
                      oom_killed=state['OOMKilled'], output=result.stdout.strip(),
                      stderr=result.stderr.strip())
        print(json.dumps(record), flush=True)
        assert result.returncode == expected, record
        if label == 'memory':
            assert state['OOMKilled'], record
    finally:
        subprocess.run(['docker', 'rm', '-f', name], check=True, capture_output=True)
        check = subprocess.run(['docker', 'inspect', name], capture_output=True)
        assert check.returncode != 0, 'Container cleanup failed'

run_case('execution_and_limits', '''
import os, json, pathlib, subprocess
assert os.getuid() == 10001
cg = pathlib.Path('/sys/fs/cgroup')
limits = {k: (cg / k).read_text().strip() for k in ['memory.max', 'cpu.max', 'pids.max']}
assert limits == {'memory.max': '67108864', 'cpu.max': '25000 100000', 'pids.max': '16'}, limits
assert not os.access(cg, os.W_OK)
assert not pathlib.Path('/var/run/docker.sock').exists()
children = []
try:
    for _ in range(32):
        try:
            children.append(subprocess.Popen(['sleep', '10']))
        except BlockingIOError:
            break
    else:
        raise AssertionError('PID limit was not enforced')
    assert 0 < len(children) < 16
finally:
    for child in children: child.terminate()
    for child in children: child.wait()
try:
    pathlib.Path('/forbidden').write_text('test')
except OSError:
    pass
else:
    raise AssertionError('Root filesystem writable')
state = {'count': 2}
result = {'count': state['count'] + 1}
print(json.dumps({'uid': os.getuid(), 'limits': limits, 'child_count': len(children), 'result': result}))
''', 0)

run_case('memory', '''
blocks = []
for _ in range(128):
    blocks.append(bytearray(1024 * 1024))
raise AssertionError('Memory limit was not enforced')
''', 137)
run_case('cpu_throttling', '''
import json, pathlib, time
stat = pathlib.Path('/sys/fs/cgroup/cpu.stat')
def read_stat():
    return {k: int(v) for k, v in (line.split() for line in stat.read_text().splitlines())}
before = read_stat()
start_cpu, start_wall = time.process_time(), time.monotonic()
while time.process_time() - start_cpu < 0.75:
    pass
elapsed = time.monotonic() - start_wall
after = read_stat()
assert after['nr_throttled'] > before['nr_throttled'], (before, after)
assert elapsed >= 2.0, elapsed
print(json.dumps({'wall_seconds': elapsed, 'throttled_periods': after['nr_throttled'] - before['nr_throttled']}))
''', 0)
run_case('timeout_with_child', '''
import subprocess, time
subprocess.Popen(['sleep', '60'])
time.sleep(60)
''', 'timeout', timeout=3)
print('PASS: CPU throttling, non-root execution, PID enforcement, OOM enforcement, timeout cleanup', flush=True)
