# Platform recovery guarantees

Baseline: `origin/main` `fcf86c31` (2026-10-08). Status: inventory, no code change.

This file is the owner of the platform recovery matrix. Every change that touches a component x phase updates the
row for that cell. The replatform delivery gate requires this update.

Related files:

- [remaining-gates.md](remaining-gates.md) lists the open gates and the continuation order.
- [testing-gaps.md](testing-gaps.md) lists the verification debt. TG-12 covers process replacement and NATS restart.
- [source-mapping/recovery-guarantees-inventory-20261008.md](source-mapping/recovery-guarantees-inventory-20261008.md) records the method and the evidence ruling.

## 1. Customer answer

The target answer is "execution continues from the point of stoppage" (class R). The current answer is narrower.

- With stock Helm values, a Worker loss during a run gives a typed failure (F). It does not give a resume (D1).
- With recovery enabled, the ordinary agent model step resumes (R). The proof ran on Docker before the NATS transport (D3).
- A tool call in flight never repeats. The turn fails (F). Effect receipts do not exist yet.
- A HITL execution at rest, settled output, browser reconnect and admission survive a single process restart. The pause-write window is unproven.
- The Worker output spool is pod-local. A pod replacement loses frames that Main has not acknowledged (D2).
- Recovery can bill the interrupted model request once more (see the gateway section).

| Component | If it goes down | Worst current outcome | Top gap ids |
|---|---|---|---|
| Main | A replica restart keeps the execution. The outbox republishes and the browser resumes from its cursor. | A pipeline schedule runs twice (L). The outbound webhook is lost (L). | G-MAIN-01, G-MAIN-04, G-NATS-01 |
| Worker | Stock Helm: the run fails with a typed failure. Recovery enabled: the model step resumes. | A tool in flight gives F with the generic text "The runtime operation failed." A turn longer than 30 s fails at a rollout. | G-WORKER-01, G-WORKER-05, G-ADV-01 |
| Supervisor | A process restart with the runtime alive resumes (R). | A destroyed sandbox container leaves the row `dispatched` and the Worker fails after `timeout+90` seconds. | G-SUP-01, G-SUP-02, G-SUP-03 |
| NATS | By design, a short outage does not stop a claimed run (unproven). PostgreSQL re-offers a message lost before the claim. | A message lost after the claim leaves the execution `RUNNING` or `PENDING` with no end (L). | G-NATS-01, G-NATS-02 |
| PostgreSQL | No fault test exists. The design fails closed. | An AgentState PostgreSQL blip ends the turn with `DependencyUnavailable` although a checkpoint exists (F). | G-PG-01, G-PG-02 |
| gateway | A restart before the first token gives F. A restart after the first token keeps the partial text (F). | A SIGKILL loses unbilled in-memory usage. A lost budget stream resets counters (L). | G-GW-01, G-GW-02, G-GW-03 |
| browser | A reload or a lost stream resumes from the cursor (R). | A network drop after the POST and before the response leaves no execution id until a reload. | G-WEB-01, G-WEB-02, G-WEB-03 |

## 2. Classes, evidence status and fault modes

Classes (from the replatform delivery gate):

- R (Resume): the run continues from the last durable checkpoint. Completed work does not run again. The user sees one result.
- I (Idempotent retry): the step runs again. A stable identity or receipt makes the effect idempotent.
- C (Reconcile): an external effect has an unknown outcome. The platform never repeats it blindly. It reconciles from receipts or owner proof, or it escalates to an operator.
- F (Typed failure): the step fails with a typed, readable failure and a support reference. Partial results stay.
- L (Lost): work or a result is lost, hangs, repeats an effect or restarts from scratch. L is always a gap.

Evidence status codes in the grid:

| Code | Meaning |
|---|---|
| `P` | PROVEN. A test in CI, or deployed evidence on the current transport. |
| `P*` | PROVEN pre-NATS. Deployed evidence from before `887bfb4d` (2026-10-06), on the Redis transport and an earlier image cohort. The proof depends on the transport. A re-run is required. |
| `U` | IMPLEMENTED-UNPROVEN. Enforcing code exists. No test or evidence covers this fault in this phase. |
| `G` | GAP. No mechanism exists, or the mechanism gives L or a blind repeat. |

Fault modes:

- crash: SIGKILL, OOM kill or node loss. No code runs in the dying process.
- exit: SIGTERM drain or rolling restart.
- malformed: a bad input arrives at the component.

The Worker "crash" fault has two sub-cases (D2). A container restart in the same pod keeps the `emptyDir` spool. A pod
replacement loses the spool.

Phases:

| Id | Phase | Id | Phase |
|---|---|---|---|
| P01 | admission | P10 | Code execution |
| P02 | model call before the first token | P11 | Code publication |
| P03 | model call after the first token | P12 | nested agent or pipeline child |
| P04 | read-only tool | P13 | Parallel or Map child |
| P05 | effectful tool | P14 | output delivery and acknowledgement |
| P06 | static HITL pause and decision | P15 | settlement |
| P07 | ask_user or sensitive-tool approval | P16 | scheduled or triggered run |
| P08 | delegated authorization pause | P17 | indexing |
| P09 | Code preparation | | |

## 3. Cross-cutting rulings

These rulings come from the adversarial review. They apply to every cell.

### D1. Recovery is off by default

- `deploy/helm/elitea/values.yaml:3631-3632` sets `agentModelCheckpointRecovery: false`. The comment says to enable it "after verifying the deployment contract".
- `deploy/helm/elitea/templates/worker/configmap-runtime.yaml:62-64` renders only `agent_model_checkpoint_recovery`. No `agent_node_recovery` key exists in `deploy/helm`.
- Main grants checkpoint recovery only when the claiming Worker sends the flag (`services/elitea-main/internal/infra/db/repos/claims.go:463-476`). Without it the disposition is `RecoverAmbiguousInvocationNoACK` and the run ends in a terminal failure.
- The project gate keeps recovery closed (`services/elitea-worker-rust/docs/remaining-gates.md:488-489`, `services/elitea-worker-rust/docs/testing-gaps.md` TG-12).
- Rule: each Worker-crash R cell in P02, P03, P09-P12 and P14 reads "R (enabled) / F (stock Helm)". Code node recovery on Kubernetes is plain F, because the chart has no key for it.

### D2. The spool is an `emptyDir`

- The spool volume is an `emptyDir` (`deploy/helm/elitea/templates/worker/deployment.yaml:302-304`). The Deployment uses RollingUpdate (`:27-28`). `replicaCount` is 1 (`deploy/helm/elitea/values.yaml:3427`).
- Container restart in the same pod: the spool survives. With one replica the redelivery returns to the same pod. R holds.
- Rollout, eviction, node loss or KEDA scale-down: the new pod has no spool. Frames that Main has not acknowledged are lost (G-WORKER-02, G-WORKER-03).
- More than one replica: a redelivery can reach a pod without the spool. The result is F, even for a plain crash.
- The Worker design document says pod-local storage cannot satisfy recovery (`services/elitea-worker-rust/docs/source-mapping/agent-runtime.md:225-232`).

### D3. All deployed crash evidence on main is pre-NATS

- Commit `887bfb4d` (2026-10-06) deleted the Redis Streams transport. Commit `3d79066a` removed Redis.
- The deployed proofs on main are chats 566 and 567 (2026-09-13), chats 632, 653, 682 and 716 (September), chat 748 (2026-09-28), and executions `67cf7dc0`, `a9a19ca8`, chat 809 and chat 814 (2026-10-04 and 2026-10-05).
- No source-mapping document on main dated 2026-10-06 or later records a kill or a restart. The images were replaced afterwards (`services/elitea-worker-rust/docs/remaining-gates.md:817,855,938`). Open PR 1084 adds NATS-era evidence (see the pending-evidence section).
- Proofs that stay `P` (the transport does not matter): PostgreSQL checkpoint and writer fencing, output inbox idempotency, SSE replay from PostgreSQL, a static pause at rest, the Supervisor-only restart (gRPC), and settlement ordering in PostgreSQL.
- Proofs that become `P*` (the transport matters): takeover after a Worker kill, continuation after a Main kill, a Worker kill during Code, and toolkit terminal rebind (claim 1,815).
- `services/elitea-worker-rust/src/execution/command_delivery.rs` was rewritten in `887bfb4d`. The ack and nak logic of the old runs no longer exists.

### D4. What CI runs

- Rust: the main `cargo test` step sets `ELITEA_TEST_DATABASE_URL`, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` and `ELITEA_REQUIRE_NATS_SECURE_TEST=1` (`.github/workflows/ci-rust.yml:240-245`). The PostgreSQL tests and `services/elitea-worker-rust/src/execution/nats_live_tests.rs` run.
- Rust `--ignored` modules in CI: only `docker_supervisor::deadline_tests`, `::preparation::tests` and `::hydration::deadline_tests` (`.github/workflows/ci-rust.yml:255,274-275`).
- Rust `#[ignore]` suites that CI never runs:
  - `services/elitea-worker-rust/src/sandbox/ledger_code_recovery_tests.rs:38,122,170`
  - `services/elitea-worker-rust/src/sandbox/docker_code_recovery_cleanup_tests.rs:143,202,264`
  - `services/elitea-worker-rust/src/sandbox/ledger_workspace_postgres_tests.rs:82,161`
  - `services/elitea-worker-rust/src/state/postgres_checkpointer_tests.rs:731,865,939,996,1345`
- Go: the PostgreSQL integration tests run in CI. `TestJetStreamCapacityReliability` runs too (`.github/workflows/ci-go.yml:346-404`).
- The Go system test `TestProductionRuntimeCrossProcessSystem` is a declared skip (`scripts/go/declared-skips.txt:47`). It drives the Python worker, covers `configuration.validate` only and has no run record. It is not evidence.

### D5. G-NATS-01 and G-MAIN-02 are one gap

- `ListPendingAgentExecutionIDs` re-offers only rows with `authority_granted_at IS NULL` and a future deadline (`services/elitea-main/internal/db/queries/runtime_agent_execution.sql:394,418-419`).
- The expired reaper requires `authority_granted_at IS NULL` and no claim row (`services/elitea-main/internal/db/queries/runtime_agent_execution.sql:480,488-492`).
- `services/elitea-main/internal/infra/db/repos/claims.go:131` retires only work without authority. No deadline check exists in Main after authority.
- The hang needs a lost message. Examples are NATS stream or PVC loss on one replica, `MaxAge` of 26 h, or a re-created consumer.
- "No live Worker for a long time" is ordinary queueing. It is not a gap.
- Extra window: the claim exists, the Worker died before authority and the message is lost. Neither the re-offer nor the reaper selects the row (G-ADV-05).

## 4. Matrix: crash fault

Cell format: class and status. Footnotes follow the table.

| Component | P01 | P02 | P03 | P04 | P05 | P06 | P07 | P08 | P09 | P10 | P11 | P12 | P13 | P14 | P15 | P16 | P17 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| Main | I·P | R/F¹·U | R/F¹·P* | F·U | F·U ³ | R·U | R·P | I·P | R/F²·P* | R/F²·U | R·P* | R/F¹·U | R·U ⁴ | I·P | R·P ⁵ | I·P / L·G ⁶ | I·P ⁷ |
| Worker (container restart) | I·P | R/F¹·P* | R/F¹·P* | F·P ⁸ | F·G ⁹ | R·P* | R·P | R·P* | R/F²·P* | R/F²·P* | I/C·U | R/F¹·P ¹⁰ | R·U ⁴ | R·P* | R·P | inherits | n/a ¹¹ |
| Worker (pod replacement or >1 replica) | I·P | R/F¹·U | R/F¹·U | F·P ⁸ | F·G ⁹ | R·U ¹² | R·U ¹² | R·U ¹² | R/F²·U | R/F²·U | I/C·U | R/F¹·U | R·U ⁴ | F·G | R·P | inherits | n/a ¹¹ |
| Supervisor | n/a | n/a | n/a | n/a | C·U | n/a | n/a | n/a | R·P / F·G ¹³ | R·P / F·G ¹⁴ | R·U | R·U | R·U | n/a | I·U | inherits | n/a |
| NATS | I·P ¹⁵ | R·U | R·U | R·U | R·U | R·U | R·U | R·U | R·U | R·U | R·U | R·U | R·U | I·P | I·P | inherits | I·U |
| PostgreSQL | I·U | F·G ¹⁶ | F·G ¹⁶ | F·G ¹⁶ | F·U | R·U | R·U | R·U | I/C·U | I/C·U | I/C·U | R·U | R·U | I·U | I·U | inherits | I·U |
| gateway | n/a | F·P ¹⁷ | F·P ¹⁸ | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | F·P | F·U | n/a | I·P / L·G ¹⁹ | inherits | I·P |
| browser | F·G ²⁰ | R·P | R·P | R·U | R·U | R·P / G ²¹ | R·P / G ²¹ | I·P / G ²¹ | R·P* | R·P* | R·P* | R·U | R·U | R·P | R·P* | n/a | R·U |

Footnotes:

1. R when the Worker sets `agent_model_checkpoint_recovery`. F with stock Helm, because `deploy/helm/elitea/values.yaml:3632` is false (D1).
2. R on Docker when `agent_node_recovery` is set. F on Kubernetes, because the chart has no key for it (G-WORKER-01, G-ADV-02).
3. Agent tools give F. The Worker refuses a `ToolMayHaveStarted` marker. Pipeline HTTP-action nodes give C by operator retry, and this is unproven.
4. Fixed Parallel and Map are production-gated. Only in-memory proof exists (G-WORKER-09).
5. Settlement is R. The outbound `AfterSettle` webhook is lost on a crash (L, G-MAIN-04).
6. The index schedule is I·P because the occurrence key is stable. The pipeline schedule is L·G and gives a duplicate run (G-MAIN-01, G-ADV-04).
7. This cell covers dispatch only. The Python index worker runs the whole ingest again after a crash (G-P17-01).
8. A safe read-only tool does not run again. Class I is feasible (G-WORKER-04).
9. No duplicate effect occurs. The user sees the generic "The runtime operation failed." and no "may have started" warning (G-ADV-01, G-WORKER-04, G-WORKER-10).
10. Agent-as-tool child model step: a PostgreSQL component test proves it in CI. Pipeline Application, DirectTool, Hitl and Printer nodes give F (G-WORKER-07).
11. The Rust Worker has no indexing. See the Indexing section.
12. At rest, the pause is in PostgreSQL. In the pause-write window, the unacknowledged `PAUSED_*` terminal is in the `emptyDir` spool and is lost (G-WORKER-14, G-ADV-07).
13. Supervisor process loss gives R. A PostgreSQL component test proves it in CI. The Supervisor-only restart is execution `b283d60230377092115caac22be1eb65`. A destroyed preparer container leaves the row `dispatched`. The Worker gives F after `timeout+90` seconds (G-SUP-01, G-SUP-08).
14. Supervisor process loss with the runtime alive gives R. Only a PostgreSQL component test proves it. An OOM-killed or removed sandbox container on Docker, or node loss on Kubernetes, gives F after `timeout+90` seconds. The row never reaches a terminal state (G-SUP-01, G-SUP-02).
15. Publish idempotency and pre-claim stream loss have live-NATS tests in CI. `TestPostgresNATSServiceBackedVisibilityRepair` covers the validation stream. A message lost after the claim gives L and a hang (G-NATS-01, G-ADV-05).
16. An AgentState PostgreSQL transient error ends the turn with `DependencyUnavailable` although a checkpoint exists (G-PG-01). No PostgreSQL fault test exists anywhere (G-PG-02).
17. The Worker does not retry before the first token (G-GW-01).
18. The gateway keeps the partial text. No resume exists (G-GW-02). A Worker crash while the gateway streams is R/F¹·P* (chat 566).
19. A graceful gateway exit bills once, because `event_id` deduplicates. A SIGKILL loses unbilled in-memory usage. A lost budget stream resets the counters (G-GW-03).
20. A network drop after an accepted POST and before the response leaves the page without an execution id until a reload (G-WEB-01).
21. The server consumes a decision once (PostgreSQL component test). A second tab keeps stale controls (G-WEB-02, TG-04, TG-08).

### Exit (SIGTERM, rolling restart)

| Component | Class and status | Outcome | Gap ids |
|---|---|---|---|
| Main | I/R·U | The drain is 15 s inside a 30 s grace period. There are 2 replicas and `maxUnavailable: 0`. No rolling-restart test under load exists. An open SSE stream ends and the browser resumes from the cursor. | G-MAIN-03 |
| Worker | F (stock Helm) for turns over 30 s. Drain mechanics: P (unit). | The Worker stops intake, nak's unstarted deliveries with zero delay and waits 30 s. A turn longer than 30 s becomes a crash with the outcome in the crash table. That outcome is F on stock Helm. A rollout replaces the pod, so the pod-replacement row applies. | G-WORKER-05, G-ADV-03 |
| Supervisor | R·P for Stop, F for long outages | The Supervisor has one replica with strategy `Recreate`. An outage longer than `timeout+90` seconds gives F. | G-SUP-05 |
| NATS | I·U | The server uses `sync_interval: always` and file storage. A restart and a leader change are unproven. | G-NATS-02 |
| PostgreSQL | I·U | A graceful failover has no test. The design fails closed and redelivery resumes after the database returns. | G-PG-02 |
| gateway | R for streams under 150 s, F for new calls in the gap | The gateway drains for 150 s inside a 180 s grace period. A stream longer than 150 s ends. The gateway bills the accumulated count. | G-GW-03 |
| browser | R·P | A closed tab does not stop the run. A reopened tab replays from the cursor. | G-WEB-02 |

### Malformed input

Every component fails closed with a typed refusal.

| Component | Strongest proof | Exception |
|---|---|---|
| Main | `services/elitea-main/internal/application/agentexecution/start_guardrails_test.go` (typed 4xx, no row). `services/elitea-main/internal/transport/runtimegrpc/output/server_test.go:211` (digest mismatch before ingest). | None. |
| Worker | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:776` (every verification failure is poison). `services/elitea-worker-rust/src/transport/openai_compatible_facade_tests.rs:1254` (SSE shapes fail closed). | A poison command waits up to 24 h and then fails by deadline (G-NATS-04). |
| Supervisor | `services/elitea-worker-rust/src/sandbox/service.rs:901` (grant, peer, scope and signature stay required). `services/elitea-worker-rust/src/sandbox/docker_preparation_tests.rs:1298` (invalid marker). | A malformed Docker receipt behaves like a missing one (G-SUP-01). |
| NATS | `services/elitea-worker-rust/tests/agent_command_contract.rs:221` (digest tamper fails before decode). `services/elitea-main/internal/transport/commandbus/producer_test.go:102` (oversize refused before append). | None. |
| PostgreSQL | `services/elitea-worker-rust/src/agents/direct_hitl_tests.rs:470` (stale or tampered decision refused). `services/elitea-worker-rust/src/agents/graph/direct_tool_tests.rs:1264,1374` (foreign pipeline MCP card is `StaleDecision`; disagreeing action and credentials are `InvalidInput`). A corrupt row gives `CorruptStoredState`. | None. |
| gateway | `services/elitea-worker-rust/src/transport/openai_compatible_facade_tests.rs:1254`. Browser evidence: chats 731 and 732 (streamed error), chat 740 (request too large). | None. |
| browser | `services/elitea-main/internal/api/v2/executions/events_test.go:214` (conflicting cursors refused). | A malformed SSE frame leaves a transcript hole (G-WEB-03). |

## 4a. Evidence pending in open PR 1084

PR 1084 (head `05fa547e`) is not on main. It adds deployed Docker recovery evidence on the NATS transport. The Worker
images are at the "c53" source boundary. The grid above does not change until the PR merges. The source-mapping
documents of the PR are not on main, so this file names them without links.

| Case | Evidence | Source-mapping document |
|---|---|---|
| (a) | Chat 851, pipeline 147 version 170. The original Worker is lost while a JavaScript Code job runs. The replacement uses a fresh empty private spool. Then the Supervisor process is lost. The original job completes once (sandbox lease epoch 4 to 5). Main claim 2, epoch 2, `AGENT_MODEL_CHECKPOINT`. No re-run and no extra job. Stream and consumer pending counts return to zero. Support reference `ca094eeb-04b0-531c-91a1-6b8dc0b657ba`. | `code-live-owner-recovery-20261008.md` |
| (b) | Chat 850, version 176. The Worker is lost during a pending preparation Stop. The spool is fresh. Claim 2, epoch 2 settles `CANCELLED`. No user Code starts. | `code-pending-stop-owner-recovery-20261008.md` |
| (c) | Chat 852, application 145 version 177, execution `76d4865967cadf00a21e30d34261e762`. Main stops while the compiled snapshot is `Publishing`. The same snapshot becomes `Ready`. One compiled execution runs. One `SUCCEEDED` settlement results. | `code-compiled-publication-main-recovery-20261008.md` |
| (d) | Chat 843, execution `692bf01411fc54220c2c47f2fff59f4f`. The original paused Worker is killed. The replacement never opens the old spool. The recorded `Failed.Stop` journal is restored. | `code-failed-stop-owner-recovery-20261007.md` |

Effect on merge:

- Worker (pod replacement) x P10 changes from `U` to `P` (Docker, NATS).
- Supervisor x P10 gets deployed `P` for process loss with a live runtime.
- Main x P11 changes from `P*` to `P`.
- Worker (pod replacement) x P09, Stop path, is `P`.

Cases (a) and (d) show that Code node recovery does not depend on the old spool. D2 therefore limits mainly the agent
model path and unacknowledged terminal or pause frames.

Limits that the PR documents state: no Kubernetes run exists, and the recovery flags are set outside the Helm chart.
D1 stays in force for stock Helm.

## 5. Main

Main means the Go service `services/elitea-main`. All paths below are repo-relative.

| Cell | Fault | Class | Status | Mechanism (path:line) | Proof | Notes |
|---|---|---|---|---|---|---|
| Main x P01 before commit | crash, exit | I | P | One transaction writes job, bundle and outbox row: `services/elitea-main/internal/infra/db/repos/agent_execution_jobs.go:420`. Idempotency scope: `services/elitea-main/internal/application/agentexecution/admission.go:102`. | `TestPostgresAgentMaterializeFailureReleasesReservation` at `services/elitea-main/internal/infra/db/repos/agent_admission_capacity_postgres_integration_test.go:400` | A retry with the same `question_id` returns the same execution. A PostgreSQL crash can drop only a cap reservation. |
| Main x P01 after commit, before publish | crash | I | P | The outbox republisher restarts with Main: `services/elitea-main/internal/application/execution/outbox_publisher.go:100`. The envelope is stored first: `services/elitea-main/internal/application/agentexecution/dispatch.go:111-160`. | `TestPostgresAgentDispatchRetainsExactEnvelopeAcrossBusOutageAndACKLoss` at `services/elitea-main/internal/infra/db/repos/agent_execution_dispatch_postgres_integration_test.go:15` | Adversarial: reclassified from R to I (exact-bytes re-publish). The system-test half is not evidence (D4). |
| Main x P01 after PubAck | crash | I | P | NATS deduplicates by `Nats-Msg-Id` (2 m) and `MaxMsgsPerSubject=1`: `services/elitea-main/internal/transport/commandbus/jetstream.go:269`. | `TestJetStreamCapacityReliability` at `services/elitea-main/internal/transport/commandbus/jetstream_reliability_test.go:28`, `TestPostgresNATSServiceBackedVisibilityRepair` at `services/elitea-main/internal/infra/db/repos/command_visibility_repair_integration_test.go:36` | The visibility-repair test covers the validation stream. The agent route has its own SQL. |
| Main x P01 | exit | I | U | `Runtime.Shutdown` stops publication first, then drains gRPC listeners: `services/elitea-main/internal/runtimecomposition/runtime.go:149-190`. | No rolling-restart test. | G-MAIN-03. |
| Main x P01 | malformed | F | P | `services/elitea-main/internal/application/agentexecution/dispatch.go:46`, `admission.go:82-135`. | `services/elitea-main/internal/application/agentexecution/start_guardrails_test.go`, `services/elitea-main/internal/application/agentexecution/admission_test.go` | Typed 4xx. No row is written. |
| Main x P01 | NATS down | I | P | Admission does not use NATS. The row waits in the outbox. A full stream gives `ErrDispatchBackpressured`. | `TestJetStreamCapacityReliability` | The deadline can expire first. |
| Main x P02 | crash | R/F¹ | U | Main keeps claim, lease and fence in PostgreSQL. A replacement claim gets `RECOVER_AGENT_MODEL_CHECKPOINT`: `services/elitea-main/internal/infra/db/repos/claims.go:461-476`. | Chat 566 is a Worker kill, not a Main fault. | Adversarial: downgraded. The model path runs through the Main `/llm` proxy, so a Main loss also cuts the stream. |
| Main x P03 | crash | R/F¹ | P* | The Worker keeps the command without ack. The next claim returns checkpoint recovery. The browser reconnects with a cursor. | Chat 567, execution `012a6df6f81faaf9fc728b4f0116e959`: Main stopped, attempt and epoch 1 to 2, cursor 177147, one POST, one answer after reload (`services/elitea-worker-rust/docs/source-mapping/agent-crash-continuation.md:416-436`). | Adversarial: downgraded from R. Redis-era proof. The stack had one Main. Helm runs 2 replicas, so the lease-loss path differs. Chat 564 (execution `56740dd1287b219bfb3d2ea32efbde52`) failed and is superseded. |
| Main x P03 | exit | R | U | An open SSE handler holds `http.Server.Shutdown` for 15 s. The durable cursor lets the browser resume: `services/elitea-main/internal/api/v2/executions/events.go:165,233`. | `TestEventHandlerReplaysFromDurableLastEventIDAndNeverAcceptsPayloadFromWaiter` at `services/elitea-main/internal/api/v2/executions/events_test.go:144` | No SIGTERM-while-streaming test. |
| Main x P04 | crash | F | U | If the Main loss causes Worker lease loss, the re-claim meets `ToolMayHaveStarted`. The Worker refuses it: `services/elitea-worker-rust/src/agents/model_checkpoint.rs:283-292`. | `services/elitea-main/internal/infra/db/repos/claims_test.go:1113` (fence handoff only) | Adversarial: downgraded from I. The Worker does not replay a read-only tool (G-WORKER-04). |
| Main x P05 | crash, exit | F | U | Ambiguous invocation gives `RECOVER_AMBIGUOUS_INVOCATION_NOACK`. Operator-gated node recovery exists for pipeline HTTP-action nodes: `services/elitea-main/internal/application/noderecovery/service.go`, `services/elitea-main/internal/infra/db/repos/node_recovery.go:168`. | `services/elitea-main/docs/source-mapping/node-recovery-control-20261004.md` (component tests, not PostgreSQL or runtime acceptance) | Adversarial: downgraded from C. C exists only by design for HTTP-action nodes. No Main-kill evidence exists. |
| Main x P05 | malformed | F | P | Closed typed node-recovery contract (`libs/proto/contracts/node-recovery-v1.md` is added by open PR 1084). | `services/elitea-main/internal/runtimecomposition/node_recovery_identifiers_test.go` | A browser body grants nothing. |
| Main x P06 | crash, exit while paused | R | U | The pause is stored in `chat_message_group.meta`. The continuation is a new admission with key `continue-static/<response>/<sha256(pauseID)>`: `services/elitea-main/internal/application/agentexecution/continue_static.go:218`. | Chat 748 (`HITL_RESTART_20260928`), executions `c3398f51c5979476585b9293ae609605`, `09560fd3df713a23c48d5370fe8cd56d`, `3f8c9cfac9cb4cbdbbe5d97962aa1645` (`services/elitea-worker-rust/docs/source-mapping/pipeline-hitl-admission-20260928.md:162-172`) | The proof is a Worker restart on the Redis transport. No Main restart while paused exists. Main holds no pause state in memory. |
| Main x P06 | competing decisions | I | P | Row-lock consumption: `services/elitea-main/internal/infra/db/repos/agent_execution_jobs.go:803,848`. | `TestPostgresDirectPipelineHITLHistoryCompetingDecisions` at `services/elitea-main/internal/infra/db/repos/agent_pipeline_hitl_history_postgres_integration_test.go:129` | G-ADV-08: decider versus pause owner is unverified. |
| Main x P06 | malformed, stale | F | P | `services/elitea-main/internal/application/agentexecution/continue_static.go:203-216`. | `services/elitea-main/internal/application/agentexecution/continue_static_test.go:37,139` | |
| Main x P07 | crash, exit | R | P | The decision set is consumed atomically in the admission transaction: `services/elitea-main/internal/infra/db/repos/agent_start.go:988`. | `TestPostgresCurrentSequentialNestedHITLContinuationConsumesExistingResponseAtomically` at `services/elitea-main/internal/infra/db/repos/agent_continuation_postgres_integration_test.go:1319` | A Main kill mid-decision is unproven. The transaction commits all or nothing. |
| Main x P08 | crash, exit | I | P | `services/elitea-main/internal/infra/db/repos/agent_execution_jobs.go:960`. Tokens use AES-256-GCM: `services/elitea-main/internal/mcpoauth/tokens.go`. | `services/elitea-main/internal/infra/db/repos/agent_continuation_postgres_integration_test.go:1615` | A crash during the token exchange forces a second click on Authorize. Nothing is lost. |
| Main x P09 | crash | R/F² | P* | Claim renewal and an immutable prepared command: `services/elitea-main/internal/infra/db/repos/command_outbox.go`. | Execution `67cf7dc068deb74053f4b616ab20c966`, chat 799 (`services/elitea-worker-rust/docs/source-mapping/code-native-combined-recovery-20261004.md:33-70`) | Main stopped together with the Worker and the Supervisor, not alone. Docker only. Earlier image cohort. |
| Main x P10 | crash | R/F² | U | Same mechanism as P09. | None for a Main kill during an executing sandbox. | Adversarial: downgraded from PROVEN. Execution `67cf7dc0` injected the fault before the bundle commit (`code-native-combined-recovery-20261004.md:43-47`). The Main-only restart `8c7a6041` happened during hydration (`code-restart-contract-20261002.md:25,30`). |
| Main x P11 | crash | R | P* | The compiled snapshot publish endpoint: `services/elitea-main/internal/infra/storage/compiled_snapshot_http.go`. | Chat 809, pipeline 145 version 162: Main stopped while `Publishing`, same command digest, snapshot Ready (`services/elitea-worker-rust/docs/source-mapping/code-publication-recovery-20261004.md:67-86`) | The document says an in-flight upload was not interrupted. Held narrowly. PR 1084 chat 852 would make this `P`. |
| Main x P12 | crash | R/F¹ | U | Child scopes: `services/elitea-main/internal/runtimecomposition/code_consumers.go:27`. | `services/elitea-worker-rust/docs/source-mapping/nested-pipeline-checkpoint-scope-20260923.md:70` ("not live crash recovery of a successful child") | |
| Main x P13 | crash | R | U | Same as P12. Fan-out V2 is not complete. | `services/elitea-worker-rust/docs/source-mapping/fixed-parallel-integration-20261004.md:39` | G-WORKER-09. |
| Main x P14 | crash after ingest, before ack | I | P | `services/elitea-main/internal/infra/db/repos/output_inbox.go:337-442`. | Proxy unit test `TestOutputACKDropProxyDropsOnlyFirstArmedCommittedACK` at `services/elitea-main/tests/system/runtime_output_ack_drop_proxy_test.go:248` | Adversarial: the system run is CI-skipped. The agent path is unproven. |
| Main x P14 | crash between ingest and projection | R | P | A terminal stays hidden until the projection commits: `services/elitea-main/internal/infra/db/repos/replay_events.go`. | `services/elitea-main/internal/infra/db/repos/replay_events_test.go:36,99` | |
| Main x P14 | browser delivery after a Main crash | R | P | The PostgreSQL replay log and cursor resume. | Chat 567, chat 557 (five HTTP 503 answers, then success at 45.18 s) | Transport-independent. |
| Main x P14 | malformed, stale fence | F | P | `services/elitea-main/internal/infra/db/repos/output_inbox.go:283`. | `services/elitea-main/internal/transport/runtimegrpc/output/server_test.go:211,237,352,385` | |
| Main x P15 | crash around settlement | R | P | `ClaimRecoverTerminalACK` and `ClaimRecoverSettlement`: `services/elitea-main/internal/application/execution/claims.go:54-58`. | `TestCrashAfterTerminalACKRecoversSettlementWithoutInputOrBusinessReplay` at `services/elitea-main/tests/failure/configuration_validation_recovery_test.go:22` | The in-process test and settlement idempotency need only PostgreSQL. |
| Main x P15 | crash after settlement, before `AfterSettle` | L | G | The hook runs after commit. The webhook dispatcher keeps events in memory with 3 attempts: `services/elitea-main/internal/api/webhook/dispatcher.go:17,76`. | `TestAfterSettleHookFailureCannotAffectSettlement` | Settlement is safe. The webhook is lost (G-MAIN-04). |
| Main x PostgreSQL and NATS crash | crash | I | U | Pools reconnect. Claims are authoritative in PostgreSQL. | The system test restarts PostgreSQL and NATS. It is CI-skipped (D4). | Adversarial: downgraded from PROVEN. |
| Main x signing-key rotation | exit | I | U | The prepared envelope stores `prepared_key_id`: `services/elitea-main/internal/runtimecomposition/verification_keyring.go`. | `services/elitea-main/internal/transport/runtimegrpc/control/production_verifier_rotation_test.go` | Not tested with a pending command. |

## 6. Worker

The Worker means `services/elitea-worker-rust`. "Container" means a restart in the same pod. "Pod" means a pod replacement or more than one replica.

| Cell | Fault | Class | Status | Mechanism (path:line) | Proof | Notes |
|---|---|---|---|---|---|---|
| Worker x P01 | crash | I | P | A durable consumer with AckWait 60 s and MaxDeliver -1 (`docs/runtime-command-bus.md:116-128`). Claim with lease epoch: `services/elitea-main/internal/infra/db/repos/claims.go:329-401`. | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:450` (unit). `services/elitea-worker-rust/src/execution/nats_live_tests.rs:292` covers the live transport. Both run in CI. | No two-worker test (G-WORKER-13). |
| Worker x P01 | exit | I | P | Unstarted deliveries get `nak(0)`: `services/elitea-worker-rust/src/execution/command_delivery.rs:793-806`. | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:1200,571,907` (unit, fake transport) | Runs in CI. |
| Worker x P01 | malformed | F | P | Poison reasons: `services/elitea-worker-rust/src/transport/command_bus.rs:309-345`. | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:776,712,745` | The Worker never claims an unverified command. It cannot tell the user. The execution waits for Main (G-NATS-04). |
| Worker x P02 | crash (container) | R/F¹ | P* | `ModelPending` marker and request are written before dispatch with a fenced write: `services/elitea-worker-rust/src/agents/model_checkpoint.rs:23-29,517`. Restore: `services/elitea-worker-rust/src/execution/checkpoint_recovery.rs:140-215`. | Chat 682, execution `87d52d54db8e8779095a3533b632e52c` (`services/elitea-worker-rust/docs/source-mapping/output-continuation-capacity-20260922.md:979-992`). Chat 653, execution `1daa33b2ea1f266183fa6fc0da81e95b`. Unit tests in CI. | Adversarial: downgraded to R (enabled) / F (stock), pre-NATS. A crash before the first marker fails the turn (G-WORKER-06). |
| Worker x P02 | crash (pod) | R/F¹ | U | Same. The spool is not needed before the first token. | None on a second pod. | G-WORKER-13. |
| Worker x P02 | exit | F for turns over 30 s | P for the drain | `services/elitea-worker-rust/src/execution/production.rs:140-166`. | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:571,907` (unit) | Adversarial: downgraded. A call that ends inside 30 s is completion, not recovery. The chart sets no worker `terminationGracePeriodSeconds`. `shutdown_timeout_millis` is 30000 (`deploy/helm/elitea/values.yaml:3669`). |
| Worker x P02 | malformed | F | P | `services/elitea-worker-rust/src/transport/openai_compatible_facade.rs:2143`. A bad checkpoint is refused before dispatch: `services/elitea-worker-rust/src/agents/model_checkpoint.rs:260-285`. | `services/elitea-worker-rust/src/transport/openai_compatible_facade_tests.rs:1254`, `services/elitea-worker-rust/src/agents/model_checkpoint.rs:1063` | No silent coercion. |
| Worker x P03 | crash (container) | R/F¹ | P* | Partial text is not a completed event. A recovered projector emits a non-continuing `agent_start` reset (`services/elitea-worker-rust/docs/source-mapping/agent-crash-continuation.md:340-346`). | Chat 566, execution `22c6fe6323359dac9cad7dd768a6f35a`. Chat 632, execution `f17862e94cbd79e0ca7db48a8ce2b900` (SIGKILL). Chat 716. | Adversarial: downgraded. The browser replacement logic is transport-independent and stays proven (`apps/elitea-web/src/features/chat-messages/lib/chatStreamReducer.test.ts`). |
| Worker x P03 | crash (pod) | R/F¹ | U | Same. | None. | |
| Worker x P03 | malformed | F | P | The terminal cause is stored before the failure frame. The partial text stays. | `services/elitea-worker-rust/src/execution/output_delivery_tests.rs:1892`. Chat 716 (98,090 bytes once, `services/elitea-worker-rust/docs/remaining-gates.md:704-712`). | |
| Worker x P04 | crash | F | P | The restore refuses `ToolMayHaveStarted`: `services/elitea-worker-rust/src/agents/model_checkpoint.rs:283-292,578`. | `services/elitea-worker-rust/src/agents/model_checkpoint.rs:1157`. Execution `80e4eb10605c424dbc11273ef202d8df` (toolkit path, Redis era). | `Tool::is_read_only()` exists at `services/elitea-worker-rust/src/toolkits/direct_execution.rs:184,262`. Recovery does not use it (G-WORKER-04). |
| Worker x P05 | crash | F | G | Same refusal. Effectful direct tools are refused: `services/elitea-worker-rust/src/toolkits/direct_execution.rs:262-265`. | `services/elitea-worker-rust/src/agents/direct_hitl_tests.rs:367`. | The refusal maps to `RuntimeFailureKind::Internal` (`services/elitea-worker-rust/src/execution/native_agent_lifecycle.rs:1456-1457`). The user does not learn that the effect may have happened (G-ADV-01). No effect receipts exist (G-WORKER-10). Pipeline direct toolkit/MCP nodes (PR 1159) now dispatch an effectful tool only after a fenced `Started` append to the node attempt journal and refuse it without a fenced writer: a re-entered attempt never calls the tool again (`services/elitea-worker-rust/src/agents/graph/direct_tool.rs` `DirectToolAttempt`; unit-proven in `services/elitea-worker-rust/src/agents/graph/node_recovery_direct_tool_tests.rs` and against real PostgreSQL in `services/elitea-worker-rust/src/state/postgres_checkpointer_tests/direct_tool_journal.rs`, DB-gated). Run-level recovery of a pending DirectTool node is still F (G-WORKER-07). |
| Worker x P06 | crash, exit | R | P* (container), U (pod) | The pause is a graph checkpoint and a `PAUSED_HITL` terminal: `services/elitea-worker-rust/src/agents/graph/static_pause.rs`. | Chat 748, executions `c3398f51c5979476585b9293ae609605`, `09560fd3df713a23c48d5370fe8cd56d`. 14 static-pause unit tests. | A pause at rest holds no Worker state. The pause-write window is unproven (G-WORKER-14, G-ADV-07). |
| Worker x P07 | crash, exit | R | P | `services/elitea-worker-rust/src/agents/direct_hitl.rs` rebuilds the pending call from session events. | `services/elitea-worker-rust/src/agents/direct_hitl_tests.rs:383,470,734,1133` | An approved effectful call is refused (G-WORKER-10). TG-01, TG-03, TG-04 are open. |
| Worker x P08 | crash, exit | R | P* (container), U (pod) | `services/elitea-worker-rust/src/toolkits/delegated_auth.rs`. | Conversation 536, execution `be8b2e98f68306fd1fc4291b9f07decb` (`services/elitea-worker-rust/docs/source-mapping/delegated-oauth-dcr.md:780-786`) | Transport-independent. TG-06 (active-run expiry) is open. |
| Worker x P08 | decision on a pipeline direct MCP node (Skip/Authorize, also after a sensitive approval of the same call) | R | P (unit, in CI), browser Skip on NATS | Main's `mcp_auth` decision is bound to the pause card, checkpoint and pending node: `services/elitea-worker-rust/src/agents/graph/resume.rs:216`. The consumed approval is tolerated exactly: `services/elitea-worker-rust/src/agents/graph/resume.rs:301`. | `services/elitea-worker-rust/src/agents/graph/direct_tool_tests.rs:1264,1407`. Browser chats 1 and 2, executions `8399c07383aa3339a00c39dad8cc1c7e` and `f095686fd8f2c976f7ffda665b3b5e1f` (`services/elitea-worker-rust/docs/source-mapping/pipeline-mcp-authorization-continuation-20261008.md`) | No crash is injected between the decision and the resumed node. Browser Authorize needs an OAuth-capable stack. Nested-pipeline direct MCP nodes are untested. |
| Worker x P09 | crash | R/F² | P* (container), U (pod) | The original submission is kept in `sandbox_jobs`. A replacement claim observes the original preparer: `services/elitea-worker-rust/src/sandbox/docker_preparation.rs`. | Execution `67cf7dc068deb74053f4b616ab20c966` (8 original dispatches, one result). | Adversarial: Kubernetes is F, because Helm cannot render `agent_node_recovery` (D1). PR 1084 chat 850 would prove the Stop path on a pod replacement. |
| Worker x P10 | crash | R/F² | P* (container), U (pod) | Stable activation id: `services/elitea-worker-rust/src/agents/graph/code_runtime.rs:393-405`. | Execution `a9a19ca888efa52e0f526dacb2b69f6c`, version 155: kill and restart during an 8 s job (`services/elitea-worker-rust/docs/source-mapping/code-source-ui-recovery-acceptance-20261004.md:31,58-66`). | Adversarial: upgraded from C. The owner-proof path (C) stays unproven (G-SUP-06). PR 1084 chat 851 would make the pod row `P`. |
| Worker x P11 | crash | I/C | U | Reconcile by receipt: `services/elitea-worker-rust/docs/source-mapping/code-publication-recovery-20261004.md:12-29`. | Main stop only (chat 809). | A Worker kill during publication has no evidence. |
| Worker x P12 | crash | R/F¹ | P | Child threads are fenced per thread: `services/elitea-worker-rust/src/state/postgres_checkpointer/application_children.rs`. Marker `DelegationPending`. | `services/elitea-worker-rust/src/state/postgres_checkpointer_tests.rs:2365,580`, `services/elitea-worker-rust/src/agents/model_scope_tests.rs:214,248` (all in CI). Chat 632 (pre-NATS). | Pipeline Application, DirectTool, Hitl and Printer nodes give F: `services/elitea-worker-rust/src/agents/graph/compiler.rs:813-830` (G-WORKER-07). Saved children whose exclusive branches converge, loop to the entry node or end twice now run to their result node: `services/elitea-worker-rust/docs/source-mapping/exclusive-branch-fan-in-20261008.md`. |
| Worker x P13 | crash | R | U | Atomic append of child receipts under the writer fence: `services/elitea-worker-rust/src/state/postgres_checkpointer/parallel_append.rs`. | `services/elitea-worker-rust/docs/source-mapping/fixed-parallel-integration-20261004.md:55-64` (in-memory only). | Production-gated (G-WORKER-09). |
| Worker x P14 | crash (container) | R | P* | Frames are encrypted, fsync'd and persisted before publish: `services/elitea-worker-rust/src/spool.rs:249`. | `services/elitea-worker-rust/src/execution/output_delivery_tests.rs:1864,1892,1965,2490,4674`. Chat 716. | Chat 716 used a persistent mount in the same container. |
| Worker x P14 | crash, exit (pod) | F | G | The spool is lost. The model-checkpoint route refuses a completed model event: `services/elitea-worker-rust/src/agents/model_checkpoint.rs:303-315`. | None. | Adversarial: downgraded from R. A rollout always replaces the pod (G-WORKER-02, G-WORKER-03). |
| Worker x P15 | crash, exit | R/I | P | Order: terminal frame, Main ack, settlement receipt, command retirement. Main never re-runs a terminal job: `services/elitea-main/internal/infra/db/repos/claims.go:416-426`. | `services/elitea-worker-rust/src/execution/output_delivery_tests.rs:4186,4360,4132` | Claim 1,815 (execution `15f6d2c70254eb8573094fa4d1cc839e`) is pre-NATS. |

## 7. Supervisor

The Supervisor is the sandbox supervisor (`services/elitea-worker-rust/src/sandbox`) and the runner `services/elitea-code-runner`. It talks to the Worker over mTLS gRPC. It does not use NATS. It runs as one replica with strategy `Recreate` and grace 25 s (`deploy/helm/elitea/templates/sandbox/supervisor.yaml:37-39,52`).

| Cell | Fault | Class | Status | Mechanism (path:line) | Proof | Notes |
|---|---|---|---|---|---|---|
| Supervisor x P05 (brokered platform call) | crash, exit | C | U | The Supervisor retains the original runtime and attests the owner lease: `services/elitea-worker-rust/src/sandbox/ledger_code_platform.rs`. | Unit only. `services/elitea-worker-rust/src/sandbox/ledger_code_platform_not_ready_tests.rs:123`. Chat 814 is a happy path. | No fault was injected during a platform call. |
| Supervisor x P05 | malformed | F | P | Ed25519 reply check: `services/elitea-code-runner/src/code_platform_signature.rs:93`. | `services/elitea-code-runner/src/code_platform_signature.rs:179` | |
| Supervisor x P09 | crash, exit | R | P | The job key is stable: `services/elitea-worker-rust/src/sandbox/code_recovery.rs:343`. `reserve` is idempotent: `services/elitea-worker-rust/src/sandbox/ledger.rs:184`. The replacement observes the original preparer: `services/elitea-worker-rust/src/sandbox/docker_preparation.rs:373-461`. | `services/elitea-worker-rust/src/sandbox/docker_preparation_tests.rs:638,697,913,976` (in CI). Supervisor-only restart, execution `b283d60230377092115caac22be1eb65`. | The restart proof is transport-independent. Kubernetes has no recovery test (G-SUP-04). |
| Supervisor x P09 | crash (preparer destroyed) | F | G | A missing receipt returns an error before the deadline check: `services/elitea-worker-rust/src/sandbox/docker_preparation.rs:421-445`. | `services/elitea-worker-rust/src/sandbox/docker_preparation_tests.rs:869` (no re-create) | The row stays `dispatched`. The Worker gives F after `timeout+90` (G-SUP-01, G-SUP-08). |
| Supervisor x P09 | malformed | F | P | `services/elitea-worker-rust/src/sandbox/docker_preparation.rs:394-415`. | `services/elitea-worker-rust/src/sandbox/docker_preparation_tests.rs:1298`, `services/elitea-worker-rust/src/sandbox/peer_identity.rs:89` | |
| Supervisor x P10 | crash, exit (runtime alive) | R | P (PostgreSQL component only) | Dispatch intent is written before the signal: `services/elitea-worker-rust/src/sandbox/ledger.rs:478,501`, `services/elitea-worker-rust/src/sandbox/docker_supervisor.rs:480-486`. Replacement claims the lease with epoch +1: `services/elitea-worker-rust/src/sandbox/ledger.rs:397-420`. | `services/elitea-worker-rust/src/sandbox/docker_deadline_tests.rs:325,434` (fake runtime, CI at `.github/workflows/ci-rust.yml:255`) | Adversarial: the Docker attribution of `b283d602` is wrong. That restart happened during hydration (P09). No deployed Supervisor kill during execution exists on main. PR 1084 chat 851 would add one. |
| Supervisor x P10 | crash (container killed or OOM, Docker) | F | G | The Docker receipt reader needs a JSON envelope in the logs: `libs/rust/vendor/adk-sandbox/src/workspace/docker_code_jobs.rs:371-405`. The error comes before the deadline check: `services/elitea-worker-rust/src/sandbox/docker_supervisor.rs:495-514`. | None. Kubernetes has the fix: `services/elitea-worker-rust/src/sandbox/kubernetes/client.rs:536`. | G-SUP-01. |
| Supervisor x P10 | crash (node loss, Kubernetes) | F | G | A receipt needs a kubelet terminated state: `services/elitea-worker-rust/src/sandbox/kubernetes/mod.rs:151-185`. | None. | G-SUP-02. Conservative by design. |
| Supervisor x P10 | crash (not yet dispatched) | I | P | Expired inert allocations are swept: `services/elitea-worker-rust/src/sandbox/docker_supervisor.rs:349-391`. | `services/elitea-worker-rust/src/sandbox/docker_hydration_deadline_tests.rs:522,769` (in CI) | The no-effect seal is unproven: its tests are `#[ignore]` (G-ADV-09). |
| Supervisor x P10 | crash (Worker dies) | R | P* | A stable activation gives the same job key. `reserve` returns the existing row. | Execution `a9a19ca888efa52e0f526dacb2b69f6c`. Execution `dd60a24c34234f58427bb42544d4bed5` (Worker restart during hydration). Unit: `services/elitea-worker-rust/src/agents/graph/code_runtime.rs:771`. | Redelivery needs a re-claim (D3). |
| Supervisor x P10 | exit (Stop) | R | P | `services/elitea-worker-rust/src/sandbox/ledger.rs:301,427`. Terminate is confirmed before the `Cancelled` receipt. | `services/elitea-worker-rust/src/sandbox/docker_deadline_tests.rs:483`, `services/elitea-worker-rust/src/execution/output_delivery_tests.rs:3531` | A Stop reaches the Supervisor only for `CANCELLED` terminals (G-SUP-03). |
| Supervisor x P10 | malformed | F | P | `services/elitea-worker-rust/src/sandbox/service.rs:353-403`. | `services/elitea-worker-rust/src/sandbox/service.rs:901,751,801` | A malformed Docker receipt behaves like a missing one (G-SUP-01). |
| Supervisor x P11 | crash, exit | R | U | The compiler is held until `Release`: `services/elitea-code-runner/src/compiled_snapshot.rs`. `services/elitea-worker-rust/src/sandbox/docker_compiled.rs:826`. | Main stop only (chat 809). `services/elitea-worker-rust/src/sandbox/docker_preparation_tests.rs:976`. | A Supervisor restart during publication is unproven. Kubernetes publication is unproven. |
| Supervisor x P12, P13 | crash, exit | R | U | Same job-key machinery. | None for Code in a child or in fan-out (G-SUP-07). | |
| Supervisor x P15 | crash, exit | I | U | The terminal receipt comes first. `cleanup_pending` is retried on the next reconcile: `services/elitea-worker-rust/src/sandbox/docker_supervisor.rs:731-760`. | `services/elitea-worker-rust/src/sandbox/ledger_workspace_postgres_tests.rs:83` is `#[ignore]`. | Adversarial: downgraded from PROVEN. |
| Supervisor x Worker outage | crash, exit | R then F | P* (short), U (long) | The Worker retries with the same request until `timeout+90`: `services/elitea-worker-rust/src/agents/graph/code_remote.rs:358-433,460-462`. | Execution `b283d60230377092115caac22be1eb65`. | An outage longer than the window gives F (G-SUP-05). |
| Supervisor x owner proof | exit | C | U | `services/elitea-worker-rust/src/sandbox/ledger_code_recovery.rs:93-196`. | `services/elitea-worker-rust/src/agents/graph/node_recovery_code_owner_tests.rs:328` (unit). The PostgreSQL suites are `#[ignore]`. | G-SUP-06. |
| Supervisor x orphans | crash | L | G | No sweeper exists for `dispatched` rows without a cancel. | None. | G-SUP-03. |

## 8. NATS

NATS is the JetStream command bus. It is an at-least-once wake-up bus. PostgreSQL is the authority (`docs/runtime-command-bus.md:25-38`).

| Cell | Fault | Class | Status | Mechanism (path:line) | Proof | Notes |
|---|---|---|---|---|---|---|
| NATS x P01 publish | crash, leader change | I | P | The outbox stores the signed bytes first: `services/elitea-main/internal/application/agentexecution/dispatch.go:112-165`. | `services/elitea-main/internal/transport/commandbus/jetstream_secured_test.go:29` | A server restart is unproven (G-NATS-02). |
| NATS x P01 | message lost before claim | I | P | The PostgreSQL re-offer selects rows with `authority_granted_at IS NULL`: `services/elitea-main/internal/db/queries/runtime_agent_execution.sql:394,418-419`. | `TestPostgresNATSServiceBackedVisibilityRepair` (validation stream, CI) | Adversarial: upgraded from U. The agent route has its own SQL. |
| NATS x P01 | message lost after claim | L | G | No re-offer for a claimed row. | None. | G-NATS-01, G-ADV-05. |
| NATS x P01 | malformed | F | P | Producer bound: `services/elitea-main/internal/transport/commandbus/producer_test.go:102`. `MaxMsgSize 65536`. | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:613,776` | |
| NATS x P02-P13 | crash, exit | R | U | A claimed run does not depend on NATS. Lease renewal is gRPC to Main: `services/elitea-worker-rust/src/execution/agent_lease.rs`. | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:874`, `services/elitea-worker-rust/src/execution/nats_live_tests.rs` (+WPI over 66 s) | Adversarial: held as U. Unverified: a stale-delivery ack after a server restart. The run is unaffected only if the server returns within AckWait. |
| NATS x P14, P15 | crash, exit | I | P | The ack comes after the terminal receipt. A double ack is idempotent. | `services/elitea-worker-rust/src/execution/nats_live_tests.rs:292` | |
| NATS x consumer | deleted, drifted | F then manual R | P (stop), U (repair) | `services/elitea-worker-rust/src/transport/nats_jetstream.rs:766` refuses drift. | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:539` | No periodic reconcile (G-NATS-03). |
| NATS x poison | malformed | F | P | Signature and subject failures: dead-letter and `Term`. Decode and unsupported failures: `nak(24h)`. | `services/elitea-worker-rust/src/execution/command_delivery_tests.rs:712,745,776` | The wait is up to 24 h (G-NATS-04). |
| NATS x P17 | crash | I | U | Stream `ELITEA_RT_V1_INDEX`. | `services/elitea-main/internal/infra/db/repos/index_v2_cutover_postgres_integration_test.go:18` | G-P17-01. |

Stream and consumer shape: see section 14.

## 9. PostgreSQL

Main owns `elitea_runtime.*`. The Worker owns the separate AgentState database. No effect spans both databases in one transaction. Claim and fence tokens plus receipts make the effects safe.

| Cell | Fault | Class | Status | Mechanism (path:line) | Proof | Notes |
|---|---|---|---|---|---|---|
| PostgreSQL x P01 | crash, failover | I | U | One transaction writes job and outbox: `services/elitea-main/internal/infra/db/repos/agent_execution_jobs.go:107-140`. | `services/elitea-main/internal/infra/db/repos/execution_admission_postgres_integration_test.go:20,129` | Main has no bounded retry of 40P01 and 40001. The caller sees a 5xx. |
| PostgreSQL x P02, P03, P04 | outage | F | G | `StorageUnavailable` maps to `DependencyUnavailable`: `services/elitea-worker-rust/src/agents/session.rs:3052-3100`. | Classification unit tests: `services/elitea-worker-rust/src/state/postgres_checkpointer.rs:1698-1723`. No fault test. | G-PG-01. The retryable flag exists and nothing uses it. |
| PostgreSQL x P05 | outage between effect and receipt | F | U | The marker flips to `tool_may_have_started` before dispatch. | Doc and unit only. | No blind repeat. |
| PostgreSQL x P06, P07, P08 | crash, failover | R | U | The pause is stored under `FOR SHARE` and `FOR UPDATE OF writer`: `services/elitea-worker-rust/src/state/postgres_checkpointer.rs:1140-1165`. | `services/elitea-worker-rust/src/state/postgres_checkpointer_tests.rs:2365`. No PostgreSQL fault test. | |
| PostgreSQL x P09-P11 | crash, failover | I/C | U | Receipts are fenced: `services/elitea-worker-rust/src/state/postgres_checkpointer_tests.rs:866,1001`. | The `#[ignore]` suites never run in CI (D4). | G-ADV-09. |
| PostgreSQL x P12, P13 | crash, failover | R | U | `services/elitea-worker-rust/src/state/postgres_checkpointer/application_children.rs`. | `services/elitea-worker-rust/src/state/postgres_checkpointer_tests.rs:580,2365` | |
| PostgreSQL x P14, P15 | crash, failover | I | U | `services/elitea-main/internal/infra/db/repos/output_inbox.go`, `services/elitea-main/internal/infra/db/repos/settlements.go`. | `services/elitea-worker-rust/src/execution/output_delivery_tests.rs` (unit) | |
| PostgreSQL x P17 | crash, failover | I | U | Index meta recovery: `services/elitea-main/internal/infra/db/repos/index_ingest_jobs.go:297-580`. | `services/elitea-main/internal/infra/db/repos/index_meta_initializer_postgres_integration_test.go:36` | |
| PostgreSQL x any | serialization failure, deadlock | F | U | The Worker classifies 40001 and 40P01 as `StorageUnavailable`: `services/elitea-worker-rust/src/state/postgres_checkpointer.rs:1435-1451`. | Classification unit only. | No bounded retry anywhere. |
| PostgreSQL x any | corrupt row | F | P | `CorruptStoredState`. | `services/elitea-worker-rust/src/agents/application_pipeline_tests.rs:800` | |

## 10. gateway

The gateway is `services/elitea-llm-gateway`. A model call is read-only at the platform. A retried call is a new billed request. Recovery can bill the interrupted model request once more. The `event_id` deduplication prevents double count of the same request.

| Cell | Fault | Class | Status | Mechanism (path:line) | Proof | Notes |
|---|---|---|---|---|---|---|
| gateway x P02 | crash, 5xx, 429, timeout, malformed stream | F | P | The Worker classifies failures: `services/elitea-worker-rust/src/execution/native_agent_lifecycle.rs:1485-1543`. No retry exists. | Browser evidence: chats 699, 697, 696, 729, 730, 733, 698, 731, 732, 702, 703, 740 (`services/elitea-worker-rust/docs/source-mapping/point4-model-failure-acceptance-20260928.md`). | G-GW-01. The call is safe to retry before the first token. |
| gateway x P02 | exit (rolling restart) | R/F | P (billing), U (end to end) | Shutdown: drain 150 s inside grace 180 s. | `services/elitea-llm-gateway/internal/llmproxy/stream_disconnect_test.go:572,615,1022,1068,1233` | A stream longer than 150 s ends. |
| gateway x P02 | NATS down | F | P | Budget plane fails closed with 503 `nats_unavailable`. | `services/elitea-llm-gateway/internal/failmode/sweep_test.go:124` | Rate limits fail open. |
| gateway x P03 | crash, provider drop, malformed stream | F | P | Idle timeout and stream errors give a typed terminal. Partial output stays. | Chat 699, chats 731 and 732. | G-GW-02. No resume after a partial answer. |
| gateway x P03 | Worker crash while streaming | R/F¹ | P* | The gateway drains the provider stream and bills the accumulated count. | Chat 566, execution `22c6fe6323359dac9cad7dd768a6f35a`. | Adversarial: downgraded from PROVEN. The provider bills twice. |
| gateway x P12 | provider fault in a child | F | P | A child failure does not end the parent. | Chats 727, 728, 668. | |
| gateway x P13 | 429 burst from fan-out | F | U | No Worker backoff. | None. | |
| gateway x P15 | crash between response and billing | L | G | Billing drain: `services/elitea-llm-gateway/internal/llmproxy/budget_gate.go:958`. | `services/elitea-llm-gateway/internal/failmode/recovery_postgres_integration_test.go:264` (graceful) | G-GW-03. SIGKILL loses unbilled usage. A lost budget stream resets the counters. |
| gateway x P17 | embedding calls | I | P | Bounded retry in `libs/rust/model-client/src/transport.rs:246-310`. | `libs/rust/model-client/tests/llm_client.rs:582,643` | Engines only. |

## 11. browser

The browser (`apps/elitea-web`) owns no durable execution state. Recovery means re-observing a PostgreSQL-durable execution.

| Cell | Fault | Class | Status | Mechanism (path:line) | Proof | Notes |
|---|---|---|---|---|---|---|
| browser x P01 | network drop after POST | F | G | Reload reattaches: `apps/elitea-web/src/features/chat-messages/model/usePendingReplay.ts:55`. | `apps/elitea-web/e2e/streaming/chat.reload-mid-turn.spec.ts` (no run record) | G-WEB-01. No automatic same-`question_id` retry. |
| browser x P02, P03 | Main down, SSE 503, drop | R | P | The stream reopens with `?cursor=` and retries every 30 s: `apps/elitea-web/src/shared/api/sse/resume.ts`, `useChatStreamConnection.ts:149-175`. | `useChatStreamConnection.test.ts:119,147,184,213,234`. Chat 557, chat 567. | PostgreSQL replay log. Transport-independent. |
| browser x P03 | malformed SSE frame | F | P | The parser drops the frame and advances the cursor: `executionEvents.ts:104-118,212-215`. | `apps/elitea-web/src/shared/api/sse/executionEvents.test.tsx` | G-WEB-03. A transcript hole. The server never emits such a frame. |
| browser x P04, P05 | tab closed | R | U | The run continues on the server. A reopened tab replays. | None. | |
| browser x P06, P07 | reload, second tab | R/G | P (server), G (second tab) | The server consumes the decision once. A second tab keeps stale controls. | `apps/elitea-web/e2e/streaming/chat.hitl.spec.ts:118` (single tab) | G-WEB-02, TG-04, TG-08. |
| browser x P08 | popup closed, second tab | I/G | P (server), G (second tab) | Conversation 533: stale guard controls (`services/elitea-worker-rust/docs/source-mapping/delegated-oauth-dcr.md:1073-1100`). | Emulator proofs only. | TG-08. |
| browser x P09-P11, P15 | reload after settlement | R | P* | The settled answer comes from product storage. | Chats 566, 567, 809. | |
| browser x P12, P13 | tab loss | R | U | Child events stay out of the parent answer. | Reducer unit tests. | |
| browser x P14 | recovered stream | R | P | A non-continuing `agent_start` replaces the interrupted text. | `apps/elitea-web/src/features/chat-messages/lib/chatStreamReducer.test.ts` (105 focused tests) | |
| browser x P17 | tab closed during index run | R | U | `apps/elitea-web/src/features/toolkits/indexes/lib/helpers/indexExecution.helpers.ts` reattach. | `apps/elitea-web/e2e/streaming/index.streaming.spec.ts` | |

## 12. Scheduled and triggered runs (P16)

`services/elitea-scheduler` is retired for dispatch (`services/elitea-scheduler/RETIREMENT.md`). Main owns two run-producing schedules, `index.schedule.scan.v1` and `pipeline.schedule.scan.v1`. Both run on `services/elitea-main/internal/application/scheduling/runner.go:25`. A scheduled run is an ordinary agent admission. Every other component follows its P01-P15 cells.

| Cell | Fault | Class | Status | Mechanism (path:line) | Proof | Notes |
|---|---|---|---|---|---|---|
| Main x P16 index schedule | crash at fire time | I | P | Key `sha256(job, revision, occurrence)`: `services/elitea-main/internal/application/indexschedule/runner.go:336-351`. `occurrence = cron.Next(last_run)`: `services/elitea-main/internal/application/indexschedule/due.go:43`. | `services/elitea-main/internal/infra/db/repos/schedule_occurrences_postgres_integration_test.go:16`, `services/elitea-main/internal/application/scheduling/runner_test.go:225,315` | A schedule edit between the crash and the retry gives one duplicate. |
| Main x P16 index schedule | exit | I | U | Bounded settlement context. | `services/elitea-main/internal/application/scheduling/runner_test.go:284` | |
| Main x P16 pipeline schedule | crash, timeout or stamp error | L | G | `QuestionID: uuid.NewString()`: `services/elitea-main/internal/api/v2/pipelinetriggers/run.go:331`. A stamp failure is only logged: `services/elitea-main/internal/api/v2/pipelinetriggers/schedulerun.go:205-209`, `services/elitea-main/internal/api/v2/pipelinetriggers/job.go:51`. | `services/elitea-main/internal/api/v2/pipelinetriggers/pipelinetriggers_postgres_integration_test.go:895` (happy path only) | G-MAIN-01, G-ADV-04. A duplicate run needs no crash. `previousRunActive` masks it only while the first run streams. |
| Main x P16 pipeline schedule | Main down at fire time | R | U | The schedule fires once on return: `services/elitea-main/internal/api/v2/pipelinetriggers/schedulerun.go:191-200`. | `services/elitea-main/internal/api/v2/pipelinetriggers/pipelinetriggers_postgres_integration_test.go:1078,1119` | Missed fires collapse to one. |
| Main x P16 inbound webhook | sender retry | I | P | Dedupe on signed `webhook-id`, retention 72 h: `services/elitea-main/internal/api/v2/pipelinetriggers/deliveries.go`. | `services/elitea-main/internal/api/v2/pipelinetriggers/deliveries_postgres_integration_test.go` | A bearer-secret trigger has no dedupe (G-MAIN-07). |
| Main x P16 | malformed cron | F | P | `services/elitea-main/internal/api/v2/pipelinetriggers/pipelinetriggers_postgres_integration_test.go:845`. | | |
| Other components x P16 | any | inherits | | `services/elitea-main/internal/domain/execution/trigger_origin.go` | `services/elitea-main/internal/domain/execution/trigger_origin_test.go` | |

## 13. Indexing (P17)

Three indexing planes exist. The Python index worker (`services/elitea-worker-python/src/elitea_worker/handlers/indexing.py`) runs the index route. The Rust indexing worker is "Planned". The engines (inventory, deepwiki) run behind `elitea-subapp-host`.

| Cell | Fault | Class | Status | Mechanism (path:line) | Proof | Notes |
|---|---|---|---|---|---|---|
| Main x P17 dispatch | crash | I | P | `services/elitea-main/internal/application/indexing/dispatch.go:226`. | `services/elitea-main/internal/infra/db/repos/index_ingest_dispatch_postgres_integration_test.go:26` | Crash during the Index v2 cutover is untested (G-MAIN-08). |
| Python index worker | crash, exit | I (full re-run) | U | Redelivery after AckWait. The handler makes no exactly-once claim: `services/elitea-worker-python/src/elitea_worker/handlers/indexing.py:634-639`. | Unit and service tests only. | G-P17-01. A crash at 90 percent restarts at 0 percent. |
| Rust indexing worker | any | n/a | G | `services/elitea-worker-rust/docs/source-mapping/indexing.md` lists all targets as "Planned". | None. | G-P17-02. |
| inventory engine | crash mid-ingest | F run, R data | P (lease) | A PostgreSQL advisory lock frees on process death: `services/elitea-inventory-engine/src/store/sources.rs:252-292`. | `services/elitea-inventory-engine/tests/ingest.rs:382,466,503` | The source status stays `in_progress` (G-P17-03). |
| subapp-host | restart | F | P | `Reconcile` ends orphan invocations: `services/elitea-subapp-host/internal/spi/pgstore.go:185-217`. | `services/elitea-subapp-host/internal/spi/pgstore_test.go:134` | `Reconcile` ends live peers too (G-P17-04). |
| deepwiki engine | crash during build | R live index, F run | P | Staged build, atomic publish, stale sweep after 2 h. | `services/elitea-deepwiki-engine/tests/storage_reconcile.rs:38,85`, `services/elitea-deepwiki-engine/tests/storage_publish_control.rs:60,136,261` | The live wiki is never half-replaced. |

## 14. Durable mechanisms

| Primitive | Owner (path:line) | Guarantee |
|---|---|---|
| Claim, lease and fence | `services/elitea-main/internal/infra/db/repos/claims.go:329-401` | One live claim per execution. A replacement claim has a higher attempt and a new token. TTL 30 s. |
| Claim dispositions | `services/elitea-main/internal/infra/db/repos/claims.go:419-476` | `RecoverSettlement`, `RecoverTerminalACK`, `RecoverAmbiguousInvocationNoACK`, `RecoverAgentModelCheckpoint`. |
| Writer fence in agentstate | `services/elitea-worker-rust/src/state/postgres_session.rs:1655-1690`, `services/elitea-worker-rust/src/state/postgres_checkpointer.rs:1143-1162` | A stale writer cannot write events, checkpoints, markers or the journal. |
| Model-boundary marker | `services/elitea-worker-rust/src/agents/model_checkpoint.rs:19-29` | Written before each model request and tool dispatch. |
| Outbox and exact signed envelope | `services/elitea-main/internal/application/agentexecution/dispatch.go:111-160` | A retry sends identical bytes. |
| Output inbox | `services/elitea-main/internal/infra/db/repos/output_inbox.go:337-442` | Idempotent insert with contiguity. The ack follows the commit. |
| Settlement order | `services/elitea-worker-rust/src/execution/output_delivery_tests.rs:4186,4360` | Terminal frame, Main ack, settlement receipt, command retirement. |
| Output spool | `services/elitea-worker-rust/src/spool.rs:249` | Encrypted, fsync'd, flock'd per replica. Pod-local (D2). |
| Toolkit invocation fence | `services/elitea-worker-rust/src/protocol/toolkit_invocation.rs` | `AUTHORIZED_NOW` is required before a provider call. |
| Node attempt journal | `services/elitea-worker-rust/src/agents/graph/node_recovery.rs:80-100,303-398` | Started before dispatch. No blind retry of an unknown effect. Code nodes only. |
| Sandbox receipt ledger and dispatch journal | `services/elitea-worker-rust/src/sandbox/ledger.rs:184,397-420,478,501,511`, `services/elitea-worker-rust/src/sandbox/dispatch.rs:202,233` | Intent before effect, fenced by owner, epoch and lease. The sandbox never re-runs after dispatch. |
| NATS stream and consumer | `docs/runtime-command-bus.md:58-90`, `deploy/helm/nats-bootstrap/files/bootstrap.sh:397-470` | WorkQueue, file storage, `MaxMsgSize 65536`, `MaxAge` deadline plus 2 h, Duplicates 2 m. Consumer: explicit ack, AckWait 60 s, MaxDeliver -1, MaxAckPending 1024. |
| Dead-letter KV | `services/elitea-worker-rust/src/execution/command_delivery.rs:895-940` | Poison records with a 7 day TTL. |
| Replay log | `services/elitea-main/internal/infra/db/repos/replay_events.go:166` | Cursor resume and a reset event for a pruned cursor. |
| Scheduler occurrence ledger | `services/elitea-main/internal/infra/db/repos/schedule_occurrences.go:96,227` | Lease epoch and `ErrStaleFence`. |
| Gateway `event_id` dedup | `services/elitea-llm-gateway/internal/failmode/store.go:166,379-395` | `Nats-Msg-Id` and `ON CONFLICT (event_id) DO NOTHING`. |
| Engine leases | `services/elitea-inventory-engine/src/store/sources.rs:252-292` | PostgreSQL advisory lock freed on process death. |

Stream and consumer shape: the streams are `ELITEA_RT_V1_VALIDATE`,
`ELITEA_RT_V1_AGENT` and `ELITEA_RT_V1_INDEX`. They use `WorkQueue` retention, `Discard=New`, `MaxMsgsPerSubject=1`,
`MaxMsgs` up to 1024 and `MaxBytes` 64 MiB. Replicas are 1 (scale-1) or 3 (HA).

## 15. Ranked gap backlog

Size: S is a small change, M is a medium change, L is a large change.

| Rank | Id(s) | Scenario | Current outcome (class) | Proposed fix (file or mechanism) | Size | Impact |
|---|---|---|---|---|---|---|
| 1 | G-WORKER-01, G-ADV-02 | A stock Helm install loses a Worker during P02, P03 or P09-P12. | Generic failure (F). Code node recovery and platform-brokered Code cannot be configured on Kubernetes (`services/elitea-worker-rust/src/config.rs:338-341`). | Add `worker.runtime.agentNodeRecovery` to `deploy/helm/elitea/templates/worker/configmap-runtime.yaml`. Assert it in `deploy/helm/tests/render-worker-sandbox.sh`. Change both defaults after ranks 3 and 4 pass. | S (keys), gated by 3 and 4 | high |
| 2 | G-WORKER-05, G-ADV-03 | A rollout or a KEDA scale-down hits a turn longer than 30 s. | The grace period (30 s) equals the drain (30 s). The turn fails (F), possibly during an effect. | Set `terminationGracePeriodSeconds` above the drain in `deploy/helm/elitea/templates/worker/deployment.yaml`. Drain at the next model or tool boundary and release the claim (`services/elitea-worker-rust/src/execution/production.rs:140-166`). | S + M | high |
| 3 | G-WORKER-02, G-WORKER-03, G-WORKER-13, G-WORKER-14, G-ADV-07 | A pod is replaced or more than one replica runs. | Unacknowledged terminal or pause frames are lost. A finished answer in agentstate is refused (`services/elitea-worker-rust/src/agents/model_checkpoint.rs:303-315`). F or a lost HITL card. | Write a `TerminalPending` marker with the final event. Rebuild the terminal from PostgreSQL (`replace_pending_agent_terminal_recovery`). Prove with two pods and no spool. | M-L | high |
| 4 | G-NATS-02, G-MAIN-05, G-PG-02, G-SUP-04, G-ADV-09 | A customer asks for proof on the NATS cohort, PostgreSQL faults or Kubernetes. | All deployed proof on main is pre-NATS. No PostgreSQL fault, Kubernetes or leader-change test. Key PostgreSQL suites never run. | Re-run chats 566, 567, 682, 716, 748 and executions `67cf7dc0`, `a9a19ca8` on the NATS cohort. Rehearse a CNPG switchover. Add the `#[ignore]` modules to `.github/workflows/ci-rust.yml` next to lines 255 and 274-275. | M-L | high |
| 5 | G-WORKER-04, G-WORKER-10, G-ADV-01 | A crash happens with a tool in flight. | "The runtime operation failed." The effect uncertainty is hidden. The user sends again (duplicate effect, L by proxy). | (S) A typed `RuntimeFailureKind::RecoveryRefused` with "may have started" text and a support reference (`services/elitea-worker-rust/src/protocol/output.rs:882-885`). (M) Replay read-only tools. (L) Effect receipts. | S / M / L | high |
| 6 | G-NATS-01, G-MAIN-02, G-ADV-05 | A bus message is lost after the claim (PVC loss, `MaxAge`, consumer re-create). | The execution stays `RUNNING` or `PENDING` (L). | Re-offer rows with an expired lease in `services/elitea-main/internal/db/queries/runtime_agent_execution.sql`. Add a Main reaper that settles `WORKER_LOST` or deadline F for claimed rows past the deadline. | M | high |
| 7 | G-GW-01, G-GW-02 | A gateway restart or a provider 5xx, 429 or EOF occurs before the first token. | Immediate failure (F). The user asks again. | A bounded pre-first-byte retry that honours `retry-after`. Reuse `libs/rust/model-client/src/transport.rs:246-310`. | S-M | high |
| 8 | G-PG-01 | An AgentState PostgreSQL blip or failover (57P01, pool timeout) occurs mid-turn. | Terminal `DependencyUnavailable` although a checkpoint exists. | A bounded retry while the lease margin holds (`services/elitea-worker-rust/src/agents/session.rs:3052-3100`). On exhaustion, retain the delivery without a terminal. | M | high |
| 9 | G-MAIN-01, G-ADV-04 | A pipeline schedule crashes, times out or fails to stamp after admission. | The same occurrence runs twice (L). | `QuestionID = H(schedule_id, next(last_run))` and a deterministic conversation id in `services/elitea-main/internal/api/v2/pipelinetriggers/run.go:331`. Or stamp inside the admission transaction. | S-M | high |
| 10 | G-SUP-01, G-SUP-03, G-SUP-08 | A Docker sandbox or preparer is killed (OOM, `docker rm`, daemon restart). | No receipt. The row stays `dispatched` (`services/elitea-worker-rust/src/sandbox/docker_supervisor.rs:496-506`). The Worker gives F after `timeout+90`. Containers leak. | Synthesize the receipt from container state (OOMKilled, ExitCode) as Kubernetes does. Move the deadline check before the error. Add an orphan sweeper. | M | high |
| 11 | G-WORKER-07 | A crash happens while a pipeline Application, DirectTool, Hitl or Printer node is pending. | F (`services/elitea-worker-rust/src/agents/graph/compiler.rs:813-830`). | Extend the frontier and the attempt journal. Add a crash acceptance test for each node family. | L | high |
| 12 | G-SUP-06 | A Worker times out and the sandbox completes later. | The owner-proof path is unrun (C is unproven). | Run the ignored suites in CI. Run a deployed owner-read test. | S-M | high |
| 13 | G-MAIN-03 | A rolling restart of 2 Main replicas runs under load. | Unproven. | A system test that restarts the replicas one at a time during a streaming turn. | M | high |
| 14 | G-MAIN-06 | Main is killed alone during P05, P12 or P13. | Unproven. | A deployed rehearsal with an effect-receipt fixture. | M | high |
| 15 | G-NATS-04, G-WORKER-11 | A command is undecodable or unsupported in a mixed fleet. | The command waits 24 h, then fails by deadline (`services/elitea-main/internal/runtimecomposition/composition.go:82`). | Use a short bounded nak for a version mismatch. Reserve the 24 h park for undecodable bytes (`services/elitea-worker-rust/src/execution/command_delivery.rs:895-940`). | M | med |
| 16 | G-P17-01 | The Python index worker dies at 90 percent of an ingest. | The ingest restarts from zero (I, full re-run). | Chunk-level progress in Main index meta, or prove idempotent upsert with a crash test. | L | med |
| 17 | G-MAIN-04 | A crash happens after settlement, before the webhook. | The `pipeline.run.*` webhook is lost (L). | A webhook outbox in the settlement transaction (`services/elitea-main/internal/api/webhook/dispatcher.go:17,76`). | M | med |
| 18 | G-WORKER-09 | The Worker is killed during a fixed Parallel or Map run. | Unproven. Production-gated. | A real-PostgreSQL reclaim test, then browser proof at the gate flip. | L | med |
| 19 | G-WORKER-08 | The Worker is killed during Code execution (owner-proof path) or publication. | The publication kill is unproven. | Docker and Kubernetes kill-during-publication acceptance with runner request counters. | M | med |
| 20 | G-SUP-02 | Node loss on Kubernetes leaves the pod unterminated. | The row never reaches a terminal state (F after `timeout+90`). | A bounded operator-proof path and an alert on `dispatched` rows older than 3660 s (`services/elitea-worker-rust/src/sandbox/kubernetes/runtime.rs:707`). | M | med |
| 21 | G-SUP-05 | A Supervisor outage is longer than `timeout+90` seconds. | F. The sandbox may still finish unseen. | Park and wake instead of fail (`services/elitea-worker-rust/src/agents/graph/code_remote.rs:421-428,460`). | M | med |
| 22 | G-WORKER-06 | A crash happens before the first model marker, or during a regenerate turn. | F, although no model was called. | Record an `Authorized` state before the first marker (`services/elitea-worker-rust/src/agents/session.rs:859,879-884`). | M | med |
| 23 | G-GW-03 | A gateway SIGKILL, or a lost budget stream. | Underbilling, or counters reset to zero (L). | A local WAL replayed with `event_id`. Re-seed counters from `gateway.llm_budget_accumulators`. | M | med |
| 24 | G-WEB-01 | A network drop occurs after the POST and before the response. | No execution id until a reload. | Keep `question_id` and re-POST on a network error (`apps/elitea-web`). | M | med |
| 25 | G-WEB-02 | Two tabs show the same pending guards. | The second tab keeps stale controls. | Cross-tab invalidation (BroadcastChannel) and a two-context Playwright spec. | M | med |
| 26 | G-SUP-07 | Code runs inside a child pipeline or a fan-out. | Unproven. | A scoped-child Code recovery fixture. | M | med |
| 27 | G-NATS-03 | The command consumer is deleted or drifted. | Workers stop until the bootstrap job runs again. | A periodic reconcile or a retry with backoff. | S | med |
| 28 | G-P17-02 | The Rust indexing worker is "Planned". | No R, I or C claim is possible. | Implement with the gates in `services/elitea-worker-rust/docs/source-mapping/indexing.md`. | L | med |
| 29 | G-P17-03 | The inventory engine crashes mid-ingest. | The source status stays `in_progress`. | Reset the status when no live lease exists (`services/elitea-inventory-engine/src/store/sources.rs`). | S | low-med |
| 30 | G-ADV-06 | A mixed fleet during the flag rollout: an enabled Worker is interrupted and a disabled Worker re-claims. | F. | Add a note to the rollout runbook. Recovery mode follows the claiming Worker (`services/elitea-main/internal/infra/db/repos/claims.go:463`). | S | low |
| 31 | G-MAIN-07 | A bearer-secret inbound trigger retries after a lost 202. | A second run (by design). | An optional `Idempotency-Key` header (`services/elitea-main/internal/api/v2/pipelinetriggers/deliveries.go`). | S | low |
| 32 | G-MAIN-08 | A crash happens during the Index v2 cutover. | The procedure is operator-driven. | Document and test a re-run of the preflight. | S | low |
| 33 | G-WEB-03 | A malformed SSE frame arrives from an intermediary. | A transcript hole until a reload. | Mark the message for refetch on a parse failure (`apps/elitea-web`). | S | low |
| 34 | G-NATS-05 | The scale-1 profile uses one NATS node with `replicas=1`. | A PVC loss gives stream loss (see rank 6). | A runbook and an alert. | S | low |
| 35 | G-P17-04 | `Reconcile` ends live peer invocations. | Latent while `replicaCount` is 1. | An owner liveness heartbeat (`services/elitea-subapp-host/internal/spi/pgstore.go:185-205`). | S | low |
| 36 | G-ADV-08 | A collaborator in a shared conversation approves another user's sensitive tool. | Verify: a negative authorization test is needed. This is not confirmed as a defect. | Test the decider against the pause owner in `services/elitea-main/internal/application/agentexecution/continue_static.go:256-280`. | S | verify |

Retired or merged ids: G-WORKER-12 is retired. It was overstated and G-ADV-09 replaces it. G-MAIN-02 merges into
G-NATS-01. G-WORKER-11 merges into G-NATS-04. G-MAIN-05, G-NATS-02, G-PG-02, G-SUP-04 and G-ADV-09 merge into rank 4.

## 16. Documentation contradictions

The stale documents are not edited in this change. Their owners update them.

1. `services/elitea-worker-rust/docs/source-mapping/chat-restart-observer.md:76-105` records the Main crash as a failure (chat 564). `services/elitea-worker-rust/docs/source-mapping/agent-crash-continuation.md:416-436` records the same scenario as a resume (chat 567).
2. `services/elitea-worker-rust/docs/source-mapping/agent-runtime.md:225-232` expects a StatefulSet with one volume per replica. The chart uses a Deployment with `emptyDir` (`deploy/helm/elitea/templates/worker/deployment.yaml:302-304`).
3. `services/elitea-worker-rust/docs/source-mapping/agent-crash-continuation.md:1-3` says "not a passing gate". The same file holds deployed acceptance sections. The comment at `services/elitea-worker-rust/src/execution/native_agent_lifecycle.rs:3-6` still says "capability-disabled".
4. `services/elitea-worker-rust/docs/source-mapping/toolkit-terminal-recovery.md` lists REC-RUST-04 as open. `services/elitea-worker-rust/docs/remaining-gates.md` records nested-agent and pipeline crash acceptance (chats 632, 653).
5. The recovery documents (`services/elitea-worker-rust/docs/source-mapping/toolkit-prepared-command-recovery.md:35-36`, `services/elitea-worker-rust/docs/source-mapping/agent-crash-continuation.md`) describe Redis delivery. The bus is NATS since `887bfb4d`. The proofs belong to the old transport (D3).
6. `services/elitea-worker-rust/docs/testing-gaps.md:46` says `services/elitea-worker-rust/src/execution/nats_live_tests.rs` proves the secured NATS path. The tests skip without a server. CI provides one (`.github/workflows/ci-rust.yml:240-245`).
7. `services/elitea-main/docs/source-mapping/node-recovery-control-20261004.md` and `services/elitea-worker-rust/docs/source-mapping/node-code-owner-recovery-20261004.md` say the tests are "authored, uncompiled and unrun". `services/elitea-worker-rust/docs/remaining-gates.md:797-800` reports a passing candidate with 55 ignored integrations. The ignored set contains the owner-proof suites.
8. `services/elitea-worker-rust/docs/source-mapping/nats-command-delivery.md` says poison commands never get `Term`. `services/elitea-worker-rust/src/transport/command_bus.rs:331-333` terminates `SignatureInvalid` and `SubjectMismatch`.
9. `docs/runtime-command-bus.md:25-38` implies that PostgreSQL heals any lost delivery. The re-offer SQL selects unclaimed rows only (G-NATS-01). The statement "MaxDeliver -1 (PostgreSQL bounds retries)" holds only before authority.
10. `services/elitea-worker-rust/docs/source-mapping/code-restart-contract-20261002.md` and `services/elitea-worker-rust/docs/source-mapping/sandbox-phase-deadlines-20261002.md` say the deadline confirms termination. With an exited container and no readable receipt, the deadline branch is unreachable (`services/elitea-worker-rust/src/sandbox/docker_supervisor.rs:495-514`). `Phase::Uncertain` is never written.
11. The Docker receipt validator rejects `memory_limit` (`libs/rust/vendor/adk-sandbox/src/workspace/docker_code_jobs.rs:392-397`). `classify_receipt` accepts it (`services/elitea-worker-rust/src/sandbox/docker_supervisor.rs:783`). Kubernetes synthesizes it.
12. `apps/elitea-web/src/shared/api/sse/resume.ts` says the client gives up after 4 attempts. `useChatStreamConnection.ts:149-175` retries every 30 s without end. `services/elitea-worker-rust/docs/source-mapping/chat-restart-observer.md` says reload observation is not restored. `usePendingReplay.ts:41-70` restores it.
13. `services/elitea-scheduler/RETIREMENT.md` says the scheduler no longer dispatches. `centry.schedule` rows are still editable in the admin page. Nothing in this repository runs them.
14. `deploy/helm/elitea/templates/worker/keda-scaledobject.yaml:86-90` says the Worker "drains on SIGTERM". The grace period equals the drain, so a long turn fails (G-ADV-03).
15. `libs/rust/model-client/docs/llm-caller-contract.md:115` says "the ADK decides" about retries. No retry code exists in `services/elitea-worker-rust/src/agents`. `docs/UPGRADING.md` presents the budget counter reset as a one-time effect. Any loss of the `GATEWAY_BUDGET` stream causes it.
16. `services/elitea-worker-rust/docs/source-mapping/code-publication-recovery-20261004.md` says Main restart during publication passes. It also says it "does not prove that an in-flight upload was interrupted". A Supervisor restart during publication is not covered.

## 17. Maintenance

- Update a row in the same change that touches its component and phase. Name the enforcing `path:line` and the proving test.
- Evidence must name a test and its CI job, or deployed execution ids on the current transport. A test that is `#[ignore]` or a declared skip is not evidence.
- A `P*` cell becomes `P` only after a re-run on the NATS cohort. Record the execution ids, the image identities and the date in a source-mapping document.
- A `U` cell becomes `P` only when a test injects the named fault in the named phase.
- Add a new gap id to section 15. Do not renumber existing ids.
- Keep the customer answer in section 1 true. If a row changes class, update the section 1 table.
- Link new evidence from [remaining-gates.md](remaining-gates.md) and ask the owner of [testing-gaps.md](testing-gaps.md) to close the matching item (TG-12 covers process replacement and NATS restart).
