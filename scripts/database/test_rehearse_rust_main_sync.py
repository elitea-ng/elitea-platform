"""Run unit checks and optional destructive-copy-only reconciliation proofs.

Set ELITEA_RUST_CUTOVER_TEST_DATABASE and ELITEA_RUST_CUTOVER_TEST_CONTAINER
to test a disposable copy with the known old ledger. The integration test
commits that copy's upgrade. It never creates or deletes a database.
"""

import os
import subprocess
import unittest
from unittest.mock import patch

import rehearse_rust_main_sync as sync


COPY = "elitea_cutover_unit_fixture"


class ReconciliationUnitTests(unittest.TestCase):
    def test_live_names_and_shell_inputs_are_rejected(self):
        for database in ("elitea", "agentstate", "postgres", "elitea_cutover_", "elitea_cutover_a';SELECT 1;--"):
            with self.subTest(database=database), self.assertRaises(ValueError):
                sync.validate_target("postgres-fixture", database)
        for container in ("-i", "", "fixture; echo unsafe", "a\nb"):
            with self.subTest(container=container), self.assertRaises(ValueError):
                sync.validate_target(container, COPY)

    def test_default_rolls_back_and_preserves_sql_bytes(self):
        sql = sync.build_sql(COPY)
        self.assertTrue(sql.endswith("ROLLBACK;\n\\echo copy_rolled_back\n"))
        self.assertNotIn("CASCADE;", sql)
        self.assertLess(sql.index("unexpected_ledger"), sql.index("TRUNCATE TABLE"))
        self.assertLess(sql.index("restoration_mismatch_"), sql.index("ROLLBACK;"))
        for version, _, _, artifact in sync.load_manifest():
            if version > 110:
                self.assertEqual(sql.count(artifact.decode()), 1)

    def test_commit_requires_explicit_option_and_keeps_failure_before_commit(self):
        sql = sync.build_sql(COPY, apply_copy=True, fail_before_commit=True)
        self.assertLess(sql.index("injected_precommit_failure"), sql.rindex("COMMIT;"))
        self.assertTrue(sql.endswith("COMMIT;\n\\echo copy_committed\n"))

    def test_errors_never_return_sql_rows_or_raw_diagnostics(self):
        completed = subprocess.CompletedProcess([], 1,
            "verified_old_ledger\nsecret_provider_payload\n", "ERROR: P0001\nDETAIL: credential-content\n")
        with patch.object(sync.subprocess, "run", return_value=completed):
            result = sync.execute("fixture", COPY, "SELECT 1")
        self.assertEqual(result["progress"], ["verified_old_ledger"])
        self.assertEqual(result["sqlstates"], ["P0001"])
        self.assertNotIn("secret", str(result))
        self.assertNotIn("credential", str(result))


@unittest.skipUnless(os.getenv("ELITEA_RUST_CUTOVER_TEST_DATABASE"), "requires an explicit disposable database copy")
class ReconciliationDatabaseTests(unittest.TestCase):
    def test_copy_collision_reconciliation(self):
        database = os.environ["ELITEA_RUST_CUTOVER_TEST_DATABASE"]
        container = os.environ["ELITEA_RUST_CUTOVER_TEST_CONTAINER"]
        sync.validate_target(container, database)

        def query(sql):
            result = subprocess.run(
                ["docker", "exec", "-i", container, "sh", "-c",
                 'exec psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$1"', "sh", database],
                input=sql, text=True, capture_output=True, timeout=120)
            self.assertEqual(result.returncode, 0, "copy verification SQL failed; diagnostics withheld")
            return result.stdout.strip()

        def row_digest():
            # Compare complete logical rows, not physical ordering. Sequences
            # are not transactional and can advance during a failed migration.
            return query("""
CREATE TEMP TABLE proof_digest(relation text,digest text);
DO $proof$
DECLARE item record; value text;
BEGIN
  FOR item IN SELECT n.nspname,c.relname FROM pg_class c
    JOIN pg_namespace n ON n.oid=c.relnamespace
    WHERE c.relkind='r' AND n.nspname !~ '^pg_' AND n.nspname<>'information_schema'
  LOOP
    EXECUTE format($query$ SELECT md5(coalesce(string_agg(row_hash,'' ORDER BY row_hash),''))
      FROM (SELECT md5(to_jsonb(r)::text) AS row_hash FROM %I.%I AS r) AS rows $query$,
      item.nspname,item.relname) INTO value;
    INSERT INTO proof_digest VALUES(item.nspname||'.'||item.relname,value);
  END LOOP;
END $proof$;
SELECT md5(string_agg(relation||':'||digest,',' ORDER BY relation)) FROM proof_digest;
""")

        before = row_digest()
        sql = sync.build_sql(database)
        result = sync.execute(container, database, sql)
        self.assertEqual(result["exit_code"], 0, result)
        self.assertEqual(result["progress"][-1], "copy_rolled_back")
        self.assertEqual(row_digest(), before, "dry-run changes existing rows")

        result = sync.execute(container, database,
                              sync.build_sql(database, apply_copy=True, fail_before_commit=True))
        self.assertNotEqual(result["exit_code"], 0)
        self.assertEqual(result["progress"][-1], "verified_runtime_and_canonical_ledger")
        self.assertEqual(row_digest(), before, "precommit failure changes existing rows")

        # An unknown reference must stop TRUNCATE, not cascade into new data.
        unknown_dependency = """BEGIN;
CREATE TABLE public.cutover_unexpected_reference (
  execution_id text, generation bigint,
  FOREIGN KEY (execution_id,generation) REFERENCES elitea_runtime.execution_jobs(execution_id,generation)
);
"""
        result = sync.execute(container, database, unknown_dependency + sql.replace("BEGIN; SET LOCAL", "SET LOCAL", 1))
        self.assertNotEqual(result["exit_code"], 0)
        self.assertIn("0A000", result["sqlstates"])
        self.assertNotIn("applied_shared_111", result["progress"])
        self.assertEqual(row_digest(), before, "unknown dependency changes existing rows or schema")

        result = sync.execute(container, database, sync.build_sql(database, apply_copy=True))
        self.assertEqual(result["exit_code"], 0, result)
        self.assertEqual(result["progress"][-1], "copy_committed")
        self.assertEqual(len(result["restored"]), len(sync.TABLES))
        self.assertEqual(query("SELECT count(*) FROM elitea_migration_audit.rust_branch_receipts_20260908"), "2")
        self.assertEqual(query("SELECT max(version) FROM elitea_runtime.schema_migrations "
                               "WHERE target_kind='shared' AND target_id='platform'"), "123")

        after = row_digest()
        result = sync.execute(container, database, sync.build_sql(database, apply_copy=True))
        self.assertNotEqual(result["exit_code"], 0)
        self.assertNotIn("verified_old_ledger", result["progress"])
        self.assertEqual(row_digest(), after, "repeat invocation changes the upgraded copy")


if __name__ == "__main__":
    unittest.main()
