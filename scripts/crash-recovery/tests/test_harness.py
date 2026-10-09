"""Self-tests for the crash-recovery harness (standard library only, no stack needed).

Run: python3 -m unittest discover -s scripts/crash-recovery/tests -v
"""
import copy
import json
import pathlib
import sys
import tempfile
import unittest

SUITE = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SUITE))

from lib import catalog, compiled_manifest, report, verdict  # noqa: E402
from lib.api import _FormParser  # noqa: E402
from lib.common import parse_iso, seconds_between  # noqa: E402
from lib.redact import scan_bytes, scan_tree, scrub_text  # noqa: E402


class ParseIsoTest(unittest.TestCase):
    def test_offsets_survive_fractional_seconds(self):
        # Regression: digits of the "+00:00" offset were once taken as fraction digits, dropping the zone.
        for text in ('2026-10-09T17:46:25.138+00:00', '2026-10-09 17:47:18.945507+00',
                     '2026-10-09T17:28:55.645955123Z', '2026-10-09T17:47:18+00:00'):
            self.assertIsNotNone(parse_iso(text).tzinfo, text)
        self.assertEqual(seconds_between('2026-10-09T17:00:00.000+00:00', '2026-10-09 17:00:01.5+00'), 1.5)

    def test_nanoseconds_are_truncated_not_misread(self):
        self.assertEqual(parse_iso('2026-10-09T17:28:55.645955123Z').microsecond, 645955)


class RedactTest(unittest.TestCase):
    def test_credential_shapes_are_detected(self):
        samples = {
            'pem': b'-----BEGIN PRIVATE KEY-----\nabc',
            'jwt': b'token eyJhbGciOiJIUzUxMiJ9.eyJ1dWlkIjoiYWJjZGVmIn0.sig',
            'mock_key': b'"credential": "mock-key-not-used"',
            'session_cookie': b'Cookie: elitea_session=abcdefgh1234',
            'bearer': b'Authorization: Bearer abcdefghijkl',
        }
        for name, data in samples.items():
            self.assertIn(name, scan_bytes(data), name)
        self.assertEqual(scan_bytes(b'{"execution_id": "0123456789abcdef0123456789abcdef"}'), [])

    def test_tree_scan_and_scrub(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / 'ok.json').write_text('{"count": 1}')
            (root / 'bad.log').write_text('Authorization: Bearer abcdefghijkl')
            self.assertEqual(scan_tree(root), [('bad.log', ['bearer'])])
        self.assertNotIn('abcdefghijkl', scrub_text('Authorization: Bearer abcdefghijkl'))


class CatalogTest(unittest.TestCase):
    def test_every_scenario_validates_and_tiers_resolve(self):
        ids = catalog.all_ids()
        self.assertTrue(ids)
        for sid in ids:
            scenario = catalog.load(sid)
            self.assertEqual(scenario['id'], sid)
        tiers = json.loads((SUITE / 'catalog/tiers.json').read_text())
        for name, members in tiers.items():
            for sid in members:
                self.assertIn(sid, ids, f'{name} names {sid}')
            catalog.select([name])

    def test_crashpoint_triggers_are_refused_until_wp2(self):
        scenario = catalog.load('CR-WK-P10')
        scenario['trigger'] = {'kind': 'cp', 'until': 'W.model.marker_written', 'timeout_s': 10}
        with self.assertRaises(Exception):
            catalog.validate(scenario)


def _run(**overrides):
    """A synthetic evidence record of one clean takeover (claim 1 -> 2) that completed."""
    exec_id = '0' * 32
    settlement = {'generation': 1, 'disposition': 'SUCCEEDED', 'error_code': None, 'claim_attempt': 2, 'lease_epoch': 2,
                  'claim_id': 'c2', 'committed_at': '2026-10-09T10:02:00+00:00'}
    claims = [{'claim_id': 'c1', 'generation': 1, 'claim_attempt': 1, 'lease_epoch': 1, 'recovery_mode': 'NONE',
               'claimed_at': '2026-10-09T10:00:01+00:00', 'released_at': '2026-10-09T10:01:00+00:00'},
              {'claim_id': 'c2', 'generation': 1, 'claim_attempt': 2, 'lease_epoch': 2, 'recovery_mode': 'NODE_RECOVERY',
               'claimed_at': '2026-10-09T10:01:10+00:00', 'released_at': '2026-10-09T10:02:00+00:00'}]
    job = {'job_key': 'k1', 'activation_id': 'a1', 'job_matched': True, 'runtime_id': 'r1', 'phase': 'completed',
           'runtime_label': 'label1',
           'job_request_digest': 'd1', 'code_recovery_cleanup_failure': None}
    product = {
        'execution': [{'execution_id': exec_id, 'generation': 1, 'command_id': 'cmd', 'request_digest': 'rd',
                       'idempotency_key': 'q', 'state': 'SUCCEEDED', 'settled_at': '2026-10-09T10:02:00+00:00'}],
        'admission': {'rows': [{'client_message_id': 'm1'}], 'same_question_executions': 1},
        'settlements': [settlement], 'claims': claims,
        'replay': {'events': 10, 'by_type': {}, 'terminal': [{'event_type': 'execution.node_event', 'node_type': 'full_message'}],
                   'first_progress_after_fault': '2026-10-09T10:01:12+00:00'},
        'outbox': [{'outbox_id': 'o1', 'prepared_digest': 'p', 'published_digest': 'p', 'prepared_key_id': 'k',
                    'retired_at': '2026-10-09T10:02:00+00:00', 'authority_granted_at': '2026-10-09T10:00:01+00:00'}],
        'output_inbox': {'stale_after_takeover': 0, 'terminal_outputs': 1},
        'node_recovery': {'visits': [], 'audit': []},
        'chat_answers': [{'message_id': 'm1', 'is_streaming': False, 'is_error': False, 'error_code': None}],
    }
    agentstate = {'sandbox': [job], 'compiled_snapshots': [],
                  'checkpoints': {'writers': [{'thread_id_sha256': 't', 'definition_digest': 'dd', 'checkpoint_family': 'f',
                                               'writer_claim_attempt': 2, 'writer_lease_epoch': 2}],
                                  'stale_checkpoints_after_takeover': 0, 'stale_session_events_after_takeover': 0}}
    js = {'stream': {'messages': 0}, 'consumer': {'num_pending': 0, 'num_ack_pending': 0, 'num_redelivered': 1},
          'deadletter_messages': 0}
    run = {
        't_admit': '2026-10-09T10:00:00+00:00', 't_fault': '2026-10-09T10:00:20+00:00', 't_fault_ms': 1791540020000,
        'trigger': {'predicate': 'code_runtime_running'},
        'preflight': {'jsz': {'consumer': {'num_redelivered': 0}, 'deadletter_messages': 0}},
        'pre_fault': {'product': copy.deepcopy(product), 'agentstate': copy.deepcopy(agentstate)},
        'final': {'product': product, 'agentstate': agentstate, 'jsz': js, 'llm_journal': [], 'tool_journal': [],
                  'mcp_journal': []},
        'after_grace': {'agentstate': agentstate, 'runtime_present': {'r1': False}, 'grace_s': 60},
        'events': {'label1': {'containers': ['r1'], 'start': 1}},
        'answer': {'oracle_ok': True, 'probe': {'started_at_ms': 1791540010000}},
    }
    for key, value in overrides.items():
        run[key] = value
    return run


SCENARIO = {'id': 'CR-WK-P10', 'now': 'R/F2·P*', 'target': 'R', 'gap': None, 'fixture': 'f',
            'expect': {'takeovers': [1], 'settlement': 'SUCCEEDED', 'effects': {'tool_items': 0}}, '_oracle': {'jobs': 1}}


class VerdictTest(unittest.TestCase):
    def test_clean_takeover_is_r_pass(self):
        result = verdict.evaluate(SCENARIO, _run())
        self.assertEqual((result['verdict'], result['observed']), ('PASS', 'R'), result['why'])
        self.assertEqual(result['measurements']['takeover_latency_s'], 50.0)

    def test_no_settlement_is_l(self):
        run = _run()
        run['final']['product']['settlements'] = []
        self.assertEqual(verdict.evaluate(SCENARIO, run)['observed'], 'L')

    def test_other_stacks_containers_are_ignored(self):
        # Another stack on the shared daemon restarting its own job must not count against this run.
        run = _run(events={'label1': {'containers': ['r1'], 'start': 1}, 'foreign': {'containers': ['x'], 'start': 3}})
        self.assertEqual(verdict.evaluate(SCENARIO, run)['invariants']['I2']['status'], 'hold')

    def test_code_rerun_is_l(self):
        run = _run(events={'label1': {'containers': ['r1'], 'start': 2}})
        result = verdict.evaluate(SCENARIO, run)
        self.assertEqual((result['observed'], result['verdict']), ('L', 'REGRESSION'))

    def test_result_started_after_the_cut_is_a_rerun(self):
        run = _run(answer={'oracle_ok': True, 'probe': {'started_at_ms': 1791540030000}})
        self.assertEqual(verdict.evaluate(SCENARIO, run)['invariants']['I2']['status'], 'violated')

    def test_extra_job_against_the_oracle_is_a_rerun(self):
        run = _run()
        run['final']['agentstate']['sandbox'] = run['final']['agentstate']['sandbox'] + [dict(run['final']['agentstate']['sandbox'][0], job_key='k2', runtime_id='r2')]
        self.assertEqual(verdict.evaluate(SCENARIO, run)['invariants']['I2']['status'], 'violated')

    def test_stale_writer_output_is_structural_regression(self):
        run = _run()
        run['final']['product']['output_inbox']['stale_after_takeover'] = 1
        result = verdict.evaluate(SCENARIO, run)
        self.assertEqual((result['verdict'], result['structural_violations']), ('REGRESSION', ['I5']))

    def test_wrong_takeover_count_is_structural(self):
        scenario = dict(SCENARIO, expect=dict(SCENARIO['expect'], takeovers=[0]))
        self.assertIn('I5', verdict.evaluate(scenario, _run())['structural_violations'])

    def test_typed_failure_against_a_gap(self):
        run = _run()
        p = run['final']['product']
        p['settlements'][0].update(disposition='FAILED', error_code='PIPELINE_CODE_FAILED')
        p['replay']['terminal'] = [{'event_type': 'execution.failed', 'code': 'PIPELINE_CODE_FAILED', 'safe_message': 'x'}]
        p['chat_answers'][0].update(is_error=True, error_code='PIPELINE_CODE_FAILED')
        run['answer'] = {'oracle_ok': False}
        self.assertEqual(verdict.evaluate(SCENARIO, run)['verdict'], 'REGRESSION')
        gap = dict(SCENARIO, gap='G-SUP-01', expected_gap_class='F')
        result = verdict.evaluate(gap, run)
        self.assertEqual((result['observed'], result['verdict']), ('F', 'EXPECTED-GAP'))

    def test_failure_without_support_reference_is_not_typed(self):
        run = _run()
        p = run['final']['product']
        p['settlements'][0].update(disposition='FAILED', error_code='INTERNAL')
        p['replay']['terminal'] = [{'event_type': 'execution.failed', 'code': 'INTERNAL', 'safe_message': 'x'}]
        p['chat_answers'][0].update(is_error=True, message_id='other')
        self.assertEqual(verdict.evaluate(SCENARIO, run)['observed'], 'L')

    def test_undrained_stream_is_structural(self):
        run = _run()
        run['final']['jsz']['consumer']['num_ack_pending'] = 1
        self.assertIn('I7', verdict.evaluate(SCENARIO, run)['structural_violations'])

    def test_orphan_runtime_is_l(self):
        run = _run()
        run['after_grace']['runtime_present'] = {'r1': True}
        self.assertEqual(verdict.evaluate(SCENARIO, run)['observed'], 'L')

    def test_committed_preparation_is_not_downloaded_again(self):
        run = _run()
        prep = {'job_key': 'p1', 'activation_id': 'pa', 'job_matched': True, 'runtime_id': 'pr', 'phase': 'completed',
                'runtime_label': 'plabel', 'audience': verdict.PREPARATION_AUDIENCE, 'has_preparation_bundle': True}
        run['pre_fault']['agentstate']['sandbox'].append(dict(prep))
        run['final']['agentstate']['sandbox'].append(dict(prep))
        run['events']['plabel'] = {'containers': ['pr'], 'start': 1, 'start_times': [1791540000000]}
        scenario = dict(SCENARIO, _oracle={'jobs': 2, 'preparations': 1})
        self.assertEqual(verdict.evaluate(scenario, run)['invariants']['I2P']['status'], 'hold')
        run['events']['plabel'] = {'containers': ['pr', 'pr2'], 'start': 2,
                                   'start_times': [1791540000000, 1791540030000]}
        result = verdict.evaluate(scenario, run)
        self.assertEqual((result['invariants']['I2P']['status'], result['observed']), ('violated', 'L'))

    def test_stop_may_delete_the_empty_answer_group(self):
        run = _run()
        p = run['final']['product']
        p['settlements'][0]['disposition'] = 'CANCELLED'
        p['chat_answers'] = []
        scenario = dict(SCENARIO, expect=dict(SCENARIO['expect'], settlement='CANCELLED'))
        result = verdict.evaluate(scenario, run)
        self.assertEqual((result['invariants']['I1']['status'], result['observed']), ('hold', 'R'))
        p['settlements'][0]['disposition'] = 'SUCCEEDED'
        self.assertEqual(verdict.evaluate(SCENARIO, run)['invariants']['I1']['status'], 'violated')

    def test_inconclusive_is_never_credited(self):
        result = verdict.evaluate(SCENARIO, {'inconclusive': 'trigger code_runtime_running timeout'})
        self.assertEqual(result['verdict'], 'INCONCLUSIVE')


class FormParserTest(unittest.TestCase):
    def test_authorize_form_fields_and_button(self):
        form = _FormParser()
        form.feed('<form method="post" action="/authorize?x=1"><input type="hidden" name="state" value="s">'
                  '<input name="sub"><button name="action" value="authorize">Authorize</button></form>')
        self.assertEqual(form.action, '/authorize?x=1')
        self.assertEqual(form.fields, {'state': 's', 'sub': ''})
        self.assertIn(('action', 'authorize'), form.buttons)


class ReportTest(unittest.TestCase):
    def test_all_repeats_must_pass_for_a_matrix_proposal(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / 'run'
            for index, outcome in enumerate(('PASS', 'PASS', 'INCONCLUSIVE'), start=1):
                d = root / 'CR-WK-P10' / f'r{index}'
                d.mkdir(parents=True)
                (d / 'evidence.json').write_text(json.dumps({'scenario': 'CR-WK-P10', 'execution_id': str(index), 'faults': []}))
                (d / 'verdict.json').write_text(json.dumps({'verdict': outcome, 'observed': 'R' if outcome == 'PASS' else None}))
            row = report.build([root])['scenarios'][0]
            self.assertEqual((row['final'], row['counted_repeats']), ('PASS', 2))
            self.assertIsNone(row['matrix_proposal'], 'two counted repeats of three do not move a cell')


class CompiledManifestTest(unittest.TestCase):
    ROOT = pathlib.Path(__file__).resolve().parents[3]

    def _load(self, relative, name):
        import importlib.util
        spec = importlib.util.spec_from_file_location(name, self.ROOT / relative)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_flags_hash_matches_shipped_producer(self):
        producer = self._load('scripts/runtime/rust_compiled_release_manifest.py', 'rcrm')
        self.assertEqual(compiled_manifest.flags_hash(), producer.flags(None))

    def test_vendor_hash_sorts_by_path_and_is_compact_utf8(self):
        a = {'path': 'b/x.rs', 'bytes': 3, 'sha256': 'a' * 64, 'mode': 0o644}
        b = {'path': 'a.toml', 'bytes': 1, 'sha256': 'b' * 64, 'mode': 0o444}
        c = {'path': 'é.rs', 'bytes': 2, 'sha256': 'c' * 64, 'mode': 0o644}
        raw = ('[{"path":"a.toml","bytes":1,"sha256":"%s","mode":292},{"path":"b/x.rs","bytes":3,"sha256":"%s",'
               '"mode":420},{"path":"é.rs","bytes":2,"sha256":"%s","mode":420}]' % ('b' * 64, 'a' * 64, 'c' * 64)).encode()
        expected = compiled_manifest.domain_hash(b'elitea.rust.vendor-tree.v1\0', raw)
        self.assertEqual(compiled_manifest.vendor_hash([a, b, c]), expected)
        self.assertEqual(compiled_manifest.vendor_hash([c, a, b]), expected)

    def test_toolchain_hash_known_bytes(self):
        import hashlib
        raw = b'[[1,2],[3]]'
        want = hashlib.sha256(b'elitea.rust.toolchain.v1\0' + len(raw).to_bytes(8, 'big') + raw).hexdigest()
        self.assertEqual(compiled_manifest.toolchain_hash(b'\x01\x02', b'\x03'), want)

    def test_manifest_bytes_canonical_and_passes_deploy_checker(self):
        measured = {name: format(i + 1, 'x') * 64 for i, name in enumerate(compiled_manifest.MEASURED_FIELDS)}
        image = 'sha256:' + 'd' * 64
        raw = compiled_manifest.manifest_bytes(measured, image, 'linux/arm64/gnu', 'aarch64-unknown-linux-gnu', 'p-v1')
        value = json.loads(raw)
        binding = value['profiles'][0]['binding']
        self.assertEqual(list(binding), list(compiled_manifest.FIELDS))
        self.assertEqual(len(binding), 19)
        self.assertEqual(value['profiles'][0]['dependency_bundle_sha256'], '')
        self.assertEqual(raw, json.dumps(value, separators=(',', ':'), ensure_ascii=False).encode())
        self.assertFalse(raw.endswith(b'\n'))
        checker = self._load('deploy/scripts/check-compiled-sandbox-material.py', 'cdsm')
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp).resolve() / 'profiles.json'
            digest = compiled_manifest.write(path, raw)
            self.assertEqual(path.stat().st_mode & 0o777, 0o644)
            self.assertEqual(len(checker.profiles(str(path), digest)), 1)


if __name__ == '__main__':
    unittest.main()
