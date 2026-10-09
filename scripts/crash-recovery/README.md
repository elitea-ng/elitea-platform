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
| `invariants/{product,agentstate}/*.sql` | Invariant queries. Values are bound as psql variables, never interpolated. |
| `fixtures/` | Pipeline and agent fixtures, and `seed.py` (public API only) |
| `lib/` | Stack bring-up, faults, triggers, collectors, verdict engine, report, redaction |
| `tests/test_harness.py` | Self-tests: `python3 -m unittest discover -s scripts/crash-recovery/tests` |
| `../../deploy/docker-compose.crash-rehearsal.yml` | The compose crash overlay (WP-4) |
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
