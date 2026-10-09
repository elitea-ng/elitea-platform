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
