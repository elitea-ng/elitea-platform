"""Self-tests for the language matrix: fixtures per language, the LM catalog, NOT-SUPPORTED, and the `npm` stack
feature (generated Worker and Supervisor configs). Standard library only; no stack and no Docker needed."""
import ast
import asyncio
import contextlib
import copy
import io
import json
import os
import pathlib
import re
import shutil
import sys
import tempfile
import unittest
from unittest import mock

SUITE = pathlib.Path(__file__).resolve().parents[1]
REPO = SUITE.parents[1]
sys.path.insert(0, str(SUITE))

from fixtures import seed  # noqa: E402
from lib import catalog, report, runner, stack_compose, triggers, verdict  # noqa: E402
from lib.common import HarnessError, is_preparation_audience  # noqa: E402

LANGS = ('python', 'javascript', 'typescript', 'rust')
NPM_REASON = 'JS/TS npm preparation needs the native Deno profile (stack feature `npm`)'


def _rendered(lang, kind, sleep_ms=50000):
    return seed.render(seed.LANGUAGE_TEMPLATE, sleep_ms, language=lang, kind=kind)


def _nodes(text):
    """(node ids, languages, code blocks) from the rendered YAML without a YAML parser."""
    ids = re.findall(r'^  - id: (\S+)$', text, re.M)
    langs = re.findall(r'^    language: (\S+)$', text, re.M)
    blocks, current = [], None
    for line in text.splitlines():
        if line == '      value: |':
            current = []
            blocks.append(current)
        elif current is not None and (line.startswith('        ') or not line.strip()):
            current.append(line[8:])
        else:
            current = None
    return ids, langs, ['\n'.join(b) for b in blocks]


class LanguageFixtureTest(unittest.TestCase):
    def kinds(self):
        for lang in LANGS:
            for kind in seed.LANGUAGES[lang]:
                yield lang, kind

    def test_every_language_and_kind_renders_with_its_nodes(self):
        self.assertEqual({k for _, k in self.kinds()}, {'sleep', 'sleep-npm', 'throw'})
        for lang, kind in self.kinds():
            text = _rendered(lang, kind)
            ids, langs, blocks = _nodes(text)
            self.assertNotIn('{{', text, (lang, kind))
            self.assertNotIn('# @', text)
            expect = ['thrower'] if kind == 'throw' else ['sleeper', 'sentinel_9999']
            self.assertEqual(ids, expect, (lang, kind))
            self.assertEqual(langs, [lang] * len(expect), (lang, kind))
            self.assertEqual(len(blocks), len(expect))
            self.assertIn(f'entry_point: {expect[0]}', text)
            # Every code line sits under `value: |` at the block indent, so none leaked into the node keys.
            self.assertTrue(all(b.strip() for b in blocks))
            self.assertEqual(text.count('    transition:'), len(expect))

    def test_sleep_and_throw_shapes(self):
        for lang in LANGS:
            _, _, (sleeper, sentinel) = _nodes(_rendered(lang, 'sleep'))
            self.assertIn('50000', sleeper if lang != 'python' else sleeper.replace('50000 / 1000', '50000'))
            self.assertIn('started_at_ms', sleeper)
            self.assertIn('nonce', sleeper)
            self.assertIn('count', sleeper)
            self.assertIn('CRASHRESULT', sentinel)
            self.assertIn('SENTINEL9999', sentinel)
            self.assertIn('probe', sentinel)
            _, _, (thrower,) = _nodes(_rendered(lang, 'throw'))
            self.assertIn('CRASHTHROW deliberate fixture error', thrower)
        self.assertIn('std::thread::sleep', _nodes(_rendered('rust', 'sleep'))[2][0])
        self.assertIn('std::process::id()', _nodes(_rendered('rust', 'sleep'))[2][0])
        self.assertIn('Err("CRASHTHROW', _nodes(_rendered('rust', 'throw'))[2][0])
        self.assertIn('raise ValueError("CRASHTHROW', _nodes(_rendered('python', 'throw'))[2][0])
        self.assertIn('throw new Error("CRASHTHROW', _nodes(_rendered('typescript', 'throw'))[2][0])
        self.assertIn(': number', _nodes(_rendered('typescript', 'sleep'))[2][0])
        self.assertNotIn(': number', _nodes(_rendered('javascript', 'sleep'))[2][0])

    def test_npm_kind_declares_one_literal_npm_import_for_js_and_ts_only(self):
        for lang in ('javascript', 'typescript'):
            sleeper = _nodes(_rendered(lang, 'sleep-npm'))[2][0]
            self.assertEqual(re.findall(r'^import \w+ from "npm:[^"]+";$', sleeper, re.M), ['import slugify from "npm:slugify@1.6.6";'])
        for lang in ('python', 'rust'):
            self.assertNotIn('sleep-npm', seed.LANGUAGES[lang])
        with self.assertRaises(ValueError):
            _rendered('python', 'sleep-npm')

    def test_python_fixture_runs_and_meets_the_runner_contract(self):
        """The Python sources are executed (Pyodide semantics: top-level await, last expression is the result)."""
        def last_expression(code, names):
            tree = ast.parse(code, mode='exec')
            last = tree.body.pop()
            self.assertIsInstance(last, ast.Expr)
            flags = ast.PyCF_ALLOW_TOP_LEVEL_AWAIT
            first = eval(compile(tree, 'fixture', 'exec', flags=flags), names)
            if asyncio.iscoroutine(first):
                asyncio.run(first)
            return eval(compile(ast.Expression(last.value), 'fixture', 'eval', flags=flags), names)
        text = _rendered('python', 'sleep', sleep_ms=1)
        sleeper, sentinel = _nodes(text)[2]
        probe = last_expression(sleeper, {})
        self.assertEqual(set(probe), {'started_at_ms', 'nonce', 'count'})
        self.assertEqual(probe['count'], 2)
        answer = last_expression(sentinel, {'elitea_state': {'probe': probe}})
        match = runner.RESULT_RE.search(answer)
        self.assertEqual(json.loads(match.group(1)), probe)
        with self.assertRaises(ValueError):
            exec(compile(_nodes(_rendered('python', 'throw'))[2][0], 'fixture', 'exec'), {})

    def test_existing_javascript_fixtures_still_render(self):
        for template, sleep_ms in seed.PIPELINE_VARIANTS.values():
            text = seed.render(template, sleep_ms)
            self.assertNotIn('{{', text)
        self.assertIn('language: javascript', seed.render('code-nonce-sleep.yaml', 50000))
        self.assertIn('setTimeout(resolve, 150000)', seed.render('code-nonce-sleep.yaml', 150000))
        self.assertIn('nonce', seed.render('rust-compiled.yaml', marker='abc'))

    def test_fixture_names_cover_the_catalog(self):
        self.assertEqual(set(seed.LANGUAGE_VARIANTS),
                         {f'code-{lang}-{kind}' for lang in LANGS for kind in ('sleep-50', 'throw')})
        self.assertEqual(set(seed.FEATURE_PIPELINES['npm']), {'code-javascript-npm-50', 'code-typescript-npm-50'})
        known = set(seed.LANGUAGE_VARIANTS) | set(seed.FEATURE_PIPELINES['npm']) | set(seed.PIPELINE_VARIANTS)
        for sid in catalog.all_ids():
            s = catalog.load(sid)
            if sid.startswith('LM-') and not s.get('lm_id'):
                self.assertIn(s['fixture'], known, sid)

    def test_seed_variants_creates_only_the_missing(self):
        client = mock.Mock()
        client.create_pipeline.side_effect = lambda project, name, text: (len(name), len(text))
        made = seed.seed_variants(client, 7, 'sfx', have={'code-throw': {}, 'code-rust-throw': {}})
        self.assertNotIn('code-throw', made)
        self.assertNotIn('code-rust-throw', made)
        self.assertIn('code-python-sleep-50', made)
        self.assertEqual(made['code-rust-sleep-50']['name'], 'crash-code-rust-sleep-50-sfx')
        self.assertEqual(made['code-rust-sleep-50']['sleep_ms'], 50000)


class AudienceTest(unittest.TestCase):
    def test_every_preparation_audience_is_recognised(self):
        for audience in ('dns:elitea-sandbox-preparation', 'dns:elitea-sandbox-javascript-preparation',
                         'dns:elitea-sandbox-typescript-preparation'):
            self.assertTrue(is_preparation_audience(audience))
        for audience in ('dns:elitea-sandbox-deno', 'dns:elitea-sandbox-deno-native', 'dns:elitea-sandbox-rust', None, ''):
            self.assertFalse(is_preparation_audience(audience))

    def test_native_preparers_are_not_taken_for_the_code_runtime(self):
        jobs = [{'job_key': 'p', 'audience': 'dns:elitea-sandbox-javascript-preparation', 'phase': 'dispatched',
                 'runtime_id': 'r', 'has_preparation_bundle': False, 'dispatched_at': '2026-10-09T10:00:00+00:00'}]
        ctx = mock.Mock()
        ctx.collector.sql.side_effect = lambda db, name, **kw: (
            [{'state': 'RUNNING', 'settled_at': None}] if name == 'execution' else jobs)
        ctx.collector.container_running.return_value = True
        self.assertFalse(triggers.code_runtime_running(ctx, {})[0])
        self.assertTrue(triggers.preparation_resolving(ctx, {'min_age_s': 0})[0])


class LanguageMatrixCatalogTest(unittest.TestCase):
    def test_expansion_ids_and_validation(self):
        ids = catalog.all_ids()
        expanded = {f'LM-{f}-{lang}' for f in ('BASE', 'WK', 'SK', 'MK', 'STOP', 'THROW') for lang in LANGS}
        prep = {'LM-PREP-python', 'LM-PREP-WK-python', 'LM-PREP-rust', 'LM-PREP-MK-rust', 'LM-PREP-javascript',
                'LM-PREP-WK-javascript', 'LM-PREP-typescript', 'LM-PREP-WK-typescript'}
        self.assertTrue((expanded | prep) <= set(ids))
        self.assertEqual({i for i in ids if i.startswith('LM-')}, expanded | prep)
        for sid in expanded:
            s = catalog.load(sid)
            self.assertEqual(s['id'], sid)
            self.assertEqual(s['language'], sid.rsplit('-', 1)[1])
            self.assertIsNone(re.search(r'\{\w+\}', json.dumps(s)), sid)

    def test_tiers_put_base_first_for_each_language(self):
        lm = [s['id'] for s in catalog.select(['LM'])]
        for lang in LANGS:
            rows = [s['id'] for s in catalog.select([f'LM-{lang}'])]
            self.assertEqual(rows[:6], [f'LM-{f}-{lang}' for f in ('BASE', 'WK', 'SK', 'MK', 'STOP', 'THROW')])
            self.assertTrue(set(rows) <= set(lm))
        self.assertEqual(lm.index('LM-BASE-python'), 0)
        for lang in LANGS:
            self.assertLess(lm.index(f'LM-BASE-{lang}'), lm.index(f'LM-WK-{lang}'))
        self.assertIn('LM-PREP', catalog.tiers())

    def test_p10_faults_match_their_original_scenarios(self):
        for fault, original in (('WK', 'CR-WK-P10'), ('SK', 'CR-SK-P10'), ('MK', 'CR-MK-P10')):
            source = catalog.load(original)
            for lang in LANGS:
                s = catalog.load(f'LM-{fault}-{lang}')
                self.assertEqual(s['faults'], source['faults'])
                self.assertEqual(s['trigger']['until'], source['trigger']['until'])
                self.assertEqual(s['trigger']['params'], source['trigger']['params'])
                self.assertEqual(s['expect']['takeovers'], source['expect']['takeovers'])
                self.assertEqual((s['target'], s['expect']['settlement']), ('R', 'SUCCEEDED'))
                self.assertEqual(s['fixture'], f'code-{lang}-sleep-50')
                self.assertEqual(s['repeat'], 3)
        self.assertGreater(catalog.load('LM-WK-rust')['trigger']['timeout_s'], catalog.load('LM-WK-javascript')['trigger']['timeout_s'])
        self.assertGreater(catalog.load('LM-BASE-rust')['expect']['budget_s'], catalog.load('LM-BASE-javascript')['expect']['budget_s'])

    def test_base_stop_and_throw(self):
        for lang in LANGS:
            base = catalog.load(f'LM-BASE-{lang}')
            self.assertEqual((base['faults'], base['trigger']['until'], base['target'], base['repeat']), ([], 'admitted', 'R', 1))
            self.assertTrue(base['baseline'])
            stop = catalog.load(f'LM-STOP-{lang}')
            self.assertEqual(stop['faults'], [{'action': 'user_stop'}])
            self.assertEqual((stop['expect']['settlement'], stop['expect']['takeovers']), ('CANCELLED', [0]))
            self.assertEqual(stop['trigger']['until'], 'code_runtime_running')
            throw = catalog.load(f'LM-THROW-{lang}')
            self.assertEqual(throw['target'], 'F')
            self.assertEqual(throw['fixture'], f'code-{lang}-throw')
            self.assertEqual(throw['faults'], [])
            self.assertEqual(throw['expect']['settlement'], 'FAILED')
            self.assertEqual(throw['expect']['replay_codes'], ['PIPELINE_CODE_FAILED'])
            self.assertTrue(throw['expect']['no_reconcile_card'])
            self.assertEqual(throw['expect']['replay_codes'], catalog.load('BASE-code-throw')['expect']['replay_codes'])

    def test_existing_scenarios_are_untouched_and_win_a_name_clash(self):
        self.assertEqual(catalog.load('CR-WK-P10')['id'], 'CR-WK-P10')
        self.assertNotIn('lm_id', catalog.load('CR-WK-P10'))
        with self.assertRaises(HarnessError):
            catalog.load('LM-NOPE-python')

    def test_package_rows_reference_the_existing_scenarios(self):
        self.assertEqual(catalog.aliases(), {
            'LM-PREP-python': 'BASE-python-prepared', 'LM-PREP-WK-python': 'CR-WK-P09',
            'LM-PREP-rust': 'BASE-rust-compiled', 'LM-PREP-MK-rust': 'CR-MK-P11'})
        s = catalog.load('LM-PREP-python')
        self.assertEqual((s['id'], s['lm_id'], s['fixture']), ('BASE-python-prepared', 'LM-PREP-python', 'python-prepared-sleep-50'))
        self.assertEqual(s['requires']['feature'], 'preparation')
        self.assertEqual(catalog.load('LM-PREP-MK-rust')['requires']['feature'], 'compiled')
        self.assertNotIn('requires', catalog.load('BASE-python-prepared'))

    def test_npm_rows_need_the_npm_feature(self):
        off, on = mock.Mock(), mock.Mock()
        off.feature.side_effect = lambda name: False
        on.feature.side_effect = lambda name: name == 'npm'
        for lang in ('javascript', 'typescript'):
            base, kill = catalog.load(f'LM-PREP-{lang}'), catalog.load(f'LM-PREP-WK-{lang}')
            self.assertEqual(base['fixture'], f'code-{lang}-npm-50')
            self.assertTrue(base['baseline'])
            self.assertEqual(kill['trigger']['until'], 'preparation_resolving')
            self.assertEqual(kill['faults'][0], {'action': 'kill9', 'target': 'worker'})
            self.assertEqual(kill['expect']['takeovers'], [1])
            for s in (base, kill):
                self.assertEqual(catalog.not_supported_reason(s, off), NPM_REASON)
                self.assertIsNone(catalog.not_supported_reason(s, on))
        self.assertIsNone(catalog.not_supported_reason(catalog.load('LM-WK-rust'), off))

    def test_select_runs_a_shared_proof_once_and_keeps_the_lm_ids(self):
        rows = catalog.select(['LM', 'T0b'])
        ids = [s['id'] for s in rows]
        self.assertEqual(len(ids), len(set(ids)))
        proof = next(s for s in rows if s['id'] == 'BASE-python-prepared')
        self.assertEqual(proof['lm_ids'], ['LM-PREP-python'])
        plain = catalog.select(['BASE-python-prepared'])[0]
        self.assertNotIn('lm_ids', plain)

    def test_unsupported_map_marks_rows_and_is_never_a_silent_skip(self):
        spec = json.loads(catalog.MATRIX.read_text())
        spec['unsupported'] = {'LM-STOP-rust': 'no stop path for compiled Rust', 'LM-PREP-WK-python': 'not here'}
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / 'language-matrix.json'
            path.write_text(json.dumps(spec))
            with mock.patch.object(catalog, 'MATRIX', path):
                rows = {s['id']: s for s in catalog.select(['LM-rust', 'LM-python'])}
                stack = mock.Mock()
                stack.feature.return_value = True
                self.assertEqual(catalog.not_supported_reason(rows['LM-STOP-rust'], stack), 'no stop path for compiled Rust')
                self.assertEqual(catalog.not_supported_reason(rows['CR-WK-P09'], stack), 'not here')
                self.assertIn('LM-STOP-rust', [s['id'] for s in catalog.select(['LM'])])
                spec['unsupported'] = {'LM-NOPE-x': 'x'}
                path.write_text(json.dumps(spec))
                with self.assertRaises(HarnessError):
                    catalog.all_ids()

    def test_unknown_placeholder_is_refused(self):
        spec = json.loads(catalog.MATRIX.read_text())
        spec['faults']['WK']['cell'] = 'Worker {nope}'
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / 'language-matrix.json'
            path.write_text(json.dumps(spec))
            with mock.patch.object(catalog, 'MATRIX', path), self.assertRaises(HarnessError):
                catalog.all_ids()

    def test_requires_must_name_feature_and_reason(self):
        s = catalog.load('LM-PREP-javascript')
        s['requires'] = {'feature': 'npm'}
        with self.assertRaises(HarnessError):
            catalog.validate(s)


class NotSupportedTest(unittest.TestCase):
    def _stack(self, **features):
        stack = mock.Mock(spec=['feature'])
        stack.feature.side_effect = lambda name: features.get(name, False)
        return stack

    def test_runner_records_the_verdict_and_touches_nothing(self):
        scenario = catalog.select(['LM-PREP-WK-javascript'])[0]
        with tempfile.TemporaryDirectory() as tmp:
            out = pathlib.Path(tmp) / 'r1'
            log = io.StringIO()
            with contextlib.redirect_stdout(log):
                # The stack mock has no collector, client or compose: any step beyond the feature check would fail.
                rec, result = runner.run_scenario(self._stack(), None, scenario, {}, out)
            self.assertIn('NOT-SUPPORTED', log.getvalue())
            self.assertIn(NPM_REASON, log.getvalue())
            self.assertEqual(result['verdict'], 'NOT-SUPPORTED')
            self.assertEqual(result['reason'], NPM_REASON)
            stored = json.loads((out / 'verdict.json').read_text())
            self.assertEqual((stored['verdict'], stored['reason']), ('NOT-SUPPORTED', NPM_REASON))
            evidence = json.loads((out / 'evidence.json').read_text())
            self.assertEqual((evidence['scenario'], evidence['lm_ids']), ('LM-PREP-WK-javascript', []))
            # verify re-evaluates from the stored evidence and reaches the same verdict
            self.assertEqual(verdict.evaluate(scenario, evidence)['verdict'], 'NOT-SUPPORTED')

    def test_feature_on_does_not_short_circuit(self):
        scenario = catalog.load('LM-PREP-javascript')
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(runner, 'Collector') as col:
            col.side_effect = RuntimeError('reached the stack')
            with self.assertRaises(RuntimeError):
                runner.run_scenario(self._stack(npm=True), None, scenario, {}, pathlib.Path(tmp) / 'r1')

    def _write(self, root, sid, verdict_name, reason=None):
        d = pathlib.Path(root) / sid / 'r1'
        d.mkdir(parents=True)
        (d / 'evidence.json').write_text(json.dumps({'scenario': sid, 'faults': []}))
        (d / 'verdict.json').write_text(json.dumps({'verdict': verdict_name, 'reason': reason,
                                                    'observed': 'R' if verdict_name == 'PASS' else None}))

    def test_report_shows_not_supported_with_its_reason_and_never_proposes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / 'run'
            self._write(root, 'LM-PREP-javascript', 'NOT-SUPPORTED', NPM_REASON)
            self._write(root, 'LM-BASE-rust', 'PASS')
            out = report.build([root])
            rows = {r['scenario']: r for r in out['scenarios']}
            self.assertEqual(rows['LM-PREP-javascript']['final'], 'NOT-SUPPORTED')
            self.assertEqual(rows['LM-PREP-javascript']['counted_repeats'], 0)
            self.assertIsNone(rows['LM-PREP-javascript']['matrix_proposal'])
            self.assertEqual(rows['LM-BASE-rust']['final'], 'PASS')
            text = report.markdown(out)
            self.assertIn('| LM-PREP-javascript |', text)
            self.assertIn('NOT-SUPPORTED', text)
            self.assertIn(NPM_REASON, text)

    def test_not_supported_never_hides_a_regression_and_is_not_counted(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / 'run'
            self._write(root, 'LM-PREP-javascript', 'NOT-SUPPORTED', NPM_REASON)
            second = pathlib.Path(tmp) / 'run2'
            self._write(second, 'LM-PREP-javascript', 'REGRESSION')
            row = report.build([root, second])['scenarios'][0]
            self.assertEqual((row['final'], row['counted_repeats']), ('REGRESSION', 1))
            third = pathlib.Path(tmp) / 'run3'
            self._write(third, 'LM-PREP-javascript', 'INCONCLUSIVE', 'x')
            self.assertEqual(report.build([root, third])['scenarios'][0]['final'], 'INCONCLUSIVE')

    def test_report_shows_the_lm_id_and_the_scenario_that_proved_it(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp) / 'run'
            self._write(root, 'BASE-python-prepared', 'PASS')
            out = report.build([root])
            rows = {r['scenario']: r for r in out['scenarios']}
            self.assertEqual(rows['LM-PREP-python']['proved_by'], 'BASE-python-prepared')
            self.assertEqual(rows['LM-PREP-python']['final'], 'PASS')
            self.assertIsNone(rows['LM-PREP-python']['matrix_proposal'])
            self.assertIn('LM-PREP-python (proved by BASE-python-prepared)', report.markdown(out))
            self.assertNotIn('LM-PREP-rust', rows)


def _stack(features, tmp, **sandbox):
    cfg = copy.deepcopy(stack_compose.CONFIG_TEMPLATE)
    cfg['features'] = dict(features)
    cfg['sandbox'].update({'supervisor_image': 's', 'deno_image': 'd', 'rust_image': 'r'}, **sandbox)
    root = pathlib.Path(tmp) / 'stack'
    root.mkdir()
    (root / 'stack.json').write_text(json.dumps(cfg))
    return stack_compose.Stack(root)


DIGESTS = {'deno': 'sha256:' + 'd' * 64, 'rust': 'sha256:' + '7' * 64}
MIB = 1024 * 1024


class NpmFeatureTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def material(self, features, arch='arm64'):
        stack = _stack(features, self.tmp.name)

        def fake_copy(src, dst):
            pathlib.Path(dst).write_bytes(b'x')
            os.chmod(dst, 0o600)
        patches = [mock.patch.object(stack_compose, 'run'), mock.patch.object(stack_compose, '_copy_private', fake_copy),
                   mock.patch.object(stack_compose, 'image_id', side_effect=lambda ref: DIGESTS[{'d': 'deno', 'r': 'rust'}[ref]]),
                   mock.patch.object(stack_compose, 'docker_arch', return_value=arch),
                   mock.patch.object(stack_compose.Stack, '_issue_owner_certificate')]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        stack.prepare_material('python3')
        return stack

    def worker(self, stack):
        return json.loads((stack.material / 'worker-runtime.json').read_text())

    def test_off_by_default_and_the_generated_config_is_unchanged(self):
        stack = self.material({'preparation': True})
        self.assertFalse(stack.feature('npm'))
        runtimes = {r['language']: r for r in self.worker(stack)['sandbox_runtimes']}
        self.assertEqual(set(runtimes), set(LANGS))
        for lang in ('javascript', 'typescript'):
            self.assertEqual(runtimes[lang]['audience'], 'dns:elitea-sandbox-deno')
            self.assertNotIn('preparation', runtimes[lang])
        self.assertNotIn('native_platform', json.dumps(self.worker(stack)))
        self.assertFalse((stack.material / 'sandbox-npm').exists())
        deno = json.loads((stack.material / 'sandbox-deno' / 'config.json').read_text())
        self.assertEqual(deno['languages'], ['python', 'javascript', 'typescript'])
        self.assertNotIn('docker-compose.crash-npm.yml', ' '.join(str(p) for p in stack.overlay_files()))
        self.assertNotIn('native', stack.owner_config())

    def test_on_worker_runtimes(self):
        stack = self.material({'preparation': True, 'npm': True}, arch='amd64')
        runtimes = self.worker(stack)['sandbox_runtimes']
        self.assertLessEqual(len(runtimes), 4)
        by_lang = {r['language']: r for r in runtimes}
        self.assertEqual(sorted(by_lang), sorted(LANGS))
        python = by_lang['python']
        self.assertEqual(python['audience'], 'dns:elitea-sandbox-deno')
        self.assertEqual(python['preparation']['audience'], 'dns:elitea-sandbox-preparation')
        self.assertNotIn('native_platform', python['preparation'])
        self.assertNotIn('preparation', by_lang['rust'])
        for lang, port in (('javascript', 9450), ('typescript', 9451)):
            entry = by_lang[lang]
            self.assertEqual((entry['target'], entry['audience']), ('elitea-sandbox-deno-native:9449', 'dns:elitea-sandbox-deno-native'))
            prep = entry['preparation']
            self.assertEqual(prep['native_platform'], {'os': 'linux', 'arch': 'amd64', 'abi': 'gnu'})
            self.assertEqual((prep['target'], prep['audience']), (f'elitea-sandbox-{lang}-preparation:{port}', f'dns:elitea-sandbox-{lang}-preparation'))
            self.assertEqual(prep['image_digest'], DIGESTS['deno'])
            self.assertLessEqual(prep['timeout_seconds'], 120)
            self.assertEqual(set(prep), {'target', 'audience', 'image_digest', 'policy_revision', 'timeout_seconds', 'native_platform'})
        stock = json.loads((stack.material / 'worker-runtime.stock.json').read_text())
        self.assertTrue(all('preparation' not in r for r in stock['sandbox_runtimes']))

    def test_on_supervisor_configs(self):
        stack = self.material({'preparation': True, 'npm': True})
        root = stack.material / 'sandbox-npm'
        names = sorted(p.name for p in root.iterdir())
        self.assertEqual(names, ['deno-native', 'javascript-preparation', 'typescript-preparation'])
        configs = {n: json.loads((root / n / 'config.json').read_text()) for n in names}
        native = configs['deno-native']
        self.assertEqual((native['purpose'], native['languages']), ('execution', ['javascript', 'typescript']))
        self.assertNotIn('python', native['languages'])
        self.assertNotIn('preparation_network', native)
        self.assertIn('dependency_content', native)
        self.assertEqual(native['native_platform'], {'os': 'linux', 'arch': 'arm64', 'abi': 'gnu'})
        self.assertTrue(512 * MIB <= native['native_workspace_bytes'] <= 4096 * MIB)
        for config in configs.values():
            self.assertGreaterEqual(config['memory_bytes'], config['native_workspace_bytes'] + 128 * MIB)
            self.assertEqual(config['native_platform'], native['native_platform'])
            self.assertEqual(config['image_digest'], DIGESTS['deno'])
            self.assertEqual(config['dependency_content']['staging_root'], f"/run/elitea-sandbox-content/{config['audience'].split('dns:elitea-sandbox-')[1]}")
            self.assertTrue(config['ca_path'].startswith(f"/run/elitea-sandbox/npm/{config['audience'].split('dns:elitea-sandbox-')[1]}/"))
        for lang in ('javascript', 'typescript'):
            config = configs[f'{lang}-preparation']
            self.assertEqual((config['purpose'], config['languages'], config['backend']), ('preparation', [lang], {'kind': 'docker'}))
            self.assertEqual(config['preparation_network'], 'elitea-python-preparation-resolver')
            self.assertLessEqual(config['timeout_seconds'], 120)
        ports = {c['listen_address'] for c in configs.values()} | {'0.0.0.0:9446', '0.0.0.0:9447', '0.0.0.0:9448'}
        self.assertEqual(len(ports), 6)
        owners = {c['owner'] for c in configs.values()}
        self.assertEqual(len(owners), 3)
        deno = json.loads((stack.material / 'sandbox-deno' / 'config.json').read_text())
        self.assertEqual(deno['languages'], ['python'])
        python_prep = json.loads((stack.material / 'sandbox-preparation' / 'config.json').read_text())
        self.assertNotIn('native_platform', python_prep)
        for p in root.rglob('*'):
            if p.is_file():
                self.assertEqual(p.stat().st_mode & 0o777, 0o600, p)

    def test_worker_and_supervisor_agree(self):
        stack = self.material({'preparation': True, 'npm': True})
        configs = {n: json.loads((stack.material / 'sandbox-npm' / n / 'config.json').read_text())
                   for n in ('deno-native', 'javascript-preparation', 'typescript-preparation')}
        for entry in self.worker(stack)['sandbox_runtimes']:
            if entry['language'] in ('javascript', 'typescript'):
                native = configs['deno-native']
                self.assertEqual(entry['target'].rsplit(':', 1)[1], native['listen_address'].rsplit(':', 1)[1])
                self.assertEqual((entry['audience'], entry['policy_revision'], entry['image_digest']),
                                 (native['audience'], native['policy_revision'], native['image_digest']))
                prep, config = entry['preparation'], configs[f"{entry['language']}-preparation"]
                self.assertEqual(prep['target'].rsplit(':', 1)[1], config['listen_address'].rsplit(':', 1)[1])
                self.assertEqual((prep['audience'], prep['policy_revision'], prep['image_digest'], prep['native_platform']),
                                 (config['audience'], config['policy_revision'], config['image_digest'], config['native_platform']))

    def test_overlay_files_owner_config_and_dependencies(self):
        with self.assertRaises(HarnessError):
            _stack({'npm': True}, self.tmp.name)
        shutil.rmtree(pathlib.Path(self.tmp.name) / 'stack')
        stack = _stack({'preparation': True, 'npm': True}, self.tmp.name)
        names = [p.name for p in stack.overlay_files()]
        self.assertEqual(names[names.index('docker-compose.crash-preparation.yml') + 1], 'docker-compose.crash-npm.yml')
        owner = json.loads(stack.owner_config())
        self.assertIn({'audience': 'dns:elitea-sandbox-deno-native', 'https_origin': 'https://elitea-sandbox-deno-native:9449'}, owner['supervisors'])
        self.assertEqual(len(owner['supervisors']), 3)

    def test_overlay_file_matches_the_generated_names(self):
        text = (REPO / 'deploy/docker-compose.crash-npm.yml').read_text()
        stack = _stack({'preparation': True, 'npm': True}, self.tmp.name)
        configs = stack.npm_configs(DIGESTS['deno'], 'arm64')
        self.assertEqual(sorted(re.findall(r'/run/elitea-sandbox/npm/([\w-]+)/config\.json', text)), sorted(configs))
        for name, config in configs.items():
            self.assertIn(f"/run/elitea-sandbox-content/{name}:", text)
            self.assertIn(config['audience'], text)
            self.assertIn(name, text)
        for alias in ('elitea-sandbox-deno-native', 'elitea-sandbox-javascript-preparation', 'elitea-sandbox-typescript-preparation'):
            self.assertIn(alias, re.search(r'aliases: \[(.*?)\]', text).group(1))
        self.assertIn('crash_sandbox:', text)

    def test_docker_arch(self):
        for raw, arch in (('aarch64', 'arm64'), ('x86_64', 'amd64'), ('arm64', 'arm64')):
            with mock.patch.object(stack_compose, 'run') as run:
                run.return_value.stdout = f'{raw}\n'.encode()
                self.assertEqual(stack_compose.docker_arch(), arch)
                self.assertEqual(run.call_args[0][0], ['docker', 'info', '--format', '{{.Architecture}}'])
        with mock.patch.object(stack_compose, 'run') as run:
            run.return_value.stdout = b'riscv64\n'
            with self.assertRaises(HarnessError):
                stack_compose.docker_arch()

    def test_memory_floor_is_enforced_when_the_stack_asks_for_less(self):
        stack = _stack({'preparation': True, 'npm': True}, self.tmp.name, memory_bytes=256 * MIB, preparation_memory_bytes=300 * MIB,
                       preparation_timeout_seconds=600)
        for config in stack.npm_configs(DIGESTS['deno'], 'arm64').values():
            self.assertGreaterEqual(config['memory_bytes'], config['native_workspace_bytes'] + 128 * MIB)
        self.assertEqual(stack.npm_configs(DIGESTS['deno'], 'arm64')['javascript-preparation']['timeout_seconds'], 120)


if __name__ == '__main__':
    unittest.main()
