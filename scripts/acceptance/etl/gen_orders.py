#!/usr/bin/env python3
"""Seeded generator for the Point 5 ETL acceptance data (ACCEPTANCE-ETL.md §1, PLAN.md §8.7). Standard library only.

It writes one big orders CSV (14 columns of mixed types: ids, dates, amounts, currency, categories, multi-valued
tags, free text, nulls and dirty values to clean), the same rows split into row batches, a small customers
dimension, and a manifest of the exact results the ETL job must produce: planted defects with their expectation
names, per-batch counts and routes, cleaning counts, and the fact and gold aggregates to the cent. The same seed and
row count always give the same bytes.

Why row batches: no pipeline node can read a multi-megabyte artifact today. The artifact toolkit refuses a whole
file above 200,000 characters (line ranges do not help), and Code nodes have no artifact read on Main yet. Every
batch file therefore stays under MAX_BATCH_CHARS and the pipelines read batches only. See README.md.

Money is integer cents with exact decimals (fx_rates.json): no float drift. Each planted defect fails exactly one
expectation, so "every rejected row is a planted defect with the right expectation name" is checkable.

Usage:
    python3 gen_orders.py --out <dir>                       # the acceptance set (default 24,000 rows)
    python3 gen_orders.py --out <dir> --rows 20000 --seed 7 # one custom orders file
"""
from __future__ import annotations

import argparse
import csv
import datetime as dt
import hashlib
import io
import json
import os
import pathlib
import random
import re
import sys
from decimal import ROUND_HALF_UP, Decimal

GENERATOR_VERSION = 2
MANIFEST_SCHEMA = 'elitea.acceptance.etl.manifest.v2'
DEFAULT_SEED = 20261012

DEFAULT_ROWS = 24_000
SMOKE_ROWS = 1_000
CUSTOMER_ROWS = 3_000         # one customers.csv stays under MAX_BATCH_CHARS, so dim_customer is one read
BATCH_ROWS = 400              # a normalized silver batch stays under the artifact write cap (64 KiB characters)
MAX_BATCHES = 64              # Map MAX_ITEMS (services/elitea-worker-rust/src/agents/graph/map_reduce.rs:26)
DECLARED_ROW_LIMIT = BATCH_ROWS * MAX_BATCHES   # 25,600: bronze_ingest refuses more rows with a typed error
MAX_BATCH_CHARS = 190_000     # under the 200,000-character artifact text read cap, with headroom

DEFAULT_BAD_PPM = 12_000          # 1.2 % planted bad rows
GATE_BAD_PPM = 30_000             # 3 %: above the quality gate, so etl_job reaches the approval HITL
MAX_BAD_PPM = 100_000
BATCH_REJECT_LIMIT_PPM = 50_000   # silver_batch.route_quality: above 5 % -> error output
QUALITY_GATE_LIMIT_PPM = 20_000   # etl_job.quality_gate: above 2 % -> approval HITL

HEADER = ('order_id', 'order_seq', 'customer_id', 'country', 'currency', 'amount', 'discount_pct', 'ordered_at',
          'ship_date', 'channel', 'sku', 'qty', 'tags', 'note')
COL = {name: i for i, name in enumerate(HEADER)}
CUSTOMER_HEADER = ('customer_id', 'name', 'country', 'segment', 'created_at')
COUNTRIES = (
    ('US', 'USD', 30), ('CA', 'CAD', 6), ('GB', 'GBP', 12), ('DE', 'EUR', 14), ('FR', 'EUR', 10), ('NL', 'EUR', 5),
    ('ES', 'EUR', 5), ('IT', 'EUR', 6), ('CH', 'CHF', 3), ('SE', 'SEK', 3), ('PL', 'PLN', 3), ('AU', 'AUD', 3),
)
SEGMENTS = ('consumer', 'smb', 'enterprise')
CHANNELS = ('web', 'store', 'partner', 'phone')
TAGS = ('bulk', 'express', 'fragile', 'gift', 'loyalty', 'promo')
DISCOUNTS = ('5', '10', '12.5', '15', '20')
NOTES = ('Leave at the door, ring twice', 'Customer said "urgent"', 'Re-order of a previous basket',
         'Gift wrap; no invoice in the box', 'Call before delivery', 'Deliver after 18:00, side entrance')
UNKNOWN_CURRENCIES = ('ZZZ', 'QQQ', 'AAA', 'JJJ')
BAD_DATES = ('2026-02-30T10:15:00Z', '2026-13-01T00:00:00Z', '2026/08/01 10:00:00', '', 'not-a-date')
ORDER_WINDOW_START = dt.datetime(2026, 7, 1, tzinfo=dt.timezone.utc)
ORDER_WINDOW_DAYS = 92
TIMESTAMP_RE = re.compile(r'^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$')
TIMESTAMP_FORMAT = '%Y-%m-%dT%H:%M:%SZ'
MALFORMED_LINE = 'O00000000,0,C000001,DE,EUR'   # 5 fields; the id is outside the sampled range

# Cleaning happens before the expectations and never rejects a row.
CLEANING = {
    'country': 'strip and upper-case (" de" -> "DE")',
    'channel': 'strip and lower-case (" Store", "WEB" -> "store", "web")',
    'amount': 'strip surrounding spaces',
    'discount_pct': 'null -> 0',
    'ship_date': 'null stays null',
    'tags': 'null -> no tags; "a|b" -> ["a", "b"]',
    'note': 'null stays null; free text, may hold commas and quotes (RFC 4180 quoting)',
}
# Evaluation order: the first failing expectation names the reject. Uniqueness only counts rows that passed the
# earlier expectations. A planted row fails exactly one expectation.
EXPECTATIONS = {
    'well_formed_line': 'exactly 14 CSV fields; bronze quarantines the line, it never becomes a row',
    'valid_date': 'ordered_at matches YYYY-MM-DDTHH:MM:SSZ and is a real UTC instant',
    'positive_qty': 'qty is an integer greater than 0',
    'known_currency': 'currency is an ISO 4217 code with a rate in fx_rates.json',
    'unique_order_id': 'within a batch the first occurrence of an order_id wins; later ones are dropped',
    'unique_order_id_global': 'across batches the first occurrence in file order wins (dedupe_global)',
}
ROW_EXPECTATIONS = tuple(name for name in EXPECTATIONS if name != 'well_formed_line')
BATCH_EXPECTATIONS = ROW_EXPECTATIONS[:-1]

HERE = pathlib.Path(__file__).resolve().parent
FX_RATES_PATH = HERE / 'fx_rates.json'


class GeneratorError(Exception):
    """A typed, readable refusal (bad arguments or a limit the data would break)."""


def load_fx_rates(path=FX_RATES_PATH):
    data = json.loads(pathlib.Path(path).read_text(encoding='utf-8'))
    return {code: Decimal(rate) for code, rate in data['rates'].items()}


def cents_text(cents):
    return f'{cents // 100}.{cents % 100:02d}'


def text_cents(text):
    whole, frac = text.strip().split('.')
    return int(whole) * 100 + int(frac)


def net_cents(amount_cents, discount_text, rate):
    """net_amount_usd = round_half_up(amount * (1 - discount_pct / 100) * rate, 2), exact on integer cents."""
    discount = Decimal(discount_text or '0')
    exact = Decimal(amount_cents) * (Decimal(100) - discount) / Decimal(100) * rate
    return int(exact.to_integral_value(rounding=ROUND_HALF_UP))


def csv_text(rows, header):
    buffer = io.StringIO()
    writer = csv.writer(buffer, lineterminator='\n')
    writer.writerow(header)
    writer.writerows(rows)
    return buffer.getvalue()


def build_customers(seed):
    rng = random.Random(f'{seed}:customers')
    countries = [c for c, _, _ in COUNTRIES]
    weights = [w for _, _, w in COUNTRIES]
    rows = []
    for n in range(1, CUSTOMER_ROWS + 1):
        created = dt.date(2024, 1, 1) + dt.timedelta(days=rng.randrange(900))
        rows.append([f'C{n:06d}', f'Customer {n:06d}', rng.choices(countries, weights)[0], rng.choice(SEGMENTS),
                     created.isoformat()])
    return rows


def _clean_rows(seed, rows, customers):
    rng = random.Random(f'{seed}:orders:{rows}')
    currency_of = {c: cur for c, cur, _ in COUNTRIES}
    order_ids = rng.sample(range(10_000_000, 100_000_000), rows)
    out = []
    for seq in range(1, rows + 1):
        customer = customers[rng.randrange(CUSTOMER_ROWS)]
        country = customer[2]
        qty = rng.randint(1, 10)
        ordered = ORDER_WINDOW_START + dt.timedelta(seconds=rng.randrange(ORDER_WINDOW_DAYS * 86_400))
        ship = '' if rng.random() < 0.10 else (ordered.date() + dt.timedelta(days=rng.randrange(8))).isoformat()
        channel = rng.choice(CHANNELS)
        if rng.random() < 0.05:
            channel = rng.choice((channel.upper(), f' {channel.capitalize()}', f'{channel.capitalize()} '))
        amount = cents_text(qty * rng.randint(199, 49_999))
        if rng.random() < 0.02:
            amount = f' {amount} '
        shown_country = country if rng.random() >= 0.03 else rng.choice((country.lower(), f' {country.lower()} '))
        tags = '' if rng.random() < 0.30 else '|'.join(sorted(rng.sample(TAGS, rng.randint(1, 3))))
        note = '' if rng.random() < 0.70 else rng.choice(NOTES)
        discount = '' if rng.random() < 0.40 else rng.choice(DISCOUNTS)
        out.append([f'O{order_ids[seq - 1]}', str(seq), customer[0], shown_country, currency_of[country], amount,
                    discount, ordered.strftime(TIMESTAMP_FORMAT), ship, channel, f'SKU-{rng.randint(1, 500):05d}',
                    str(qty), tags, note])
    return out


def _plant_defects(seed, rows, bad_ppm, data):
    rng = random.Random(f'{seed}:defects:{rows}:{bad_ppm}')
    n_bad = rows * bad_ppm // 1_000_000
    if n_bad * 3 >= rows:
        raise GeneratorError(f'{n_bad} planted defects do not fit in {rows} rows')
    per_kind = n_bad // 4
    n_dup = n_bad - 3 * per_kind
    n_cross = n_dup // 2 if rows > BATCH_ROWS else 0
    n_within = n_dup - n_cross
    defects = {}
    simple = rng.sample(range(rows), 3 * per_kind)
    for k, name in enumerate(('valid_date', 'positive_qty', 'known_currency')):
        for i in simple[k * per_kind:(k + 1) * per_kind]:
            defects[i] = name
    sources = set()

    def plant(count, name, pick):
        placed, attempts = 0, 0
        while placed < count:
            attempts += 1
            if attempts > 100 * count + 1000:
                raise GeneratorError(f'could not place {count} {name} duplicates')
            p, s = pick()
            if s is None or p in defects or p in sources or s in defects or s in sources:
                continue
            defects[p] = name
            sources.add(s)
            data[p][COL['order_id']] = data[s][COL['order_id']]
            placed += 1

    def within():
        p = rng.randrange(rows)
        start = p - p % BATCH_ROWS
        return (p, rng.randrange(start, p)) if p > start else (p, None)

    def cross():
        p = rng.randrange(BATCH_ROWS, rows)
        return p, rng.randrange(0, p - p % BATCH_ROWS)

    plant(n_within, 'unique_order_id', within)
    plant(n_cross, 'unique_order_id_global', cross)
    for i, name in sorted(defects.items()):
        if name == 'valid_date':
            data[i][COL['ordered_at']] = BAD_DATES[rng.randrange(len(BAD_DATES))]
            data[i][COL['ship_date']] = ''
        elif name == 'positive_qty':
            data[i][COL['qty']] = str(-rng.randint(1, 5))
        elif name == 'known_currency':
            data[i][COL['currency']] = UNKNOWN_CURRENCIES[rng.randrange(len(UNKNOWN_CURRENCIES))]
    # The malformed line sits inside a batch, never after a batch's last row.
    candidates = [i for i in range(rows - 1) if i % BATCH_ROWS != BATCH_ROWS - 1]
    malformed_after = candidates[rng.randrange(len(candidates))] if candidates else -1
    return defects, malformed_after


def build_orders(seed, rows, bad_ppm, customers):
    if not 1 <= rows <= DECLARED_ROW_LIMIT + 1:
        raise GeneratorError(f'rows must be 1..{DECLARED_ROW_LIMIT + 1} (64 Map items of {BATCH_ROWS} rows, '
                             'plus one row for the limit+1 refusal file)')
    if not 0 <= bad_ppm <= MAX_BAD_PPM:
        raise GeneratorError(f'bad_ppm must be 0..{MAX_BAD_PPM}')
    data = _clean_rows(seed, rows, customers)
    defects, malformed_after = _plant_defects(seed, rows, bad_ppm, data)
    return {'rows': data, 'defects': defects, 'malformed_after': malformed_after}


def _lines_with_malformed(orders, start, end):
    """CSV lines for rows[start:end] with the malformed line inserted where it belongs."""
    lines = []
    for i in range(start, end):
        lines.append(csv_text([orders['rows'][i]], HEADER).split('\n', 1)[1])
        if i == orders['malformed_after']:
            lines.append(MALFORMED_LINE + '\n')
    return lines


def serialize(orders):
    """The big CSV and its batch files. Returns (big_text, [batch_text], row_line_numbers, malformed_line_no)."""
    header = ','.join(HEADER) + '\n'
    rows = len(orders['rows'])
    big = [header]
    row_lines, malformed_line_no = [], 0
    batches = []
    for start in range(0, rows, BATCH_ROWS):
        part = _lines_with_malformed(orders, start, min(start + BATCH_ROWS, rows))
        for line in part:
            if line == MALFORMED_LINE + '\n':
                malformed_line_no = len(big) + 1
            else:
                row_lines.append(len(big) + 1)
            big.append(line)
        text = header + ''.join(part)
        if len(text) > MAX_BATCH_CHARS:
            raise GeneratorError(f'batch {len(batches)} has {len(text)} characters, above {MAX_BATCH_CHARS}')
        batches.append(text)
    return ''.join(big), batches, row_lines, malformed_line_no


def _sha256(text):
    return hashlib.sha256(text.encode('utf-8')).hexdigest()


def _cleaned(row):
    """The row after the CLEANING rules, as a dict."""
    r = dict(zip(HEADER, row))
    r['country'] = r['country'].strip().upper()
    r['channel'] = r['channel'].strip().lower()
    r['amount'] = r['amount'].strip()
    r['tags'] = r['tags'].split('|') if r['tags'] else []
    return r


def orders_manifest(orders, batch_texts, row_lines, malformed_line_no, rates):
    rows = orders['rows']
    defects = orders['defects']
    totals = dict.fromkeys(EXPECTATIONS, 0)
    totals['well_formed_line'] = 1 if orders['malformed_after'] >= 0 else 0
    batches = []
    for k, text in enumerate(batch_texts):
        start = k * BATCH_ROWS
        end = min(start + BATCH_ROWS, len(rows))
        counts = dict.fromkeys(ROW_EXPECTATIONS, 0)
        for i in range(start, end):
            if i in defects:
                counts[defects[i]] += 1
        in_batch = sum(counts[n] for n in BATCH_EXPECTATIONS)
        reject_ppm = in_batch * 1_000_000 // (end - start)
        batches.append({
            'batch_no': k, 'file': f'batch-{k:04d}.csv', 'chars': len(text), 'sha256': _sha256(text),
            'rows': end - start, 'first_seq': start + 1, 'last_seq': end,
            'quarantined_lines': 1 if start <= orders['malformed_after'] < end else 0,
            'rejects': {n: counts[n] for n in BATCH_EXPECTATIONS}, 'global_duplicates': counts['unique_order_id_global'],
            'reject_ppm': reject_ppm, 'route': 'error_output' if reject_ppm > BATCH_REJECT_LIMIT_PPM else 'continue'})
        for n, v in counts.items():
            totals[n] += v
    rejected_rows = sum(totals[n] for n in ROW_EXPECTATIONS)
    valid = [_cleaned(r) for i, r in enumerate(rows) if i not in defects]
    cleaning = {
        'country_normalized': sum(1 for r in rows if r[COL['country']] != r[COL['country']].strip().upper()),
        'channel_normalized': sum(1 for r in rows if r[COL['channel']] != r[COL['channel']].strip().lower()),
        'amount_trimmed': sum(1 for r in rows if r[COL['amount']] != r[COL['amount']].strip()),
        'discount_null': sum(1 for r in rows if not r[COL['discount_pct']]),
        'ship_date_null': sum(1 for r in rows if not r[COL['ship_date']]),
        'tags_null': sum(1 for r in rows if not r[COL['tags']]),
        'note_null': sum(1 for r in rows if not r[COL['note']]),
    }
    gross_by_currency, daily, by_country, by_channel, tag_counts = {}, {}, {}, {}, {}
    sum_net = sum_qty = ties = 0
    for r in valid:
        cents, qty, rate = text_cents(r['amount']), int(r['qty']), rates[r['currency']]
        exact = Decimal(cents) * (Decimal(100) - Decimal(r['discount_pct'] or '0')) / Decimal(100) * rate
        if exact % 1 == Decimal('0.5'):
            ties += 1
        net = net_cents(cents, r['discount_pct'], rate)
        sum_net += net
        sum_qty += qty
        gross_by_currency[r['currency']] = gross_by_currency.get(r['currency'], 0) + cents
        for key, table in ((r['ordered_at'][:10], daily), (r['country'], by_country), (r['channel'], by_channel)):
            agg = table.setdefault(key, [0, 0, 0])
            agg[0] += 1
            agg[1] += qty
            agg[2] += net
        for tag in r['tags']:
            agg = tag_counts.setdefault(tag, [0, 0])
            agg[0] += 1
            agg[1] += qty
    quality_ppm = rejected_rows * 1_000_000 // len(rows)

    def table(rows_, key):
        return [{key: k, 'orders': a[0], 'qty': a[1], 'net_amount_usd': cents_text(a[2])}
                for k, a in sorted(rows_.items())]

    return {
        'rows_well_formed': len(rows),
        'lines_data': len(rows) + totals['well_formed_line'],
        'batch_rows': BATCH_ROWS,
        'batches': batches,
        'batch_set_sha256': _sha256('\n'.join(b['sha256'] for b in batches)),
        # What bronze_ingest can compute from a listing alone (it never reads the big file): the bronze marker key.
        'batch_listing_sha256': _sha256('\n'.join(f"{b['file']}:{b['chars']}" for b in batches)),
        'malformed_line': None if not malformed_line_no else {
            'line_no': malformed_line_no, 'batch_no': orders['malformed_after'] // BATCH_ROWS,
            'expectation': 'well_formed_line'},
        'cleaning': cleaning,
        'rejects': totals,
        'rejected_rows': rejected_rows,
        'valid_rows': len(valid),
        'quality_gate': {'reject_ppm': quality_ppm, 'limit_ppm': QUALITY_GATE_LIMIT_PPM,
                         'route': 'approval' if quality_ppm > QUALITY_GATE_LIMIT_PPM else 'publish'},
        'fact_orders': {
            'rows': len(valid),
            'distinct_order_ids': len({r['order_id'] for r in valid}),
            'distinct_customers': len({r['customer_id'] for r in valid}),
            'sum_qty': sum_qty,
            'sum_amount_by_currency': {c: cents_text(v) for c, v in sorted(gross_by_currency.items())},
            'sum_net_amount_usd': cents_text(sum_net),
            'net_amount_half_up_ties': ties,
            'order_ids_sha256': _sha256('\n'.join(sorted(r['order_id'] for r in valid))),
        },
        'gold_sales_daily': table(daily, 'order_date'),
        'gold_sales_by_country': table(by_country, 'country'),
        'gold_sales_by_channel': table(by_channel, 'channel'),
        'tag_counts': [{'tag': t, 'orders': a[0], 'qty': a[1]} for t, a in sorted(tag_counts.items())],
    }


def expected_rejects_text(orders):
    """Every reject, keyed like silver_clean's rejects-<run>.csv: (order_seq, order_id, batch_no, expectation)."""
    entries = [(i + 1, orders['rows'][i][COL['order_id']], i // BATCH_ROWS, name)
               for i, name in orders['defects'].items()]
    if orders['malformed_after'] >= 0:
        entries.append((0, 'O00000000', orders['malformed_after'] // BATCH_ROWS, 'well_formed_line'))
    entries.sort()
    return 'order_seq,order_id,batch_no,expectation\n' + ''.join(f'{a},{b},{c},{d}\n' for a, b, c, d in entries)


def _write(out_dir, name, text):
    path = out_dir / name
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f'.{path.name}.tmp')
    tmp.write_bytes(text.encode('utf-8'))
    os.replace(tmp, path)
    return {'bytes': len(text.encode('utf-8')), 'chars': len(text), 'sha256': _sha256(text)}


def dataset_specs(set_name, rows):
    if set_name == 'custom':
        return [(f'orders-{rows}.csv', rows, DEFAULT_BAD_PPM, 'custom size')]
    return [
        ('orders.csv', DEFAULT_ROWS, DEFAULT_BAD_PPM, 'happy path: quality gate passes, publish without approval'),
        ('orders-gate.csv', DEFAULT_ROWS, GATE_BAD_PPM, 'reject rate above the quality gate: the approval HITL pauses'),
        ('orders-1k.csv', SMOKE_ROWS, DEFAULT_BAD_PPM, 'smoke run'),
        ('orders-limit.csv', DECLARED_ROW_LIMIT, DEFAULT_BAD_PPM, 'exactly the declared row limit: accepted'),
        ('orders-limit-plus-1.csv', DECLARED_ROW_LIMIT + 1, DEFAULT_BAD_PPM,
         'one row over the declared limit (65 batches): typed, readable refusal'),
    ]


def generate(out_dir, seed=DEFAULT_SEED, set_name='acceptance', rows=DEFAULT_ROWS):
    out_dir = pathlib.Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    rates = load_fx_rates()
    customers = build_customers(seed)
    manifest = {
        'schema': MANIFEST_SCHEMA,
        'generator_version': GENERATOR_VERSION,
        'seed': seed,
        'set': set_name,
        'columns': list(HEADER),
        'csv': 'RFC 4180, comma, double-quote quoting, LF line endings, UTF-8 (ASCII content)',
        'cleaning': CLEANING,
        'expectations': EXPECTATIONS,
        'money': {'unit': 'decimal string, 2 places',
                  'net_amount_usd': 'round_half_up(amount * (1 - discount_pct / 100) * rate, 2)',
                  'fx_rates_sha256': hashlib.sha256(FX_RATES_PATH.read_bytes()).hexdigest()},
        'limits': {'batch_rows': BATCH_ROWS, 'max_batches': MAX_BATCHES, 'declared_row_limit': DECLARED_ROW_LIMIT,
                   'max_batch_chars': MAX_BATCH_CHARS, 'batch_reject_limit_ppm': BATCH_REJECT_LIMIT_PPM,
                   'quality_gate_limit_ppm': QUALITY_GATE_LIMIT_PPM},
        'customers': {'file': 'customers.csv', 'rows': CUSTOMER_ROWS,
                      **_write(out_dir, 'customers.csv', csv_text(customers, CUSTOMER_HEADER))},
        'datasets': {},
    }
    if manifest['customers']['chars'] > MAX_BATCH_CHARS:
        raise GeneratorError('customers.csv is above the artifact text read cap')
    for name, n_rows, bad_ppm, purpose in dataset_specs(set_name, rows):
        orders = build_orders(seed, n_rows, bad_ppm, customers)
        big, batch_texts, row_lines, malformed_line_no = serialize(orders)
        stem = name[:-4]
        entry = {'purpose': purpose, 'bad_ppm': bad_ppm, 'file': _write(out_dir, name, big),
                 'batch_dir': f'{stem}.batches'}
        entry['expected_outcome'] = 'refused' if n_rows > DECLARED_ROW_LIMIT else 'accepted'
        for k, text in enumerate(batch_texts):
            _write(out_dir, f'{stem}.batches/batch-{k:04d}.csv', text)
        if entry['expected_outcome'] == 'accepted':
            rejects_name = f'expected-rejects-{name}'
            entry['expected_rejects'] = {'file': rejects_name, **_write(
                out_dir, rejects_name, expected_rejects_text(orders))}
            entry.update(orders_manifest(orders, batch_texts, row_lines, malformed_line_no, rates))
        else:
            entry['batches'] = len(batch_texts)
        manifest['datasets'][name] = entry
    _write(out_dir, 'manifest.json', json.dumps(manifest, indent=2, sort_keys=True) + '\n')
    return manifest


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--out', required=True, type=pathlib.Path, help='output directory (outside the repository)')
    parser.add_argument('--seed', type=int, default=DEFAULT_SEED)
    parser.add_argument('--rows', type=int, help=f'write one orders file of this many rows (1..{DECLARED_ROW_LIMIT})'
                                                 ' instead of the acceptance set')
    args = parser.parse_args(argv)
    if args.rows is not None and not 1 <= args.rows <= DECLARED_ROW_LIMIT:
        parser.error(f'--rows must be 1..{DECLARED_ROW_LIMIT}')
    try:
        manifest = generate(args.out, args.seed, 'custom' if args.rows else 'acceptance', args.rows or DEFAULT_ROWS)
    except GeneratorError as exc:
        print(f'gen_orders: {exc}', file=sys.stderr)
        return 2
    for name, entry in manifest['datasets'].items():
        print(f"{name}: {entry['file']['bytes']} bytes, {entry['expected_outcome']}, "
              f"valid_rows={entry.get('valid_rows', '-')}")
    return 0


if __name__ == '__main__':
    sys.exit(main())
