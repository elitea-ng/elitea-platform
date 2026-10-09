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

Across every R run:
- **No user Code ran again.** Each sandbox job had one runtime start, and the fixture's start time precedes the cut.
- **One execution, one settlement, one answer** after reload.
- **Fencing held.** Claim attempts are contiguous and epochs increase. No superseded writer was accepted.
- **NATS drained:** zero messages, pending and ack-pending.
- **No orphan runtime** after 60 s.

A recovery on the model path calls the model once more. This is the documented "recovery can bill once more".

T0b results are in §9.

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

T0B_RESULTS_PENDING

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
| `python3 -m unittest discover -s scripts/crash-recovery/tests` | 26 | pass, no skip |
| `deploy/mock-llm` `unittest discover` (CI workflow `ci-mock-llm`) | 29 (20 existing + 9 new) | pass |
| `deploy/mock-mcp` `unittest discover` (no CI) | 9 | pass |
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
7. **Not yet built:** crashpoints (WP-2/WP-3, after #1084), the second Worker (T1: CX-05, CX-09 zombie fencing), the K
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
```

Raw evidence (`evidence.json`, `verdict.json`, Docker event streams, browser results) stays in the local run
directory `handoffs/crash-recovery-suite/runs/` and is not committed. Its paths and execution ids are cited above.
