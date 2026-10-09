"""Process, time and file helpers shared by every harness module."""
import datetime
import hashlib
import json
import os
import pathlib
import subprocess
import time

REPO = pathlib.Path(__file__).resolve().parents[3]
SUITE = REPO / 'scripts' / 'crash-recovery'
# Raw evidence never enters the repository (DESIGN §3.1); the run book names this default.
DEFAULT_RUNS = pathlib.Path(os.environ.get(
    'CRASH_RUNS_DIR', '/Users/romanmitusov/Documents/Work/.claude/handoffs/crash-recovery-suite/runs'))
DEFAULT_TIMEOUT_S = 120


class HarnessError(RuntimeError):
    """A harness step failed. The message never carries command output that could hold secrets."""


def run(args, *, input_bytes=None, timeout=DEFAULT_TIMEOUT_S, env=None, check=True, cwd=None):
    """Run one bounded command; return CompletedProcess with bytes stdout/stderr."""
    try:
        result = subprocess.run(args, input=input_bytes, capture_output=True, timeout=timeout,
                                env=env, cwd=cwd)
    except subprocess.TimeoutExpired as exc:
        raise HarnessError(f'{args[0]} {args[1] if len(args) > 1 else ""} timed out after {timeout}s') from exc
    if check and result.returncode != 0:
        tail = result.stderr.decode('utf-8', 'replace').strip().splitlines()[-3:]
        raise HarnessError(f'{" ".join(str(a) for a in args[:4])} exited {result.returncode}: {" | ".join(tail)[:400]}')
    return result


def now():
    return datetime.datetime.now(datetime.timezone.utc)


def iso(ts=None):
    return (ts or now()).isoformat(timespec='milliseconds')


def parse_iso(value):
    if value is None:
        return None
    text = str(value).replace('Z', '+00:00')
    if ' ' in text and 'T' not in text:
        text = text.replace(' ', 'T', 1)
    # PostgreSQL json renders 6 fractional digits and a +00:00 offset; Docker renders 9 digits.
    if '.' in text:
        head, _, rest = text.partition('.')
        digits = rest[:len(rest) - len(rest.lstrip('0123456789'))]
        tail = rest[len(digits):]
        text = f'{head}.{digits[:6]}{tail}'
    return datetime.datetime.fromisoformat(text)


def seconds_between(a, b):
    if a is None or b is None:
        return None
    return round((parse_iso(b) - parse_iso(a)).total_seconds(), 3)


def sha256_hex(data):
    if isinstance(data, str):
        data = data.encode('utf-8')
    return hashlib.sha256(data).hexdigest()


def write_json(path, value):
    path = pathlib.Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + '.tmp')
    tmp.write_text(json.dumps(value, indent=2, sort_keys=True, default=str) + '\n')
    os.replace(tmp, path)


def read_json(path, default=None):
    path = pathlib.Path(path)
    if not path.exists():
        return default
    return json.loads(path.read_text())


def poll(predicate, *, timeout_s, interval_s=0.25, what='condition'):
    """Call predicate() until it returns a truthy value or the bound passes; return (value, elapsed_s)."""
    start = time.monotonic()
    while True:
        value = predicate()
        elapsed = time.monotonic() - start
        if value:
            return value, round(elapsed, 3)
        if elapsed >= timeout_s:
            return None, round(elapsed, 3)
        time.sleep(interval_s)
