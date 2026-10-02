#!/usr/bin/env python3
"""Reconcile the known Rust ledger collision on an isolated database copy.

This is not a production migrator. It rejects normal database names and unknown
histories. The default run verifies restoration, then rolls back. PostgreSQL
sequences can advance during a rolled-back migration. Use a disposable copy.
"""

import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[2]
MIGRATIONS = ROOT / "services/elitea-main/migrations/shared"
OLD_RECEIPTS = (
    (111, "mcp_prebuilt_parameter_schema",
     "e0d3ebd4b3c7d03810a93d3e824d9a391ff2c870cbd79832c6cdca1028e86289"),
    (112, "toolkit_execute_read",
     "388a51a21473470faeff162e743e6ca3af079d5b72b52f921d6ee7565734355b"),
)
# Restore parents before children. TRUNCATE has no CASCADE option. An unknown
# referencing table therefore stops the transaction before any migration runs.
TABLES = (
    "execution_jobs", "execution_claims", "agent_execution_jobs",
    "command_outbox", "execution_replay_events", "execution_replay_state",
    "execution_settlements", "index_ingest_jobs", "index_result_artifacts",
    "output_inbox", "configuration_validation_results", "index_ingest_results",
    "toolkit_execute_read_jobs", "toolkit_execute_read_results",
)


def validate_target(container, database):
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}", container):
        raise ValueError("invalid container name")
    if not re.fullmatch(r"elitea_cutover_[a-z0-9_]{1,40}", database):
        raise ValueError("only an elitea_cutover_ database copy is allowed")


def load_manifest():
    manifest = []
    for path in sorted(MIGRATIONS.iterdir()):
        match = re.fullmatch(r"([0-9]{4})_([a-z][a-z0-9_]*)\.sql", path.name)
        if not match or not path.is_file():
            raise ValueError("unexpected migration artifact")
        sql = path.read_bytes()
        manifest.append((int(match[1]), match[2], hashlib.sha256(sql).hexdigest(), sql))
    versions = [1, *range(30, 92), *range(93, 124)]
    if [item[0] for item in manifest] != versions:
        raise ValueError("this procedure requires the known manifest through 123")
    return manifest


def lock_key():
    # Match internal/infra/db/migrate/lock.go. PostgreSQL accepts signed int64.
    value = 14695981039346656037
    for byte in b"elitea-platform:migrations:shared\0platform":
        value = ((value ^ byte) * 1099511628211) & ((1 << 64) - 1)
    return value if value < (1 << 63) else value - (1 << 64)


def assert_sql(condition, message):
    return f"DO $proof$ BEGIN IF NOT ({condition}) THEN RAISE EXCEPTION '{message}'; END IF; END $proof$;\n"


def receipt_values(receipts):
    return ",\n".join(f"({version},'{name}',decode('{digest}','hex'))"
                      for version, name, digest in receipts)


def same_rows(left, right, projection="*"):
    return (f"NOT EXISTS ((SELECT {projection} FROM {left} EXCEPT ALL SELECT {projection} FROM {right}) "
            f"UNION ALL (SELECT {projection} FROM {right} EXCEPT ALL SELECT {projection} FROM {left}))")


def build_sql(database, *, apply_copy=False, fail_before_commit=False):
    validate_target("validated", database)
    manifest = load_manifest()
    old = [item[:3] for item in manifest if item[0] <= 110] + list(OLD_RECEIPTS)
    relations = ",".join(f"elitea_runtime.{name}" for name in TABLES)
    relation_oids = ",".join(f"'elitea_runtime.{name}'::regclass" for name in TABLES)
    chunks = [
        "\\set ON_ERROR_STOP on\n\\set VERBOSITY sqlstate\n",
        "BEGIN; SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='120s';\n",
        "SET LOCAL idle_in_transaction_session_timeout='30s'; SET LOCAL client_min_messages=error;\n",
        f"SELECT pg_advisory_xact_lock({lock_key()});\n",
        assert_sql(f"current_database()='{database}'", "unexpected_database"),
        assert_sql("NOT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() "
                   "AND pid<>pg_backend_pid() AND backend_type='client backend')", "copy_has_other_clients"),
        "LOCK TABLE elitea_runtime.schema_migrations IN ACCESS EXCLUSIVE MODE;\n",
        "CREATE TEMP TABLE expected_old(version bigint,name text,checksum bytea) ON COMMIT DROP;\n",
        "INSERT INTO expected_old VALUES " + receipt_values(old) + ";\n",
        assert_sql(same_rows("expected_old", "(SELECT version,name,checksum FROM elitea_runtime.schema_migrations "
                             "WHERE target_kind='shared' AND target_id='platform') AS actual"), "unexpected_ledger"),
        "\\echo verified_old_ledger\n",
        f"LOCK TABLE {relations} IN ACCESS EXCLUSIVE MODE;\n",
        assert_sql(f"NOT EXISTS (SELECT 1 FROM pg_class WHERE oid IN ({relation_oids}) "
                   "AND (relkind<>'r' OR relrowsecurity))", "unsupported_relation"),
        assert_sql(f"NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid IN ({relation_oids}) "
                   "AND NOT tgisinternal)", "unexpected_trigger"),
        assert_sql(f"(SELECT sum(pg_total_relation_size(oid)) FROM pg_class WHERE oid IN ({relation_oids}))"
                   " <= 134217728", "copy_exceeds_rehearsal_bound"),
    ]
    for name in TABLES:
        chunks.append(f"CREATE TEMP TABLE saved_{name} ON COMMIT DROP AS TABLE elitea_runtime.{name};\n")
    # Keep the complete receipts, including their original timestamps.
    chunks.extend([
        "CREATE SCHEMA elitea_migration_audit;\n",
        "CREATE TABLE elitea_migration_audit.rust_branch_receipts_20260908 AS "
        "SELECT *, '046e84a2'::text AS source_revision, "
        "CASE version WHEN 111 THEN 123 ELSE 122 END::bigint AS replacement_version "
        "FROM elitea_runtime.schema_migrations "
        "WHERE target_kind='shared' AND target_id='platform' AND version IN (111,112);\n",
        assert_sql("(SELECT count(*) FROM elitea_migration_audit.rust_branch_receipts_20260908)=2", "missing_archive"),
        "\\echo captured_runtime_and_receipts\n",
        f"TRUNCATE TABLE {relations} CONTINUE IDENTITY;\n",
        "DROP TABLE elitea_runtime.toolkit_execute_read_results;\n",
        "DROP TABLE elitea_runtime.toolkit_execute_read_jobs;\n",
        "DELETE FROM elitea_runtime.schema_migrations "
        "WHERE target_kind='shared' AND target_id='platform' AND version IN (111,112);\n",
    ])
    for version, name, digest, sql in manifest:
        if version <= 110:
            continue
        # Execute the original artifact bytes. Never alter SQL and record the
        # checksum of a different artifact as though that artifact had run.
        chunks.extend([
            sql.decode("utf-8"), "\n",
            "INSERT INTO elitea_runtime.schema_migrations "
            "(target_kind,target_id,version,name,checksum) VALUES "
            f"('shared','platform',{version},'{name}',decode('{digest}','hex'));\n",
            f"\\echo applied_shared_{version}\n",
        ])
    for name in TABLES:
        # These tables have no generated columns. An incompatible schema change
        # stops the INSERT or the complete row comparison; it never drops data.
        chunks.extend([
            f"INSERT INTO elitea_runtime.{name} OVERRIDING SYSTEM VALUE SELECT * FROM saved_{name};\n",
            assert_sql(same_rows(f"elitea_runtime.{name} AS r", f"saved_{name} AS r", "to_jsonb(r)"),
                       f"restoration_mismatch_{name}"),
            f"SELECT json_build_object('restored_table','{name}','rows',count(*)) FROM saved_{name};\n",
        ])
    chunks.extend([
        "SET CONSTRAINTS ALL IMMEDIATE;\n",
        "CREATE TEMP TABLE expected_new(version bigint,name text,checksum bytea) ON COMMIT DROP;\n",
        "INSERT INTO expected_new VALUES " + receipt_values(item[:3] for item in manifest) + ";\n",
        assert_sql(same_rows("expected_new", "(SELECT version,name,checksum FROM elitea_runtime.schema_migrations "
                             "WHERE target_kind='shared' AND target_id='platform') AS actual"), "final_ledger_mismatch"),
        "\\echo verified_runtime_and_canonical_ledger\n",
    ])
    if fail_before_commit:
        chunks.append(assert_sql("false", "injected_precommit_failure"))
    chunks.extend(["COMMIT;\n" if apply_copy else "ROLLBACK;\n",
                   "\\echo copy_committed\n" if apply_copy else "\\echo copy_rolled_back\n"])
    return "".join(chunks)


def execute(container, database, sql):
    validate_target(container, database)
    command = ["docker", "exec", "-i", container, "sh", "-c",
               'exec psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$1"', "sh", database]
    result = subprocess.run(command, input=sql, capture_output=True, text=True, timeout=600)
    # SQL errors can include existing row contents. Only emit fixed progress
    # markers, counts, and SQLSTATE. Never emit provider or credential payloads.
    progress = [line for line in result.stdout.splitlines()
                if re.fullmatch(r"(?:verified|captured|applied|copy)_[a-z0-9_]+", line)]
    restored = []
    for line in result.stdout.splitlines():
        if line.startswith('{"restored_table"'):
            value = json.loads(line)
            if value.get("restored_table") in TABLES and isinstance(value.get("rows"), int):
                restored.append(value)
    states = re.findall(r"ERROR:\s+([0-9A-Z]{5})\b", result.stderr)
    return {"database": database, "exit_code": result.returncode,
            "progress": progress, "restored": restored, "sqlstates": states}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--container", required=True)
    parser.add_argument("--database", required=True)
    parser.add_argument("--apply-copy", action="store_true", help="commit only the named isolated copy")
    parser.add_argument("--fail-before-commit", action="store_true", help="inject a rollback proof failure")
    args = parser.parse_args()
    try:
        validate_target(args.container, args.database)
        sql = build_sql(args.database, apply_copy=args.apply_copy, fail_before_commit=args.fail_before_commit)
        result = execute(args.container, args.database, sql)
    except (ValueError, OSError, subprocess.TimeoutExpired) as error:
        print(json.dumps({"error": type(error).__name__, "details_withheld": True}))
        return 1
    print(json.dumps(result))
    return 0 if result["exit_code"] == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
