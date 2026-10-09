"""Compose fault actions (DESIGN §1.1). Every step records its command, exit status and the container's
identity (id, PID, StartedAt) before and after, so "the process really changed" is machine evidence."""
import time

from .common import HarnessError, iso, now, poll, run
from .stack_compose import TARGETS, inspect
from . import triggers

DEFAULT_RESTART_DELAY_S = 2
READY_TIMEOUT_S = 180


def _service(target):
    if target not in TARGETS:
        raise HarnessError(f'unknown fault target {target}')
    return TARGETS[target]


def _identity(stack, service):
    try:
        return inspect(stack.container_id(service))
    except HarnessError as err:
        return {'error': str(err)}


def _wait_running(stack, service, healthy):
    def ready():
        info = _identity(stack, service)
        if not info.get('running'):
            return None
        if healthy and info.get('health') not in (None, 'healthy'):
            return None
        return info
    info, waited = poll(ready, timeout_s=READY_TIMEOUT_S, interval_s=1, what=f'{service} ready')
    if not info:
        raise HarnessError(f'{service} did not become ready within {READY_TIMEOUT_S}s')
    return waited


def apply(stack, ctx, step):
    """Run one fault step; return its evidence record."""
    action = step['action']
    record = {'action': action, 'target': step.get('target'), 't_start': iso()}
    if step.get('after_s'):
        time.sleep(step['after_s'])
    if action == 'wait':
        time.sleep(step['seconds'])
    elif action == 'user_stop':
        # The user's Stop through the public API (DELETE .../task/...), as the chat UI sends it. 204 accepted,
        # 409 when the answer is no longer running.
        record['status'] = ctx.client.stop(ctx.project_id, ctx.response_message_id)
        record['command'] = ['DELETE', '/api/v2/elitea_core/task/prompt_lib/<project>/<response_message_id>']
    elif action == 'wait_until':
        status, evidence, t_held = triggers.wait_for(ctx, step['predicate'], step.get('params', {}),
                                                     step.get('timeout_s', 180))
        record.update({'status': status, 'evidence': evidence, 't_held': iso(t_held)})
        if status != 'held':
            raise StepMissed(f"{step['predicate']} {status}", record)
    else:
        service = _service(step['target'])
        record['before'] = _identity(stack, service)
        cid = record['before'].get('id')
        if action == 'kill9':
            record['command'] = ['docker', 'kill', '-s', 'SIGKILL', '<container>']
            record['rc'] = run(['docker', 'kill', '-s', 'SIGKILL', cid], check=False).returncode
            poll(lambda: not _identity(stack, service).get('running'), timeout_s=30, interval_s=0.2)
        elif action == 'stop':
            grace = str(step.get('grace_s', 10))
            record['command'] = ['docker', 'stop', '-t', grace, '<container>']
            record['rc'] = run(['docker', 'stop', '-t', grace, cid], check=False, timeout=int(grace) + 60).returncode
        elif action == 'start':
            record['command'] = ['docker', 'start', '<container>']
            record['rc'] = run(['docker', 'start', cid], check=False).returncode
            record['ready_after_s'] = _wait_running(stack, service, healthy=step.get('healthy', True))
        elif action in ('pause', 'unpause'):
            record['command'] = ['docker', action, '<container>']
            record['rc'] = run(['docker', action, cid], check=False).returncode
        elif action == 'replace_worker':
            # Pod-replacement emulation (D2): a new container on a new, empty spool volume. The old container
            # and its spool stay for evidence; they are removed only by `crashctl down`.
            if record['before'].get('running'):
                raise HarnessError('replace_worker needs the Worker stopped or killed first')
            spool = stack.rotate_spool()
            record['spool'] = spool
            stack.compose('up', '-d', '--no-deps', 'worker-spool-init', timeout=120)
            stack.compose('rm', '-f', 'elitea-worker', timeout=60)
            stack.compose('up', '-d', '--no-deps', 'elitea-worker', timeout=180)
            record['command'] = ['docker', 'compose', 'up', '-d', '--no-deps', 'elitea-worker', '(new spool)']
            record['rc'] = 0
            record['ready_after_s'] = _wait_running(stack, service, healthy=False)
        else:
            raise HarnessError(f'unknown fault action {action}')
        record['after'] = _identity(stack, service)
        record['process_changed'] = (record['before'].get('id') != record['after'].get('id')
                                     or record['before'].get('started_at') != record['after'].get('started_at'))
        record['process_stopped'] = bool(record['before'].get('running')) and not record['after'].get('running')
    record['t_end'] = iso()
    return record


class StepMissed(HarnessError):
    def __init__(self, message, record):
        super().__init__(message)
        self.record = record


def ensure_running(stack, targets=('nats', 'main', 'worker', 'supervisor')):
    """Restore the stack after a scenario that left a target stopped (cleanup, never a fault)."""
    started = []
    for target in targets:
        service = _service(target)
        info = _identity(stack, service)
        if not info.get('running'):
            run(['docker', 'start', info['id']], check=False)
            _wait_running(stack, service, healthy=target == 'main')
            started.append(service)
    return started


def disk_free_gib(path='/System/Volumes/Data'):
    out = run(['df', '-k', path]).stdout.decode().splitlines()[-1].split()
    return round(int(out[3]) / 1024 / 1024, 1)


def heavy_builds_running():
    out = run(['ps', '-axo', 'command'], check=False).stdout.decode().splitlines()
    docker_builds = sum(1 for l in out if ('buildx' in l and ('build' in l or 'bake' in l)) or 'docker build' in l)
    rust_builds = sum(1 for l in out if l.split(' ')[0].endswith('/cargo') and any(
        w in l.split() for w in ('build', 'test', 'check', 'clippy')))
    return {'docker_builds': docker_builds, 'rust_builds': rust_builds, 'at': iso(now())}
