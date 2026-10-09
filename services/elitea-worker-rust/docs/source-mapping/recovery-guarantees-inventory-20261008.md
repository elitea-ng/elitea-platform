# Recovery guarantees inventory

Baseline: `origin/main` `fcf86c31` (2026-10-08). This entry records the method, the evidence ruling and the ranked gap
backlog behind [recovery-guarantees.md](../recovery-guarantees.md). It changes no code.

## 1. Business behaviour from the current platform

The current Elitea platform (Pylon and Arbiter) keeps task results in arbiter memory. Results are ephemeral. The
retention is about 3600 s, and a file-backed result is deleted on read. A lost worker means "run again from scratch".

This change does not port that behaviour or its defects. The replatform target is class R: execution continues from
the point of stoppage. No current-platform code is ported by this change.

## 2. Changed paths

- `services/elitea-worker-rust/docs/recovery-guarantees.md` (new).
- `services/elitea-worker-rust/docs/source-mapping/recovery-guarantees-inventory-20261008.md` (new, this file).
- `services/elitea-worker-rust/docs/source-mapping/README.md` (one bullet in "Detailed ledgers").

No code, CI, chart or test file changes.

## 3. Method

Four component analyses and one adversarial review ran over `fcf86c31`. All work was read-only.

- The analyses covered Main and the browser, the Worker, the Supervisor, and NATS, PostgreSQL and the gateway.
- The adversarial review checked each R, I and PROVEN claim. Its rulings win every conflict (D1 to D5 in the matrix).
- Sources read: Go and Rust code, Helm charts and values, CI workflows, the 191 source-mapping ledgers,
  `services/elitea-worker-rust/docs/remaining-gates.md` and `services/elitea-worker-rust/docs/testing-gaps.md`.

State of evidence for this inventory:

- No test was run.
- No stack was deployed.
- No browser run was made.

Every proof in the matrix comes from existing tests and existing source-mapping documents.

## 4. Tests

None executed by this change.

CI facts the matrix relies on (D4):

- `.github/workflows/ci-rust.yml:240-245` sets `ELITEA_TEST_DATABASE_URL`, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` and `ELITEA_REQUIRE_NATS_SECURE_TEST=1`. The PostgreSQL tests and `services/elitea-worker-rust/src/execution/nats_live_tests.rs` run in CI.
- `--ignored` modules in CI: `docker_supervisor::deadline_tests`, `::preparation::tests` and `::hydration::deadline_tests` (`.github/workflows/ci-rust.yml:255,274-275`).
- `.github/workflows/ci-go.yml:346-404` runs the PostgreSQL integration tests and `TestJetStreamCapacityReliability`.

`#[ignore]` suites that CI never runs:

| File | Lines |
|---|---|
| `services/elitea-worker-rust/src/sandbox/ledger_code_recovery_tests.rs` | 38, 122, 170 |
| `services/elitea-worker-rust/src/sandbox/docker_code_recovery_cleanup_tests.rs` | 143, 202, 264 |
| `services/elitea-worker-rust/src/sandbox/ledger_workspace_postgres_tests.rs` | 82, 161 |
| `services/elitea-worker-rust/src/state/postgres_checkpointer_tests.rs` | 731, 865, 939, 996, 1345 |

Declared Go skip: `TestProductionRuntimeCrossProcessSystem` (`scripts/go/declared-skips.txt:47`). It drives the Python
worker and covers `configuration.validate` only. No run record exists. It is not evidence.

## 5. Performance, durability, resilience, security

Measured result for every mechanism below: none (inventory). This change measures nothing.

### Performance

| Mechanism | Path:line | Proving test |
|---|---|---|
| Bounded admission with a capacity reservation and a reaper | `services/elitea-main/internal/infra/db/repos/agent_execution_jobs.go:273-365` | `services/elitea-main/internal/infra/db/repos/agent_admission_capacity_postgres_integration_test.go:24,170` |
| Outbox batch up to 256, concurrency up to 32, poll 250 ms | `services/elitea-main/internal/application/execution/outbox_publisher.go:19-23,100` | None for the limits. |
| NATS `MaxAckPending` 1024 and `MaxMsgSize` 65536 | `deploy/helm/nats-bootstrap/files/bootstrap.sh:397-470` | `services/elitea-main/internal/transport/commandbus/producer_test.go:102` |
| Spool size limit 2Gi | `deploy/helm/elitea/values.yaml:3630` | `services/elitea-worker-rust/src/execution/output_delivery_tests.rs` |
| Worker drain 30 s, Main drain 15 s, gateway drain 150 s | `services/elitea-worker-rust/src/execution/production.rs:140-166` | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:571,907` (unit) |

### Durability

| Mechanism | Path:line | Proving test |
|---|---|---|
| Intent before effect: job, bundle and outbox row in one transaction | `services/elitea-main/internal/infra/db/repos/agent_execution_jobs.go:420` | `services/elitea-main/internal/infra/db/repos/agent_admission_capacity_postgres_integration_test.go:400` |
| Writer fence on every agentstate write | `services/elitea-worker-rust/src/state/postgres_session.rs:1655-1690` | `services/elitea-worker-rust/src/state/postgres_session_tests.rs:266` |
| Model-boundary marker before each model request and tool dispatch | `services/elitea-worker-rust/src/agents/model_checkpoint.rs:19-29,517,578` | `services/elitea-worker-rust/src/agents/model_checkpoint.rs:1063,1157` |
| Settlement order: terminal, Main ack, receipt, retirement | `services/elitea-main/internal/infra/db/repos/claims.go:416-426` | `services/elitea-worker-rust/src/execution/output_delivery_tests.rs:4186,4360` |
| Sandbox ledger and dispatch journal | `services/elitea-worker-rust/src/sandbox/ledger.rs:184,397-420,501` | `services/elitea-worker-rust/src/sandbox/docker_deadline_tests.rs:325` (CI). The `#[ignore]` suites do not run. |

### Resilience

| Mechanism | Path:line | Proving test |
|---|---|---|
| Typed failures with data-free errors | `services/elitea-worker-rust/src/transport/openai_compatible_facade.rs:2143` | `services/elitea-worker-rust/src/transport/openai_compatible_facade_tests.rs:1254` |
| Poison handling: dead-letter, `Term` or `nak(24h)` | `services/elitea-worker-rust/src/execution/command_delivery.rs:895-940` | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:712,745,776` |
| Lease loss: the Worker stops on lost margin | `services/elitea-worker-rust/src/execution/agent_lease.rs:259-356` | `services/elitea-worker-rust/src/execution/output_delivery_tests.rs:4132` |
| Phase deadlines on the PostgreSQL clock | `services/elitea-worker-rust/src/sandbox/ledger.rs:273-300` | `services/elitea-worker-rust/src/sandbox/docker_deadline_tests.rs:325,434` |

Known weak points: the generic "The runtime operation failed." text for a refused recovery (G-ADV-01) and the 24 h
poison wait (G-NATS-04).

### Security

| Mechanism | Path:line | Proving test |
|---|---|---|
| Ed25519 verify before decode, domain-separated | `services/elitea-worker-rust/src/protocol/command.rs:216-246` | `services/elitea-worker-rust/tests/agent_command_contract.rs:221` |
| Subject hash token checked against `sha256(idempotency_key)` before claim | `services/elitea-worker-rust/src/protocol/command.rs:727` | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:776` |
| mTLS peer identity from the certificate SAN only | `services/elitea-worker-rust/src/sandbox/peer_identity.rs:17-60` | `services/elitea-worker-rust/src/sandbox/peer_identity.rs:89` |
| Sandbox grants bound to peer, audience, scope and purpose | `services/elitea-worker-rust/src/protocol/sandbox_grant.rs` | `services/elitea-worker-rust/src/protocol/sandbox_grant.rs:622,960` |
| Decision digests and one-time consumption under a row lock | `services/elitea-main/internal/infra/db/repos/agent_execution_jobs.go:803,848` | `services/elitea-main/internal/infra/db/repos/agent_pipeline_hitl_history_postgres_integration_test.go:129` |

Open check: G-ADV-08. Continuation re-authorizes the deciding actor
(`services/elitea-main/internal/application/agentexecution/continue_static.go:256-280`). No comparison with the pause
owner was found. A negative authorization test is needed. This is not confirmed as a defect.

## 6. Recovery-guarantee rows

The matrix is in [recovery-guarantees.md](../recovery-guarantees.md) section 4. It has 8 rows and 17 phases, which gives
136 cells. The counts below use the crash grid. Each cell counts once. A split cell such as `R·P / F·G` counts under its
first class.

| Class bucket | Cells |
|---|---|
| R (plain, including 4 cells with a second `F` or `G` part for another case) | 52 |
| R (enabled) / F (stock Helm), footnote 1 | 9 |
| R (Docker) / F (Kubernetes), footnote 2 | 6 |
| I (including 3 cells with a second `L` or `G` part) | 19 |
| I/C | 5 |
| C | 1 |
| F | 16 |
| n/a | 22 |
| inherits | 6 |
| Total | 136 |

| Evidence status | Cells |
|---|---|
| `P` PROVEN | 24 |
| `P` first, with a second `G` or `L` case | 7 |
| `P*` PROVEN pre-NATS | 14 |
| `U` IMPLEMENTED-UNPROVEN | 56 |
| `G` GAP | 7 |
| n/a or inherits (no status) | 28 |
| Total | 136 |

Reading the counts: 56 of 108 statused cells are unproven, and 14 more depend on the old transport. Only 24 cells
have a plain `P` with no second case.

## 7. Real-browser evidence

None new. The matrix relies on deployed evidence from earlier work. All of it predates the NATS transport (D3). The
transport was Redis Streams. The images were replaced afterwards.

| Id | Date | Subject |
|---|---|---|
| Chat 566, execution `22c6fe6323359dac9cad7dd768a6f35a` | 2026-09-13 | Worker kill after generated text |
| Chat 567, execution `012a6df6f81faaf9fc728b4f0116e959` | 2026-09-13 | Main stop during a stream |
| Chat 557 | 2026-09 | Browser reconnect after five HTTP 503 answers |
| Chat 632, execution `f17862e94cbd79e0ca7db48a8ce2b900` | 2026-09 | Nested agent, SIGKILL |
| Chat 653, execution `1daa33b2ea1f266183fa6fc0da81e95b` | 2026-09 | Structured pipeline LLM node |
| Chat 682, execution `87d52d54db8e8779095a3533b632e52c` | 2026-09 | Worker kill during a repair request |
| Chat 716 | 2026-09 | Kill after a committed terminal checkpoint |
| Chat 748, executions `c3398f51c5979476585b9293ae609605`, `09560fd3df713a23c48d5370fe8cd56d`, `3f8c9cfac9cb4cbdbbe5d97962aa1645` | 2026-09-28 | Worker restart while paused |
| Conversation 536, execution `be8b2e98f68306fd1fc4291b9f07decb` | 2026-09 | Delegated authorization pause |
| Chat 799, execution `67cf7dc068deb74053f4b616ab20c966` | 2026-10-04 | Combined Main, Worker and Supervisor loss during preparation |
| Execution `a9a19ca888efa52e0f526dacb2b69f6c` (version 155) | 2026-10-04 | Worker kill during a Code job |
| Chat 809 | 2026-10-05 | Main stop during snapshot publication |
| Chat 814 | 2026-10-05 | Platform-brokered Code, happy path |
| Execution `b283d60230377092115caac22be1eb65` | 2026-10-02 | Supervisor-only restart |
| Execution `dd60a24c34234f58427bb42544d4bed5` | 2026-10-02 | Worker restart during hydration |

Open PR 1084 (head `05fa547e`, not on main) adds NATS-era evidence for chats 851, 850, 852 and 843. See "Evidence
pending in open PR 1084" in the matrix document. No result of that PR is counted in the tables above.

## 8. Fixtures

None created.

## 9. Follow-ups

Top ten gaps, in rank order:

1. G-WORKER-01 and G-ADV-02: add the `agentNodeRecovery` Helm key. Change both defaults after ranks 3 and 4 pass.
2. G-WORKER-05 and G-ADV-03: set the Worker grace period above the drain. Drain at a model or tool boundary.
3. G-WORKER-02, -03, -13, -14 and G-ADV-07: make the terminal and pause frames survive a pod replacement.
4. G-NATS-02, G-MAIN-05, G-PG-02, G-SUP-04 and G-ADV-09: prove recovery on the NATS cohort, with PostgreSQL faults, and run the ignored suites in CI.
5. G-WORKER-04, G-WORKER-10 and G-ADV-01: give a typed "may have started" failure. Replay read-only tools. Add effect receipts.
6. G-NATS-01, G-MAIN-02 and G-ADV-05: re-offer and reap claimed rows whose message is lost.
7. G-GW-01 and G-GW-02: retry a model call before the first byte.
8. G-PG-01: retry an AgentState PostgreSQL blip while the lease margin holds.
9. G-MAIN-01 and G-ADV-04: make the pipeline schedule admission key deterministic.
10. G-SUP-01, G-SUP-03 and G-SUP-08: synthesize the Docker receipt from container state. Add an orphan sweeper.

Also: re-run the `P*` evidence on the NATS cohort. A `P*` cell becomes `P` only after that re-run.
