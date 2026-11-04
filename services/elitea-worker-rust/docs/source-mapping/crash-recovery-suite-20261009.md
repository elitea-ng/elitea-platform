# Crash-recovery acceptance suite, tier T0 — 2026-10-09

This mapping records the first runs of the crash-recovery suite on merged `main`, plus the harness that produced them.
The question it answers: *"What happens if Main, the Worker, the sandbox Supervisor or NATS goes down during
execution?"* Each matrix cell in [recovery-guarantees.md](../recovery-guarantees.md) becomes a scripted scenario with
machine-checked invariants. Each scenario is repeated three times. The design is the local handoff
`handoffs/crash-recovery-suite/DESIGN.md`. The harness run book is
[`scripts/crash-recovery/README.md`](../../../../scripts/crash-recovery/README.md).

Scope:
- **T0:** the Code path (chats 851 and 855, automated), Main at P10, the Supervisor alone, Main+Worker+Supervisor
  together, and Worker and Main kills on the model path over NATS.
- **T0b** (added at the user's request): Python dependency preparation (chat 850), hydration, prepared execution, and
  compiled Rust publication (chat 852).
- **Not in T0:** crashpoints (WP-2/WP-3) wait for PR #1084, which rewrites their call sites. Every trigger here
  observes state. Kubernetes is tier T1+ and only prepared.

## 1. Customer answer, as measured

All results come from a Docker Compose stack on merged `main` (`c0f2e5f9b` images, `58abb650c` for the Supervisor and
runners), over the NATS transport, with both recovery flags on:

| Goes down during execution | Result | Evidence |
|---|---|---|
| Worker, container restart | **Resumes (R)**: Code result kept, model step resumed | CR-WK-P10, CR-WK-P02, CR-WK-P03 (3/3 each) |
| Worker, pod replacement with an empty spool | **Resumes (R)** | CR-WPK-P10 (3/3), CX-01 (3/3) |
| Main | **Resumes (R)** | CR-MK-P10, CR-MK-P03 (3/3 each) |
| Supervisor, process loss with a live runtime | **Resumes (R)**; the original job finishes once | CR-SK-P10 (3/3), CX-01 |
| NATS, with a claimed run or a queued command | **Resumes (R) / runs once (I)** | CR-NK-P10, CR-NK-P01q (3/3 each) |
| Main, Worker and Supervisor together | **Resumes (R)** | CX-03b (3/3) |
| Worker, with stock Helm flags (D1) | **Typed failure (F)**, generic text | CR-WK-P03-stock (1/1) |
| Worker, Supervisor or Main during dependency resolve, or a prepared job (T0b) | **Resumes (R)**, no re-download | CR-{W,S,M}K-P09, CR-{W,S,M}K-P10-prep (3/3 each) |
| Worker during hydration (T0b) | **Resumes (R)** | CR-WK-P09-hyd (3/3) |
| Main during hydration or compiled publication; leases lost during preparation (T0b) | **Hangs (L)**; Stop does not settle. Fix: #1084 | CR-MK-P09-hyd, CR-MK-P11, F1 (§9.3) |
| Supervisor during hydration (T0b) | R 2/3; once **failed and orphaned its runtime**. Fix: #1084 | CR-SK-P09-hyd (§9.3 F2) |

Across every R run:
- **No user Code ran again.** Each sandbox job had one runtime start, and the fixture's start time precedes the cut.
- **One execution, one settlement, one answer** after reload.
- **Fencing held.** Claim attempts are contiguous and epochs increase. No superseded writer was accepted.
- **NATS drained:** zero messages, pending and ack-pending.
- **No orphan runtime** after 60 s.

A recovery on the model path calls the model once more. This is the documented "recovery can bill once more".

T0b results and the four Code-durability findings are in §9.

## 2. What was built (work packages)

| WP | Content | Paths |
|---|---|---|
| WP-0 | Scenario catalog, invariant SQL, pinned facts (§4) | `scripts/crash-recovery/catalog/**`, `scripts/crash-recovery/invariants/**` |
| WP-1 | `crashctl.py` (standard library only), triggers, collectors, verdict engine, report, redaction, self-tests | `scripts/crash-recovery/crashctl.py`, `lib/**`, `tests/test_harness.py`, `README.md` |
| WP-4 | Compose crash overlay and crash Worker config, plus T0b add-ons | `deploy/docker-compose.crash-rehearsal.yml`, `deploy/docker-compose.crash-preparation.yml`, `deploy/docker-compose.crash-compiled.yml`, `deploy/runtime/worker-runtime.crash.json` |
| WP-6 | Fixtures, API seeding, mock journals and delays | `scripts/crash-recovery/fixtures/**`, `deploy/mock-llm/server.py` (+ `test_crash_knobs.py`), `deploy/mock-mcp/server.py` (+ `test_journal.py`) |
| WP-7 | Opt-in Playwright check (I9) | `apps/elitea-web/playwright.crash.config.ts`, `apps/elitea-web/e2e/crash/**`, `apps/elitea-web/knip.json` (entry) |
| WP-5 prep | kind profile, prepared only (T1+), and the same preparation and compiled profiles | `deploy/kind-crash/**` |

Deviations from DESIGN, each deliberate:
- **Scenario files are JSON.** The standard library has no YAML parser. The schema is §3.2.
- **kind replaces minikube.** This was the user's decision; the profile directory is `deploy/kind-crash/`.
- **The second Worker (`elitea-worker-b`) is not added.** No T0 scenario needs it. CX-05 and CX-09 (T1) do.
- **The Playwright spec runs on installed Chrome** (`CRASH_BROWSER_CHANNEL=chrome`). Playwright 1.62.1's pinned
  Chromium build is not cached here, and downloading it was not approved.

No product code changed. No migration was added. Nothing is wired into CI. `deploy/mock-llm/test_crash_knobs.py` runs in
the existing mock-LLM workflow, because that workflow discovers `test_*.py`.

## 3. Business behaviour taken, and what was not ported

The current platform has no crash-recovery contract to port. Its Python worker re-runs a turn from scratch after a
crash, and that behaviour was deliberately not ported. The suite holds the new platform to the delivery-gate classes
R/I/C/F/L. The harness follows two rules from the PR #1084 private controller:
- It never writes claims, checkpoints, journals or business results. It reads as a read-only role, and fixtures go
  through the public API.
- Each cut is checked again just before the fault ("repeated cut"). A miss is `INCONCLUSIVE` and is never credited.

## 4. Pinned facts (WP-0)

| Item | Pin |
|---|---|
| Terminal replay events | Success is `execution.node_event` with JSON `type = full_message` (`services/elitea-main/internal/infra/db/repos/agent_execution_results.go`). Failure and cancellation are `execution.failed` with `{code, safe_message, retryable}` (`configuration_validation_results.go:26`). No dedicated success event type exists. |
| Support reference | The response message UUID: `chat_message_group.uuid`, which equals `agent_execution_jobs.client_message_id`. `apps/elitea-web/src/features/chat-messages/ui/error-trace/FailureReference.tsx` shows it as "Message ID". No column is called "support reference". |
| Chat transcript | `p_<projection_project_id>.chat_message_group`, where `task_id` is the execution id, joined to `chat_message_items` and `chat_messages_text` (`services/elitea-main/internal/db/queries/agent_chat.sql`) |
| `sandbox_jobs` ↔ `sandbox_dispatches` | `job_key = SHA256("elitea.sandbox.activation.v1\0" ‖ be64(len exec) ‖ exec ‖ be64(64) ‖ hex(activation))` within tenant and project (`services/elitea-worker-rust/src/sandbox/code_recovery.rs:343-352`). Every dispatch of every run matched (`job_matched`). |
| Docker label `io.elitea.code.job` | Not the `job_key`. It is `SHA256("elitea.sandbox.runtime-job.v1\0" ‖ be64(len tenant) ‖ tenant ‖ be32(project) ‖ job_key)` (`src/sandbox/ledger.rs:46-66`). It was verified in SQL against a recorded label. |
| Preparer label | There is none. Preparation is a separate dispatch and job with audience `dns:elitea-sandbox-preparation`. |
| Migration 0147 | Applied on `main` (`0147_original_code_intents`), with `original_code_visits` present. The shared head is 158 and the tenant head is 148. |

## 5. Stack, images and fixtures

- **Compose project:** `elitea-crash` on the shared Docker VM, browsed at `http://crash.localhost:18140`. It is based on
  the real-model runbook: the dump `product-real-models-main-c0f2e5f9b`, the same `SECRETS_MASTER_KEY`, and the OIDC
  subject `admin@centry.user` (project 2).
- **Overlays:** standalone-full, standalone-rust-agent, sandbox, crash-rehearsal. T0b adds sandbox-preparation,
  crash-preparation and crash-compiled. Other stacks were not touched. The crash overlay has three properties:
  - It splits the networks: NATS sits on `crash_bus` only, and the Supervisor's names resolve on `crash_sandbox` only.
  - It keeps the NATS store on a volume.
  - It names the spool volume per Worker generation. Five pod replacements used `elitea-crash-worker-spool-<n>`, and
    the old spools were kept.
- **Main owner recovery.** The overlay enables original-Code owner recovery on Main. Without it, a Worker with
  `agent_node_recovery` refuses every Code node with `authorization_denied` (finding 1).
- **Images, identities by `docker image inspect` (local IDs):**
  - Worker `ghcr.io/elitea-ng/elitea-worker-rust:main-c0f2e5f9b-verify` `sha256:500d9904cc77`
  - Main `elitea-main:main-c0f2e5f9b-verify` `sha256:3a2ac118e53b`
  - Web `sha256:3f30846074d8`
  - Gateway `sha256:cd261a0c5453`
  - Supervisor `elitea-sandbox-supervisor:crash-main-58abb650c` `sha256:422a002fe878`. It was built for this run from a
    clean detached checkout of `58abb650c`.
  - Code runners, also built from `58abb650c`: Deno `sha256:76813e683650`, Rust `sha256:9b246ba6b5ed`
  - Mocks (WP-6, this branch): LLM `sha256:9fa7a90a9777`, MCP `sha256:4cbafab236a2`
- **§3b binary check** (`crashctl binary-check`: `docker create` + `docker cp`, marker count):
  - The #1180 refusal text "This pipeline uses a node type that is not available on this deployment" occurs once in
    the c0f2e5f9b Worker (`sha256 8fc66e61e8c1…`) and in Main (`14ba238e90aa…`). It is absent from the 1ab920dde Worker
    (negative control).
  - The Supervisor binary (`1be0181c7c6d…`) carries the job-key domain `elitea.sandbox.activation.v1`.
  - All images contain #1160, because `524165ed09` is an ancestor of `c0f2e5f9b`.
- **Fixtures** were created through the public API only:
  - OIDC login, then a short-lived PAT that `seed` revokes at the end;
  - `applications` POST, `conversations` and `participants`;
  - the mock model credential through `deploy/scripts/seed-llm-api.py`, under its own titles.

  The pipelines are: `crash-code-nonce-sleep-50` (app 166, version 192), `-150` (167/193), `crash-model-slow`
  (168/194) and `crash-python-prepared-sleep-50` (172/198). `rust-compiled` is created fresh per run: an identical
  source would reuse a Ready snapshot.
- **Resets during T0b (2026-10-12).** `elitea-crash` was recreated from the same dump three times (`down --volumes`,
  `up`, `seed --all`), each time after a stuck run had been snapshotted (§9). It was brought down after the last one.
  The fixtures were re-seeded through the API, so T0b pipeline ids differ between resets. The image IDs were unchanged. One orphaned Code runtime of this
  stack (`30f187e676d9`, F2) was removed by container id after its snapshot. Nothing else on the shared daemon was
  touched.
- **Database writes** were limited to the `crash_reader` role (`CREATE ROLE`, grants). That role is the collector's
  read-only login. No product or AgentState row was written by the harness.
- **Inherited rows.** The restored dump holds 25 non-terminal executions from August with expired deadlines. 23 of
  them have authority granted, which is the G-NATS-01/G-MAIN-02 "hang" shape in real data. Preflight counts them
  separately and never touches them.

## 6. Results, tier T0 (run `t0-20261009`)

Every scenario uses the compose crash stack with both recovery flags on, unless stated otherwise.
- "t→settled" is the median time from the fault to the committed settlement.
- "Takeover" is the median from the fault to the replacement claim.

| Scenario | Cell | Fault and cut | Repeats | Observed | Verdict | t→settled | Takeover |
|---|---|---|---|---|---|---|---|
| CR-WK-P10 | Worker (container) × P10 | SIGKILL during a 50 s Code job, then restart | 3/3 | R | PASS | 60.3 s | 58.5 s |
| CR-WPK-P10 | Worker (pod) × P10 | SIGKILL, then a new container on an empty spool | 3/3 | R | PASS | 62.0 s | 58.2 s |
| CX-01 | Worker (pod) then Supervisor × P10 (chat 851) | Worker SIGKILL plus replacement; wait for claim 2; Supervisor SIGKILL | 3/3 | R | PASS | 148.2 s | 58.1 s |
| CR-MK-P10 | Main × P10 | Main SIGKILL during Code, then restart | 3/3 | R | PASS | 48.2 s | none |
| CR-SK-P10 | Supervisor × P10 | Supervisor SIGKILL with the runtime alive | 3/3 | R | PASS | 58.3 s | none |
| CX-03b | Main + Worker + Supervisor × P10 | All three SIGKILLed within one second | 3/3 | R | PASS | 150.7 s | 59.5 s |
| CR-NK-P10 | NATS × P10 (chat 855) | Broker SIGKILL during Code, store kept | 3/3 | R | PASS | 47.8 s | none |
| CR-NK-P01q | NATS × P01, queued (chat 856) | Worker stopped; command queued; broker SIGKILL; restart broker, then Worker | 3/3 | R (target I) | PASS | 61.2 s | none |
| CR-WK-P02 | Worker (container) × P02 | SIGKILL 2.4 s into the held model request (no token yet) | 3/3 | R | PASS | 140.1 s | 58.4 s |
| CR-WK-P03 | Worker (container) × P03 | SIGKILL after ≥10 streamed events | 3/3 | R | PASS | 138.5 s | 56.7 s |
| CR-MK-P03 | Main × P03 | Main SIGKILL after ≥10 streamed events | 3/3 | R | PASS | 142.2 s | 60.4 s |
| CR-WK-P03-stock | D1, stock flags | As CR-WK-P03, with both recovery flags off | 1/1 | F (target F) | PASS | 55.5 s | 55.5 s |

Control runs, without a fault: BASE-code-50 (settled in 51.7 s), BASE-code-150 (151.5 s), BASE-model-slow (81.2 s).
They set the I2 job oracle (2 jobs per Code run).

Execution ids (r1, r2, r3):
- **CR-WK-P10:** `09462500f0839ba938b974b0ae6d0df8`, `acadfdcf741c2a578b70000fc986ff28`, `cb102b9bfcd6fee9e3f298e01a27e991`
- **CR-WPK-P10:** `e7bcf55f2caab5b6439c945b4092befa`, `ccbd681ccf655d7899d30c0637cc1077`, `2c27bd768ecdce36616bf91dadd2d1a2`
- **CX-01:** `4edc041f320c9f93951699bd379d760f`, `b8806a7e8524e55ab068b69077902c00`, `7a87646accb6d95ad26b5b63dde85f44`
- **CR-MK-P10:** `ec029837f0c91c413af2145722bb7304`, `e099e298d0d33906eb0e70f563f0252d`, `9c4ee273c8ba859aac973bdc9828ff82`
- **CR-SK-P10:** `9b52f37c41d5e0057e6ee72c92695664`, `ebf82d9b5b14f945c164c8a7d5be724c`, `45b7d7c5e03fb560a2c070176d758e68`
- **CX-03b:** `fa2fded15df5a656b9e1ad786195b33f`, `22faed248c6d1173b96f27cea6fa1213`, `b5c72ab0fba32d8cc10e9d3a56595dfd`
- **CR-NK-P10:** `a501990a8a052de9a8115b4d4f036902`, `41e5cf4244f5f9fa7954e3ffda1c9028`, `ff74537e877f9169a0563125a8afe7ea`
- **CR-NK-P01q:** `681ff811e4756b4a963582960bc43248`, `868408597fbeeab0300b198a39911e38`, `804435bd029f13e8c483b170901cbff2`
- **CR-WK-P02:** `3541d8ee85c41b3b7813858611c6b35f`, `3bde7f1f02ce4018f4bb5fe6d129f4ac`, `0e4d022bc9c95d20625802f379747250`
- **CR-WK-P03:** `dd071bf6fbc2a04a229970824b7aac89`, `b2e1a6af1ef26026986f92896ad82e01`, `da6f3180a43f07cc7bc3063fe2858450`
- **CR-MK-P03:** `1bc573686f9c2ed4c8260c67a95bcf03`, `e40f62fbe35364d5001d92274954c42d`, `d865336eba9f00e1d40ea53c9996d3a8`
- **CR-WK-P03-stock:** `e740befa3203aea09721976a9278e196`

Representative facts, read from the evidence:
- **CX-01, chat 851 automated with a positive tail:**
  - The original 150 s job ran from 18:30:54 to 18:33:25 across a Worker pod replacement (spool `-4`) and a later
    Supervisor SIGKILL.
  - The sandbox lease epoch went 1 → 3, with one runtime start.
  - Main took over with claim 2, epoch 2, recovery mode `AGENT_MODEL_CHECKPOINT`. NATS showed one redelivery.
  - The chat showed one answer, live and after reload.
- **Supervisor kill alone:** the original job's sandbox epoch went 1 → 2 with no Main takeover. This is the deployed
  form of the Supervisor × P10 component proof.
- **Main kill during Code:** the Worker kept claim 1. The Main PID changed (e.g. 66790 → 21513), and the result was
  projected once.
- **NATS kill during Code:** claim 1 was kept and the Code process never noticed. The queued variant held one stream
  message with zero ack-pending across the broker SIGKILL, and executed once.
- **Model path:**
  - Pre-first-token and mid-stream Worker kills, and mid-stream Main kills, resume with claim 2 in
    `AGENT_MODEL_CHECKPOINT` mode and exactly one extra model call.
  - The recovered projector reset the partial text. The final answer holds `slow-001` and `MOCKSTREAMEND` exactly once
    each, live and after reload.
- **D1 stock side:**
  - The replacement claim has recovery mode `NONE`. The run settles `FAILED`, with replay code `INTERNAL` and the
    generic "The runtime operation failed.".
  - The settlement row's `error_code` is NULL.
  - The support reference (Message ID) is present.
  - This is typed but generic: the G-ADV-01 shape (finding 3).

Run notes:
- **First stock attempt invalid.** Its `stack_setup` was not yet implemented, so the Worker still ran with recovery and
  the turn succeeded. The harness flagged it against the F target. It is kept as `invalid-attempt-setup-not-applied` and
  not credited.
- **Disk guard.** 13 repeats (CR-NK-P01q and the model path) first stopped at the preflight guard: free disk fell from
  89 to 29.4 GiB because of activity outside this session. With the user's approval they were re-run at a 20 GiB floor
  (`CRASH_MIN_FREE_GIB`, scenario runs only, no build or new stack). All 13 passed.
- **No other misses.** No trigger missed its window and nothing was `INCONCLUSIVE` in T0.

## 7. Recovery-guarantee rows (delivery gate §2)

Classes are as measured on Docker Compose with NATS and both recovery flags on. Kubernetes keeps its current classes
until the K tier runs.

| Component | Phase | Class | Status | Enforcing code | Proof |
|---|---|---|---|---|---|
| Worker (container) | P10 | R | P | Stable activation id `src/agents/graph/code_runtime.rs:393-405`; replacement claim `services/elitea-main/internal/infra/db/repos/claims.go:461-476` | CR-WK-P10 ×3 |
| Worker (pod, empty spool) | P10 | R | P | Code node recovery does not read the old spool; original-visit intents (migration 0147) | CR-WPK-P10 ×3, CX-01 ×3 |
| Main | P10 | R | P | Worker keeps claim and lease; output re-delivered after Main returns | CR-MK-P10 ×3 |
| Supervisor (process loss) | P10 | R | P (deployed) | Dispatch intent before signal, lease reclaim epoch +1: `src/sandbox/ledger.rs:397-420,478,501` | CR-SK-P10 ×3, CX-01 ×3 |
| NATS | P10 | R | P | A claimed run does not depend on NATS; `+WPI` resumes | CR-NK-P10 ×3 |
| NATS | P01 (queued, broker restart) | I | P | WorkQueue stream on durable storage; one delivery | CR-NK-P01q ×3 |
| Worker (container) | P02 | R | P | `ModelPending` marker before dispatch: `src/agents/model_checkpoint.rs:23-29,517` | CR-WK-P02 ×3 |
| Worker (container) | P03 | R | P | Non-continuing `agent_start` reset on recovery | CR-WK-P03 ×3 |
| Main | P03 | R | P | Next claim gets `AGENT_MODEL_CHECKPOINT`: `claims.go:461-476` | CR-MK-P03 ×3 |
| Worker, stock Helm flags (D1) | P03 | F | P | Recovery refused without the flag: `claims.go:463-476` | CR-WK-P03-stock ×1 (generic text, finding 3) |
| Main + Worker + Supervisor | P10 | R | P | As above, combined | CX-03b ×3 |

T0b rows are in §9.

## 8. Performance, Durability, Resilience, Security

### Performance (I10, measured)

| Measurement | Value | Reason |
|---|---|---|
| Takeover after a Worker loss | 56.7–60.4 s (median 58.3 s, n=24) | AckWait 60 s. The lease TTL is 30 s, but redelivery waits for AckWait. |
| Time to settle, Main/NATS/Supervisor faults | 47.8–58.3 s | No takeover. The time is the remaining job time plus delivery. |
| Extra model calls after a model-path recovery | exactly 1 | The resumed model step calls the provider again. |
| NATS redeliveries per takeover | 1 | |
| Harness cost per repeat | 1 SQL round trip per poll (250 ms, 100 ms for short windows); 2 full collections; 1 `/jsz` read per 2 s while open | |

There is no performance budget for recovery latency yet. The roughly 58 s is dominated by the AckWait default
(`deploy/helm/nats-bootstrap/files/bootstrap.sh`, consumer `--ack-wait`). A faster takeover is a product decision. It
is listed as a follow-up and was not changed here.

### Durability

These invariants are enforced by SQL (`scripts/crash-recovery/invariants/**`) and the verdict engine
(`lib/verdict.py`). They were proven above and by `tests/test_harness.py`.

| Invariant | What it checks |
|---|---|
| I1 | One execution and one admission per client question, one committed settlement, one terminal projection, one chat answer group with no streaming left |
| I2 | One runtime start per job label of this stack; the job count equals the control run; the result's start time precedes the cut |
| I4 | The frozen identity (execution, outbox digests, writer definition, sandbox request digests) is byte-equal before and after |
| I5 | Contiguous attempts, strictly increasing epochs, the exact takeover count, the settlement bound to the last claim, zero outputs/checkpoints/session events from a superseded claim after the takeover |
| I6 | No live sandbox row and no remaining runtime after 60 s (`docker inspect` of every recorded runtime id, plus this stack's labels) |
| I7 | Stream messages, pending and ack-pending at zero; dead-letter delta at zero; outbox closed |

### Resilience

- Every wait is bounded: trigger timeouts, `budget_s` per scenario, a 180 s readiness wait after each restart, and the
  cleanup grace.
- The repeated cut and `INCONCLUSIVE` replace the manual "missed cut" outcome.
- Preflight refuses on:
  - a non-terminal execution admitted since the baseline;
  - live sandbox rows;
  - a non-empty stream;
  - a stopped target;
  - low disk.

### Security

| `rules/security.md` category | Applies | How it was checked |
|---|---|---|
| Identity and trust boundaries | Yes (harness auth) | The OIDC login is the mock's browser flow. The PAT is minted through `/api/v2/auth/token/` and revoked after seeding. No identity header is forged. |
| Object-level authorization | No product change | The harness uses the admin subject's own project. No cross-project call. |
| Input and amplification | Yes (harness parsing) | Response bodies are capped at 8 MiB. Output is bounded (`compiled_manifest.py` refuses over 64 MiB, more than 16384 entries, or version output over 8 KiB). |
| Injection | Yes | SQL binds through psql variables (`-v name=value`, `:'name'`), never interpolated. No shell is built from strings: every command is an argv list. |
| Egress and SSRF | Yes (T0b) | The resolver bridge for preparers cannot be restricted to hostnames on Compose (§9). Only Deno's `--allow-net` limits destinations. The harness checks that the bridge has no foreign members (`NET`). |
| Secrets | Yes | Material lives in a mode-700 directory outside the repository. Evidence holds ids, counts, digests and states only. The `run` redaction scan (PEM, JWT, `mock-key-`, cookies, bearer) found 0 files. The diff secret scan found only the deliberately fake PEM/JWT test strings in `tests/test_harness.py`. |
| Supply chain | Yes | No new dependency (standard library and existing images). Images come from merged `main`. The mock images are rebuilt only from this branch's mock sources. |
| Shared daemon | Yes | Container checks match only this stack's `runtime_label`s and runtime ids. Nothing is swept or removed by label or name (DESIGN §2). |

`security-review` was run on the branch (§11).

## 9. Results, tier T0b (run `t0b-20261009`)

T0b covers Python dependency preparation (chat 850), hydration, prepared execution, and compiled Rust publication
(chat 852). It ran on the same images as T0. The preparation profile uses the existing Deno runner image
`sha256:76813e683650` as its `image_digest` with `purpose: preparation`, so no image was built. The compiled-release
material is the hand-measured manifest (§5).

The run took place on 2026-10-09 and 2026-10-12. Two hangs and one orphan (below) blocked the stack. After each one I
snapshotted the stuck state and recreated `elitea-crash` from the dump with `crashctl down --volumes`, `up` and
`seed --all`; the fixtures were recreated through the public API. No SQL was written. The same image IDs were
re-checked against the §3b binary check before every re-run. Repeats that were blocked by a stuck predecessor are
INCONCLUSIVE and are never credited.

### 9.1 Verdicts

| Scenario | Cut | Verdict | Repeats | Observed | t→settled median (s) | Executions |
|---|---|---|---|---|---|---|
| BASE-python-prepared | no fault | PASS | 1/1 | R | 63.0 | `149b051717249dd4a6f68f4c01b13dc2` (preparation about 8 s, hydration about 1 s) |
| BASE-rust-compiled | no fault | PASS | 1/1 | R | 66.7 | `9ce3a642d78e408af5054847e527a5f1` (compile about 64 s, `publishing` then Ready) |
| CR-WK-P09 | Worker kill9, preparer resolving | PASS | 3/3 | R | 113.7 | `47327213…`, `2ecb9521…`, `510c8cc2…` |
| CR-SK-P09 | Supervisor kill9, preparer resolving | PASS | 3/3 | R | 114.7 | `49688227…`, `039b7b4f…`, `5e3ac0e9…` |
| CR-MK-P09 | Main kill9, preparer resolving | PASS | 3/3 | R | 60.7 | `3ea14829…`, `934df5fc…`, `6702a063…` |
| CR-WK-P09-hyd | Worker kill9, hydration | PASS | 3/3 | R | 115.2 | `fe75b8ca…`, `d56aa032…`, `547e9862…` |
| CR-SK-P09-hyd | Supervisor kill9, hydration | **REGRESSION** | 3/3 | R, **L**, R | 115.9 | `b0aba844…`, **`4e160a9d8aeca4f419e2517aff77fa3e`** (§9.3 F2), `79c31341…` |
| CR-MK-P09-hyd | Main kill9, hydration | **REGRESSION** | 1/3 | **L** | — | **`0b19b052f3345b52d60c61116ead7b47`** (§9.3 F3); r2 and r3 inconclusive |
| CR-WK-P10-prep | Worker kill9, prepared job executing | PASS | 3/3 | R | 60.3 | `169f9d3a…`, `a85fd91d…`, `6f417c32…` |
| CR-SK-P10-prep | Supervisor kill9, prepared job executing | PASS | 3/3 | R | 60.7 | `1793f3dd…`, `990ff5ab…`, `ed206153…` |
| CR-MK-P10-prep | Main kill9, prepared job executing | PASS | 3/3 | R | 72.0 | `3343ec3e…`, `2959d386…`, `203ad769…` |
| CR-WPK-P09 | user Stop, then Worker kill9 and pod replacement during preparation (chat 850) | PASS | 3/3 | R (CANCELLED once) | 59.4 | `723425d5…`, `0c7e9c3f…`, `99ac65e3…` |
| CR-MK-P11 | Main kill9 after compile, snapshot publishing (chat 852) | **REGRESSION** | 1/3 | **L** | — | **`3fc85dada0e76311b12bfffc043f5b45`** (§9.3 F4); r2 and r3 inconclusive |
| CR-WSP-P09-lease | Worker + Supervisor frozen past the leases, preparer resolving | pending | 0/1 | — | — | Deterministic stand-in for F1. Not run yet: the disk floor stopped the session (§9.5). |

What held in every R run:
- **One preparation per job (I2P).** One preparation dispatch row and one preparer start, and no preparer start
  after the bundle was committed. A recovery after commit did not download again.
- **One settlement and one answer (I1).**
- **Fencing (I5) and drained NATS (I7).**
- **No foreign member on the resolver bridge (NET).**

CR-WPK-P09 settled `CANCELLED` exactly once on every repeat, from the takeover claim. Its preparation job ended
`cancelled`. The Stop behaviour of chat 850 therefore holds on `main` for this cut.

### 9.2 Observations

- **Hydration bumps the execution job's lease epoch once per file.** Epochs 4, 8 and 11 were observed. A takeover
  during hydration resumes at the next index. This is expected behaviour (`code_preparation.rs:306-358`, fresh
  authority per index), but epoch values are not a takeover count for Code jobs.
- **The resolver network cannot be restricted to registry hostnames on Compose.**
  - `elitea-python-preparation-resolver` is a plain, non-internal bridge that the harness creates and labels
    `io.elitea.crash.stack=elitea-crash`.
  - Compose and Docker have no per-hostname egress filter. The only destination limit is the preparer's own Deno
    `--allow-net` list (pypi.org, files.pythonhosted.org, cdn.jsdelivr.net, registry.npmjs.org).
  - The harness proves membership instead: only this stack's preparers ever joined the bridge (`NET` held in every
    run).
  - The Kubernetes profile (`deploy/kind-crash/manifests/`) carries the equivalent NetworkPolicy. It needs Calico,
    which has not been pulled (it needs approval).

### 9.3 Findings (all in the scope of #1084, which owns Code durability; this branch changes no product code)

**F1. A preparation failure seen on a recovery visit never settles (class L).** Execution
`097ca8e5a5e41a99623f98fd546bacad` (2026-10-09), #1084 parts 1–4.

- **Trigger.** The host slept for about 16 minutes while the preparer was resolving. No fault had been applied yet,
  and that attempt was not credited. The Worker's claim lease failed (`claim_lease_control_failure`, kept without an
  ack). On the next delivery the Supervisor expired the preparation job with `sandbox.preparation_failed`
  (`src/sandbox/docker_preparation.rs:430`).
- **Why it never settles:**
  1. Preparation errors are `NodeFailureClass::Unknown` (`src/agents/graph/code_attempt_remote.rs:140-147`). On a
     recovery visit `recovering_started` makes them `UnknownExternalEffect` (`code_attempt_remote.rs:606-620`), which
     always decides `Reconcile` (`libs/rust/agent-runtime/src/graph/node_recovery.rs:335-343`). The typed-failure
     branch (`src/agents/graph/node_recovery_runtime.rs:1045-1052`) is skipped, and the node emits
     `agent_node_recovery_required` (`src/agents/events.rs:1531-1571`).
  2. The Worker's receipt check passes `failure_class: unknown` (`src/agents/graph/node_recovery_receipt.rs:68-75,95-99`).
     Main's contract refuses it (`services/elitea-main/internal/domain/noderecovery/contract.go:94-100`) as
     `PROTOCOL_VIOLATION` ("invalid node event output").
  3. A non-retryable output rejection is not a terminal (`src/execution/native_agent_lifecycle.rs:874-879`; only
     cancellation or deadline winners are, `src/transport/output_grpc.rs:1072-1082`). The rejected frame stays in the
     spool, and every redelivery replays it (`src/execution/output_delivery.rs:2598-2652`), which yields
     `agent_output.invalid_durable_state`.
  4. Delivery failures only Nak (`src/execution/agent_delivery_processor.rs:238-253`,
     `src/execution/command_delivery.rs:820-821`). The consumer has `max_deliver=-1`
     (`deploy/helm/nats-bootstrap/files/bootstrap.sh:441-464`). Result: more than 430 deliveries and 14 claims over
     2.6 days.
- **Stop does not end it.** The public Stop returned 204 and set `desired_state = CANCELLED`. Eight minutes later the
  run was still `RUNNING`, and deliveries reported `agent_delivery.output_recovery_pending`.
- **Repro.** `CR-WSP-P09-lease` (tier `T0b-gap`) freezes the Worker and the Supervisor with `docker pause` for 100 s
  during resolve. Its floor is F: a typed `pipeline.code_preparation_failed` with one settlement, or R. Its Stop
  variant `CR-WSP-P09-lease-stop` expects `CANCELLED` once.

**F2. A Code node fails at the end of hydration, and its reserved job and runtime are orphaned (REGRESSION).**
CR-SK-P09-hyd r2, execution `4e160a9d8aeca4f419e2517aff77fa3e`, #1084 parts E/F (FU-197/198).

- The node failed about 0.3 s *before* the Supervisor kill: `agent_tool_end` was published at 12:06:43.10 and the
  kill was issued after 12:06:43.17 host time. The VM clock runs about 0.1 s ahead.
- It settled FAILED: replay `INTERNAL` with generic text, settlement `error_code` NULL.
- The cause is not observable. `hydrate_python_dependencies` errors are discarded by
  `.map_err(|_| before(Hydration, Unknown))` (`code_attempt_remote.rs:295`), and nothing logs them.
- After settlement, the execution job stayed `reserved` (lease released at 12:06:43.095, epoch 8) and its runtime kept
  running more than 4 minutes later (I6). Cleanup on a settled failure does not cover a reserved, never-dispatched job.

**F3. A Main restart during hydration hangs the run (class L).** CR-MK-P09-hyd r1, execution
`0b19b052f3345b52d60c61116ead7b47`, #1084 part I (FU-207).

- With Main down, hydration (intent finalization and content on `:9445`) failed the node instead of retrying.
- The `agent_tool_end` publish got `output_unavailable`, and the run was kept without an ack, with the frame spooled
  under claim 1.
- The takeover replays that frame under the old fence. Main rejects it with `STALE_FENCE` ("stale execution fence"),
  and every later delivery is `agent_output.invalid_durable_state`. That is the F1 loop with a different rejection.
- The public Stop returned 204 and the run stayed `RUNNING`.

**F4. A Main restart while a compiled snapshot is publishing hangs the run (class L).** CR-MK-P11 r1, execution
`3fc85dada0e76311b12bfffc043f5b45`, #1084 part I.

- The compile finished and the snapshot was recorded, but the compiled `execute` job stayed `reserved`.
- The node's `agent_tool_end` publish failed while Main was down (`output_unavailable`). The takeover replay was
  rejected with `STALE_FENCE`, and the run looped with no settlement.
- The public Stop returned 204 and the run stayed `RUNNING`.

Snapshots for the deployed re-proof are local (mode 600) under each run's `hang-snapshot/` or `orphan-snapshot-r2/`.
Each holds full dumps of both databases, the Worker spool, the NATS store and the logs. The F3 and F4 snapshots
already contain the user's Stop. When restored with the #1084 images (`crashctl restore-snapshot`), each should
settle `CANCELLED` once. The F1 snapshot is partial: the `elitea_runtime` schema only, and no NATS store.
`restore-snapshot` refuses it; `CR-WSP-P09-lease` stands in for F1.

### 9.4 Recovery-guarantee rows, T0b

| Component | Phase | Class | Status | Enforcing code | Proof |
|---|---|---|---|---|---|
| Worker (container) | P09 resolve | R | P | Preparation job committed before hydration; I2P | CR-WK-P09 ×3 |
| Supervisor (process loss) | P09 resolve | R | P (deployed) | Preparation lease reclaim | CR-SK-P09 ×3 |
| Main | P09 resolve | R | P | Worker keeps claim; preparer independent of Main | CR-MK-P09 ×3 |
| Worker (container) | P09 hydration | R | P | Per-index fresh authority `code_preparation.rs:306-358` | CR-WK-P09-hyd ×3 |
| Supervisor (process loss) | P09 hydration | **L** (1/3) | gap → #1084 E/F | Error discarded at `code_attempt_remote.rs:295`; no reap of a reserved job | CR-SK-P09-hyd r2 |
| Main | P09 hydration | **L** | gap → #1084 I | Node fails on Main loss; stale-fence spool loop | CR-MK-P09-hyd r1 |
| Worker / Supervisor / Main | P10 prepared execution | R | P | As P10 (§7) | CR-{W,S,M}K-P10-prep ×3 |
| Worker (pod) + user Stop | P09 | R (CANCELLED once) | P | Stop honoured on takeover | CR-WPK-P09 ×3 |
| Main | P11 compiled publication | **L** | gap → #1084 I | Stale-fence spool loop | CR-MK-P11 r1 |
| Worker + Supervisor (frozen past leases) | P09 | **L** | gap → #1084 1–4 | Unknown-effect receipt refused by Main; no terminal | F1 (host sleep); `CR-WSP-P09-lease` pending |

### 9.5 Not done

- `CR-WSP-P09-lease` and its Stop variant have not run. On 2026-10-12 free disk fell from 99 GiB to 15 GiB (this suite's local
  evidence is about 0.1 GiB), below the agreed 20 GiB floor for scenario runs. The session stopped there and brought
  `elitea-crash` down. Its result will be added to this PR.
- CR-MK-P09-hyd and CR-MK-P11 have one decisive repeat each. Their remaining repeats belong in the #1084 re-proof set,
  together with CR-SK-P09-hyd ×3, CR-WSP-P09-lease ×3 and the Stop variant.

## 10. Browser evidence (I9)

The first repeat of each browser-flagged scenario ran `apps/elitea-web/e2e/crash/recovery-one-answer.spec.ts` on
installed Chrome 155 against the live stack. The spec:
- signs in through the OIDC mock;
- sends the prompt through the normal composer;
- keeps the page open through the fault;
- after settlement, asserts exactly one occurrence of each answer marker and no streaming placeholder;
- reloads and asserts again.

There are no response mocks. Each result file stores a DOM text digest and a screenshot, never message text.

| Scenario | Execution | Live | After reload |
|---|---|---|---|
| CR-WK-P10 | `09462500f0839ba938b974b0ae6d0df8` | one answer | one answer |
| CX-01 | `4edc041f320c9f93951699bd379d760f` | one answer | one answer |
| CR-MK-P10 | `ec029837f0c91c413af2145722bb7304` | one answer | one answer |
| CR-NK-P10 | `a501990a8a052de9a8115b4d4f036902` | one answer | one answer |
| CR-WK-P03 | `dd071bf6fbc2a04a229970824b7aac89` | one answer, `slow-001` once | one answer |
| CR-MK-P03 | `1bc573686f9c2ed4c8260c67a95bcf03` | one answer, `slow-001` once | one answer |

## 11. Tests and checks

| Suite | Count | Result |
|---|---|---|
| `python3 -m unittest discover -s scripts/crash-recovery/tests` | 33 | pass, no skip |
| `deploy/mock-llm` `unittest discover` (CI workflow `ci-mock-llm`) | 29 (20 existing + 9 new) | pass |
| `deploy/mock-mcp` `unittest discover` (no CI) | 10 | pass |
| `oxlint --deny-warnings` on the crash Playwright files | 4 files | pass |
| `tsc --noEmit --strict` on the crash Playwright files | 4 files | pass |
| `deploy/scripts/check-compiled-sandbox-material.py` on the generated manifest and configs | 1 profile | pass |
| `helm template` with `deploy/kind-crash/values-crash.yaml` | 1 render | pass (placeholders) |
| `bash -n` on `deploy/kind-crash/*.sh` | 2 scripts | pass |

CI was not changed. Dependency scanners (`govulncheck`, `cargo deny`, `npm audit`) are not applicable, because no
dependency changed. The code-review and security-review results are recorded in the PR description.

## 12. Findings and follow-ups

1. **Node recovery without owner recovery fails every Code node.**
   - A Rust Worker with `agent_node_recovery: true` and sandbox runtimes needs Main's original-Code owner recovery
     (`ELITEA_RUNTIME_CODE_OWNER_RECOVERY_*`). Without it, every Code node fails with `authorization_denied`, before any
     dispatch, and the user sees `INTERNAL`.
   - The Helm chart accepts that combination.
   - Follow-up (offered as a separate task): fail the chart render, and consider a typed refusal.
2. **Stale matrix line.** D1's "No `agent_node_recovery` key exists in `deploy/helm`" is no longer true. The key exists
   at `deploy/helm/elitea/values.yaml:3714` (default `false`, since `00b8a259b`). G-WORKER-01 now means "off by
   default", not "no key". See the matrix diff below.
3. **Stock-flag F is generic.** Stock-flag recovery refusal gives `INTERNAL` "The runtime operation failed." with a NULL
   settlement `error_code`. It is typed, but it is not the specific refusal G-ADV-01 asks for. The fix is F7.
4. **Shared-daemon labelling.** Code runtimes carry no per-deployment label (`docker_code_jobs.rs:70`). Before any
   label-based orphan sweeper (G-SUP-03), add one (e.g. `io.elitea.code.owner=<supervisor owner>`). Proving test: two
   Supervisors on one daemon, where a sweep by one leaves the other's runtime alone.
5. **Takeover latency is about 58 s.** That is AckWait. A product budget and possibly a shorter AckWait are a separate
   decision.
6. **Inherited hangs in real data.** The real-model dump carries 23 executions with authority granted and expired
   deadlines that never settled. This is G-NATS-01/G-MAIN-02 in the data, not caused by this suite.
7. **Code durability under Main loss, lease loss and hydration (§9.3 F1–F4).** These are three hangs (class L) and
   one failure that orphaned its runtime. Stop does not settle any of the hangs. They are fixed in #1084 (pending): the
   unknown-effect preparation receipt, a terminal for a non-retryable output rejection, re-fencing pending frames on
   takeover, Stop settling while output recovery is pending, and reaping a reserved job that was never dispatched.
   The re-proof set on the #1084 images:
   - CR-SK-P09-hyd ×3, CR-MK-P09-hyd ×3, CR-MK-P11 ×3;
   - CR-WSP-P09-lease ×3 and its Stop variant;
   - restoring the F3 and F4 snapshots, each of which must settle `CANCELLED` once.
8. **Supervisor preparation observability.** The Supervisor logs nothing for preparation or hydration, and the
   Worker discards hydration error kinds (`code_attempt_remote.rs:295`). F1 and F2 had to be reconstructed from Docker
   events.
9. **Not yet built:** crashpoints (WP-2/WP-3, after #1084), the second Worker (T1: CX-05, CX-09 zombie fencing), the K
   tier (prepared in `deploy/kind-crash/`; it needs registry and Calico approvals), and the T2 gap scenarios.

### Proposed matrix diff (for a human to apply; this PR does not edit `recovery-guarantees.md`)

```diff
-| Worker (container restart) | ... | R/F²·P* (P09) | R/F²·P* (P10) | ...
+| Worker (container restart) | ... | R/F²·P* (P09) | R/F²·P (P10) | ...      # CR-WK-P10 ×3 (Docker, NATS)
-| Worker (pod replacement or >1 replica) | ... | R/F²·U (P10) | ...
+| Worker (pod replacement or >1 replica) | ... | R/F²·P (P10) | ...       # CR-WPK-P10 ×3, CX-01 ×3 (Docker)
-| Main | ... | R/F¹·P* (P03) | ... | R/F²·U (P10) | ...
+| Main | ... | R/F¹·P (P03) | ... | R/F²·P (P10) | ...                       # CR-MK-P03 ×3, CR-MK-P10 ×3
-| Supervisor | ... | R·P / F·G (P10) | ...
+| Supervisor | ... | R·P (deployed) / F·G (P10) | ...                       # CR-SK-P10 ×3, CX-01 ×3
-| NATS | I·P (P01) | R·U (P02) | R·U (P03) | ... | R·U (P10) | ...
+| NATS | I·P (P01, broker restart proven) | R·U | R·U | ... | R·P (P10) | ... # CR-NK-P01q ×3, CR-NK-P10 ×3
-| Worker (container restart) | ... | R/F¹·P* (P02) | R/F¹·P* (P03) | ...
+| Worker (container restart) | ... | R/F¹·P (P02) | R/F¹·P (P03) | ...     # CR-WK-P02 ×3, CR-WK-P03 ×3
 D1:
-- `deploy/helm/elitea/templates/worker/configmap-runtime.yaml:62-64` renders only `agent_model_checkpoint_recovery`. No `agent_node_recovery` key exists in `deploy/helm`.
+- `deploy/helm/elitea/templates/worker/configmap-runtime.yaml:62-66` renders `agent_model_checkpoint_recovery` and `agent_node_recovery` (`values.yaml:3710,3714`, both default false). The stock F side is measured: CR-WK-P03-stock settles FAILED, INTERNAL, generic text.
 D3:
+- NATS-era deployed evidence on main (crash-recovery-suite-20261009): Worker, Main, Supervisor and NATS kills at P10; Worker and Main kills at P02/P03; a combined M+W+S kill.
 §6 "Supervisor x P10 | crash, exit (runtime alive)":
-| R | P (PostgreSQL component only) | ...
+| R | P (component + deployed: CR-SK-P10, CX-01) | ...
 §6, T0b (crash-recovery-suite-20261009 §9):
-| Main x P09 | crash | R/F² | P* | ...
+| Main x P09 | crash (resolve) | R/F² | P | ... | CR-MK-P09 ×3 | ...
+| Main x P09 | crash (hydration) | L | gap: #1084 | Node fails on Main loss; stale-fence spool loop | CR-MK-P09-hyd r1 | Stop does not settle. |
-| Main x P10 | crash | R/F² | U | ...
+| Main x P10 | crash | R/F² | P | ... | CR-MK-P10 ×3, CR-MK-P10-prep ×3 | ...
-| Main x P11 | crash | R | P* | ...
+| Main x P11 | crash | L | gap: #1084 | ... | CR-MK-P11 r1: Main killed after compile while publishing; stale-fence spool loop, no settlement | Supersedes chat 809's narrow P*. |
-| Worker x P09 | crash | R/F² | P* (container), U (pod) | ...
+| Worker x P09 | crash | R/F² | P (container: resolve and hydration), P (pod, Stop path) | ... | CR-WK-P09 ×3, CR-WK-P09-hyd ×3, CR-WPK-P09 ×3 (CANCELLED once) | ...
+| Worker + Supervisor x P09 | frozen past the claim and job leases | L | gap: #1084 | Unknown-effect receipt refused by Main; no terminal; Stop does not settle | F1 (`097ca8e5…`), CR-WSP-P09-lease (pending) | |
-| Supervisor x P09 | crash, exit | R | P | ...
+| Supervisor x P09 | crash, exit | R (resolve) / R 2 of 3, L 1 of 3 (hydration) | P / gap: #1084 | ... | CR-SK-P09 ×3; CR-SK-P09-hyd r2: node failed at the end of hydration and its runtime was orphaned | |
 Supervisor x P10:
+| ... | CR-SK-P10-prep ×3 (prepared job) | ...
```

Raw evidence (`evidence.json`, `verdict.json`, Docker event streams, browser results) stays in the local run
directory `handoffs/crash-recovery-suite/runs/` and is not committed. Its paths and execution ids are cited above.

## 13. Re-proof on the #1084 fix images (2026-10-12)

Images were built from #1084 commit `9bf82d60d` (merged with main `719d12178`):

| Service | Tag | Image | Binary |
|---|---|---|---|
| Main | `elitea-main:code1084-9bf82d60d` | `sha256:811f516d53d9` | `ae39dfb9c01d73ce…` |
| Worker | `elitea-worker-rust:code1084-9bf82d60d` | `sha256:4bfaea5b9720` | `d38a613217dcc1a4…` |
| Supervisor | `elitea-sandbox-supervisor:code1084-9bf82d60d` | `sha256:efacee7226c2` | `a673eea7218c8819…` |
| Web | `elitea-web:code1084-9bf82d60d` | `sha256:f18101e4ef63` | — |

The Code runners, the gateway and the sub-app images are unchanged.

**§3b binary check.** The fix markers are present: Worker `code_platform_effects` ×6, `release_undispatched` ×3,
`code_hydration_failed` ×1; Main `resource_project_id=$1::integer` ×1, `original source application handle differs` ×1;
Supervisor `sandbox_hydration_refused` ×1. The old images (Worker `c0f2e5f9b`, Main `c0f2e5f9b`, Supervisor
`58abb650c`) are the negative controls and show 0. `524165ed09` (#1160) is an ancestor of `9bf82d60d`.

**Stack.** The merge of main into this branch brought in elitea-vector, the Rust Inventory engine and a new sub-app
host contract.
- The fix-image stack runs elitea-vector from the local `:standalone` image and Qdrant `v1.19.2`.
- It sets `INVENTORY_RUNNER=legacy`, as the `elitea-code1084` stack does.
- Compose built the missing `elitea-inventory-engine:local` image (a sidecar not under test) once from this branch.
- The old-image baseline ran from a detached checkout of the pre-merge commit `3e0be8244`, with the same stack
  material.

**Baseline on the old images.** `CR-WSP-P09-lease` reproduced F1 deterministically, with no host sleep, as execution
`d86f6b9385518f966e4896f273ba1f2a`:
1. `sandbox.deadline_exceeded` on the preparation job;
2. the recovery event was rejected as `PROTOCOL_VIOLATION`;
3. a `STALE_FENCE` / `invalid_durable_state` loop followed;
4. Stop returned 204 and did not settle it.

Its full snapshot is the F1 restore below.

### #1084 gap re-proof (fix images, run fix1084-gap-20261012)

| Row | Verdict (repeats) | Observed | fault→settled s (each; admit→settled for no-fault rows) | median | Executions |
|---|---|---|---|---|---|
| CR-MK-P09-hyd | PASS (3/3) | R | 118.1, 115.5, 115.2 | 115.5 | 1c751aa0, a2278cb0, a8206c5d |
| CR-MK-P11 | PASS (3/3) | R | 62.8, 62.6, 65.2 | 62.8 | f19c726f, 827c1a5a, d004a230 |
| CR-SK-P09-hyd | PASS (3/3) | R | 63.3, 114.4, 114.1 | 114.1 | 75ac42cb, 76ec9c68, fcc33e54 |
| CR-WSP-P09-lease | PASS (3/3) | F,R | 161.5, 159.4, 158.9 | 159.4 | 89b2b8e4, de542983, e461343a |
| CR-WSP-P09-lease-stop | PASS (1/1) | R | 101.5 | 101.5 | f4f5a084 |

### Language matrix (fix images; runs fix1084-lm-20261012, fix1084-npm-20261012)

| Row | Verdict (repeats) | Observed | fault→settled s (each; admit→settled for no-fault rows) | median | Executions |
|---|---|---|---|---|---|
| BASE-python-prepared | PASS (1/1) | R | 67.5 | 67.5 | a6c0c21b |
| CR-WK-P09 | PASS (3/3) | R | 113.1, 113.4, 114.9 | 113.4 | e58c70af, 4b89bd9b, 29237bf9 |
| LM-BASE-javascript | PASS (2/2) | R | 52.4, 55.2 | 53.8 | 113fa7f3, 57c9d853 |
| LM-BASE-python | PASS (4/4) | R | 109.2, 65.6, 68.5, 67.5 | 68.0 | eb9f75da, 92a0de97, a3abf314, cb063390 |
| LM-BASE-typescript | PASS (1/1) | R | 55.0 | 55.0 | 1d97523e |
| LM-MK-javascript | PASS (3/3) | R | 48.0, 65.6, 47.7 | 48.0 | 21de35de, 2e584bc4, ad94ae2b |
| LM-MK-python | PASS (3/3) | R | 67.8, 56.6, 69.7 | 67.8 | 21771006, 15c9808c, 216d4126 |
| LM-PREP-WK-javascript | PASS (2/2, 1 inconclusive) | R | 116.2, 113.0 | 114.6 | df89944b, 76f47b89, 18e5dc41 |
| LM-PREP-WK-typescript | PASS (1/1, 2 inconclusive) | R | 112.4 | 112.4 | 015a358f, 9f236c24, 4199852c |
| LM-PREP-javascript | PASS (1/1) | R | 57.0 | 57.0 | 727c2419 |
| LM-PREP-typescript | PASS (1/1) | R | 56.8 | 56.8 | 911d33dd |
| LM-SK-javascript | PASS (3/3) | R | 60.3, 59.4, 59.0 | 59.4 | 1055f0d5, d40c6723, 35846c06 |
| LM-SK-python | PASS (3/3) | R | 172.2, 67.0, 66.2 | 67.0 | 9f221e93, a057c7b1, 102113af |
| LM-STOP-javascript | PASS (2/2) | R | 5.5, 4.7 | 5.1 | a9062a19, 651bdfe0 |
| LM-STOP-python | PASS (3/3) | R | 1.4, 9.7, 0.9 | 1.4 | f60bc826, 6826fa90, b6706252 |
| LM-THROW-python | PASS (1/1) | F | 6.2 | 6.2 | dc43acf3 |
| LM-WK-javascript | PASS (3/3) | R | 56.4, 57.3, 61.7 | 57.3 | 679570a1, 228d5eb9, 3407c649 |
| LM-WK-python | PASS (3/3) | R | 79.8, 127.6, 130.8 | 127.6 | f65e3e53, e5374766, 9f85a8c1 |

### Snapshot restores on the fix images (run fix1084-restore-20261012)

| Snapshot | Execution | Expected | Result | Settlements | NATS drained | Live jobs | Running runtimes | waited s |
|---|---|---|---|---|---|---|---|---|
| F1-lease-baseline | d86f6b93… | CANCELLED | PASS (CANCELLED) | 1 | True | 0 | 0 | 0.2 |
| F3-main-hydration | 0b19b052… | CANCELLED | PASS (CANCELLED) | 1 | True | 0 | 0 | 0.3 |
| F4-main-p11 | 3fc85dad… | CANCELLED | PASS (CANCELLED) | 1 | True | 0 | 0 | 0.2 |
| F2-orphan | 4e160a9d… | FAILED | PASS (FAILED) | 1 | True | 0 | 0 | 0.2 |


**Restores.** Each restore ran `crashctl restore-snapshot` into a fresh stack on the fix images, then
`crashctl observe <execution>`. "waited s" is the time from the end of the restore until the settlement was seen;
every restored run settled while the stack started. F2 (CR-SK-P09-hyd r2) had already settled FAILED, so its restore
proves the reaping only: no reserved job and no runtime is left.

On the fix images the four §9.3 findings are proven fixed (#1084):
- F1 settles typed (`PIPELINE_CODE_FAILED`, "Code dependency preparation failed. Later nodes did not run. Share the
  support reference with your administrator before retrying.") or completes. The typed code is on the replay terminal
  and the chat answer; the settlement `error_code` stays NULL.
- F2–F4 resume (R).
- Stop settles CANCELLED once in every restored hang.

## 14. Code language matrix (fix images; in progress)

Every Code language is tested under the same faults, using one language-parameterised fixture (`code-lang.yaml` and
`seed.py` `LANGUAGES`; no YAML is copied per language). The catalog expands `catalog/language-matrix.json` into
`LM-<FAULT>-<lang>` rows:
- languages: python, javascript, typescript, rust;
- faults: BASE, WK, SK, MK (kill9 at P10), STOP (user Stop at P10), THROW (no fault; the user's Code throws).

A row the deployment cannot run is reported NOT-SUPPORTED with its reason, never skipped.

**Package preparation per language:**
- **Python, PyPI.** The `preparation` profile, rows aliased to BASE-python-prepared and CR-WK-P09.
- **Rust.** A dependency-free node uses the compiled-snapshot path (BASE-rust-compiled, CR-MK-P11).
- **JS/TS, npm.** This needs the native Deno profile (stack feature `npm`).
  - JavaScript and TypeScript each get a native preparation listener.
  - JS/TS execution moves to a native Deno listener (`dns:elitea-sandbox-deno-native`); Python keeps the Deno
    listener.
  - The preparers share the resolver bridge.
  - Compose cannot restrict egress to `registry.npmjs.org` and `jsr.io` (§9.2).
  - With `npm` off, the JS/TS npm rows report NOT-SUPPORTED.

Results so far are in the language-matrix table above. The fault→settle medians are the baseline for FU-215 (AckWait
and leases toward ≤ 20 s forced-crash recovery):
- a Worker or Supervisor kill at P10 settles in about 57–67 s, dominated by the 60 s AckWait;
- a Main kill at P10 settles in about 48–68 s;
- a kill during preparation settles in about 113 s;
- a Stop settles in 1–10 s.

**Still to run** (the session paused on 2026-10-12):
- LM-STOP-javascript r3 and LM-THROW-javascript;
- the TypeScript fault rows with `npm` off;
- all Rust rows;
- the T0+T0b regression ×1;
- three npm Worker-kill repeats whose trigger missed the ~2 s resolve (the trigger now polls every 0.1 s).

**Harness incidents fixed during the run:**
- I2P allowed only one preparation per run, but a pipeline with two prepared nodes prepares twice; it now allows one
  per Code activation.
- I2 counted preparation jobs as Code re-runs.
- The crash stack's Postgres ran as PID 1 and reaped an orphaned process that exited 2, which restarted every backend.
  It now runs with `init: true`. This was a test-stack configuration issue, not a product defect.

