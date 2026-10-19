"""Lint of the pipeline drafts (no YAML library in the standard library, so it reads the drafts' own block style):
every reference resolves, every not-yet-admitted node names the item that unblocks it, no URL or credential is in
YAML, and the set covers every Point 5 node family and the Split Out / Aggregate modes listed in README.md."""
import pathlib
import re
import unittest

PIPELINES = pathlib.Path(__file__).resolve().parent.parent / 'pipelines'
FILES = sorted(PIPELINES.glob('*.yaml'))
NOT_ADMITTED = {'map', 'parallel', 'http', 'db'}
# POINT5-REMAINING.md items a TODO may name, plus decisions X1-X4 and items found by this work (NEW).
KNOWN_ITEMS = {'0.4', '0.8', '1.3', '1.5', '1.9', '1.12', '1.14', '1.15', '2.4'}
TODO = re.compile(r'TODO\(([^)]+)\)')

COVERAGE = {
    'Code python (prepared packages)': r"language: python[\s\S]*?micropip\.install\(\['python-dateutil",
    'Code javascript': r'language: javascript',
    'Code typescript': r'language: typescript',
    'Split Out mode list': r'split: \{mode: list\}',
    'Split Out mode row_field': r'split: \{mode: row_field',
    'Split Out mode rows_field': r'split: \{mode: rows_field',
    'Split Out retain none (default)': r'split: \{mode: list\}\n    destination',
    'Split Out retain all': r'retain: \{mode: all\}',
    'Split Out retain only (+ missing: null)': r'mode: only\n      fields:[\s\S]*?missing: null',
    'Split Out retain except': r'retain: \{mode: except',
    'Split Out missing_list/null_list empty': r'missing_list: empty\n    null_list: empty',
    'Aggregate layout plain + group_by': r'type: aggregate\n    source: silver_rows\n    output: \[country_stats\]\n    group_by',
    'Aggregate layout split_out': r'layout: split_out',
    'Aggregate regroup parent': r'regroup: parent',
    'Aggregate group_by missing: null': r'group_by:\n      - \{path: /expectation, output: expectation, missing: null\}',
    'Aggregate count_rows': r'operation: count_rows',
    'Aggregate collect_rows (retain only)': r'operation: collect_rows, retain: \{mode: only',
    'Aggregate collect merge_lists': r'operation: collect, .*merge_lists: true',
    'Aggregate sum_int': r'operation: sum_int',
    'Aggregate min_int / max_int': r'operation: min_int[\s\S]*operation: max_int|operation: max_int[\s\S]*operation: min_int',
    'Aggregate first / last': r'operation: first[\s\S]*operation: last',
    'Aggregate field policies skip/keep': r'missing: skip, null: skip[\s\S]*null: keep|null: keep[\s\S]*missing: skip',
    'Map': r'type: map\n',
    'Parallel (two of them)': r'type: parallel[\s\S]*type: parallel',
    'HTTP GET (FX rates)': r'type: http\n    revision: 2\n    operation: fx_reference\.rates',
    'HTTP POST (webhook, idempotency key)': r'operation: etl_webhook\.complete[\s\S]*?idempotency_key: \{from: run_id\}',
    'DB write PostgreSQL': r'operation: warehouse_pg\.load\n    mode: write',
    'DB write MySQL': r'operation: warehouse_mysql\.load\n    mode: write',
    'DB read both dialects': r'operation: warehouse_pg\.read\n    mode: read[\s\S]*operation: warehouse_mysql\.read',
    'Router': r'type: router',
    'HITL': r'type: hitl',
    'State modifier + typed reducers': r'reducer: merge[\s\S]*reducer: append[\s\S]*reducer: sum_int',
    'LLM': r'type: llm',
    'Printer': r'type: printer',
    'Toolkit list_files': r'type: toolkit\n    toolkit_name: ETL artifacts\n    tool: list_files',
    'Toolkit read_file': r'tool: read_file',
    'Toolkit read_multiple_files': r'tool: read_multiple_files',
    'Toolkit create_file': r'tool: create_file',
    'Application (saved pipeline) node': r'type: agent\n    tool: ',
    '5b retry_on_fail': r'retry_on_fail: true',
    '5b on_error continue': r'on_error: continue',
    '5b on_error error_output': r'on_error: error_output\n      error_transition:',
    '5b execute_once': r'execute_once: true',
    '5b always_output_data': r'always_output_data: true',
    '5b timeout_ms': r'settings:[\s\S]*?timeout_ms:',
}


def nodes(text):
    """[(id, block text)] for every `  - id:` block."""
    parts = re.split(r'\n(?=  - id: )', text)
    return [(re.match(r'  - id: (\S+)', p).group(1), p) for p in parts[1:]]


def preceding_comment(text, block):
    """The comment lines directly above a node block."""
    before = text[:text.index(block)].rstrip('\n').splitlines()
    out = []
    for line in reversed(before):
        if not line.strip().startswith('#'):
            break
        out.append(line)
    return '\n'.join(reversed(out))


class PipelineFixtures(unittest.TestCase):
    def test_the_set_is_complete(self):
        self.assertEqual([f.name for f in FILES], ['bronze_ingest.yaml', 'etl_job.yaml', 'gold_build.yaml',
                                                   'load_mysql.yaml', 'load_postgres.yaml', 'silver_batch.yaml',
                                                   'silver_clean.yaml'])

    def test_drafts_are_marked_and_references_resolve(self):
        for path in FILES:
            text = path.read_text()
            with self.subTest(file=path.name):
                self.assertTrue(text.startswith('# DRAFT fixture'), 'missing the DRAFT header')
                ids = {node_id for node_id, _ in nodes(text)}
                entry = re.search(r'^entry_point: (\S+)$', text, re.MULTILINE).group(1)
                self.assertIn(entry, ids)
                targets = re.findall(r'^\s+(?:transition|default_output|error_transition|worker|node): (\S+)$',
                                     text, re.MULTILINE)
                targets += [t for r in re.findall(r'routes: \[([^\]]+)\]', text) for t in r.split(', ')]
                targets += re.findall(r'^\s+(?:approve|reject): (\S+)$', text, re.MULTILINE)
                for target in targets:
                    self.assertIn(target, ids | {'END'}, f'unknown node {target}')

    def test_unadmitted_nodes_name_the_item_that_unblocks_them(self):
        for path in FILES:
            text = path.read_text()
            for node_id, block in nodes(text):
                kind = re.search(r'^    type: (\S+)$', block, re.MULTILINE).group(1)
                context = preceding_comment(text, block) + block
                if kind in NOT_ADMITTED or '    settings:' in block:
                    with self.subTest(file=path.name, node=node_id):
                        self.assertRegex(context, TODO, f'{kind} node without a TODO(<item>)')
        for path in FILES:
            for item in TODO.findall(path.read_text()):
                with self.subTest(file=path.name, item=item):
                    self.assertTrue(item in KNOWN_ITEMS or re.fullmatch(r'X\d|NEW', item), item)

    def test_no_url_host_or_credential_in_yaml(self):
        forbidden = re.compile(r'https?://|etl-mocks|warehouse-(?:pg|mysql)\b|:8443|password|passwd|secret|'
                               r'api[_-]?key|bearer\s+\S|BEGIN [A-Z ]*PRIVATE KEY', re.IGNORECASE)
        for path in FILES:
            for line_no, line in enumerate(path.read_text().splitlines(), 1):
                with self.subTest(file=path.name, line=line_no):
                    self.assertIsNone(forbidden.search(line), line)

    def test_coverage_of_every_point5_family_and_mode(self):
        corpus = '\n'.join(path.read_text() for path in FILES)
        for feature, pattern in COVERAGE.items():
            with self.subTest(feature=feature):
                self.assertRegex(corpus, pattern)

    def test_sql_is_frozen_text_with_typed_parameters(self):
        for path in FILES:
            text = path.read_text()
            for node_id, block in nodes(text):
                if '    type: db' not in block:
                    continue
                with self.subTest(file=path.name, node=node_id):
                    statements = re.findall(r'- sql: ', block)
                    params = re.findall(r'\{from: \w+, type: (\w+)\}', block)
                    self.assertTrue(1 <= len(statements) <= 16)
                    self.assertTrue(set(params) <= {'text', 'int8', 'numeric', 'bool', 'timestamptz', 'date',
                                                    'uuid', 'json'})
                    self.assertNotRegex(block, r'\{\{|\{[a-z_]+\}', 'templating inside SQL')
                    if 'mode: read' in block:
                        self.assertEqual(len(statements), 1)


if __name__ == '__main__':
    unittest.main()
