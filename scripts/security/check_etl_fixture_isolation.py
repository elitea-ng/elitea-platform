#!/usr/bin/env python3
"""Guard: the ETL acceptance fixtures (scripts/acceptance/etl) stay test-only and TLS-only.

1. No shipped artifact references them: nothing under deploy/ (compose files, Helm charts, kind, Argo CD, runtime
   material) and no Containerfile or Dockerfile anywhere names the fixture services, the fixture directory or the
   mock. The default compose file and the charts can therefore never start a test warehouse.
2. The fixture compose file keeps its own guarantees: TLS-only PostgreSQL (ssl=on with a hostssl-only pg_hba.conf
   that rejects hostnossl), TLS-only MySQL (require_secure_transport, users REQUIRE SSL), no published ports, images
   pinned by digest, and no literal credentials (every password comes from the local, uncommitted env file).

Run from the repository root (CI runs every scripts/security/check_*.py). Exit 1 lists every finding.
"""
from __future__ import annotations

import argparse
import os
import pathlib
import re
import sys

FIXTURE_DIR = pathlib.Path('scripts/acceptance/etl')
REFERENCE = re.compile(r'warehouse-(?:pg|mysql)|etl[-_]mocks?\b|etl_mock\.py|elitea-etl-fixtures|scripts/acceptance')
SHIPPED_ROOTS = ('deploy',)
IMAGE_FILE = re.compile(r'^(?:Containerfile|Dockerfile)(?:\..+)?$|\.(?:containerfile|dockerfile)$', re.IGNORECASE)
SKIP_DIRS = {'.git', 'node_modules', 'target', '.venv', 'venv', '__pycache__', 'dist', 'build'}
MAX_FILE_BYTES = 4 * 1024 * 1024


def shipped_files(root):
    """Everything under deploy/, plus every image build file anywhere except inside the fixture directory."""
    for base in SHIPPED_ROOTS:
        for dirpath, dirnames, filenames in os.walk(root / base):
            dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
            for name in filenames:
                yield pathlib.Path(dirpath) / name
    fixture = root / FIXTURE_DIR
    for dirpath, dirnames, filenames in os.walk(root):
        here = pathlib.Path(dirpath)
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS and here / d != fixture]
        for name in filenames:
            if IMAGE_FILE.search(name):
                yield here / name


def check_references(root):
    findings = []
    for path in sorted(set(shipped_files(root))):
        try:
            if path.stat().st_size > MAX_FILE_BYTES:
                continue
            text = path.read_text(encoding='utf-8', errors='replace')
        except OSError:
            continue
        for line_no, line in enumerate(text.splitlines(), 1):
            if REFERENCE.search(line):
                findings.append(f'{path.relative_to(root)}:{line_no}: shipped artifact references the ETL test '
                                'fixtures (they must stay under scripts/acceptance/etl)')
    return findings


def _service_block(compose_text, service):
    match = re.search(rf'^  {re.escape(service)}:\n((?:    .*\n|\n)*)', compose_text, re.MULTILINE)
    return match.group(1) if match else ''


def check_fixture_compose(root):
    findings = []
    fixture = root / FIXTURE_DIR
    compose_path = fixture / 'compose.yaml'
    if not compose_path.exists():
        return [f'{compose_path.relative_to(root)} is missing']
    # Comments never count as configuration: strip them before any check.
    text = '\n'.join(re.sub(r'(^|\s)#.*$', '', line) for line in compose_path.read_text(encoding='utf-8')
                     .splitlines()) + '\n'
    rel = compose_path.relative_to(root)
    if re.search(r'^\s+ports:', text, re.MULTILINE):
        findings.append(f'{rel}: publishes ports; the fixtures are reachable only on their own network')
    for line_no, line in enumerate(text.splitlines(), 1):
        image = re.match(r'^\s+image:\s*(\S+)', line)
        if image and '@sha256:' not in image.group(1):
            findings.append(f'{rel}:{line_no}: image is not pinned by digest')
        # Mapping form (KEY: value) and list form (- KEY=value), quoted or not.
        secret = re.match(r'^\s+(?:-\s*)?["\']?([A-Z0-9_]*(?:PASSWORD|TOKEN|SECRET|PWD)[A-Z0-9_]*)\s*[:=]\s*(.+?)'
                          r'["\']?$', line)
        if secret and not secret.group(1).endswith('_FILE') and not re.fullmatch(r'\$\{[A-Z0-9_]+:\?[^}]*\}', secret.group(2).strip()):
            findings.append(f'{rel}:{line_no}: {secret.group(1)} must come from the local env file (${{VAR:?...}})')
    pg = _service_block(text, 'warehouse-pg')
    for forbidden in ('ssl=off', 'ssl=false', 'hba_file=/var', 'listen_addresses'):
        if forbidden in pg:
            findings.append(f'{rel}: warehouse-pg must not set {forbidden}')
    for needed in ('ssl=on', 'hba_file=/etl/warehouse/pg/pg_hba.conf', 'ssl_min_protocol_version=TLSv1.2'):
        if needed not in pg:
            findings.append(f'{rel}: warehouse-pg must run with {needed}')
    mysql = _service_block(text, 'warehouse-mysql')
    for forbidden in ('require-secure-transport=OFF', 'require_secure_transport=OFF', 'skip-ssl', 'ssl=0',
                      'tls-version=TLSv1,', 'tls-version=TLSv1.1'):
        if forbidden.lower() in mysql.lower():
            findings.append(f'{rel}: warehouse-mysql must not set {forbidden}')
    for needed in ('--require-secure-transport=ON', '--ssl-cert=', '--ssl-key='):
        if needed not in mysql:
            findings.append(f'{rel}: warehouse-mysql must run with {needed}')
    mocks = _service_block(text, 'etl-mocks')
    for needed in ('ETL_MOCK_TLS_CERT', 'ETL_MOCK_TLS_KEY', 'read_only: true', 'cap_drop: [ALL]'):
        if needed not in mocks:
            findings.append(f'{rel}: etl-mocks must keep {needed}')
    hba_path = fixture / 'warehouse' / 'pg' / 'pg_hba.conf'
    hba = [line.split() for line in hba_path.read_text(encoding='utf-8').splitlines()
           if line.strip() and not line.lstrip().startswith('#')] if hba_path.exists() else []
    if not hba:
        findings.append(f'{hba_path.relative_to(root)} is missing or empty')
    for fields in hba:
        if fields[0] in ('host', 'hostgssenc', 'hostnogssenc'):
            findings.append(f'{hba_path.relative_to(root)}: "{fields[0]}" accepts connections without TLS')
        if fields[0] == 'hostnossl' and fields[-1] != 'reject':
            findings.append(f'{hba_path.relative_to(root)}: hostnossl must be reject')
        if fields[0] == 'hostssl' and fields[-1] in ('trust', 'password', 'md5'):
            findings.append(f'{hba_path.relative_to(root)}: hostssl must use scram-sha-256')
    if not any(f[0] == 'hostnossl' and f[-1] == 'reject' for f in hba):
        findings.append(f'{hba_path.relative_to(root)}: needs an explicit "hostnossl ... reject"')
    users = fixture / 'warehouse' / 'mysql' / 'init' / '010_users.sh'
    if users.exists():
        created = re.findall(r'CREATE USER [^;]+;', users.read_text(encoding='utf-8'))
        if not created or any('REQUIRE SSL' not in statement for statement in created):
            findings.append(f'{users.relative_to(root)}: every MySQL user must be created with REQUIRE SSL')
    else:
        findings.append(f'{users.relative_to(root)} is missing')
    return findings


def run(root):
    root = pathlib.Path(root).resolve()
    return check_references(root) + check_fixture_compose(root)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--root', default='.', help='repository root (default: the current directory)')
    args = parser.parse_args(argv)
    findings = run(args.root)
    for finding in findings:
        print(finding)
    if findings:
        print(f'check_etl_fixture_isolation: {len(findings)} finding(s)')
        return 1
    print('check_etl_fixture_isolation: OK')
    return 0


if __name__ == '__main__':
    sys.exit(main())
