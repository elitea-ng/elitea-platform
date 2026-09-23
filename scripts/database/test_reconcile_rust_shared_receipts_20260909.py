"""Prove receipt reconciliation on newly created disposable databases only.

Set ELITEA_RUST_RECEIPT_TEST_CONTAINER to the local development PostgreSQL
container. Tests create and drop only their own elitea_cutover_receipts_ names.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
import uuid
from unittest.mock import patch

import reconcile_rust_shared_receipts_20260909 as sync

CONTAINER = os.getenv("ELITEA_RUST_RECEIPT_TEST_CONTAINER", "")
COPY = "elitea_cutover_receipts_unit"
MARKER = "elitea_receipt_reconciliation_disposable_copy_20260909"


def query(container, database, sql):
    # Database creation uses postgres only as an administrative connection.
    sync.validate_target(container, COPY)
    if database != "postgres":
        sync.validate_target(container,database)
    result=subprocess.run(["docker","exec","-i",container,"sh","-c",
      'exec psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$1"',"sh",database],
      input="\\set VERBOSITY sqlstate\n"+sql,text=True,capture_output=True,timeout=180)
    if result.returncode:
        states=re.findall(r"ERROR:\s+([0-9A-Z]{5})\b",result.stderr)
        raise RuntimeError("fixture SQL failed; states="+",".join(states))
    return result.stdout.strip()


def fixture_sql(oauth):
    prefix,_=sync.verified_artifacts()
    chunks=["BEGIN; SET LOCAL client_min_messages=error;\n",
            (sync.ROOT/"services/elitea-main/internal/infra/db/migrations/001_initial.sql").read_text()]
    for version,name,digest in prefix:
        chunks.append((sync.MIGRATIONS/"shared"/f"{version:04}_{name}.sql").read_text())
        chunks.append(f"INSERT INTO elitea_runtime.schema_migrations(target_kind,target_id,version,name,checksum) VALUES ('shared','platform',{version},'{name}',decode('{digest}','hex'));\n")
    for old,new,name,digest in sync.MOVES[:3 if oauth else 2]:
        chunks.append((sync.MIGRATIONS/"shared"/f"{new:04}_{name}.sql").read_text())
        chunks.append(f"INSERT INTO elitea_runtime.schema_migrations(target_kind,target_id,version,name,checksum,applied_at) VALUES ('shared','platform',{old},'{name}',decode('{digest}','hex'),'2026-09-08T12:00:00Z');\n")
    chunks.append("COMMIT;\n")
    return "\n".join(chunks)


SEED = """
INSERT INTO elitea_runtime.input_bundles(input_bundle_id,immutable_version,media_type,resource_project_id,manifest_digest,manifest_size,manifest_bytes,created_by)
VALUES ('receipt-bundle','1','application/x-protobuf',1,decode(repeat('ab',32),'hex'),1,decode('01','hex'),'fixture');
INSERT INTO elitea_runtime.input_bundle_entries(input_bundle_id,entry_id,entry_version,semantic_role,media_type,content_digest,content_size,content_reference,classification,required_grant_audience,content_bytes)
VALUES ('receipt-bundle','request','1','request','application/vnd.elitea.toolkit-execute-read-input.v1+protobuf',decode(repeat('ac',32),'hex'),1,'fixture-reference','confidential','runtime-worker',decode('02','hex'));
INSERT INTO elitea_runtime.execution_jobs(execution_id,generation,command_id,tenant_id,resource_project_id,projection_project_id,actor_id,principal_ref,capability_id,capability_version,input_bundle_id,request_digest,idempotency_scope,idempotency_key)
VALUES ('receipt-execution',1,'receipt-command','1',1,1,'7','fixture','toolkit.execute.read.v1','1','receipt-bundle',decode(repeat('ad',32),'hex'),'receipt-fixture','receipt-fixture-key');
INSERT INTO elitea_runtime.toolkit_execute_read_jobs(execution_id,generation,capability_id,input_bundle_id,request_entry_id)
VALUES ('receipt-execution',1,'toolkit.execute.read.v1','receipt-bundle','request');
INSERT INTO elitea_mcp.prebuilt_servers(catalogue_key,display_name,config_schema) VALUES ('receipt_fixture','Receipt fixture','{"properties":{"project":{"type":"string"}}}');
INSERT INTO p_1.tags(name,data) VALUES ('preserved-product-row','{"marker":"retained"}');
"""
OAUTH_SEED = """
INSERT INTO elitea_auth.mcp_oauth_clients(id,project_id,actor_id,client_id,token_endpoint,encrypted_credentials)
VALUES (repeat('x',43),1,7,'opaque-client','https://example.invalid/token',decode(repeat('be',29),'hex'));
"""


def create_fixture(container, oauth=True):
    database="elitea_cutover_receipts_"+uuid.uuid4().hex[:16]
    sync.validate_target(container,database)
    query(container,"postgres",f'''CREATE DATABASE "{database}"; COMMENT ON DATABASE "{database}" IS '{MARKER}';''')
    try:
        query(container,database,fixture_sql(oauth))
        query(container,database,SEED+(OAUTH_SEED if oauth else ""))
    except Exception:
        drop_fixture(container,database)
        raise
    return database


def drop_fixture(container,database):
    sync.validate_target(container,database)
    if not database.startswith("elitea_cutover_receipts_"):
        raise ValueError("not a test-owned database")
    query(container,"postgres",f'DROP DATABASE "{database}";')


def snapshot(container,database):
    # Keep only hashes and relation identities in the test process.
    return query(container,database,"""
CREATE TEMP TABLE proof_rows(relation text,oid oid,digest text);
DO $proof$ DECLARE r record; value text; BEGIN
 FOR r IN SELECT n.nspname,c.relname,c.oid FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE c.relkind='r' AND n.nspname!~'^pg_' AND n.nspname NOT IN ('information_schema','elitea_migration_audit')
 LOOP
 EXECUTE format($sql$SELECT md5(COALESCE(string_agg(md5(to_jsonb(x)::text),'' ORDER BY md5(to_jsonb(x)::text)),'')) FROM %I.%I x$sql$,r.nspname,r.relname) INTO value;
 INSERT INTO proof_rows VALUES(r.nspname||'.'||r.relname,r.oid,value);
 END LOOP;
END $proof$;
SELECT jsonb_object_agg(relation,jsonb_build_object('oid',oid,'digest',digest)) FROM proof_rows;
""")


class ReceiptUnitTests(unittest.TestCase):
    def test_guard_rejects_live_names_and_shell_fragments(self):
        for name in ["elitea","postgres","agentstate","elitea_cutover_","elitea_cutover_a';--"]:
            with self.assertRaises(ValueError):sync.validate_target("postgres-fixture",name)
        for name in ["-i","bad;command",""]:
            with self.assertRaises(ValueError):sync.validate_target(name,COPY)

    def test_exact_artifacts_and_rollback_default(self):
        sync.verified_artifacts()
        sql=sync.build_sql(COPY)
        self.assertTrue(sql.endswith("ROLLBACK;\n\\echo copy_rolled_back\n"))
        self.assertNotIn("TRUNCATE",sql)
        self.assertNotIn("DROP TABLE",sql)
        self.assertNotIn("CREATE SCHEMA",sql)
        self.assertNotIn("CREATE TABLE elitea_migration_audit",sql)
        self.assertNotIn("DELETE FROM",sql)
        self.assertEqual(sql.count("UPDATE elitea_runtime.schema_migrations"),1)
        self.assertLess(sql.index("unexpected_shared_ledger"),sql.index("UPDATE elitea_runtime.schema_migrations"))
        self.assertLess(sql.index("branch_catalog_mismatch"),sql.index("UPDATE elitea_runtime.schema_migrations"))
        self.assertIn(str(sync.lock_key()),sql)

    def test_explicit_commit_and_failure_order(self):
        sql=sync.build_sql(COPY,apply_copy=True,fail_before_commit=True)
        self.assertLess(sql.index("injected_precommit_failure"),sql.rindex("COMMIT;"))
        self.assertTrue(sql.endswith("COMMIT;\n\\echo copy_committed\n"))

    def test_sanitized_errors(self):
        value=subprocess.CompletedProcess([],1,"verified_old_ledger\nprivate-row\n","ERROR: P0001\nDETAIL: credential-value")
        with patch("rehearse_rust_main_sync.subprocess.run",return_value=value):
            result=sync.execute("fixture",COPY,"SELECT 1")
        self.assertNotIn("private-row",str(result));self.assertNotIn("credential",str(result))
        self.assertEqual(result["sqlstates"],["P0001"])


@unittest.skipUnless(CONTAINER,"requires local disposable-copy PostgreSQL opt-in")
class ReceiptDatabaseTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.directory=tempfile.TemporaryDirectory(prefix="elitea-receipt-proof-")
        cls.binary=str(Path(cls.directory.name)/"elitea-migrate")
        result=subprocess.run(["go","build","-o",cls.binary,"./cmd/elitea-migrate"],
           cwd=sync.ROOT/"services/elitea-main",capture_output=True,timeout=180)
        if result.returncode:raise RuntimeError("build unchanged migrator failed; diagnostics withheld")

    @classmethod
    def tearDownClass(cls):cls.directory.cleanup()

    def test_known_receipts_rollback_refusals_and_normal_migrator(self):
        for oauth in [False,True]:
            with self.subTest(optional_oauth=oauth):
                database=create_fixture(CONTAINER,oauth)
                try:self.prove(database,oauth)
                finally:drop_fixture(CONTAINER,database)

    def prove(self,database,oauth):
        before=snapshot(CONTAINER,database)
        receipt=Path(self.directory.name)/(database+"-dry.jsonl")
        dry=sync.run_copy(CONTAINER,database,receipt)
        records=[json.loads(line) for line in receipt.read_text().splitlines()]
        self.assertEqual(receipt.stat().st_mode & 0o777,0o600)
        self.assertEqual(records[0]["archive"]["database"],database)
        self.assertEqual(records[1]["outcome"],"rolled_back")
        self.assertEqual(records[0]["sha256"],records[1]["archive_sha256"])
        self.assertEqual(records[0]["sha256"],hashlib.sha256(json.dumps(records[0]["archive"],sort_keys=True,separators=(",", ":")).encode()).hexdigest())
        with self.assertRaises(FileExistsError):sync.run_copy(CONTAINER,database,receipt)
        self.assertEqual(dry["exit_code"],0,dry)
        self.assertEqual(dry["progress"][-1],"copy_rolled_back")
        self.assertEqual(snapshot(CONTAINER,database),before)
        failed=sync.run_copy(CONTAINER,database,Path(self.directory.name)/(database+"-failure.jsonl"),apply_copy=True,fail_before_commit=True)
        self.assertNotEqual(failed["exit_code"],0)
        self.assertEqual(snapshot(CONTAINER,database),before)
        # Each fault is inside the rejected transaction.
        for fault in [
          f"COMMENT ON DATABASE {database} IS NULL;",
          "UPDATE elitea_runtime.schema_migrations SET checksum=decode(repeat('aa',32),'hex') WHERE version=121;",
          "UPDATE elitea_runtime.schema_migrations SET checksum=decode(repeat('aa',32),'hex') WHERE version=122;",
          "INSERT INTO elitea_runtime.schema_migrations VALUES ('tenant','1',9999,'unknown',decode(repeat('aa',32),'hex'),clock_timestamp());",
          "UPDATE elitea_runtime.schema_migrations SET version=9999 WHERE version=123;",
          "INSERT INTO elitea_runtime.schema_migrations SELECT target_kind,target_id,125,name,checksum,applied_at FROM elitea_runtime.schema_migrations WHERE version=122;",
          "ALTER TABLE elitea_runtime.toolkit_execute_read_jobs DROP CONSTRAINT toolkit_execute_read_jobs_request_entry;",
          "CREATE TABLE public.unexpected_dependency(execution_id text,generation bigint,FOREIGN KEY(execution_id,generation) REFERENCES elitea_runtime.toolkit_execute_read_jobs(execution_id,generation));",
        ]:
            sql=sync.build_sql(database,apply_copy=True).replace("BEGIN; SET LOCAL","BEGIN; "+fault+" SET LOCAL",1)
            refusal=sync.execute(CONTAINER,database,sql)
            self.assertNotEqual(refusal["exit_code"],0,refusal)
            self.assertEqual(snapshot(CONTAINER,database),before)
        commit_receipt=Path(self.directory.name)/(database+"-commit.jsonl")
        applied=sync.run_copy(CONTAINER,database,commit_receipt,apply_copy=True)
        committed_records=[json.loads(line) for line in commit_receipt.read_text().splitlines()]
        self.assertEqual(committed_records[1]["outcome"],"committed")
        self.assertEqual(len(committed_records[0]["archive"]["ledger"]),len(sync.verified_artifacts()[0])+(3 if oauth else 2))
        self.assertEqual(query(CONTAINER,database,"SELECT count(*) FROM pg_namespace WHERE nspname='elitea_migration_audit'"),"0")
        self.assertEqual(applied["exit_code"],0,applied)
        after=json.loads(snapshot(CONTAINER,database));old=json.loads(before)
        for relation,value in old.items():
            if relation!="elitea_runtime.schema_migrations":self.assertEqual(after[relation],value,relation)
        repeat=sync.execute(CONTAINER,database,sync.build_sql(database,apply_copy=True))
        self.assertNotEqual(repeat["exit_code"],0)
        self.assertEqual(json.loads(snapshot(CONTAINER,database)),after)
        env=os.environ.copy()
        # This is the tracked local development endpoint, with no credential discovery.
        env["DATABASE_URL"]=f"postgres://elitea:elitea@127.0.0.1:15433/{database}?sslmode=disable"
        for attempt in [1,2]:
            result=subprocess.run([self.binary,"-all-tenants"],env=env,capture_output=True,timeout=180)
            self.assertEqual(result.returncode,0,"unchanged migrator rejected copy; diagnostics withheld")
            receipts=json.loads(query(CONTAINER,database,"SELECT jsonb_agg(jsonb_build_array(version,name,encode(checksum,'hex')) ORDER BY version) FROM elitea_runtime.schema_migrations WHERE target_kind='shared' AND target_id='platform'"))
            self.assertEqual(receipts,[list(row) for row in sync.artifacts("shared")])
            self.assertEqual(query(CONTAINER,database,"SELECT to_regclass('public.webhooks') IS NOT NULL AND to_regclass('public.webhook_deliveries') IS NOT NULL AND to_regclass('public.pipeline_runs') IS NOT NULL"),"t")
            current=json.loads(snapshot(CONTAINER,database))
            for relation,value in old.items():
                if relation!="elitea_runtime.schema_migrations":self.assertEqual(current[relation],value,relation)
            if attempt==1:first=current
            else:self.assertEqual(current,first)
        evidence_dir=os.getenv("ELITEA_RUST_RECEIPT_EVIDENCE_DIR")
        if evidence_dir:
            destination=Path(evidence_dir);destination.mkdir(mode=0o700,parents=True,exist_ok=True)
            for original in [receipt,commit_receipt]:
                fd=os.open(destination/original.name,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
                with os.fdopen(fd,"wb") as output:output.write(original.read_bytes())
        print(json.dumps({"database":database,"optional_oauth":oauth,"dry_run":dry,"applied":applied,
          "normal_migrator_runs":2,"shared_versions":[row[0] for row in sync.artifacts("shared")][-8:],
          "product_rows_and_relation_oids_preserved":True,"repeat_refused":True}))


if __name__=="__main__":unittest.main()
