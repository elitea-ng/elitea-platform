#!/usr/bin/env python3
"""Move verified 122/123[/124] receipts on an isolated database copy only.

The default transaction rolls back. --apply-copy commits receipt changes and
a protected external receipt archive. Run the unchanged Go migrator separately after that commit.
No product table is recreated, truncated, restored, or updated.
"""
import argparse
import hashlib
import json
import os
import subprocess
from pathlib import Path
import re
import sys

from rehearse_rust_main_sync import (execute, lock_key, receipt_values,
                                    same_rows, validate_target)

ROOT = Path(__file__).resolve().parents[2]
MIGRATIONS = ROOT / "services/elitea-main/migrations"
CONTRACT = Path(__file__).with_name("rust_shared_receipts_20260909_catalog.json")
PREFIX_DIGEST = "fad7b561d47caa990d3eb0f280fb4bbceafbb51a6bf7de1122e3ca46f1c1c68c"
TENANT_DIGEST = "181066188992d213049f551c78fb04b1eb87fb9faa02f1bfcad847d1a7af3451"
CONTRACT_DIGEST = "b5fbc0c32de115cb106ec4ba577ebb934a25ec8e3f75fbcd1b5e84b8d638e16a"
MOVES = (
 (122,125,"toolkit_execute_read","c2497df59b29fec9230c0164e2e1e44d9c1f5d0d0bf08d7ae1dfd0aea4535e71"),
 (123,126,"mcp_prebuilt_parameter_schema","e0d3ebd4b3c7d03810a93d3e824d9a391ff2c870cbd79832c6cdca1028e86289"),
 (124,127,"mcp_oauth_clients","62bf7e4c22d2bba3375b216fd578aa1fdb57b1243dada093126d2076b8445515"),
)
BASE_TABLES = (
 "elitea_runtime.execution_jobs", "elitea_runtime.input_bundle_entries",
 "elitea_runtime.output_inbox", "elitea_runtime.toolkit_execute_read_jobs",
 "elitea_runtime.toolkit_execute_read_results", "elitea_mcp.prebuilt_servers",
)
OAUTH_TABLE = "elitea_auth.mcp_oauth_clients"
PROCEDURE = "main-sync-20260909"


def assert_sql(condition, message):
    # SQL NULL must refuse the operation, including a missing copy marker.
    return f"DO $proof$ BEGIN IF ({condition}) IS DISTINCT FROM TRUE THEN RAISE EXCEPTION '{message}'; END IF; END $proof$;\n"


def artifacts(scope):
    result = []
    for path in sorted((MIGRATIONS / scope).iterdir()):
        match = re.fullmatch(r"([0-9]{4})_([a-z][a-z0-9_]*)\.sql", path.name)
        if not match or not path.is_file():
            raise ValueError("invalid migration artifact")
        result.append((int(match[1]), match[2], hashlib.sha256(path.read_bytes()).hexdigest()))
    return result


def manifest_digest(rows):
    return hashlib.sha256(json.dumps(rows,separators=(",", ":")).encode()).hexdigest()


def verified_artifacts():
    shared, tenant = artifacts("shared"), artifacts("tenant")
    prefix = [row for row in shared if row[0] <= 121]
    if manifest_digest(prefix) != PREFIX_DIGEST or manifest_digest(tenant) != TENANT_DIGEST:
        raise ValueError("historical migration artifacts changed")
    by_version = {row[0]: row for row in shared}
    for old,new,name,digest in MOVES:
        if by_version.get(new) != (new,name,digest):
            raise ValueError("renamed migration artifact changed")
    return prefix, tenant


def catalog_query(tables):
    # Table identifiers come only from the fixed list above.
    values = ",".join("('" + name + "')" for name in tables)
    return f"""WITH relations(name) AS (VALUES {values})
SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'table',r.name,'kind',c.relkind,'rls',c.relrowsecurity,
 'columns',(SELECT COALESCE(jsonb_agg(jsonb_build_object(
  'name',a.attname,'type',format_type(a.atttypid,a.atttypmod),'not_null',a.attnotnull,
  'identity',a.attidentity,'generated',a.attgenerated,
  'default',pg_get_expr(d.adbin,d.adrelid)) ORDER BY a.attnum),'[]')
  FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
  WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped),
 'constraints',(SELECT COALESCE(jsonb_agg(jsonb_build_object(
  'name',k.conname,'definition',pg_get_constraintdef(k.oid),'validated',k.convalidated)
  ORDER BY k.conname),'[]') FROM pg_constraint k WHERE k.conrelid=c.oid),
 'indexes',(SELECT COALESCE(jsonb_agg(pg_get_indexdef(i.indexrelid) ORDER BY pg_get_indexdef(i.indexrelid)),'[]')
  FROM pg_index i WHERE i.indrelid=c.oid),
 'inbound',(SELECT COALESCE(jsonb_agg(jsonb_build_object(
  'relation',k.conrelid::regclass::text,'name',k.conname,'definition',pg_get_constraintdef(k.oid))
  ORDER BY k.conrelid::regclass::text,k.conname),'[]') FROM pg_constraint k WHERE k.confrelid=c.oid),
 'triggers',(SELECT count(*) FROM pg_trigger t WHERE t.tgrelid=c.oid AND NOT t.tgisinternal)
) ORDER BY r.name),'[]') FROM relations r JOIN pg_class c ON c.oid=to_regclass(r.name)"""


def build_sql(database, *, apply_copy=False, fail_before_commit=False, inspect_only=False, expected_archive=None):
    validate_target("validated",database)
    prefix,tenant = verified_artifacts()
    raw = CONTRACT.read_bytes()
    if hashlib.sha256(raw).hexdigest() != CONTRACT_DIGEST:
        raise ValueError("catalog proof artifact changed")
    contract = json.loads(raw)
    chunks = ["\\set ON_ERROR_STOP on\n\\set VERBOSITY sqlstate\n",
      "BEGIN; SET LOCAL search_path=pg_catalog,public; SET LOCAL lock_timeout='5s';\n",
      "SET LOCAL statement_timeout='120s'; SET LOCAL idle_in_transaction_session_timeout='30s';\n",
      "SET LOCAL client_min_messages=error;\n",
      f"SELECT pg_advisory_xact_lock({lock_key()});\n",
      assert_sql(f"current_database()='{database}'","unexpected_database"),
      assert_sql("(SELECT shobj_description(oid,'pg_database') FROM pg_database WHERE datname=current_database())='elitea_receipt_reconciliation_disposable_copy_20260909'","copy_marker_required"),
      assert_sql("NOT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND backend_type='client backend')","copy_has_other_clients"),
      "LOCK TABLE elitea_runtime.schema_migrations IN ACCESS EXCLUSIVE MODE;\n",
      assert_sql("NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='elitea_runtime.schema_migrations'::regclass AND NOT tgisinternal) AND NOT EXISTS(SELECT 1 FROM pg_class WHERE oid='elitea_runtime.schema_migrations'::regclass AND (relkind<>'r' OR relrowsecurity))","unsupported_ledger_relation"),
      "CREATE TEMP TABLE expected_shared(version bigint,name text,checksum bytea) ON COMMIT DROP;\n",
      "INSERT INTO expected_shared VALUES " + receipt_values(prefix+[(o,n,d) for o,_,n,d in MOVES[:2]]) + ";\n",
      "INSERT INTO expected_shared SELECT 124,'mcp_oauth_clients',decode('"+MOVES[2][3]+"','hex') WHERE EXISTS (SELECT 1 FROM elitea_runtime.schema_migrations WHERE target_kind='shared' AND target_id='platform' AND version=124);\n",
      assert_sql(same_rows("expected_shared","(SELECT version,name,checksum FROM elitea_runtime.schema_migrations WHERE target_kind='shared' AND target_id='platform') actual"),"unexpected_shared_ledger"),
      "CREATE TEMP TABLE expected_tenant(version bigint,name text,checksum bytea) ON COMMIT DROP;\n",
      "INSERT INTO expected_tenant VALUES "+receipt_values(tenant)+";\n",
      assert_sql("NOT EXISTS (SELECT 1 FROM elitea_runtime.schema_migrations r WHERE NOT (r.target_kind='shared' AND r.target_id='platform') AND NOT (r.target_kind='tenant' AND r.target_id ~ '^[1-9][0-9]{0,9}$' AND EXISTS (SELECT 1 FROM expected_tenant e WHERE (e.version,e.name,e.checksum)=(r.version,r.name,r.checksum)) AND EXISTS (SELECT 1 FROM centry.project p WHERE p.id::text=r.target_id) AND to_regnamespace('p_'||r.target_id) IS NOT NULL))","unexpected_nonshared_ledger"),
      "\\echo verified_old_ledger\n",
      "CREATE TEMP TABLE before_receipts ON COMMIT DROP AS TABLE elitea_runtime.schema_migrations;\n",
    ]
    tables_sql = ",".join("('"+table+"')" for table in BASE_TABLES)
    chunks += ["CREATE TEMP TABLE affected_relations(name text PRIMARY KEY,oid oid) ON COMMIT DROP;\n",
      f"INSERT INTO affected_relations SELECT column1,to_regclass(column1) FROM (VALUES {tables_sql}) v;\n",
      f"INSERT INTO affected_relations SELECT '{OAUTH_TABLE}',to_regclass('{OAUTH_TABLE}') WHERE EXISTS(SELECT 1 FROM expected_shared WHERE version=124);\n",
      assert_sql("NOT EXISTS(SELECT 1 FROM affected_relations WHERE oid IS NULL)","missing_branch_relation"),
      assert_sql(f"EXISTS(SELECT 1 FROM expected_shared WHERE version=124) OR to_regclass('{OAUTH_TABLE}') IS NULL","unrecorded_oauth_objects"),
      assert_sql("(SELECT sum(pg_total_relation_size(oid)) FROM affected_relations)<=134217728","copy_exceeds_rehearsal_bound"),
      "DO $proof$ DECLARE r record; BEGIN FOR r IN SELECT * FROM affected_relations ORDER BY name LOOP EXECUTE 'LOCK TABLE '||r.oid::regclass||' IN ACCESS EXCLUSIVE MODE'; END LOOP; END $proof$;\n",
    ]
    for key,tables in [("base",BASE_TABLES),("oauth",(OAUTH_TABLE,))]:
        expected = json.dumps(contract[key],separators=(",", ":")).replace("'","''")
        condition = f"({catalog_query(tables)})='{expected}'::jsonb"
        if key == "base":
            # pg_dump/pg_restore flattens the first AND group of this CHECK.
            # Accept only this exact equivalent catalog; archive the raw form.
            restored = json.loads(json.dumps(contract[key]))
            for relation in restored:
                if relation["table"] == "elitea_runtime.toolkit_execute_read_results":
                    for constraint in relation["constraints"]:
                        if constraint["name"] == "toolkit_execute_read_results_identity":
                            constraint["definition"] = constraint["definition"].replace(
                                "CHECK ((((octet_length(toolkit_type)", "CHECK (((octet_length(toolkit_type)", 1
                            ).replace("(octet_length(toolkit_type) <= 256))", "(octet_length(toolkit_type) <= 256)", 1)
            restored_json = json.dumps(restored,separators=(",", ":")).replace("'","''")
            condition = f"(({condition}) OR ({catalog_query(tables)})='{restored_json}'::jsonb)"
        if key == "oauth":
            condition = "NOT EXISTS(SELECT 1 FROM expected_shared WHERE version=124) OR ("+condition+")"
        chunks.append(assert_sql(condition,"branch_catalog_mismatch_"+key))
    chunks += ["\\echo verified_branch_objects\n"]
    archive_query = "jsonb_build_object('procedure', '"+PROCEDURE+"', 'database',current_database(), 'database_oid',(SELECT oid FROM pg_database WHERE datname=current_database()), 'cluster_system_identifier',(SELECT system_identifier::text FROM pg_control_system()), 'server_version',current_setting('server_version_num'), 'captured_at',clock_timestamp(), 'catalog_sha256','"+CONTRACT_DIGEST+"', 'prefix_sha256','"+PREFIX_DIGEST+"', 'ledger',(SELECT jsonb_agg(to_jsonb(r) ORDER BY target_kind,target_id,version) FROM before_receipts r), 'catalog',jsonb_build_object('base',("+catalog_query(BASE_TABLES)+"),'oauth',("+catalog_query((OAUTH_TABLE,))+")), 'relation_oids',(SELECT jsonb_object_agg(name,oid) FROM affected_relations))"
    chunks.append("SELECT jsonb_build_object('receipt_archive',"+archive_query+");\n")
    if inspect_only:
        return "".join(chunks)+"ROLLBACK;\n\\echo copy_inspected\n"
    if expected_archive is not None:
        for field,expression in {
            "database":"to_jsonb(current_database())",
            "database_oid":"(SELECT to_jsonb(oid) FROM pg_database WHERE datname=current_database())",
            "cluster_system_identifier":"(SELECT to_jsonb(system_identifier::text) FROM pg_control_system())",
            "ledger":"(SELECT jsonb_agg(to_jsonb(r) ORDER BY target_kind,target_id,version) FROM before_receipts r)",
            "relation_oids":"(SELECT jsonb_object_agg(name,oid) FROM affected_relations)",
        }.items():
            expected=json.dumps(expected_archive[field],separators=(",", ":")).replace("'","''")
            chunks.append(assert_sql(f"({expression})='{expected}'::jsonb","archive_snapshot_changed_"+field))
    chunks += ["\\echo captured_original_receipts\n",

      "CREATE TEMP TABLE before_rows(relation text,payload jsonb) ON COMMIT DROP;\n",
      "DO $proof$ DECLARE r record; BEGIN FOR r IN SELECT * FROM affected_relations LOOP EXECUTE format('INSERT INTO before_rows SELECT %L,to_jsonb(x) FROM %s x',r.name,r.oid::regclass); END LOOP; END $proof$;\n",
      "UPDATE elitea_runtime.schema_migrations SET version=version+3 WHERE target_kind='shared' AND target_id='platform' AND version BETWEEN 122 AND 124;\n",
      assert_sql(same_rows("elitea_runtime.schema_migrations r","(SELECT target_kind,target_id,CASE WHEN target_kind='shared' AND target_id='platform' AND version BETWEEN 122 AND 124 THEN version+3 ELSE version END AS version,name,checksum,applied_at FROM before_receipts) r","to_jsonb(r)"),"receipt_metadata_changed"),
      assert_sql("NOT EXISTS(SELECT 1 FROM affected_relations WHERE to_regclass(name) IS DISTINCT FROM oid)","relation_recreated"),
      "CREATE TEMP TABLE after_rows(relation text,payload jsonb) ON COMMIT DROP;\n",
      "DO $proof$ DECLARE r record; BEGIN FOR r IN SELECT * FROM affected_relations LOOP EXECUTE format('INSERT INTO after_rows SELECT %L,to_jsonb(x) FROM %s x',r.name,r.oid::regclass); END LOOP; END $proof$;\n",
      assert_sql(same_rows("before_rows","after_rows"),"product_rows_changed"),
      "SET CONSTRAINTS ALL IMMEDIATE;\n\\echo verified_receipts_and_rows\n",
    ]
    if fail_before_commit:
        chunks.append(assert_sql("false","injected_precommit_failure"))
    chunks += ["COMMIT;\n\\echo copy_committed\n" if apply_copy else "ROLLBACK;\n\\echo copy_rolled_back\n"]
    return "".join(chunks)


def inspect_copy(container, database):
    """Return verified receipt metadata. Never return SQL diagnostics or product rows."""
    validate_target(container,database)
    command=["docker","exec","-i",container,"sh","-c",
             'exec psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$1"',"sh",database]
    result=subprocess.run(command,input=build_sql(database,inspect_only=True),capture_output=True,text=True,timeout=180)
    if result.returncode:
        raise ValueError("copy preflight failed; diagnostics withheld")
    archives=[]
    for line in result.stdout.splitlines():
        if line.startswith('{"receipt_archive"'):
            archive=json.loads(line).get("receipt_archive")
            if isinstance(archive,dict) and archive.get("procedure")==PROCEDURE and archive.get("database")==database:
                archives.append(archive)
    if len(archives)!=1 or "copy_inspected" not in result.stdout.splitlines():
        raise ValueError("copy preflight output is incomplete")
    return archives[0]


def write_record(fd,record):
    payload=(json.dumps(record,separators=(",", ":"))+"\n").encode()
    remaining=memoryview(payload)
    while remaining:
        count=os.write(fd,remaining)
        if count<=0:raise OSError("receipt write failed")
        remaining=remaining[count:]
    os.fsync(fd)


def run_copy(container,database,receipt_file,*,apply_copy=False,fail_before_commit=False):
    """Fsync an exclusive 0600 archive before the version-only transaction."""
    archive=inspect_copy(container,database)
    payload=json.dumps(archive,sort_keys=True,separators=(",", ":")).encode()
    digest=hashlib.sha256(payload).hexdigest()
    fd=os.open(receipt_file,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    try:
        write_record(fd,{"record":"archive","sha256":digest,"archive":archive})
        directory=os.open(str(Path(receipt_file).parent),os.O_RDONLY)
        try:os.fsync(directory)
        finally:os.close(directory)
        try:
            result=execute(container,database,build_sql(database,apply_copy=apply_copy,
                fail_before_commit=fail_before_commit,expected_archive=archive))
        except Exception:
            write_record(fd,{"record":"outcome","archive_sha256":digest,"database":database,"outcome":"unknown"})
            raise
        expected_marker="copy_committed" if apply_copy else "copy_rolled_back"
        if result["exit_code"]==0 and expected_marker in result["progress"]:
            outcome="committed" if apply_copy else "rolled_back"
        elif result["exit_code"]!=0 and result["sqlstates"]:
            outcome="rolled_back"
        else:outcome="unknown"
        write_record(fd,{"record":"outcome","archive_sha256":digest,"database":database,
                         "outcome":outcome,"result":result})
    finally:
        os.close(fd)
    return result


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--container",required=True)
    parser.add_argument("--database",required=True)
    parser.add_argument("--receipt-file",required=True,help="new external JSONL archive path; existing files are refused")
    parser.add_argument("--apply-copy",action="store_true")
    parser.add_argument("--fail-before-commit",action="store_true")
    args=parser.parse_args()
    try:
        validate_target(args.container,args.database)
        result=run_copy(args.container,args.database,args.receipt_file,apply_copy=args.apply_copy,fail_before_commit=args.fail_before_commit)
    except Exception as error:
        print(json.dumps({"error":type(error).__name__,"details_withheld":True}))
        return 1
    print(json.dumps(result))
    return int(result["exit_code"]!=0)


if __name__=="__main__":
    sys.exit(main())
