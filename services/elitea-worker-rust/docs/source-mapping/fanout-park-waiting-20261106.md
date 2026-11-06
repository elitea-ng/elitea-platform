# A parked fan-out reads as waiting for a person (FU-284), 2026-11-06

`M/` = `services/elitea-main/internal/`. Main only. One additive tenant migration (0161, number assigned at merge). No
contract change.

## Business behaviour

Taken from the current platform: a run that stops for a person's decision ends a run-to-completion chain as waiting.
No goal check runs and no continuation starts until the person answers. The run is announced as "needs you", and an
open pause is not something you can fork from.

On the new platform, a Parallel or a Map pauses by parking its members on cards (`fanout_parked_v1`, contract §8)
instead of a root `hitl_interrupt`. Six readers knew only the root pause keys. A parked fan-out therefore read as
finished:
- the chain ran its goal check and could start a continuation while the cards were open; the answered card's wake then
  ended INTERNAL (FU-284, chat 40);
- a parked schedule, webhook or handed-off run was announced as "completed";
- the workspace hand-back outcome said "completed";
- the editor's test run said TERMINAL;
- the conversation list did not say "needs you";
- a fork could copy an open park whose cards belong to the source response's ledger.

**What changed:** one shared definition of "waits for a person" (`M/domain/responsepause`: `hitl_interrupt`,
`hitl_interrupts`, `authorization_requests`, `fanout_parked_v1`), used by every reader that asks it.

**Deliberately unchanged:**
- **Unattended approval.** `lockPausedResponse` (`M/infra/db/repos/approval_unattended_store.go:152-165`) decides only
  the root-pause path. A fan-out card's unattended approval closes its ledger cards and drops `fanout_parked_v1`
  itself (`approval_unattended_store.go:296-325`, `approval_unattended.go:645`).
- **Cancel.** `agent_cancel.sql:66-73` cancels a settled job only when the response holds a root pause, and strips
  only those keys. A parked run has settled, and its cards close by supersede (`M/infra/db/repos/
  execution_interrupts.go:701-727`; T-2A3 browser case 6). Adding the park key there would set the job cancelled
  while the ledger cards stayed open.
- **Mix refusal, strip and decode lists** (`execution_interrupts_wiring.go:38`, `node_recovery_projection.go:30`): not
  "is it waiting" readers.

## Changed paths

| Path | Change |
|---|---|
| `M/domain/responsepause/responsepause.go` | new: `WaitingKeys`, `SQL(alias)` over a closed set of aliases (`Unqualified`, `Response`, `Runs`, `Group`) with an identifier backstop, `Waiting(meta)` |
| `M/infra/db/repos/run_finished.go:322-352` | `readResponseFacts(…, executionID)`: `Paused` = waiting keys, or a fan-out wake **of this park** took the response |
| `M/infra/db/repos/run_chains_settle.go:42` | passes the settled execution |
| `M/infra/db/repos/workspace_handoff_continue.go:427` | hand-back outcome `waiting` via the predicate |
| `M/infra/db/repos/editor_test_runs.go:63` | `PAUSED` via the predicate (plus its own extra keys) |
| `M/infra/db/chatactivity/activity.go` | `Paused()` reads one arm per partial index (0153 paused, 0161 parked), each in its index's exact text; `Load()`'s predicate is built from `responsepause` |
| `services/elitea-main/migrations/tenant/0161_chat_message_group_parked_index.sql` | additive: partial index `chat_message_group_parked_idx (conversation_id) WHERE NOT is_streaming AND meta ? 'fanout_parked_v1'`, and `chat_group_meta_paused` with the park (the conversation sync bump) |
| `M/infra/db/repos/execution_interrupts.go` (`closeOpenCards` → `dropResponsePark`) | when every open card is closed (new turn, regenerate, stop, run end, failed wake), the response's park marker goes too, unless a wake of it is queued or admitted |
| `M/db/queries/agent_chat.sql`, `agent_static_pause.sql` (+ sqlc) | the output-limit and static-pause continue guards refuse the park key |
| `M/infra/db/repos/conversation_fork.go:93-96` | `forkPauseKeys` = waiting keys + `node_recovery_required_v1` |

## Tests

All Main tests listed below ran on a disposable PostgreSQL 18. "Fails on main" was checked by restoring main's version
of the reader and re-running.

| Test | Proves | Fails on main |
|---|---|---|
| `TestPostgresChainWaitsOnAParkedFanout` | real hook and repository: the chain ends `waiting`, no goal check, no continuation, one link; the wake's settle leaves the chain unchanged | yes ("completed") |
| `TestPostgresChainWaitsWhenTheParkWakeAlreadyTookTheResponse` | a wake of this park already took the response: still `waiting`, no goal check | yes ("completed", 1 check) |
| `TestPostgresChainKeepsItsOutcomeWhenAnotherExecutionTookTheResponse` | a non-wake owner, or a wake of another park, keeps today's outcome (completed after one goal check) | no (control) |
| `TestPostgresChainStillContinuesARunThatDidNotPark` | a step-limit run still starts its continuation | no (control) |
| `TestPostgresRunFinishedAnnouncesAParkedRunAsWaiting` | a parked schedule run is announced `waiting` | yes ("completed") |
| `TestConversationActivityFollowsTheTurn` (park case) | "needs you" and `chatactivity.Paused` for a park | yes (idle) |
| `TestPostgresHandoffContinueCommitsTheSendAndTheTurnTogether` (park case) | hand-back outcome `waiting` | yes ("completed") |
| `TestForkPointMustBeSettled` (park case) | a parked point is `fork_point_unsettled` | yes (201) |
| `TestEditorLifecyclePostgres/…/parked_fanout` | the editor test run is `PAUSED` | — (the fixture's disposable listener) |
| `TestPostgresChainWaitsWhenTheParkWakeAlreadyFailed` | a wake of this park that already finished with an error and a step limit: the parked run's facts are only "parked" (waiting, no check, no continuation) | yes |
| `TestClosedCardsEndTheResponsePark` | a new turn supersedes the cards and the park marker goes with them, keeping the other meta | yes |
| `TestClosedCardsKeepTheParkOfARunningWake` | cards closing while a wake is admitted keep the marker | — |
| `TestPausedReadsBothPartialIndexes` | `Paused()` answers a paused and a parked conversation; `EXPLAIN` uses `chat_message_group_paused_idx` and `chat_message_group_parked_idx`, no `Seq Scan` on the table | — |
| `TestParkedIndexMigrationIsANoOpTwice` | 0161 applied twice changes nothing; the function reads the park | — |
| `TestResumeGuardsRefuseEveryWaitingKey` | the four continue/resume guards refuse every `responsepause` key | yes (park key missing) |
| `responsepause` unit tests | each key waits; other keys don't; the SQL per alias; the alias backstop | — |
| `TestPausedPredicateNamesEveryWaitingKey` | "needs you" carries every waiting key and the partial index's exact text | — |

**Counts at `e6ebabb7a`:** Main `go test ./internal/... ./migrations/...`, 281 packages ok; `repos` re-run on a quiet
machine, ok in 209 s (an earlier run during image builds hit the package's 10-minute default and a 45 s timing budget
in an unrelated index-activity test; that budget passes unchanged on the quiet machine). `go vet` clean. The editor
fixture test ran on its own disposable listener (127.0.0.1:15444), removed afterwards. Skips: none of the tests above.

## Performance

- **Chain settle and run finished.** `readResponseFacts` stays one statement. The wake check is an `EXISTS` on
  `execution_interrupt_wakes_wake_execution` (`migrations/shared/0202_execution_interrupt_wakes.sql:76`). It reads at
  most the rows of one wake execution, and only for the one response row read by uuid.
- **"Needs you."** `Paused()` is a `UNION` of two arms, each in its partial index's exact text: 0153's
  `chat_message_group_paused_idx` and 0161's `chat_message_group_parked_idx`. It reads only paused and parked groups, as
  before (`TestPausedReadsBothPartialIndexes` checks the plan).
- **The 0161 build.** The runner wraps each tenant file in a transaction, so `CONCURRENTLY` cannot run (as 0156). The
  plain `CREATE INDEX` holds a SHARE lock on `chat_message_group` for one pass over the table: writes wait, reads go on.
  - Measured on PostgreSQL 18 (this machine): 1,000,000 groups with about 0.4 KB of meta each, 1 in 1,000 parked,
    built in 0.31 s.
  - The real-model dump's largest tenant (3,491 groups) built in under 2 ms.
  - The index holds only parked groups.
- **Other readers.** Each is one extra `?|` on a row already read.

## Durability

- Nothing new is written beyond the park marker's removal when the cards close (same transaction, under the response
  row lock the caller holds; kept while a wake is queued or admitted). The chain end is written once, as before (`EndChain`, `TestPostgresUnattendedChainSettleViewAndEndOnce`).
- **What happens when a decision meets a running chain:**
  - *Parked, no decision yet.* The chain ends `waiting` (`M/application/unattendedchain/hook.go:145-146`). The decision
    queues the wake. The wake is not a chain link, so its settle is a no-op for the chain
    (`M/infra/db/repos/run_chains_settle.go:31-33`), the same as a cloud HITL resume. Cloud never writes a `resume`
    link; that kind is desktop-only (`M/application/unattended/unattended.go:62-69`).
  - *A decision already waiting at park time.* The wake is queued in the settlement transaction
    (`execution_interrupts_wiring.go:405-418`), and wake start takes the response (`execution_interrupt_wakes.go:375-378`).
    The settle sees either the park key or a response held by a wake of this very park, and ends `waiting` either way.
  - *A continuation already running when a card is answered.* No continuation starts while a park is open, so a
    park can no longer reach this state.

## Resilience

- A settle replayed after a crash reads the same facts and ends the chain once (`EndChain` refuses a second end).
- Typed and readable: the chain's end state and the run-finished outcome say `waiting`, and the conversation says
  "needs you".

## Security

- **SQL construction.** `responsepause.SQL` builds only from constant keys and one of four constant aliases. A value
  outside the identifier backstop yields `FALSE`. The response and execution are bound parameters (`$1`, `$2`), and the
  schema comes from `tenantSchema` as before.
- No authorization, identity, egress or secret change. No new endpoint.
- A fork can no longer copy an open park, whose cards would answer into another response's ledger.

## Recovery rows

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Main × HITL pause and decision (fan-out park in a chain) | R | `hook.go:145-146` with `readResponseFacts` | `TestPostgresChainWaitsOnAParkedFanout` |
| Main × settlement (replayed settle) | I | `EndChain` single end | `TestPostgresUnattendedChainSettleViewAndEndOnce` |
| Main × settlement while a wake runs | I | `run_finished.go` wake check | `TestPostgresChainWaitsWhenTheParkWakeAlreadyTookTheResponse` |
| Web × activity (needs you) | I | `chatactivity.PausedPredicate` | `TestConversationActivityFollowsTheTurn` |

## Browser evidence

- **Stack.** My own standalone `fu284` (`STANDALONE_HOST=fu284.localhost`, port base 19300), from the real-model dump
  `product-real-models-main-d5e966074.dump`, with the production profile.
  - verify-profile 50/50.
  - The migrations at the branch head, 0161 included, applied to this throwaway database only, never to a shared stack.
  - Torn down afterwards (`stack.sh down --volumes`).
- **FU-279 workaround** (documented): migration `shared/0206` fails on this dump's legacy `centry.audit_events`. The
  throwaway database got the five columns `input_tokens INTEGER`, `output_tokens INTEGER`, `llm_cost NUMERIC(18, 8)`,
  `token_source VARCHAR(16)` and `cost_source VARCHAR(64)` (the types of `001_initial.sql`) before migrate was re-run.
- **Images:** built from `e6ebabb7a`.
  - Main `fu284-e6ebabb7a` (`sha256:ff0a7514c95e…`); binary check: `chat_message_group_parked_idx` present.
  - Worker `fu284-e6ebabb7a` (`sha256:786161ffa658…`).
  - Web `fu284-e6ebabb7a` (`sha256:0eaf32fa6efe…`).
  - The other services are the `pm-*` images fanpar runs.
- **Fixtures.** Created through the UI's own REST endpoints from the signed-in page (`POST
  …/applications/prompt_lib/118`, relation `PATCH`), never the database:
  - "FU284 asking agent" (gpt-5.4-mini, `ask_user`);
  - "FU284 parallel asks" (a Parallel of two Agent branches running it).
- **Run.** Claude in Chrome, no mocks. Chat 3, run-to-completion on (`aria-pressed=true`).
  - From `run_chains`: one row, trigger `manual`, grant `goal_check: true`, brief `ready`. So the mode is real, not
    read from the toggle.
- **While parked.**
  - Two cards: "Which colour … for the lights?" and "… for the bell?".
  - The chain `end_state = waiting`, `continuations = 0`, one link (`root`), `done_check` empty: no goal check ran.
  - Ledger: 2 PENDING. The response carries `fanout_parked_v1`.
  - The sidebar shows "Today · 1 needs you".
- **Answered, then reloaded.**
  - "amber" (lights), then "silver" (bell). After a reload: one answer, bell "Silver.", lights "amber", both succeeded.
  - Ledger: 2 CONSUMED. The park marker is gone. The parked root and both wakes are SUCCEEDED.
  - The chain is still `waiting`, 0 continuations, 1 link. No goal check in Main's log.
  - The run-finished notification's outcome is `waiting`.
- **PASS.** On main, this park read as finished: the goal check ran beside the open cards (chat 40, FU-284).

## Follow-ups

- FU-279 (pre-existing): a fresh restore of the real-model dump fails `shared/0206`; the workaround above was needed
  again.
