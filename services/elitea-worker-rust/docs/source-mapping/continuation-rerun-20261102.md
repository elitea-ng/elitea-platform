# A decided continuation lost before its first root save re-runs (FU-269), 2026-11-02

- **Branch:** `fix/fu269-continuation-rerun`, off `main` `88ce1d7bc`.
- **Plan:** `handoffs/point5-wave1-20261008/fu269/PLAN.md`. The orchestrator approved option D with decisions 1–3 and
  changes E1–E7 (`PLAN-REVIEW.md`).
- **Scope:** contract (`control.proto`), Main (migration, admission, claim), Worker.
- **Supersedes:** FU-265's typed refusal of the re-run (`continuation-lost-before-root-save-20261030.md`). FU-265's
  rule for a non-continuation run (D1) is unchanged.

## Business behaviour

- **The run.** A decision on a paused pipeline (an approval, an answer, an MCP authorization or a static-tools
  selection) starts continuation B, which re-enters the root checkpoint row written by the paused execution A.
- **Before.** When B was lost before its own first root save, the reclaim ended with the registered
  `RECOVERY_FRONTIER_UNSUPPORTED` terminal (FU-265). The user had to start again.
- **Now.** The reclaim runs B again from A's row, and each call's journal and dispatch records decide it:
  - **R:** a committed result is replayed, with 0 new calls;
  - **C:** a call that may have run is never repeated. In a saved child it ends with the registered may-have-started
    reason (F); on a root direct node it is escalated to the operator reconcile pause, as any reclaim of that node is;
  - **I:** a call that never started runs once, and a HITL node's decision is applied once.
- **Current platform.** A lost continuation is lost; the user decides again. That is not ported.
- **Rollout.** There is no runtime switch and no ordering rule:
  - a new Main with an older Worker: the Worker ignores the unknown protobuf field, the command bytes are unchanged, and
    the continuation runs as today;
  - a new Worker with an older Main: no field, so FU-265's typed refusal on reclaim applies;
  - `UPGRADING.md` needs nothing.

## How Main names the paused writer (option D)

- **Contract:** `ClaimReceiptV1.paused_writer`, a `PausedWriterV1 { execution_id, generation }`
  (`libs/proto/elitea/runtime/v1/control.proto:124`). An older Worker ignores it; the decode test is
  `an_older_worker_ignores_the_paused_writer_on_its_claim_receipt`.
- **Admission** (`services/elitea-main/internal/infra/db/repos/agent_execution_jobs.go:737`, then
  `continuation_paused_writer.go:73` `recordPausedWriter`):
  - It runs in the admitting transaction, before the resume consumes the pause.
  - It locks the response row (`readPausedWriter`, `:32`, `FOR UPDATE OF g`) and reads A (`chat_message_group.task_id`)
    and the generation of A's single `SUCCEEDED` settlement.
  - It stores them on the job, in shared migration `0218_agent_continuation_paused_writer.sql`.
- **Written once.** The `UPDATE … WHERE paused_execution_id IS NULL` is the only writer of these columns. A CHECK
  requires both columns or neither, a 32-hex id, a generation of at least 1, and a writer different from the job's own
  execution.
- **Naming none (deviations from decision 2, accepted by the orchestrator).** Decision 2 said "none or more than one
  is refused"; instead, Main names no writer:
  - **No SUCCEEDED settlement** (a response paused before settlements existed): refusing would make it unresumable.
  - **Two or more SUCCEEDED settlements:** this breaks the one-settlement invariant above. Main never picks one, and
    logs a warning with the paused execution id and the generations only (`readPausedWriter`; asserted in
    `TestPostgresPausedWriterIsTheResponsesSettledPausedExecution`).

  In both cases the continuation's first attempt runs exactly as before FU-269
  (`postgres_a_continuation_without_a_named_writer_resumes_on_its_first_attempt`), and a reclaim lost before its first
  root save ends typed. An output-limit continuation, or a paused execution that is not a runtime id, also names
  none.
- **Claim** (`services/elitea-main/internal/infra/db/repos/claims.go:225,503`, then `continuation_paused_writer.go:112`
  `selectClaimPausedWriter`):
  - The writer is read from the job row and served only on ACCEPTED, `RECOVER_AGENT_MODEL_CHECKPOINT` and
    `RECOVER_NODE_VISIT` agent claims (E1).
  - `ClaimDecision.validate` refuses it on any other disposition or capability, on the claimed execution itself, on an
    id that is not 32 lowercase hex, and on generation 0 (`internal/application/execution/claims.go:145`).
  - `server.go:307` copies it onto the receipt.
- **An older Main replica (E1).** It never reads or writes the new columns. Its only insert into
  `agent_execution_jobs` names its columns (`internal/db/queries/runtime_agent_execution.sql`
  `InsertAgentExecutionBinding`), and no query copies or selects the row with `*`.

## Worker

- **The claim carries the writer** (`src/protocol/control.rs:3744`) to `ClaimBoundSessionAuthority::paused_writer`
  (`:1336`). All three claim kinds decode through `parse_input_claim_binding`.
- **`admit_pipeline_plan`** (`src/agents/runtime.rs:469`) validates it with `PausedWriter::from_claim`
  (`src/agents/graph/continuation_writer.rs:31`). E2 refuses:
  - a malformed field;
  - the claimed execution itself;
  - generation 0;
  - a start that does not re-enter a paused row.
- **Decision 2:** the FU-265 meta key is removed. The strict common profile is restored, so a continuation that still
  carries the key is refused (`the_retired_paused_writer_meta_key_is_refused`).
- **`continuation_entry`** (`src/agents/session.rs:1875`), used on the first attempt (`:2751`) and on the reclaim
  (`:1977`), proceeds in three steps:
  1. It proves the paused row (`prove_continuation_row`, `:1814`): the exact writer, a pause frontier on this root
     thread, and the latest pause event naming the row. The session is restored once and passed on (E7).
  2. It reads the events after that pause (`owned_continuation_suffix`, `src/agents/dispatch_journal.rs:312`; E5,
     below).
  3. Nothing after the pause means a first attempt. An owner-proven suffix means a re-run, and the resolvers read
     `PauseView` (`continuation_writer.rs:69`, E6). Anything else is refused typed with
     `pipeline.recovery.continuation_suffix_unowned` (`session.rs:2137`), logged ids-only.
- **Re-run.** `RecoveryFrontier::ContinuationPause` re-runs the continuation start (`src/agents/pipeline.rs:1426`),
  the same path as a lost fan-out wake. The continuation's resolver binds the decision to the paused row again, so no
  frontier is resumed blind (`session.rs:1995`).
- **E3, the `continuing` record** (`session.rs:3136`). With the writer, the continuation appends one owner-bound
  `continuing` record before its Runner starts: the first event after the pause. Arms that already wrote one (static
  tools, Application decisions) keep theirs; a re-run writes none. Without the writer, nothing changes. The record has
  no content or state delta, so ADK history, the trace and the browser projection ignore it, as they ignore every
  dispatch record (`dispatch_journal.rs` module doc).
- **E4, no second user event.** When the suffix already holds the continuation's user event (its resume marker or its
  answer), the Runner gets empty input, as a checkpoint recovery does (`session.rs:2758`).

### E5: the owner-proven suffix, exactly

Let `p` be the latest pause event. It must name the paused row (`prove_continuation_row`). The events after `p` are
owner-proven when:

1. the first is a dispatch record with phase `continuing`, owned by `(B, g)`, whose `call_id` is `p`'s event id;
2. every other dispatch record is owned by `(B, g)`;
3. every other event is one the continuation's own start path writes, as `ContinuationSuffix::owns` admits:
   - its resume marker, or its own user input (a text answer);
   - the root's replayed Application call, and each child's replayed call with its results;
   - content-free bookkeeping.

Anything else is refused. That covers:

- another execution's record;
- a second pause;
- a child's finished result;
- a later model turn;
- a model-checkpoint record;
- a record that does not read.

- **`g` is the claimed generation, not "g < current generation" (deviation from E5, accepted by the orchestrator).**
  - Main admits an agent execution at generation 1: `services/elitea-main/internal/application/agentexecution/admission.go:256`
    (the job) and `:265` (the outbox).
  - A reclaim keeps that generation. It allocates the next claim attempt within the same `(execution, generation)`:
    `services/elitea-main/internal/infra/db/repos/claims.go:350-352`, `MAX(claim_attempt) + 1`.
  - So the owner is exactly this execution, which is the rule `continuation_pause` already applies.
- **A generation above 1 does not occur for an agent execution today.** Main's readers do not assume 1: they take the
  settled generation as stored. The real-PG test settles generation 3 to prove it, beside a FAILED generation 1 that is
  ignored. If a later change introduced re-admission under a new generation, it would be a new claim identity, and the
  suffix rule would treat the earlier generation's records as foreign (refused typed), never as owned.

## Changed paths

- `libs/proto/elitea/runtime/v1/control.proto`, plus the generated Go and Python code.
- `services/elitea-main/migrations/shared/0218_agent_continuation_paused_writer.sql`, the baseline schema and sqlc
  models, and the manifest head test.
- `services/elitea-main/internal/infra/db/repos/continuation_paused_writer.go` (new), `agent_execution_jobs.go`,
  `claims.go`.
- `services/elitea-main/internal/application/execution/claims.go`.
- `services/elitea-main/internal/transport/runtimegrpc/control/server.go`.
- `services/elitea-worker-rust/src/agents/graph/continuation_writer.rs`, `session.rs`, `dispatch_journal.rs`,
  `runtime.rs`, `pipeline.rs`, `assembly.rs`, `graph/fanout_wake.rs`.
- `services/elitea-worker-rust/src/protocol/control.rs`, `node_recovery_inspection.rs`.

## Tests

**Worker, two processes, real PostgreSQL** (`src/agents/pipeline_fanout_effect_pg_tests.rs`: `continuation_scenario`
plus the `continuation_case!` macro). Each case: A pauses, B is admitted with the writer and lost at the point named,
then reclaimed.

| Case | Expected |
|---|---|
| Saved child, depth 1, started | C → F, `RECOVERY_REFUSED_UPSTREAM`, 0 calls |
| Saved child, depth 1, committed | R, 0 calls, only the next model step |
| Saved child, depth 1, undispatched | I, the call runs once |
| Depth 2, started | C → F |
| Depth 2, committed | R |
| Depth 2, undispatched | I |
| Root direct node, started | C: the operator reconcile pause, 0 calls |
| Root direct node, lost before its Runner | I, runs once |
| Root HITL node, lost before its Runner | the decision is applied once |
| No writer (older Main) | `root_writer_mismatch`, typed, ids-only log |
| No writer named, first attempt, no loss | resumes and completes, the call runs once, no `continuing` record |

**Every case also asserts:**

- one `continuing` record with the writer, none without it (E3);
- the re-run adds no `continuing` record and no resume marker, and removes nothing (E4, E6).

**Worker unit tests:**

- `only_this_executions_own_suffix_lets_a_continuation_re_run` (E5);
- `the_pause_view_ends_at_the_pause_and_hides_nothing_it_does_not_own` (E6);
- `the_claim_field_is_optional_strict_and_matches_only_its_writer` (E2);
- `a_paused_writer_is_refused_on_a_fresh_run` (E2);
- `the_retired_paused_writer_meta_key_is_refused`;
- `an_older_worker_ignores_the_paused_writer_on_its_claim_receipt`;
- `continuation_refusals_are_recovery_frontier_causes`.

**Main, real PostgreSQL** (`continuation_paused_writer_postgres_integration_test.go`):

- a single settlement (generation 3) is named, and a FAILED generation 1 is ignored;
- none or two settlements name none;
- the read under the response lock;
- C5, project isolation;
- the job row stores the writer before the resume moves the response, and never rewrites it;
- the CHECK refuses its own execution, generation 0, half a writer and a non-id;
- no single settled pause, an output limit or a non-runtime id stores none;
- the claim serves the writer only on the three dispositions, for that job and generation;
- through the production admission service and claim path: a fresh run is served none, and a job holding a writer
  has it served and validated.

**Main unit tests:**

- `TestClaimDecisionPausedWriterIsBoundToAgentWork`;
- `TestClaimReceiptCarriesThePausedWriterOnlyWithInputs`.

**Counts:** TO FILL (full Worker and Main suites).

## Performance

- **Admission:** one locked single-row read joined to settlements by primary-key prefix, and one single-row `UPDATE`,
  only for a continuation.
- **Claim:** one primary-key read of the job row, only on the three agent dispositions.
- **Worker first attempt:** one checkpoint load and one session restore, reused by the arm (E7 removed the second
  restore). The suffix scan is linear in the events after the pause.
- **Re-run:** the same as a lost fan-out wake's re-run.

## Durability

- **Admission and claim.** The writer is stored in the admitting transaction, before the pause is consumed, and
  served only from the job row. A lost claim reply serves it again.
- **Re-run.** The continuation re-runs behind its own durable records only. The journal's committed result, Started
  record or absence decides each call. The re-run deletes nothing: the view is read-only, and writes append after the
  existing suffix.

## Resilience

- A continuation lost before its first root save now continues: R, C or I per call, where FU-265 had a typed stop.
- A foreign or ambiguous suffix, or a mismatched or missing writer, ends with the registered
  `RECOVERY_FRONTIER_UNSUPPORTED` terminal, never Internal.

## Security

- **Where the writer comes from.** Main reads it in the admission transaction that authorized the decision, under the
  response lock, in the deciding project's schema (C5 test). It is served only to the fenced claim of that job and
  generation. The client never supplies it.
- **It grants nothing by itself.** The Worker also requires:
  - an exact writer match on the root row;
  - the latest pause naming that row;
  - an owner-proven suffix.

  A malformed writer, one naming the claimed execution itself, or one on a non-re-entering start is refused (E2).
- **Logging.** Refusal logs carry ids and generations only (FU-265 C4 test).
- **Parsing.** Strict: the meta key is gone and the common profile is strict again.
- **Unchanged:** routes, authorization (the decide and continue routes are untouched), egress and credentials. All SQL
  is parameterized.
- **Scanners:** no dependency manifest changed. `cargo deny` and `govulncheck` report the same results as main.

## Recovery rows (gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × a decided continuation, approved call in a saved child (depth 1–2), lost before its first root save | R (committed) / C → F (started) / I (undispatched) | `session.rs:1875,1977`, `pipeline.rs:1426`, `dispatch_journal.rs:312` | the six depth cases above |
| Worker × a decided continuation, root direct node, lost before its first root save | C (operator reconcile pause) / I | the same, and the node-recovery journal | the root cases above |
| Worker × a HITL-node decision lost before its first root save | R (applied once) | the same | the gate case above |
| Main × continuation admission | I (the writer is written once, in the admitting transaction) | `continuation_paused_writer.go:73` | Main real-PG tests |
| Worker × a continuation for which Main names no writer (no SUCCEEDED settlement, or two), lost before its first root save | F typed (no writer named): `RECOVERY_FRONTIER_UNSUPPORTED` (`root_writer_mismatch`); the first attempt resumes as before | `session.rs` `inspect_checkpoint` writer `else` branch; `continuation_paused_writer.go` `readPausedWriter` | `postgres_a_continuation_without_its_paused_writer_is_refused_typed_on_reclaim`, `postgres_a_continuation_without_a_named_writer_resumes_on_its_first_attempt` |
| Worker × a non-continuation run lost before its first root save | unchanged: F, FU-265 D1, FU-270 | — | — |

**Gap (stated, not proven):** a root direct node whose committed result was lost before the root save has no crash
fixture, because this fixture's root saves right after its only node. Its class on re-run is not measured here.

## Real-browser evidence

TO FILL (fanpar).

## Fixtures

- Built in code. A's pause is real (the pause phase runs the pipeline).
- The loss points are real process exits:
  - inside the journaled call;
  - at the next model step;
  - before the Runner.
- Two are narrowed by deleting rows, as in FU-248: `committed` removes the child's row saved after the call, and
  `undispatched` removes the journal.

## Follow-ups

- FU-270 (a fresh turn re-run from its input) is unchanged.
