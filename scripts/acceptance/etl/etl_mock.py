#!/usr/bin/env python3
"""FX reference service and completion-webhook receiver for the ETL acceptance, with a durable request journal.
Standard library only. Test fixture: never part of a product image, chart or default compose file.

Public listener (TLS only, the harness's test CA):
  GET  /fx/v1/runs/{run_id}/batches/{batch_no}/rates   -> 200 {base, as_of, rates, run_id, batch_no}
  POST /hooks/v1/etl-complete                          -> 201 accepted | 200 duplicate | 409 conflict
       Idempotency-Key header required; JSON body <= MAX_BODY_BYTES with a run_id field.
  GET  /healthz                                        -> 200 (not journaled)
Admin listener (plain HTTP, loopback only; reach it with `docker compose exec`):
  GET /_admin/summary, GET /_admin/journal?limit=N, GET|PUT /_admin/faults, POST /_admin/rotate

Every public request except /healthz appends one JSON line to the journal (fsync) BEFORE the response is written, so
an effect is counted even when the response is lost. The journal holds ids, statuses, digests and lengths only:
never header values, tokens or bodies. Webhook deduplication is rebuilt from the journal on start, so a restarted
mock still answers a repeated delivery as a duplicate.

Subcommands: serve | summary | journal | faults | rotate | check | probe | health
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import hmac
import http.client
import http.server
import json
import os
import pathlib
import re
import socket
import ssl
import sys
import threading
import urllib.error
import urllib.request

MAX_BODY_BYTES = 64 * 1024
MAX_JOURNAL_ENTRIES = 100_000
MAX_INFLIGHT = 32
MAX_FAULT_RULES = 64
MAX_FAULT_DELAY_MS = 60_000
MAX_JOURNAL_TAIL = 1_000
SOCKET_TIMEOUT_S = 15
ID_RE = re.compile(r'^[A-Za-z0-9._:-]{1,128}$')
FX_PATH_RE = re.compile(r'^/fx/v1/runs/([^/]+)/batches/([0-9]{1,4})/rates$')
WEBHOOK_PATH = '/hooks/v1/etl-complete'
FAULT_ACTIONS = ('status', 'delay', 'drop', 'accept_then_drop')
HERE = pathlib.Path(__file__).resolve().parent
DEFAULT_ADMIN = '127.0.0.1:9101'


class MockError(Exception):
    """A typed, readable configuration error."""


def _now():
    return dt.datetime.now(dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S.%fZ')


def _read_token(path):
    if not path:
        return None
    token = pathlib.Path(path).read_text(encoding='utf-8').strip()
    if len(token) < 24:
        raise MockError(f'token file {path} holds fewer than 24 characters')
    return token


def validate_faults(rules):
    if not isinstance(rules, list) or len(rules) > MAX_FAULT_RULES:
        raise MockError(f'faults must be a list of at most {MAX_FAULT_RULES} rules')
    out = []
    for rule in rules:
        if not isinstance(rule, dict) or set(rule) - {'route', 'run_id', 'batch_no', 'action', 'status', 'delay_ms',
                                                       'times'}:
            raise MockError('a fault rule has unknown fields')
        if rule.get('route') not in ('fx', 'webhook') or rule.get('action') not in FAULT_ACTIONS:
            raise MockError('fault route must be fx|webhook and action one of ' + '|'.join(FAULT_ACTIONS))
        times = rule.get('times', 1)
        if not isinstance(times, int) or not 1 <= times <= 1000:
            raise MockError('fault times must be 1..1000')
        if rule['action'] == 'status' and rule.get('status') not in (429, 500, 502, 503, 504):
            raise MockError('a status fault needs status 429, 500, 502, 503 or 504')
        if rule['action'] == 'delay' and not (isinstance(rule.get('delay_ms'), int)
                                               and 1 <= rule['delay_ms'] <= MAX_FAULT_DELAY_MS):
            raise MockError(f'a delay fault needs delay_ms 1..{MAX_FAULT_DELAY_MS}')
        if rule['action'] == 'accept_then_drop' and rule['route'] != 'webhook':
            raise MockError('accept_then_drop applies to the webhook only')
        if 'run_id' in rule and not (isinstance(rule['run_id'], str) and ID_RE.match(rule['run_id'])):
            raise MockError('fault run_id is invalid')
        if 'batch_no' in rule and not (isinstance(rule['batch_no'], int) and 0 <= rule['batch_no'] <= 9999):
            raise MockError('fault batch_no must be 0..9999')
        out.append(dict(rule, times=times))
    return out


class Journal:
    """Append-only JSONL journal with the webhook dedupe index. append() takes the (re-entrant) lock itself;
    callers hold it around a read-decide-append sequence such as the webhook dedupe."""

    def __init__(self, path):
        self.path = pathlib.Path(path)
        self.lock = threading.RLock()
        self.entries = []
        self.deliveries = {}   # idempotency key -> body sha256 of the accepted delivery
        if self.path.exists():
            raw = self.path.read_bytes()
            good = 0
            for line in raw.splitlines(keepends=True):
                try:
                    if not line.endswith(b'\n'):
                        raise ValueError('torn line')
                    if line.strip():
                        self._index(json.loads(line))
                except ValueError:
                    # A crash mid-append leaves at most one torn final line; anything else is corruption.
                    if good + len(line) != len(raw):
                        raise MockError(f'journal {self.path} is corrupt at byte {good}')
                    sys.stderr.write(f'{_now()} journal: dropped a torn final line ({len(line)} bytes)\n')
                    with open(self.path, 'r+b') as handle:
                        handle.truncate(good)
                    break
                good += len(line)

    def _index(self, entry):
        self.entries.append(entry)
        if entry['route'] == 'webhook' and entry['outcome'] in ('accepted', 'accepted_dropped'):
            self.deliveries[entry['idempotency_key']] = entry['body_sha256']

    def full(self):
        return len(self.entries) >= MAX_JOURNAL_ENTRIES

    def append(self, entry):
        """Write and fsync before the caller answers. Raises MockError when full (never drops silently)."""
        with self.lock:
            if self.full():
                raise MockError('journal_full')
            entry = dict(entry, seq=len(self.entries) + 1, at=_now())
            line = json.dumps(entry, sort_keys=True, separators=(',', ':')) + '\n'
            with open(self.path, 'a', encoding='utf-8') as handle:
                handle.write(line)
                handle.flush()
                os.fsync(handle.fileno())
            self._index(entry)
            return entry

    def rotate(self):
        if self.path.exists():
            stamp = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
            target = self.path.with_name(f'{self.path.stem}-{stamp}-{len(self.entries)}.jsonl')
            if target.exists():
                raise MockError(f'{target.name} exists; refusing to overwrite an archived journal')
            self.path.rename(target)
        self.entries, self.deliveries = [], {}

    def summary(self):
        fx, webhook = {}, {}
        for e in self.entries:
            if e['route'] == 'fx' and e.get('run_id') is not None:
                slot = fx.setdefault(e['run_id'], {}).setdefault(str(e['batch_no']),
                                                                 {'requests': 0, 'served': 0, 'served_delayed': 0})
                slot['requests'] += 1
                slot['served'] += e['outcome'] == 'served'
                # Served only after an injected delay: the client may have timed out and retried, so these are
                # reported apart and do not count against "at most one served GET".
                slot['served_delayed'] += e['outcome'] == 'served_delayed'
            elif e['route'] == 'webhook' and e.get('run_id') is not None:
                slot = webhook.setdefault(e['run_id'], {'requests': 0, 'accepted': 0, 'duplicate': 0, 'conflict': 0,
                                                        'keys': []})
                slot['requests'] += 1
                if e['outcome'] in ('accepted', 'accepted_dropped'):
                    slot['accepted'] += 1
                    slot['keys'].append(e['idempotency_key'])
                elif e['outcome'] in ('duplicate', 'conflict'):
                    slot[e['outcome']] += 1
        violations = []
        for run_id, batches in sorted(fx.items()):
            for batch_no, slot in sorted(batches.items(), key=lambda kv: int(kv[0])):
                if slot['served'] > 1:
                    violations.append(f'fx run {run_id} batch {batch_no}: {slot["served"]} successful GETs')
        for run_id, slot in sorted(webhook.items()):
            if slot['accepted'] > 1:
                violations.append(f'webhook run {run_id}: {slot["accepted"]} accepted deliveries')
            if any(key != run_id for key in slot['keys']):
                violations.append(f'webhook run {run_id}: idempotency key differs from the run_id')
        return {'entries': len(self.entries), 'fx': fx, 'webhook': webhook, 'violations': violations,
                'full': self.full()}


class MockState:
    def __init__(self, journal, rates, fx_token=None, webhook_token=None, faults=()):
        self.journal = journal
        self.rates = rates
        self.fx_token = fx_token
        self.webhook_token = webhook_token
        self.faults = validate_faults(list(faults))
        self.faults_lock = threading.Lock()
        self.inflight = threading.BoundedSemaphore(MAX_INFLIGHT)
        self.handshake_failures = 0

    def take_fault(self, route, run_id, batch_no=None):
        with self.faults_lock:
            for rule in self.faults:
                if rule['route'] != route or rule['times'] < 1:
                    continue
                if 'run_id' in rule and rule['run_id'] != run_id:
                    continue
                if 'batch_no' in rule and rule['batch_no'] != batch_no:
                    continue
                rule['times'] -= 1
                return dict(rule)
        return None


def _auth(header, token):
    if token is None:
        return 'not_required'
    if not header:
        return 'missing'
    expected = f'Bearer {token}'.encode()
    return 'ok' if hmac.compare_digest(header.encode('utf-8', 'replace'), expected) else 'invalid'


class PublicHandler(http.server.BaseHTTPRequestHandler):
    server_version = 'etl-mock/1'
    sys_version = ''
    timeout = SOCKET_TIMEOUT_S
    protocol_version = 'HTTP/1.1'

    def setup(self):
        # TLS handshake in the handler thread, so a slow or plain-text client never blocks accept().
        self.request.settimeout(self.timeout)
        try:
            self.request.do_handshake()
        except (ssl.SSLError, OSError):
            self.server.state.handshake_failures += 1
            raise
        super().setup()

    def log_message(self, fmt, *args):  # status lines only; never headers or bodies
        sys.stderr.write(f'{_now()} {self.command} {self.path.split("?")[0][:80]} {args[1] if len(args) > 1 else ""}\n')

    def _send(self, status, payload, extra=()):
        body = json.dumps(payload, sort_keys=True).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        for name, value in extra:
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(body)

    def _drop(self):
        self.close_connection = True
        try:
            self.request.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass

    def _journal(self, entry):
        try:
            return self.server.state.journal.append(entry)
        except MockError:
            self._send(507, {'error': 'journal_full'})
            return None

    def _guarded(self, method):
        state = self.server.state
        if not state.inflight.acquire(blocking=False):
            self._send(503, {'error': 'saturated'}, [('Retry-After', '1')])
            return
        try:
            method()
        finally:
            state.inflight.release()

    def do_GET(self):
        if self.path == '/healthz':
            self._send(200, {'status': 'ok'})
            return
        self._guarded(self._fx)

    def do_POST(self):
        self._guarded(self._webhook)

    def _base(self, route):
        return {'route': route, 'method': self.command, 'tls': self.request.version(),
                'run_id': None, 'batch_no': None, 'idempotency_key': None, 'body_sha256': None, 'body_bytes': 0,
                'fault': None}

    def _fx(self):
        state = self.server.state
        entry = self._base('fx')
        match = FX_PATH_RE.match(self.path)
        if not match or not ID_RE.match(match.group(1)):
            self._journal(dict(entry, route='unknown', status=404, outcome='rejected', auth='n/a'))
            self._send(404, {'error': 'not_found'})
            return
        run_id, batch_no = match.group(1), int(match.group(2))
        entry.update(run_id=run_id, batch_no=batch_no, auth=_auth(self.headers.get('Authorization'), state.fx_token))
        if entry['auth'] not in ('ok', 'not_required'):
            if self._journal(dict(entry, status=401, outcome='unauthorized')):
                self._send(401, {'error': 'unauthorized'})
            return
        fault = state.take_fault('fx', run_id, batch_no)
        if fault and fault['action'] == 'delay':
            threading.Event().wait(fault['delay_ms'] / 1000)
        if fault and fault['action'] == 'drop':
            if self._journal(dict(entry, status=0, outcome='fault_drop', fault='drop')):
                self._drop()
            return
        if fault and fault['action'] == 'status':
            if self._journal(dict(entry, status=fault['status'], outcome='fault_status', fault='status')):
                self._send(fault['status'], {'error': 'injected'}, [('Retry-After', '1')])
            return
        outcome = 'served_delayed' if fault and fault['action'] == 'delay' else 'served'
        if self._journal(dict(entry, status=200, outcome=outcome, fault=fault and fault['action'])):
            self._send(200, {'base': state.rates['base'], 'as_of': state.rates['as_of'], 'rates': state.rates['rates'],
                             'run_id': run_id, 'batch_no': batch_no})

    def _webhook(self):
        state = self.server.state
        entry = self._base('webhook')
        if self.path != WEBHOOK_PATH:
            self._journal(dict(entry, route='unknown', status=404, outcome='rejected', auth='n/a'))
            self._send(404, {'error': 'not_found'})
            return
        entry['auth'] = _auth(self.headers.get('Authorization'), state.webhook_token)
        length = self.headers.get('Content-Length')
        if length is None or not length.isdigit():
            self._journal(dict(entry, status=411, outcome='rejected'))
            self._send(411, {'error': 'length_required'})
            return
        if int(length) > MAX_BODY_BYTES:
            self._journal(dict(entry, status=413, outcome='rejected', body_bytes=int(length)))
            self.close_connection = True
            self._send(413, {'error': 'body_too_large', 'limit': MAX_BODY_BYTES})
            return
        body = self.rfile.read(int(length))
        entry.update(body_bytes=len(body), body_sha256=hashlib.sha256(body).hexdigest())
        if entry['auth'] not in ('ok', 'not_required'):
            if self._journal(dict(entry, status=401, outcome='unauthorized')):
                self._send(401, {'error': 'unauthorized'})
            return
        key = self.headers.get('Idempotency-Key', '')
        try:
            run_id = json.loads(body)['run_id']
        except (ValueError, KeyError, TypeError):
            run_id = None
        if not ID_RE.match(key) or not isinstance(run_id, str) or not ID_RE.match(run_id):
            self._journal(dict(entry, status=422, outcome='rejected'))
            self._send(422, {'error': 'idempotency_key_and_run_id_required'})
            return
        entry.update(idempotency_key=key, run_id=run_id)
        fault = state.take_fault('webhook', run_id)
        if fault and fault['action'] == 'delay':
            threading.Event().wait(fault['delay_ms'] / 1000)
        with state.journal.lock:   # dedupe decision and journal append are one step
            previous = state.journal.deliveries.get(key)
            if fault and fault['action'] in ('status', 'drop'):
                status = fault['status'] if fault['action'] == 'status' else 0
                saved = self._journal_locked(dict(entry, status=status, outcome=f"fault_{fault['action']}",
                                                  fault=fault['action']))
            elif previous is None:
                dropped = bool(fault and fault['action'] == 'accept_then_drop')
                saved = self._journal_locked(dict(entry, status=0 if dropped else 201,
                                                  outcome='accepted_dropped' if dropped else 'accepted',
                                                  fault=fault and fault['action']))
            elif previous == entry['body_sha256']:
                saved = self._journal_locked(dict(entry, status=200, outcome='duplicate'))
            else:
                saved = self._journal_locked(dict(entry, status=409, outcome='conflict'))
        if saved is None:
            return
        if saved['status'] == 0:
            self._drop()
        elif saved['outcome'] == 'fault_status':
            self._send(saved['status'], {'error': 'injected'}, [('Retry-After', '1')])
        else:
            self._send(saved['status'], {'status': saved['outcome'], 'run_id': run_id, 'seq': saved['seq']})

    def _journal_locked(self, entry):
        try:
            return self.server.state.journal.append(entry)
        except MockError:
            self._send(507, {'error': 'journal_full'})
            return None


class AdminHandler(http.server.BaseHTTPRequestHandler):
    server_version = 'etl-mock-admin/1'
    sys_version = ''
    timeout = SOCKET_TIMEOUT_S

    def log_message(self, fmt, *args):
        pass

    def _send(self, status, payload):
        body = json.dumps(payload, sort_keys=True).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        state = self.server.state
        path, _, query = self.path.partition('?')
        # Snapshot under the lock and answer after releasing it: a slow admin client never stalls the journal.
        if path == '/_admin/summary':
            with state.journal.lock:
                payload = state.journal.summary()
            payload['tls_handshake_failures'] = state.handshake_failures
        elif path == '/_admin/journal':
            match = re.fullmatch(r'limit=([0-9]{1,4})', query)
            limit = min(int(match.group(1)) if match else 100, MAX_JOURNAL_TAIL)
            with state.journal.lock:
                payload = {'entries': list(state.journal.entries[-limit:])}
        elif path == '/_admin/faults':
            with state.faults_lock:
                payload = {'faults': [dict(rule) for rule in state.faults]}
        elif path == '/_admin/healthz':
            payload = {'status': 'ok'}
        else:
            self._send(404, {'error': 'not_found'})
            return
        self._send(200, payload)

    def do_PUT(self):
        state = self.server.state
        length = self.headers.get('Content-Length', '')
        if self.path != '/_admin/faults' or not length.isdigit() or int(length) > MAX_BODY_BYTES:
            self._send(400, {'error': 'bad_request'})
            return
        try:
            rules = validate_faults(json.loads(self.rfile.read(int(length))))
        except (ValueError, MockError) as exc:
            self._send(400, {'error': str(exc)})
            return
        with state.faults_lock:
            state.faults = rules
        self._send(200, {'faults': rules})

    def do_POST(self):
        state = self.server.state
        if self.path != '/_admin/rotate':
            self._send(404, {'error': 'not_found'})
            return
        with state.journal.lock:
            state.journal.rotate()
        self._send(200, {'rotated': True})


class _Server(http.server.ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 64

    def handle_error(self, request, client_address):  # one line, no traceback (plain-text clients are expected)
        sys.stderr.write(f'{_now()} connection from {client_address[0]} closed: {sys.exc_info()[0].__name__}\n')


def build_servers(state, listen, admin_listen, tls_context):
    host, port = listen.rsplit(':', 1)
    admin_host, admin_port = admin_listen.rsplit(':', 1)
    if admin_host not in ('127.0.0.1', '::1', 'localhost'):
        raise MockError('the admin listener must bind to loopback')
    public = _Server((host, int(port)), PublicHandler)
    public.socket = tls_context.wrap_socket(public.socket, server_side=True, do_handshake_on_connect=False)
    public.state = state
    admin = _Server((admin_host, int(admin_port)), AdminHandler)
    admin.state = state
    return public, admin


def tls_context(cert, key):
    if not cert or not key:
        raise MockError('TLS is mandatory: set ETL_MOCK_TLS_CERT and ETL_MOCK_TLS_KEY')
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(cert, key)
    return context


def load_rates(path):
    data = json.loads(pathlib.Path(path).read_text(encoding='utf-8'))
    return {'base': data['base'], 'as_of': data['as_of'], 'rates': data['rates']}


def serve(env):
    state = MockState(Journal(env.get('ETL_MOCK_JOURNAL', '/journal/journal.jsonl')),
                      load_rates(env.get('ETL_MOCK_FX_RATES', str(HERE / 'fx_rates.json'))),
                      fx_token=_read_token(env.get('ETL_MOCK_FX_TOKEN_FILE')),
                      webhook_token=_read_token(env.get('ETL_MOCK_WEBHOOK_TOKEN_FILE')))
    public, admin = build_servers(state, env.get('ETL_MOCK_LISTEN', '0.0.0.0:8443'),
                                  env.get('ETL_MOCK_ADMIN_LISTEN', DEFAULT_ADMIN),
                                  tls_context(env.get('ETL_MOCK_TLS_CERT'), env.get('ETL_MOCK_TLS_KEY')))
    threading.Thread(target=admin.serve_forever, daemon=True).start()
    sys.stderr.write(f'{_now()} etl-mock serving TLS on {public.server_address}, admin on {admin.server_address}\n')
    public.serve_forever()


def admin_call(method, path, payload=None, admin=DEFAULT_ADMIN):
    data = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request(f'http://{admin}{path}', data=data, method=method,
                                     headers={'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as exc:
        return json.loads(exc.read() or b'{}') | {'http_status': exc.code}


def check(summary, run_id, expect_batches=None):
    """The acceptance oracle: exactly one webhook delivery for run_id, at most one served FX GET per batch
    (exactly one per batch 0..n-1 when expect_batches is given), and no journal-wide violations."""
    problems = list(summary['violations'])
    hook = summary['webhook'].get(run_id)
    if not hook or hook['accepted'] != 1:
        problems.append(f'webhook run {run_id}: {hook["accepted"] if hook else 0} accepted deliveries, expected 1')
    fx = summary['fx'].get(run_id, {})
    if expect_batches is not None:
        for batch_no in range(expect_batches):
            served = fx.get(str(batch_no), {}).get('served', 0)
            if served != 1:
                problems.append(f'fx run {run_id} batch {batch_no}: {served} successful GETs, expected 1')
    return problems


def _https(host, port, ca, method, path, body=None, headers=None, plain=False):
    if plain:
        conn = http.client.HTTPConnection(host, port, timeout=10)
    else:
        context = ssl.create_default_context(cafile=ca)
        conn = http.client.HTTPSConnection(host, port, context=context, timeout=10)
    try:
        conn.request(method, path, body=body, headers=headers or {})
        version = None if plain else conn.sock.version()
        response = conn.getresponse()
        return response.status, json.loads(response.read() or b'{}'), version
    finally:
        conn.close()


def probe(host, port, ca, run_id, fx_token=None, webhook_token=None):
    """End-to-end self-check against a running mock: TLS only, FX served, webhook exactly once. Returns a report of
    statuses and journal counts (no secrets)."""
    report = {}
    try:
        _https(host, port, ca, 'GET', '/healthz', plain=True)
        report['plain_http'] = 'answered'
    except (OSError, http.client.HTTPException) as exc:
        report['plain_http'] = f'refused ({type(exc).__name__})'
    auth = {'Authorization': f'Bearer {fx_token}'} if fx_token else {}
    fx_path = f'/fx/v1/runs/{run_id}/batches/0/rates'
    report['fx_without_token'] = _https(host, port, ca, 'GET', fx_path)[0] if fx_token else 'n/a'
    status, payload, version = _https(host, port, ca, 'GET', fx_path, headers=auth)
    report['fx'] = {'status': status, 'tls': version, 'currencies': sorted(payload.get('rates', {}))}
    body = json.dumps({'run_id': run_id, 'status': 'published'}).encode()
    headers = {'Content-Type': 'application/json', 'Idempotency-Key': run_id}
    if webhook_token:
        headers['Authorization'] = f'Bearer {webhook_token}'
    report['webhook_first'] = _https(host, port, ca, 'POST', WEBHOOK_PATH, body, headers)[0]
    report['webhook_repeat'] = _https(host, port, ca, 'POST', WEBHOOK_PATH, body, headers)[0]
    changed = json.dumps({'run_id': run_id, 'status': 'changed'}).encode()
    report['webhook_conflict'] = _https(host, port, ca, 'POST', WEBHOOK_PATH, changed, headers)[0]
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description='ETL acceptance FX + webhook mock')
    sub = parser.add_subparsers(dest='command', required=True)
    sub.add_parser('serve')
    sub.add_parser('summary')
    sub.add_parser('health')
    journal = sub.add_parser('journal')
    journal.add_argument('--limit', type=int, default=100)
    faults = sub.add_parser('faults')
    faults.add_argument('--set', help='JSON list of fault rules; omit to show the current rules')
    sub.add_parser('rotate')
    chk = sub.add_parser('check')
    chk.add_argument('--run-id', required=True)
    chk.add_argument('--expect-batches', type=int)
    prb = sub.add_parser('probe')
    prb.add_argument('--host', default='etl-mocks')
    prb.add_argument('--port', type=int, default=8443)
    prb.add_argument('--ca', required=True)
    prb.add_argument('--run-id', required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == 'serve':
            serve(os.environ)
        elif args.command == 'summary':
            print(json.dumps(admin_call('GET', '/_admin/summary'), indent=2, sort_keys=True))
        elif args.command == 'health':
            return 0 if admin_call('GET', '/_admin/healthz').get('status') == 'ok' else 1
        elif args.command == 'journal':
            print(json.dumps(admin_call('GET', f'/_admin/journal?limit={args.limit}'), indent=2, sort_keys=True))
        elif args.command == 'faults':
            payload = admin_call('PUT', '/_admin/faults', json.loads(args.set)) if args.set else \
                admin_call('GET', '/_admin/faults')
            print(json.dumps(payload, indent=2, sort_keys=True))
            return 1 if 'error' in payload else 0
        elif args.command == 'rotate':
            print(json.dumps(admin_call('POST', '/_admin/rotate')))
        elif args.command == 'check':
            problems = check(admin_call('GET', '/_admin/summary'), args.run_id, args.expect_batches)
            print(json.dumps({'run_id': args.run_id, 'ok': not problems, 'problems': problems}, indent=2))
            return 1 if problems else 0
        elif args.command == 'probe':
            env = os.environ
            report = probe(args.host, args.port, args.ca, args.run_id,
                           _read_token(env.get('ETL_MOCK_FX_TOKEN_FILE')),
                           _read_token(env.get('ETL_MOCK_WEBHOOK_TOKEN_FILE')))
            print(json.dumps(report, indent=2, sort_keys=True))
    except (MockError, OSError) as exc:
        print(f'etl_mock: {exc}', file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
