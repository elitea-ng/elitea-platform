# ETL acceptance fixtures (Point 5)

Fixtures for the final Point 5 acceptance: a Databricks-style medallion ETL job (bronze → silver → gold) built from
pipelines that call each other. It is a UI demonstration that every Point 5 node family works. Everything here is
**test-only**. `scripts/security/check_etl_fixture_isolation.py` fails CI if a product image, Helm chart or default
compose file references these services.

| Path | What it is |
|---|---|
| `gen_orders.py` | Seeded data generator (standard library only) with a manifest of exact expected results |
| `fx_rates.json` | The FX table, shared by the generator (oracle) and the mock (service), so they cannot drift |
| `compose.yaml` | `warehouse-pg`, `warehouse-mysql`, `etl-mocks`: TLS only, the harness's own test CA, no published ports |
| `warehouse/` | TLS entrypoints, `pg_hba.conf` (hostssl only), roles/users and schemas for both dialects |
| `etl_mock.py` | FX reference service, completion-webhook receiver and request journal |
| `etlctl.py` | `init`, `material`, `up`, `verify`, `sqlcheck`, `down` |
| `pipelines/*.yaml` | The pipeline drafts (see "Pipelines" and "Coverage") |
| `tests/` | Unit tests for the generator, the mock and the drafts |

## Run

```bash
# Unit tests (about 15 s; the mock tests need openssl)
python3 -m unittest discover -s scripts/acceptance/etl/tests -v
python3 -m pytest -q scripts/security/test_etl_fixture_isolation.py   # or: python3 scripts/security/check_etl_fixture_isolation.py

# Data: write it OUTSIDE the repository
python3 scripts/acceptance/etl/gen_orders.py --out ~/etl-data                      # the acceptance set
python3 scripts/acceptance/etl/gen_orders.py --out ~/etl-data --rows 20000 --seed 7  # one custom file

# Fixture services: credentials and certificates stay in the stack directory (default ~/.elitea-etl-acceptance,
# mode 700; override with ETL_STACK_DIR or --stack-dir). Nothing is written into the repository.
python3 scripts/acceptance/etl/etlctl.py init       # generated test credentials -> etl.env (mode 600) + mock tokens
python3 scripts/acceptance/etl/etlctl.py material   # test CA + server certificates (30 days)
python3 scripts/acceptance/etl/etlctl.py up         # compose project elitea-etl-fixtures, waits for health
python3 scripts/acceptance/etl/etlctl.py verify     # TLS-only proof + journal proof -> evidence JSON
python3 scripts/acceptance/etl/etlctl.py sqlcheck [--rows 24000]   # the drafts' SQL on both warehouses, twice
python3 scripts/acceptance/etl/etlctl.py down       # removes the containers and volumes
```

Keep evidence that must survive a pause under `handoffs/` (`--evidence-dir`), never only in a scratch directory.
The shared-machine rules apply: no image builds here (only pinned, pulled images), and check free disk first.
Take a docker slot around every bring-up and release it on every exit path: `handoffs/build-slots/slot.sh acquire
docker etl 6` … `slot.sh release docker etl` (00-COMMON "Build priority and slots").

- `verify` proves for each service that plaintext is refused and verified TLS works:
  - PostgreSQL: `sslmode=disable` gets `pg_hba.conf rejects ... no encryption`; an untrusted CA fails; a
    `verify-full` session reports `ssl = t`, TLSv1.3. The reader role cannot write.
  - MySQL: a plaintext root login gets `ERROR 3159` (`require_secure_transport`); a `REQUIRE SSL` user is refused;
    `VERIFY_IDENTITY` reports TLSv1.3. The reader cannot write.
  - Mocks: plain HTTP is closed during the handshake; FX needs the bearer token; the webhook answers 201 for the
    first delivery, 200 for the repeat with the same key and body, 409 for the same key with another body; the
    journal oracle (`check`) passes.
- `sqlcheck` extracts the DB statements from `load_postgres.yaml`, `load_mysql.yaml` and `gold_build.yaml` and runs
  them as the loader over verified TLS. It loads a dataset twice under one run_id. Then fact and gold tables must
  equal the manifest exactly, and PostgreSQL must equal MySQL. Values reach SQL only as psql variables or base64
  literals decoded by the server: no string-built SQL.

### Mock

`etl_mock.py` journals every request before answering, as JSON lines on the `etl-journal` volume, fsynced. A lost
response is still counted. The journal holds ids, statuses, digests and lengths only: never header values, tokens
or bodies. The webhook dedupe index is rebuilt from the journal when the mock starts. Use the admin listener with
`docker exec` (it is bound to loopback inside the container):

```bash
docker exec elitea-etl-fixtures-etl-mocks-1 python3 -I /app/etl_mock.py summary
docker exec elitea-etl-fixtures-etl-mocks-1 python3 -I /app/etl_mock.py check --run-id <run_id> --expect-batches 60
docker exec elitea-etl-fixtures-etl-mocks-1 python3 -I /app/etl_mock.py faults --set \
  '[{"route": "fx", "batch_no": 3, "action": "status", "status": 503, "times": 2}]'
docker exec elitea-etl-fixtures-etl-mocks-1 python3 -I /app/etl_mock.py rotate   # fresh journal between runs
```

Fault actions:

| Action | Effect |
|---|---|
| `status` | 429 or 5xx, nothing served |
| `delay` | Waits up to 60 s, then serves. FX journals it as `served_delayed`, reported apart: the client may have timed out and retried |
| `drop` | Closes the connection with no effect |
| `accept_then_drop` | Webhook only: the delivery is accepted and journaled, then the connection is closed. This is the unknown-outcome (class C) case. A retry with the same key gets 200 duplicate. |

`check` fails when:
- a run has two accepted webhook deliveries;
- an idempotency key differs from the run_id;
- any batch has more than one served FX GET;
- with `--expect-batches`, a batch has no served FX GET.

## Data

The generator is deterministic: the same `--seed` and `--rows` always give the same bytes (tested). By default it
writes 24,000 orders in 14 columns of mixed types. The big `orders.csv` is about 2.6 MB.

- Columns: `order_id`, `order_seq`, `customer_id`, `country`, `currency`, `amount`, `discount_pct`, `ordered_at`,
  `ship_date`, `channel`, `sku`, `qty`, `tags` (multi-valued `a|b`) and `note` (free text with commas and quotes,
  RFC 4180 quoting).
- **Dirty values to clean** (never rejected):
  - padded or lower-case `country`;
  - mixed-case or padded `channel`;
  - padded `amount`;
  - nulls in `discount_pct`, `ship_date`, `tags` and `note`.
- **Planted defects**: 1.2 % of the rows. Each fails exactly one expectation (`valid_date`, `positive_qty`,
  `known_currency`, `unique_order_id`, `unique_order_id_global`), and there is one malformed line
  (`well_formed_line`).
- **`manifest.json`** holds the exact results the job must produce:
  - per-batch counts, reject rates and routes;
  - cleaning counts;
  - `fact_orders` totals to the cent, with `net_amount_usd = round_half_up(amount × (1 − discount/100) × rate, 2)`;
  - the three gold tables and the tag counts;
  - the quality-gate route.

  `expected-rejects-<file>.csv` lists every reject in the order `silver_clean` writes `rejects-<run>.csv`.

The acceptance set:

| File | Rows | Purpose |
|---|---|---|
| `orders.csv` | 24,000 | Happy path. Reject rate 1.2 %, so the quality gate publishes without approval |
| `orders-gate.csv` | 24,000 | 3 % rejects: the approval HITL pauses (reload while pending) |
| `orders-1k.csv` | 1,000 | Smoke run |
| `orders-limit.csv` | 25,600 | Exactly the declared row limit (64 batches): accepted |
| `orders-limit-plus-1.csv` | 25,601 | 65 batches: `bronze_ingest` refuses with the typed `etl.too_many_batches` |
| `customers.csv` | 3,000 | The `dim_customer` dimension; one file stays under one text read |

Each orders file comes with `<name>.batches/batch-NNNN.csv`: 400 rows each, header repeated, at most 190,000
characters. The batches joined together are byte-identical to the big file (tested).

### Artifact limits (why there are row batches)

Measured on `main` at `ddc92a3cf`. The big CSV is a single artifact and is uploaded as one, but no pipeline node can
read a multi-megabyte artifact whole today.

| Path | Limit | Where |
|---|---|---|
| Artifact toolkit text read (`read_file`, `read_multiple_files`) | A whole file above 200,000 characters is refused, even with `start_line`/`end_line`. `read_multiple_files` stops at 200,000 characters in total. | `services/elitea-main/internal/infra/storage/runtime_artifact_object.go:65,669-675` |
| Artifact toolkit write (`create_file`) | 65,536 characters per call | `runtime_artifact_object.go:79`, `libs/rust/agent-runtime/src/toolkits/families/artifact/tools.rs:35` |
| Code node platform client | `artifact_head`/`read`/`read_chunk`/`write_*` are refused with `unsupported_operation` on Main. Objects are capped at 8 MiB, and the production profile allows a Code job 32 calls and 1 MiB. | `services/elitea-main/internal/application/codeplatform/native_operations.go:51,91-93`, `services/elitea-main/internal/infra/storage/code_platform_native.go:129-140,236`, `services/elitea-main/internal/domain/codeplatform/frame.go:23` |
| Office host (docx/xlsx/pptx) | 50 MiB binary | `libs/rust/agent-runtime/src/toolkits/families/office_host.rs:29` |
| Map | ≤ 64 items, ≤ 8 concurrent | `services/elitea-worker-rust/src/agents/graph/map_reduce.rs:26,28` |
| Split Out / Aggregate output | ≤ 32,768 values, ≤ 512 KiB, ≤ 10,000 items | `services/elitea-worker-rust/docs/split-out-aggregate-contract.md` §1 |
| Whole-state checkpoint | 65,536 values | same document, §1 |

**Choice: Split Out over row batches.**
- `bronze_ingest` lists `<dataset>.batches/` with the artifact toolkit (`list_files`). It turns the listing into a
  batch plan and splits it into ≤ 64 items. The declared limit is 64 × 400 = 25,600 rows.
- `silver_batch` reads one batch with one text read (≤ 190,000 characters, under the 200,000 cap).
- It writes one silver CSV of under 64 KiB (no `tags`/`note` columns), so one `create_file` call is enough.
- The loaders read three silver files per `read_multiple_files` call.
- Parent state stays bounded: each batch result carries its order ids as one compact string.

Reading the single big CSV directly would need one of two new capabilities. Neither is in POINT5-REMAINING.md:
- Code artifact operations on Main (today `unsupported_operation`), with a per-job byte budget above 1 MiB and a
  ranged read;
- or a ranged text read on the artifact toolkit.

## Pipelines

The drafts in `pipelines/` are the fixtures; the browser run rebuilds the same pipelines in the editor. The five
pipelines of ACCEPTANCE-ETL §2, plus the Map child and the two Parallel children:

```
etl_job (root, from chat: "orders <run label>")
 ├─ bronze_ingest   list batches → plan → bronze marker (DB write, PostgreSQL) → Split Out (row_field)
 ├─ silver_clean    Map over batches ──► silver_batch (read, validate, route, FX, normalize, shaping, write)
 │                  → Aggregate join → Split Out / Aggregate → dedupe_global → rejects-<run>.csv
 ├─ gold_build      Parallel: load_postgres ‖ load_mysql (stage loop, merge_fact, gold) → reconcile (read, both)
 ├─ quality gate    Router → HITL approval (above 2 % rejects or any drift)
 └─ publish         Parallel: report (artifact) ‖ webhook (HTTP POST, exactly once) → LLM summary → Printer
```

Deliberate differences from the ACCEPTANCE-ETL §2 tables:
- `enrich_fx` runs before `normalize`, because `net_amount_usd` needs the FX rate.
- Typed reducers live in `etl_job`, because a nested child refuses them (`compiler.rs:1405-1408`; decision X3 keeps
  that refusal).
- Code writes no artifacts: Main does not serve them to Code yet. Artifact toolkit nodes write instead.
- A reconcile drift goes to the approval HITL rather than failing the run (§2.4 over §2.3).

Every node that needs unmerged work names the POINT5-REMAINING.md item that unblocks it, as `TODO(<item>)`
(`tests/test_pipeline_fixtures.py` enforces it):

| Item | Unblocks |
|---|---|
| 0.4 | Saved pipelines as Application nodes (scoped continuation) |
| 0.8 | Parallel admitted |
| 1.3 | Map admitted |
| 1.5 | 5b node settings |
| 1.9 | HTTP node |
| 1.12 | DB node |
| 1.14 | Code, Router, effectful Toolkit and non-Agent branches inside Map and Parallel |
| 1.15 | HTTP and settings inside fan-out children |
| 2.4 | Pin a toolkit answer shape or a template-to-dict projection against the live stack |

### Admin set-up for the run (once 1.9 and 1.12 land)

All of this is created in the UI.

- **Artifact toolkit** `ETL artifacts`: bucket `etl`. Upload `orders.csv`, `orders.batches/`, `customers.csv`.
- **HTTP rules**. Both use a credential by reference: an `openapi` configuration with a Bearer key whose value is the
  token in `<stack>/tokens/`. Both need the `etl-mocks` address in `ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST`, and the
  harness CA in Main's trust.
  - `fx_reference.rates`: `GET https://etl-mocks:8443/fx/v1/runs/{run_id}/batches/{batch_no}/rates`.
  - `etl_webhook.complete`: `POST https://etl-mocks:8443/hooks/v1/etl-complete`, replay `idempotency_key`.
- **DB rules**. They use `sql` configurations for `etl_loader` (write) and `etl_reader` (read) with the passwords
  from `<stack>/etl.env`. They need the warehouse addresses in `ELITEA_DB_EGRESS_ALLOWLIST`, and the harness CA in
  the Worker's trust (PostgreSQL `VerifyFull`, MySQL `VerifyIdentity`).
  - `warehouse_pg.load` (write, marker table `etl.elitea_effects`) and `warehouse_pg.read` (read): PostgreSQL,
    database `warehouse`.
  - `warehouse_mysql.load` and `warehouse_mysql.read`: MySQL, database `warehouse`, marker table `elitea_effects`.
- **Network**: attach the platform's Main and Worker to the fixture network:
  `docker network connect elitea-etl-fixtures <container>`.

## Coverage

Which node and mode covers which Point 5 feature. `tests/test_pipeline_fixtures.py` checks that every row is
present in the drafts.

| Feature | Where (pipeline.node) | Modes / settings | Admitted on main today? |
|---|---|---|---|
| Code, Python with prepared packages | silver_batch.validate | `python-dateutil`, `pycountry` via literal `micropip.install` | yes |
| Code, Python | bronze_ingest.parse_request/plan_batches/emit, silver_clean.dedupe_global, gold_build.compare, load_*.to_rows, etl_job.assess | typed `ValueError` refusals (`etl.too_many_batches`, …) | yes |
| Code, JavaScript | etl_job.start_run | WebCrypto SHA-256 run id | yes |
| Code, TypeScript | silver_batch.normalize | exact BigInt money | yes |
| Split Out `row_field` + `retain: only` (with `missing: null`) | bronze_ingest.split_batches | `remove_source: true` | yes |
| Split Out `rows_field` + `retain: all` | silver_batch.tags_split | `missing_list`/`null_list: empty` | yes |
| Split Out `rows_field` + `retain: except` | silver_clean.country_split | `missing_list`/`null_list: empty` | yes |
| Split Out `list` (retain none) | silver_clean.batch_split | — | yes |
| Aggregate `plain`, no group_by | silver_clean.merge_batches | `count_rows`, `sum_int`, `min_int`, `max_int`, `first`, `last`, `collect` (`merge_lists`), `collect_rows` (retain only), skip policies | yes |
| Aggregate `plain`, group_by 2 keys | silver_batch.country_stats | `count_rows`, `sum_int`, `min_int`, `max_int`, `first`, `last` | yes |
| Aggregate `plain`, group_by with `missing: null` | silver_batch.reject_summary | `collect_rows` (retain only), `min_int` | yes |
| Aggregate `split_out` layout, group_by | silver_batch.tag_counts, silver_clean.country_totals, silver_clean.route_counts | `count_rows`, `sum_int`, `collect` | yes |
| Aggregate `split_out` + `regroup: parent` | silver_batch.tags_regroup | `first`, `collect`, `last` (`missing: null`, `null: keep`) | yes |
| Map (bounded fan-out, ordered collection) | silver_clean.per_batch → silver_batch | `max_items: 64`, `max_concurrency: 8` | no: TODO(1.3), (1.14), (1.15) |
| Parallel (warehouses) | gold_build.load_both | `wait: all`, `fail_after_drain` | no: TODO(0.8), (0.4), (1.14) |
| Parallel (publish) | etl_job.publish | Toolkit ‖ HTTP branches | no: TODO(0.8), (1.14), (1.15) |
| HTTP GET, safe-method retry (class I) | silver_batch.enrich_fx | 5b `retry_on_fail`, `max_tries: 3`, `wait_between_tries_ms`, `timeout_ms` | no: TODO(1.9), (1.15), (1.5) |
| HTTP POST, idempotency key (exactly once) | etl_job.send_webhook | 5b `execute_once`, `timeout_ms`, `on_error: continue`, `always_output_data` | no: TODO(1.9), (1.5) |
| DB write transaction, PostgreSQL | bronze_ingest.land_bronze, load_postgres.stage/merge_fact/gold_aggregates | 1–6 statements in one transaction; marker table | no: TODO(1.12) |
| DB write transaction, MySQL | load_mysql.stage/merge_fact/gold_aggregates | same | no: TODO(1.12) |
| DB read-only, both dialects | gold_build.reconcile_pg/reconcile_my, load_*.read_totals | `rows`, `first_row` | no: TODO(1.12) |
| Router | etl_job.quality_gate, silver_batch.route_quality, load_*.more_groups (loop) | fail-closed `default_output` | yes |
| HITL | etl_job.approve_publish | approve / reject routes | yes |
| State modifier + typed reducers | etl_job.record_counts/record_publication/count_publication | `merge`, `append`, `sum_int` | yes (root only) |
| 5b `on_error: error_output` | silver_batch.validate → batch_failed | `error_transition` | no: TODO(1.5) |
| 5b failures as results (§8.7) | etl_job.send_webhook → compose_answer reads `node_errors` | `on_error: continue` | no: TODO(1.5) |
| Toolkit (artifact) | bronze_ingest.list_batches, silver_batch.read_batch/write_silver, load_*.read_customers/read_group, silver_clean.write_rejects, etl_job.write_report | `list_files`, `read_file`, `read_multiple_files`, `create_file` (effectful, journaled) | yes (root); in Map: TODO(1.14) |
| Application (saved pipeline) | etl_job.bronze_ingest/silver_clean/gold_build, gold_build.load_pg/load_my, silver_clean.batch_worker | JSON text task | TODO(0.4) |
| LLM + Printer | etl_job.compose_answer → print_answer | real model | yes |
