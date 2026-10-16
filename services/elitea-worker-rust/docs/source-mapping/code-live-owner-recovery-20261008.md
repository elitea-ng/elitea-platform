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
Preparation failure outcomes and replacement debug reconciliation are closed on the merged head; see "Deployed preparation and NATS crash matrix".
Later [chat852](code-compiled-publication-main-recovery-20261008.md) closes Main compilation publication loss for the original successful compiler receipt.
Queued and in-flight NATS loss is closed on the merged head (same section). Current Kubernetes acceptance remains open; it is tracked as group 3.
The separate live canvas image later passes build and strict scan. Its deployed browser acceptance is closed: "Run is stopped" after Stop, see "Deployed crash matrix".
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

## Deployed preparation and NATS crash matrix on the merged branch (2026-10-09)

This closes the two open items of the PR #1084 body: group 1 (finite preparation fault outcomes, original debug-writer
reconciliation) and group 2 (queued and in-flight NATS recovery with the original broker storage).

The branch head is `e7a291985` (`22f33b8f8`, then `main` merged in again). The runs used the standalone compose stack
`elitea-code1084` at `http://code1084.localhost:18300`, on its own project, volumes and networks. They ran 19:10-19:50 UTC.

**Stack.**
- Images, rebuilt at the head and tagged `code1084-e7a291985`: Main `2919f6aea9b6`, Worker `ac43f999bd24`,
  Supervisor `0d79c411fc66`, Web `6e49b27b1f16`, LLM gateway `e99b09a0b208`. The Code runner is the existing Deno
  image `76813e683650` (`sha256:76813e683650491a915ab811cdd15cfae3cfccef6a236969a79d29042426c60d`); no new image.
  Both preparation profiles use that same digest.
- Binary checks (binaries extracted with `docker create` + `docker cp`): the Worker contains `code_timing` and
  `pipeline.code_cancelled`, which `main-c0f2e5f9b-verify` does not. Main contains `resumeCurrentAgentStaticTools` and
  `PIPELINE_YAML_EXPANSION_TOO_LARGE`; neither is in `main-c0f2e5f9b-verify`, and the second is not in the earlier
  `22f33b8f8` build either, so the Main image is the new head. The Supervisor contains `service_job_owners.rs`. Worker and
  Main SHA-256 differ from the `22f33b8f8` builds. #1160 (`524165ed09`) is an ancestor of the head.
- Database: real-model product DB; AgentState migrations through version 13 (including the 0009 preparation receipt and 0010 phase clocks).
- Code runtimes: `agent_node_recovery: true`; five profiles in the one Supervisor process.

| Profile | Port, audience | Languages | Notes |
|---|---|---|---|
| `deno` (existing, owner kept) | 9446, `dns:elitea-sandbox-deno` | python | now `languages: ["python"]`, plus `dependency_content` (execution content download) |
| `denonative` (new) | 9450, `dns:elitea-sandbox-deno-native` | javascript, typescript | `native_platform` linux/arm64/gnu, `native_workspace_bytes` 512 MiB, memory 1 GiB |
| `prep` (new) | 9448, `dns:elitea-sandbox-preparation` | python | `purpose: preparation`, 512 MiB, 120 s, policy `python-preparation-v1` |
| `jsprep` (new) | 9449, `dns:elitea-sandbox-javascript-preparation` | javascript | `purpose: preparation`, native platform, 512 MiB workspace, 1 GiB, 120 s, policy `deno-preparation-v1` |
| `rust` (existing) | 9447 | rust | unchanged |

**Why five profiles.** `docker-compose.sandbox-preparation.yml` and `sandbox-preparation-deployment.md` describe Python
preparation only. JavaScript preparation needs the native Deno path: a preparation profile with `native_platform`, and an
execution profile with the same `native_platform` that does not list `python` (`process.rs` refuses the mix). The leaves
came from `gen-sandbox-certs.sh --native` under the existing runtime CA. Main's `ELITEA_RUNTIME_SANDBOX_AUDIENCES` lists the
three new audiences, and its Code-owner recovery config lists the `deno-native` origin. The Worker config adds `preparation`
to the Python entry and to the JavaScript entry, and moves JavaScript and TypeScript to `deno-native`. The overlay is a local
file because the Deno and Rust material sit in named volumes (UID 10001, mode 0600); it keeps the same mechanics as the repo
overlay (aliases, one tmpfs per content client with `noexec,nosuid,nodev`, 256 MiB, supervisor memory raised to 2 GiB).
A Compose overlay and a deployment page for the JavaScript profiles do not exist in the repo yet.

**Resolver network and egress.**
- `elitea-python-preparation-resolver-code1084` is a plain bridge (`internal=false`) created for this stack. Only preparer
  containers join it: the observed preparer has this single network, the execution runtime has `NetworkMode: none` and no
  network, and the Worker and Supervisor are not attached.
- On Compose a named network does not restrict destinations. A preparer can reach any internet host and the bridge gateway.
  The preparers downloaded from `pypi.org`, `files.pythonhosted.org`, `cdn.jsdelivr.net` and `registry.npmjs.org`
  (approved). Kubernetes is the enforcement point: NetworkPolicy on the preparation namespace with resolver CIDRs and DNS
  Pods only. The Kubernetes track covers that; nothing here proves it.
- Downloads were real: the image carries no `python-slugify`, `text-unidecode` or `is-number` (`/opt/elitea-wheels` holds 3
  unrelated files; `python-packages.json` and `javascript-packages.json` are empty). The preparer's receive counter on eth0
  rose from about 0.4 KB to 0.43 MB for Python and to about 19 KB for JavaScript. No registry error, timeout or throttle
  occurred in any run. An uninterrupted Python preparation takes 5-7 s from preparer start to publication, JavaScript about 1 s.

**Fixtures.** Each run uses a fresh saved pipeline (created through the product API) with a nonce comment in the source, so
every run has its own request digest. Each has one Code node:
- Python: records `time.time_ns()` first, runs `micropip.install("python-slugify==8.0.4")`, imports `slugify`, then returns
  `PY_PKG started=<ms> slug=hello-elitea-1084`. The slow variant waits 25 s after the install.
- JavaScript: `import isNumber from "npm:is-number@7.0.0"`, returns `JS_PKG started=<ms> isNumber=true`; the slow variant waits 25 s.
- Plain JavaScript (group 2): records its start, waits 0 s or 30 s, returns `JS_PLAIN started=<ms>`.
- Debug: the Python slow node with `debug: true`.
All runs are normal saved-pipeline Chat Sends through the API (`conversations`, `participants`, `messages`), not browser
clicks; Stop is the product `DELETE .../task/prompt_lib/{project}/{response_message_id}` route.

**Fault injection.** SIGKILL (`docker kill -s KILL`) of a service container of this stack, then `docker start` 7-8 s later.
The trigger is observed, not timed from Send: the preparation cuts fire when the preparer's receive bytes pass 100 KB (Python)
or 3 KB (JavaScript), i.e. inside the download; the hydration cuts fire when the execution runtime is bound and the job is
still `reserved` (before dispatch); the execution cuts fire 8 s after `dispatched`. Only `elitea-code1084-*` containers were
touched. Other Docker-daemon runtimes (`elitea-crash`, `elitea-nestedapp`) were never killed or removed.

**Invariants** (checked by the harness after every run, never by eye). Container checks use only the runtime labels computed
from this stack's own `sandbox_jobs` rows (`sha256("elitea.sandbox.runtime-job.v1\0" || len(tenant) u64 BE || tenant ||
project i32 BE || job_key)`, `ledger.rs:46-66`), read from a `docker events` recorder:
- **One execution, one settlement.** `execution_jobs` and committed `execution_settlements` rows each count 1; at most one
  terminal replay event.
- **One preparation per job.** One preparation ledger row and exactly one preparer container create/start for it. A second
  download would need a second preparer container.
- **No re-download after the prepared artifact is committed.** In the hydration and execution cuts the preparation job was
  already `completed`; afterwards the count of preparer containers and preparation rows stays 1.
- **One sandbox job.** One execution ledger row, one execution container start. Zero for Stop and the F cases.
- **User Code not re-run.** For the execution cuts the stored start time precedes the kill. For the earlier cuts, user Code can only
  start after preparation and hydration, and it starts exactly once (one execution container start).
- **Claims.** Attempts are contiguous, lease epochs increase, and the settlement is bound to the last claim.
- **Drain.** `command_outbox` row retired or authority granted; no job left `reserved` or `dispatched`; no container left for the
  execution's jobs after a 60 s grace (8 s used; all removals were visible earlier).
- **F cases** also check the typed code and readable message, and that no execution job exists.
- **NATS** (group 2): `/jsz` of stream `ELITEA_RT_V1_AGENT` and durable `elitea-agent-worker-v1` from the `nats-health` container:
  sequence advance equals the number of runs, and messages, pending and ack_pending are 0 at the end.

### Group 1: preparation

| Fault | Execution | Kill → restart (UTC) | Settlement | Answer (stored) | Class |
|---|---|---|---|---|---|
| none, Python package (python-slugify) | `b7a58b7e…` | — | SUCCEEDED at 19:49:08, attempt 1 / epoch 1 | PY_PKG started=19:49:07.687Z slug=hello-elitea-1084 | — |
| none, JavaScript package (npm is-number) | `efdf7ebb…` | — | SUCCEEDED at 19:11:25, attempt 1 / epoch 1 | JS_PKG started=19:11:25.038Z isNumber=true | — |
| Worker, Python preparation download | `4143187a…` | 19:41:48 → 19:41:56 | SUCCEEDED at 19:42:50, attempt 2 / epoch 2 | PY_PKG started=19:42:50.043Z slug=hello-elitea-1084 | R |
| Supervisor, Python preparation download | `108f55f4…` | 19:43:06 → 19:43:13 | SUCCEEDED at 19:44:07, attempt 1 / epoch 1 | PY_PKG started=19:44:06.980Z slug=hello-elitea-1084 | R |
| Main, Python preparation download | `e299a00a…` | 19:44:22 → 19:44:29 | SUCCEEDED at 19:45:33, attempt 2 / epoch 2 | PY_PKG started=19:45:33.129Z slug=hello-elitea-1084 | R |
| Worker, JavaScript preparation download | `9d148944…` | 19:45:46 → 19:45:53 | SUCCEEDED at 19:46:49, attempt 2 / epoch 2 | JS_PKG started=19:46:48.626Z isNumber=true | R |
| Supervisor, JavaScript preparation download | `dcab20f3…` | 19:47:02 → 19:47:09 | SUCCEEDED at 19:48:05, attempt 1 / epoch 1 | JS_PKG started=19:48:04.607Z isNumber=true | R |
| Worker, hydration (Python) | `4f47b392…` | 19:19:34 → 19:19:41 | SUCCEEDED at 19:20:38, attempt 2 / epoch 2 | PY_PKG started=19:20:37.816Z slug=hello-elitea-1084 | R |
| Supervisor, hydration (Python) | `afe6e82d…` | 19:20:56 → 19:21:03 | SUCCEEDED at 19:22:00, attempt 1 / epoch 1 | PY_PKG started=19:22:00.144Z slug=hello-elitea-1084 | R |
| Worker, prepared Python job running | `a2dc226b…` | 19:22:48 → 19:22:55 | SUCCEEDED at 19:23:47, attempt 2 / epoch 2 | PY_PKG started=19:22:42.681Z slug=hello-elitea-1084 | R |
| Worker, prepared JavaScript job running | `569462d9…` | 19:24:11 → 19:24:18 | SUCCEEDED at 19:25:11, attempt 2 / epoch 2 | JS_PKG started=19:24:03.228Z isNumber=true | R |
| Stop, Python preparation running | `db68c522…` | Stop at 19:25:25 | CANCELLED at 19:25:28, attempt 1 / epoch 1 | Execution was cancelled. | — |
| Worker, 3 ms after the debug write committed | `2e8e7785…` | 19:25:40 → 19:25:47 | SUCCEEDED at 19:27:16, attempt 2 / epoch 2 | PY_PKG started=19:26:50.832Z slug=hello-elitea-1084 | R |
| Worker, debug write stalled in `staging` | `3ccfac0c…` | 19:27:57 → 19:28:04 | SUCCEEDED at 19:29:31, attempt 2 / epoch 2 | PY_PKG started=19:29:05.620Z slug=hello-elitea-1084 | R |
| none, nonexistent PyPI package | `fb0ec73f…` | — | FAILED at 19:37:21, attempt 1 / epoch 1 | Code dependency preparation failed. Later nodes did not run. Share the support reference with your administrator before retrying. | F |
| none, nonexistent npm package | `e2cf1562…` | — | FAILED at 19:37:35, attempt 1 / epoch 1 | Code dependency preparation failed. Later nodes did not run. Share the support reference with your administrator before retrying. | F |
| preparer cut from the resolver network | `f6a60f17…` | 19:37:48 → - | FAILED at 19:37:50, attempt 1 / epoch 1 | Code dependency preparation failed. Later nodes did not run. Share the support reference with your administrator before retrying. | F |

The rows with the fault in the first column are from the final run of each case. Earlier runs of the same case (a first pass
before the cut was moved inside the download, or with a harness check that was wrong) are not counted; none contradicted these.

**Class justification.**
- **Preparation cuts (Worker, Supervisor, Main), R.** The Supervisor ledger owns the preparation job and the holding preparer
  container. The replacement Worker (claim attempt 2 after `LEASE_EXPIRED` for Worker and Main loss) or the same claim
  (Supervisor loss, attempt 1) reconciles the original job. The preparer is created once, no second preparation row appears,
  and the bundle is published once. The holding preparer was removed about 60 s after the cut, when the original job completed (`exit=137` is the
  Supervisor's removal). Recovery time is bound by the 60 s claim or sandbox lease.
- **Hydration cuts (Worker, Supervisor), R.** The execution runtime was bound and `reserved`, before dispatch.
  The original runtime is reused and dispatched once. Preparation was already committed, so nothing is downloaded again.
- **Execution cut (Worker), R.** The Python and JavaScript start times (19:22:42.681Z, 19:24:03.228Z) precede the kills (19:22:48,
  19:24:11). The original runtime ran to completion (exit 0); the settled answer carries its original start time.
- **Stop, none (user cancellation).** Stop 2.3 s after the preparer started: CANCELLED at 19:25:28, `sandbox.cancelled` on the
  preparation job, preparer removed, no execution job, no user Code. The answer is the registered "Execution was cancelled."
- **Debug, R.** `DEBUG-W` is a race, reported as it happened: the `staging` row became `committed` 34 ms after it was created
  (19:25:40.153 to .187) and the kill landed 3 ms later, so the write had already finished. It is not a cut in the write. The
  deterministic case is `DEBUG-STALL`: this stack's object store (`rustfs`) was paused, the Send created the artifact row in
  `staging` (19:27:55.942), the Worker was killed with the row still `staging` (19:27:57.95), the store was unpaused at 19:28:01,
  the Worker restarted 19:28:04. The replacement claim reconciled the original reservation: one row for the original visit,
  `committed` at 19:28:57.554, attempt 1, no second artifact. Code then ran once (19:29:05.6). The run checks the row state and
  count; it does not read the stored object back.
- **Finite preparation failures, F.** A nonexistent PyPI package, a nonexistent npm package, and a preparer whose network is
  removed during the download (`docker network disconnect` of that one preparer, 19:37:48) all end FAILED within 2-4 s of Send. The
  terminal `execution.failed` event carries code `PIPELINE_CODE_FAILED`, `retryable: false` and the safe message "Code dependency
  preparation failed. Later nodes did not run. Share the support reference with your administrator before retrying." No
  execution job exists, so no user Code ran. The generic runtime text is not used. F is right because retrying cannot help: the
  outcome is a definitive resolver result, not an unknown effect, and nothing is lost.

### Group 2: NATS

| Fault | Execution | Kill → restart (UTC) | Settlement | Answer (stored) | Class |
|---|---|---|---|---|---|
| NATS, Worker down, 2 queued commands | `6e99b808…`, `8597e6ed…` | Worker stopped 19:34:46; NATS 19:34:49 → 19:34:55; Worker 19:34:55 | SUCCEEDED at 19:35:00 and 19:35:03, attempt 1 / epoch 1 each | `JS_PLAIN started=19:34:59.500Z`, `JS_PLAIN started=19:35:02.387Z` | R |
| NATS, running Code job | `b915c849…` | 19:35:23 → 19:35:31 | SUCCEEDED at 19:35:48, attempt 1 / epoch 1 | `JS_PLAIN started=19:35:17.760Z` | R |
| NATS + Worker, running Code job | `bc816224…` | NATS 19:36:09 / Worker 19:36:09 → 19:36:17 / 19:36:18 | SUCCEEDED at 19:37:06, attempt 2 / epoch 2 | `JS_PLAIN started=19:36:03.721Z` | R |

Stream `ELITEA_RT_V1_AGENT`, consumer `elitea-agent-worker-v1`, read from `/jsz` (sequences are stream sequences):

| Case | Before | At the fault | After NATS restart | End |
|---|---|---|---|---|
| Queued | first 35, last 34, 0 messages | last 36, 2 messages, pending 2, ack_pending 0 | identical (same messages and sequences) | first 37, last 36, 0 messages, pending 0, ack_pending 0, delivered 36, redelivered 0 |
| In flight | last 36, 0 messages | last 37, 1 message, ack_pending 1 | identical | last 37, 0 messages, pending 0, ack_pending 0, redelivered 0 |
| NATS + Worker | last 37, 0 messages | last 38, 1 message, ack_pending 1 | identical | last 38, 0 messages, pending 0, ack_pending 0, redelivered 0 |

- **Queued.** Both commands were published (outbox `published_at` set, attempts 1) and sat in the stream with the Worker stopped.
  NATS was killed with SIGKILL, so nothing flushed on shutdown; `sync_interval: always` held. It restarted on the original volume
  `standalone_nats_data` and `/jsz` showed the same two messages. The Worker started 0.3 s later and each run executed once
  (one preparer and one execution container each), about 3 s apart in order. The sequence advanced by exactly 2: no duplicate publish.
  Every JavaScript job runs a preparation (the native path), so each run shows one preparation row even without imports.
- **In flight.** The job was `dispatched` and the user Code had started (19:35:17.760) when NATS was killed. The Worker kept its
  claim (attempt 1) and the original runtime finished at 19:35:47, after NATS was back. The message stayed unacknowledged across
  the restart, was acknowledged after settlement, and the stream drained.
- **NATS + Worker.** The Worker's claim expired (`LEASE_EXPIRED`), the replacement claimed attempt 2 / epoch 2 and collected the
  original runtime's result; user Code started once (19:36:03.7, before both kills). The unacknowledged message was acknowledged
  after settlement and the stream drained.
- Redelivery counts are 0 in all three cases: `num_redelivered` did not rise, so recovery did not depend on redelivering a
  message after the ack wait.
- **Class R** for all three: the command is durable in JetStream and in `command_outbox`, the work is owned by the PostgreSQL claim
  and the Supervisor ledger, and a NATS loss delays acknowledgement but does not re-run anything.

**Observations, not defects of this branch.**
- Every recovery waited for a 60 s lease (claim lease for Worker and Main loss, sandbox lease for Supervisor loss), as in the
  earlier matrix. A Supervisor cut during preparation did not trigger a Worker takeover (attempt 1).
- The `FAILED` settlement row has an empty `error_code`; the typed code is in the projected `execution.failed` event. The
  support reference is not in the event payload or the replay row; this run checked the safe message and the response message id
  only, and the planning session's browser spot check should confirm the reference renders live and after reload.
- The join between `sandbox_dispatches` and `sandbox_jobs` on `(tenant_id, project_id, request_digest)` is not unique per
  execution: two executions with identical Code source share the digest and each other's jobs. Harness queries must
  either use unique sources (as here) or bound `j.created_at >= d.created_at`. DESIGN.md I2/I6 should record this.
- The Docker host dropped to 23 GiB free during the session (other sessions' builds included); nothing here builds any more.

**Reproduction.** The scenario harness (private, not shipping) is `h.py`, `scen.py` and `nats.py` in the session scratchpad;
per-run results (JSON, with the invariant rows and container event times) are in `runs/<scenario>/`.

**Limits.** This closes the preparation fault outcomes and the original debug-writer reconciliation for the Docker backend, and the
queued, in-flight and combined NATS cases with the original broker storage. It does not cover: Kubernetes preparation and its
NetworkPolicy enforcement; a Main loss during hydration or execution of a prepared job; TypeScript or Rust preparation; the
debug object read-back; a registry outage longer than the preparation deadline; or a live-browser check. Runs use the Chat
Send API, not browser clicks.

**Planning-session browser spot check** (stack `code1084` at `e7a291985`, 2026-10-09):
- Pipeline 167 (`micropip.install("python-slugify==8.0.4")`) answered live with `PY_PKG … slug=hello-elitea-1084`.
- Pipeline 207, created in the UI, installs `elitea-nonexistent-package-1084==9.9.9`. It showed *"Code dependency
  preparation failed. Later nodes did not run. Share the support reference with your administrator before retrying."*
  Its support details give `PIPELINE_CODE_FAILED` and Message ID `0d57d804-…`, with a copy button.
- After reload, Run history keeps the run as `FAILED · 4s · TERMINAL`.

## Preparation-failure recovery hang (crash suite CR-WK-P09-hyd)

Findings from the crash suite (2026-10-12): executions `097ca8e5a5e41a99623f98fd546bacad` (CR-WK-P09-hyd), `4e160a9d8aeca4f419e2517aff77fa3e` (CR-SK-P09-hyd r2) and `0b19b052f3345b52d60c61116ead7b47` (CR-MK-P09-hyd r1), plus two Kubernetes matrix findings. Part letters A to I match the work brief; Part J follows the follow-ups. The trigger in the first case was a claim lease lost during Python dependency preparation, then a redelivery. Neutral wording: no host fault is the product cause.

**Business behavior kept from the current platform.** A Code job that fails is a typed, readable failure (`PIPELINE_CODE_FAILED`, preparation variants) with a support reference. Nothing is run again from scratch. **Deliberately not ported:** the current application's generic "operation failed" answer and its lost-on-restart behavior.

### Mechanism and changed paths

| Part | Defect | Mechanism (`path:line`) |
|---|---|---|
| A | A recovered `Running` journal made every pre-dispatch failure an unknown external effect, so a lost preparation job paused for reconciliation. | `src/agents/graph/code_attempt_remote.rs:703` `prior_dispatch_possible`, called at `:41`: the dispatch ledger decides. No `sandbox_dispatches` row for the execute activation means no submission was ever sent (the row is registered before the first submit). A row, a missing scope or a lookup error keep the conservative answer. |
| B | Worker built a `failure_class: unknown` reconcile receipt that Main, the JSON schema and the web parser refused (`PROTOCOL_VIOLATION`, no operator card). | Decision: the receipt is legitimate (see "Decision B"). `src/agents/graph/node_recovery_receipt.rs:49` `validate` (unknown only on reconcile), `services/elitea-main/internal/domain/noderecovery/contract.go` `Receipt.Validate`, `libs/jsonschema/runtime/v1/node-recovery-required.schema.json`, `apps/elitea-web/src/shared/lib/nodeRecovery.ts`. |
| C | A pending frame Main refused with a bound, non-retryable `PROTOCOL_VIOLATION` looped forever. | `src/transport/output_session.rs` `OutputSessionError::FrameRefused` (bound, non-retryable, zero credit, desired state unspecified); `src/transport/output_grpc.rs` `ProgressRejectionWinner::FrameRefused` (replacement authority, settles `Internal`); `:394` `replace_pending_with_failure`; `src/execution/output_delivery.rs:2678,2737` `replay_checkpoint_progress` returns the bound rejection and `settle_rejected_checkpoint_progress` replaces the exact frame at its own sequence; `agent_delivery_processor.rs:301` settles through `recover_accepted_terminal` (one settlement, double-ack retirement). Main logs the reason of a refused claim: `services/elitea-main/internal/transport/runtimegrpc/control/server.go` `logClaimRejection`. |
| D | Stop never settled behind a pending frame (`agent_delivery.output_recovery_pending`, retryable, forever). | `output_delivery.rs:2807` `prepare_empty_recovery(.., settle_pending)`: one unaccepted nonterminal frame at the sequence after Main's watermark no longer blocks; `PreparedAgentOutput::persist_terminal` replaces that exact frame with the canonical cancelled (or internal) terminal; a frame Main already covers is reconciled away first. |
| E | Hydration discarded the supervisor and content error kind; the run ended as a generic internal failure with nothing logged. | `src/agents/graph/code_preparation.rs:358` `hydrate_python_dependencies` returns a typed `CodePreparationFailure` and logs a static reason (`code_hydration_failed`, `code_hydration_unconfirmed`); `:467` `intent_disposition` retries transient content or Main errors, `:477` `hydration_reason`; supervisor side `src/sandbox/service.rs:376,592` `sandbox_hydration_refused` with `supervisor_error_reason`. Settles `pipeline.code_preparation_failed` or `_unconfirmed`. |
| F | A job reserved and hydrated, then failed before any dispatch, kept a live runtime until the one-hour hydration retention. | `code_attempt_remote.rs:642` `release_undispatched`: the claim-bound stop (`sandbox_authority.rs` `release_undispatched_sandbox_job`), only when the replay safety proves no dispatch. Supervisor reap is the existing cancellation path. |
| G | No HTTP/2 or TCP keepalive: a force-deleted Main Pod left the connection in conntrack for about 16 minutes. | `src/transport/keepalive.rs:15` constants and `:49` `with_keepalive`, applied in `bootstrap.rs` `connect_private_grpc` (control, output, sandbox profiles), `transport/input_content.rs`, `transport/runtime_context.rs`, `transport/openai_compatible_facade.rs`, and the supervisor's `sandbox/dependency_content.rs` reqwest client. |
| H | A Code job in a definite `failed` phase paused for operator reconcile. | `code_attempt_remote.rs:758` `unreceipted_platform_effect_possible` drains the platform-call journal after the terminal phase; no unknown or unreadable step means the outcome is known (`NoExternalEffect`, `pipeline.code_failed`, class F). |
| I | After a Main outage and a takeover, claim 2 replayed claim 1's spooled frame under the old fence (`STALE_FENCE`) forever. | `src/spool.rs:434` `retire_pending` (an encrypted `.retired` record, never replayed, counted, bounded to 16), `output_grpc.rs:426` `retire_superseded_progress`, `output_delivery.rs:2871` `retire_superseded_frame` used by `prepare`, `prepare_checkpoint` and `prepare_node_recovery`; `protocol/control.rs` `supersedes_output_fence`. `code_attempt_remote.rs:724` `retry_transient_main` makes Main being down during intent admission or finalization retry until the deadline. |

### Decision B (contract parity)

`failure_class: unknown` with an unknown or unclassified external effect is a legitimate reconciliation receipt, so Main accepts it and the other contract surfaces follow. Reasons: the Worker deliberately emits it (the journal tests pin it, and the direct-tool path produces it for an unclassified stage); the ledger already handles it end to end (`reconcile_verified_no_effect` plans a terminal `not_retryable` stop, a committed owner result resumes); and refusing it would turn an unknown external effect into a plain failure, losing class C (never repeated blindly, reconciled from owner proof or escalated). `unknown` is valid only with `effect_reconciliation_required` and `["reconcile"]`; a retry or a result resume with `unknown` stays refused, and an `unknown` failure with no effect uncertainty stays terminal. One set of golden vectors, `libs/jsonschema/runtime/v1/fixtures/node-recovery-required-v1*.json` (a `.invalid.` name must be refused), is read by the Go domain test, the Go schema test and the Worker test, so drift fails on both sides.

### Decision I (superseded claim's frame)

The spool stream is bound to the fence, and only terminal frames have a rebinding protocol. A progress frame of a superseded claim describes an attempt Main has replaced, and the resumed run emits its own events from Main's watermark. So it is **retired with a durable, encrypted record, not re-fenced and not replayed**. The route that owns the claim then decides the outcome: resume (checkpoint and node routes), or settle (output recovery: the canonical terminal replaces the pending frame under the new fence). `STALE_FENCE` therefore never reaches a loop, for progress frames; for a terminal frame the existing terminal rebinding is unchanged. The brief's "settle `STALE_FENCE` as a typed failure" is covered where the run cannot resume (output recovery); a resumable run is not failed. Open: the retired record is a spool file and a log event, not a database row.

### Tests (counts)

| Case | Test | Result |
|---|---|---|
| 1 preparation failure in recovery, no dispatch row, settles typed once | `node_recovery_code_owner_tests.rs` `preparation_recovery::preparation_failure_in_recovery_without_a_dispatch_row_settles_typed_once` (real PostgreSQL) | passes; mutation (decision reverted to the error kind) fails it |
| 2 dispatch row present stays reconcile, receipt Main accepts | `preparation_failure_in_recovery_with_a_dispatch_row_stays_reconcile_with_an_accepted_receipt` (real PostgreSQL; the emitted receipt equals the golden vector `reconcile-unknown-class` once identities are normalized) | passes |
| 3 shared vectors, both sides | Go `TestNodeRecoveryReceiptSharedGoldenVectors`, `TestNodeRecoveryRequiredSchemaMatchesSharedVectors`; Rust `worker_and_main_agree_on_every_shared_receipt_vector`; web `nodeRecovery.test.ts` | pass (6 valid, 8 invalid vectors) |
| 4 refused frame settles typed, retired, not resent | `output_delivery_tests.rs` `a_frame_main_refuses_settles_an_internal_failure_once_and_is_not_resent`, `frame_refused_by_main_is_retained_as_a_rejection_and_never_resent`; transport `bound_protocol_violation_mints_a_frame_refused_rejection_and_nothing_else_does` (bound, retryable, foreign stream, credit) | pass |
| 5 stop with output recovery pending settles CANCELLED once | `a_stop_settles_cancelled_once_while_output_recovery_is_pending`, `a_stop_wins_over_a_pending_progress_frame_and_settles_once` | pass; the first fails when `settle_pending` is off |
| E hydration cause kept, transient errors retry | `code_preparation.rs` `hydration_reason_tests`, `code_attempt_remote.rs` `transient_main_errors_retry_until_the_deadline_and_others_do_not` | pass |
| F orphan reaped once | `docker_hydration_deadline_tests.rs` `hydrated_never_dispatched_job_is_reaped_once_after_a_settled_failure` (real PostgreSQL: reserved, `runtime_id` set, no `dispatched_at`; termination and cleanup once, repeat adds none) | passes |
| G keepalive | `keepalive::tests` (schedule against the claim lease, every production endpoint applies it), `client_disconnect_tests.rs` `black_holed_peer_fails_within_the_keepalive_bound_and_the_channel_reconnects` (proxy that stops forwarding without closing: fails `Unavailable` inside the bound, same channel reconnects) | pass |
| H terminal failed phase is known | `terminal_runtime_failure` (typed, no operator; unreceipted effect still reconciles; journal drain truth table) | pass |
| I takeover never replays the old frame | `a_takeover_never_replays_a_superseded_claims_pending_progress`, `output_recovery_replaces_a_pending_superseded_frame_and_settles_once`, spool `a_retired_frame_leaves_replay_but_stays_as_an_encrypted_record` | pass |

Suites (2026-10-12, this branch): Worker `cargo clippy --offline --locked --all-targets --all-features -- -D warnings` clean; `cargo test --offline --locked --all-targets --all-features` 1996 passed, 0 failed, 77 ignored (lib 1900 passed). The real-PostgreSQL cases above pass against a disposable PG18 with `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` and `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`; ignored suites that also need Docker, Kubernetes or `ELITEA_TEST_BUNDLE_TLS` were not run. `libs/rust`: the same clippy command is clean and `cargo test --offline --locked --all-targets --all-features` passes 1611 tests, 0 failed (no `libs/rust` code changed). Web `vitest` `nodeRecovery.test.ts` 29 passed. Main `go test` for `noderecovery`, `runtimecontracts`, `runtimegrpc/...` and `infra/storage` passes in `golang:1.26.9-bookworm`.

### Recovery classes after the fix

| Component x phase | Before | After | Notes |
|---|---|---|---|
| Worker x P09, claim lease lost during preparation, redelivered | L (hang) | F | Typed `pipeline.code_preparation_failed`, support reference, one settlement. If a dispatch row exists: C (operator card Main accepts). |
| Worker x P09, hydration fails (supervisor refusal, content error) | F, generic cause lost | F typed | `pipeline.code_preparation_failed` or `_unconfirmed`; transient Main or content errors retry (I, R). |
| Worker x P10, runtime `failed` phase | C (operator for a user code error) | F | Known outcome; C unless Main's platform-call journal proves nothing unresolved (see "Review fixes"). |
| Worker x P14 / Main x P14, refused or superseded pending frame | L (infinite redelivery) | F or R | Refused: F (internal failure, once). Superseded claim: frame retired, run resumes (R) or settles. |
| Main x P14, stop with a pending frame | L | F (cancelled) | One settlement, writer fencing unchanged. |
| Worker x Main channel, dead peer | L for about 16 minutes | R | Detected within 20 s by HTTP/2 PING (15 s interval, 5 s timeout; 60 s lease). |
| Supervisor x P09, job hydrated and never dispatched after a failure | L (hour-long orphan) | I | Released at once; the retention sweep remains the backstop. |

### Follow-ups

- Bounded redelivery or backoff on the NATS consumer (`max_deliver=-1`, no backoff in `bootstrap.sh`): not changed here; with the settle paths above a poisoned frame no longer redelivers forever.
- The retired record is an encrypted spool file plus a log event; a database row is not written.
- Release on failure (F) is best effort; a failure after a possible dispatch is never released here.
- The root cause of the `4e160a9d` (CR-SK-P09-hyd r2) generic failure is not proven from the retained logs: the error text was discarded (E). The code paths that could end a hydrated run within 0.2 s of the last lease release now keep their kind. A rerun on the fixed images will show it in `code_hydration_failed` or `code_main_call_retrying`.
- Deployed re-proof of CR-WK-P09-hyd, CR-SK-P09-hyd and CR-MK-P09-hyd (x3) and a Kubernetes force-delete run for G.

### Part J: HITL continuation refused when Main owner recovery is on and a child stores `openai`

Found on a deployed stack with `ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED=true` (the combination Helm requires for node recovery with Code). Pipeline 138 "Gate5 agent variable parent" has two application nodes calling agent 137, whose stored `agent_type` is `openai`. Answering the first `ask_user` card returned 422 `unsupported_agent_execution`.

**Business behavior kept from the current platform.** A paused turn continues from its original captured source, with the application handles the turn was admitted with; an edited child never replaces them. **Not ported:** nothing; this is a parity defect of the new platform.

**Mechanism.**

| Step | Path |
|---|---|
| Continuation restores the original source, only when original-source capture is configured (`originalSources != nil`, which is why stacks without owner recovery worked) | `services/elitea-main/internal/application/agentexecution/original_source_continuation.go:30` `restoreContinuationSource`, called from `continue.go:752` and `:812` |
| Strict comparison of each captured application tool with the tool admitted at turn start | `original_source_continuation.go` `restoreOriginalApplicationHandles` (`bytes.Equal` of the two frozen handles) |
| The captured source keeps the stored spelling `"agent_type":"openai"`; admission renamed the admitted copy to `"agent"` | `tools.go:175` and `:764-781` `normalizeCurrentAgentRuntimeProfile` / `normalizeCurrentAgentTypeField` |
| Fix: both sides pass through `normalizeCurrentAgentTypeField` before the comparison. It is the only field `normalizeCurrentAgentRuntimeProfile` rewrites on an application tool, so no other comparison has the blind spot (`context_policy.go:123` compares settings, which admission does not rename). The comparison stays strict for id, version id, application id, settings, variables and name | `original_source_continuation.go` (two call sites) |
| Every refusal in the restore path carries a fixed reason, and the two callers return it instead of the bare sentinel, so a log names the site. `errors.Is(err, ErrUnsupportedCurrentAgentStart)` still maps to 422 | `original_source_continuation.go`, `continue.go:754,817` |

**Provenance.** The comparison came with `f8a1499d1` (original-source continuation). It did not account for the stored-to-runtime rename of `0f682f7c9` (#1032). Pre-existing on `main`.

**Web (same flow).** `resumeHitlOverRest` returned only `outcome.started`, so a `rejected` outcome fell through to the socket stub and the user read "The message was not sent: no chat connection is available" instead of the server's reason. It now returns the outcome; `continueHitl` restores the card with the server message and does not emit on a rejection (`apps/elitea-web/src/widgets/chat-box/ui/hooks/useChatBoxHandlers.ts` `resumeHitlOverRest`, `continueHitl`), the way `continueTokenLimit` and the authorization flow already do.

**Tests.**

| Case | Test | Result |
|---|---|---|
| Captured `openai`, admitted `agent` (and a legacy admitted `openai`) restore; handle is in runtime spelling | `original_source_agent_type_test.go` `TestRestoreOriginalApplicationHandlesComparesInRuntimeSpelling` (admitted handles are produced by the production normalizer) | passes; fails with the two normalize calls removed |
| Still strict: changed version id, application id, name, agent type beyond the rename, variables, each with its named reason | `TestRestoreOriginalApplicationHandlesStaysStrictBeyondAgentType` (5 cases) | pass |
| `ContinueCurrentAgent` with original sources configured and an `openai` child application accepts; child is `agent` on the wire | `TestOriginalSourceContinueAcceptsStoredOpenAIChildApplication` (actual snapshot freezer, real start then continue) | passes; fails on the old code |
| A drifted handle is refused with the site named | `TestOriginalSourceContinueRefusalNamesItsSite` | passes |
| Web: a 422 on HITL continue shows the server message, restores the card, never emits; an absent route still falls back to the socket | `useChatBoxHandlers.test.ts` `continueHitl — a continuation the route refused` (2 tests) | pass; the first fails on the old code |

Suites (2026-10-12): Main `go build ./...`, `go vet`, and `go test` for `internal/application/agentexecution` (221 top-level tests, 0 failed), `internal/api/v2/agentexecution` (59) and `internal/runtimecomposition` (279) pass in `golang:1.26.9-bookworm`. Web `tsc --noEmit` and lint clean; `useChatBoxHandlers.test.ts` 46 passed; the whole `src/widgets/chat-box` folder 31 files, 346 tests passed.

**Recovery class.** Main x P06/P07, a HITL or authorization continuation with owner recovery on, for a turn containing a stored-`openai` application tool: was F with a generic refusal (the run stayed paused, no way forward); after the fix R, as for every other agent type. The refusal reason is now named in the Main log.

**Real-browser proof (2026-10-12, after the second residual below).** Own stack `elitea-code1084`
(`http://code1084.localhost:18300/app/`), restored from the real-model dump. Production configuration: Worker
`agent_node_recovery` and `agent_model_checkpoint_recovery` true, Main Code owner recovery and code consumers on,
Code sandbox supervisor with Python preparation. Images: Main and Web `code1084-3c5c88096` (Main `sha256:17cc5231c3f7…`,
binary contains `resource_project_id=$1::integer`, the previous image does not; Web `sha256:6d3e05a73f51…`), Worker
and Supervisor `code1084-e9cdbaa1f` (Worker `sha256:326b7589795c…`). Agentstate migration 0014 applied to this stack
only, after a local snapshot of both databases.

Before the fixes the same flow failed on this stack with every image combination tried (merged-main Worker, pre-merge
Worker, #1084 Main/Web, pristine `main-c0f2e5f9b-verify` Main/Web): 422 on answering card 1, shown as "no chat
connection".

Chat 933, pipeline 138 "Gate5 agent variable parent 20260930" as the active participant:

| Step | Execution | Settlement |
|---|---|---|
| Message → card 1 (`override_call` asks) | `95069a39ff955e391027b9152d202ed5` | SUCCEEDED, attempt 1 |
| `docker restart` of the Worker while card 1 is pending, then a full reload: card 1 still shown | — | — |
| Answer card 1 ("Databases") → `override_call` completes, card 2 (`defaults_call`, different options) | `e7a0af51812504cb1bcd7b39b22e1a91` | SUCCEEDED, attempt 1 |
| Answer card 2 ("Hardware") → final answer | `e42593fb89b95a6b63e2f8ae11d2cf73` | SUCCEEDED, attempt 1 |

The final answer has an OVERRIDE CALL section (audience operators, tone brief) and a DEFAULT CALL section (audience
users, tone formal), each with its own variable bindings. After a full reload the same single answer shows and no card
remains. Main logged no `current agent continuation failed` and the Worker logged no ERROR in the window. Fixture: the
existing pipeline 138 and agent 137 rows from the real-model dump (created through the UI on 2026-09-30, per the
#1199 source mapping); nothing was created or edited through the database.

#### Part J, second residual: the original-input lookup never ran on PostgreSQL

On the rebuilt stack the same flow (pipeline 138, chat 933, answer card 1) still answered 422, now with a named reason: `original source restore: saved child scope is not authorized`. It was not a child check. The name came from `scope.ErrDenied`, the one sentinel the root owner returned for every failure, and the deployed trigger was the first statement of the restore, not the comparison fixed above.

| Step | Path |
|---|---|
| The lookup compared the INTEGER column `execution_jobs.resource_project_id` with `$1::bigint::text`. PostgreSQL refuses it (`operator does not exist: integer = text`) | `services/elitea-main/internal/infra/db/repos/execution_source_definitions.go` `RestoreOriginalRootContinuation` (SQL: `j.resource_project_id=$1::integer`, `j.actor_id` stays text) |
| The query error was returned as `scope.ErrDenied`, so an outage or a broken statement read as an authorization refusal | same function: a failed query or cursor is now `scope.ErrUnavailable`; the cause goes to the log, never to the caller; a missing or second row stays `ErrDenied` |
| Why no test saw it | the only tests of this function use a scripted store that never parses the SQL; the statement had never run on a database since `f8a1499d1` (2026-10-05) |

Evidence on the deployed data (read-only): the response message `bf7bacc5-998e-5380-a039-9cbd9db195bf` has exactly one `chat_predict` row for its stream and generation; the root source receipt and captured definition join and `DecodeSourceWire` verifies for project 2, actor 3; `originalContinuationApplicationTools` yields the one admitted handle (agent 137 version 144); `restoreOriginalApplicationHandles` accepts that handle against the captured pipeline. The same statement with the integer cast returns that one row. Every later step of the restore therefore passes for this data; the cast was the only failure.

Authorization is unchanged and strict: the lookup still binds the actor, the project, the stream, the response message and the generation, and requires exactly one original. A foreign actor, a foreign project, another stream, message or generation each stay `ErrDenied`.

**Web (same flow, second layer).** The 422 still showed "no chat connection is available" because `continueStreamedExecution` in `useChatBoxSend.ts` returned the route's answer only for the static contract; every other contract went through the boolean `resume`, so a rejected outcome reached the handlers as `no-transport` and `useChatBoxHandlers.ts` `continueHitl` (fixed in `acd0cf4fd`) never saw `rejected`. The served bundle (`usePipelineEditorLifecycle-7eKhspa5.js`) contains the `acd0cf4fd` handler change and the boolean collapse together. The same collapse hid refusals of the authorization and output-limit resumes. `continueStreamedExecution` now returns `resumeDetailed` for every contract. A 404 or 405 is still `no-transport`, so the socket fallback is unchanged; any other HTTP status, an auth failure, a network failure or a cancel is now `rejected`, as on the start path.

**Tests (second residual).**

| Case | Test | Result |
|---|---|---|
| Real, migrated PostgreSQL: the admitted saved pipeline restores (source reference, project, actor, one application handle) | `execution_source_continuation_postgres_integration_test.go` `TestPostgresOriginalRootContinuationRestoresTheAdmittedSavedPipeline` | passes; fails on the old SQL with the observed error |
| Another actor, project, stream, message or generation is refused as `ErrDenied` and returns nothing | `TestPostgresOriginalRootContinuationRefusesEveryOtherRelation` (5 cases) | pass |
| A statement that cannot run is `ErrUnavailable`, not `ErrDenied`, and the error text carries no SQL | `TestPostgresOriginalRootContinuationReportsABrokenLookupAsUnavailable` | passes; fails on the old code |
| Web: a 422 on the HITL, authorization and output-limit resumes reaches the caller as `rejected` with the server's message; 404 stays `no-transport`; 200 is `started` | `useChatBoxSend.runEvents.test.tsx` `a resume the continuation route answers` (4 tests, real transport and `msw`) | pass; the first three fail on the old code |

Suites: Main build and vet clean; `internal/application/agentexecution` 221 passed, `internal/api/v2/agentexecution` 59, `internal/runtimecomposition` 279 (10 skipped, as before), `internal/infra/db/repos` 446 passed with 566 skipped without a database (the three new tests run only with `ELITEA_TEST_DATABASE_URL`, run here against `pgvector/pgvector:0.8.1-pg18-trixie`; the whole package passed there in 82 s). Web `tsc --noEmit` clean, `oxlint --deny-warnings` clean on the changed files, `src/widgets/chat-box` 31 files and 350 tests passed.

**Recovery class.** unchanged from Part J (F before, R after) for this flow; the refusal now also names an outage as unavailable.

**Not verified.** The continued run itself (Worker side) and a real-browser answer on the rebuilt stack; both need the fixed Main and Web images deployed, which this change does not do. `unsupportedCurrentAgentStart` still maps `ErrUnavailable` to the same 422 as a refusal; a 5xx for an unavailable owner would need a route change.

## Review fixes (code review of PR #1084)

A read-only review of parts A to I found nine items. This section records the fix for each, with the evidence. Part J above is a separate change.

### 1. H lost class C (HIGH)

**Defect.** After a terminal `failed` runtime the Worker "drained" `step_code_platform`, but Main's `CodePlatformPump.Step` answers idle once the retained runtime is completed, so the drain never read Main's journal. Claim 2 after a takeover never saw claim 1's call, `prior_dispatch` was ignored, and the node settled a plain failure. The pump future was also dropped mid-`service.Call` when the submission won.

**Mechanism (changed paths).**

| Step | Path |
|---|---|
| Main: read-only, fenced, authorized query of the platform-call journal, `POST .../code-platform/effects`. It runs inside `WithRegisteredCodePlatformIntent`, so claim fence, lease, RUNNING state, registered intent and project scope are checked in the same transaction as the read. `possible` iff a call is `dispatching` or `uncertain`, or a call's toolkit child is not terminal. No registration row means no frame was ever read: `none`. A missing reader, or any error, is an error, never `none` | `services/elitea-main/internal/infra/storage/runtime_code_platform_effects.go` (`PostCodePlatformEffects`, `CodePlatformPump.Effects`), `internal/infra/db/repos/code_platform_calls.go:487`, wiring `internal/runtimecomposition/code_consumers.go`, contract `libs/proto/elitea/runtime/v1/code_platform.proto` |
| Worker client; only an explicit `none` clears the effect, a refusal or unreadable answer (after a 30 s retry of transient Main errors) keeps it possible | `src/transport/code_platform_content.rs:72` `code_platform_effects`, `src/agents/graph/code_attempt_remote.rs:809` `platform_effects_possible` |
| Reusable step tracking: a pump dropped inside a Main step marks the tracker interrupted. When the terminal receipt wins with a step in flight, the step may finish for up to 5 s (`PLATFORM_STEP_SETTLE_GRACE`), so the pump is dropped at a boundary | `libs/rust/agent-runtime/src/graph/code_platform_drive.rs` `StepTracker`, `drive_tracked` (ADR-0027: reusable logic in the library) |
| Decision: `effect_possible = platform_client && (unknown call seen by the pump \|\| step interrupted \|\| journal answer is not an explicit none)`. A runtime without the platform client has no network and asks nothing | `code_attempt_remote.rs:618` |

**Why the journal query and the pump grace are both needed.** The journal answers for claim 1's calls and for calls the pump never observed. It cannot settle a step that is still running on Main for this claim, because Main's handler is not serialized with the query; the grace lets that step finish, and an unfinished one is treated as possible. A stale claim cannot begin or dispatch a call (`lockCodeCallClaim` needs the live current claim), so after claim 2 holds the lease the journal is stable.

**Decisions not taken.** A `prepared` call is `none`: Dispatch was never granted, so no effect ran (a test pins this; counting it as an effect also failed the journal test). The existing `ReadOriginalCodeBrokerEffects` was not reused: it errors without a registration row and counts any observed call, which is correct for the supervisor's no-dispatch proof but would turn every receipted read into an operator card.

### 2. G keepalive against Main's enforcement policy (HIGH)

Worker pings every idle channel; Main enforced `MinTime 30 s, PermitWithoutStream false`, so grpc-go counted strikes and sent GOAWAY `too_many_pings` after about 40 s idle, aborting idle control and output connections and long silent output streams. Now: Worker interval 15 s, timeout 5 s, idle pings on (`src/transport/keepalive.rs`, a dead peer in 20 s against the 60 s lease); Main `RuntimeKeepaliveMinTime` 10 s and `RuntimeKeepalivePermitWithoutStream` true (`services/elitea-main/internal/transport/runtimegrpc/keepalive.go`), one constructor used by every Main gRPC server. Both private servers are created only after `validateServerTLSConfig` (TLS 1.3, `RequireAndVerifyClientCert`), so only mTLS-verified internal peers can ping while idle. Audit: Main's content server is `net/http` HTTP/2 (no strike policy); the sandbox supervisor and `elitea-vector` are tonic servers without enforcement; the subapp-host SPI is not reached by the Worker. Tests: Worker `keepalive.rs` (3 constant and margin tests plus the wiring guard), Main `keepalive_test.go` (values and margin; Worker constants read from the Rust source; every server uses the one constructor; a real grpc-go server accepts idle pings for more than 3 strike windows with scaled time; three negative controls are closed with `too_many_pings`).

### 3. Retired-frame cap wedged the spool (MEDIUM)

The rename happened before the cap check, so at 17 every `entries()` failed (open, pending, acknowledge, usage). Records were never deleted, and `renameat` overwrote an existing `N.retired` when a later claim reused a sequence. Now (`src/spool.rs`): the record name is `{sequence}-{claim_attempt}-{epoch}.retired` and is created with `link` then `unlink` (no replace; a crash between the two resumes); the cap is checked before the link and **prunes the oldest record with a logged event** (chosen over failing the execution: a record is audit evidence, every retired frame's digest is already in the durable log, and failing an execution on record capacity would turn a recoverable takeover into a loss); enumeration never fails on the number of records; records are deleted when the terminal frame is acknowledged (`output_grpc.rs` `apply_ack`) and swept after 7 days at open. Legacy `{sequence}.retired` records still enumerate and count as the oldest. Tests: 6 spool cases (cap, cap+1, 3x cap, sequence reuse across claims, interrupted retirement, settlement and sweep, legacy) and one output-session case.

### 4. A and H were not tested through production wiring (MEDIUM)

New `src/agents/graph/node_recovery_code_wiring_tests.rs` (feature `sandbox-supervisor`, real PostgreSQL, ignored by default like the other database tests). It drives the real Code node, recovery journal and `RemoteCodeRuntime::execute_recovery_attempt`, with a Main content fixture, a real tonic Main control server and a real tonic supervisor server on loopback. 13 cases (see the recovery rows). **Revert checks:** (a) `prior_dispatch = authority.recovering_started()` (reverting the proof at `code_attempt_remote.rs:41-55`) fails 2 cases (`recovered_attempt_without_a_dispatch_row_fails_typed_before_dispatch`, `recovered_attempt_with_a_registered_but_unsubmitted_row_fails_typed`); (b) `effect_possible = platform_client || unknown` (reverting the proof at `:583-603`) fails 4 cases; (c) removing the interrupted-step check fails `a_platform_step_dropped_mid_flight_keeps_the_effect_possible`. Each was restored.

### 5. A missed a lease lost during hydration (LOW)

The dispatch row is written before hydration, so a row did not mean a submission. Now the row is registered `submission_pending` and a durable marker is written just before the first execute submission (`code_attempt_remote.rs:214` and `:343`; `src/sandbox/dispatch.rs`). `prior_dispatch_possible` reads the marker. Migration `services/elitea-main/migrations/agentstate/0014_sandbox_dispatch_submission_marker.sql` adds `submission_pending BOOLEAN NOT NULL DEFAULT FALSE`: rows from earlier Workers, and any registration that does not opt in, read as submitted, so a mixed-version rollout stays conservative. **The migration is unmerged; its number must be re-checked at merge and it must not be applied to a shared stack before then.** Tests: `recovered_attempt_with_a_registered_but_unsubmitted_row_fails_typed`, `recovered_attempt_with_a_submitted_row_keeps_the_effect_unknown`, `the_submission_marker_is_durable_before_the_supervisor_sees_the_job` (the fixture supervisor reads the row when the submission arrives), `the_submission_marker_reads_conservatively_and_moves_one_way`.

### 6. F only released on the recovery path (LOW)

The raw `execute` path now releases a job hydrated for a failed attempt (`src/agents/graph/code_remote.rs:225` `hydrate_or_release`, shared `release_hydrated_job`). Test: `raw_path_hydration_failure_releases_the_hydrated_job` calls the helper the raw path uses; it does not run the full raw `execute`, because reaching hydration there needs a dependency-preparation exchange with the supervisor that the fixture does not serve.

### 7, 8. Web parser and Main log (LOW)

`apps/elitea-web/src/shared/lib/nodeRecovery.test.ts` reads `libs/jsonschema/runtime/v1/fixtures/node-recovery-required-v1*.json` and refuses every `.invalid.` vector, like Go and Rust. Main `logClaimRejection` logs one of 11 fixed reason strings and never the error text; its test feeds driver-like and payload-like text and asserts it is absent.

### 9. Documentation accuracy (LOW)

E: Main down for longer than the hydration deadline settles the terminal `pipeline.code_preparation_unconfirmed` (class F), it is not resumed or retried. H on Kubernetes relies on the execution namespace default-deny NetworkPolicy. The chart renders `elitea-code-deny-network` whenever `sandboxKubernetes.enabled`; `deploy/helm/tests/render-sandbox-kubernetes.sh` already asserted its content and now also asserts that it is the only NetworkPolicy in that namespace (policies are additive). The rows in `recovery-guarantees.md` are updated.

### Performance / Durability / Resilience / Security

| Property | Mechanism | Proving test | Measured |
|---|---|---|---|
| Performance | The journal query is one `SELECT EXISTS`, asked once per failed platform runtime; the grace adds at most 5 s only when a step is in flight; spool retirement enumerates one bounded directory | `failed_runtime_with_a_clean_journal_settles_typed_without_an_operator_card` asserts one query; `a_platform_step_dropped_mid_flight_keeps_the_effect_possible` asserts the wait is at least the grace | Wiring suite 13 cases in about 7 s |
| Durability | Marker written before the effect; the journal read is claim-fenced; class C is never lost | the wiring suite; Main real-PostgreSQL journal and matrix tests | all pass |
| Resilience | Everything bounded: proof retry 30 s, step grace 5 s, record cap 16, retention 7 days; typed outcomes; idle channels survive | spool, keepalive and wiring suites | all pass |
| Security | The effects route is claim-bound and answers only `none` or `possible`; authorization in the effect transaction; keepalive relaxed only on mTLS-verified private servers; no payload text in claim-rejection logs | Main authorization matrix (16 refusal cases) and body pins; `keepalive_test.go`; `claim_rejection_log_test.go` | all pass |

### Tests (counts)

Worker `cargo test --offline --locked --all-features --no-fail-fast`: 2035 passed, 0 failed, 90 ignored across all targets (lib 1933). The 90 ignored need a disposable database or a container runtime; the 15 ignored cases of this change (13 wiring cases, plus 2 `preparation_recovery::*`) passed against a disposable PostgreSQL 18 (`--include-ignored`). Worker `cargo clippy --offline --locked --all-targets --all-features -- -D warnings` clean. `libs/rust`: clippy `--workspace --all-targets --all-features -D warnings` clean; `cargo test --workspace` plus `-p elitea-agent-runtime --features toolkit-sql` and `--features test-preserve-order,toolkit-sql` pass (agent-runtime 736 tests, 4 new step-tracker cases). Main: `go build ./...`, `go vet` and `go test` for storage, repos (full package against a disposable PostgreSQL), runtimecomposition, runtimegrpc/..., `cmd/elitea-runtime-material` and `cmd/elitea-main` pass. Web: `tsc --noEmit` clean, `nodeRecovery.test.ts` 30 passed. Helm: `render-sandbox-kubernetes.sh` passes.

### Follow-ups

- Python proto stubs for `code_platform.proto` (`libs/proto/gen/python/...`) are not regenerated: the pinned grpcio-tools is not installed. Run `scripts/contract/generate_proto.sh` before the CI regenerate-and-diff.
- Deployed re-proof (real browser, reload) of the typed-failure and operator-card cases: pending on a rebuilt stack. Fixtures in the tests are created by the test code, not through the UI.
- Migration 0014 number to confirm at merge.
- A raw-path end-to-end hydration test needs the dependency-preparation exchange in the fixture.
- Main `control/server.go` still logs a raw error for "vector claim token not minted" (same class as item 8, outside this review).


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

## Re-verification of #1258 and #1199 on the production configuration (2026-10-12)

#1258 (session-write deadlock) and #1199 (nested child ids across Worker processes) were browser-proven on their own
stacks, which ran without Main Code owner recovery. That configuration hid Part J. Both journeys were re-run on
`elitea-code1084` with the production configuration (Worker `agent_node_recovery` and
`agent_model_checkpoint_recovery` true, Main owner recovery and code consumers on, Code sandbox with Python
preparation) and the final images built from `9bf82d60d` (this branch merged with main `719d12178`: Main
`sha256:811f516d53d9…`, Worker `sha256:4bfaea5b9720…`, Supervisor `sha256:efacee7226c2…`, Web `sha256:f18101e4ef63…`).
Fixture: pipeline 138 / agent 137 from the real-model dump (the #1258 and #1199 fixture).

| Journey | Chat | Executions (message → answer 1 → answer 2) | Result |
|---|---|---|---|
| J1, no restart | 934 | `feaf9024…`, `bac040bd…`, `48f3c39c…` | all SUCCEEDED, attempt 1; final OVERRIDE/DEFAULT sections |
| J2, Worker restart while card 1 pending + reload | 933 | `95069a39…`, `e7a0af51…`, `e42593fb…` (images 3c5c88096/e9cdbaa1f) | all SUCCEEDED, attempt 1 |
| J3, Worker restart while card 2 pending + reload (the #1258 incident step) | 935 | `bf156977…`, `25ccaf72…`, `58ba66f9…` | all SUCCEEDED, attempt 1; final sections |

After reload each chat shows one final answer with both sections and no card. Server side over the window from
15:28:37Z: 0 agentstate backends `idle in transaction`; Worker 0 `lock_not_available`, `storage_unavailable`,
`driven_task_failed` or panic lines, 0 ERROR lines, 0 `too_many_pings`/GOAWAY; Main 0 `current agent continuation
failed`.

## Deployed Kubernetes Code matrix on the merged branch (2026-10-09)

The branch head is `e7a291985`. These runs used a two-node kind cluster, `k8s1084`, reached at
`http://k8s1084.localhost:18400` through an in-cluster Traefik front door and a port-forward.

**Cluster.**
- kind v0.33.0; node image `kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5`;
  one control-plane node (also hosts Main, Supervisor, PostgreSQL, NATS, Web, front door) and one worker node
  (`elitea.ai/sandbox=true`; hosts the Worker and every Code Pod). Kubelet `podPidsLimit: 128` on both nodes.
- Calico v3.30.3 (`disableDefaultCNI`), so NetworkPolicy is enforced. cert-manager v1.19.2.
- NATS: `deploy/helm/nats` (scale-1 profile, secured, 2 Gi PVC) plus `deploy/helm/nats-bootstrap`.
- PostgreSQL `pgvector/pgvector:0.8.1-pg18-trixie` with TLS (the Supervisor connects with `verify-full`).
  The real-model product dump (`main-c0f2e5f9b`) was restored; shared migrations 158 and tenants 148 are already the
  branch level (the migration hook found nothing to apply). AgentState database at migration 13.
- Chart: `deploy/helm/elitea` with `values-standalone.yaml` plus the values listed below. Registry: a local
  `registry:2` container joined to the kind network; the Supervisor and the runners are referenced by digest.
- RuntimeClass `elitea-code` (handler `runc`); execution namespace `elitea-code-execution` (deny-all policy, quota 16
  Pods); preparation namespace `elitea-python-preparation` (DNS plus TCP 443 to six registry addresses).

**Images.** Main, Worker, Web and Supervisor were built from `e7a291985` (tag `code1084-e7a291985`) and pushed to the
registry. The runner sources are unchanged between `22f33b8f8` and `e7a291985` (`git diff --stat` on
`services/elitea-code-runner`: empty), so the existing `22f33b8f8` runner images were reused instead of rebuilt.

| Role | Registry manifest digest |
| --- | --- |
| Main | `sha256:4f9eade9ed20570f7f9660f65c21ee10d6cbe56c8c0c8844832f527085483799` |
| Worker | `sha256:4445580b01d696664ab91645c0260c08919b10983bd6e4ed327c07119b3fd940` |
| Web | `sha256:f1fabe1db2b539de8d6175a39e77c3297a315fe13a1c0ef5fe54b6628d71daf4` |
| Supervisor | `sha256:04992018ffca7e1dc6e8b57228ecb991f2a8a18bc670f99ece672cf66789974c` |
| Runner (Deno, Python/JavaScript/TypeScript) | `sha256:8434c49a99e04de1a45d00364ae9b0810c1d73f3afa920589dd3ffd5769e2105` |
| Runner (Rust) | `sha256:d8af139c5395c10a05552965646c39a5ecdf77f537874ed3f354bf32e9b0ce4f` |

The running Pods' `imageID` values equal the pushed digests for Main, Worker, Web and Supervisor. Main, Worker and Web
are selected by tag (the chart has one global tag); the Supervisor and the runners are selected by digest, as the
chart and the Pod policy require. Runner images are pre-pulled on the sandbox node by digest (`imagePullPolicy: Never`).

**Binary check against `main-c0f2e5f9b-verify`.** Worker contains `code_timing.rs` and `pipeline.code_cancelled`
(count 1; the verify image: 0). Main contains `resumeCurrentAgentStaticTools` (1; verify image: 0). Supervisor
contains `service_job_owners.rs` (4). #1160 is an ancestor of the head.

**Values.** `worker.runtime.agentNodeRecovery: true` (chart default `false`, set explicitly).
`main.runtime.codeOwnerRecovery.enabled: true`, `mainWorkloadIdentity: dns:elitea-code-owner-main`, supervisors
`dns:elitea-sandbox-deno` / `dns:elitea-sandbox-rust` at `https://elitea-sandbox-{deno,rust}:944{6,7}`;
`codePlatform` one broker policy; `codeDebugArtifacts` on. Four `sandboxRuntimes` entries (Python, JavaScript,
TypeScript on the Deno profile; Rust). Supervisor profiles `deno.json` and `rust.json`, `backend.kind: kubernetes`,
cluster `k8s1084`, 512 MiB, 1 CPU, 180 s. The Gateway, Scheduler, DeepWiki and Inventory are off: the fixtures are
Code-only. The Supervisor Deployment was patched after install with `nodeSelector tier=infra` (not in the chart).

**Fixtures.** Saved pipelines with one Code node that records its start time and sleeps 25 s: JavaScript, Python
(no packages), Rust. Each run is a normal conversation send through the API with a signed-in cookie.

**Invariants.** All are scoped to this cluster's own ledger rows. The Pod name is
`elitea-code-<runtime-job hash[:48]>` (`ledger.rs:46-66`).
- I1: one execution row, one committed settlement, at most one terminal replay event.
- I2: the stored start time precedes the fault (no re-run); one Pod UID per runtime job.
- I5: claim attempts contiguous, epochs increasing, the settlement bound to the last claim.
- I6: Pod physically deleted; no Pod left in either Code namespace; no ledger job left `reserved`/`dispatched`.
- I7: outbox drained; the NATS agent durable has pending 0 and ack_pending 0.

| Case | Language | Fault | Execution | Fault to settle | Settlement | Answer | Class | Invariants |
|---|---|---|---|---|---|---|---|---|
| POS-js | JavaScript | none | `429ed94ba4214f5bf59bef8b31dc8f8c` | - | SUCCEEDED, attempt 1 | started=`1791574045461` | - | all hold |
| POS-py | Python | none | `e4078f93fbbc6195239e817df6021f5f` | - | SUCCEEDED, attempt 1 | started=`1791574113054` | - | all hold |
| POS-rs | Rust | none | `53b0cee5ddf68b7e82ab5d28961c9ee5` | - | SUCCEEDED, attempt 1 | started=`1791574171901` | - | all hold |
| POS-rs-2 | Rust | none (second run) | `b2ac4a99e8a69e22c5249433b5d48ff5` | - | SUCCEEDED, attempt 1 | started=`1791574229235` | - | all hold |
| STOP-js | JavaScript | Stop at 8 s | `2403dcd1b3a4892a99e6444f4410e453` | 6 s | CANCELLED, attempt 1 | "Execution was cancelled." | - | all hold; Pod removed |
| STOP-py | Python | Stop at 8 s | `2711c1d9b50ee0863fa6d102a78820c4` | 6 s | CANCELLED, attempt 1 | "Execution was cancelled." | - | all hold; Pod removed |
| WFORCE-js | JavaScript | Worker Pod `--grace-period=0 --force` | `f27f19806d1c3df65bd9355378c7e7b9` | 60 s | SUCCEEDED, attempt 2 / epoch 2 | started=`1791574340026` | R | all hold; claim 1 LEASE_EXPIRED |
| WFORCE-py | Python | same | `024436c3d1841f0c674ab27bd72c4795` | 58 s | SUCCEEDED, attempt 2 / epoch 2 | started=`1791574490836` | R | all hold |
| WFORCE-rs | Rust | same (re-run, see below) | `28abf31eca156cec39838f169c4838d1` | 38 s | SUCCEEDED, attempt 2 / epoch 2 | started=`1791805654972` | R | all hold |
| WROLL-js | JavaScript | Worker `rollout restart` | `f5985835643f01ae832d0e2249ea075a` | 17 s | SUCCEEDED, attempt 1 | started=`1791574584931` | R (drain) | all hold |
| SDEL-js | JavaScript | Supervisor Pod force-delete | `d8af6ffd176360d788df87df1455cd89` | 51 s | SUCCEEDED, attempt 1 | started=`1791574649297` | R | all hold; gap 2.3 s |
| SROLL-js | JavaScript | Supervisor `rollout restart` (`Recreate`) | `2172971eadf8c148e4cf2b37e3c7573e` | 54 s | SUCCEEDED, attempt 1 | started=`1791574739034` | R | all hold; gap 1.9 s |
| MGRACE-js | JavaScript | Main Pod graceful delete | `2e77c3644b49d535cafb175bdcd72670` | 61 s | SUCCEEDED, attempt 2 / epoch 2 | started=`1791575840900` | R | all hold |
| MDEL-js | JavaScript | Main Pod force-delete | `eddcfc7f196ae73458d73b425526ee49` | 16 min 17 s | SUCCEEDED, attempt 2 / epoch 2, at 19:56:56 | original result | R, late | holds after settlement; see below |
| NODE-js | JavaScript | `docker kill` worker node, restart after 20 s | `0e33cba58631b61112c6b4fce086641c` | not settled; claims 1-3 | RUNNING / SUSPENDED; operator Stop then CANCELLED (attempt 4) | none | C | no re-run; sandbox job `failed`; node-recovery visit SUSPENDED |
| NODEDOWN-js | JavaScript | `docker kill` worker node, restart after 420 s | `17186c116343b9de04ec0f58506dfe39` | not settled; claims 1-3 | RUNNING / SUSPENDED; operator Stop then CANCELLED | none | C | no re-run; Worker replacement Pending until the node returned |

**Late recovery after a force-deleted Main (MDEL-js).** The Worker's control connection to the ClusterIP stayed in
conntrack toward the old Main Pod IP. Every control call timed out at the 5 s deadline, and the retried NATS delivery
(60 s) failed the same way until the kernel gave up on the connection, 16 min 17 s after the delete. The original
result was then recovered with claim 2 and no Code re-run. A graceful Main delete (MGRACE-js) recovered in 61 s. The
Worker source has no gRPC or TCP keepalive on its clients (`grep keep_alive` over `services/elitea-worker-rust/src`
finds none in the transports). This is a follow-up, not a fix made here.

**Node loss.** With the node killed and restarted after 20 s, or after 420 s, the Code Pod reached a kubelet
terminated state (exit 255, `Unknown`) only after the node returned. In the 420 s case the taint manager evicted the
Pods at about five minutes; the Pod stayed `Terminating` (receipt finalizer) until the kubelet reported the terminated
container, then the Supervisor removed it. The Worker then found the Code job `failed` and suspended the node for
operator reconcile (`agent_node_recovery_required`, `allowed_actions: [reconcile]`, `failure_class: invalid_result`,
`replay_safety: unknown_external_effect`). No Code ran again. Stop settled the execution as CANCELLED and drained the
durable. This is class C, not the documented F-after-`timeout+90` for G-SUP-02 (the node came back before that bound
mattered).

**User-code failure.** A Code node whose user code throws (typed receipt `failed`) was also suspended for operator
reconcile instead of settling `PIPELINE_CODE_FAILED` (`code_attempt_remote.rs:535-552` classifies it `InvalidResult`
with `UnknownExternalEffect`; `node_recovery_runtime.rs:1062-1072` publishes a terminal failure only for a Stop
decision other than operator approval). It was observed on Kubernetes only; the code path is not Kubernetes-specific
and was not compared on Docker.


### Python dependency preparation (added after the first run)

Preparation was enabled afterwards: a fourth supervisor profile `preparation.json` (port 9448, audience
`dns:elitea-sandbox-preparation`, the Deno runner image as preparer), content staging for the `deno` and `preparation`
clients (256 MiB tmpfs each), separate content-client certificates (`gen-sandbox-certs.sh --preparation`), a
`dependency_content` block in `deno.json`, a `preparation` block on the Worker's Python entry, the preparation
audience in Main's `sandboxAudiences` and `codeOwnerRecovery.supervisors`, and `sandboxKubernetes.preparation` with ten
registry addresses on TCP 443. Fixture: `python-prepared-sleep` from the crash suite (micropip pins `python-slugify`,
`text-unidecode`, `python-dateutil`, `six`, `pytz`, `idna`; a 25 s sleep; then a JavaScript sentinel node). Each run has
one preparation job and two execution jobs (Python, sentinel). The answer is the sentinel's
`CRASHRESULT {... "slug":"crash-recovery" ...} SENTINEL9999`, which proves the packages imported.

| Case | Fault | Execution | Fault to settle | Settlement | Class | Invariants |
|---|---|---|---|---|---|---|
| PREP-POS | none | `8c0b74f802e8e942988d33fd41660b00` | - | SUCCEEDED, attempt 1 | - | all hold; 1 resolver Pod, 3 Pods in total, each created once and deleted |
| PREP-WFORCE | Worker force-delete 1 s after the resolver Pod was bound | `1a1971c36d372387df4f701bed3af14c` | 96 s | SUCCEEDED, attempt 2 / epoch 2 | R | all hold; still 1 resolver Pod (no second download) |
| PREP-SDEL | Supervisor force-delete 1 s after the resolver Pod was bound | `3f303a19c0543f5cc5796617b4e55ccd` | 95 s | SUCCEEDED, attempt 1; sandbox epoch 12 | R | all hold; 1 resolver Pod |
| HYD-SDEL | Supervisor force-delete while the execution job was `reserved` (hydration) | `73ae9218ce74f8ae3da0c65b103a13c1` | 42 s | SUCCEEDED, attempt 1 | R | all hold |
| PREPX-WFORCE | Worker force-delete during the prepared Python execution | `d40b67424dbf4d82b8f3eaf3b4a9e2a3` | 59 s | SUCCEEDED, attempt 2 / epoch 2 | R | all hold |

Every row: one settlement, claims contiguous, outbox drained, NATS durable pending 0 and ack_pending 0, both Code
namespaces empty afterwards, one Pod UID per ledger job. 69 checks held across the four fault runs; none was violated.

A first PREP-POS (`5bde36dc3a96...`) ended `PIPELINE_CODE_FAILED` "Code dependency preparation failed" after about 2
minutes. Cause: `cdn.jsdelivr.net` resolves inside the cluster to Fastly `151.101.{1,65,129,193}.229`, not the
Cloudflare addresses seen from the host, so the egress policy blocked it. Those four addresses were added to
`resolverCidrs` and the case then passed. This is an operator-allowlist finding: the policy cannot name hostnames.

Extra manifest outside the chart: `elitea-main-content-from-supervisor` admits the Supervisor Pods to Main's content
listener (9445); the chart's Main policy admits only Worker Pods on the runtime listeners.

### Refusals

| Case | Execution | Result |
|---|---|---|
| language `go`, `ruby`, `bash` | `ae1ac6a2...`, `5f1f3f22d4c9...`, `c8875572d6f0...` | FAILED, `INVALID_INPUT` "The execution input is invalid.", one settlement, no ledger job, no Pod |
| unknown node field `timeout_seconds` | `010770aad269...` | same |
| unknown node field `memory_bytes` | `a4d7e42bd0b8...` | same |
| 2 MiB source | - | refused at save: HTTP 400, "exceeds the 512 KiB size limit" |
| comment-only source (REF-empty_source, `c70ef5da069b...`) | - | not a refusal: the Pod ran, the runner failed, the node suspended for operator reconcile (same as the user-code failure above); Stop settled CANCELLED |

### NetworkPolicy proof (`evidence/netpol.txt`)

Probe Pods used the pre-warmed runner image in the preparation namespace, the execution namespace, and `default`
(control, no policy).

| Probe | Preparation namespace | Execution namespace | Control |
|---|---|---|---|
| DNS `pypi.org` | resolves | fails | resolves |
| TCP `151.101.0.223:443` (listed) | connects (37 ms) | blocked | connects |
| same IP, port 80 | blocked | blocked | connects |
| `151.101.1.223:443` (not listed) | blocked | blocked | connects |
| `1.1.1.1:443` | blocked | blocked | connects |
| PostgreSQL / Main Service | blocked | blocked | PostgreSQL connects; Main blocked by its own policy |

### Runs invalidated and re-run

- The first `NODEDOWN-js` and `WFORCE-rs` (`run-f7.log`) spanned hours of host sleep (SEND 03:48, CHECKS 05:26,
  trigger MISSED) and are discarded. `WFORCE-rs` was re-run awake: it first stalled with the Pod in
  `ContainerCreating` because Calico's CNI credential on the worker node had gone stale (`error getting
  ClusterInformation: connection is unauthorized`), which is a cluster artefact of the sleep and not a product defect.
  Restarting `calico-node` fixed it; the clean re-run is the `WFORCE-rs` row (`28abf31e...`). `NODEDOWN-js` was
  re-run awake after that.
- A grep of Main, Worker and Supervisor logs for `PROTOCOL_VIOLATION`, `invalid node event output`,
  `invalid_durable_state`, `output_recovery_pending` and `control_rejected` found no match on this cluster. The
  old stuck `9279e24b...` execution was an artefact of the same stale-CNI condition and was settled CANCELLED by Stop.

### Limits and follow-ups

- Compiled Rust snapshots (the descriptor cache) were not enabled. They need the release manifest, native Cargo
  bundle and preparation profiles. Cache evidence here is limited to the pre-warmed immutable runner images
  (no image pull for any Pod). No same-source repeat run was made, so reuse of a compile result is not shown.
- Python preparation was run end to end (section above) but only with the six-package fixture and one resolver Pod at a
  time; a PyPI outage or a changed CDN address set was not tested.
- The node kill preserved the node's disk, so the Worker `emptyDir` spool survived `docker start`; it is lost only in
  the force-delete cases (new Pod).
- Main, Worker and Web are selected by tag plus verified `imageID`, not by digest in the manifest.
- Follow-ups: Worker gRPC keepalive (MDEL-js); user-code failure suspending for operator reconcile; the nats-bootstrap
  chart renders `deltas.maxBytes: 1073741824` as `1.073741824e+09` under Helm 4.1.0 (install needed `--set-string`).
- Helm follow-ups: (1) `deploy/helm/nats-bootstrap` renders `deltas.maxBytes: 1073741824` as `1.073741824e+09` under
  Helm 4.1.0, which `nats` rejects ("bytes must end in B, K, KB..."); the install used `--set-string`. (2) The
  Supervisor Deployment has no `nodeSelector`/affinity value; it was patched after install and `helm upgrade` reverts the
  patch. (3) The chart has no way to admit the Supervisor to Main's content listener (extra NetworkPolicy above).

### Fix images `9bf82d60d` (the #1084 branch merged with main `719d12178`)

Images from `9bf82d60d` replaced Main, Worker, Web and Supervisor on the same cluster, with the same values (tag
`code1084-9bf82d60d`, Supervisor by digest; the runners are unchanged). Main applied the new AgentState migration 0014
on start (agentstate now at 14). The Supervisor `nodeSelector` patch and the extra content NetworkPolicy were re-applied.

| Role | Local image | Registry manifest digest |
| --- | --- | --- |
| Main | `811f516d53d9` | `sha256:242b88f9a7aa4f64296fd8efa458c897aee4f4b21343319fa8b344143c23940d` |
| Worker | `4bfaea5b9720` | `sha256:1cd75013c17f836ddcb0543862506eec45c8ebeab6b434805e0cbe8847b3f9c7` |
| Web | `f18101e4ef63` | `sha256:49c4f5afb6fd62bbc80e8e14cc15ee574ad5adff5558dc59126bb62537cda136` |
| Supervisor | `efacee7226c2` | `sha256:e5d39a028b21f4bf2e50a6295a7ddba9515368d4ea0e894b4933af3a2a9384e5` |

Running Pods' `imageID` equals these digests. Marker check on the extracted binaries: Worker contains
`code_platform_effects` (1) and `release_undispatched` (2; the old Worker: 0); Main contains
`resource_project_id=$1::integer` (1; plus 2 spaced variants).

| Case | Fault | Execution | Fault to settle | Settlement | Result | Before (e7a291985) |
|---|---|---|---|---|---|---|
| MDEL-js | Main Pod force-delete | `aafa080717b152c0efa42feec4970bf7` | 66 s | SUCCEEDED, attempt 2 / epoch 2 | R, original result | 16 min 17 s |
| THROW-js (user code throws) | none | `1a9c3bf9d36a12734dc9b5b2cce30afb` | - | FAILED `PIPELINE_CODE_FAILED`, readable message | typed failure, no `agent_node_recovery_required` event | suspended for operator reconcile |
| THROW-py | none | `4a87c096e01a953fa9a6c781a17ac429` | - | FAILED `PIPELINE_CODE_FAILED` | same | not run |
| COMMENT-js (comment-only source) | none | `56d31f7832cf00af14e5041b4df44c3a` | - | FAILED `PIPELINE_CODE_FAILED` | same | suspended |
| NODE-js | worker node killed 20 s | `ddc2a1c5712e8304fd8b0e8fab201a73` | 65 s | FAILED `PIPELINE_CODE_FAILED`, attempt 2 / epoch 2 | typed failure | suspended (C) |
| IDLE-js | none, 120 s with no frames | `295c44bd7bc34042ff5274d1dcdb6480` | - | SUCCEEDED, attempt 1 | no GOAWAY / `too_many_pings` in Main, Worker or Supervisor logs | not run |
| POS-js, POS-py, POS-rs | none | `1b27bb2e…`, `609484d9…`, `12a07191…` | - | SUCCEEDED, attempt 1 | pass | pass |
| WFORCE-js | Worker force-delete | `7749ed259f3d86aed8ec5c1fbd146b6f` | 55 s | SUCCEEDED, attempt 2 | R | R |
| SDEL-js | Supervisor force-delete | `3cefeef25305cf8263e7e539a2181d2f` | 51 s | SUCCEEDED, attempt 1 | R | R |
| MGRACE-js | Main graceful delete | `bdf9029592189de44b5823ff621de8d1` | 61 s | SUCCEEDED, attempt 2 | R | R |
| PREP-POS | none | `9e8cf19dea6a020ce568217febdf6c0b` | - | SUCCEEDED, attempt 1 | pass | pass |
| PREP-WFORCE | Worker force-delete in preparation | `f134eb435c7a0e77ae8461f8f137027b` | 99 s | SUCCEEDED, attempt 2 | R | R |
| HYD-SDEL | Supervisor force-delete in hydration | `076fad3966e3bec08df326e9e29fb9a2` | 95 s | SUCCEEDED, attempt 1 | R | R |
| PREPX-WFORCE | Worker force-delete in prepared execution | `14b776d12b84ad77f80d2d93b31b0887` | 62 s | SUCCEEDED, attempt 2 | R | R |

All invariants held on every row (one settlement, one Pod UID per ledger job, Pods deleted, namespaces empty,
NATS drained, one resolver Pod in the prepared cases). The only reported check failure is my own harness expectation
for `POS-py`, `THROW-py` and the other Python runs: the harness expected zero preparation jobs, but with the
preparation profile deployed a Python node always has one (completed).

**Why NODE-js now fails typed.** After the node returned, the Code job's ledger phase was `failed`
(`sandbox.code_failed`, the kubelet's terminated container). The Worker raised `pipeline.code_failed` upstream and
settled the execution at claim 2; there was no node-recovery visit row (the Main platform-call journal held no
unresolved effect), so the "unresolved effect" exception did not apply. The Pod, the Worker and the platform-edge Pod
restarted once.

**MDEL-js.** The fault-to-settle time is dominated by the 60 s claim lease and NATS retry delay, not by a stale
connection: the Worker's retried claim succeeded on the first attempt after the lease expired. Before the keepalive
change the same fault took 16 min 17 s.

**Follow-up from the fix run (FU-214).** NODE-js now settles typed instead of suspending, but node loss is an
infrastructure loss, not a user-code failure: the Kubernetes backend reports the terminated container (exit 255,
reason `Unknown`) as phase `failed` (`sandbox.code_failed`), the same as a thrown error. For effect-free Code (no
network; no unresolved platform call in the Main journal) a lost runtime can be re-executed safely (class I). Distinguish
"runtime lost" (node loss, eviction, `Unknown`/OOM-kill by the platform) from "user code failed" in the supervisor
receipt, and retry the former within the deadline; keep the typed failure for the latter.

### Language matrix (9bf82d60d)

Same cluster and the `9bf82d60d` images. Every cell is a saved pipeline with one Code node that records its start time
and sleeps 25 s (THROW: the node throws). Faults are injected 8 s after the job is dispatched (Rust MDEL16: 16 s).
"Fault to settle" is the time from the fault to the committed settlement (baseline numbers for the lease and AckWait
tuning, FU-215). Class: R = resumed from the last durable point (the original result, no Code re-run), T = typed
failure. Invariants held in every row unless noted: one settlement, claims contiguous, outbox drained, one Pod UID per
ledger job, Pods deleted, namespaces empty, NATS durable drained, stored start time before the fault.

| Language | POS | Stop | Worker force-delete | Supervisor force-delete | Main force-delete | User code throws |
|---|---|---|---|---|---|---|
| Python | `ba2546b0…` SUCCEEDED, attempt 1 | `9a9dd329…` CANCELLED, 8.6 s | `b9b7ffbd…` R, attempt 2, 58.7 s | `e7446a1a…` R, attempt 1, 54.4 s | `87599333…` R, attempt 2, 70.3 s | `ffe29091…` T `PIPELINE_CODE_FAILED` |
| JavaScript | `ce120393…` SUCCEEDED, attempt 1 | `3d925192…` CANCELLED, 6.1 s | `b8766248…` R, attempt 2, 57.0 s | `1fe12964…` R, attempt 1, 51.8 s | `b0e2c561…` R, attempt 2, 66.2 s | `8db63646…` T `PIPELINE_CODE_FAILED` |
| TypeScript | `04f4c7f3…` SUCCEEDED, attempt 1 | `b0bc6119…` CANCELLED, 6.1 s | `91e02681…` R, attempt 2, 56.1 s | `c979aec6…` R, attempt 1, 51.9 s | `1306ec99…` R, attempt 2, 61.8 s | `91f5a957…` T `PIPELINE_CODE_FAILED` |
| Rust | `b589bf97…` SUCCEEDED, attempt 1 | `6e9eb37d…` CANCELLED, 6.1 s | `ce7443f7…` R, attempt 2, 60.0 s | `f35105c3…` R, attempt 2, 54.2 s; `25beed9c…` (16 s variant) R, attempt 2, 169.3 s | `b8af6f77…` R, attempt 2, 64.8 s; `351267dc…` (16 s variant) R, attempt 2, 72.3 s | `1006757a…` T `PIPELINE_CODE_FAILED` |

Notes on rows:
- Python has one preparation job per run (the preparation profile is deployed), so its rows show three ledger jobs
  rather than one; the harness check "execution jobs == 1" is relaxed for it.
- Rust SDEL/MDEL at 8 s: the fault landed during compilation, so the user code had not started yet and the stored start
  time is after the fault (flagged by the harness; the Pod was created once). The 16 s variants repeat both. MDEL16 holds
  every invariant. SDEL16 took 169 s because the Supervisor replacement was in place about 40 s before the Code started
  and the first claim's lease only expired at 16:56:39; its stored start time (16:54:32) is again after the fault
  (16:53:52), so the fault landed before the user code started. User Code ran once. No cell re-ran Code that had started.
- Stop takes about 6 s for the JavaScript, TypeScript and Rust fixtures and 8.6 s for Python.

**Packages per language** (same cluster; probes after the matrix)

| Language | Mechanism | Status on this deployment | Evidence |
|---|---|---|---|
| Python | micropip pins (PyPI through the preparation namespace) | supported; PREP-POS `9e8cf19dea6a020ce568217febdf6c0b`, PREP-WFORCE `f134eb435c7a0e77ae8461f8f137027b` (99 s), HYD-SDEL `076fad3966e3bec08df326e9e29fb9a2` (95 s), PREPX-WFORCE `14b776d12b84ad77f80d2d93b31b0887` (62 s) | section above |
| JavaScript | `npm:` import | not supported: no JavaScript preparation profile is deployed. `process.rs:170-176` requires a native platform and workspace for any non-Python preparation profile (certificates from `gen-sandbox-certs.sh --native`, a native bundle); `code_preparation.rs:548-554` then runs the node offline. The `npm:is-number@7.0.0` import failed in the Deno sandbox (`sandbox.code_failed`), execution `5d8228b089ff584ca5f54b652e4bcf50` | probe |
| TypeScript | `npm:` import | not supported, same reason; execution `0fe836590cb988cca96a5b963d254491` FAILED `PIPELINE_CODE_FAILED` | probe |
| Rust | `dependencies:` (Cargo) | not supported: Cargo acquisition needs a native Rust preparation profile (`code_preparation.rs:549,566`) that is not deployed; execution `6a263a785c40ef99939a457d1c8f77f7` FAILED `PIPELINE_CODE_FAILED` "Code dependency preparation failed" before any sandbox job | probe |

Enabling JavaScript, TypeScript or Rust package preparation needs the native profiles
(`elitea-sandbox-{javascript,typescript,rust}-preparation` leaves and content clients, a native platform value and
workspace size, and the resolver egress for npm and crates.io); that was not deployed here.

### Findings from the matrix run

- **Stale PostgreSQL sessions starve the database Pod.** A force-deleted client leaves its server backend idle until
  TCP keepalive notices (default about 2 h). After the first MDEL/WFORCE/SDEL rounds the Postgres Pod held about 119
  sessions; with the kubelet `podPidsLimit: 128` this lab applies to every Pod, `fork` failed ("could not fork new
  process"), the Supervisor replacement exited at start and the harness's psql sessions died. Everything from the second
  Python/Rust rounds onward in the first matrix pass was invalidated and re-run after restarting Postgres and setting
  `tcp_keepalives_idle=20`, `tcp_keepalives_interval=5`, `tcp_keepalives_count=3`. Follow-ups: keepalive on the server
  or the client DSN; the same limit would hit a product deployment with a pid-limited database Pod.
- **The Supervisor binary logs nothing on startup failure.** `sandbox_supervisor_exit` (`sandbox_main.rs:46-52`) is
  emitted under the binary's own target, which the log filter drops (`diagnostics.rs:42-46`, `OWNED_TARGETS` lists only
  the library crates), so the crash loop above showed an empty log and exit code 1.
- **Main exits when OIDC discovery fails at start.** After the last force-delete, Main crash-looped four times (about
  four minutes) with "OIDC discovery failed … connection refused" while the mock was reachable, until the Pod's network
  came up; a transient failure at start is fatal.
- **Rust Supervisor force-delete took 169 s** in one variant (above), against 51 to 54 s for the other languages.
- **One unexplained Python failure.** A no-fault Python run (`1487051ec5abd8ef61fa8632dc581fc3`, the first M-py-STOP,
  16:35 UTC) settled FAILED `AUTHORIZATION_FAILED` ("Execution authorization failed") 21 s after its claim; the Stop was
  never sent (its trigger was missed) and the Worker and Main logs of that period were lost to later restarts. It did
  not reproduce in 5 repeats (4 no-fault Python runs and the Stop re-run `9a9dd329…`). Treat it as an open observation.
