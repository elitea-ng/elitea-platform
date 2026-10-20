"""Tests for gen_orders.py. Run: python3 -m unittest discover -s scripts/acceptance/etl/tests -v

The core test re-derives every manifest number from the written CSV bytes with an independent implementation of the
cleaning rules and expectations (csv module, regex, strptime, Decimal quantize), so the manifest cannot agree with
itself by construction."""
import contextlib
import csv
import datetime as dt
import io
import json
import pathlib
import re
import sys
import tempfile
import unittest
from decimal import ROUND_HALF_UP, Decimal

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import gen_orders  # noqa: E402

RATES = {k: Decimal(v) for k, v in json.loads((HERE.parent / 'fx_rates.json').read_text())['rates'].items()}
STAMP = re.compile(r'^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$')


def failing_expectations(f):
    """Every expectation a well-formed row fails, uniqueness excluded (evaluated by the caller)."""
    failed = []
    try:
        ok = bool(STAMP.match(f['ordered_at'])) and dt.datetime.strptime(f['ordered_at'], '%Y-%m-%dT%H:%M:%SZ')
    except ValueError:
        ok = False
    if not ok:
        failed.append('valid_date')
    try:
        if int(f['qty']) <= 0:
            failed.append('positive_qty')
    except ValueError:
        failed.append('positive_qty')
    if f['currency'] not in RATES:
        failed.append('known_currency')
    return failed


def evaluate(batch_texts):
    """Independent oracle over the batch files: what silver_clean must compute."""
    header = list(gen_orders.HEADER)
    out = {'rejects': dict.fromkeys(gen_orders.EXPECTATIONS, 0), 'reject_list': [], 'batches': [],
           'valid': [], 'multi_failures': 0, 'cleaning': dict.fromkeys(
               ('country_normalized', 'channel_normalized', 'amount_trimmed', 'discount_null', 'ship_date_null',
                'tags_null', 'note_null'), 0), 'rows': 0}
    seen_global = set()
    for batch_no, text in enumerate(batch_texts):
        reader = csv.reader(io.StringIO(text))
        assert next(reader) == header
        seen, counts, total = set(), dict.fromkeys(('valid_date', 'positive_qty', 'known_currency',
                                                    'unique_order_id'), 0), 0
        passed = []
        for fields in reader:
            if len(fields) != len(header):
                out['rejects']['well_formed_line'] += 1
                out['reject_list'].append((0, 'O00000000', batch_no, 'well_formed_line'))
                continue
            total += 1
            f = dict(zip(header, fields))
            c = out['cleaning']
            c['country_normalized'] += f['country'] != f['country'].strip().upper()
            c['channel_normalized'] += f['channel'] != f['channel'].strip().lower()
            c['amount_trimmed'] += f['amount'] != f['amount'].strip()
            for key, name in (('discount_pct', 'discount_null'), ('ship_date', 'ship_date_null'),
                              ('tags', 'tags_null'), ('note', 'note_null')):
                c[name] += not f[key]
            f['country'], f['channel'], f['amount'] = f['country'].strip().upper(), f['channel'].strip().lower(), \
                f['amount'].strip()
            failed = failing_expectations(f)
            if len(failed) > 1:
                out['multi_failures'] += 1
            if failed:
                name = failed[0]
            elif f['order_id'] in seen:
                name = 'unique_order_id'
            else:
                seen.add(f['order_id'])
                passed.append(f)
                continue
            counts[name] += 1
            out['reject_list'].append((int(f['order_seq']), f['order_id'], batch_no, name))
        for f in passed:
            if f['order_id'] in seen_global:
                out['rejects']['unique_order_id_global'] += 1
                out['reject_list'].append((int(f['order_seq']), f['order_id'], batch_no, 'unique_order_id_global'))
            else:
                seen_global.add(f['order_id'])
                out['valid'].append(f)
        for name, value in counts.items():
            out['rejects'][name] += value
        out['rows'] += total
        out['batches'].append({'rows': total, 'rejects': counts,
                               'reject_ppm': sum(counts.values()) * 1_000_000 // total})
    return out


def net(f):
    discount = Decimal(f['discount_pct'] or '0')
    value = Decimal(f['amount']) * (Decimal(100) - discount) / Decimal(100) * RATES[f['currency']]
    return value.quantize(Decimal('0.01'), rounding=ROUND_HALF_UP)


class AcceptanceSet(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.out = pathlib.Path(cls.tmp.name)
        cls.manifest = gen_orders.generate(cls.out)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def batches(self, name):
        entry = self.manifest['datasets'][name]
        return [(self.out / entry['batch_dir'] / f'batch-{k:04d}.csv').read_text()
                for k in range(len(entry['batches']) if isinstance(entry['batches'], list) else entry['batches'])]

    def test_default_shape_meets_the_user_requirements(self):
        entry = self.manifest['datasets']['orders.csv']
        self.assertGreaterEqual(entry['rows_well_formed'], 20_000)
        self.assertTrue(10 <= len(self.manifest['columns']) <= 15)
        text = (self.out / 'orders.csv').read_text()
        self.assertIn('""', text)                       # free text with quotes (RFC 4180 escaping)
        self.assertRegex(text, r'"[^"\n]*,[^"\n]*"')    # free text with commas
        for key in ('discount_null', 'ship_date_null', 'tags_null', 'note_null', 'country_normalized',
                    'channel_normalized', 'amount_trimmed'):
            self.assertGreater(entry['cleaning'][key], 0, key)
        self.assertGreater(entry['file']['chars'], 200_000)   # bigger than one artifact text read

    def test_manifest_matches_an_independent_evaluation(self):
        for name in ('orders.csv', 'orders-gate.csv', 'orders-1k.csv', 'orders-limit.csv'):
            with self.subTest(name=name):
                entry = self.manifest['datasets'][name]
                result = evaluate(self.batches(name))
                self.assertEqual(result['multi_failures'], 0, 'a planted row fails more than one expectation')
                self.assertEqual(result['rejects'], entry['rejects'])
                self.assertEqual(result['rows'], entry['rows_well_formed'])
                self.assertEqual(len(result['valid']), entry['valid_rows'])
                self.assertEqual(result['cleaning'], entry['cleaning'])
                for got, want in zip(result['batches'], entry['batches']):
                    self.assertEqual((got['rows'], got['rejects'], got['reject_ppm']),
                                     (want['rows'], want['rejects'], want['reject_ppm']))
                fact = entry['fact_orders']
                self.assertEqual(sum(net(f) for f in result['valid']), Decimal(fact['sum_net_amount_usd']))
                self.assertEqual(sum(int(f['qty']) for f in result['valid']), fact['sum_qty'])
                self.assertEqual(len({f['order_id'] for f in result['valid']}), fact['rows'])
                daily, country, channel, tags = {}, {}, {}, {}
                for f in result['valid']:
                    for table, key in ((daily, f['ordered_at'][:10]), (country, f['country']),
                                       (channel, f['channel'])):
                        agg = table.setdefault(key, [0, 0, Decimal('0.00')])
                        agg[0] += 1
                        agg[1] += int(f['qty'])
                        agg[2] += net(f)
                    for tag in filter(None, f['tags'].split('|')):
                        tags[tag] = tags.get(tag, 0) + 1
                for table, key, gold in ((daily, 'order_date', 'gold_sales_daily'),
                                         (country, 'country', 'gold_sales_by_country'),
                                         (channel, 'channel', 'gold_sales_by_channel')):
                    self.assertEqual([[r[key], r['orders'], r['qty'], r['net_amount_usd']] for r in entry[gold]],
                                     [[k, a[0], a[1], str(a[2])] for k, a in sorted(table.items())])
                self.assertEqual({r['tag']: r['orders'] for r in entry['tag_counts']}, tags)
                expected = (self.out / entry['expected_rejects']['file']).read_text().splitlines()[1:]
                self.assertEqual(expected, [f'{a},{b},{c},{d}' for a, b, c, d in sorted(result['reject_list'])])

    def test_every_planted_defect_kind_and_the_malformed_line_are_present(self):
        entry = self.manifest['datasets']['orders.csv']
        self.assertEqual(entry['rejects']['well_formed_line'], 1)
        for name in gen_orders.ROW_EXPECTATIONS:
            self.assertGreater(entry['rejects'][name], 0, name)
        self.assertEqual(entry['rejected_rows'], entry['rows_well_formed'] * gen_orders.DEFAULT_BAD_PPM // 1_000_000)
        self.assertEqual(entry['quality_gate']['route'], 'publish')
        self.assertEqual(self.manifest['datasets']['orders-gate.csv']['quality_gate']['route'], 'approval')

    def test_batches_are_the_big_file_and_fit_the_artifact_caps(self):
        for name in ('orders.csv', 'orders-limit.csv'):
            entry = self.manifest['datasets'][name]
            batches = self.batches(name)
            header = ','.join(gen_orders.HEADER) + '\n'
            self.assertEqual(header + ''.join(b[len(header):] for b in batches), (self.out / name).read_text())
            self.assertLessEqual(len(batches), gen_orders.MAX_BATCHES)
            for text in batches:
                self.assertLessEqual(len(text), gen_orders.MAX_BATCH_CHARS)
                self.assertLess(len(text), 200_000)   # the artifact toolkit's whole-file text read cap
            self.assertTrue(all(b['route'] == 'continue' for b in entry['batches']))
        self.assertLess(self.manifest['customers']['chars'], 200_000)

    def test_declared_row_limit_and_limit_plus_one(self):
        limit = self.manifest['datasets']['orders-limit.csv']
        over = self.manifest['datasets']['orders-limit-plus-1.csv']
        self.assertEqual(limit['rows_well_formed'], gen_orders.DECLARED_ROW_LIMIT)
        self.assertEqual(len(limit['batches']), 64)
        self.assertEqual(limit['expected_outcome'], 'accepted')
        self.assertEqual(over['expected_outcome'], 'refused')
        self.assertEqual(over['batches'], 65)
        self.assertNotIn('fact_orders', over)


class Determinism(unittest.TestCase):
    def run_set(self, seed, rows):
        with tempfile.TemporaryDirectory() as tmp:
            manifest = gen_orders.generate(tmp, seed, 'custom', rows)
            files = {p.relative_to(tmp).as_posix(): p.read_bytes() for p in pathlib.Path(tmp).rglob('*') if p.is_file()}
        return manifest, files

    def test_same_seed_same_bytes_and_other_seed_differs(self):
        first, files_a = self.run_set(7, 2_000)
        second, files_b = self.run_set(7, 2_000)
        third, files_c = self.run_set(8, 2_000)
        self.assertEqual(files_a, files_b)
        self.assertEqual(first, second)
        self.assertNotEqual(files_a['orders-2000.csv'], files_c['orders-2000.csv'])

    def test_custom_rows_writes_one_dataset(self):
        manifest, files = self.run_set(gen_orders.DEFAULT_SEED, 20_000)
        self.assertEqual(list(manifest['datasets']), ['orders-20000.csv'])
        self.assertEqual(manifest['datasets']['orders-20000.csv']['rows_well_formed'], 20_000)
        self.assertEqual(len([n for n in files if n.startswith('orders-20000.batches/')]), 50)


class Bounds(unittest.TestCase):
    def test_refusals_are_typed(self):
        customers = gen_orders.build_customers(1)
        for rows, bad_ppm in ((0, 0), (gen_orders.DECLARED_ROW_LIMIT + 2, 0), (1_000, gen_orders.MAX_BAD_PPM + 1)):
            with self.subTest(rows=rows, bad_ppm=bad_ppm), self.assertRaises(gen_orders.GeneratorError):
                gen_orders.build_orders(1, rows, bad_ppm, customers)

    def test_cli_refuses_rows_above_the_declared_limit(self):
        with tempfile.TemporaryDirectory() as tmp, contextlib.redirect_stderr(io.StringIO()), \
                self.assertRaises(SystemExit) as raised:
            gen_orders.main(['--out', tmp, '--rows', str(gen_orders.DECLARED_ROW_LIMIT + 1)])
        self.assertEqual(raised.exception.code, 2)

    def test_net_amount_is_exact_half_up(self):
        self.assertEqual(gen_orders.net_cents(1, '50', Decimal('1.000000')), 1)      # 0.5 cent rounds up
        self.assertEqual(gen_orders.net_cents(1001, '', Decimal('0.500000')), 501)   # 500.5 cents rounds up
        self.assertEqual(gen_orders.net_cents(1000, '12.5', Decimal('1.084300')), 949)


if __name__ == '__main__':
    unittest.main()
