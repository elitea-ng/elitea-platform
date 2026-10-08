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

## 12. Rejected alternatives

- Main-executed Go drivers: duplicates dialect semantics and keeps customer database connections in Main.
- String interpolation, multiple statements, transactions in V1.
- Operator-only reconciliation for writes without a marker: the outcome would stay unknown.
- A generic effects table shared with HTTP: owner-specific ledgers stay behind the recovery proof interface.
