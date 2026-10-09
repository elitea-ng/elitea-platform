# Live Code owner recovery, 2026-10-08

## Application behavior and ownership

The current application's Code behavior remains the reference.
An infrastructure restart must retain the original Code invocation, selected state, and result.
PostgreSQL owns execution authority. NATS carries commands. The Sandbox Supervisor owns the original sandbox receipt.

The private acceptance controller tests these contracts. It supplies no shipping behavior.

| Owner | Source or symbol | Responsibility |
| --- | --- | --- |
| Web | Normal saved-pipeline Chat and Send | Start one persistent request and show its original terminal result after reload. |
| Main | `infra/db/repos/claims.go`, `ClaimRecoverRunningNoACK` | Issue a fresh claim attempt and lease epoch with original checkpoint recovery authority. |
| Rust Worker | `agents/graph/code_runtime.rs`, `agents/graph/code_state.rs` | Select the saved source and count input. Apply the typed result once. |
| Rust Worker | `agents/graph/code_attempt_remote.rs`, `sandbox/code_recovery.rs` | Bind the original visit, request, job, prepared bundle, and dispatch identity. |
| Sandbox Supervisor | `sandbox/ledger.rs`, `sandbox/docker_supervisor.rs` | Reclaim the original job after lease expiry. Collect the original runtime receipt. |
| Rust Worker | `agents/graph/node_recovery_runtime.rs` | Preserve Completed and Failed.Stop journals. Suppress the downstream sentinel. |
| Main | `infra/db/repos/output_inbox.go` | Project one claim-fenced failure and commit its matching settlement. |
| Rust Worker | `execution/agent_delivery_processor.rs`, `transport/command_bus.rs`, `transport/nats_jetstream.rs` | Deliver the terminal result and acknowledge normal delivery after settlement. |

Worker paths are relative to `services/elitea-worker-rust/src/`.
Main paths are relative to `services/elitea-main/internal/`.
The current SDK Code reference remains `elitea_sdk/runtime/tools/function.py`.
This recovery design preserves user behavior through durable ownership rather than copying the legacy process implementation.

## Accepted deployed case

Chat851 starts unchanged saved pipeline147/version170 through normal Chat and one Send.
The JavaScript node waits30 seconds and returns its selected count2.
The next Python node intentionally has an empty variable source. The final9999 sentinel must not run.

The first strict cut observes the original running JavaScript runtime without a result or terminal settlement.
All16 isolation checks pass. The repeated cut still binds the same live claim and original job.
The original Worker loses its process. Its replacement uses the same immutable image and one fresh empty private spool.

The second strict cut observes that same running JavaScript runtime before its result.
The Sandbox Supervisor loses its process and restarts with the same full container contract.
All eight authenticated listeners return after restart.

The original JavaScript job completes with typed result2 under the same runtime and request identities.
Its sandbox lease epoch advances from4 to5 after normal expiry and recovery.
Main grants claim2/epoch2 with `AGENT_MODEL_CHECKPOINT` authority.
The original completed result projects once. The empty next node records typed invalid-input Failed.Stop.
No replacement execution, user Code rerun, extra sandbox job, or sentinel starts.

One189-byte `PIPELINE_CODE_FAILED` output and one matching FAILED settlement commit.
Both original runtimes return HTTP404 and are absent from the complete job-container list.
Stream messages, consumer pending, and pending acknowledgements return to zero.
Live and reloaded chat retain the same safe message and support reference `ca094eeb-04b0-531c-91a1-6b8dc0b657ba`.

The idle store retains74 terminal jobs,74 dispatches, and157 checkpoints.
All protected containers,72 material files,eight profiles,historical rows,and platform rows remain unchanged.
The three prior Workers and their unopened spools remain preserved.
The sole replacement Worker is `761c497b45b0a507587ce77d265f4d7fe14683363c7828d0d762cdd97552dff1`.

Its full fingerprint is `ce73a172195e21c7a9bf57aaaa02a80d08a84faf1f94c8c472c5b2e88a3dc081`.
Worker and Supervisor images retain the accepted c53 source boundary. Web retains its42a0 source boundary.

## Evidence and implementation history

The technical result digest is `b333f8f0c2a38ff6c19f52231c5f7facb2b3df07c6f40c3e1339f01d966e62c7`.
The independent root acceptance digest is `320c26ce098c1ba52137a0beeb2f5640b0b4a66f1901977ebd5aa5d3b7969f01`.
The browser receipt digest is `c1cf39ee24ec1791695c6501f3e45941d62deeb39825fe936f749c65e3457059`.
The current native adapter digest is `0caa1eabae0a3ca7447dcef539af5f010639ab48c262d2758f81e36c764bc794`.
The Worker live-cut digest is `59ec1bfeb15a070a08760673607bcbd2cd4723190bdb289ea204f8e8ee60fcc7`.
The Supervisor live-cut digest is `5aaddb1900fca8c34e0be8ca5bfc4dae8870e7105b5dbd2df1b022f9293bdf2e`.

This case requires no Rust product correction.
The private controller validates the accepted successor chain and exact retained container contracts.
Its mount exclusion cache uses an immutable conservative set after full preservation checks.
Every fault cut still reads fresh job, runtime, profile, claim, phase, and all16 isolation checks.

The terminal guard follows ledger behavior: a completed receipt can retain a future lease timestamp.
It requires completed receipt ownership and epoch continuity. It does not require an artificial lease wait.
The frozen earlier controller and all accepted historical receipts remain unchanged.

## Limits

This case closes live Worker and Supervisor loss for the original JavaScript Code job.
Preparation failure outcomes and replacement debug reconciliation remain open.
Later [chat852](code-compiled-publication-main-recovery-20261008.md) closes Main compilation publication loss for the original successful compiler receipt.
Queued/in-flight NATS loss and current Kubernetes acceptance remain open.
The separate live canvas image later passes build and strict scan. Its deployed browser acceptance remains open.
Stored cleanup flags remain false. Independent removal checks supply physical cleanup proof.
Product and execution-store observations are sequential, not atomic. This case supplies no performance benchmark.

## Deployed crash matrix on the merged branch (2026-10-09)

The branch head is `22f33b8f8`: `main` merged in, plus the PERF-1084 changes. These runs used a standalone compose stack,
`elitea-code1084`, at `http://code1084.localhost:18300`.

**Stack.**
- Images: Main `644541553d39`, Worker `f6f064aabe94`, Supervisor `9ae7d121a4e4`, Web `23587013fbad`.
- Binary checks: the Worker contains `code_timing.rs` and `pipeline.code_cancelled`, which `main-c0f2e5f9b-verify` does
  not. Main contains `resumeCurrentAgentStaticTools`. The Supervisor contains `service_job_owners.rs`. #1160 is an
  ancestor of the head.
- Database: real-model product DB, shared migrations at 158, tenants at 148.
- Code runtimes: Deno (Python, JavaScript, TypeScript) and pure Rust. `agent_node_recovery: true`.

**Fixture.** Pipeline 166, created in the UI. It has one JavaScript `code` node that records its start time, waits 25 s,
and returns `SLOW_DONE started=<time>`.

**Fault injection.** Each fault is `docker kill` (SIGKILL), sent about 9 s after Send while the runtime is running,
followed by `docker start` 6–8 s later.

**Invariants.** All of them are scoped to this stack's own `elitea_runtime.sandbox_jobs` rows. Runtime containers carry
no stack label on the shared Docker daemon, so other stacks' `elitea-code-*` runtimes are excluded.
- **One sandbox job per run.** The ledger row count grows by exactly 1.
- **The original result.** The stored start time is earlier than the kill time.
- **One settlement** in `elitea_runtime.execution_settlements`.

| Fault | Execution | Kill → restart | Settlement | Answer (stored) | Class |
|---|---|---|---|---|---|
| none, baseline | `f8e02bcf…` | — | SUCCEEDED, attempt 1 | `started=17:54:02.579Z` | — |
| Worker | `c4e66b17…` | 17:55:30 → 17:55:36 | SUCCEEDED at 17:56:27, attempt 2 / epoch 2 | `started=17:55:21.739Z` | R |
| Stop | `077441c2…` | Stop clicked at about 8 s | CANCELLED at 17:57:49; runtime removed | "Execution was cancelled." | — |
| Supervisor | `37697d0e…` | 18:09:28 → 18:09:34 | SUCCEEDED at 18:10:25, attempt 1; sandbox lease epoch 2 | `started=18:09:20.809Z` | R |
| Main | `f1e0e427…` | 18:11:17 → 18:11:24 | SUCCEEDED at 18:12:19, attempt 2 / epoch 2 | `started=18:11:09.325Z` | R |
| Main + Worker + Supervisor | `9452c832…` | 18:12:59 → 18:13:07 | SUCCEEDED at 18:13:57, attempt 2 / epoch 2; sandbox lease epoch 2 | `started=18:12:50.830Z` | R |

**Reproduction.** An independent investigation reproduced the Worker case: one job (`f947b03f…`), one runtime created
at 18:05:27 and destroyed at 18:05:57, original result recovered.

**Canvas correction `ac6e0adea`.** After the Stop, the editor shows "Run is stopped" and clears the active node. This
closes the deployed-browser item listed for it.

**Observations, not defects of this branch.**
- After a Worker takeover, the live test chat showed only the node chip until reload; the stored answer is complete.
  Recorded as a Web live-delivery follow-up.
- The Worker log filter (`diagnostics.rs`) drops `elitea_agent_runtime` events, so recovery detail from shared crates is
  not logged. This is pre-existing and tracked separately.
- Runtimes have no per-deployment label. A future orphan sweeper (G-SUP-03) must scope by owner before it removes
  anything.

## Performance (PERF-1084)

Source: the PR #1084 performance review (findings F1, F2, F3, F4, F8, F9). Costs are estimates from code paths except where a test measures them. No durability rule changes: NATS `sync_interval`, the end-of-transaction fence re-check, the stored result copies and unsettled ledger rows are untouched, and nothing answers from memory before commit.

| Finding | Mechanism (`path:line`) | Constant | Proving test | Before / after |
|---|---|---|---|---|
| F1 duplicate access lock | `services/elitea-main/internal/infra/db/repos/code_sandbox_intent.go` `lockAccessIdentity`: one `codeAccessSQL` execution; the second `lockAccess` per `With*Intent` transaction stays as the fence | `codeIntentTxAccessQueryBudget = 2`, `codeIntentTxStatementBudget = 10` | `code_sandbox_intent_budget_test.go` `TestCodeIntentTxStatementBudget` (counting fake over the scripted executor; no PostgreSQL) | 4 access queries (8-way join, request bytes up to 8 MiB each) per transaction -> 2 |
| F2 pump idle backoff | `src/agents/graph/code_attempt_remote.rs:323-346` uses `code_timing::next_idle_interval`; reset on any non-idle step | `PLATFORM_PUMP_MIN_INTERVAL` 250 ms, `PLATFORM_PUMP_MAX_IDLE_INTERVAL` 1 s, `PLATFORM_PUMP_IDLE_STEPS_PER_MINUTE_BUDGET = 70` | `code_timing::tests::idle_interval_grows_and_is_capped`, `idle_steps_per_minute_stay_within_budget` | 240 idle steps per minute -> 61 (250, 500, then 1 s) |
| F3 frozen lookup capacity | `src/sandbox/docker_supervisor.rs:41,158` `lookup_capacity`; `src/sandbox/docker_preparation.rs:76-111` `admit_frozen_lookup` replaces the execution permit | `FROZEN_LOOKUP_CONCURRENCY = 16` | `frozen_lookup_succeeds_while_execution_capacity_is_exhausted` (limit) | a saturated Supervisor refused every lookup -> lookups use their own 16 permits |
| F3 Worker retry | `src/agents/graph/code_preparation.rs:195` `retry_frozen_lookup`: retryable errors (including `ResourceExhausted`) wait `CODE_RECONCILE_INTERVAL` until the absolute `observation_deadline`; non-retryable errors and timeouts stay terminal; errors never fall through to prepare | `CODE_RECONCILE_INTERVAL` 1 s | `frozen_flow_retries_busy_lookup_until_hit` (Busy, Busy, hit: 3 lookups, 0 preparations, 2 s of paused time), `frozen_lookup_busy_until_deadline_is_unconfirmed_and_bounded` (deadline), `frozen_lookup_terminal_error_is_not_retried`; existing `frozen_flow_lookup_errors_never_prepare` still passes | terminal `Unconfirmed` on the first Busy -> retried until the deadline |
| F4 takeover backoff | `code_attempt_remote.rs:354,481` `PendingBackoff` (capped exponential with jitter) around the unchanged deadline check | `CODE_PENDING_INITIAL_INTERVAL` 1 s, `CODE_PENDING_MAX_INTERVAL` 5 s, `CODE_PENDING_JITTER_PERCENT = 25`, `CODE_RECONCILE_SUBMISSIONS_PER_MINUTE_BUDGET = 20` | `code_timing::tests::pending_backoff_is_capped_jittered_and_bounded` (per-wait cap, jitter lower bound, at most 20 submissions in 60 s); the deadline `min(...)` and the post-attempt deadline check are unchanged | 60 submissions per minute -> 14 to 17 |
| F9 named intervals | `code_timing.rs`; used in `code_attempt_remote.rs`, `code_compiled.rs:350,411`, `code_remote.rs:428,463`, `code_preparation.rs`, `code_workspace_remote.rs:143,192` | `CODE_FAST_RECONCILE_INTERVAL`, `CODE_RECONCILE_INTERVAL`, `OBSERVATION_MARGIN` (90 s, same value) | `observation_margin_covers_supervisor_lease_and_heartbeat` (pins the margin against `LEASE_SECONDS` and the new `HEARTBEAT_INTERVAL` in `docker_supervisor.rs:32-33`) | no behavior change |
| F8 identity probes | `src/sandbox/dispatch.rs:89` `contains_activations` (`= ANY($4::bytea[])`); `code_preparation.rs:177` | `PREPARATION_IDENTITY_PROBES_BUDGET = 1` | `identity_probe_fold_matches_sequential_probes` (truth table against the three sequential probes); real PostgreSQL: `sandbox_dispatch_journal_preserves_exact_pending_identity` asserts the batched answer equals the per-id answers | 3 round trips -> 1 for a fresh Rust preparation |

Deliberately not done (follow-ups in the review): byte projection of the access query (F1.2), long-poll pump and no-op write skip (F2.2, F2.3), reattach-to-owner and intent reuse (F4.2, F4.3), batched hydration (F5), retention (F6), single stored result (F7).

Test counts: Worker lib `cargo test --offline --locked --all-features --lib` 1778 passed, 0 failed, 74 ignored (run with the PostgreSQL environment, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`). The new tests are 9 Rust unit tests and 1 Go test, plus one added assertion block in the ignored real-PostgreSQL journal test. Main `./internal/...` passes in `golang:1.26.9`.
