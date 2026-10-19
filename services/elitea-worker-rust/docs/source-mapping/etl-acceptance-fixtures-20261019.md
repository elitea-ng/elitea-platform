# ETL acceptance fixtures (FU-085, Point 5 remaining item 2.1)

These are the fixtures for the closing Point 5 acceptance: a Databricks-style medallion ETL job. It is a UI
demonstration that every Point 5 node family works, and it commits to PostgreSQL and MySQL.
The change is test-only:
- a seeded data generator with an exact manifest;
- TLS-only `warehouse-pg` and `warehouse-mysql` with the harness's own test CA;
- an FX reference and webhook mock with a durable request journal;
- the pipeline YAML drafts;
- a guard test that keeps all of it out of shipped artifacts.

No product code changes. Branch `test/etl-acceptance-fixtures` from `origin/main` `ddc92a3cf`.

User requirements (PLAN.md §8.7, 2026-10-19):
- the input is a big CSV artifact of at least 20,000 rows and 10–15 mixed-type columns, with nulls and dirty values;
- the generator is deterministic and supports `--rows`/`--seed`;
- the pipelines transform the data, commit to both databases, save artifacts, and use every new node type;
- it is built and run in the UI.

## 1. Business behaviour taken, and what was not ported

- **Taken from the acceptance definition** (ACCEPTANCE-ETL.md §1–§5):
  - the medallion shape: bronze landing, silver cleansing with expectations, gold loads in both dialects,
    reconcile, quality gate with HITL approval, publish with a report and a webhook;
  - the oracles: exact fact and gold numbers, every reject is a planted defect, one webhook delivery per run, at
    most one served FX GET per batch.
- **No current-platform reference exists for this flow.**
  - The legacy SDK's SQL tool runs raw `text(sql)` with no binds or TLS and always commits. It has no HTTP node and
    no Map or Parallel.
  - None of that is ported. The drafts use frozen statements with typed parameters, effect-marker tables, and
    TLS-verified connections only.
- **Deliberate differences from the ACCEPTANCE-ETL §2 sketch.** Each is recorded in
  `scripts/acceptance/etl/README.md` "Pipelines" and in ACCEPTANCE-ETL.md §2 and §8.
  - Row batches of 400 replace 5k-row byte-range partitions (§5 below).
  - `enrich_fx` runs before `normalize`, because the net amount needs the rate.
  - Typed reducers live in the root, because nested children refuse them (`compiler.rs:1405-1408`, decision X3).
  - Artifact writes go through artifact toolkit nodes, because Code has no artifact operations on Main yet.
  - A reconcile drift goes to the approval HITL (§2.4 wins over §2.3).

## 2. Changed paths

All new; no existing file is modified.

| Path | Purpose |
|---|---|
| `scripts/acceptance/etl/gen_orders.py` | Generator, stdlib only. Limits `:41-53`. `net_cents` `:121` (exact half-up on integer cents). Defect planting `:175` (one expectation per bad row, within- and cross-batch duplicates, malformed line inside a batch). Batch split and character cap check `:251-270`. Manifest `:287`. Generate `:413` |
| `scripts/acceptance/etl/fx_rates.json` | One FX table for the oracle and the mock |
| `scripts/acceptance/etl/etl_mock.py` | FX + webhook mock. Bounds `:39-45`. The journal appends and fsyncs before the response `:121-136`; it rebuilds dedupe on start `:103`. Constant-time bearer check `:195-201`. TLS handshake in the handler thread `:210`. Dedupe decision and append are one locked step `:337`. Admin is loopback-only `:442`. TLS is mandatory `:451`. Oracle `check` `:489` |
| `scripts/acceptance/etl/etlctl.py` | `init` (generated credentials, env file `0600`, `O_EXCL`) `:82-102`. `material` (EC P-256 test CA, server leaves) `:110`. `verify` `:180-258`. Evidence redaction `:260-272`. `sqlcheck` (statement extraction `:297`, psql variables `:350`, base64 literals `:369`) |
| `scripts/acceptance/etl/compose.yaml` | Three services. TLS-only flags, digest-pinned images, no ports, memory limits; the mock is `read_only` with `cap_drop: ALL` |
| `scripts/acceptance/etl/warehouse/**` | TLS entrypoints, `pg_hba.conf` (hostssl scram, hostnossl reject), roles and users (`REQUIRE SSL`), schemas for both dialects with an effect-marker table |
| `scripts/acceptance/etl/pipelines/*.yaml` | Seven drafts: `etl_job`, `bronze_ingest`, `silver_clean`, `silver_batch`, `gold_build`, `load_postgres`, `load_mysql` |
| `scripts/acceptance/etl/tests/*.py` | Generator, mock and draft-lint tests |
| `scripts/acceptance/etl/README.md` | How to run; artifact limits; admin set-up; coverage table |
| `scripts/security/check_etl_fixture_isolation.py` | Guard. `REFERENCE` `:22`. Shipped roots `:29`. Fixture compose and `pg_hba.conf` invariants `:66` |
| `scripts/security/test_etl_fixture_isolation.py` | Guard tests |

The guard and its tests sit under `scripts/security/`, which `no-binaries.yml` already runs on every pull request
(`check_*.py` loop, and pytest over `test_*.py`). No CI workflow was changed.

## 3. Tests

| Suite | Count | Result | Skips |
|---|---|---|---|
| `scripts/acceptance/etl/tests/test_gen_orders.py` | 10 | pass, 2.5 s | 0 |
| `scripts/acceptance/etl/tests/test_etl_mock.py` | 21 | pass, 15 s (real TLS server, throwaway CA) | 0; the class skips only when `openssl` is absent |
| `scripts/acceptance/etl/tests/test_pipeline_fixtures.py` | 6 (with subtests) | pass | 0 |
| `scripts/security/test_etl_fixture_isolation.py` | 17 (one runs the 37 fixture tests above, so CI enforces them) | pass | 0 |
| CI glob `pytest -q scripts/security/test_*.py` | 91 + 24 subtests | pass, 21 s | 0 |
| `python3 scripts/security/check_*.py` (all) | 7 | pass | 0 |

Key tests:
- **Generator.** The manifest is re-derived from the written CSV bytes by an independent implementation: csv,
  regex, `strptime`, `Decimal.quantize`. That covers rejects per expectation, cleaning counts, per-batch counts and
  ppm, fact sums, the three gold tables, tag counts and the expected-rejects file. The check runs over four
  datasets.
  - No planted row fails two expectations.
  - The joined batches are byte-identical to the big file, and every batch is under 190,000 characters.
  - The limit file has 64 batches; limit+1 has 65 and is marked refused.
  - Same seed and rows give the same bytes; another seed gives different bytes.
  - Out-of-range rows or `bad_ppm` raise a typed error, and the CLI exits 2.
- **Mock.**
  - Transport: plain HTTP is closed at the handshake and counted; a client with the system CA bundle is refused.
  - Authentication: FX needs the token.
  - Webhook: 201 for the first delivery, 200 for a duplicate, 409 for a conflict. Bounds: 422, 413, and an
    at-limit body is accepted.
  - Oracle: a second key for the same run, or two served GETs for one batch, is a violation.
  - Faults: 503 ×2 then 200 counts one served GET; `accept_then_drop` followed by a retry deduplicates.
  - Durability: a restart keeps the dedupe state. A torn final line is dropped on restart, while corruption before
    the last line is refused. A full journal answers 507 and drops nothing. Rotating twice keeps both archives.
  - A delayed serve is reported apart as `served_delayed`, so a client's timeout-retry is not a violation.
  - The journal never contains tokens, `Bearer` or bodies.
  - Strict fault-rule validation; TLS and a loopback admin listener are mandatory.
- **Drafts.**
  - Every reference resolves.
  - Every Map, Parallel, HTTP or DB node, and every node with `settings:`, names its unblocking item. Item ids come
    from an allow-list.
  - No URL, host, password, secret or bearer text appears in the YAML.
  - The coverage table holds (43 features).
  - DB nodes have 1–16 frozen statements with typed parameters, no templating, and exactly one statement in read
    mode.
- **Guard.**
  - The real repository passes.
  - Each of these is caught: a warehouse in the default compose file, the mock in a Helm template, a Containerfile
    copying `scripts/acceptance`, published ports, an unpinned image, a literal password, `ssl=off`, a plain
    `host` rule in `pg_hba.conf`, a missing `require_secure_transport`, a MySQL user without `REQUIRE SSL`, a mock
    without TLS, a list-form literal password, `ssl=off` after `ssl=on`, and a TLS flag present only in a comment.

## 4. Live bring-up (once, on its own; 2026-10-19)

The stack ran as compose project `elitea-etl-fixtures`, alone, outside the platform stack. Images:
- `postgres:18.4@sha256:a02db8cac496f15b094798a38254f14d6e00741f709360e5e00bb6668ea31636`;
- `mysql:8.4.11@sha256:24989179f58856db763f418010c1169fecc944e3ec1ea7e921572d83eb070f70` (pulled once);
- `python:3.12.14-slim-trixie@sha256:f77ac9e44ae96ef2c90b8053ea08c31f8be030f824196b0ae4db6d462c84e51f`.

No image was built. Free disk before the bring-up was 36.6 GiB.

**`etlctl.py verify`** passed (`ok: true`, 5 s). Evidence:
`handoffs/point5-wave1-20261008/etl-fixtures-evidence/verify-20261019T154742Z.json`.
- **PostgreSQL.** `sslmode=disable` is rejected: "pg_hba.conf rejects connection … no encryption". `verify-full`
  against the system CA bundle is refused. `verify-full` with the test CA gives `ssl = t`, `TLSv1.3`. The schema
  has 10 tables. The reader role is refused on insert.
- **MySQL.**
  - A plaintext root login with `--get-server-public-key` (so the server, not the client, decides) gets
    `ERROR 3159`, "Connections using insecure transport are prohibited while --require_secure_transport=ON".
  - The `REQUIRE SSL` reader over plaintext is refused.
  - `VERIFY_IDENTITY` gives `Ssl_version TLSv1.3`. Server settings: `require_secure_transport=1`, `TLSv1.2,TLSv1.3`.
  - Users `etl_loader|ANY` and `etl_reader|ANY`.
  - 10 tables; the reader is refused on insert (1142).
- **Mocks.** Plain HTTP is "refused (RemoteDisconnected)". FX without the token gets 401; with it, 200 over TLSv1.3
  with eight currencies. Webhook 201 → 200 → 409. Journal for the probe run: FX requests 2 / served 1, webhook
  accepted 1 / duplicate 1 / conflict 1. The `check` oracle passes. `tls_handshake_failures` ≥ 1.

**`etlctl.py sqlcheck`** runs the drafts' own DB statements, extracted from `load_postgres.yaml`,
`load_mysql.yaml` and `gold_build.yaml`. They run as the loader over verified TLS, and each load is executed twice
under one run_id.

| Dataset | Batches | Fact rows / qty / net USD (both dialects = manifest) | Gold rows (PostgreSQL = MySQL = manifest) | Largest stage parameter | Wall time | Evidence |
|---|---|---|---|---|---|---|
| 1,000 rows | 3 | 988 / 5,451 / 1,253,957.30 | 108 | — | 3.2 s | `sqlcheck-20261019T154749Z.json` |
| 24,000 rows (default) | 60 | 23,712 / 131,323 / 30,186,896.85 | 108 | 165,387 B | 13 min 10 s | `sqlcheck-20261019T160115Z.json` |

The 24,000-row wall time is dominated by `docker exec` latency on the shared,
busy Docker host (about 94 running containers; single `docker compose exec` calls took 10–70 s). It is not
database time. The second load of the same run changed no counts: staging is keyed by
`(run_id, batch_no, order_seq)`, and fact and gold are upserted or rebuilt per run.

The stack was torn down with `etlctl.py down` after the evidence was recorded.

After the review fixes (§ Reviews), I ran one more clean cycle under the shared build-slot queue: `slot.sh acquire
docker etl 6`, then `up`, `verify`, `sqlcheck` (1,000 rows) and `down`, then release. All of it passed, and the
healthchecks now wait for the real TCP listeners. Evidence: `verify-20261019T163758Z.json` and
`sqlcheck-20261019T163802Z.json`.

## 5. Artifact limits and the design choice

These limits were measured on `main` `ddc92a3cf`, and they decide the shape of the drafts:
- **Artifact toolkit text read.** A whole file above 200,000 characters is refused, even with `start_line`/
  `end_line` (`services/elitea-main/internal/infra/storage/runtime_artifact_object.go:65,669-675`).
  `read_multiple_files` stops at 200,000 characters in total.
- **Artifact toolkit write.** 65,536 characters per call (`:79`).
- **Code nodes.**
  - Main answers every `artifact_*` platform operation with `unsupported_operation`
    (`services/elitea-main/internal/application/codeplatform/native_operations.go:51,91-93`;
    `services/elitea-main/internal/infra/storage/code_platform_native.go:129-140,236`).
  - Objects are capped at 8 MiB (`services/elitea-main/internal/domain/codeplatform/frame.go:23`).
  - The production profile grants a Code job 32 platform calls and 1 MiB
    (`handoffs/real-model-stack/production-profile*/lib/profile.py`).
- **Office host.** 50 MiB binary, Office formats only
  (`libs/rust/agent-runtime/src/toolkits/families/office_host.rs:29`).
- **Fan-out and state.**
  - Map: ≤ 64 items, ≤ 8 concurrent (`services/elitea-worker-rust/src/agents/graph/map_reduce.rs:26,28`).
  - Split Out / Aggregate output: ≤ 32,768 values and ≤ 512 KiB.
  - The whole-state checkpoint: 65,536 values.

**Choice: Split Out over row batches.**
- The big CSV (about 2.6 MB) is uploaded once for the demo, together with its deterministic batch split: 400 rows and
  ≤ 190,000 characters per batch, byte-identical to the big file once joined.
- `bronze_ingest` lists the batch folder and splits the plan into ≤ 64 items.
- Each Map child reads one batch with one text read and writes one silver CSV of under 64 KiB.
- The loaders read three silver files per call.
- A batch result carries its order ids as one compact string, so 64 results stay far under the state value limit.

The declared row limit is 64 × 400 = 25,600 rows; limit+1 is a typed refusal in `plan_batches`.

## Performance

| Budget | Declared | Measured |
|---|---|---|
| Generator, acceptance set (5 datasets, about 22 MB) | ≤ 5 s, ≤ 200 MB RSS | 1.22 s, 99 MB max RSS |
| One silver batch | ≤ 190,000 characters read, < 65,536 written | largest batch 44,556 characters |
| Stage call per loader visit (3 batches) | ≤ 256 KiB (Code result cap) | 165,387 B |
| Fixture services memory | limits 512 MiB / 1 GiB / 128 MiB | 47 MiB / 445 MiB / 38 MiB idle after the run |
| Mock concurrency | `MAX_INFLIGHT` 32, then 503 + `Retry-After` | bound in code (`etl_mock.py:41`); not load-tested |

The platform budgets of ACCEPTANCE §3 (per-child p99 ≤ 30 ms, ≤ 2 parent rows per activation) belong to the final
acceptance run (2.4), not to these fixtures.

## Durability

- **Mock journal.** Each entry is written and fsynced before the response (`etl_mock.py:121-136`), so an effect
  whose response is lost still counts once. The webhook dedupe index is rebuilt from the journal on start (`:103`).
  - Tests: `test_restart_keeps_the_dedupe_state` and `test_accept_then_drop_is_an_effect_and_the_retry_deduplicates`.
  - The journal lives on a named volume, so it survives a container restart.
- **Warehouses.**
  - Every draft write is keyed for idempotency per run: staging `(run_id, batch_no, order_seq)`, fact
    `(run_id, order_id)`, gold rebuilt per run_id, the bronze marker per listing digest.
  - Each warehouse schema has the DB node's effect-marker table (`elitea_effects`, T-5E §3.3).
  - Proven: `sqlcheck` loads twice with unchanged counts.
- **Fixtures across a pause.** Evidence lives under `handoffs/` (00-COMMON "Scratchpad is not durable"). The
  credentials live in the stack directory `~/.elitea-etl-acceptance` (mode 700), outside the repository.

## Resilience

- Bounded everything:
  - generator rows ≤ 25,601, `bad_ppm` ≤ 100,000;
  - batch size checked at write (`gen_orders.py:268`);
  - mock body ≤ 64 KiB (413), journal ≤ 100,000 entries (507, never a silent drop), in-flight ≤ 32 (503),
    fault delay ≤ 60 s, ≤ 64 fault rules, socket timeout 15 s, admin tail ≤ 1,000;
  - `etlctl` subprocess timeouts.
- Typed, readable failures: `GeneratorError`, `MockError`, `CtlError`, and the drafts' `etl.*` codes
  (`etl.too_many_batches`, `etl.batch_gap`, `etl.batch_header`, …).
- Fault injection for the crash matrix:
  - `status`, `delay` and `drop` on FX (class I retries);
  - `accept_then_drop` on the webhook (class C, unknown outcome), followed by a deduplicated retry.

## Security

| Threat / rule | Mechanism | Proof |
|---|---|---|
| Test services reach a product artifact | Guard over `deploy/**` and every Containerfile/Dockerfile (`check_etl_fixture_isolation.py:22,29,45`) | 13 guard tests; real tree passes; CI already runs `scripts/security/` |
| Plaintext DB or HTTP | PostgreSQL `ssl=on` + `hostssl` scram + `hostnossl reject`; MySQL `require_secure_transport` + users `REQUIRE SSL`; mock TLS mandatory with TLS ≥ 1.2 (`etl_mock.py:451`) | Guard invariants; live `verify` (§4); mock TLS tests |
| Secrets in fixtures | Credentials generated by `etlctl.py init` (`secrets.token_urlsafe(32)`) into a local `0600` env file; compose uses `${VAR:?}` only; tokens are files outside the repo | Guard `literal_password` test; diff secret scan (§8) |
| Secrets in evidence or the journal | Journal stores digests and lengths only; evidence is refused if it holds any known credential or a PEM, JWT or bearer shape (`etlctl.py:260-272`) | `test_journal_is_durable_and_holds_no_secrets`; evidence written cleanly |
| SQL injection or string-built SQL | Drafts: frozen statements, typed `{from, type}` parameters, no templating (lint test). Harness: psql variables (`:'name'`) and server-decoded base64 literals (`etlctl.py:350,369`). The MySQL user script validates the password alphabet before `IDENTIFIED BY` | `test_sql_is_frozen_text_with_typed_parameters`; `sqlcheck` |
| Shell built from strings | `subprocess` argument lists only; the in-container scripts are constants and receive values as positional arguments or container environment | Review |
| URLs or credentials in pipeline YAML | Rules are named by `operation`, with credentials by reference; the lint test forbids URLs, hosts, `password`, `secret`, `api_key` and bearer text | `test_no_url_host_or_credential_in_yaml` |
| Over-privileged services | Read-only reader roles (proven refused on insert); mock `read_only`, `cap_drop: ALL`, `no-new-privileges`; admin listener loopback-only (`etl_mock.py:442`); no published ports | `verify`; `test_admin_must_be_loopback` |
| Supply chain | No new dependency (standard library + openssl + docker). Images pinned by digest | Guard `unpinned image` test |

`rules/security.md` categories:
- **Apply and are covered above:** trust boundaries (bearer tokens, constant-time compare), input bounds, injection,
  secrets, supply chain.
- **Do not apply** (no product route, RPC or egress code changes): authorization matrix, edge identity, egress guard,
  web rendering, startup config.

Dependency scanners (`govulncheck`, `cargo deny`, `npm audit`) do not apply: no Go, Rust or Web dependency changed.

## Recovery-guarantee rows

These fixtures change no product phase, so they add no new product rows. They add the instruments that let the
final acceptance (2.4) and crash family `ETL-*` (2.2) prove these rows:

| Component × phase | Class the acceptance must show | Instrument here |
|---|---|---|
| Worker × tool call (HTTP GET, FX) | I: at most one served GET per batch after retries | Journal `fx.<run>.<batch>.served`; `check --expect-batches` |
| Worker/Main × tool call (HTTP POST, webhook) | I with idempotency key; C if the outcome is unknown | `accept_then_drop` fault; journal `accepted` = 1 per run |
| Worker × effectful DB write | R (marker present) / I (verified no effect) | Marker tables; per-run keys; `sqlcheck` double load |
| PostgreSQL/MySQL × open transaction | rolled back, then applied once | Per-run row counts versus the manifest |

None of these is L.

## Real-browser evidence

Not applicable to this change: it adds fixtures only, with no product or UI behaviour. The browser run (pipelines
rebuilt in the editor, uploads, admin rules, run from chat, reloads) is the final acceptance, item 2.4. It needs
items 0.4, 0.8, 1.3, 1.5, 1.9, 1.12, 1.14 and 1.15 merged first.

## How fixtures were created

- Data: by `gen_orders.py`, deterministic, outside the repository.
- Fixture services: from compose with generated local credentials. The warehouse schemas and users come from the
  image init scripts.
- Pipeline drafts: hand-written YAML. They were not saved through the UI or the Main API: HTTP, DB, Map-with-Code and
  5b settings are not admitted yet.
- The DB statements were executed directly on the warehouses by `sqlcheck`, not through a DB node.

## Reviews and scans

- **`code-review`** (medium, staged diff) found 10 issues. All were fixed:
  1. Warehouse healthchecks passed against the image's socket-only init server. They now check the TCP listener;
     MySQL uses TLS.
  2. Silver per-country totals were labelled as the gold oracle although they are computed before the global
     dedupe. They are renamed `by_country_before_global_dedupe` and dropped from the report.
  3. A delayed FX serve after a client timeout counted as a second served GET. It is now `served_delayed`.
  4. A torn final journal line crashed the restart. It is now truncated; earlier corruption is refused.
  5. Rotated journal names collided (pid 1). They are now timestamped and refuse to overwrite.
  6. The admin listener held the journal lock while writing its response. It now snapshots, then sends.
  7. The guard could be bypassed with list-form environment entries, `ssl=off` after `ssl=on`, or TLS flags kept
     only in comments. All three are closed and tested.
  8. A partial or expired test CA was never reissued. Both are now reissued.
  9. `sqlcheck` built the dataset three times with a second copy of the model. It now builds once, with one model.
  10. A copy-pasted TODO in `load_mysql.yaml`. Also, the fixture tests ran in no CI job; the guard test now runs
      them.
- **`security-review`** (opus sub-task on the staged and unstaged diff): no findings at confidence 8 or above. It
  checked SQL and command injection, path traversal, the bearer compare, admin exposure, certificate validation,
  secrets in the diff, logs, journal and evidence, and deserialization. One hardening note below the threshold:
  all three services mount the same test-CA leaf directory, so each container can read the others' throwaway leaf
  keys. The CA key is not mounted. Kept as a follow-up.
- Secret scan of the staged diff: no token, key or password values. Only placeholders such as
  `${ETL_PG_ADMIN_PASSWORD:?…}` and variable names.

## Follow-ups

1. **NEW, not in POINT5-REMAINING.** Reading one big CSV directly needs one of:
   - Code artifact operations on Main (today `unsupported_operation`), with a ranged read and a per-job byte budget
     above the production profile's 1 MiB;
   - or a ranged artifact text read.

   Until then the batch split is the documented design.
2. Pin at 2.4 against the live stack: the `list_files` and `read_multiple_files` answer shapes, and a state-modifier
   template that renders JSON into a dict channel (TODO(2.4) in the drafts).
3. Finalise the drafts as 1.12 (DB node) and 1.14 (Code, Router and effectful Toolkit in fan-out) land. Then save
   them through Main's validator and fix any refusals.
4. Crash-suite family `ETL-*` (2.2): reuse `etl_mock.py` faults, and the journal oracle as an invariant.
5. Hardening: mount only each service's own leaf key (security-review note).
6. Attaching the platform's Main and Worker to `elitea-etl-fixtures`, the egress allowlists and the test-CA trust
   are documented in the README "Admin set-up". They are not automated yet.
