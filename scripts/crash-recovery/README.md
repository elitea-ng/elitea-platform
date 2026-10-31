# Crash-recovery acceptance suite

This suite answers the customer question "what happens if Main, the Worker, the Supervisor or NATS goes down
during execution?" with repeatable scenarios and machine-checked invariants. Each scenario maps to one cell of
[`recovery-guarantees.md`](../../services/elitea-worker-rust/docs/recovery-guarantees.md).

Scope: tier T0 on Docker Compose. Crashpoints (WP-2/WP-3) wait for PR #1084, which rewrites their call sites. Until
then every trigger observes state (`obs:`), and windows shorter than a few seconds stay out of T0.
Kubernetes is tier T1+ and only prepared, in [`deploy/kind-crash/`](../../deploy/kind-crash/).

The harness is local and opt-in. CI never runs it. The design is
`handoffs/crash-recovery-suite/DESIGN.md`. The first run report is
[`crash-recovery-suite-20261009.md`](../../services/elitea-worker-rust/docs/source-mapping/crash-recovery-suite-20261009.md).

## Rules

- Python standard library only. The harness reads PostgreSQL as the `crash_reader` role, whose sessions are
  read-only by default. It never writes product or AgentState rows. Fixtures go through the public API.
- Use one dedicated compose project, `elitea-crash`, browsed at `http://crash.localhost:<port>`. Never use plain
  `localhost`, and never point the harness at another stack.
- The stack, its secrets and raw evidence live outside the repository: the stack directory
  (`CRASH_STACK_DIR`, mode 700) and the run directory (`CRASH_RUNS_DIR`). Evidence files hold ids, counts,
  digests and states only. `run` finishes with a redaction scan and fails if it finds a credential shape.
- Shared machine rules (delivery gate §3b) still apply:
  - at most 2 image builds;
  - more than 30 GiB free (preflight refuses below that);
  - compose and the kind cluster never run at the same time;
  - destructive scenarios run only on `elitea-crash`.
- Images come from merged `main`, which contains #1160. Record the image identities with `binary-check`.

## Layout

| Path | Content |
|---|---|
| `crashctl.py` | CLI: `init`, `material`, `up`, `seed`, `preflight`, `run`, `verify`, `report`, `binary-check`, `down` |
| `catalog/scenarios/*.json` | One scenario per file. The schema is DESIGN §3.2 in JSON, because the standard library has no YAML parser. |
| `catalog/tiers.json` | Tier membership (`T0`, `T0-base`, `T0-code`, `T0-model`) |
| `catalog/language-matrix.json` | The language matrix: fault templates x languages, expanded in `lib/catalog.py` into `LM-<FAULT>-<lang>` scenarios, plus the tiers `LM`, `LM-<lang>` and `LM-PREP` |
| `invariants/{product,agentstate}/*.sql` | Invariant queries. Values are bound as psql variables, never interpolated. |
| `fixtures/` | Pipeline and agent fixtures, and `seed.py` (public API only). `pipelines/code-lang.yaml` is the one template of the language matrix; `seed.py` `LANGUAGES` holds each language's node sources. |
| `lib/` | Stack bring-up, faults, triggers, collectors, verdict engine, report, redaction |
| `tests/test_harness.py` | Self-tests: `python3 -m unittest discover -s scripts/crash-recovery/tests` |
| `../../deploy/docker-compose.crash-rehearsal.yml` | The compose crash overlay (WP-4) |
| `../../deploy/docker-compose.crash-npm.yml` | Overlay of the opt-in `npm` stack feature (native Deno) |
| `../../deploy/runtime/worker-runtime.crash.json` | The Worker config with both recovery flags |
| `../../apps/elitea-web/playwright.crash.config.ts` | The opt-in browser check (I9), `e2e/crash/` |

## Bring-up

```bash
python3 scripts/crash-recovery/crashctl.py init        # writes stack.json; fill the image references
python3 scripts/crash-recovery/crashctl.py material    # certificates, Supervisor material, rendered Worker config
python3 scripts/crash-recovery/crashctl.py up          # restores the real-model dump, then standalone-stack.sh up
python3 scripts/crash-recovery/crashctl.py seed        # fixtures through the API (OIDC login, short-lived PAT)
```

What the overlay adds to the standard standalone, Rust-agent and sandbox overlays:

- **Networks.** NATS is only on `crash_bus`. The Supervisor's TLS names resolve only on `crash_sandbox`. Then
  `docker network disconnect` gives an exact partition: Worker from NATS, or Worker from Supervisor.
- **Durable state.**
  - The NATS store lives on a named volume.
  - The Worker spool volume takes its name from `ELITEA_CRASH_SPOOL_NAME`. `replace_worker` emulates a pod
    replacement with a new container on an empty spool (D2).
- **TLS and owner recovery.**
  - PostgreSQL TLS, for the Supervisor's verify-full ledger connection.
  - Original-Code owner recovery on Main. A Worker with `agent_node_recovery` refuses Code without it: the Code
    node fails with `authorization_denied` before any dispatch.
  - `crashctl material` issues the owner client certificate (`dns:elitea-main`) from the runtime CA. It places
    the certificate in the gitignored `deploy/certs/runtime/`.
- **Read-only role.** The `crash_reader` login role.
- **Restart and resources.**
  - `restart: "no"` on the fault targets, so the harness controls every restart.
  - Memory limits.
  - Services no scenario uses are disabled.
- **Worker diagnostics.** `ELITEA_RUST_FAILURE_DIAGNOSTICS=on`.

## Stack features

`stack.json` `features` switches opt-in overlays (all off by default; `crashctl material`, `up` and `seed` read them):

| Feature | Adds | Needed by |
|---|---|---|
| `preparation` | Python dependency preparation (`docker-compose.crash-preparation.yml`, the resolver network) | `BASE-python-prepared`, `CR-*-P09`, `LM-PREP-python` |
| `compiled` | Compiled Rust snapshots (`docker-compose.crash-compiled.yml`) | `BASE-rust-compiled`, `CR-MK-P11`, `LM-PREP-rust` |
| `npm` | Native Deno for JavaScript and TypeScript (`docker-compose.crash-npm.yml`); needs `preparation` | `LM-PREP-javascript`, `LM-PREP-typescript` and their Worker-kill rows |

How `npm` is configured (`crashctl material` writes it; `up` adds the overlay):
- **Worker config.** The `javascript` and `typescript` entries of `sandbox_runtimes` execute on a native listener
  (`dns:elitea-sandbox-deno-native`, port 9449) and each gets a native `preparation` object: its own listener
  (`dns:elitea-sandbox-<lang>-preparation`, ports 9450 and 9451), the existing Deno runner image digest, and
  `native_platform` {linux, `arm64` or `amd64`, gnu}. The architecture comes from
  `docker info --format {{.Architecture}}` (`aarch64` becomes `arm64`, `x86_64` becomes `amd64`). The Python entry keeps
  its preparation without `native_platform`. The Worker allows four runtimes, one per language, and this keeps four.
- **Supervisor.** The `deno` execution listener narrows to `python`. Three listeners are added, with material under
  `sandbox-npm/`: native execution (`purpose: execution`, languages `javascript` and `typescript`, `native_platform`,
  `native_workspace_bytes` 512 MiB and `memory_bytes` at least workspace + 128 MiB), and one preparation listener per
  language (`purpose: preparation`, one language, Docker backend, `preparation_network`, `dependency_content`,
  `native_platform`). Their certificates are the `--native` leaves of `deploy/scripts/gen-sandbox-certs.sh`
  (`elitea-sandbox-deno-native`, `elitea-sandbox-javascript-preparation`, `elitea-sandbox-typescript-preparation`).
  Main's sandbox audiences and its original-Code owner config gain the native audiences.
- **Network and egress.** Preparers join the existing `elitea-python-preparation-resolver` bridge, the one the Python
  preparer uses. Compose cannot restrict a bridge to hostnames: a preparer can reach `registry.npmjs.org` and `jsr.io`
  (and, since the bridge is shared, the Python registries) only because Deno's `--allow-net` names them. Execution
  stays offline. The harness checks that only this stack's preparers are attached to the bridge (`NET`).
- **Fixtures.** `seed` adds `code-javascript-npm-50` and `code-typescript-npm-50`, whose sleep node starts with
  `import slugify from "npm:slugify@1.6.6";`. npm has no dependencies field: the literal specifier is the declaration.
- **Status.** The generated configs are unit-tested only. Nothing has run against a live native Deno stack yet.

## Running

```bash
python3 scripts/crash-recovery/crashctl.py run T0-base            # control runs; they set the I2 job oracle
python3 scripts/crash-recovery/crashctl.py run T0-code T0-model   # each scenario's `repeat` (T0: 3)
python3 scripts/crash-recovery/crashctl.py run CR-WK-P10 --browser --node-path <node_modules>
python3 scripts/crash-recovery/crashctl.py report <run-id> [<run-id>...]
```

Each repeat runs these steps:
- preflight;
- admission through the API (or the browser);
- the trigger, polled every 250 ms and checked again just before the fault;
- the faults, each step recording the container id, PID and `StartedAt` before and after;
- a bounded wait for settlement;
- two collections 5 s apart;
- a 60 s cleanup grace, then the orphan check;
- the verdict.

The stack is restored after every repeat.

### Language matrix

`crashctl run LM` runs the same faults over every Code language (`python`, `javascript`, `typescript`, `rust`), without
a JSON file per row. `catalog/language-matrix.json` holds the fault templates and `lib/catalog.py` expands them:

| Template | Row id | Fault | Target |
|---|---|---|---|
| `BASE` | `LM-BASE-<lang>` | none; sleep fixture; sets the I2 oracle (run first) | R |
| `WK`, `SK`, `MK` | `LM-WK-<lang>` ... | kill -9 and start of Worker, Supervisor or Main at `code_runtime_running` (as `CR-WK-P10`, `CR-SK-P10`, `CR-MK-P10`) | R |
| `STOP` | `LM-STOP-<lang>` | the user's Stop while the Code runtime runs; no pod replacement | R, settles CANCELLED |
| `THROW` | `LM-THROW-<lang>` | none; the throw fixture | F, `PIPELINE_CODE_FAILED`, no reconcile card |

Package preparation rows, `LM-PREP*`:
- `LM-PREP-python` and `LM-PREP-WK-python` reference `BASE-python-prepared` and `CR-WK-P09`. `LM-PREP-rust` and
  `LM-PREP-MK-rust` reference `BASE-rust-compiled` and `CR-MK-P11`. A reference is an `alias`: the run, its evidence
  directory and its oracle belong to the referenced scenario, which runs once even when a tier also names it.
- `LM-PREP-javascript`, `LM-PREP-typescript` and `LM-PREP-WK-<lang>` are new: a baseline and a Worker kill during
  preparation (`preparation_resolving`) on the npm fixture. They need the stack feature `npm`.

Tiers: `LM` (all languages), `LM-<lang>` (for each language: `BASE`, the faults, then its package rows) and `LM-PREP`.
The fixtures `code-<lang>-sleep-50` and `code-<lang>-throw` are seeded for every language; `seed` adds the missing ones
to an existing `fixtures.json`. All of them come from `fixtures/pipelines/code-lang.yaml`. The P10 triggers use the 50 s
sleep. Rust gets a longer trigger timeout and budget, because the first (`BASE`) run compiles and later runs reuse the
Ready snapshot when the `compiled` feature is on.

`NOT-SUPPORTED`: a row the stack cannot run is recorded, not skipped. `run` writes `verdict.json` with verdict
`NOT-SUPPORTED` and a `reason`, logs `NOT-SUPPORTED: <reason>`, runs the row once, and touches nothing on the stack.
Two sources: a scenario's `requires` (`{"feature", "reason"}`; the npm rows say "JS/TS npm preparation needs the native
Deno profile (stack feature `npm`)", the Python and Rust package rows name `preparation` or `compiled`), and the
`unsupported` map of `language-matrix.json` (`{id: reason}`). `report` lists these rows with their reason and shows the
LM id of a referenced row with the scenario that proved it: `LM-PREP-python (proved by BASE-python-prepared)`.
A `NOT-SUPPORTED` repeat is never counted: it cannot become a pass, and it never moves a matrix cell.

`T0b-gap` holds scenarios that reproduce known gaps: `CR-WSP-P09-lease` and its Stop variant. Before the fix lands
they leave an unsettled execution, which blocks every later preflight. Run them alone and last. Then snapshot the
stuck state and reset:

```bash
python3 scripts/crash-recovery/crashctl.py run CR-WSP-P09-lease --run-id <run-id>
python3 scripts/crash-recovery/crashctl.py snapshot <run-dir>/<scenario>/r1/hang-snapshot   # both DB dumps, spool, NATS store
python3 scripts/crash-recovery/crashctl.py down --volumes && python3 scripts/crash-recovery/crashctl.py up
```

The snapshot is local only: mode 600 files in a mode 700 directory. NATS is paused while its store is copied.

To replay a stuck state on new images (for example, a fix's deployed re-proof), bring the stack fully down and
restore:

```bash
python3 scripts/crash-recovery/crashctl.py down --volumes
python3 scripts/crash-recovery/crashctl.py restore-snapshot <run-dir>/<scenario>/r1/hang-snapshot
```

`restore-snapshot` refuses when the kind guard trips, when `elitea-crash` still has containers or volumes, or when a
snapshot volume is not one of this project's. It unpacks the spool and the NATS store into newly created,
project-labelled volumes and restores both databases (owners kept, grants re-applied). It then starts the stack on
the images currently configured.

Then follow the restored execution:

```bash
python3 scripts/crash-recovery/crashctl.py observe <execution-id> --expect CANCELLED --out <run-dir>/restore-<name>
```

It waits for the settlement, allows 60 s for cleanup, and checks five things: one settlement, the expected
disposition, a drained agent stream, no reserved or dispatched sandbox job, and no running runtime of this execution.
It exits non-zero if any check fails.

## Invariants and verdicts

| Id | Checked from |
|---|---|
| I1 one execution, one settlement, one answer | `execution`, `admission`, `settlements`, `replay`, `chat_answers` |
| I2 no user Code re-run | runtime start count from `docker events`, the fixture's start-time sentinel, the job count against the control run |
| I3 effects | the mock-llm tool journal and the mock-mcp journal (WP-6) |
| I4 frozen identity | pre-fault snapshot against the final readback: execution, outbox, writer, sandbox jobs |
| I5 claims and fencing | `claims`, `output_inbox.stale_after_takeover`, `checkpoints` |
| I6 no orphan | sandbox rows after the grace period, plus `docker inspect` of every recorded runtime (404) |
| I7 NATS drained | `/jsz` on the monitor port: stream messages, pending and ack-pending, dead-letter delta |
| I8 typed failure | settlement code, the replay failure payload, the chat error flag. The support reference is the chat "Message ID" (`agent_execution_jobs.client_message_id`). |
| I9 browser | `apps/elitea-web/e2e/crash/recovery-one-answer.spec.ts` |
| I10 measurements | fault to first progress, fault to settled, takeover latency, model calls, redeliveries |

Verdicts:
- `PASS`: the observed class is at least the target and I4, I5 and I7 hold.
- `EXPECTED-GAP`: the observed class equals the documented class of a gap cell.
- `REGRESSION`: anything worse.
- `INCONCLUSIVE`: the trigger missed its window or preflight failed. It is never credited.
- `NOT-SUPPORTED`: the stack cannot run the row (a missing stack feature, or an `unsupported` entry). Recorded with its
  reason, never credited and never counted as a pass or a fail.

## Pinned facts (WP-0)

| Item | Pin |
|---|---|
| Agent success replay event | `execution.node_event` with JSON `type = full_message` (`repos/agent_execution_results.go`) |
| Failure and cancellation replay event | `execution.failed` with `{code, safe_message, retryable}` |
| Support reference | The response message UUID: `chat_message_group.uuid` = `agent_execution_jobs.client_message_id`, shown as "Message ID" by `FailureReference.tsx` |
| Chat transcript | `p_<projection_project_id>.chat_message_group` (`task_id` = execution id), `chat_message_items`, `chat_messages_text` |
| `sandbox_jobs` to `sandbox_dispatches` | `job_key = SHA256("elitea.sandbox.activation.v1\0" ‖ be64(len exec) ‖ exec ‖ be64(64) ‖ hex(activation))` within tenant and project (`sandbox/code_recovery.rs:343-352`). `sandbox.sql` reports `job_matched` per dispatch. |
| Preparer or runtime label | Docker carries no preparer role label. The preparation job has its own dispatch and job row. |
| Migration 0147 | Applied on main (`0147_original_code_intents`); `original_code_visits` exists |
