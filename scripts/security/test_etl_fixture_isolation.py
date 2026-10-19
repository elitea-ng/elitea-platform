"""Tests for check_etl_fixture_isolation.py: the real tree passes, and each forbidden change is caught."""
import pathlib
import shutil
import sys
import tempfile
import unittest

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parent.parent
sys.path.insert(0, str(HERE))
import check_etl_fixture_isolation as guard  # noqa: E402

FIXTURE = REPO / 'scripts' / 'acceptance' / 'etl'


class Guard(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tmp.name)
        (self.root / 'deploy' / 'helm' / 'elitea' / 'templates').mkdir(parents=True)
        (self.root / 'deploy' / 'docker-compose.yml').write_text('services:\n  postgres:\n    image: postgres\n')
        (self.root / 'services' / 'svc').mkdir(parents=True)
        (self.root / 'services' / 'svc' / 'Containerfile').write_text('FROM scratch\nCOPY bin /bin\n')
        target = self.root / 'scripts' / 'acceptance' / 'etl'
        target.mkdir(parents=True)
        shutil.copy(FIXTURE / 'compose.yaml', target / 'compose.yaml')
        shutil.copytree(FIXTURE / 'warehouse', target / 'warehouse')

    def tearDown(self):
        self.tmp.cleanup()

    def edit(self, relative, old, new):
        path = self.root / relative
        text = path.read_text()
        self.assertIn(old, text)
        path.write_text(text.replace(old, new, 1))

    def test_the_repository_passes(self):
        self.assertEqual(guard.run(REPO), [])

    def test_a_clean_tree_passes(self):
        self.assertEqual(guard.run(self.root), [])

    def test_default_compose_referencing_a_warehouse_fails(self):
        self.edit('deploy/docker-compose.yml', '  postgres:\n', '  warehouse-pg:\n')
        self.assertTrue(any('deploy/docker-compose.yml:2' in f for f in guard.run(self.root)))

    def test_helm_template_referencing_the_mock_fails(self):
        (self.root / 'deploy/helm/elitea/templates/fx.yaml').write_text('url: https://etl-mocks:8443\n')
        self.assertTrue(any('fx.yaml:1' in f for f in guard.run(self.root)))

    def test_an_image_copying_the_fixture_directory_fails(self):
        self.edit('services/svc/Containerfile', 'COPY bin /bin', 'COPY scripts/acceptance /opt/acceptance')
        self.assertTrue(any('Containerfile:2' in f for f in guard.run(self.root)))

    def test_published_ports_fail(self):
        self.edit('scripts/acceptance/etl/compose.yaml', '    hostname: warehouse-pg\n',
                  '    hostname: warehouse-pg\n    ports: ["5432:5432"]\n')
        self.assertTrue(any('publishes ports' in f for f in guard.run(self.root)))

    def test_unpinned_image_fails(self):
        path = self.root / 'scripts/acceptance/etl/compose.yaml'
        text = path.read_text()
        start = text.index('image: docker.io/library/mysql')
        end = text.index('\n', start)
        path.write_text(text[:start] + 'image: mysql:8.4' + text[end:])
        self.assertTrue(any('not pinned by digest' in f for f in guard.run(self.root)))

    def test_literal_password_fails(self):
        self.edit('scripts/acceptance/etl/compose.yaml', '${ETL_MYSQL_ROOT_PASSWORD:?run etlctl.py init}',
                  'example-not-a-secret')
        self.assertTrue(any('MYSQL_ROOT_PASSWORD must come from the local env file' in f
                            for f in guard.run(self.root)))

    def test_list_form_literal_password_fails(self):
        self.edit('scripts/acceptance/etl/compose.yaml', '      POSTGRES_DB: warehouse\n',
                  '      POSTGRES_DB: warehouse\n    environment2:\n      - "PGPASSWORD=example-not-a-secret"\n')
        self.assertTrue(any('PGPASSWORD must come from the local env file' in f for f in guard.run(self.root)))

    def test_ssl_off_after_ssl_on_fails(self):
        self.edit('scripts/acceptance/etl/compose.yaml', '      - ssl=on\n', '      - ssl=on\n      - -c\n      - ssl=off\n')
        self.assertTrue(any('must not set ssl=off' in f for f in guard.run(self.root)))

    def test_tls_flag_only_in_a_comment_fails(self):
        self.edit('scripts/acceptance/etl/compose.yaml', '      - --require-secure-transport=ON\n',
                  '      # --require-secure-transport=ON\n')
        self.assertTrue(any('--require-secure-transport=ON' in f for f in guard.run(self.root)))

    def test_fixture_unit_suites_pass(self):
        """Runs the fixture's own unittest suites, so CI (pytest over scripts/security/test_*.py) enforces them."""
        import subprocess
        result = subprocess.run([sys.executable, '-m', 'unittest', 'discover', '-s',
                                 str(REPO / 'scripts' / 'acceptance' / 'etl' / 'tests')],
                                capture_output=True, text=True, timeout=600, cwd=REPO)
        self.assertEqual(result.returncode, 0, result.stderr[-3000:])
        ran = int(__import__('re').search(r'Ran (\d+) tests?', result.stderr).group(1))
        self.assertGreaterEqual(ran, 37)
        self.assertNotIn('skipped', result.stderr.splitlines()[-1])

    def test_postgres_without_tls_fails(self):
        self.edit('scripts/acceptance/etl/compose.yaml', '      - ssl=on\n', '      - ssl=off\n')
        self.assertTrue(any('warehouse-pg must run with ssl=on' in f for f in guard.run(self.root)))

    def test_plain_host_rule_in_pg_hba_fails(self):
        self.edit('scripts/acceptance/etl/warehouse/pg/pg_hba.conf', 'hostnossl  all       all   0.0.0.0/0   reject',
                  'host       all       all   0.0.0.0/0   scram-sha-256')
        self.assertTrue(any('accepts connections without TLS' in f for f in guard.run(self.root)))

    def test_mysql_without_secure_transport_fails(self):
        self.edit('scripts/acceptance/etl/compose.yaml', '      - --require-secure-transport=ON\n', '')
        self.assertTrue(any('--require-secure-transport=ON' in f for f in guard.run(self.root)))

    def test_mysql_user_without_require_ssl_fails(self):
        self.edit('scripts/acceptance/etl/warehouse/mysql/init/010_users.sh', "' REQUIRE SSL;", "';")
        self.assertTrue(any('REQUIRE SSL' in f for f in guard.run(self.root)))

    def test_mock_without_tls_fails(self):
        self.edit('scripts/acceptance/etl/compose.yaml', '      ETL_MOCK_TLS_KEY: /etl/tls/etl-mocks.key\n', '')
        self.assertTrue(any('ETL_MOCK_TLS_KEY' in f for f in guard.run(self.root)))


if __name__ == '__main__':
    unittest.main()
