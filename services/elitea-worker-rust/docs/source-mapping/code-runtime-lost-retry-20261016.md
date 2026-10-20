# Code: a lost sandbox runtime is re-run, a user-code failure stays typed (FU-214), 2026-10-16

## Problem and business behaviour

The supervisor receipt used to say `sandbox.code_failed` both when the user's code threw and when the sandbox
runtime itself was lost:

- On Kubernetes, the kubelet reports a container whose state it lost (node loss, node restart) as terminated with
  exit 255 and reason `Unknown`. `termination_receipt` turned every non-zero exit into `failed`.
- On Docker, a container killed from outside (`docker kill`, a daemon or host stop) or removed has no runner
  envelope. The receipt read failed, the row stayed `dispatched`, and the Worker gave F after `timeout+90`.

Effect-free Code therefore ended in a typed failure (F, `PIPELINE_CODE_FAILED`), although running it again is safe.
The k8s1084 fix run NODE-js `ddc2a1c5712e8304fd8b0e8fab201a73` showed it (code-live-owner-recovery-20261008.md,
"Follow-up from the fix run").

The current platform has no sandbox supervisor and no node loss. Its behaviour (a Code node error stops the
pipeline) is kept for user-code failures. Nothing about lost runtimes is ported; the class is new.

Target, now implemented:

- a lost runtime of effect-free Code is re-run inside the node's deadline (class I);
- a user-code failure is still typed F, `PIPELINE_CODE_FAILED`, and never re-run;
- re-runs are bounded, keep stable identities and fencing, and settle exactly once.

## Design

### 1. How "lost" is proven (supervisor)

The runner is the container's PID 1 (`exec elitea-code-runner`). It writes exactly one envelope to stdout and exits
0, also when the user's program fails (`services/elitea-code-runner/src/main.rs:105-111,180-191`). Code inside the
container cannot signal PID 1 of its own PID namespace. So a user failure always arrives as an envelope, and "lost" is
proven only by the container platform:

- **Kubernetes**: the original, UID-checked Pod is terminated with a non-zero exit, no `OOMKilled`, no
  `DeadlineExceeded`, and at least one of the following holds:
  - a node shutdown or node loss Pod reason (`Terminated`, `Shutdown`, `NodeShutdown`, `NodeLost`, `Preempting`);
  - exit 137, 143 or 255;
  - container reason `Unknown` or `ContainerStatusUnknown`;
  - a `DisruptionTarget=True` condition.

  A kubelet eviction is excluded: Pod reason `Evicted`, or a `DisruptionTarget` reason `TerminationByKubelet`
  without a node-shutdown reason. The Pod's own workspace or ephemeral-storage use can cause it, and it repeats
  (review M1).
  - Code: `services/elitea-worker-rust/src/sandbox/runtime_loss.rs:93` `kubernetes_lost`, used by
    `src/sandbox/kubernetes/client.rs:415` `termination_receipt`.
- **Docker**: the runner envelope cannot be read, and container state shows one of:
  - the bound container is gone;
  - the container stopped with exit 137, 143 or 255, wrote nothing to stdout, and has no `OOMKilled`.

  `OOMKilled` stays `memory_limit` (F).
  - Code: `runtime_loss.rs:67` `docker_status`, `src/sandbox/runtime.rs:699` `receipt` and `:847`
    `synthesized_receipt`, over the new read-only `adk-sandbox` observation
    `libs/rust/vendor/adk-sandbox/src/workspace/docker_code_jobs.rs:437` `observe_code_job_exit` (one inspect by
    immutable container id, the first stdout log chunk, 10 s bound).
- Nothing short of that evidence changes today's answer:
  - a running or never-bound container, a damaged envelope (any stdout output) and a runner error exit (1, 101) keep
    the unreadable-receipt error;
  - a missing Kubernetes Pod is not evidence (the receipt finalizer keeps it).
- The envelope `{"status":"runtime_lost"}` is **synthesized only by the adapters**. Both runner-log validators
  (`kubernetes/client.rs` `validate_receipt`, `docker_code_jobs.rs` `read_code_job_receipt`) allowlist statuses
  without it, so code output cannot claim a lost runtime. `docker_supervisor.rs:797` `classify_receipt` maps it to
  the ledger failure code `sandbox.runtime_lost` (`runtime_loss::FAILURE_CODE`).

### 2. What the Worker does with it

- **Classification.** A `Failed` outcome with `sandbox.runtime_lost` becomes `NodeFailureClass::RuntimeLost` with
  `ReplaySafety::NoExternalEffect`, but only for a runtime **without the platform client**
  (`src/agents/graph/code_attempt_remote.rs:844` `terminal_failure_class`, called at `:646`).
  - A platform-client runtime is never re-run. Main's platform-call journal answers "nothing unresolved" also for
    calls that already **committed**, and a re-run would repeat them under new identities (review H1). Such a lost
    runtime is `InvalidResult`: F with a clean journal, C with a possible effect, exactly as before.
  - Every other failure code, including the user's own `sandbox.code_failed`, stays `InvalidResult`.
- **Re-run as a new node attempt.** The node attempt ledger (`libs/rust/agent-runtime/src/graph/node_recovery.rs`)
  grants a platform-owned allowance for `RuntimeLost`:
  - `plan_runtime_lost` (`:452`) gives `RetryAt` for the next ordinal attempt;
  - the bounds are named constants: `MAX_RUNTIME_LOST_RETRIES = 2` (`:18`), `RUNTIME_LOST_RETRY_WINDOW_MS = 900_000`
    (`:22`) from the first attempt's start, or the node policy's own `max_elapsed_ms` when shorter
    (`runtime_lost_deadline`, `:435`), and `RUNTIME_LOST_RETRY_DELAY_MS = 1_000` times the re-run count;
  - the re-runs do not consume the node policy's own attempts: `plan` (`:1062`), `attempt_limit`,
    `runtime_lost_retries`, `retry_deadline`;
  - `expire_wait` and `start_attempt` enforce the lost-runtime deadline on a pending re-run, also after a Worker
    restart;
  - past the allowance, the failure is decided like before FU-214, as a failed result: Stop, or the node's configured
    error route (review M3).
- **Platform-owned class.**
  - YAML cannot name it: `NodeErrorRoute::admit` refuses it, and `RetryCondition` has no variant for it.
  - Operators cannot retry it (`operator_retry` needs a retry condition).
  - It never appears on a node-recovery receipt: `src/agents/graph/node_recovery_receipt.rs:77` refuses it, so
    Main's receipt contract is unchanged.
- **Identity.** Every attempt has its own dispatch activation, H(activation, attempt), which is also its own
  supervisor job and Pod/container, its own Main visit and intent, and its own dispatch-journal row.
  - A default-policy node (no `recovery:` block) keeps its pre-existing identity for attempt 1. Its re-runs use the
    ordinal identity (`src/agents/graph/node_recovery_runtime.rs:1014`). The owner-result projector already did
    this for attempts ≥ 2 (`code_committed.rs:84,197`).
- **Main.** `matchOriginalCodeDispatch` admits attempts 2..1+`MaxRuntimeLostRetries` of a default-policy node, and
  only with the derived ordinal identity: never the sealed first identity, never another ordinal, never attempt 4,
  never for a platform-client node (security review)
  (`services/elitea-main/internal/infra/db/repos/code_sandbox_intent.go:609-631`). Nothing else in Main changes. The
  receipt, settlement, failure catalogue and protobuf schemas are untouched.
- **Shared limit.** The allowance is one value across the layers. `testdata/proto/runtime/v1/code_runtime_lost_retry_policy.json`
  is asserted by both the Worker (`runtime_loss.rs` `the_retry_allowance_matches_the_shared_policy`) and Main
  (`TestRuntimeLostRetryAllowanceMatchesTheSharedPolicy`).
- **Terminal text.** An exhausted allowance settles `pipeline.code_failed`, which is `PIPELINE_CODE_FAILED` with its
  existing message (`code_runtime.rs:75`). Main validates the message text against its allowlist, so a dedicated
  sentence would need a Main change. That is follow-up FU-214-b.
- **Data-analysis Code tool.** `sandbox.runtime_lost` maps to `AnalysisCodeFailure::Interrupted`. Its sentence tells
  the model that the run was lost before it finished, not because of an error in the code. The tool has no platform
  client, so a new call is safe (`code_tool.rs:85`, `families/data_analysis/code.rs`).

### 3. Why a re-run is safe (stated side effects)

- **Network.** Code runs with `NetworkMode none` on Docker and the default-deny `elitea-code-deny-network` policy on
  Kubernetes. A runtime without the platform client cannot leave an effect outside its own container.
- **Platform client.** It is excluded from the re-run (see above).
- **No two runtimes at once.** A re-run starts only after the supervisor durably persisted the lost attempt as
  `failed`. On Kubernetes this needs a kubelet-reported terminated state. On Docker, a container that is absent or
  exited is not running on that host.
- **Work repeated per attempt, all bounded:**
  - the Main original visit and intent are keyed per attempt (`UNIQUE(execution_id, generation, activation_id, attempt)`);
  - the debug-writer export runs again for the new attempt, so a node with debugging on gets one more debug artifact;
  - Python dependency preparation runs again for the new attempt through the preparation network (the PyPI
    egress already admitted for the node);
  - hydration fills the new attempt's own inert runtime.
- **Workspaces.** Repository and workspace content is hydrated into the attempt's own job runtime. No write-back path
  from a runtime exists (there is no workspace publication from runtime state), so a lost runtime's partial writes die
  with it.

### 4. Interaction with epochs and takeover

- The node journal's fenced append orders `Failure` (with its `RetryAt`) and the next `Start` before any dispatch.
- A takeover reads the same durable ledger:
  - after the `RetryAt`, the new claim starts the re-run once;
  - after the re-run's `Start`, it reconciles the re-run with the same identity, and the supervisor's job key makes
    the submission idempotent.
- A superseded claim's result cannot pass the newer claim's fence (real-PostgreSQL test below).
- Supervisor lease epochs are per job, so each attempt's job has its own.

### 5. Backward compatibility

- **Older Workers and Main.** `sandbox.runtime_lost` is an ordinary failure code for them, and it fits the ledger
  `failure_code` check `^[a-z][a-z0-9_.]{0,63}$`. No migration and no protobuf change.
- **New node ledgers.** A ledger that recorded a `RuntimeLost` failure is stored as schema
  `elitea.pipeline.node-recovery.v3` (`node_recovery_codec.rs:17,20`). Ledgers without one keep v1/v2 byte for byte,
  and decode replays them identically.
- **Rollout order: Main first, then Supervisor and Worker.**
  - An older Main refuses attempt 2 of a default-policy node. The node then fails typed with an authorization
    refusal, not a duplicate run.
  - An older Worker refuses a v3 ledger as invalid history (F) instead of guessing.

## Changed paths

| Area | Paths |
|---|---|
| Supervisor evidence | `services/elitea-worker-rust/src/sandbox/runtime_loss.rs` (new), `sandbox/kubernetes/client.rs`, `sandbox/runtime.rs`, `sandbox/docker_supervisor.rs`, `sandbox/mod.rs`, `libs/rust/vendor/adk-sandbox/src/workspace/{docker_code_jobs.rs,docker.rs}` |
| Worker | `src/agents/graph/{code_attempt_remote.rs,code_runtime.rs,code_tool.rs,node_recovery_runtime.rs,node_recovery_receipt.rs}`, `src/protocol/output.rs` (`runtime_error` visibility for the parity test) |
| agent-runtime | `libs/rust/agent-runtime/src/graph/{node_recovery.rs,node_recovery_codec.rs}`, `toolkits/families/data_analysis/code.rs` |
| Main | `services/elitea-main/internal/infra/db/repos/code_sandbox_intent.go` |
| Contract | `testdata/proto/runtime/v1/code_runtime_lost_retry_policy.json` (new) |

No dependency was added or changed.

## Tests

| Suite | New tests | Result |
|---|---|---|
| agent-runtime ledger: `node_recovery_runtime_lost_tests.rs` (11) | a default-policy node is re-run and completes once (v3 round trip); a user failure is never re-run, also after a re-run; the allowance is bounded; the window edge (limit and limit+1 ms); a node elapsed limit shorter than the window; a pending re-run expires at the deadline after a Worker restart; a possible effect is not this class; not routable, no operator retry; a forged v1 record is refused; impossible histories; an exhausted lost runtime reaches the configured error route | pass |
| Worker unit | `runtime_loss` (5: Docker evidence, Kubernetes evidence, kubelet eviction is not loss, envelope shape, shared policy), `kubernetes::client` (2 new or extended: killed-pod receipts incl. `Unknown` 255 / 137, Pod reason and `DisruptionTarget` extraction), `synthesized_receipt_tests` (Docker fallback), `classify_receipt`, `code_tool` mapping | pass |
| Worker real PostgreSQL, Supervisor ledger (`docker_deadline_tests.rs`, run by CI with `--ignored`) | a lost runtime and a user failure persist distinct terminal codes after a Supervisor replacement; neither restarts or re-signals a runtime | pass |
| Worker real PostgreSQL, production `RemoteCodeRuntime` + Code node + dispatch journal (`node_recovery_code_wiring_tests.rs`) | lost → re-run with a new identity → completes once, both dispatch rows resolved, replay submits nothing; user failure → `pipeline.code_failed`, one submission; the allowance spent → typed, 3 submissions, visits 1..3; a platform-client node with a possible effect reconciles; a platform-client node with a clean journal is typed and not re-run | pass |
| Worker real PostgreSQL, node journal (`postgres_checkpointer_tests/runtime_lost_journal.rs`) | re-run across process replacement (claim 1 dies inside the re-run; claim 2 reconciles the same identity and completes once; re-entry runs nothing); a superseded claim cannot settle its re-run; user failure never re-run; exhausted allowance typed, distinct identities | pass |
| Main | `TestOriginalCodeDispatchAdmitsBoundedLostRuntimeRerunsOfADefaultPolicyNode`, `TestRuntimeLostRetryAllowanceMatchesTheSharedPolicy`, existing `TestOriginalCodeDispatchMigrationUsesSavedPolicyPresence` | pass |

Full runs on the final commit (disposable `postgres:18` container, `ELITEA_TEST_DATABASE_URL`,
`ELITEA_TEST_DATABASE_GUARD=disposable-pg18`, `cargo --offline --locked`):

- `libs/rust`: `cargo test --workspace --all-targets --all-features`. 2650 passed, 0 failed, 1 ignored (pre-existing).
  `elitea-agent-runtime` alone has 1360 passed.
- Worker: `cargo test --all-targets --all-features`. 2268 passed, 23 failed, 100 ignored, on a host at load average
  ~60 to 75.
  - All 23 failures are PostgreSQL connect or acquire timeouts in test fixtures (`postgres_session_tests.rs:42`).
  - Re-run alone with `--test-threads=2`: 21 pass.
  - The remaining 2 are `postgres_graph_receipts_*`, the known FU-210 failure on a bare PostgreSQL.
  - The `#[ignore]` suites above were run with `--ignored`. Under the same load, some fixtures timed out on connect;
    each such test then passed when run alone.
- `cargo fmt --check`, `cargo clippy --all-targets --all-features -D warnings` and
  `RUSTDOCFLAGS=-D warnings cargo doc --no-deps --all-features` are clean in both workspaces.
- Main (Go 1.26.9 toolchain, `GOPROXY=off`): `go test ./internal/infra/db/repos/ -run 'OriginalCode|CodeSandbox|CodePlatform|CodeIntent'`
  against the same PostgreSQL passes (263 s); `go vet` is clean; `gofmt` is clean.
  - The `golang:1.26.9-bookworm` container could not be used: the shared Docker VM was unresponsive. The same
    toolchain version was run from the module cache instead.

Revert checks:

- Classifying `sandbox.runtime_lost` as `InvalidResult` again fails the wiring lost→re-run test.
- Removing the `platform_client` guard fails the clean-journal platform test.
- Restoring `Evicted` as loss fails `a_kubelet_eviction_is_not_loss`.
- Reverting the legacy attempt rule fails the real-PostgreSQL journal test at attempt 2.

## Performance / Durability / Resilience / Security

- **Performance.** Nothing changes on the success path.
  - A lost runtime costs one extra inspect plus one first log chunk on Docker (bounded at 10 s), and nothing extra
    on Kubernetes.
  - A re-run costs one new attempt (one Main visit and intent, one job), after `1 s × n`.
  - At most 2 re-runs per node visit.
- **Durability.**
  - Crash windows: (a) Supervisor dies after observing the loss, before persisting it; (b) Worker dies after the
    lost receipt, before the failure append; (c) Worker dies after the `RetryAt`, before the re-run `Start`; (d)
    Worker dies inside the re-run.
  - Rules: (a) the next reconcile synthesizes the same receipt from the same container state. (b) The re-entered
    attempt re-submits the same identity, and the terminal record answers. (c) The new claim starts the re-run once.
    (d) The new claim reconciles the re-run's identity.
  - (d) and fencing are proven on real PostgreSQL. (a) is proven by the Supervisor ledger test (a replacement
    reconciles the dispatched row).
- **Resilience.** Everything is bounded:
  - re-runs: 2;
  - window: 15 min, or the node's own limit;
  - attempts: ≤ 16;
  - Docker observation: 10 s.

  Typed failures stay typed. A lost runtime with unknown evidence keeps today's conservative answer, never a re-run.
- **Security.**
  - The lost status cannot be forged from code output (log allowlists).
  - Main's wider admission is bounded to the ordinal identity and 1+2 attempts, inside its existing claim-fenced
    transaction.
  - No new log field carries data: the new `warn` events have fixed text, and the Docker one logs no identifiers.
  - No secrets in the diff; the secret scan is clean.
  - Defence-in-depth gap: Main does not yet verify that attempt n−1 ended `sandbox.runtime_lost`. That is FU-214-c.

## Recovery-guarantee rows

| Component × phase | Fault | Before | After | Mechanism | Proof |
|---|---|---|---|---|---|
| Supervisor × P10 (Docker) | runtime container killed (`docker kill`, daemon or host stop) or removed | F after `timeout+90` | **I** (no platform client); F typed (platform client, clean journal); C (possible effect) | `runtime.rs:699,847`, `runtime_loss.rs:67`, `code_attempt_remote.rs:844`, `node_recovery.rs:452,1062` | wiring + journal real-PG tests; deployed compose KILL-js `54fe68de…` |
| Supervisor × P10 (Docker) | runtime OOM-killed | F after `timeout+90` | F typed `sandbox.memory_limit`, at once | `runtime_loss.rs:67` | `synthesized_receipt_tests` |
| Supervisor × P10 (Kubernetes) | node loss / restart, the kubelet reports `Unknown` 255 | F typed (`ddc2a1c5…`) | **I**, inside the 15 min window | `kubernetes/client.rs:415`, `runtime_loss.rs:93` | `killed_original_pod_returns_failure_without_runner_logs`; deployed kind NODE-js `b0b76ae6…`, NODEDOWN-js `fedd3c29…` |
| Supervisor × P10 (Kubernetes) | node never returns (the Pod stays `Terminating` without a terminated state) | F after the deadline | unchanged (G-SUP-02 residual) | — | — |
| Supervisor × P10 | kubelet eviction (`Evicted`, `TerminationByKubelet`) | F | F typed (deliberate) | `runtime_loss.rs:93` | `a_kubelet_eviction_is_not_loss` |
| Worker × P10 | user code throws | F typed | F typed, never re-run | `code_attempt_remote.rs:844` | `a_user_code_failure_*` (3 suites) |
| Worker × P10 | lost runtime, allowance spent or window passed | — | F typed `PIPELINE_CODE_FAILED`, or the node's error route | `node_recovery.rs:1062` | `lost_runtimes_past_the_allowance_settle_typed`, `an_exhausted_lost_runtime_still_reaches_the_configured_error_route` |
| Worker × P10 | Worker dies inside a re-run | — | R (same identity reconciled) | node journal fence | `a_lost_runtime_is_rerun_across_process_replacement_and_settles_once`, `a_superseded_claim_cannot_settle_its_rerun` |

## Deployed evidence

### Deployed runs: compose, production profile (`elitea-fu214`, 2026-10-16)

- Stack: `production-profile/stack.sh up --name fu214 --port-base 18700`, real-model dump; `verify-profile` 50 passed,
  0 failed (agent node recovery, model-checkpoint recovery, Main Code owner recovery, five supervisor profiles).
- Images built from commit `89521291a`, checked per rules §3b with `stack.sh binary-check`:

  | Role | Image | Id | Marker |
  |---|---|---|---|
  | Main | `elitea-main:fu214-89521291a` | `763397a3807e` | build version `fu214-89521291a`, count 1 |
  | Worker | `elitea-worker-rust:fu214-89521291a` | `e6bcea1e18d9` | `code_runtime_lost`, count ≥ 1; absent in the `pm1084-dd0bbc170` base build |
  | Supervisor | `elitea-sandbox-supervisor:fu214-89521291a` | `19a3b1651bb3` | `runtime_lost`, count 2 |

  The Rust sources are identical to the first build commit (`git diff 6ac37b277 89521291a` is empty under
  `services/elitea-worker-rust/src`, `libs/rust`, `services/elitea-code-runner`). Web and subapp-host are the
  `pm1084-dd0bbc170` builds of the same base commit; the MCP mock is `pm1084-dd0bbc170`.
- Fixtures: pipelines and chats were created through the product API as the stack's OIDC test user (project 2), not
  in the database.

| Case | Execution | Chat | Fault | Outcome |
|---|---|---|---|---|
| POS-js (3 s) | `e91f015636d3de4e4a852e8cf4307668` | 880 | none | SUCCEEDED, attempt 1 |
| KILL-js (25 s) | `54fe68def419465f6c19b7d37bcf2754` | 881 | `docker kill` of the job's own runtime container 8 s after dispatch (16:33:39.247Z) | Attempt 1 job `failed` / `sandbox.runtime_lost`; attempt 2 job `completed`; Main visits for attempts 1 and 2; one settlement SUCCEEDED (claim 1, epoch 1); answer `JS started=1792168422729` (16:33:42.7Z, after the kill: the code ran again); about 30 s from fault to settlement; no runtime container left |
| RAISE-py | `bc01c45ca21c01d4b375b63822a73b50` | 882 | none (the code raises) | FAILED `PIPELINE_CODE_FAILED`, one visit, job `failed` / `sandbox.code_failed`; no re-run |

The container to kill was identified by the runtime-job hash of this stack's own ledger rows
(`ledger.rs` `runtime_identity`). No other stack's container was touched.

Browser: the user's Chrome, in a new tab, signed in through the stack's OIDC mock test identity. Chats 881 and 882
were loaded fresh and then reloaded. 881 shows the single answer `"JS started=1792168422729"`. 882 shows the
`PIPELINE_CODE_FAILED` sentence with "Details for support". The tab was closed afterwards.

Observed, not changed here: `execution_settlements.error_code` is empty for the FAILED run (FU-217).

### Deployed runs: Kubernetes (kind cluster `k8s1084`, 2026-10-19)

- Cluster: two-node kind (`k8s1084-control-plane`, `k8s1084-worker`), the chart deployment of the #1084 Kubernetes
  matrix (the Code runtime Pods run on `k8s1084-worker`).
- Images from commit `89521291a`, pushed to the cluster registry, same binaries as the compose run:
  - Main `k8s1084-registry:5000/elitea-main:fu214-89521291a`;
  - Worker `k8s1084-registry:5000/elitea-worker-rust:fu214-89521291a`;
  - Supervisor by digest `sha256:19a3b1651bb3feec12f22fb52cd9bcdc828faee18eebb124a9f2c52999f1ff74`.

  The merged main shared/tenant migrations were applied with the Main image's own migrator before the Main rollout.
- Harness: `handoffs/crash-recovery-suite/k8s/scen.py` (local tooling, outside git). It sends a saved pipeline with one
  JavaScript Code node that records its start time and sleeps 25 s. It waits for the execution job to be dispatched,
  then 8 s later runs `docker kill k8s1084-worker` (the kind node container) and `docker start` again after the
  delay.
- Fixtures were created through the product API as the cluster's OIDC test user (project 2), not in the database.

| Case | Execution | Chat | Fault | Outcome | Fault to settlement |
|---|---|---|---|---|---|
| NODE-js | `b0b76ae6ac75d729b4f9a8b1b1c25b61` | 973 | node killed 14:18:52.8Z, started again after 21 s | attempt 1 job `failed` / `sandbox.runtime_lost` (kubelet-reported terminated container); attempt 2 job `completed`; Main visits for attempts 1 and 2; claim 1 `LEASE_EXPIRED` (the Worker Pod was on the same node), claim 2 settled SUCCEEDED; answer `JS started=1792419631161` (after the fault: the code ran again) | 129.9 s |
| NODEDOWN-js | `fedd3c2973c50f801561f0c0a3453456` | 974 | node killed 14:21:46.0Z, started again after 423 s | same: `sandbox.runtime_lost` then `completed`, visits 1 and 2, claim 2 settled SUCCEEDED, answer `JS started=1792420174693`; the re-run started 476 s after the first attempt, inside the 900 s window | 498.4 s (76 s after the node returned) |

Every harness invariant held in both runs:
- one execution row and one committed settlement, bound to the last claim, with at most one terminal replay event;
- claim attempts contiguous;
- the command outbox and the NATS agent stream drained;
- two execution jobs, none left reserved or dispatched;
- each runtime Pod created exactly once and deleted, and no orphan Pod;
- the FU-214 checks: the first job ended `sandbox.runtime_lost`, the re-run completed, the run settled SUCCEEDED, and
  the user code started again after the fault.

Results: `handoffs/crash-recovery-suite/k8s/runs-fu214/{NODE-js,NODEDOWN-js}/result.json`.

Before this change the same NODE-js fault settled FAILED `PIPELINE_CODE_FAILED` (`ddc2a1c5…`, code-live-owner-recovery-20261008.md).

Most of the fault-to-settlement time is the Worker's own recovery, because the Worker Pod sat on the killed node: its
claim lease had to expire before claim 2 could start (FU-215). The re-run itself took about 25 s of user code.

Browser: the user's Chrome, in a new tab, signed in through the cluster's OIDC mock test identity. Chats 973 and 974
were loaded and then reloaded. Each shows exactly one answer (`"JS started=1792419631161"` and
`"JS started=1792420174693"`) and no failure card. The tab was closed afterwards.

## Follow-ups

- **FU-214-b.** A dedicated user-facing sentence for an exhausted lost runtime. Main allowlists the
  `PIPELINE_CODE_FAILED` text, so this needs a paired Main and Worker change.
- **FU-214-c (SEC-FU214-4).** Main verifies the outcome of attempt n−1 before it admits attempt n of a
  default-policy node (defence in depth).
- **FU-214-d (SEC-FU214-1).** Harden the Docker memory-limit classification of the runner process.
- **FU-214-e (SEC-FU214-2).** Keep the Supervisor deadline failure code stable across a Supervisor restart (no re-run is affected).
- **FU-214-f.** A permanently lost Kubernetes node (Pod stays `Terminating`, no terminated container state) is still
  G-SUP-02. A bounded operator path is needed.
- **FU-214-g.** Preparation, compiled-snapshot and platform-client lost runtimes settle typed; they are not re-run.
- **FU-214-h (SEC-FU214-3).** Harden runner process isolation (a separate identity for user code).
