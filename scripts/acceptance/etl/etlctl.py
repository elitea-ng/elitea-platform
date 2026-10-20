#!/usr/bin/env python3
"""Bring-up and self-check of the ETL acceptance fixture services (compose.yaml). Standard library + openssl + docker.

  init      create the stack directory (mode 700) with a local env file of generated test credentials (mode 600)
            and the mock bearer tokens; nothing is written into the repository
  material  issue the harness's own test CA and the server certificates (warehouse-pg, warehouse-mysql, etl-mocks)
  up        start the three services (compose project elitea-etl-fixtures) and wait for health
  verify    prove TLS-only on all three (plaintext refused, verified TLS accepted), the schemas and users, and the
            mock journal (FX served, webhook exactly once, duplicate and conflict answered); writes redacted evidence
  sqlcheck  run the DB statements of the pipeline drafts (load_postgres / load_mysql / gold_build) on both warehouses
            with the smoke dataset, twice, and compare fact and gold tables with the generator's manifest
  down      stop the services and delete their volumes

Values reach SQL only as psql variables (PostgreSQL) or base64 literals decoded by the server (MySQL): no secret or
data value is ever spliced into SQL text. Evidence holds ids, counts, statuses and versions only.
"""
from __future__ import annotations

import argparse
import base64
import datetime as dt
import json
import os
import pathlib
import re
import secrets
import shutil
import subprocess
import sys
import tempfile

import gen_orders

HERE = pathlib.Path(__file__).resolve().parent
COMPOSE = HERE / 'compose.yaml'
PROJECT = 'elitea-etl-fixtures'
DEFAULT_STACK_DIR = pathlib.Path.home() / '.elitea-etl-acceptance'
SERVER_NAMES = ('warehouse-pg', 'warehouse-mysql', 'etl-mocks')
PASSWORD_KEYS = ('ETL_PG_ADMIN_PASSWORD', 'ETL_PG_LOADER_PASSWORD', 'ETL_PG_READER_PASSWORD',
                 'ETL_MYSQL_ROOT_PASSWORD', 'ETL_MYSQL_LOADER_PASSWORD', 'ETL_MYSQL_READER_PASSWORD')
CERT_DAYS = 30
EXPECTED_TABLES = 10
# Credential shapes that must never appear in evidence (same idea as scripts/crash-recovery/lib/redact.py).
REDACTION_PATTERNS = (re.compile(r'-----BEGIN [A-Z ]*(PRIVATE KEY|CERTIFICATE)-----'),
                      re.compile(r'eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.'),
                      re.compile(r'(?i)authorization:\s*bearer\s+\S{8,}'))


class CtlError(Exception):
    """A typed, readable refusal."""


def stack_dir(args):
    return pathlib.Path(args.stack_dir or os.environ.get('ETL_STACK_DIR') or DEFAULT_STACK_DIR).expanduser().resolve()


def read_env(directory):
    path = directory / 'etl.env'
    if not path.exists():
        raise CtlError(f'{path} is missing: run etlctl.py init first')
    env = {}
    for line in path.read_text(encoding='utf-8').splitlines():
        if line and not line.startswith('#'):
            key, _, value = line.partition('=')
            env[key] = value
    return env


def run(cmd, *, env=None, check=True, input_text=None, timeout=300):
    result = subprocess.run(cmd, env=env, input=input_text, capture_output=True, text=True, timeout=timeout)
    if check and result.returncode != 0:
        raise CtlError(f'{cmd[0]} {cmd[1] if len(cmd) > 1 else ""} failed ({result.returncode}): '
                       f'{result.stderr.strip()[-400:]}')
    return result


def compose(directory, *args, **kwargs):
    return run(['docker', 'compose', '-p', PROJECT, '-f', str(COMPOSE), '--env-file', str(directory / 'etl.env'),
                *args], **kwargs)


def cmd_init(directory):
    directory.mkdir(parents=True, exist_ok=True)
    os.chmod(directory, 0o700)
    env_path = directory / 'etl.env'
    if env_path.exists():
        print(f'{env_path} exists; keeping it')
    else:
        lines = ['# ETL acceptance fixture credentials: generated, local only, never committed.',
                 f'ETL_STACK_DIR={directory}']
        lines += [f'{key}={secrets.token_urlsafe(32)}' for key in PASSWORD_KEYS]
        fd = os.open(env_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'w', encoding='utf-8') as handle:
            handle.write('\n'.join(lines) + '\n')
    tokens = directory / 'tokens'
    tokens.mkdir(exist_ok=True)
    os.chmod(tokens, 0o755)   # mounted read-only into etl-mocks; the stack directory itself stays 0700
    for name in ('fx.token', 'webhook.token'):
        path = tokens / name
        if not path.exists():
            path.write_text(secrets.token_urlsafe(32) + '\n', encoding='utf-8')
            os.chmod(path, 0o644)
    print(f'stack directory {directory} ready')


def _openssl(*args):
    run(['openssl', *args])


def cmd_material(directory):
    read_env(directory)
    ca_dir, tls = directory / 'ca', directory / 'tls'
    ca_dir.mkdir(exist_ok=True)
    os.chmod(ca_dir, 0o700)
    tls.mkdir(exist_ok=True)
    os.chmod(tls, 0o755)       # mounted read-only into the three services
    ca_key, ca_crt = ca_dir / 'ca.key', tls / 'ca.crt'

    def valid_for_a_day(path):
        return path.exists() and run(['openssl', 'x509', '-in', str(path), '-checkend', '86400', '-noout'],
                                     check=False).returncode == 0

    if not (ca_key.exists() and valid_for_a_day(ca_crt)):
        # A partial earlier run or an expiring CA: start over, so every leaf is issued by the new CA.
        for stale in [ca_key, ca_crt] + [tls / f'{n}.{e}' for n in SERVER_NAMES for e in ('crt', 'key')]:
            stale.unlink(missing_ok=True)
        _openssl('genpkey', '-algorithm', 'EC', '-pkeyopt', 'ec_paramgen_curve:P-256', '-out', str(ca_key))
        os.chmod(ca_key, 0o600)
        _openssl('req', '-x509', '-new', '-key', str(ca_key), '-sha256', '-days', str(CERT_DAYS),
                 '-subj', '/CN=elitea ETL acceptance test CA', '-addext', 'basicConstraints=critical,CA:TRUE',
                 '-addext', 'keyUsage=critical,keyCertSign,cRLSign', '-out', str(ca_crt))
    for name in SERVER_NAMES:
        crt, key = tls / f'{name}.crt', tls / f'{name}.key'
        if key.exists() and valid_for_a_day(crt) and run(
                ['openssl', 'verify', '-CAfile', str(ca_crt), '-verify_hostname', name, str(crt)],
                check=False).returncode == 0:
            continue
        with tempfile.TemporaryDirectory(dir=directory) as scratch:
            csr, ext = pathlib.Path(scratch) / 'req.csr', pathlib.Path(scratch) / 'ext.cnf'
            _openssl('genpkey', '-algorithm', 'EC', '-pkeyopt', 'ec_paramgen_curve:P-256', '-out', str(key))
            _openssl('req', '-new', '-key', str(key), '-subj', f'/CN={name}', '-out', str(csr))
            ext.write_text('basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n'
                           f'extendedKeyUsage=serverAuth\nsubjectAltName=DNS:{name}\n', encoding='utf-8')
            _openssl('x509', '-req', '-in', str(csr), '-CA', str(ca_crt), '-CAkey', str(ca_key), '-set_serial',
                     '0x' + secrets.token_hex(16), '-days', str(CERT_DAYS), '-sha256', '-extfile', str(ext),
                     '-out', str(crt))
        # Test-CA leaf keys: readable inside the containers (mounted read-only); the stack directory is 0700.
        os.chmod(key, 0o644)
        run(['openssl', 'verify', '-CAfile', str(ca_crt), '-verify_hostname', name, str(crt)])
    print(f'test CA and server certificates in {tls}')


def cmd_up(directory):
    read_env(directory)
    if not (directory / 'tls' / 'ca.crt').exists():
        raise CtlError('run etlctl.py material first')
    compose(directory, 'up', '-d', '--wait', timeout=1200)   # a busy shared Docker host is slow to report health
    print('fixture services are healthy')


def cmd_down(directory):
    compose(directory, 'down', '-v', '--remove-orphans', timeout=300)
    print('fixture services and volumes removed')


# The scripts below are constant; values arrive as positional arguments or container environment variables.
PG_AS = 'PGPASSWORD="$1" PGCONNECT_TIMEOUT=5 exec psql -X -v ON_ERROR_STOP=1 -At "${@:2}"'
PG_READER = ['bash', '-c', 'p="$ETL_PG_READER_PASSWORD"; set -- "$p" "$@"; ' + PG_AS, 'psql']
PG_LOADER = ['bash', '-c', 'p="$ETL_PG_LOADER_PASSWORD"; set -- "$p" "$@"; ' + PG_AS, 'psql']
MY_AS = 'MYSQL_PWD="$1" exec mysql --connect-timeout=5 -N -B "${@:2}"'
MY_READER = ['bash', '-c', 'p="$ETL_MYSQL_READER_PASSWORD"; set -- "$p" "$@"; ' + MY_AS, 'mysql']
MY_LOADER = ['bash', '-c', 'p="$ETL_MYSQL_LOADER_PASSWORD"; set -- "$p" "$@"; ' + MY_AS, 'mysql']
MY_ROOT_TCP = ['bash', '-c', 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --connect-timeout=5 -uroot -N -B "$@"', 'mysql']
MY_ROOT_SOCKET = ['bash', '-c', 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --protocol=socket -N -B "$@"',
                  'mysql']
PG_TLS = 'host=warehouse-pg dbname=warehouse sslmode=verify-full sslrootcert=/run/etl-tls/ca.crt'
MY_TLS = ['-h', 'warehouse-mysql', '--ssl-mode=VERIFY_IDENTITY', '--ssl-ca=/run/etl-tls/ca.crt', 'warehouse']


def _exec(directory, service, argv, input_text=None, check=False):
    # `docker exec` on the compose container name: much faster than `docker compose exec` on a busy Docker host.
    return run(['docker', 'exec', '-i', f'{PROJECT}-{service}-1', *argv], check=check, input_text=input_text)


def _first_line(text):
    return (text.strip().splitlines() or [''])[-1][:200]


def verify_pg(directory):
    out = {}
    plain = _exec(directory, 'warehouse-pg', PG_READER + [f'host=warehouse-pg dbname=warehouse user=etl_reader '
                                                          'sslmode=disable', '-c', 'select 1'])
    out['plaintext'] = {'exit': plain.returncode, 'refused': plain.returncode != 0 and 'no encryption' in plain.stderr,
                        'error': _first_line(plain.stderr)}
    wrong_ca = _exec(directory, 'warehouse-pg', PG_READER + [
        'host=warehouse-pg dbname=warehouse user=etl_reader sslmode=verify-full sslrootcert=/etc/ssl/certs/'
        'ca-certificates.crt', '-c', 'select 1'])
    out['untrusted_ca'] = {'exit': wrong_ca.returncode, 'refused': wrong_ca.returncode != 0}
    tls = _exec(directory, 'warehouse-pg', PG_READER + [f'{PG_TLS} user=etl_reader', '-c',
                'select ssl, version from pg_stat_ssl where pid = pg_backend_pid()'])
    out['verified_tls'] = {'exit': tls.returncode, 'session': tls.stdout.strip()}
    tables = _exec(directory, 'warehouse-pg', PG_READER + [f'{PG_TLS} user=etl_reader', '-c',
                   "select count(*) from information_schema.tables where table_schema = 'etl'"])
    out['etl_tables'] = int(tables.stdout.strip() or 0)
    write = _exec(directory, 'warehouse-pg', PG_READER + [f'{PG_TLS} user=etl_reader', '-c',
                  "insert into etl.etl_runs (run_id, status) values ('reader-probe', 'x')"])
    out['reader_write_refused'] = write.returncode != 0 and 'permission denied' in write.stderr
    out['ok'] = (out['plaintext']['refused'] and out['untrusted_ca']['refused'] and tls.returncode == 0
                 and tls.stdout.strip().startswith('t|TLSv1.') and out['etl_tables'] == EXPECTED_TABLES
                 and out['reader_write_refused'])
    return out


def verify_mysql(directory):
    out = {}
    # --get-server-public-key lets the client finish caching_sha2 without TLS, so the refusal comes from the server.
    plain = _exec(directory, 'warehouse-mysql', MY_ROOT_TCP + ['-h', 'warehouse-mysql', '--ssl-mode=DISABLED',
                                                               '--get-server-public-key', '-e', 'select 1'])
    out['plaintext'] = {'exit': plain.returncode, 'refused': plain.returncode != 0 and '3159' in plain.stderr,
                        'error': _first_line(plain.stderr)}
    plain_user = _exec(directory, 'warehouse-mysql', MY_READER + ['-h', 'warehouse-mysql', '-u', 'etl_reader',
                                                                  '--ssl-mode=DISABLED', '--get-server-public-key',
                                                                  '-e', 'select 1'])
    out['plaintext_require_ssl_user'] = {'exit': plain_user.returncode, 'refused': plain_user.returncode != 0,
                                         'error': _first_line(plain_user.stderr)}
    tls = _exec(directory, 'warehouse-mysql', MY_READER + ['-u', 'etl_reader', *MY_TLS, '-e',
                "show session status like 'Ssl_version'"])
    out['verified_tls'] = {'exit': tls.returncode, 'session': tls.stdout.strip().replace('\t', '|')}
    users = _exec(directory, 'warehouse-mysql', MY_ROOT_SOCKET + ['-e',
                  "select user, ssl_type from mysql.user where user like 'etl_%' order by user"])
    out['users'] = users.stdout.strip().replace('\t', '|').splitlines()
    variables = _exec(directory, 'warehouse-mysql', MY_ROOT_SOCKET + ['-e',
                      "select @@require_secure_transport, @@tls_version"])
    out['server'] = variables.stdout.strip().replace('\t', '|')
    tables = _exec(directory, 'warehouse-mysql', MY_READER + ['-u', 'etl_reader', *MY_TLS, '-e',
                   "select count(*) from information_schema.tables where table_schema = 'warehouse'"])
    out['warehouse_tables'] = int(tables.stdout.strip() or 0)
    write = _exec(directory, 'warehouse-mysql', MY_READER + ['-u', 'etl_reader', *MY_TLS, '-e',
                  "insert into etl_runs (run_id, status) values ('reader-probe', 'x')"])
    out['reader_write_refused'] = write.returncode != 0 and '1142' in write.stderr
    out['ok'] = (out['plaintext']['refused'] and out['plaintext_require_ssl_user']['refused'] and tls.returncode == 0
                 and 'TLSv1.' in out['verified_tls']['session']
                 and out['users'] == ['etl_loader|ANY', 'etl_reader|ANY'] and out['server'].startswith('1|')
                 and out['warehouse_tables'] == EXPECTED_TABLES and out['reader_write_refused'])
    return out


def verify_mocks(directory, run_id):
    mock = ['python3', '-I', '/app/etl_mock.py']
    probe = _exec(directory, 'etl-mocks', mock + ['probe', '--ca', '/etl/tls/ca.crt', '--run-id', run_id],
                  check=True)
    report = json.loads(probe.stdout)
    checked = _exec(directory, 'etl-mocks', mock + ['check', '--run-id', run_id])
    summary = json.loads(_exec(directory, 'etl-mocks', mock + ['summary'], check=True).stdout)
    out = {'probe': report, 'check': json.loads(checked.stdout), 'webhook': summary['webhook'].get(run_id),
           'fx': summary['fx'].get(run_id), 'tls_handshake_failures': summary['tls_handshake_failures'],
           'journal_entries': summary['entries']}
    out['ok'] = (report['plain_http'].startswith('refused') and report['fx']['status'] == 200
                 and report['fx_without_token'] == 401 and report['webhook_first'] == 201
                 and report['webhook_repeat'] == 200 and report['webhook_conflict'] == 409
                 and checked.returncode == 0 and summary['tls_handshake_failures'] >= 1)
    return out


def redaction_hits(text):
    return [p.pattern for p in REDACTION_PATTERNS if p.search(text)]


def write_evidence(directory, name, payload, evidence_dir):
    target = pathlib.Path(evidence_dir).expanduser() if evidence_dir else directory / 'evidence'
    target.mkdir(parents=True, exist_ok=True)
    text = json.dumps(payload, indent=2, sort_keys=True) + '\n'
    env = read_env(directory)
    leaked = [k for k in PASSWORD_KEYS if env.get(k) and env[k] in text]
    for token in ('fx.token', 'webhook.token'):
        if (directory / 'tokens' / token).read_text(encoding='utf-8').strip() in text:
            leaked.append(token)
    if leaked or redaction_hits(text):
        raise CtlError(f'evidence refused: it contains secret material ({len(leaked)} known values)')
    path = target / f'{name}-{dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")}.json'
    path.write_text(text, encoding='utf-8')
    return path


def images(directory):
    out = {}
    for service in ('warehouse-pg', 'warehouse-mysql', 'etl-mocks'):
        cid = compose(directory, 'ps', '-q', service).stdout.strip()
        image = run(['docker', 'inspect', '--format', '{{.Config.Image}}', cid]).stdout.strip() if cid else None
        out[service] = image
    return out


def cmd_verify(directory, evidence_dir):
    run_id = 'verify-' + dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S')
    payload = {'images': images(directory), 'postgres': verify_pg(directory), 'mysql': verify_mysql(directory),
               'mocks': verify_mocks(directory, run_id)}
    payload['ok'] = all(payload[k]['ok'] for k in ('postgres', 'mysql', 'mocks'))
    path = write_evidence(directory, 'verify', payload, evidence_dir)
    print(json.dumps({'ok': payload['ok'], 'evidence': str(path)}, indent=2))
    return 0 if payload['ok'] else 1


# --- sqlcheck --------------------------------------------------------------------------------------------------

def extract_statements(path, node_ids):
    """The `statements` of the named DB nodes in a draft, in order: {node: [(sql, [(from, type)])]}. Reads the
    fixtures' own block style (no YAML library in the standard library)."""
    nodes, current, statement, in_sql, sql_lines = {}, None, None, False, []
    for line in pathlib.Path(path).read_text(encoding='utf-8').splitlines():
        node = re.match(r'^  - id: (\S+)$', line)
        if node:
            current = node.group(1) if node.group(1) in node_ids else None
            in_sql = False
            continue
        if current is None:
            continue
        inline = re.match(r'^      - sql: "(.*)"$', line)
        folded = re.match(r'^      - sql: >-$', line)
        if inline or folded:
            statement = [inline.group(1) if inline else '', []]
            nodes.setdefault(current, []).append(statement)
            in_sql = bool(folded)
            continue
        if in_sql and line.startswith('          ') and not line.strip().startswith('- {from'):
            statement[0] = (statement[0] + ' ' + line.strip()).strip()
            continue
        in_sql = False
        if statement is not None:
            for name, kind in re.findall(r'\{from: (\w+), type: (\w+)\}', line):
                statement[1].append((name, kind))
    return nodes


def silver_reference(orders, rates):
    """What silver_clean hands the loaders: the stage rows per batch, from the same generator model (cleaning rules
    via gen_orders._cleaned, money via gen_orders.net_cents) that the manifest is computed from."""
    batches = {}
    for i, raw in enumerate(orders['rows']):
        if i in orders['defects']:
            continue
        r = gen_orders._cleaned(raw)
        net = gen_orders.net_cents(gen_orders.text_cents(r['amount']), r['discount_pct'], rates[r['currency']])
        batch_no = i // gen_orders.BATCH_ROWS
        batches.setdefault(batch_no, []).append([
            batch_no, r['order_id'], r['order_seq'], r['customer_id'], r['country'], r['currency'], r['amount'],
            r['discount_pct'] or '0', gen_orders.cents_text(net), r['ordered_at'], r['ordered_at'][:7],
            r['ship_date'] or None, r['channel'], r['sku'], r['qty']])
    return batches


def _pg_type(kind):
    return {'text': 'text', 'json': 'jsonb', 'int8': 'bigint'}[kind]


def pg_script(statements, values, workdir_in_container):
    """One transaction per call: PREPARE/EXECUTE with psql variables read from files (never spliced)."""
    lines = ['\\set ON_ERROR_STOP on']
    for name in sorted(values):
        lines.append(f'\\set {name} `cat {workdir_in_container}/{name}`')
    lines.append('BEGIN;')
    for k, (sql, params) in enumerate(statements):
        types = ', '.join(_pg_type(t) for _, t in params)
        lines.append(f'PREPARE s{k}({types}) AS {sql};')
        lines.append(f"EXECUTE s{k}({', '.join(f':{chr(39)}{n}{chr(39)}' for n, _ in params)});")
        lines.append(f'DEALLOCATE s{k};')
    lines.append('COMMIT;')
    return '\n'.join(lines) + '\n'


def _b64(text):
    return base64.b64encode(text.encode('utf-8')).decode('ascii')


def my_script(statements, values):
    """One transaction per call: every value and every statement arrives as base64 decoded by the server."""
    lines = [f"SET @{name} = CONVERT(FROM_BASE64('{_b64(value)}') USING utf8mb4);" for name, value in values.items()]
    lines.append('START TRANSACTION;')
    for k, (sql, params) in enumerate(statements):
        lines.append(f"SET @q{k} = CONVERT(FROM_BASE64('{_b64(sql)}') USING utf8mb4);")
        lines.append(f'PREPARE s{k} FROM @q{k};')
        using = ', '.join(f'@{n}' for n, _ in params)
        lines.append(f'EXECUTE s{k}' + (f' USING {using}' if using else '') + ';')
        lines.append(f'DEALLOCATE PREPARE s{k};')
    lines.append('COMMIT;')
    return '\n'.join(lines) + '\n'


def cmd_sqlcheck(directory, evidence_dir, seed=gen_orders.DEFAULT_SEED, rows=gen_orders.SMOKE_ROWS):
    rates = gen_orders.load_fx_rates()
    customers = gen_orders.build_customers(seed)
    orders = gen_orders.build_orders(seed, rows, gen_orders.DEFAULT_BAD_PPM, customers)
    expected = gen_orders.orders_manifest(orders, *gen_orders.serialize(orders)[1:], rates)
    batches = silver_reference(orders, rates)
    run_id = 'sqlcheck-' + dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S')
    customers_json = json.dumps([r for r in customers], separators=(',', ':'))
    pg = extract_statements(HERE / 'pipelines' / 'load_postgres.yaml',
                            {'stage', 'merge_fact', 'gold_aggregates', 'read_totals'})
    my = extract_statements(HERE / 'pipelines' / 'load_mysql.yaml',
                            {'stage', 'merge_fact', 'gold_aggregates', 'read_totals'})
    gold = extract_statements(HERE / 'pipelines' / 'gold_build.yaml', {'reconcile_pg', 'reconcile_my'})
    groups = [sorted(batches)[k:k + 3] for k in range(0, len(batches), 3)]
    result = {'run_id_prefix': 'sqlcheck', 'rows': rows, 'batches': len(batches), 'passes': 2,
              'max_stage_param_bytes': 0}

    def pg_call(statements, values):
        workdir = '/tmp/etl-sqlcheck'
        _exec(directory, 'warehouse-pg', ['mkdir', '-p', workdir], check=True)
        with tempfile.TemporaryDirectory() as local:
            for name, value in values.items():
                (pathlib.Path(local) / name).write_text(value, encoding='utf-8')
            run(['docker', 'cp', f'{local}/.', f'{PROJECT}-warehouse-pg-1:{workdir}'])
        out = _exec(directory, 'warehouse-pg', PG_LOADER + [f'{PG_TLS} user=etl_loader', '-q'],
                    input_text=pg_script(statements, values, workdir))
        if out.returncode != 0:
            raise CtlError(f'PostgreSQL statement failed: {_first_line(out.stderr)}')
        return out.stdout

    def my_call(statements, values):
        out = _exec(directory, 'warehouse-mysql', MY_LOADER + ['-u', 'etl_loader', *MY_TLS],
                    input_text=my_script(statements, values))
        if out.returncode != 0:
            raise CtlError(f'MySQL statement failed: {_first_line(out.stderr)}')
        return out.stdout

    for dialect, nodes, call in (('postgres', pg, pg_call), ('mysql', my, my_call)):
        for _ in range(2):   # the second pass proves the loads are idempotent per run_id
            for group in groups:
                stage_rows = json.dumps([row for b in group for row in batches[b]], separators=(',', ':'))
                result['max_stage_param_bytes'] = max(result['max_stage_param_bytes'], len(stage_rows))
                call(nodes['stage'], {'run_id': run_id, 'stage_rows': stage_rows,
                                      'stage_batches': json.dumps([[b, len(batches[b])] for b in group])})
            call(nodes['merge_fact'], {'run_id': run_id, 'customers_json': customers_json})
            call(nodes['gold_aggregates'], {'run_id': run_id})
        totals = call(nodes['read_totals'], {'run_id': run_id}).strip().replace('\t', '|').splitlines()[-1]
        result[f'{dialect}_totals'] = totals
    fact = expected['fact_orders']
    want = f"{fact['rows']}|{fact['sum_qty']}|{fact['sum_net_amount_usd']}"
    pg_rows = pg_call(gold['reconcile_pg'], {'run_id': run_id}).strip().splitlines()
    my_rows = [line.replace('\t', '|') for line in my_call(gold['reconcile_my'], {'run_id': run_id})
               .strip().splitlines()]
    want_rows = sorted(
        [f"channel|{r['channel']}|{r['orders']}|{r['qty']}|{r['net_amount_usd']}" for r in expected['gold_sales_by_channel']]
        + [f"country|{r['country']}|{r['orders']}|{r['qty']}|{r['net_amount_usd']}" for r in expected['gold_sales_by_country']]
        + [f"daily|{r['order_date']}|{r['orders']}|{r['qty']}|{r['net_amount_usd']}" for r in expected['gold_sales_daily']])
    result.update({'expected_totals': want, 'gold_rows': {'postgres': len(pg_rows), 'mysql': len(my_rows),
                                                          'manifest': len(want_rows)},
                   'postgres_equals_manifest': sorted(pg_rows) == want_rows,
                   'mysql_equals_manifest': sorted(my_rows) == want_rows,
                   'postgres_equals_mysql': pg_rows == my_rows})
    result['ok'] = (result['postgres_totals'] == want and result['mysql_totals'] == want
                    and result['postgres_equals_manifest'] and result['mysql_equals_manifest']
                    and result['postgres_equals_mysql'])
    path = write_evidence(directory, 'sqlcheck', result, evidence_dir)
    print(json.dumps({'ok': result['ok'], 'evidence': str(path)}, indent=2))
    return 0 if result['ok'] else 1


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--stack-dir', help=f'default $ETL_STACK_DIR or {DEFAULT_STACK_DIR}')
    parser.add_argument('--evidence-dir', help='where verify/sqlcheck write evidence (default <stack>/evidence)')
    parser.add_argument('--rows', type=int, default=gen_orders.SMOKE_ROWS,
                        help=f'sqlcheck dataset size (1..{gen_orders.DECLARED_ROW_LIMIT}, default the smoke size)')
    parser.add_argument('command', choices=('init', 'material', 'up', 'verify', 'sqlcheck', 'down'))
    args = parser.parse_args(argv)
    directory = stack_dir(args)
    for tool in ('docker', 'openssl'):
        if shutil.which(tool) is None:
            print(f'etlctl: {tool} is required', file=sys.stderr)
            return 2
    try:
        if args.command == 'init':
            cmd_init(directory)
        elif args.command == 'material':
            cmd_material(directory)
        elif args.command == 'up':
            cmd_up(directory)
        elif args.command == 'verify':
            return cmd_verify(directory, args.evidence_dir)
        elif args.command == 'sqlcheck':
            if not 1 <= args.rows <= gen_orders.DECLARED_ROW_LIMIT:
                parser.error(f'--rows must be 1..{gen_orders.DECLARED_ROW_LIMIT}')
            return cmd_sqlcheck(directory, args.evidence_dir, rows=args.rows)
        elif args.command == 'down':
            cmd_down(directory)
    except (CtlError, subprocess.TimeoutExpired) as exc:
        print(f'etlctl: {exc}', file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
