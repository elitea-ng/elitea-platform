# Database action node design (Gate 5e, outline)

Status: outline only. No schemas, no runtime code. Capability-disabled.

| Item | Value |
| --- | --- |
| Date | 2026-10-08 |
| Wave | Wave 3 (implementation). This outline fixes the direction so Wave 2 HTTP work does not block it. |
| Gate | A separate Worker gate constant, `false` in production builds. Name it in Wave 3. |
| Sources | `experts/05-main-actions.md` D7, F9 and Q3; `PLAN.md` section 3 Wave 3 and section 6 answer 1. |
| Related | `http-action-node-design.md` (same Lookup, Begin, Commit and owner-proof pattern). |
| Unverified claims | Marked ASSUMPTION. |

Paths are relative to the repository root. `W/` is `services/elitea-worker-rust/`, `M/` is `services/elitea-main/`.

## 1. Summary

A `type: database` node runs one frozen SQL statement with typed parameters against a configured PostgreSQL or MySQL
database. The Worker executes the statement. Main owns the intent, the rule and the receipt. A write is allowed only
if the target database can prove later whether it committed.

## 2. Binding decisions

| # | Decision |
| --- | --- |
| 1 | Execution stays in the Worker (sqlx, existing SQL toolkit semantics). Main does not run Go database drivers. |
| 2 | `operation` names a rule: configuration, mode (`read` or `write`) and the frozen statement digest. |
| 3 | The statement text is frozen when the version is frozen. Parameters are typed and bound only with sqlx `.bind()`. |
| 4 | `read` runs inside `START TRANSACTION READ ONLY` on both engines. Main refuses DML text for a `read` rule. |
| 5 | A `write` rule requires the marker table in the target database. A write without it is refused in V1 (user answer to Q3, ASSUMPTION: confirmed with the PLAN section 6 answers; re-check at Wave 3 start). |
| 6 | Rules are authored by project admins with the new permission, inside the operator egress allowlist (PLAN section 6 answer 1). The permission name is the one proposed in the HTTP design (`models.http_action_rules.manage` is a proposal; a database variant may need its own name). |
| 7 | Unknown outcomes are reconciled with the backend session id plus the marker table. Without a marker the node cannot be a write. |

## 3. What exists today (verified)

| Area | Finding | Evidence |
| --- | --- | --- |
| Raw SQL | The SQL toolkit runs one statement as text. No bind parameters: it calls `statement.query().fetch/execute` with no `.bind`. | `W/src/toolkits/families/sql/client.rs:315,331,357,373`; tool description `W/src/toolkits/families/sql/tools.rs:168` |
| Lexer | `admit_one_statement` accepts exactly one statement, at most 64 KiB, and rejects transaction control. It does not count placeholders. | `W/src/toolkits/families/sql/lexer.rs:3,25-40` |
| Bounds | Connect 10 s, statement 30 s, 1,000 rows, 512 KiB output. PostgreSQL also sets `lock_timeout` 5 s. | `W/src/toolkits/families/sql/client.rs:18-27,223-226` |
| TLS | `VerifyFull` for PostgreSQL, `VerifyIdentity` for MySQL. | `client.rs:219,238` |
| TLS seam | SQLx 0.8.6 has no fully environment-free PostgreSQL options constructor. The code says production activation is gated on removing the driver's ambient TLS-file seam. | `client.rs:207-212` |
| Connections | One connection per operation, closed afterwards. No pool, no per-execution cap. | `client.rs` (`execute_sql`) |
| Outcome | After dispatch, any timeout, cancellation or decoding failure is an unknown outcome. The tool text already says reconcile before retry. | `tools.rs:168` |
| Main side | No database-effect table or owner exists. HTTP has `execution_http_effects` and an owner proof. | `M/migrations/shared/0145_*.sql`; `M/internal/infra/db/repos/http_action_recovery.go` |

## 4. YAML sketch

```yaml
- id: set_status
  type: database
  operation: crm.update_status       # rule: configuration, mode, frozen statement digest
  statement: "UPDATE tickets SET status = $1 WHERE id = $2"
  params:
    - {from: status, type: text}
    - {from: ticket_id, type: int8}
  output: [rows_affected]
  transition: next
```

- `statement` is frozen at version freeze. Its digest must equal the rule's digest. A changed statement needs a new rule revision.
- `params` are positional. Types: `text`, `int8`, `float8`, `bool`, `json`, `timestamptz` (ASSUMPTION: final list set in Wave 3).
- Rejected: string interpolation, multiple statements, explicit transactions in V1.

## 5. Parameter binding

- The lexer gains a dialect-aware placeholder counter (`$n` for PostgreSQL, `?` for MySQL). It must ignore placeholders
  inside strings, comments and dollar quotes, as it already does for statement splitting.
- Admission requires: one statement, placeholder count equals `len(params)`, every parameter typed, none interpolated.
- Values go through sqlx `.bind()` only. The statement text never contains a value.
- Bounds: at most 100 parameters, each at most 64 KiB.

## 6. Read and write authority

| Mode | Execution | Main admission |
| --- | --- | --- |
| `read` | `START TRANSACTION READ ONLY` on PostgreSQL and MySQL | Refuses statements that look like DML or DDL. The database also enforces read-only. |
| `write` | One transaction that runs the statement and the marker insert | Requires a write-enabled rule and a verified marker table. A sensitive write goes through the existing tool-guard interrupt. |

The lexer is a safety net, not the authority: authorization does not depend on statement classification (comment in
`lexer.rs:28-31`). The read-only transaction is the enforcement.

## 7. Receipts and reconciliation

Main owns `elitea_runtime.execution_db_effects` (new, same shape as the HTTP table: Lookup, Begin, Commit,
`dispatching`, `completed`, `failed`, `uncertain`). `effect_id = H(execution, generation, activation,
statement_digest, params_digest)`.

Write rules require the marker table in the target database:

```sql
INSERT INTO elitea_effects(effect_id) VALUES ($n)   -- in the same transaction as the statement
```

Unknown outcome (timeout, lost connection, cancellation after dispatch):

1. At Begin the Worker records the backend session id: `pg_backend_pid()` or `CONNECTION_ID()`.
2. Reconciliation waits until that session no longer appears in `pg_stat_activity` or `information_schema.PROCESSLIST`.
3. It then reads the marker row for `effect_id`.
4. Present: committed (result unknown, so `completed` without rows). Absent: `verified_no_effect`.

ASSUMPTION: the reconciliation role can read `pg_stat_activity` and `PROCESSLIST` for the application's own sessions
(PostgreSQL shows `pid` for other roles only with the right privilege). Check privileges in Wave 3.

Without a marker table the outcome stays `uncertain` and goes to 5b `effect_reconciliation_required`. In V1 a write rule
without a marker table is refused at rule-write time, so this case applies only to reads (reads are safe to re-run).

## 8. Bounds

| Bound | Value |
| --- | --- |
| Parameters | at most 100, each at most 64 KiB |
| Statement text | at most 64 KiB (existing) |
| Rows | at most 1,000 |
| Output | at most 512 KiB |
| Connections | at most 4 per execution (new) |
| Statement time | 30 s |
| Lock timeout | 5 s |

## 9. Authority and egress

- Rules are project-scoped, authored by project admins with the new permission. The Web editor lists rule names only.
- The database host must be inside the operator egress allowlist. The Worker, not Main, dials it, so the allowlist and
  the CGNAT and NAT64 blocks need a Worker-side equivalent (ASSUMPTION: the same class list as the Main guard; the
  Worker has no such guard for SQL today, verify in Wave 3).
- Credentials come from the configuration at claim time as today.

## 10. Blockers and order

1. Remove sqlx's ambient PostgreSQL TLS-file seam (`client.rs:207-212`). PostgreSQL activation is blocked until then.
   MySQL can be earlier.
2. HTTP effects repository and owner proof (Wave 2 T4 to T6) to copy the pattern.
3. Placeholder-aware lexer and typed bind path.
4. `execution_db_effects`, marker verification, session-id reconciliation.
5. Gate flip after the real-database acceptance below.

## 11. Acceptance (Wave 3)

- A placeholder-count mismatch, an interpolated value and a second statement are refused.
- A `read` rule cannot change data on either engine (a DML text inside a read rule fails in the database too).
- A write without a marker is refused at rule-write time.
- Kill the Worker between statement and commit: reconciliation reads the marker and reports exactly committed or
  `verified_no_effect`, never both, and no second write happens.
- 4 connections per execution is enforced under concurrency.
- Real PostgreSQL 18 and MySQL runs in CI.

## 12. Performance, durability, resilience and security

Outline-level; numbers marked *proposal* are confirmed in the Wave 3 design.

### Performance

| Budget | Limit |
|---|---|
| Statement timeout / lock timeout | 30 s / 5 s |
| Rows / output | ≤ 1,000 rows / ≤ 512 KiB projected output |
| Parameters | ≤ 100, each ≤ 64 KiB |
| Connections | ≤ 4 per execution |
| Main overhead per action (intent + receipt), excluding the statement | ≤ 15 ms p95 (*proposal*, same as HTTP) |

One intent write (Begin) and one receipt write (Commit) per action; no per-row writes.

| Requirement | Enforced by (code mechanism) | Proven by (test) |
| --- | --- | --- |
| Statement timeout 30 s | `STATEMENT_TIMEOUT` (`sql/client.rs:20`) wraps every execute | Cancellation `statement_timeout_marks_unknown` (`sql/client.rs` tests, real PG + MySQL, Wave 3): `pg_sleep(31)` → `statement_timeout`, outcome `uncertain` for a write |
| Lock timeout 5 s | PostgreSQL `SET lock_timeout = 5000` at session start (`client.rs:223-226`); MySQL equivalent `innodb_lock_wait_timeout=5` (Wave 3) | Real DB `lock_timeout_fires_at_5s`: second session holds the row lock, statement fails in 5 s ± 1 s on both engines |
| Rows ≤ 1,000, output ≤ 512 KiB | `MAX_ROWS` (`client.rs:23`), `MAX_OUTPUT_BYTES` (`client.rs:26`), checked while streaming rows, typed truncation error `ResourceExhausted` (`lexer.rs:9` enum precedent) | Boundary `rows_limit_boundary`, `output_bytes_boundary` (`client.rs` tests, existing/Wave 3): 1,000 rows and 512 KiB accepted; 1,001 rows / 512 KiB + 1 → `ResourceExhausted` |
| Parameters ≤ 100, each ≤ 64 KiB | `MAX_PARAMS = 100`, `MAX_PARAM_BYTES = 64 * 1_024` in `sql/params.rs` (Wave 3), checked in admission before connect or bind; typed `SqlLexError::ResourceExhausted` | Boundary `params_count_boundary` (100 ok, 101 refused), `param_bytes_boundary` (64 KiB ok, + 1 refused); both assert 0 connections opened (`sql/params.rs` tests, Wave 3) |
| Statement text ≤ 64 KiB | `MAX_SQL_BYTES` (`lexer.rs:3`) in `admit_one_statement` (`lexer.rs:25-40`) | Boundary `statement_bytes_boundary` (`lexer.rs` tests): 64 KiB ok, + 1 → `ResourceExhausted` |
| Connections ≤ 4 per execution | `MAX_CONNECTIONS_PER_EXECUTION = 4` as a per-execution `tokio::sync::Semaphore` in `sql/client.rs` (Wave 3), permit acquired before `connect`; waits are bounded by `CONNECT_TIMEOUT` (`client.rs:18`) | Concurrency `connections_per_execution_capped` (Wave 3): 16 concurrent actions, peak open connections observed on the server (`pg_stat_activity`/`PROCESSLIST`) = 4, never 5 |
| Main overhead ≤ 15 ms p95 (*proposal*) | Same single-transaction Begin and single Commit as HTTP (`Effects` port, Wave 3 `repos/db_effects.go`) | Budget `BenchmarkDBActionOverhead` (Wave 3, real PostgreSQL, stub target): 1,000 runs, p95 ≤ 15 ms |
| One Begin and one Commit per action; no per-row writes | `execution_db_effects` repo exposes Lookup, Begin, Commit only | Budget `TestDBActionWriteCount` (Wave 3): 1,000-row result → exactly 1 INSERT + 1 UPDATE on `execution_db_effects` |

### Durability

| Window | Durable state | Recovery rule |
|---|---|---|
| Crash before Begin | Nothing | Retry; Begin inserts the intent |
| Crash after Begin, before the statement | Intent `dispatching` with the session id | Marker absent after the session is gone → `verified_no_effect`; retry is allowed |
| Crash during or after a write, before Commit | Intent `dispatching` | Wait until the recorded backend pid / `CONNECTION_ID()` is gone, read the marker: present → committed without result (operator resumes through 5b), absent → `verified_no_effect` |
| Read action crash | Nothing external | Safe to retry: reads run in a read-only transaction |
| Write rule without a marker table | — | Refused in V1 (no unknown-outcome proof) |

| Requirement | Enforced by (code mechanism) | Proven by (test) |
| --- | --- | --- |
| Crash before Begin leaves nothing | Begin is the first durable step; no external call before it | Crash-window PG `crash_before_begin_no_row` (`repos/db_effects_postgres_integration_test.go`, Wave 3): process replacement, 0 rows, retry inserts intent |
| After Begin, before statement: marker absent → `verified_no_effect` | Backend session id recorded at Begin (`pg_backend_pid()`/`CONNECTION_ID()`); reconciler waits for the session to leave `pg_stat_activity`/`PROCESSLIST`, then reads marker `elitea_effects(effect_id)` — typed `Reconciled::{Committed, VerifiedNoEffect}` | Crash-window `kill_worker_before_statement_verified_no_effect` (real PG 18 and MySQL, Wave 3): Worker killed; marker absent; result exactly `verified_no_effect`; retry allowed |
| Crash during or after a write: marker present → committed, else `verified_no_effect`, never both, no second write | Statement and marker INSERT in one target transaction (§6); reconciler never runs while the session is alive; second dispatch is blocked by the `dispatching` row (`ErrDispatchInFlight`, Wave 3) | Crash-window `kill_worker_between_statement_and_commit` (real PG + MySQL): exactly one of `committed`/`verified_no_effect`; row count in target table = 1 or 0 as reported; 0 re-executions. `second_claim_does_not_redispatch` (PG) |
| Reconciliation role can read session list | Preflight `reconciliation_role_can_see_sessions` check at rule write; refusal `ErrReconciliationUnsupported` | Real DB `rule_write_refused_without_pg_stat_activity_privilege` (Wave 3). ASSUMPTION in §7 |
| Read crash is safe to retry | `read` runs only in `START TRANSACTION READ ONLY` (§6) | Real DB `read_rule_cannot_modify_data` (PG + MySQL): INSERT/UPDATE/DDL text in a read rule fails in the database, 0 rows changed |
| Write rule without marker table refused (V1) | Rule-write validator checks marker table exists with required shape; `ErrMarkerMissing` → `policy_denied` | Negative `write_rule_without_marker_refused` (`repos/db_rules_test.go`, Wave 3): rule not stored |

### Resilience

- Every bound above is enforced before execution (parameter count and size, single statement, placeholder count) or by
  the database (timeouts, read-only transaction).
- Typed failures: `policy_denied`, `invalid_input`, `statement_timeout`, `uncertain/reconciliation_required`.
- Cancellation cancels the statement; lease loss leaves the intent `dispatching` for reconciliation, never a blind
  retry.

| Requirement | Enforced by (code mechanism) | Proven by (test) |
| --- | --- | --- |
| Parameter count, size, single statement, placeholder count enforced before execution | Dialect-aware placeholder counter in `sql/lexer.rs` (Wave 3, extends `admit_one_statement`, `lexer.rs:25-40`); checks run before connect; typed `SqlLexError::{Invalid, ResourceExhausted, MultipleStatements}` (`lexer.rs:7-12`) | Table tests `placeholder_count_mismatch_refused`, `interpolated_value_refused`, `second_statement_refused`, `placeholders_in_strings_comments_dollar_quotes_ignored` (`lexer.rs` tests, Wave 3): each → typed error, 0 connections |
| Database enforces timeouts and read-only | `STATEMENT_TIMEOUT`, `lock_timeout`, `READ ONLY` transaction (`client.rs:20,223-226`) | See Performance rows and `read_rule_cannot_modify_data` |
| Typed failures `policy_denied`, `invalid_input`, `statement_timeout`, `uncertain/reconciliation_required` | Closed enum `DbActionFailure` in `W/src/agents/graph/db_action.rs` and Main `ErrDB*` set (Wave 3); no generic string errors | Mapping test `failure_codes_exhaustive` (Rust) and `TestDBFailureCodeMapping` (Go), Wave 3: each variant → exact wire code; unknown → internal, no detail |
| Cancellation cancels the statement | Cancel token select around execute; target-side `pg_cancel_backend`/`KILL QUERY` on the recorded session id (Wave 3) | Cancellation `cancel_kills_running_statement` (real DB): cancel at 1 s of `pg_sleep(30)`; session gone ≤ 2 s; outcome `uncertain` for a write |
| Lease loss leaves intent `dispatching`, never a blind retry | Commit fenced by `dispatch_claim_id` + lease epoch (`ErrFenced`, Wave 3) | Lease-loss PG `lease_lost_commit_refused_row_stays_dispatching`: second claim sees `dispatching`, goes to reconciliation, 0 re-dispatch |

### Security

- **Authority inside the effect transaction.** The intent row is written in the same Main transaction that rechecks
  the rule (enabled, mode, statement digest) and the original actor's live RBAC on the configuration. Fail closed.
- **Parameterized only.** The statement is frozen at version freeze and authored by a project admin under the new
  permission; parameters are typed and bound only through sqlx `.bind()`; no string-built SQL; nothing from model
  output ever becomes SQL text (the legacy raw-SQL tool is not ported).
- **Least privilege.** Reads run in `START TRANSACTION READ ONLY`, and Main refuses DML text in read rules; writes
  need a write rule and a marker table.
- **Egress and TLS.** Database hosts must be inside the operator egress allowlist; resolved addresses are checked
  before connecting; TLS `VerifyFull`/`VerifyIdentity` only. PostgreSQL activation waits until sqlx's ambient
  TLS-file seam is removed (§10).
- **Credentials by reference.** Connection credentials come from the configuration, resolved server-side; never in
  YAML, events or logs.
- **No data in logs.** Logs carry rule name, row count, byte count, duration and digests; never statement parameters,
  rows or connection strings.
- **Supply chain.** Reuses the Worker's existing `sqlx` dependency; no new crate.

| Requirement | Enforced by (code mechanism) | Proven by (test) |
| --- | --- | --- |
| Authority inside the effect transaction (rule enabled, mode, statement digest, original actor's live RBAC on the configuration) | `DBEffects.Begin` (Wave 3) opens one Begin transaction: `FOR SHARE` read of the rule, recheck `enabled`, `mode`, `statement_digest == frozen digest`, live RBAC, then INSERT intent; any failure → `ErrUnauthorized`/`policy_denied`, no row | Negative-authz `begin_foreign_actor_refused`, `begin_foreign_project_rule_refused`, `begin_disabled_rule_refused`, `begin_digest_mismatch_refused` (PG, `repos/db_effects_postgres_integration_test.go`, Wave 3): 0 intent rows, 0 target connections |
| Parameterized only; nothing from model output becomes SQL text | Statement frozen at version freeze, digest must equal the rule's (`ErrStatementDigestMismatch`); values only via sqlx `.bind()` in `sql/params.rs` (Wave 3); the raw-SQL tool is not ported | `injection_value_stays_data` (real DB): value `'; DROP TABLE t;--` stored literally, table intact; `statement_edit_without_new_rule_revision_refused`; grep-style test `no_bind_free_execute_on_action_path` (asserts the action path calls only the bound entry point) |
| Reads in `READ ONLY`; Main refuses DML text in read rules; writes need write rule + marker | `START TRANSACTION READ ONLY` on both engines; Main classifier (safety net only, `lexer.rs:28-31`); write rule requires marker (`ErrMarkerMissing`) | `read_rule_cannot_modify_data` (PG + MySQL); `read_rule_dml_text_refused_at_write`; `write_rule_without_marker_refused` |
| DB host inside operator egress allowlist; resolved addresses checked before connect | Worker-side `egress_guard` (`W/src/egress/guard.rs`, Wave 3, same class list as the Main guard incl. CGNAT `100.64.0.0/10` and NAT64), applied after DNS resolution and before `connect`; `EgressError::Blocked` | Guard tests `blocked_classes_table` (each class incl. IPv4-mapped), `dns_rebinding_to_blocked_ip_refused`, `host_outside_allowlist_refused` (Wave 3): 0 sockets opened. ASSUMPTION §9: no Worker guard exists today |
| TLS `VerifyFull`/`VerifyIdentity` only; PG waits for ambient TLS-file seam removal | `ssl-mode` fixed in `client.rs:219,238`; Wave 3 removes the seam (`client.rs:207-212`); PG gate stays `false` until then | `tls_mode_not_downgradable` (config rejects `disable`/`prefer`); `tls_hostname_mismatch_refused` (real DB); `pg_ambient_tls_env_ignored` (sets `PGSSLROOTCERT` to junk, connection unaffected) |
| Credentials by reference; never in YAML, events or logs | Credentials resolved at claim time from the configuration; connection string type `DbConn` implements no `Display`/`Debug` of secrets (redacting `Debug`) and is never logged | Log-capture `db_action_logs_and_events_no_sensitive` (Wave 3): sentinel password, parameter value, row value, connection string; captured logs, spans, events, receipts contain none |
| No data in logs: only rule name, row count, byte count, duration, digests | Safe-field-only type `DbActionLogFields{rule, rows, bytes, duration_ms, digest}` is the only logger argument; `Params`, `Rows`, `DbConn` do not implement the logging interface | Same log-capture test, including failure and `uncertain` paths |
| No new crate | `Cargo.toml` unchanged except existing `sqlx` | `cargo deny --all-features check advisories` on the PR: 0 new findings |

- **Scanners:** `govulncheck` (Main) and `cargo deny --all-features check advisories` (Worker) run on the implementing PR; they find vulnerable dependencies only, never defects in this design's code.

## 13. Rejected alternatives

- Main-executed Go drivers: duplicates dialect semantics and keeps customer database connections in Main.
- String interpolation, multiple statements, transactions in V1.
- Operator-only reconciliation for writes without a marker: the outcome would stay unknown.
- A generic effects table shared with HTTP: owner-specific ledgers stay behind the recovery proof interface.
