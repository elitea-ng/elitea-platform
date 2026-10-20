"""Tests for etl_mock.py against a real in-process TLS server (throwaway CA from openssl).
Run: python3 -m unittest discover -s scripts/acceptance/etl/tests -v"""
import http.client
import json
import pathlib
import secrets
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import etl_mock  # noqa: E402

RATES = etl_mock.load_rates(HERE.parent / 'fx_rates.json')


def issue_material(directory):
    d = pathlib.Path(directory)

    def ossl(*args):
        subprocess.run(['openssl', *args], check=True, capture_output=True)

    ossl('req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes', '-keyout', str(d / 'ca.key'),
         '-out', str(d / 'ca.crt'), '-days', '1', '-subj', '/CN=test CA', '-addext', 'basicConstraints=critical,CA:TRUE')
    ossl('req', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes', '-keyout', str(d / 'srv.key'),
         '-out', str(d / 'srv.csr'), '-subj', '/CN=localhost')
    (d / 'ext').write_text('subjectAltName=DNS:localhost\nextendedKeyUsage=serverAuth\n')
    ossl('x509', '-req', '-in', str(d / 'srv.csr'), '-CA', str(d / 'ca.crt'), '-CAkey', str(d / 'ca.key'),
         '-CAcreateserial', '-out', str(d / 'srv.crt'), '-days', '1', '-extfile', str(d / 'ext'))
    return d / 'ca.crt', d / 'srv.crt', d / 'srv.key'


@unittest.skipIf(shutil.which('openssl') is None, 'openssl is required for the TLS tests')
class MockServer(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.ca, cls.cert, cls.key = issue_material(cls.tmp.name)
        cls.fx_token, cls.hook_token = secrets.token_urlsafe(32), secrets.token_urlsafe(32)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def setUp(self):
        self.journal_path = pathlib.Path(self.tmp.name) / f'journal-{self.id().rsplit(".", 1)[-1]}.jsonl'
        self.start()

    def start(self, journal=None):
        self.state = etl_mock.MockState(journal or etl_mock.Journal(self.journal_path), RATES,
                                        fx_token=self.fx_token, webhook_token=self.hook_token)
        self.public, self.admin = etl_mock.build_servers(self.state, '127.0.0.1:0', '127.0.0.1:0',
                                                         etl_mock.tls_context(str(self.cert), str(self.key)))
        self.port = self.public.server_address[1]
        self.admin_addr = f'127.0.0.1:{self.admin.server_address[1]}'
        for server in (self.public, self.admin):
            threading.Thread(target=server.serve_forever, daemon=True).start()

    def stop(self):
        for server in (self.public, self.admin):
            server.shutdown()
            server.server_close()

    def tearDown(self):
        self.stop()

    def request(self, method, path, body=None, headers=None):
        context = ssl.create_default_context(cafile=str(self.ca))
        conn = http.client.HTTPSConnection('localhost', self.port, context=context, timeout=10)
        try:
            conn.request(method, path, body=body, headers=headers or {})
            response = conn.getresponse()
            return response.status, json.loads(response.read() or b'{}')
        finally:
            conn.close()

    def fx(self, run_id='run-1', batch=0, token=True):
        headers = {'Authorization': f'Bearer {self.fx_token}'} if token else {}
        return self.request('GET', f'/fx/v1/runs/{run_id}/batches/{batch}/rates', headers=headers)

    def hook(self, run_id='run-1', key=None, status='published', token=True):
        body = json.dumps({'run_id': run_id, 'status': status}).encode()
        headers = {'Content-Type': 'application/json', 'Idempotency-Key': key or run_id}
        if token:
            headers['Authorization'] = f'Bearer {self.hook_token}'
        return self.request('POST', '/hooks/v1/etl-complete', body, headers)

    def summary(self):
        return etl_mock.admin_call('GET', '/_admin/summary', admin=self.admin_addr)

    def test_plain_http_is_refused_and_counted(self):
        conn = http.client.HTTPConnection('localhost', self.port, timeout=5)
        with self.assertRaises((http.client.HTTPException, OSError)):
            conn.request('GET', '/healthz')
            conn.getresponse()
        conn.close()
        self.assertEqual(self.summary()['entries'], 0)
        self.assertGreaterEqual(self.summary()['tls_handshake_failures'], 1)

    def test_an_untrusted_client_ca_cannot_connect(self):
        context = ssl.create_default_context()   # system roots, not the test CA
        conn = http.client.HTTPSConnection('localhost', self.port, context=context, timeout=5)
        with self.assertRaises(ssl.SSLCertVerificationError):
            conn.request('GET', '/healthz')
        conn.close()

    def test_fx_serves_the_shared_rate_table_and_needs_the_token(self):
        self.assertEqual(self.fx(token=False)[0], 401)
        status, payload = self.fx()
        self.assertEqual(status, 200)
        self.assertEqual(payload['rates'], RATES['rates'])
        self.assertEqual((payload['run_id'], payload['batch_no']), ('run-1', 0))
        self.assertEqual(self.request('GET', '/fx/v1/runs/bad%2Fid/batches/0/rates')[0], 404)
        fx = self.summary()['fx']['run-1']['0']
        self.assertEqual(fx, {'requests': 2, 'served': 1, 'served_delayed': 0})

    def test_webhook_exactly_once_duplicate_and_conflict(self):
        self.assertEqual(self.hook()[0], 201)
        self.assertEqual(self.hook()[0], 200)
        self.assertEqual(self.hook(status='changed')[0], 409)
        self.assertEqual(self.hook(token=False)[0], 401)
        hook = self.summary()['webhook']['run-1']
        self.assertEqual((hook['accepted'], hook['duplicate'], hook['conflict']), (1, 1, 1))
        self.assertEqual(etl_mock.check(self.summary(), 'run-1'), [])

    def test_webhook_input_bounds(self):
        headers = {'Idempotency-Key': 'run-1', 'Authorization': f'Bearer {self.hook_token}'}
        self.assertEqual(self.request('POST', '/hooks/v1/etl-complete', b'{"status": 1}', headers)[0], 422)
        too_big = b'{"run_id": "run-1", "pad": "' + b'x' * etl_mock.MAX_BODY_BYTES + b'"}'
        self.assertEqual(self.request('POST', '/hooks/v1/etl-complete', too_big, headers)[0], 413)
        at_limit = b'{"run_id": "run-1", "pad": "' + b'x' * (etl_mock.MAX_BODY_BYTES - 30) + b'"}'
        self.assertLessEqual(len(at_limit), etl_mock.MAX_BODY_BYTES)
        self.assertEqual(self.request('POST', '/hooks/v1/etl-complete', at_limit, headers)[0], 201)
        self.assertEqual(self.hook(key='bad key with spaces')[0], 422)

    def test_a_second_key_for_the_same_run_is_a_violation(self):
        self.hook(key='run-1')
        self.hook(key='run-1-retry')
        problems = etl_mock.check(self.summary(), 'run-1')
        self.assertTrue(any('2 accepted deliveries' in p for p in problems))
        self.assertTrue(any('differs from the run_id' in p for p in problems))

    def test_two_served_fx_gets_for_one_batch_are_a_violation(self):
        self.fx()
        self.fx()
        self.assertTrue(etl_mock.check(self.summary(), 'run-1')[0].startswith('fx run run-1 batch 0: 2'))
        self.assertTrue(any('batch 1: 0' in p for p in etl_mock.check(self.summary(), 'run-1', expect_batches=2)))

    def test_status_fault_then_retry_serves_once(self):
        state_faults = etl_mock.admin_call('PUT', '/_admin/faults', [
            {'route': 'fx', 'batch_no': 3, 'action': 'status', 'status': 503, 'times': 2}], admin=self.admin_addr)
        self.assertEqual(len(state_faults['faults']), 1)
        self.assertEqual([self.fx(batch=3)[0] for _ in range(3)], [503, 503, 200])
        self.assertEqual(self.summary()['fx']['run-1']['3'], {'requests': 3, 'served': 1, 'served_delayed': 0})

    def test_accept_then_drop_is_an_effect_and_the_retry_deduplicates(self):
        etl_mock.admin_call('PUT', '/_admin/faults', [{'route': 'webhook', 'action': 'accept_then_drop'}],
                            admin=self.admin_addr)
        with self.assertRaises((http.client.HTTPException, OSError)):
            self.hook()
        self.assertEqual(self.hook()[0], 200)   # the platform's retry with the same key: duplicate, not a 2nd effect
        self.assertEqual(etl_mock.check(self.summary(), 'run-1'), [])

    def test_restart_keeps_the_dedupe_state(self):
        self.assertEqual(self.hook()[0], 201)
        self.stop()
        self.start(etl_mock.Journal(self.journal_path))
        self.assertEqual(self.hook()[0], 200)
        self.assertEqual(self.summary()['webhook']['run-1']['accepted'], 1)

    def test_journal_is_durable_and_holds_no_secrets(self):
        self.fx()
        self.hook()
        text = self.journal_path.read_text()
        self.assertEqual(len(text.splitlines()), 2)
        self.assertNotIn(self.fx_token, text)
        self.assertNotIn(self.hook_token, text)
        self.assertNotIn('Bearer', text)
        self.assertNotIn('published', text)   # bodies are digested, never stored
        entry = json.loads(text.splitlines()[1])
        self.assertEqual(entry['auth'], 'ok')
        self.assertTrue(entry['tls'].startswith('TLSv1.'))

    def test_full_journal_refuses_instead_of_dropping(self):
        with mock.patch.object(etl_mock, 'MAX_JOURNAL_ENTRIES', 1):
            self.assertEqual(self.fx()[0], 200)
            self.assertEqual(self.fx()[0], 507)
            self.assertEqual(self.hook()[0], 507)
        self.assertEqual(len(self.journal_path.read_text().splitlines()), 1)

    def test_rotate_starts_a_fresh_journal_and_keeps_every_archive(self):
        for _ in range(2):   # same entry count both times: the archives must not overwrite each other
            self.hook()
            etl_mock.admin_call('POST', '/_admin/rotate', {}, admin=self.admin_addr)
            self.assertEqual(self.summary()['entries'], 0)
        archives = list(self.journal_path.parent.glob(f'{self.journal_path.stem}-*.jsonl'))
        self.assertEqual(len(archives), 2)
        self.assertEqual(self.hook()[0], 201)

    def test_a_delayed_serve_is_reported_apart_and_is_not_a_violation(self):
        etl_mock.admin_call('PUT', '/_admin/faults', [{'route': 'fx', 'action': 'delay', 'delay_ms': 50}],
                            admin=self.admin_addr)
        self.assertEqual(self.fx()[0], 200)   # delayed; the client may have given up and retried
        self.assertEqual(self.fx()[0], 200)   # the retry
        self.assertEqual(self.summary()['fx']['run-1']['0'], {'requests': 2, 'served': 1, 'served_delayed': 1})
        self.assertEqual(etl_mock.check(self.summary(), 'run-1', expect_batches=None)[0][:7], 'webhook')

    def test_a_torn_final_line_is_dropped_on_restart(self):
        self.hook()
        self.stop()
        with open(self.journal_path, 'ab') as handle:
            handle.write(b'{"route": "webhook", "outc')   # crash mid-append
        self.start(etl_mock.Journal(self.journal_path))
        self.assertEqual(self.summary()['entries'], 1)
        self.assertTrue(self.journal_path.read_bytes().endswith(b'\n'))
        self.assertEqual(self.hook()[0], 200)

    def test_corruption_before_the_last_line_is_refused(self):
        self.hook()
        self.stop()
        good = self.journal_path.read_bytes()
        self.journal_path.write_bytes(b'not json\n' + good)
        with self.assertRaises(etl_mock.MockError):
            etl_mock.Journal(self.journal_path)
        self.journal_path.write_bytes(good)
        self.start(etl_mock.Journal(self.journal_path))

    def test_probe_reports_tls_only_and_exactly_once(self):
        with mock.patch.object(etl_mock, '_https', wraps=etl_mock._https) as wrapped:
            report = etl_mock.probe('localhost', self.port, str(self.ca), 'probe-1', self.fx_token, self.hook_token)
        self.assertTrue(wrapped.called)
        self.assertTrue(report['plain_http'].startswith('refused'))
        self.assertEqual((report['fx']['status'], report['fx_without_token']), (200, 401))
        self.assertEqual((report['webhook_first'], report['webhook_repeat'], report['webhook_conflict']),
                         (201, 200, 409))
        self.assertEqual(etl_mock.check(self.summary(), 'probe-1'), [])


class Configuration(unittest.TestCase):
    def test_tls_is_mandatory(self):
        with self.assertRaises(etl_mock.MockError):
            etl_mock.tls_context(None, None)

    def test_admin_must_be_loopback(self):
        state = etl_mock.MockState(etl_mock.Journal(pathlib.Path(tempfile.mkdtemp()) / 'j.jsonl'), RATES)
        with self.assertRaises(etl_mock.MockError):
            etl_mock.build_servers(state, '127.0.0.1:0', '0.0.0.0:0', ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER))

    def test_fault_rules_are_strict(self):
        bad = [
            [{'route': 'fx', 'action': 'explode'}],
            [{'route': 'fx', 'action': 'status', 'status': 200}],
            [{'route': 'fx', 'action': 'delay', 'delay_ms': etl_mock.MAX_FAULT_DELAY_MS + 1}],
            [{'route': 'fx', 'action': 'accept_then_drop'}],
            [{'route': 'fx', 'action': 'drop', 'unknown': 1}],
            [{'route': 'fx', 'action': 'drop'}] * (etl_mock.MAX_FAULT_RULES + 1),
        ]
        for rules in bad:
            with self.subTest(rules=rules[0]), self.assertRaises(etl_mock.MockError):
                etl_mock.validate_faults(rules)
        self.assertEqual(etl_mock.validate_faults([{'route': 'fx', 'action': 'drop'}])[0]['times'], 1)

    def test_short_tokens_are_refused(self):
        with tempfile.NamedTemporaryFile('w', suffix='.token', delete=False) as handle:
            handle.write('short')
        with self.assertRaises(etl_mock.MockError):
            etl_mock._read_token(handle.name)


if __name__ == '__main__':
    unittest.main()
