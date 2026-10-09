# Point 5 Wave 1 Track M2: per-interrupt decision ledger and public decision API (2026-10-08)

Branch `feat/main-execution-interrupts-ledger`, stacked on `docs/graph-point5-v2-contracts` (PR
elitea-ng/elitea-platform#1143, the D2 contract). Draft PR elitea-ng/elitea-platform#1150 targets that branch and is
retargeted to `main` after #1143 merges. Brief:
`.claude/handoffs/point5-wave1-20261008/T-M2-interrupt-ledger.md`.

**Status:** built, not wired.
- **Built:** the ledger, its repository and the public API. The API is registered only when
  `ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED=true`.
- **Off by default:** a production build has the flag off, and the flag requires agent dispatch.
- **Wave 2:** the frame projection (`Raise`), the private Worker fetch/ACK routes, the stop/regenerate call sites
  (`CancelAllForResponse` / `SupersedeForResponse`), settlement, park/wake and the Web callers.
- **Unchanged:** the existing complete-set HITL resume path. It has no diff, and its tests pass as before.
- **Off-limits respected:** no file changed by #1084 is edited, and there are no CI changes.

## Business behaviour taken from the current platform, and what was not ported

Taken:
- **Who may decide.** The conversation author or the question author, who must still be a user participant of the
  conversation. This is the rule of today's continuation (`internal/db/queries/agent_chat.sql`
  `ResolveCurrentContinuation`, which mirrors EliteaUI and Pylon chat continuation). Project RBAC is checked with
  `models.chat.messages.create` (POST) and `models.applications.task.get` (GET), the same permissions node recovery
  uses.
- **Action vocabulary.** approve, reject, edit, block_with_comment, answer, authorize, skip, plus `continue` for a
  static pause (user decision 2026-10-08).
- **Multi-tab behaviour.** A repeated identical submit is answered as a replay. Any other conflict is a quiet 409
  with a `*_already_resolved` code, the same family as `agent_hitl_already_resolved`.

Not ported:
- **The complete-decision-set rule** (`continue.go:409-432`, SQL `ResumeCurrentAgentHITL`). The ledger decides each
  card on its own; the root path moves to the ledger in Wave 2.
- **Pending cards stored only in `chat_message_group.meta.hitl_interrupts`.** A concurrent JSON rewrite cannot
  consume one card exactly once.
- **The legacy park/reconcile/replay supervisor.** Nothing here parks children or replays decisions.
- **Raw tokens in decisions.** Delegated auth carries an opaque `credential_ref` (16–128 URL-safe characters), and a
  body with any extra field, a raw token for example, is refused.

## Changed paths

New:
- `services/elitea-main/migrations/shared/0157_execution_interrupts.sql`:
  - `execution_interrupt_responses` (:18), with `decision_revision`;
  - `execution_interrupts` (:28), with decision/consumption column groups and state coherence (:79);
  - one open `interrupt_id` per response (:95);
  - FK-support indexes (:101, :103);
  - `execution_interrupt_audit` (:106).
- `services/elitea-main/internal/domain/executioninterrupt/`:
  - `limits.go:13-55`: every bound is a named constant;
  - `canonical.go:24` `Canonicalize`: the contract canonical JSON, with strict decoding (duplicate keys, depth 8,
    exact int64, UTF-8);
  - `schemas.go:34`: embedded byte-identical copies of five D2 schemas, compiled once with
    `santhosh-tekuri/jsonschema/v6` (already a direct dependency);
  - `card.go:44` `ParseCard`, `decision.go:23` `ParseDecisionRequest`, `decision.go:49` `DecisionSHA256`,
    `ack.go:25` `ParseAck`, `ack.go:59` `Frontier.CanonicalJSON`;
  - `ledger.go`: selectors, inputs and results;
  - `contract.go`: typed errors, plus id validators that reuse `domain/noderecovery`.
- `services/elitea-main/internal/infra/db/repos/execution_interrupts.go`:
  - `lockOwnedResponse` :74;
  - `raiseExecutionInterrupt` :133;
  - `List` :243;
  - `Decide` :309;
  - `interruptClaimAuthoritySQL` :410, looked up twice at :447;
  - `FetchDecided` :473;
  - `Ack` :548;
  - `closeOpenExecutionInterrupts` :649.
- `services/elitea-main/internal/api/v2/agentexecution/interrupts.go`:
  - `NewCurrentExecutionInterruptRoute`;
  - the `MaxBytesReader` body cap :121;
  - `interruptLogFields` :63;
  - `writeInterruptJSON` :176;
  - `writeInterruptError` :195.
- Tests:
  - `domain/executioninterrupt/{contract,limits}_test.go`;
  - `repos/execution_interrupts_postgres_integration_test.go`;
  - `repos/execution_interrupts_http_postgres_integration_test.go`;
  - `api/v2/agentexecution/{interrupts,interrupts_spec}_test.go`.

Edited:
- **Flag and composition:**
  - `runtimecomposition/config.go`: the flag at :76 and :210; `Validate` :347 requires agent dispatch; the
    runtime-disabled branch refuses the flag;
  - `composition.go:617`; `public_routes.go`; `cmd/elitea-main/main.go:2389`;
  - `api/router.go`; `api/production_router.go:233-235` (mounts only when composed).
- **Deprovisioning:** `application/projectprovisioning/steps.go:348` deletes the audit rows, cards and response rows
  before job rows. The existing guard `TestEveryBlockingForeignKeyIsCovered` caught the gap.
- **OpenAPI:** `api/openapi/v2.yaml` adds `listExecutionInterrupts` and `decideExecutionInterrupt`, tagged `chat`,
  not `client`. The card is a closed `ExecutionInterruptCard` schema.
- **Generated code:**
  - `internal/api/generated/api.gen.go`, regenerated with oapi-codegen v2.7.2 (the CI pin), additions only;
  - `apps/elitea-web/src/shared/api/generated/**`, regenerated with `node scripts/check-generated-client.mjs --write`
    (orval 8.33.0): 6 new files and 3 updated.
- **Pins:**
  - `apps/elitea-web/scripts/check-endpoint-manifest.test.mjs`: 313 → 315 operations (309 → 311 before main added the desktop operations);
  - `migrate/manifest_test.go`: head 157.
- **Chart:** `deploy/helm/elitea/templates/main/_helpers.tpl` renders
  `ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED: "false"` beside the other runtime flags, so
  `deploy/helm/tests/render-capabilities.sh` (which requires every name `config.go` looks up) passes and the gate stays
  off in every chart deployment.

## Design decisions to review

1. **The CAS runs under READ COMMITTED behind explicit row locks, not SERIALIZABLE.**
   - **Lock order:** `chat_message_group` response row → `execution_interrupt_responses` row → job and claim rows →
     card rows.
   - **Why:** under SERIALIZABLE, every losing tab of a contended card fails with SQLSTATE 40001 and needs a retry
     loop. With explicit locks, a waiting transaction re-reads the committed winner and answers a replay or a 409.
   - **Same guarantees:** the CAS `UPDATE … WHERE state='PENDING' AND revision=$expected` and the CHECKs give the
     same exactly-once result.
   - **The trap, and how it is handled:** a READ COMMITTED statement that waited for a lock does not refresh the rows
     it joined. So every authority predicate is evaluated again in a new statement after the lock:
     - ownership and membership: `lockOwnedResponse` :74;
     - RBAC: `Decide` :328;
     - the claim lease, session, deadline and terminal checks: the second lookup at :447.
   - **Proven by:** `TestDecideRechecksMembershipAfterTheLockWait` and `TestFetchRechecksTheClaimAfterTheLockWait`.
     Both fail on the mutant that skips the recheck.
   - The adversarial reviewer confirmed that SERIALIZABLE would not have caught these cases either.
2. **Inline parameterized SQL, not sqlc.** This is the pattern of the node recovery repository (also
   `elitea_runtime`, also not in the sqlc schema projection). Every statement is covered by real-PostgreSQL tests.
3. **Cards are validated against the contract schemas themselves**, using embedded copies plus a byte-equality drift
   test, instead of a hand-written mirror. Go code adds only what JSON Schema cannot express:
   - the member tier's `sibling_ordinal` equals `fanout_v1.ordinal`;
   - byte bounds;
   - canonical form.
4. **`decision_revision` lives in a small side table** (`execution_interrupt_responses`). Its row lock also orders the
   ledger's writes for one response.
5. **The private claim authority is the node recovery authority** with `recovery_mode='NONE'` and
   `desired_state='RUNNING'`. Code review suggested sharing one query. Skipped: the selected columns and predicates
   differ, and node recovery code is outside this PR. Follow-up below.
6. **Migration number 0157.** The migration was written as 0155, which was free on 2026-10-08. On 2026-10-09
   `origin/main` took 0155 (`local_turn_executions`) and 0156, so merging main renumbered this one to 0157 with no
   change to its body. It has not shipped. Any later collision must renumber again.

## Tests (2026-10-08, darwin/arm64, go1.26.5, `-race`)

All tests below ran with zero skips. PostgreSQL was present: postgres:18 for development, pgvector/pgvector:pg16
(the CI image family) for the full suite.

| Suite | Count |
|---|---|
| `internal/domain/executioninterrupt` | 13 PASS: every D2 card, decision and ACK fixture (valid pass, invalid refused, canonical round-trip); limit and limit+1 for card bytes, decision body bytes and interrupt id; the drift test; limits pinned to schema bounds |
| `internal/infra/db/repos` ledger (real PostgreSQL) | 26 PASS (listed below) |
| `internal/api/v2/agentexecution` | 6 PASS: strict admission (16 subtests); D2 decision fixtures as HTTP bodies; list (5 subtests); spec; card spec accepts all contract cards; log capture. Every body is validated against the D2 schemas. |
| `internal/runtimecomposition` / `internal/api` | 1 + 1 PASS: config flag (default off, strict, needs dispatch, refused with the runtime off); router mounts only when composed (8 subtests) |
| `internal/application/projectprovisioning` | `TestEveryBlockingForeignKeyIsCovered` PASS (it was red before the deprovision fix) |
| Full Main suite `go test -race ./...` (pgvector PG16) | 187 packages ok, 0 failures, 22 without tests, at `75bde938` |
| Web | `check-generated-client` OK (679 files); `check-endpoint-manifest` vitest 14/14; `check-contract-coverage` OK; `tsc --noEmit` OK; `check-dead-code`, `check-layer-cycle`, `check-budgets`, `check-handlers` OK |

The 26 PostgreSQL tests:
- **Raise:**
  - `RaiseReplayIdenticalIsNoopAndDifferentBytesFault`;
  - `RaiseRefusesInvalidInputBeforeAnyWrite`;
  - `RaiseCapAtSixteenAndSeventeen`;
  - `RaiseCapHoldsUnderConcurrency` (20 concurrent raises → 16 admitted);
  - `RaiseRefusesAnExecutionNotBoundToTheResponse`.
- **Decide:**
  - `DecideAppliesOnceAndReplays`;
  - `DecideRefusesInvalidActionStaleRevisionAndUnknownKey`;
  - `DecideFiftyConcurrentTwoTabs` (1 applied, 24 replays, 25 conflicts);
  - `DecideWaitsForAnInFlightDecision` (a forced interleaving; it fails on the lock-free mutant);
  - `DecideReverseOrderAndSameToolCallInTwoChildren` (TG-03);
  - `DecideAuthorizationInsideTheTransaction`;
  - `DecideKeyFromOtherResponseIs404`;
  - `DecideRechecksMembershipAfterTheLockWait`;
  - `DecideByOutsiderNeverWaitsForTheResponseLock`;
  - `DecideSingleTransaction`;
  - `DecideLatencyBudget`.
- **Cancel and supersede:** `CancelAndSupersedeCloseEveryOpenCard`, `CancelSeesACardRaisedWhileItWaited`.
- **Fetch and ACK:**
  - `FetchAndAckUnderTheLiveClaim`;
  - `FetchAndAckRefuseStaleFences` (9 fence cases);
  - `FetchRechecksTheClaimAfterTheLockWait`;
  - `LateAckAfterCancelRefused`;
  - `FetchAndListRefuseMoreThanTheCap`;
  - `AckRefusesInvalidBodies`.
- **Schema:** `InterruptStateChecksRejectIncoherentRows`.
- **HTTP:** `ExecutionInterruptHTTPAgainstPostgres` (real route, real RBAC resolver and real ledger: multi-tab and
  foreign refusals).

Mutation checks: each of these mutants made its proving test fail.
- No row locks;
- a single claim lookup;
- cancel without the per-response lock;
- no binding check;
- lock-and-check in one statement.

The existing complete-set path is untouched: there is no diff to `application/agentexecution`, `db/queries` or
`sqlcgen`. `internal/application/agentexecution` passes, as do 24 existing PostgreSQL HITL and continuation tests,
unchanged.

## Performance

| Budget (contract §12) | Mechanism | Proof and measurement |
|---|---|---|
| One transaction per decision | `Decide` :309. Lock, re-checks, card lock, then one CTE statement for the CAS, `decision_revision`+1 and the DECIDED audit row (:372) | `TestDecideSingleTransaction`: 1 BEGIN/COMMIT, 11 statements (3 tenant binding, 1 response lock, 1 ownership re-check, 3 RBAC, 1 per-response lock, 1 card lock, 1 CAS+audit) and 3 row writes. A replay is ≤10 reads. The budget rose from 9 after the review fixes (2 extra statements for the re-check and the per-response lock). |
| Decision POST p95 ≤ 150 ms | Primary-key access only, a bounded lock scope, no network calls inside the transaction | `TestDecideLatencyBudget` asserts the best p95 of 3 rounds of 200 sequential decisions. Low load: p50 3.6 ms, p95 7.3 ms. Same host under load (pgvector PG16): p95 12–33 ms. At host load average ≈50, single rounds reached p95 169–198 ms, which is why the assertion takes the best round. |
| GET ≤ 16 cards × ≤ 32 KiB | `List` :243 `LIMIT 17`; more than 16 is a ledger fault, never truncated | `TestFetchAndListRefuseMoreThanTheCap` |
| Fetch ≤ 16 entries × ≤ 64 KiB, one snapshot | `FetchDecided` :473, a single LATERAL statement (:483), the entry-size check, `LIMIT 17` | `TestFetchAndListRefuseMoreThanTheCap`, `TestFetchAndAckUnderTheLiveClaim` |
| Ledger rows | 1 row per card, 1 audit row per transition, 1 row per response | the CHECK and audit assertions in the tests above |

## Durability

| Window | Durable state | Recovery rule | Proof |
|---|---|---|---|
| Raise replayed (frame redelivery or worker restart) | the card row | A byte-identical card and frontier is a no-op under any event id. Different bytes, a reused event id, or an open `interrupt_id` clash is `ErrRaiseConflict`, and nothing is written. | `TestRaiseReplayIdenticalIsNoopAndDifferentBytesFault` |
| Decide crashes before commit | nothing | The transaction rolls back. The client retries with the same `request_id`. | atomic transaction (`DecideSingleTransaction`) |
| Decide committed, response lost | the DECIDED row with its canonical bytes | The same `request_id` with the same bytes returns 200 `replay:true` (also after CONSUMED). | `TestDecideAppliesOnceAndReplays`, `TestFetchAndAckUnderTheLiveClaim`; live, see below |
| Concurrent tabs | response row lock, then card lock, then CAS | Exactly 1 DECIDED; the rest replay or get 409 | `TestDecideFiftyConcurrentTwoTabs`, `TestDecideWaitsForAnInFlightDecision` |
| Fetched, worker died before ACK (Wave 2 caller) | the DECIDED row | Fetched again under the new claim. Fetch is read-only. | `TestFetchAndAckUnderTheLiveClaim` |
| ACK applied, response lost | CONSUMED/SUPERSEDED with the exact ACK bytes | A byte-identical replay returns `replay:true`; any other bytes give `ErrAckConflict` | `TestFetchAndAckUnderTheLiveClaim` |
| Stop or regenerate during a raise in flight | the per-response lock | Cancel waits for the raise and closes its card too | `TestCancelSeesACardRaisedWhileItWaited` |
| Late ACK after a stop | CANCELLED (and the dead fence) | Refused: `ErrAckConflict`, or `ErrStaleFence` once the job is stopped | `TestLateAckAfterCancelRefused`, `TestFetchAndAckRefuseStaleFences` |
| Incoherent row | n/a | The table CHECKs reject it, whoever writes it | `TestInterruptStateChecksRejectIncoherentRows` |
| Project deleted | n/a | Audit rows, then cards, then response rows are deleted before jobs | `TestEveryBlockingForeignKeyIsCovered` |

### Recovery-guarantee rows (component × phase touched)

| Component | Phase | Class | Enforcing code | Proof |
|---|---|---|---|---|
| Main | HITL decision (public POST) | **I** | `Decide` :309: CAS, `request_id` plus byte replay | `TestDecideAppliesOnceAndReplays`; live replay |
| Main | HITL decision, crash mid-transaction | **I** | one transaction (rollback); the client retries the same `request_id` | `TestDecideSingleTransaction` |
| PostgreSQL | HITL decision | **I** | an atomic transaction plus CHECKs | as above; `TestInterruptStateChecksRejectIncoherentRows` |
| Web/browser | HITL decision from multiple tabs | **I** | replay or 409 `agent_interrupt_already_resolved` | `TestDecideFiftyConcurrentTwoTabs`; live tab A/B |
| Worker | HITL delivery: fetch and ACK (repository built; Wave 2 route) | **R** for the decision, **I** for the ACK | DECIDED survives until an ACK under a live claim; byte-identical ACK replay | `TestFetchAndAckUnderTheLiveClaim`, `TestFetchRechecksTheClaimAfterTheLockWait` |
| Main | stop/regenerate vs open cards (repository built; Wave 2 call sites) | **F** (typed close) | `closeOpenExecutionInterrupts` :649: CANCELLED/SUPERSEDED, then a late decision gets 409 and a late ACK is refused | `TestCancelAndSupersedeCloseEveryOpenCard`, `TestLateAckAfterCancelRefused` |
| Main | admission of a raise (Wave 2 caller) | **I** | idempotent on (response, key); cap and conflicts are typed | Raise tests |

No row is **L**. The existing complete-set root HITL path is unchanged. Its own recovery behaviour is out of scope
here and moves to the ledger in Wave 2.

## Resilience

- **Bounds**, all named in `limits.go` and checked at the boundary before any database work:
  - 16 open cards per response (PENDING + DECIDED, user decision 2026-10-08);
  - decision body ≤ 8192 bytes, enforced by `MaxBytesReader` (:121) and again on the canonical bytes;
  - value ≤ 8192 characters;
  - card ≤ 32768 canonical bytes, with raw input ≤ 6× that for fully escaped producers;
  - `interrupt_id` printable ASCII ≤ 512;
  - ACK ≤ 1024 bytes;
  - frontier ≤ 2048 bytes;
  - JSON depth ≤ 8;
  - fetch ≤ 16 × 64 KiB;
  - every list and fetch query has a `LIMIT`.
  - Proofs: limit and limit+1 tests in `contract_test.go`; `TestExecutionInterruptLimitsMatchContractSchemas` pins
    each constant to its schema bound.
- **Typed failures.** `ErrInvalidDecision` → 400, `ErrNotFound` → 404, `ErrAlreadyResolved` → 409,
  `ErrNotAllowed` → 403, cancelled or deadline → 504, anything else → 500 with a generic body. The private-path
  errors are `ErrStaleFence`, `ErrAckConflict`, `ErrCapReached`, `ErrRaiseConflict` and `ErrLedgerFault`.
- **No panics on request paths.** The canonical writer returns an error for an unsupported type.
- **Cancellation.** Contexts propagate into pgx. A request context canceled while waiting on a lock fails with 504
  and leaves nothing written.
- **Flag fail-closed.** Unknown values are refused, the flag needs agent dispatch, and it is refused when the
  runtime is disabled (`TestConfigExecutionInterruptsAPIIsExplicitAndRequiresAgentDispatch`).

## Security

- **Authorization inside the effect transaction, fail closed.**
  - Steps, in order:
    1. lock the response only if the actor may decide, so a non-owner never takes the lock (:74);
    2. re-check ownership and membership in a new statement;
    3. re-check RBAC (active user, active project, project role grant) in a new statement;
    4. lock the per-response row and the card;
    5. CAS.
  - The middleware also checks the permission before the handler runs.
  - A missing response and a foreign one both answer 403.
  - Proofs: `TestDecideAuthorizationInsideTheTransaction` (outsider, foreign actor, foreign project, suspended
    user, removed member, revoked role, unknown response, for both Decide and List);
    `TestDecideRechecksMembershipAfterTheLockWait`; `TestDecideByOutsiderNeverWaitsForTheResponseLock`;
    `TestExecutionInterruptHTTPAgainstPostgres`; live checks (project 1 and 99 → 403).
- **`interruptKey` grants nothing.** A card is looked up only under the authorized response and project
  (`TestDecideKeyFromOtherResponseIs404`).
- **Private routes (Wave 2).** The claim must be an ordinary RUNNING claim (never `NODE_RECOVERY`) with a current
  session, a granted command inside its deadline, an immutable manifest and no terminal result. It is checked twice
  under the locks, and the original actor's permission is re-checked (`TestFetchAndAckRefuseStaleFences`, 9 cases).
- **Input handling.**
  - Closed schemas with `additionalProperties: false`, plus duplicate-key, depth and UTF-8 checks.
  - Parameterized SQL only.
  - The tenant `search_path` comes from the integer project id.
  - The decision and ACK are re-derived from the canonical bytes inside the repository.
  - Raise binds the execution to the response and project.
- **No secrets.**
  - Decisions carry `credential_ref`, never a token, and a raw-token body is refused (fixture and live check).
  - Logs carry only `interruptLogFields`: response id, a 12-hex key prefix, actor, state, revision and replay. This
    is proven by `TestInterruptLogsCarryNoValues`: no value, credential ref, request id, full key or display text.
  - The 500 body is generic, and `PgError.Error()` excludes row detail.
  - Responses never include the frontier, `decided_by`, the value or `credential_ref`.
  - Audit rows hold ids only.
- **Rejection is never permission.** The stored action is returned exactly as decided (`TestFetchAndAckUnderTheLiveClaim`).
- **Supply chain.** No new dependency, and no `go.mod`, `go.sum` or `package.json` change.
- **Reviews:**
  - **Adversarial race/security review** (opus): 10 findings, two of them proven against the database. All are
    fixed, each with a test that fails without the fix:
    - deprovision blocked by the new FKs;
    - membership not re-checked after the lock wait;
    - a single claim lookup;
    - cancel without the response lock;
    - lock order;
    - GET 400 envelope;
    - struct/bytes pairing;
    - Raise binding;
    - HTML escaping;
    - the fetch snapshot.
  - **`code-review` (high)**: 7 findings. 6 fixed: lock before the ownership check, the flaky wall-clock test, the
    raw card bound, reuse of the id validators, the digest error, and the flag with the runtime off. 1 skipped: the
    shared claim-authority SQL (see follow-ups).
  - **`security-review`**: no vulnerability at confidence ≥ 8. One below-threshold design note is recorded as a
    follow-up: a conversation author may decide on a run another participant started (today's rule), so Wave 2 must
    check that `credential_ref` belongs to the job's actor.
- **`govulncheck` v1.1.4** on `services/elitea-main` (go1.26.5): 7 reachable Go standard-library advisories
  (GO-2026-6218, -6091, -6090, -6089, -6088, -5972, -5026), all fixed in go1.26.6, plus 1 imported package and 5
  required modules with no reachable call. `go.mod` and `go.sum` are unchanged by this branch, so every finding is
  pre-existing (toolchain level) and none is new.

## Deployed rehearsal evidence (local NATS candidate stack, 2026-10-08)

Deployment was approved by the user: the Main container was swapped after a database snapshot.

- **Stack.** `elitea-nats-candidate-*-a5994fa609c2`, browser ingress `http://localhost:18094`, OIDC emulator
  `oidc.localhost:19494`.
  - Worker `elitea-worker-rust:frozen-cargo-c53ab7d5496a-hotfix1140-20261008`, Web `web-reload-42a0c390bc19`.
  - Both unchanged.
- **Main images:**
  - previous: `elitea-main:current-nats-efa7213e803d-hotfix1140-20261008`;
  - deployed: `elitea-main:current-nats-efa7213e803d-hotfix1140-m2-99be91e4-20261008`, id
    `sha256:39030075444a924c63a63399c1086f9df2cc215397bd1d6bf2db7a4c96191011`.
- **The deployed image:**
  - It is the previous image with the seven Main binaries replaced. They are cross-compiled with the Containerfile's
    exact flags (`CGO_ENABLED=0`, `-trimpath`, `-s -w`, linux/arm64, go1.26.5).
  - Composite source `99be91e4`, a local merge that is not pushed: candidate source `efa7213e` (#1084) + the #1140
    hotfix (`024e6881`, cherry-picked as `3ab0ef89`) + this branch at `75bde938`.
  - Flag: `ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED=true`.
- **Snapshot and migration.** `pg_dump -Fc` of `elitea_nats_candidate_product_a5994fa609c2` was taken before
  migrating: 35 MB, 294 table-data entries, verified with `pg_restore --list`. Then `elitea-migrate` (shared)
  applied 0154 and 0155; the ledger shows `154|agent_stop_question_author` and `155|execution_interrupts`.
- **Rollback.** Stop the new container, restore the dump, then rename and start the preserved previous container
  `…-pre-m2`. The previous binary refuses a database whose ledger has versions it does not know, which is why the
  dump comes first.

### Live HTTP checks of the new API (browser session cookie, values redacted)

- **Fixtures.** Response `cbcbae73-1490-5b67-be35-42c755f79c95` (chat 868, execution
  `fe84ff3161a1e92b73830ba25e56b344`, project 2).
- **Seeding.** Two cards from the D2 fixtures were **seeded directly into the database**, because `Raise` has no
  caller in Wave 1: `m2-live:1` tool_guard (key `813ec8f736d3…`) and `m2-live:2` ask_user (key `8e147c3ff965…`).
  The seed SQL mirrors `Raise`: a response row, a PENDING card at revision 1, and a RAISED audit row with claim
  `m2-live-db-seed`.

| Request | Response |
|---|---|
| GET `…/prompt_lib/2/cbcbae73…/interrupts` | 200 `fanout-interrupt-list.v1`, `decision_revision` 0, 2 PENDING |
| Tab A POST `…/813ec8f7…/decision` `{approve, request_id a1…}` | 200 `{state DECIDED, revision 2, replay false}` |
| Tab A same body again | 200 `replay true` |
| Tab B POST `{block_with_comment "not now", request_id b2…}` | 409 `agent_interrupt_already_resolved` |
| Unknown key | 404 `agent_interrupt_not_found` |
| `approve` on the ask_user card | 400 `agent_interrupt_invalid_decision` |
| Extra `access_token` field / `?value=x` query / body > 8 KiB | 400, 400, 400 |
| POST and GET through project 1 or 99 for the same response | 403 |
| GET after | `decision_revision` 1; card 1 DECIDED rev 2; card 2 PENDING rev 1 |

The live list, decision-result and error bodies validate against the D2 schemas with Ajv (Draft 2020-12), a second
validator independent of the Go one.

The database afterwards showed:
- card 1 DECIDED rev 2, `request_id a1…`, `decided_by` 3, 133-byte decision;
- audit: RAISED, RAISED, `DECIDED:2:3`;
- `decision_revision` 1.

The state persisted across three Main restarts.

### Browser regression of the existing HITL flows (real backend, no mocks, reload)

The candidate stack has **no LLM gateway on its network** and the C1 MCP mock lives on another network. So these
flows cannot run on this stack under any Main build:
- sensitive-tool approve/block: agent 157 "C1 sensitive record agent" fails at assembly with "the native MCP
  toolsets could not be materialized" (chat 867);
- ask_user: also capability-disabled in the Rust worker;
- delegated auth: needs a remote OAuth MCP server.

Two pipelines were created **through the UI** as fixtures, with no model call.

1. **HITL node: pipeline 160 "M2 HITL regression 20261008", chat 868.**
   - Start, pause and the approve/reject card all worked, and the card survived a reload under the M2 Main.
   - Approve sends `continue_predict` (`agent.continue.hitl.v1`), which returns **422**
     `unsupported_agent_execution`. The Main log says "current agent start is not supported by the admitted parity
     slice"; the UI shows "no chat connection is available".
   - A/B: the same approval against the candidate's own Main code (`3ab0ef89` plus only migration files 0154/0155,
     image `sha256:7f99f1a20d9e…`) returns the **identical** 422 and log line.
   - So this is a pre-existing limitation of this stack, and M2 does not change it.
2. **Static pause: pipeline 161 "M2 static pause 20261008", chat 869.**
   - Under M2 the pause card ("Pipeline paused. Type a message to continue.", composer "Paused after tick") rendered
     and survived a reload.
   - The typed continuation was sent as a new turn (7469/7470), which paused again.
   - A/B on the candidate's own Main produced the **identical** outcome (7471/7472).
3. **After restoring M2**, chat 868 reloads with its HITL card intact, rendered from `meta.hitl_interrupts`. The UI
   makes no ledger call in Wave 1, as intended.

The positive browser proof of the ledger, a card raised by the Worker and decided in the UI, happens when Wave 2
wires it.

## Fixtures: how they were created

- **PostgreSQL integration tests:** fixtures are built by test SQL in isolated per-test databases (migrated
  template). The auth tables use the `001_initial` shape that sibling tests use.
- **Live checks:** ledger cards were seeded **directly in the database** (above).
- **Browser regression:** pipelines 160 and 161 and chats 868 and 869 were **created through the UI**; chat 867 was
  opened from an existing agent through the UI.

## Open limits and follow-ups

- **Wave 2:**
  - the `agent_interrupt_pending` projection into `Raise` (in the frame-accept transaction);
  - the private fetch/ACK routes;
  - `ObserveDesiredState` hints;
  - the stop/regenerate call sites (they must hold the `chat_message_group` lock);
  - park/wake with one continuation per response;
  - moving root pauses to the ledger and removing the complete-set checks;
  - Web callers, plus an `agent_hitl_resolved` frame from `DecideResult`/`Resolved`.
- **`credential_ref` ownership.** Before Wave 2 resolves a `credential_ref`, the token-store lookup must check that
  the reference belongs to the job's original actor (security-review note).
- **Shared claim authority.** Share one claim-authority query between node recovery and the ledger (code-review
  item 4, skipped here to keep node recovery code out of this PR).
- **Edit/answer values over 8 KiB** cannot be decided in the ledger (contract §6, user decision). Wave 2 must either
  accept that or add content references.
- **Candidate stack, HITL node continuation.** The 422 is a pre-existing problem, proven by the A/B above. Separate
  investigation needed (Main `ContinueCurrentAgent` parity gate for pipeline HITL review).
- **Rehearsal stack state.**
  - The candidate Main now runs the M2 composite with migrations 0154/0155 applied. Other sessions that deploy a
    Main without these migrations must restore the snapshot first, or include them.
  - That database records this ledger as version 155, the number it had on 2026-10-08. Main's own 0155 is now
    `local_turn_executions`, so a Main built from current `main` or from this branch will not start on it. Restore
    the snapshot before deploying either.
  - The previous container `…-pre-m2` and the dump are kept for rollback.
  - The seeded `m2-live:*` ledger rows remain as evidence. Nothing consumes them.
- **CI.** The Go job will run the new PostgreSQL tests. The latency test asserts the best of three rounds so that a
  loaded runner does not flake it.
